from __future__ import annotations

import json
from datetime import datetime, timezone
from pathlib import Path
from typing import Any

import pytest
from fastapi.testclient import TestClient

from app.config import AppConfig
from app.main import create_app
from app.ollama import ClassificationResult, parse_llm_json
from app.youtube import CandidateVideo, parse_iso8601_duration

CSRF = {"X-Scout-Request": "1"}


class FakeYouTubeClient:
    def __init__(self, videos_by_query: dict[str, list[CandidateVideo]] | None = None) -> None:
        self.videos_by_query = videos_by_query or {}
        self.channel_videos: dict[str, list[CandidateVideo]] = {}

    async def aclose(self) -> None:
        pass

    async def search_long_videos(
        self, query: str, max_age_days: int = 14, max_results: int = 25, min_duration_sec: int = 1200
    ) -> list[CandidateVideo]:
        return list(self.videos_by_query.get(query, []))

    async def list_channel_recent_videos(
        self, channel_id: str, max_age_days: int = 14, max_results: int = 15
    ) -> list[CandidateVideo]:
        return list(self.channel_videos.get(channel_id, []))


class FakeOllamaClient:
    def __init__(
        self,
        verdicts: dict[str, ClassificationResult] | None = None,
        verdicts_by_pair: dict[tuple[str, str], ClassificationResult] | None = None,
        fail: bool = False,
    ) -> None:
        self.verdicts = verdicts or {}
        self.verdicts_by_pair = verdicts_by_pair or {}
        self.fail = fail
        self.calls: list[tuple[str, str]] = []

    async def aclose(self) -> None:
        pass

    async def list_models(self) -> list[dict[str, Any]]:
        if self.fail:
            raise RuntimeError("ollama unreachable")
        return [{"name": "llama3.1:8b", "size_gb": 4.9}]

    async def classify_video(
        self,
        model: str,
        person_name: str,
        title: str,
        channel_title: str,
        subscriber_count: int,
        duration_sec: int,
        description: str,
    ) -> ClassificationResult:
        self.calls.append((person_name, title))
        if self.fail:
            raise RuntimeError("connection refused")
        if (person_name, title) in self.verdicts_by_pair:
            return self.verdicts_by_pair[(person_name, title)]
        if title in self.verdicts:
            return self.verdicts[title]
        return ClassificationResult(verdict="authentic", confidence=0.9, reason="Looks like a genuine interview")


class FakePodSyncClient:
    def __init__(self) -> None:
        self.sync_calls = 0
        self.deleted_ids: list[str] = []

    async def aclose(self) -> None:
        pass

    async def trigger_sync(self) -> dict[str, Any]:
        self.sync_calls += 1
        return {"ok": True, "status": 202}

    async def delete_episode(self, video_id: str) -> dict[str, Any]:
        self.deleted_ids.append(video_id)
        return {"ok": True, "deleted": True, "status": 200}


def make_cfg(tmp_path: Path, scout_token: str = "") -> AppConfig:
    return AppConfig(
        db_path=tmp_path / "db" / "scout.db",
        feeds_dir=tmp_path / "feeds",
        feed_id="AutoPodcasts",
        feed_title="Auto Podcasts",
        feed_description="Test auto podcasts",
        feed_author="PodSync Scout",
        feed_link="https://podsync.example.com/scout/",
        feed_max_episodes=50,
        youtube_api_keys=("test-yt-key",),
        ollama_url="http://ollama.example:11434",
        ollama_default_model="llama3.1:8b",
        ollama_timeout_sec=30.0,
        podsync_admin_url="http://podsync:8095",
        podsync_admin_token="test-admin-token",
        scan_interval_minutes=360,
        base_path="/scout",
        scout_token=scout_token,
    )


def seed_default_people(app: Any) -> None:
    app.state.db.create_person(name="Alice Example", min_duration_sec=1200, max_age_days=14)
    app.state.db.create_person(name="Bob Example", min_duration_sec=1500, max_age_days=14)


def test_parse_iso8601_duration() -> None:
    assert parse_iso8601_duration("PT1H48M08S") == 6488
    assert parse_iso8601_duration("PT23M42S") == 1422
    assert parse_iso8601_duration("PT45S") == 45
    assert parse_iso8601_duration("P1DT2H3M4S") == 86400 + 7200 + 180 + 4
    assert parse_iso8601_duration("P0D") == 0
    assert parse_iso8601_duration("") == 0
    assert parse_iso8601_duration("invalid") == 0


def test_parse_llm_json_edge_cases() -> None:
    v, c, r = parse_llm_json('{"verdict": "spam_or_clip", "confidence": 0.94, "reason": "Promo reupload"}')
    assert v == "spam_or_clip"
    assert c == pytest.approx(0.94)
    assert r == "Promo reupload"

    # Negated forms must NEVER invert to "authentic"
    for neg in ("inauthentic", "not_authentic", "unauthentic", "not authentic"):
        v_neg, _, _ = parse_llm_json(f'{{"verdict": "{neg}", "confidence": 0.9, "reason": "Fake"}}')
        assert v_neg == "spam_or_clip", f"{neg} flipped to authentic"

    # <think> block containing braces must be stripped before parsing JSON
    raw_think = '<think>Maybe {this is not json} let me check.</think>\n```json\n{"verdict":"authentic","confidence":0.88,"reason":"Long-form interview"}\n```'
    v2, _, _ = parse_llm_json(raw_think)
    assert v2 == "authentic"


def test_full_scan_filter_block_purge_and_rescue(tmp_path: Path) -> None:
    now_iso = datetime.now(timezone.utc).replace(microsecond=0).isoformat().replace("+00:00", "Z")
    host_vid = CandidateVideo(
        video_id="host123",
        title="Full Episode: Deep Macro Discussion",
        description="Bob Example joins to discuss macroeconomics.",
        channel_id="UC_HOST",
        channel_title="Major Podcast Host",
        subscriber_count=3_500_000,
        duration_sec=6488,
        published_at="2026-11-24T01:00:00Z",
        thumbnail_url="https://i.ytimg.com/vi/host123/hqdefault.jpg",
        video_url="https://www.youtube.com/watch?v=host123",
    )
    spam_vid = CandidateVideo(
        video_id="spam999",
        title="HUGE MOVE! PRICE TARGET — Bob Example",
        description="Promotional reupload",
        channel_id="UC_SPAM",
        channel_title="Clickbait Finance Clips",
        subscriber_count=4_200,
        duration_sec=1600,
        published_at="2026-11-23T12:00:00Z",
        thumbnail_url="https://i.ytimg.com/vi/spam999/hqdefault.jpg",
        video_url="https://www.youtube.com/watch?v=spam999",
    )
    live_vid = CandidateVideo(
        video_id="live777",
        title="Alice Example Live Now",
        description="Live stream",
        channel_id="UC_SHOW",
        channel_title="Weekly Show",
        subscriber_count=500_000,
        duration_sec=1800,
        published_at=now_iso,
        thumbnail_url="https://i.ytimg.com/vi/live777/hqdefault.jpg",
        video_url="https://www.youtube.com/watch?v=live777",
        live_broadcast_content="live",
    )
    just_ended_vid = CandidateVideo(
        video_id="ended000",
        title="Alice Example Just Ended Stream",
        description="VOD duration still 0",
        channel_id="UC_SHOW",
        channel_title="Weekly Show",
        subscriber_count=500_000,
        duration_sec=0,
        published_at=now_iso,
        thumbnail_url="https://i.ytimg.com/vi/ended000/hqdefault.jpg",
        video_url="https://www.youtube.com/watch?v=ended000",
        live_broadcast_content="none",
    )

    yt = FakeYouTubeClient(
        {
            '"Bob Example"': [host_vid, spam_vid],
            '"Alice Example"': [live_vid, just_ended_vid],
        }
    )
    ollama = FakeOllamaClient(
        {
            host_vid.title: ClassificationResult("authentic", 0.96, "Full interview on major host"),
            spam_vid.title: ClassificationResult("spam_or_clip", 0.98, "Clickbait promo channel"),
        }
    )
    podsync = FakePodSyncClient()
    cfg = make_cfg(tmp_path)

    app = create_app(cfg, youtube_client=yt, ollama_client=ollama, podsync_client=podsync, enable_scheduler=False)
    seed_default_people(app)
    with TestClient(app) as client:
        hz = client.get("/healthz")
        assert hz.status_code == 200
        assert hz.headers["x-content-type-options"] == "nosniff"
        assert hz.headers["x-frame-options"] == "DENY"
        assert "frame-ancestors 'none'" in hz.headers["content-security-policy"]

        assert client.get("/scout/").status_code == 200

        # CSRF check blocks mutating requests without X-Scout-Request header or from cross-site
        assert client.post("/scout/api/scan").status_code == 403
        assert client.post("/scout/api/scan", headers={"X-Scout-Request": "1", "Sec-Fetch-Site": "cross-site"}).status_code == 403

        # Trigger synchronous scan via ?wait=true
        scan_resp = client.post("/scout/api/scan?wait=true", headers=CSRF)
        assert scan_resp.status_code == 200
        scan = scan_resp.json()
        assert scan["status"] == "completed"
        assert scan["candidates"] == 2  # live & just-ended 0s skipped and not persisted
        assert scan["added"] == 1
        assert scan["filtered"] == 1
        assert podsync.sync_calls == 1

        # Just-ended stream must not have been permanently persisted
        assert not app.state.db.episode_exists("ended000")
        assert not app.state.db.episode_exists("live777")

        # Verify AutoPodcasts.json contains only host123
        feed_data = json.loads(cfg.feed_json_path.read_text(encoding="utf-8"))
        assert feed_data["title"] == "Auto Podcasts"
        assert [e["id"] for e in feed_data["episodes"]] == ["host123"]
        assert feed_data["episodes"][0]["video_url"] == "https://www.youtube.com/watch?v=host123"

        # Rescue spam999 + trust channel
        grab_resp = client.post(
            "/scout/api/episodes/spam999/grab",
            headers=CSRF,
            json={"trust_channel": True, "trust_mode": "trusted_matching"},
        )
        assert grab_resp.status_code == 200
        assert podsync.sync_calls == 2
        feed_data = json.loads(cfg.feed_json_path.read_text(encoding="utf-8"))
        assert {e["id"] for e in feed_data["episodes"]} == {"host123", "spam999"}

        # Now block UC_SPAM & purge existing
        block_resp = client.put(
            "/scout/api/channels/UC_SPAM",
            headers=CSRF,
            json={"policy": "blocked", "channel_title": "Clickbait Finance Clips", "purge_existing": True},
        )
        assert block_resp.status_code == 200
        assert block_resp.json()["purged_video_ids"] == ["spam999"]
        assert podsync.deleted_ids == ["spam999"]

        feed_data = json.loads(cfg.feed_json_path.read_text(encoding="utf-8"))
        assert [e["id"] for e in feed_data["episodes"]] == ["host123"]

        # Async POST /scout/api/scan returns 202 Accepted immediately
        async_resp = client.post("/scout/api/scan", headers=CSRF)
        assert async_resp.status_code == 202


def test_cross_person_overlap_does_not_filter_genuine_match(tmp_path: Path) -> None:
    """If a Bob Example interview also matches Alice Example via description footer,
    evaluating Alice first must not permanently filter out the Bob match."""
    shared_vid = CandidateVideo(
        video_id="overlap1",
        title="Bob Example: Long-Form Interview",
        description="Full interview with Bob Example. Previous guests: Alice Example.",
        channel_id="UC_PODCAST",
        channel_title="Interview Podcast",
        subscriber_count=400_000,
        duration_sec=2400,
        published_at="2026-11-24T01:00:00Z",
        thumbnail_url="https://i.ytimg.com/vi/overlap1/hqdefault.jpg",
        video_url="https://www.youtube.com/watch?v=overlap1",
    )

    yt = FakeYouTubeClient(
        {
            '"Alice Example"': [shared_vid],
            '"Bob Example"': [shared_vid],
        }
    )
    ollama = FakeOllamaClient(
        verdicts_by_pair={
            ("Alice Example", shared_vid.title): ClassificationResult("spam_or_clip", 0.9, "Footer mention only"),
            ("Bob Example", shared_vid.title): ClassificationResult("authentic", 0.97, "Full Bob Example interview"),
        }
    )
    podsync = FakePodSyncClient()
    cfg = make_cfg(tmp_path)

    app = create_app(cfg, youtube_client=yt, ollama_client=ollama, podsync_client=podsync, enable_scheduler=False)
    seed_default_people(app)
    with TestClient(app) as client:
        scan = client.post("/scout/api/scan?wait=true", headers=CSRF).json()
        assert scan["added"] == 1
        assert scan["filtered"] == 0
        added = client.get("/scout/api/episodes?status=added").json()["episodes"]
        assert [e["video_id"] for e in added] == ["overlap1"]
        assert added[0]["matched_person"] == "Bob Example"


def test_ollama_offline_fallback_auth_and_people_errors(tmp_path: Path) -> None:
    big_ch = CandidateVideo(
        video_id="big1",
        title="Alice Example Discussion",
        description="Full episode",
        channel_id="UC_BIG",
        channel_title="Established Show",
        subscriber_count=250_000,
        duration_sec=1500,
        published_at="2026-11-24T01:00:00Z",
        thumbnail_url="https://i.ytimg.com/vi/big1/hqdefault.jpg",
        video_url="https://www.youtube.com/watch?v=big1",
    )
    tiny_ch = CandidateVideo(
        video_id="tiny1",
        title="Alice Example Reupload",
        description="Reupload",
        channel_id="UC_TINY",
        channel_title="AI News Clip",
        subscriber_count=1_200,
        duration_sec=1500,
        published_at="2026-11-24T01:00:00Z",
        thumbnail_url="https://i.ytimg.com/vi/tiny1/hqdefault.jpg",
        video_url="https://www.youtube.com/watch?v=tiny1",
    )

    yt = FakeYouTubeClient({'"Alice Example"': [big_ch, tiny_ch]})
    ollama = FakeOllamaClient(fail=True)
    podsync = FakePodSyncClient()
    cfg = make_cfg(tmp_path, scout_token="super-secret-scout-token")

    app = create_app(cfg, youtube_client=yt, ollama_client=ollama, podsync_client=podsync, enable_scheduler=False)
    seed_default_people(app)
    with TestClient(app) as client:
        assert client.get("/scout/api/status").status_code == 401
        headers = {"Authorization": "Bearer super-secret-scout-token", **CSRF}
        assert client.get("/scout/api/status", headers=headers).status_code == 200

        scan = client.post("/scout/api/scan?wait=true", headers=headers).json()
        assert scan["added"] == 1
        assert scan["filtered"] == 1

        added = client.get("/scout/api/episodes?status=added", headers=headers).json()["episodes"]
        filtered = client.get("/scout/api/episodes?status=filtered", headers=headers).json()["episodes"]
        assert [e["video_id"] for e in added] == ["big1"]
        assert [e["video_id"] for e in filtered] == ["tiny1"]
        assert filtered[0]["filter_source"] == "fallback_subs"

        # People validation error cases: whitespace name -> 400, duplicate name -> 409
        people = client.get("/scout/api/people", headers=headers).json()["people"]
        first_id = people[0]["id"]
        assert client.put(f"/scout/api/people/{first_id}", headers=headers, json={"name": "   "}).status_code == 400
        assert client.put(f"/scout/api/people/{first_id}", headers=headers, json={"name": people[1]["name"]}).status_code == 409


def test_non_english_dubs_auto_filtered_before_ollama(tmp_path: Path) -> None:
    dub_it = CandidateVideo(
        video_id="it1",
        title="Intervista completa | Alice Example",
        description="Italian dub",
        channel_id="UC_SHOW_IT",
        channel_title="Deep Dive Italiano",
        subscriber_count=3500,
        duration_sec=1400,
        published_at="2026-11-24T01:00:00Z",
        thumbnail_url="https://i.ytimg.com/vi/it1/hqdefault.jpg",
        video_url="https://www.youtube.com/watch?v=it1",
        default_audio_language="it",
    )
    dub_es = CandidateVideo(
        video_id="es1",
        title="Alice Example entrevista",
        description="Spanish dub",
        channel_id="UC_SHOW_ES",
        channel_title="Podcast Español",
        subscriber_count=50000,
        duration_sec=2600,
        published_at="2026-11-24T01:00:00Z",
        thumbnail_url="https://i.ytimg.com/vi/es1/hqdefault.jpg",
        video_url="https://www.youtube.com/watch?v=es1",
    )
    en_orig = CandidateVideo(
        video_id="en1",
        title="Alice Example: Full Interview",
        description="Original English",
        channel_id="UC_SHOW_EN",
        channel_title="Deep Dive Show",
        subscriber_count=480000,
        duration_sec=1500,
        published_at="2026-11-24T01:00:00Z",
        thumbnail_url="https://i.ytimg.com/vi/en1/hqdefault.jpg",
        video_url="https://www.youtube.com/watch?v=en1",
        default_audio_language="en-US",
    )

    yt = FakeYouTubeClient({'"Alice Example"': [dub_it, dub_es, en_orig]})
    ollama = FakeOllamaClient()
    podsync = FakePodSyncClient()
    cfg = make_cfg(tmp_path)

    app = create_app(cfg, youtube_client=yt, ollama_client=ollama, podsync_client=podsync, enable_scheduler=False)
    seed_default_people(app)
    with TestClient(app) as client:
        scan = client.post("/scout/api/scan?wait=true", headers=CSRF).json()
        assert scan["added"] == 1
        assert scan["filtered"] == 2
        # Ollama should ONLY have been called once (for the English original!)
        assert len(ollama.calls) == 1
        assert ollama.calls[0][1] == en_orig.title


def test_same_channel_clip_detector(tmp_path: Path) -> None:
    # Full interview (75m) + 24m clip posted 2 days later (listed clip-first to verify longest-first sort!)
    show_clip = CandidateVideo(
        video_id="show_clip",
        title="Excerpt Discussion | Alice Example",
        description="Clip",
        channel_id="UC_SHOW_A",
        channel_title="Deep Dive Show",
        subscriber_count=480000,
        duration_sec=1438,
        published_at="2026-11-22T13:33:08Z",
        thumbnail_url="https://i.ytimg.com/vi/show_clip/hqdefault.jpg",
        video_url="https://www.youtube.com/watch?v=show_clip",
    )
    show_full = CandidateVideo(
        video_id="show_full",
        title="Full Panel Discussion | Alice Example",
        description="Full interview",
        channel_id="UC_SHOW_A",
        channel_title="Deep Dive Show",
        subscriber_count=480000,
        duration_sec=4523,
        published_at="2026-11-20T16:21:21Z",
        thumbnail_url="https://i.ytimg.com/vi/show_full/hqdefault.jpg",
        video_url="https://www.youtube.com/watch?v=show_full",
    )
    # Weekly shows (7 days apart, 23m & 32m) -> BOTH must be kept!
    weekly_1 = CandidateVideo(
        video_id="wk1",
        title="Alice Example: Episode 1",
        description="Weekly show 1",
        channel_id="UC_WEEKLY",
        channel_title="Weekly Commentary",
        subscriber_count=798000,
        duration_sec=1920,
        published_at="2026-11-16T15:00:00Z",
        thumbnail_url="https://i.ytimg.com/vi/wk1/hqdefault.jpg",
        video_url="https://www.youtube.com/watch?v=wk1",
    )
    weekly_2 = CandidateVideo(
        video_id="wk2",
        title="Alice Example: Episode 2",
        description="Weekly show 2",
        channel_id="UC_WEEKLY",
        channel_title="Weekly Commentary",
        subscriber_count=798000,
        duration_sec=1422,
        published_at="2026-11-23T15:00:00Z",
        thumbnail_url="https://i.ytimg.com/vi/wk2/hqdefault.jpg",
        video_url="https://www.youtube.com/watch?v=wk2",
    )

    yt = FakeYouTubeClient({'"Alice Example"': [show_clip, show_full, weekly_2, weekly_1]})
    ollama = FakeOllamaClient()
    podsync = FakePodSyncClient()
    cfg = make_cfg(tmp_path)

    app = create_app(cfg, youtube_client=yt, ollama_client=ollama, podsync_client=podsync, enable_scheduler=False)
    seed_default_people(app)
    with TestClient(app) as client:
        # Trust channel first — clip detector must STILL catch clips on trusted channels!
        client.put(
            "/scout/api/channels/UC_SHOW_A",
            headers=CSRF,
            json={"policy": "trusted_matching", "channel_title": "Deep Dive Show"},
        )

        scan = client.post("/scout/api/scan?wait=true", headers=CSRF).json()
        assert scan["added"] == 3
        assert scan["filtered"] == 1

        added = client.get("/scout/api/episodes?status=added").json()["episodes"]
        filtered = client.get("/scout/api/episodes?status=filtered").json()["episodes"]
        assert {e["video_id"] for e in added} == {"show_full", "wk1", "wk2"}
        assert [e["video_id"] for e in filtered] == ["show_clip"]
        assert filtered[0]["filter_source"] == "same_channel_clip"
