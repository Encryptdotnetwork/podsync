from __future__ import annotations

import asyncio
import logging
import re
from dataclasses import dataclass
from datetime import datetime, timedelta, timezone
from typing import Any

from app.config import AppConfig
from app.db import Database
from app.ollama import ClassificationResult, OllamaClient
from app.podsync import PodSyncAdminClient, write_feed_json
from app.youtube import CandidateVideo, YouTubeClient

logger = logging.getLogger(__name__)

# Grace window for recently-ended livestreams whose YouTube VOD duration is still 0s
_RECENT_ZERO_DUR_GRACE = timedelta(hours=6)

_NON_ENGLISH_CHANNEL_RE = re.compile(
    r"(?i)(?:^|[\s/|\-_(\[])(italiano|italian|portugu[eê]s|portuguese|deutsch|german|"
    r"espa[nñ]ol|spanish|fran[cç]ais|french|turkish|t[uü]rk[cç]e|russian|polski|nederlands|"
    r"hindi|arabic|japanese|korean|chinese|mandarin|dubbed)(?:$|[\s/|\-_)\]])"
)
_NON_LATIN_SCRIPT_RE = re.compile(r"[\u0400-\u04FF\u0600-\u06FF\u0900-\u097F\u3040-\u30FF\u4E00-\u9FFF\uAC00-\uD7AF]")


def _detect_non_english(video: CandidateVideo) -> str | None:
    """Return a reason string if the video is clearly non-English, else None."""
    for label, lang in (
        ("audio language", video.default_audio_language),
        ("language", video.default_language),
    ):
        clean = lang.strip().lower()
        if clean and not clean.startswith("en") and clean not in {"und", "zxx"}:
            return f"Non-English {label}: {lang}"
    m = _NON_ENGLISH_CHANNEL_RE.search(video.channel_title)
    if m:
        return f"Non-English mirror channel ({m.group(1)})"
    if _NON_LATIN_SCRIPT_RE.search(video.title):
        return "Non-English script in title"
    return None


@dataclass
class ScanSummary:
    run_id: int
    status: str
    candidates: int
    added: int
    filtered: int
    error: str = ""
    sync_result: dict[str, Any] | None = None


def _parse_iso_dt(raw: str) -> datetime | None:
    if not raw:
        return None
    try:
        dt = datetime.fromisoformat(raw.replace("Z", "+00:00"))
        if dt.tzinfo is None:
            dt = dt.replace(tzinfo=timezone.utc)
        return dt
    except ValueError:
        return None


def _is_recently_published(published_at: str, window: timedelta) -> bool:
    dt = _parse_iso_dt(published_at)
    if dt is None:
        return False
    return (datetime.now(timezone.utc) - dt) <= window


def _person_tokens(raw: str) -> set[str]:
    return {part.strip().lower() for part in raw.split(",") if part.strip()}


def _is_same_channel_clip(
    video: CandidateVideo,
    matched_names: set[str],
    existing_added: list[dict[str, Any]],
    clip_dedupe_days: int,
) -> dict[str, Any] | None:
    """Return the longer parent episode if `video` looks like an excerpt from the same channel."""
    if clip_dedupe_days <= 0 or video.duration_sec <= 0:
        return None
    vid_dt = _parse_iso_dt(video.published_at)
    if vid_dt is None:
        return None
    max_delta_sec = clip_dedupe_days * 86400

    for ep in existing_added:
        if ep["video_id"] == video.video_id:
            continue
        other_dur = int(ep.get("duration_sec") or 0)
        # Must be significantly shorter (<= 65% of parent AND at least 10 min shorter)
        if other_dur <= 0 or video.duration_sec > other_dur * 0.65 or (other_dur - video.duration_sec) < 600:
            continue
        other_dt = _parse_iso_dt(str(ep.get("published_at") or ""))
        if other_dt is None or abs((vid_dt - other_dt).total_seconds()) > max_delta_sec:
            continue
        other_names = _person_tokens(str(ep.get("matched_person") or ""))
        if matched_names and other_names and not (matched_names & other_names):
            continue
        return ep
    return None


class Scanner:
    def __init__(
        self,
        cfg: AppConfig,
        db: Database,
        youtube: YouTubeClient,
        ollama: OllamaClient,
        podsync: PodSyncAdminClient,
    ) -> None:
        self.cfg = cfg
        self.db = db
        self.youtube = youtube
        self.ollama = ollama
        self.podsync = podsync
        self._scan_lock = asyncio.Lock()
        self._feed_write_lock = asyncio.Lock()
        self._did_initial_sync = False

    @property
    def is_running(self) -> bool:
        return self._scan_lock.locked()

    async def rebuild_feed_file_async(self) -> None:
        async with self._feed_write_lock:
            added = self.db.list_episodes(status="added", limit=self.cfg.feed_max_episodes)
            await asyncio.to_thread(write_feed_json, self.cfg, added)

    def rebuild_feed_file_sync(self) -> None:
        added = self.db.list_episodes(status="added", limit=self.cfg.feed_max_episodes)
        write_feed_json(self.cfg, added)

    async def run_scan(self, trigger: str = "scheduled") -> ScanSummary:
        if self._scan_lock.locked():
            latest = self.db.latest_scan_run()
            return ScanSummary(
                run_id=int(latest["id"]) if latest else 0,
                status="already_running",
                candidates=0,
                added=0,
                filtered=0,
            )

        async with self._scan_lock:
            run_id = self.db.start_scan_run(trigger=trigger)
            candidates_count = 0
            added_count = 0
            filtered_count = 0
            sync_result: dict[str, Any] | None = None

            try:
                settings = self.db.get_settings()
                people = self.db.list_people(enabled_only=True)

                # Precompile exclude regexes once per scan
                compiled_regex: dict[int, re.Pattern[str] | None] = {}
                for p in people:
                    raw_rx = str(p.get("exclude_regex") or "").strip()
                    if raw_rx:
                        try:
                            compiled_regex[int(p["id"])] = re.compile(raw_rx)
                        except re.error:
                            logger.warning("Invalid exclude_regex %r for %s", raw_rx, p["name"])
                            compiled_regex[int(p["id"])] = None
                    else:
                        compiled_regex[int(p["id"])] = None

                # Collect ALL matching person rules per video_id before filtering/classifying
                # so overlapping description footers never block another watched person's match.
                grouped_candidates: dict[str, tuple[CandidateVideo, list[dict[str, Any]]]] = {}

                for person in people:
                    videos = await self.youtube.search_long_videos(
                        query=person["query"],
                        max_age_days=int(person["max_age_days"]),
                        min_duration_sec=int(person["min_duration_sec"]),
                    )
                    for video in videos:
                        if self.db.episode_exists(video.video_id):
                            continue
                        if video.video_id not in grouped_candidates:
                            grouped_candidates[video.video_id] = (video, [person])
                        else:
                            grouped_candidates[video.video_id][1].append(person)

                # Also poll channels marked trusted_all (e.g. a guest's own channel)
                trusted_all_channels = self.db.list_channels(policy="trusted_all")
                for ch in trusted_all_channels:
                    videos = await self.youtube.list_channel_recent_videos(
                        channel_id=ch["channel_id"],
                        max_age_days=14,
                    )
                    pseudo_rule = {
                        "id": -1,
                        "name": ch["channel_title"],
                        "min_duration_sec": 1200,
                        "exclude_regex": "",
                    }
                    for video in videos:
                        if self.db.episode_exists(video.video_id):
                            continue
                        if video.video_id not in grouped_candidates:
                            grouped_candidates[video.video_id] = (video, [pseudo_rule])
                        else:
                            grouped_candidates[video.video_id][1].append(pseudo_rule)

                clip_dedupe_days = int(settings.get("clip_dedupe_days", 5))
                # Sort longest-first so full interviews are always added before shorter clips from the same channel
                sorted_candidates = sorted(
                    grouped_candidates.values(),
                    key=lambda item: item[0].duration_sec,
                    reverse=True,
                )

                pending_for_llm: list[tuple[CandidateVideo, list[dict[str, Any]]]] = []
                for video, matched_rules in sorted_candidates:
                    outcome, eligible_rules = self._evaluate_deterministic(
                        video, matched_rules, compiled_regex, clip_dedupe_days
                    )
                    if outcome == "skip":
                        continue
                    candidates_count += 1
                    if outcome == "llm":
                        pending_for_llm.append((video, eligible_rules))
                    elif outcome == "added":
                        added_count += 1
                    elif outcome == "filtered":
                        filtered_count += 1

                # Batch unknown candidates (still longest-first) through Ollama back-to-back
                for video, eligible_rules in pending_for_llm:
                    stored_status = await self._classify_and_store(video, eligible_rules, settings)
                    if stored_status == "added":
                        added_count += 1
                    elif stored_status == "filtered":
                        filtered_count += 1

                await self.rebuild_feed_file_async()
                if added_count > 0 or not self._did_initial_sync:
                    sync_result = await self.podsync.trigger_sync()
                    self._did_initial_sync = True

                self.db.finish_scan_run(
                    run_id=run_id,
                    status="completed",
                    candidates=candidates_count,
                    added=added_count,
                    filtered=filtered_count,
                )
                return ScanSummary(
                    run_id=run_id,
                    status="completed",
                    candidates=candidates_count,
                    added=added_count,
                    filtered=filtered_count,
                    sync_result=sync_result,
                )
            except asyncio.CancelledError:
                self.db.finish_scan_run(
                    run_id=run_id,
                    status="cancelled",
                    candidates=candidates_count,
                    added=added_count,
                    filtered=filtered_count,
                    error="scan cancelled",
                )
                raise
            except Exception as exc:
                logger.exception("Scan run %s failed", run_id)
                self.db.finish_scan_run(
                    run_id=run_id,
                    status="failed",
                    candidates=candidates_count,
                    added=added_count,
                    filtered=filtered_count,
                    error=str(exc)[:500],
                )
                return ScanSummary(
                    run_id=run_id,
                    status="failed",
                    candidates=candidates_count,
                    added=added_count,
                    filtered=filtered_count,
                    error=str(exc),
                )

    def _evaluate_deterministic(
        self,
        video: CandidateVideo,
        matched_rules: list[dict[str, Any]],
        compiled_regex: dict[int, re.Pattern[str] | None],
        clip_dedupe_days: int = 5,
    ) -> tuple[str, list[dict[str, Any]]]:
        """Apply channel policy & hard filters across all matched rules."""
        ch = self.db.upsert_channel(
            channel_id=video.channel_id,
            channel_title=video.channel_title,
            subscriber_count=video.subscriber_count,
        )
        policy = ch["policy"]

        # Skip live/upcoming streams and just-ended streams whose VOD duration is still 0s
        if video.live_broadcast_content in {"live", "upcoming"}:
            return "skip", []
        if video.duration_sec == 0 and _is_recently_published(video.published_at, _RECENT_ZERO_DUR_GRACE):
            return "skip", []

        primary_name = ", ".join(r["name"] for r in matched_rules)

        if policy == "blocked":
            self._store_if_absent(
                video=video,
                matched_person=primary_name,
                status="filtered",
                filter_source="channel_block",
                filter_reason="Channel is blocked",
            )
            return "filtered"

        if policy not in {"trusted_matching", "trusted_all"}:
            non_en = _detect_non_english(video)
            if non_en:
                self._store_if_absent(
                    video=video,
                    matched_person=primary_name,
                    status="filtered",
                    filter_source="language",
                    filter_reason=non_en,
                )
                return "filtered", []

        # Keep rules whose duration & exclude_regex allow this video
        eligible: list[dict[str, Any]] = []
        last_reject_source = "duration"
        last_reject_reason = "Filtered by hard rules"
        for rule in matched_rules:
            min_dur = int(rule.get("min_duration_sec") or 0)
            if video.duration_sec < min_dur:
                last_reject_source = "duration"
                last_reject_reason = (
                    f"Shorter than {min_dur // 60}m minimum ({video.duration_sec // 60}m {video.duration_sec % 60:02d}s)"
                )
                continue
            rx = compiled_regex.get(int(rule.get("id", -1)))
            if rx is not None and rx.search(video.title):
                last_reject_source = "regex"
                last_reject_reason = "Title matched exclude regex"
                continue
            eligible.append(rule)

        if not eligible:
            self._store_if_absent(
                video=video,
                matched_person=primary_name,
                status="filtered",
                filter_source=last_reject_source,
                filter_reason=last_reject_reason,
            )
            return "filtered", []

        # Same-channel clip check runs even on trusted channels (e.g. Daniel Davis / Tucker clips)
        matched_names = {r["name"].strip().lower() for r in eligible if r.get("name")}
        parent = _is_same_channel_clip(
            video,
            matched_names,
            self.db.list_added_by_channel(video.channel_id),
            clip_dedupe_days,
        )
        if parent is not None:
            self._store_if_absent(
                video=video,
                matched_person=", ".join(r["name"] for r in eligible),
                status="filtered",
                filter_source="same_channel_clip",
                filter_reason=(
                    f"Likely clip of longer episode from same channel "
                    f"({video.duration_sec // 60}m vs {int(parent['duration_sec']) // 60}m): {parent['title'][:80]}"
                ),
            )
            return "filtered", []

        if policy in {"trusted_matching", "trusted_all"}:
            self._store_if_absent(
                video=video,
                matched_person=", ".join(r["name"] for r in eligible),
                status="added",
                filter_source="channel_trust",
                filter_reason=f"Auto-added from trusted channel ({policy})",
            )
            return "added", []

        return "llm", eligible

    async def _classify_and_store(
        self,
        video: CandidateVideo,
        eligible_rules: list[dict[str, Any]],
        settings: dict[str, Any],
    ) -> str:
        ollama_enabled = bool(settings.get("ollama_enabled", True))
        model = str(settings.get("ollama_model") or self.cfg.ollama_default_model)
        min_subs = int(settings.get("fallback_min_subscribers", 10000))
        clip_dedupe_days = int(settings.get("clip_dedupe_days", 5))

        # Re-check same-channel clip against any longer video added earlier in this same LLM batch
        matched_names = {r["name"].strip().lower() for r in eligible_rules if r.get("name")}
        parent = _is_same_channel_clip(
            video,
            matched_names,
            self.db.list_added_by_channel(video.channel_id),
            clip_dedupe_days,
        )
        if parent is not None:
            if self._store_if_absent(
                video=video,
                matched_person=", ".join(r["name"] for r in eligible_rules),
                status="filtered",
                filter_source="same_channel_clip",
                filter_reason=(
                    f"Likely clip of longer episode from same channel "
                    f"({video.duration_sec // 60}m vs {int(parent['duration_sec']) // 60}m): {parent['title'][:80]}"
                ),
            ):
                return "filtered"
            return "skipped"

        # Re-check channel policy right before LLM classification in case user blocked it mid-scan
        ch = self.db.get_channel(video.channel_id)
        if ch and ch["policy"] == "blocked":
            if self._store_if_absent(
                video=video,
                matched_person=", ".join(r["name"] for r in eligible_rules),
                status="filtered",
                filter_source="channel_block",
                filter_reason="Channel was blocked during scan",
            ):
                return "filtered"
            return "skipped"

        chosen_person = eligible_rules[0]["name"]
        result: ClassificationResult | None = None

        if ollama_enabled:
            for rule in eligible_rules:
                try:
                    candidate_res = await self.ollama.classify_video(
                        model=model,
                        person_name=rule["name"],
                        title=video.title,
                        channel_title=video.channel_title,
                        subscriber_count=video.subscriber_count,
                        duration_sec=video.duration_sec,
                        description=video.description,
                    )
                    chosen_person = rule["name"]
                    result = candidate_res
                    if candidate_res.verdict == "authentic":
                        break
                except Exception as exc:
                    logger.warning("Ollama classification failed for %s (%s), using subscriber fallback", video.video_id, exc)
                    chosen_person = rule["name"]
                    result = self._subscriber_fallback(video.subscriber_count, min_subs, f"Ollama error: {exc}")
                    break
        else:
            result = self._subscriber_fallback(video.subscriber_count, min_subs, "Ollama disabled")

        assert result is not None

        # Re-check channel policy AFTER awaiting Ollama so a concurrent block is respected
        ch_after = self.db.get_channel(video.channel_id)
        if ch_after and ch_after["policy"] == "blocked":
            if self._store_if_absent(
                video=video,
                matched_person=chosen_person,
                status="filtered",
                filter_source="channel_block",
                filter_reason="Channel was blocked during scan",
            ):
                return "filtered"
            return "skipped"

        target_status = "added" if result.verdict == "authentic" else "filtered"
        inserted = self._store_if_absent(
            video=video,
            matched_person=chosen_person,
            status=target_status,
            filter_source="fallback_subs" if result.used_fallback else f"ollama:{model}",
            filter_reason=result.reason,
            llm_verdict=result.verdict,
            llm_confidence=result.confidence,
        )
        return target_status if inserted else "skipped"

    @staticmethod
    def _subscriber_fallback(subs: int, min_subs: int, prefix: str) -> ClassificationResult:
        if subs >= min_subs:
            return ClassificationResult(
                verdict="authentic",
                confidence=0.6,
                reason=f"{prefix}; channel has {subs:,} subs (>= {min_subs:,})",
                used_fallback=True,
            )
        return ClassificationResult(
            verdict="spam_or_clip",
            confidence=0.6,
            reason=f"{prefix}; channel has {subs:,} subs (< {min_subs:,})",
            used_fallback=True,
        )

    def _store_if_absent(
        self,
        video: CandidateVideo,
        matched_person: str,
        status: str,
        filter_source: str,
        filter_reason: str,
        llm_verdict: str = "",
        llm_confidence: float | None = None,
    ) -> bool:
        return self.db.save_episode_if_absent(
            {
                "video_id": video.video_id,
                "title": video.title,
                "description": video.description,
                "channel_id": video.channel_id,
                "channel_title": video.channel_title,
                "subscriber_count": video.subscriber_count,
                "duration_sec": video.duration_sec,
                "published_at": video.published_at,
                "thumbnail_url": video.thumbnail_url,
                "video_url": video.video_url,
                "matched_person": matched_person,
                "status": status,
                "filter_source": filter_source,
                "filter_reason": filter_reason,
                "llm_verdict": llm_verdict,
                "llm_confidence": llm_confidence,
            }
        )
