from __future__ import annotations

import itertools
import re
from dataclasses import dataclass
from datetime import datetime, timedelta, timezone
from typing import Any, Sequence

import httpx

YOUTUBE_API_BASE = "https://www.googleapis.com/youtube/v3"
_ISO_DUR_RE = re.compile(r"^P(?:(\d+)D)?(?:T(?:(\d+)H)?(?:(\d+)M)?(?:(\d+)S)?)?$")


@dataclass(frozen=True)
class CandidateVideo:
    video_id: str
    title: str
    description: str
    channel_id: str
    channel_title: str
    subscriber_count: int
    duration_sec: int
    published_at: str
    thumbnail_url: str
    video_url: str
    live_broadcast_content: str = "none"
    default_audio_language: str = ""
    default_language: str = ""


def parse_iso8601_duration(raw: str) -> int:
    if not raw:
        return 0
    s = raw.strip()
    if s in {"P", "PT"}:
        return 0
    m = _ISO_DUR_RE.match(s)
    if not m:
        return 0
    days = int(m.group(1) or 0)
    hours = int(m.group(2) or 0)
    minutes = int(m.group(3) or 0)
    seconds = int(m.group(4) or 0)
    return days * 86400 + hours * 3600 + minutes * 60 + seconds


def pick_thumbnail(thumbnails: dict[str, Any] | None, video_id: str) -> str:
    if thumbnails:
        for key in ("high", "medium", "standard", "maxres", "default"):
            item = thumbnails.get(key)
            if isinstance(item, dict) and item.get("url"):
                return str(item["url"])
    return f"https://i.ytimg.com/vi/{video_id}/hqdefault.jpg"


class YouTubeClient:
    def __init__(self, api_keys: str | Sequence[str], timeout_sec: float = 30.0) -> None:
        if isinstance(api_keys, str):
            keys = tuple(api_keys.split())
        else:
            keys = tuple(k.strip() for k in api_keys if k and k.strip())
        self.api_keys = keys
        self._key_cycle = itertools.cycle(keys) if keys else None
        self.timeout_sec = timeout_sec
        self._client = httpx.AsyncClient(timeout=timeout_sec)

    async def aclose(self) -> None:
        await self._client.aclose()

    def _headers(self) -> dict[str, str]:
        if self._key_cycle is None:
            raise RuntimeError("YOUTUBE_API_KEY is not configured")
        return {"X-Goog-Api-Key": next(self._key_cycle)}

    async def search_long_videos(
        self,
        query: str,
        max_age_days: int = 14,
        max_results: int = 25,
        min_duration_sec: int = 1200,
    ) -> list[CandidateVideo]:
        published_after = (
            datetime.now(timezone.utc) - timedelta(days=max(1, max_age_days))
        ).replace(microsecond=0).isoformat().replace("+00:00", "Z")

        params: dict[str, Any] = {
            "part": "id",
            "type": "video",
            "order": "date",
            "q": query,
            "publishedAfter": published_after,
            "relevanceLanguage": "en",
            "maxResults": min(50, max(1, max_results)),
        }
        if min_duration_sec >= 1200:
            params["videoDuration"] = "long"

        resp = await self._client.get(
            f"{YOUTUBE_API_BASE}/search",
            headers=self._headers(),
            params=params,
        )
        resp.raise_for_status()
        search_data = resp.json()

        video_ids: list[str] = []
        seen: set[str] = set()
        for item in search_data.get("items", []):
            vid = (item.get("id") or {}).get("videoId")
            if vid and vid not in seen:
                seen.add(vid)
                video_ids.append(vid)

        if not video_ids:
            return []

        return await self._hydrate_videos(video_ids)

    async def list_channel_recent_videos(
        self,
        channel_id: str,
        max_age_days: int = 14,
        max_results: int = 15,
    ) -> list[CandidateVideo]:
        """Cheaply list recent uploads from a trusted_all channel (3-5 API units)."""
        cutoff = datetime.now(timezone.utc) - timedelta(days=max(1, max_age_days))
        ch_resp = await self._client.get(
            f"{YOUTUBE_API_BASE}/channels",
            headers=self._headers(),
            params={"part": "contentDetails", "id": channel_id},
        )
        ch_resp.raise_for_status()
        items = ch_resp.json().get("items", [])
        if not items:
            return []
        uploads_playlist = items[0].get("contentDetails", {}).get("relatedPlaylists", {}).get("uploads")
        if not uploads_playlist:
            return []

        pl_resp = await self._client.get(
            f"{YOUTUBE_API_BASE}/playlistItems",
            headers=self._headers(),
            params={
                "part": "snippet",
                "playlistId": uploads_playlist,
                "maxResults": min(50, max(1, max_results)),
            },
        )
        pl_resp.raise_for_status()

        video_ids: list[str] = []
        for item in pl_resp.json().get("items", []):
            snippet = item.get("snippet") or {}
            pub_raw = snippet.get("publishedAt", "")
            if pub_raw:
                try:
                    pub_dt = datetime.fromisoformat(pub_raw.replace("Z", "+00:00"))
                    if pub_dt < cutoff:
                        continue
                except ValueError:
                    pass
            vid = (snippet.get("resourceId") or {}).get("videoId")
            if vid:
                video_ids.append(vid)

        if not video_ids:
            return []

        return await self._hydrate_videos(video_ids)

    async def _hydrate_videos(self, video_ids: list[str]) -> list[CandidateVideo]:
        videos_raw: list[dict[str, Any]] = []
        for i in range(0, len(video_ids), 50):
            batch = video_ids[i : i + 50]
            resp = await self._client.get(
                f"{YOUTUBE_API_BASE}/videos",
                headers=self._headers(),
                params={"part": "snippet,contentDetails", "id": ",".join(batch)},
            )
            resp.raise_for_status()
            videos_raw.extend(resp.json().get("items", []))

        channel_ids = sorted(
            {
                str((v.get("snippet") or {}).get("channelId", "")).strip()
                for v in videos_raw
                if (v.get("snippet") or {}).get("channelId")
            }
        )
        sub_counts: dict[str, int] = {}
        for i in range(0, len(channel_ids), 50):
            batch = channel_ids[i : i + 50]
            ch_resp = await self._client.get(
                f"{YOUTUBE_API_BASE}/channels",
                headers=self._headers(),
                params={"part": "statistics", "id": ",".join(batch)},
            )
            ch_resp.raise_for_status()
            for ch in ch_resp.json().get("items", []):
                cid = ch.get("id", "")
                stats = ch.get("statistics") or {}
                if cid:
                    try:
                        sub_counts[cid] = int(stats.get("subscriberCount") or 0)
                    except (ValueError, TypeError):
                        sub_counts[cid] = 0

        candidates: list[CandidateVideo] = []
        for v in videos_raw:
            vid = str(v.get("id") or "").strip()
            snippet = v.get("snippet") or {}
            details = v.get("contentDetails") or {}
            if not vid or not snippet:
                continue
            cid = str(snippet.get("channelId") or "").strip()
            candidates.append(
                CandidateVideo(
                    video_id=vid,
                    title=str(snippet.get("title") or vid).strip(),
                    description=str(snippet.get("description") or "").strip(),
                    channel_id=cid,
                    channel_title=str(snippet.get("channelTitle") or cid).strip(),
                    subscriber_count=sub_counts.get(cid, 0),
                    duration_sec=parse_iso8601_duration(str(details.get("duration") or "")),
                    published_at=str(snippet.get("publishedAt") or ""),
                    thumbnail_url=pick_thumbnail(snippet.get("thumbnails"), vid),
                    video_url=f"https://www.youtube.com/watch?v={vid}",
                    live_broadcast_content=str(snippet.get("liveBroadcastContent") or "none"),
                    default_audio_language=str(snippet.get("defaultAudioLanguage") or "").strip(),
                    default_language=str(snippet.get("defaultLanguage") or "").strip(),
                )
            )
        return candidates
