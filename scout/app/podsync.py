from __future__ import annotations

import asyncio
import json
import logging
import os
import tempfile
from pathlib import Path
from typing import Any
from urllib.parse import quote

import httpx

from app.config import AppConfig

logger = logging.getLogger(__name__)


def write_feed_json(cfg: AppConfig, added_episodes: list[dict[str, Any]]) -> Path:
    """Atomically write the PodSync local JSON feed file."""
    cfg.feeds_dir.mkdir(parents=True, exist_ok=True)
    target = cfg.feed_json_path

    sorted_eps = sorted(
        added_episodes,
        key=lambda e: str(e.get("published_at") or ""),
        reverse=True,
    )[: cfg.feed_max_episodes]

    payload = {
        "title": cfg.feed_title,
        "description": cfg.feed_description,
        "author": cfg.feed_author,
        "link": cfg.feed_link,
        "cover_art": sorted_eps[0].get("thumbnail_url", "") if sorted_eps else "",
        "episodes": [
            {
                "id": ep["video_id"],
                "title": f"{ep['title']} ({ep['channel_title']})" if ep.get("channel_title") else ep["title"],
                "description": ep.get("description", ""),
                "video_url": ep["video_url"],
                "thumbnail": ep.get("thumbnail_url", ""),
                "duration": int(ep.get("duration_sec") or 0),
                "pub_date": ep["published_at"],
            }
            for ep in sorted_eps
        ],
    }

    encoded = json.dumps(payload, indent=2, ensure_ascii=False).encode("utf-8")

    fd, tmp_name = tempfile.mkstemp(prefix=f".{cfg.feed_id}.", suffix=".tmp", dir=str(cfg.feeds_dir))
    try:
        with os.fdopen(fd, "wb") as f:
            f.write(encoded)
            f.flush()
            os.fsync(f.fileno())
        os.chmod(tmp_name, 0o644)
        os.replace(tmp_name, target)
    except Exception:
        try:
            os.unlink(tmp_name)
        except OSError:
            pass
        raise

    return target


class PodSyncAdminClient:
    def __init__(self, admin_url: str, admin_token: str, feed_id: str, timeout_sec: float = 15.0) -> None:
        self.admin_url = admin_url.rstrip("/")
        self.admin_token = admin_token.strip()
        self.feed_id = feed_id
        self.timeout_sec = timeout_sec
        self._client = httpx.AsyncClient(timeout=timeout_sec)
        self._retry_task: asyncio.Task[None] | None = None

    async def aclose(self) -> None:
        if self._retry_task is not None and not self._retry_task.done():
            self._retry_task.cancel()
            try:
                await self._retry_task
            except asyncio.CancelledError:
                pass
        await self._client.aclose()

    def _headers(self) -> dict[str, str]:
        if not self.admin_token:
            return {}
        return {"Authorization": f"Bearer {self.admin_token}"}

    def _schedule_retry_sync(self, delay_sec: float) -> None:
        if self._retry_task is not None and not self._retry_task.done():
            return

        async def _delayed() -> None:
            await asyncio.sleep(max(1.0, min(300.0, delay_sec)))
            logger.info("Retrying throttled PodSync sync for %s", self.feed_id)
            await self.trigger_sync(_allow_retry_schedule=False)

        try:
            loop = asyncio.get_running_loop()
            self._retry_task = loop.create_task(_delayed())
        except RuntimeError:
            pass

    async def trigger_sync(self, _allow_retry_schedule: bool = True) -> dict[str, Any]:
        if not self.admin_url:
            return {"skipped": True, "reason": "PODSYNC_ADMIN_URL not configured"}
        encoded_feed = quote(self.feed_id, safe="")
        url = f"{self.admin_url}/admin/feeds/{encoded_feed}/sync"
        try:
            resp = await self._client.post(url, headers=self._headers())
            if resp.status_code in (200, 202):
                return {"ok": True, "status": resp.status_code, "body": resp.json()}
            if resp.status_code == 429:
                retry_after = 35.0
                raw_ra = resp.headers.get("Retry-After", "").strip()
                if raw_ra.isdigit():
                    retry_after = float(int(raw_ra) + 2)
                if _allow_retry_schedule:
                    self._schedule_retry_sync(retry_after)
                return {
                    "ok": True,
                    "throttled": True,
                    "retry_scheduled_sec": retry_after if _allow_retry_schedule else None,
                    "status": 429,
                    "body": resp.text[:200],
                }
            logger.warning("PodSync sync returned HTTP %s: %s", resp.status_code, resp.text[:200])
            return {"ok": False, "status": resp.status_code, "body": resp.text[:200]}
        except Exception as exc:
            logger.warning("Failed to trigger PodSync sync: %s", exc)
            return {"ok": False, "error": str(exc)}

    async def delete_episode(self, video_id: str) -> dict[str, Any]:
        if not self.admin_url:
            return {"skipped": True, "reason": "PODSYNC_ADMIN_URL not configured"}
        encoded_feed = quote(self.feed_id, safe="")
        encoded_vid = quote(video_id, safe="")
        url = f"{self.admin_url}/admin/feeds/{encoded_feed}/episodes/{encoded_vid}"
        try:
            resp = await self._client.delete(url, headers=self._headers())
            if resp.status_code in (200, 204):
                return {"ok": True, "deleted": True, "status": resp.status_code}
            if resp.status_code == 404:
                return {"ok": True, "deleted": False, "status": 404, "reason": "not in podsync yet"}
            logger.warning("PodSync delete episode %s returned HTTP %s: %s", video_id, resp.status_code, resp.text[:200])
            return {"ok": False, "status": resp.status_code, "body": resp.text[:200]}
        except Exception as exc:
            logger.warning("Failed to delete episode %s from PodSync: %s", video_id, exc)
            return {"ok": False, "error": str(exc)}
