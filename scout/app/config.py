from __future__ import annotations

import os
import re
from dataclasses import dataclass
from pathlib import Path

_FEED_ID_RE = re.compile(r"^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$")


def _env_int(name: str, default: int, minimum: int) -> int:
    raw = (os.getenv(name) or "").strip()
    if not raw:
        return max(minimum, default)
    return max(minimum, int(raw))


def _env_float(name: str, default: float, minimum: float) -> float:
    raw = (os.getenv(name) or "").strip()
    if not raw:
        return max(minimum, default)
    return max(minimum, float(raw))


@dataclass(frozen=True)
class AppConfig:
    db_path: Path
    feeds_dir: Path
    feed_id: str
    feed_title: str
    feed_description: str
    feed_author: str
    feed_link: str
    feed_max_episodes: int

    youtube_api_keys: tuple[str, ...]

    ollama_url: str
    ollama_default_model: str
    ollama_timeout_sec: float

    podsync_admin_url: str
    podsync_admin_token: str

    scan_interval_minutes: int
    base_path: str
    scout_token: str

    @property
    def youtube_api_key(self) -> str:
        return self.youtube_api_keys[0] if self.youtube_api_keys else ""

    @property
    def feed_json_path(self) -> Path:
        return self.feeds_dir / f"{self.feed_id}.json"

    @classmethod
    def from_env(cls) -> AppConfig:
        base_path = (os.getenv("SCOUT_BASE_PATH") or "/scout").strip()
        if base_path and not base_path.startswith("/"):
            base_path = "/" + base_path
        base_path = base_path.rstrip("/")

        feed_id = (os.getenv("SCOUT_FEED_ID") or "AutoPodcasts").strip()
        if not _FEED_ID_RE.match(feed_id):
            raise ValueError(f"invalid SCOUT_FEED_ID {feed_id!r}: must match {_FEED_ID_RE.pattern}")

        raw_yt_key = (os.getenv("YOUTUBE_API_KEY") or "").strip()
        if not raw_yt_key:
            raw_yt_key = (os.getenv("PODSYNC_YOUTUBE_API_KEY") or "").strip()
        yt_keys = tuple(raw_yt_key.split()) if raw_yt_key else ()

        return cls(
            db_path=Path((os.getenv("SCOUT_DB_PATH") or "/data/db/scout.db").strip()),
            feeds_dir=Path((os.getenv("SCOUT_FEEDS_DIR") or "/data/feeds").strip()),
            feed_id=feed_id,
            feed_title=(os.getenv("SCOUT_FEED_TITLE") or "Auto Podcasts").strip(),
            feed_description=(
                os.getenv("SCOUT_FEED_DESCRIPTION")
                or "Automatically discovered full-length interviews curated by PodSync Scout."
            ).strip(),
            feed_author=(os.getenv("SCOUT_FEED_AUTHOR") or "PodSync Scout").strip(),
            feed_link=(os.getenv("SCOUT_FEED_LINK") or "").strip(),
            feed_max_episodes=_env_int("SCOUT_FEED_MAX_EPISODES", 100, 10),
            youtube_api_keys=yt_keys,
            ollama_url=(os.getenv("OLLAMA_URL") or "http://localhost:11434").strip().rstrip("/"),
            ollama_default_model=(os.getenv("OLLAMA_MODEL") or "llama3.1:8b").strip(),
            ollama_timeout_sec=_env_float("OLLAMA_TIMEOUT_SEC", 120.0, 5.0),
            podsync_admin_url=(os.getenv("PODSYNC_ADMIN_URL") or "http://podsync:8095").strip().rstrip("/"),
            podsync_admin_token=(os.getenv("PODSYNC_ADMIN_TOKEN") or "").strip(),
            scan_interval_minutes=_env_int("SCOUT_SCAN_INTERVAL_MINUTES", 360, 15),
            base_path=base_path,
            scout_token=(os.getenv("SCOUT_TOKEN") or "").strip(),
        )
