from __future__ import annotations

import sqlite3
import threading
from datetime import datetime, timezone
from pathlib import Path
from typing import Any


DEFAULT_EXCLUDE_REGEX = r"(?i)(#shorts|\bshort\b|\bclip\b|\bclips\b|\bhighlight\b|\bhighlights\b|\bteaser\b|\btrailer\b)"

DEFAULT_WATCHED_PEOPLE: list[dict[str, Any]] = []

VALID_CHANNEL_POLICIES = {"neutral", "trusted_matching", "trusted_all", "blocked"}
VALID_EPISODE_STATUSES = {"added", "filtered", "purged"}


def utc_now_iso() -> str:
    return datetime.now(timezone.utc).replace(microsecond=0).isoformat().replace("+00:00", "Z")


class Database:
    def __init__(self, path: Path, default_ollama_model: str = "llama3.1:8b", default_scan_interval_minutes: int = 360) -> None:
        self.path = path
        self.path.parent.mkdir(parents=True, exist_ok=True)
        self._lock = threading.RLock()
        self._conn = sqlite3.connect(str(self.path), check_same_thread=False)
        self._conn.row_factory = sqlite3.Row
        self._conn.execute("PRAGMA journal_mode=WAL;")
        self._conn.execute("PRAGMA busy_timeout=5000;")
        self._conn.execute("PRAGMA foreign_keys=ON;")
        self._init_schema(default_ollama_model, default_scan_interval_minutes)

    def close(self) -> None:
        with self._lock:
            self._conn.close()

    def _init_schema(self, default_ollama_model: str, default_scan_interval_minutes: int) -> None:
        with self._lock, self._conn:
            self._conn.executescript(
                """
                CREATE TABLE IF NOT EXISTS settings (
                    key TEXT PRIMARY KEY,
                    value TEXT NOT NULL
                );

                CREATE TABLE IF NOT EXISTS watched_people (
                    id INTEGER PRIMARY KEY AUTOINCREMENT,
                    name TEXT NOT NULL UNIQUE,
                    query TEXT NOT NULL,
                    min_duration_sec INTEGER NOT NULL DEFAULT 1200,
                    max_age_days INTEGER NOT NULL DEFAULT 14,
                    exclude_regex TEXT NOT NULL DEFAULT '',
                    enabled INTEGER NOT NULL DEFAULT 1,
                    created_at TEXT NOT NULL
                );

                CREATE TABLE IF NOT EXISTS channels (
                    channel_id TEXT PRIMARY KEY,
                    channel_title TEXT NOT NULL,
                    subscriber_count INTEGER NOT NULL DEFAULT 0,
                    policy TEXT NOT NULL DEFAULT 'neutral',
                    notes TEXT NOT NULL DEFAULT '',
                    updated_at TEXT NOT NULL
                );

                CREATE TABLE IF NOT EXISTS episodes (
                    video_id TEXT PRIMARY KEY,
                    title TEXT NOT NULL,
                    description TEXT NOT NULL DEFAULT '',
                    channel_id TEXT NOT NULL,
                    channel_title TEXT NOT NULL,
                    subscriber_count INTEGER NOT NULL DEFAULT 0,
                    duration_sec INTEGER NOT NULL DEFAULT 0,
                    published_at TEXT NOT NULL,
                    thumbnail_url TEXT NOT NULL DEFAULT '',
                    video_url TEXT NOT NULL,
                    matched_person TEXT NOT NULL DEFAULT '',
                    status TEXT NOT NULL,
                    filter_source TEXT NOT NULL DEFAULT '',
                    filter_reason TEXT NOT NULL DEFAULT '',
                    llm_verdict TEXT NOT NULL DEFAULT '',
                    llm_confidence REAL,
                    discovered_at TEXT NOT NULL,
                    updated_at TEXT NOT NULL
                );

                CREATE INDEX IF NOT EXISTS idx_episodes_status_pub ON episodes(status, published_at DESC);
                CREATE INDEX IF NOT EXISTS idx_episodes_channel ON episodes(channel_id);

                CREATE TABLE IF NOT EXISTS scan_runs (
                    id INTEGER PRIMARY KEY AUTOINCREMENT,
                    started_at TEXT NOT NULL,
                    finished_at TEXT,
                    trigger TEXT NOT NULL DEFAULT 'scheduled',
                    status TEXT NOT NULL DEFAULT 'running',
                    candidates INTEGER NOT NULL DEFAULT 0,
                    added INTEGER NOT NULL DEFAULT 0,
                    filtered INTEGER NOT NULL DEFAULT 0,
                    error TEXT NOT NULL DEFAULT ''
                );
                """
            )

            defaults = {
                "ollama_enabled": "true",
                "ollama_model": default_ollama_model,
                "fallback_min_subscribers": "10000",
                "scan_interval_minutes": str(default_scan_interval_minutes),
                "clip_dedupe_days": "5",
            }
            for key, value in defaults.items():
                self._conn.execute(
                    "INSERT OR IGNORE INTO settings (key, value) VALUES (?, ?)",
                    (key, value),
                )

            # Mark any stale in-flight scan runs left behind by a crash/restart
            self._conn.execute(
                "UPDATE scan_runs SET status = 'interrupted', finished_at = ? WHERE status = 'running'",
                (utc_now_iso(),),
            )

            count = self._conn.execute("SELECT COUNT(*) FROM watched_people").fetchone()[0]
            if count == 0:
                now = utc_now_iso()
                for person in DEFAULT_WATCHED_PEOPLE:
                    self._conn.execute(
                        """
                        INSERT INTO watched_people (name, query, min_duration_sec, max_age_days, exclude_regex, enabled, created_at)
                        VALUES (?, ?, ?, ?, ?, 1, ?)
                        """,
                        (
                            person["name"],
                            person["query"],
                            person["min_duration_sec"],
                            person["max_age_days"],
                            person["exclude_regex"],
                            now,
                        ),
                    )

    # --- Settings ---

    def get_settings(self) -> dict[str, Any]:
        with self._lock:
            rows = self._conn.execute("SELECT key, value FROM settings").fetchall()
            raw = {r["key"]: r["value"] for r in rows}
            return {
                "ollama_enabled": raw.get("ollama_enabled", "true").lower() == "true",
                "ollama_model": raw.get("ollama_model", "llama3.1:8b"),
                "fallback_min_subscribers": int(raw.get("fallback_min_subscribers", "10000")),
                "scan_interval_minutes": max(15, int(raw.get("scan_interval_minutes", "360"))),
                "clip_dedupe_days": max(0, int(raw.get("clip_dedupe_days", "5"))),
            }

    def update_settings(self, updates: dict[str, Any]) -> dict[str, Any]:
        allowed = {
            "ollama_enabled",
            "ollama_model",
            "fallback_min_subscribers",
            "scan_interval_minutes",
            "clip_dedupe_days",
        }
        with self._lock, self._conn:
            for key, value in updates.items():
                if key not in allowed or value is None:
                    continue
                if key == "ollama_enabled":
                    str_val = "true" if bool(value) else "false"
                elif key == "fallback_min_subscribers":
                    str_val = str(max(0, int(value)))
                elif key == "scan_interval_minutes":
                    str_val = str(max(15, int(value)))
                elif key == "clip_dedupe_days":
                    str_val = str(max(0, min(30, int(value))))
                else:
                    str_val = str(value).strip()
                self._conn.execute(
                    "INSERT INTO settings (key, value) VALUES (?, ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value",
                    (key, str_val),
                )
        return self.get_settings()

    # --- Watched People ---

    def list_people(self, enabled_only: bool = False) -> list[dict[str, Any]]:
        with self._lock:
            sql = "SELECT * FROM watched_people"
            if enabled_only:
                sql += " WHERE enabled = 1"
            sql += " ORDER BY id ASC"
            rows = self._conn.execute(sql).fetchall()
            return [self._person_row(r) for r in rows]

    def create_person(
        self,
        name: str,
        query: str | None = None,
        min_duration_sec: int = 1200,
        max_age_days: int = 14,
        exclude_regex: str = DEFAULT_EXCLUDE_REGEX,
        enabled: bool = True,
    ) -> dict[str, Any]:
        clean_name = name.strip()
        if not clean_name:
            raise ValueError("name is required")
        clean_query = (query or "").strip() or f'"{clean_name}"'
        now = utc_now_iso()
        with self._lock, self._conn:
            cur = self._conn.execute(
                """
                INSERT INTO watched_people (name, query, min_duration_sec, max_age_days, exclude_regex, enabled, created_at)
                VALUES (?, ?, ?, ?, ?, ?, ?)
                """,
                (
                    clean_name,
                    clean_query,
                    max(60, int(min_duration_sec)),
                    max(1, int(max_age_days)),
                    exclude_regex.strip(),
                    1 if enabled else 0,
                    now,
                ),
            )
            row = self._conn.execute("SELECT * FROM watched_people WHERE id = ?", (cur.lastrowid,)).fetchone()
            return self._person_row(row)

    def update_person(self, person_id: int, updates: dict[str, Any]) -> dict[str, Any] | None:
        with self._lock, self._conn:
            row = self._conn.execute("SELECT * FROM watched_people WHERE id = ?", (person_id,)).fetchone()
            if row is None:
                return None
            name = str(updates.get("name", row["name"])).strip()
            if not name:
                raise ValueError("name is required")
            query = str(updates.get("query", row["query"])).strip() or f'"{name}"'
            min_dur = max(60, int(updates.get("min_duration_sec", row["min_duration_sec"])))
            max_age = max(1, int(updates.get("max_age_days", row["max_age_days"])))
            exclude_regex = str(updates.get("exclude_regex", row["exclude_regex"])).strip()
            enabled = 1 if bool(updates.get("enabled", bool(row["enabled"]))) else 0

            self._conn.execute(
                """
                UPDATE watched_people
                SET name = ?, query = ?, min_duration_sec = ?, max_age_days = ?, exclude_regex = ?, enabled = ?
                WHERE id = ?
                """,
                (name, query, min_dur, max_age, exclude_regex, enabled, person_id),
            )
            updated = self._conn.execute("SELECT * FROM watched_people WHERE id = ?", (person_id,)).fetchone()
            return self._person_row(updated)

    def delete_person(self, person_id: int) -> bool:
        with self._lock, self._conn:
            cur = self._conn.execute("DELETE FROM watched_people WHERE id = ?", (person_id,))
            return cur.rowcount > 0

    @staticmethod
    def _person_row(row: sqlite3.Row) -> dict[str, Any]:
        return {
            "id": row["id"],
            "name": row["name"],
            "query": row["query"],
            "min_duration_sec": row["min_duration_sec"],
            "max_age_days": row["max_age_days"],
            "exclude_regex": row["exclude_regex"],
            "enabled": bool(row["enabled"]),
            "created_at": row["created_at"],
        }

    # --- Channels ---

    def get_channel(self, channel_id: str) -> dict[str, Any] | None:
        with self._lock:
            row = self._conn.execute("SELECT * FROM channels WHERE channel_id = ?", (channel_id,)).fetchone()
            return dict(row) if row is not None else None

    def list_channels(self, policy: str | None = None) -> list[dict[str, Any]]:
        with self._lock:
            if policy:
                rows = self._conn.execute(
                    "SELECT * FROM channels WHERE policy = ? ORDER BY updated_at DESC",
                    (policy,),
                ).fetchall()
            else:
                rows = self._conn.execute(
                    "SELECT * FROM channels WHERE policy != 'neutral' ORDER BY policy ASC, updated_at DESC"
                ).fetchall()
            return [dict(r) for r in rows]

    def upsert_channel(
        self,
        channel_id: str,
        channel_title: str,
        subscriber_count: int = 0,
        policy: str | None = None,
        notes: str | None = None,
    ) -> dict[str, Any]:
        if policy is not None and policy not in VALID_CHANNEL_POLICIES:
            raise ValueError(f"invalid channel policy: {policy}")
        now = utc_now_iso()
        with self._lock, self._conn:
            existing = self._conn.execute("SELECT * FROM channels WHERE channel_id = ?", (channel_id,)).fetchone()
            if existing is None:
                self._conn.execute(
                    """
                    INSERT INTO channels (channel_id, channel_title, subscriber_count, policy, notes, updated_at)
                    VALUES (?, ?, ?, ?, ?, ?)
                    """,
                    (
                        channel_id,
                        channel_title or channel_id,
                        int(subscriber_count or 0),
                        policy or "neutral",
                        notes or "",
                        now,
                    ),
                )
            else:
                new_title = channel_title or existing["channel_title"]
                new_subs = int(subscriber_count) if subscriber_count else existing["subscriber_count"]
                new_policy = policy if policy is not None else existing["policy"]
                new_notes = notes if notes is not None else existing["notes"]
                self._conn.execute(
                    """
                    UPDATE channels
                    SET channel_title = ?, subscriber_count = ?, policy = ?, notes = ?, updated_at = ?
                    WHERE channel_id = ?
                    """,
                    (new_title, new_subs, new_policy, new_notes, now, channel_id),
                )
            row = self._conn.execute("SELECT * FROM channels WHERE channel_id = ?", (channel_id,)).fetchone()
            return dict(row)

    # --- Episodes ---

    def get_episode(self, video_id: str) -> dict[str, Any] | None:
        with self._lock:
            row = self._conn.execute("SELECT * FROM episodes WHERE video_id = ?", (video_id,)).fetchone()
            return dict(row) if row is not None else None

    def episode_exists(self, video_id: str) -> bool:
        with self._lock:
            row = self._conn.execute("SELECT 1 FROM episodes WHERE video_id = ?", (video_id,)).fetchone()
            return row is not None

    def episode_counts(self) -> dict[str, int]:
        counts = {"added": 0, "filtered": 0, "purged": 0}
        with self._lock:
            rows = self._conn.execute("SELECT status, COUNT(*) AS cnt FROM episodes GROUP BY status").fetchall()
            for r in rows:
                st = str(r["status"])
                if st in counts:
                    counts[st] = int(r["cnt"])
        return counts

    def save_episode_if_absent(self, ep: dict[str, Any]) -> bool:
        """Insert episode only if not already present (prevents overwriting concurrent user actions)."""
        status = ep["status"]
        if status not in VALID_EPISODE_STATUSES:
            raise ValueError(f"invalid episode status: {status}")
        now = utc_now_iso()
        with self._lock, self._conn:
            cur = self._conn.execute(
                """
                INSERT OR IGNORE INTO episodes (
                    video_id, title, description, channel_id, channel_title, subscriber_count,
                    duration_sec, published_at, thumbnail_url, video_url, matched_person,
                    status, filter_source, filter_reason, llm_verdict, llm_confidence,
                    discovered_at, updated_at
                ) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
                """,
                (
                    ep["video_id"],
                    ep["title"],
                    ep.get("description", ""),
                    ep["channel_id"],
                    ep["channel_title"],
                    int(ep.get("subscriber_count", 0)),
                    int(ep.get("duration_sec", 0)),
                    ep["published_at"],
                    ep.get("thumbnail_url", ""),
                    ep["video_url"],
                    ep.get("matched_person", ""),
                    status,
                    ep.get("filter_source", ""),
                    ep.get("filter_reason", ""),
                    ep.get("llm_verdict", ""),
                    ep.get("llm_confidence"),
                    ep.get("discovered_at", now),
                    now,
                ),
            )
            return cur.rowcount > 0

    def save_episode(self, ep: dict[str, Any]) -> dict[str, Any]:
        status = ep["status"]
        if status not in VALID_EPISODE_STATUSES:
            raise ValueError(f"invalid episode status: {status}")
        now = utc_now_iso()
        with self._lock, self._conn:
            self._conn.execute(
                """
                INSERT INTO episodes (
                    video_id, title, description, channel_id, channel_title, subscriber_count,
                    duration_sec, published_at, thumbnail_url, video_url, matched_person,
                    status, filter_source, filter_reason, llm_verdict, llm_confidence,
                    discovered_at, updated_at
                ) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
                ON CONFLICT(video_id) DO UPDATE SET
                    title = excluded.title,
                    description = excluded.description,
                    channel_title = excluded.channel_title,
                    subscriber_count = excluded.subscriber_count,
                    duration_sec = excluded.duration_sec,
                    thumbnail_url = excluded.thumbnail_url,
                    status = excluded.status,
                    filter_source = excluded.filter_source,
                    filter_reason = excluded.filter_reason,
                    llm_verdict = excluded.llm_verdict,
                    llm_confidence = excluded.llm_confidence,
                    updated_at = excluded.updated_at
                """,
                (
                    ep["video_id"],
                    ep["title"],
                    ep.get("description", ""),
                    ep["channel_id"],
                    ep["channel_title"],
                    int(ep.get("subscriber_count", 0)),
                    int(ep.get("duration_sec", 0)),
                    ep["published_at"],
                    ep.get("thumbnail_url", ""),
                    ep["video_url"],
                    ep.get("matched_person", ""),
                    status,
                    ep.get("filter_source", ""),
                    ep.get("filter_reason", ""),
                    ep.get("llm_verdict", ""),
                    ep.get("llm_confidence"),
                    ep.get("discovered_at", now),
                    now,
                ),
            )
            row = self._conn.execute("SELECT * FROM episodes WHERE video_id = ?", (ep["video_id"],)).fetchone()
            return dict(row)

    def list_episodes(self, status: str | None = None, limit: int = 100) -> list[dict[str, Any]]:
        with self._lock:
            if status:
                rows = self._conn.execute(
                    """
                    SELECT e.*, COALESCE(c.policy, 'neutral') AS channel_policy
                    FROM episodes e
                    LEFT JOIN channels c ON c.channel_id = e.channel_id
                    WHERE e.status = ?
                    ORDER BY e.published_at DESC, e.discovered_at DESC
                    LIMIT ?
                    """,
                    (status, max(1, int(limit))),
                ).fetchall()
            else:
                rows = self._conn.execute(
                    """
                    SELECT e.*, COALESCE(c.policy, 'neutral') AS channel_policy
                    FROM episodes e
                    LEFT JOIN channels c ON c.channel_id = e.channel_id
                    ORDER BY e.published_at DESC, e.discovered_at DESC
                    LIMIT ?
                    """,
                    (max(1, int(limit)),),
                ).fetchall()
            return [dict(r) for r in rows]

    def list_added_by_channel(self, channel_id: str) -> list[dict[str, Any]]:
        with self._lock:
            rows = self._conn.execute(
                """
                SELECT video_id, title, duration_sec, published_at, matched_person
                FROM episodes
                WHERE channel_id = ? AND status = 'added'
                ORDER BY duration_sec DESC
                """,
                (channel_id,),
            ).fetchall()
            return [dict(r) for r in rows]

    def set_episode_status(self, video_id: str, status: str, filter_source: str, filter_reason: str) -> dict[str, Any] | None:
        if status not in VALID_EPISODE_STATUSES:
            raise ValueError(f"invalid status: {status}")
        now = utc_now_iso()
        with self._lock, self._conn:
            cur = self._conn.execute(
                """
                UPDATE episodes
                SET status = ?, filter_source = ?, filter_reason = ?, updated_at = ?
                WHERE video_id = ?
                """,
                (status, filter_source, filter_reason, now, video_id),
            )
            if cur.rowcount == 0:
                return None
            row = self._conn.execute("SELECT * FROM episodes WHERE video_id = ?", (video_id,)).fetchone()
            return dict(row)

    def purge_channel_episodes(self, channel_id: str, reason: str = "Channel blocked by user") -> list[str]:
        now = utc_now_iso()
        with self._lock, self._conn:
            rows = self._conn.execute(
                "SELECT video_id FROM episodes WHERE channel_id = ? AND status = 'added'",
                (channel_id,),
            ).fetchall()
            ids = [r["video_id"] for r in rows]
            if ids:
                self._conn.execute(
                    """
                    UPDATE episodes
                    SET status = 'purged', filter_source = 'manual_block', filter_reason = ?, updated_at = ?
                    WHERE channel_id = ? AND status = 'added'
                    """,
                    (reason, now, channel_id),
                )
            return ids

    # --- Scan runs ---

    def start_scan_run(self, trigger: str = "scheduled") -> int:
        now = utc_now_iso()
        with self._lock, self._conn:
            cur = self._conn.execute(
                "INSERT INTO scan_runs (started_at, trigger, status) VALUES (?, ?, 'running')",
                (now, trigger),
            )
            return int(cur.lastrowid)

    def finish_scan_run(
        self,
        run_id: int,
        status: str,
        candidates: int = 0,
        added: int = 0,
        filtered: int = 0,
        error: str = "",
    ) -> None:
        now = utc_now_iso()
        with self._lock, self._conn:
            self._conn.execute(
                """
                UPDATE scan_runs
                SET finished_at = ?, status = ?, candidates = ?, added = ?, filtered = ?, error = ?
                WHERE id = ?
                """,
                (now, status, candidates, added, filtered, error, run_id),
            )

    def latest_scan_run(self) -> dict[str, Any] | None:
        with self._lock:
            row = self._conn.execute("SELECT * FROM scan_runs ORDER BY id DESC LIMIT 1").fetchone()
            return dict(row) if row is not None else None
