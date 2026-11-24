from __future__ import annotations

import asyncio
import hmac
import logging
import re
import sqlite3
from contextlib import asynccontextmanager
from pathlib import Path
from typing import Any

from fastapi import APIRouter, Depends, FastAPI, Header, HTTPException, Query, Request
from fastapi.responses import FileResponse, JSONResponse, RedirectResponse
from fastapi.staticfiles import StaticFiles
from pydantic import BaseModel, Field

from app.config import AppConfig
from app.db import DEFAULT_EXCLUDE_REGEX, VALID_CHANNEL_POLICIES, Database
from app.ollama import OllamaClient
from app.podsync import PodSyncAdminClient
from app.scanner import Scanner
from app.youtube import YouTubeClient

logging.basicConfig(
    level=logging.INFO,
    format="%(asctime)s %(levelname)s %(name)s: %(message)s",
)
logger = logging.getLogger("podsync-scout")

STATIC_DIR = Path(__file__).resolve().parent / "static"

_CSP_HEADER = (
    "default-src 'none'; "
    "script-src 'self'; "
    "style-src 'self'; "
    "img-src https: data:; "
    "connect-src 'self'; "
    "base-uri 'none'; "
    "form-action 'none'; "
    "frame-ancestors 'none'"
)


class PersonCreate(BaseModel):
    name: str = Field(..., min_length=1, max_length=120)
    query: str | None = Field(default=None, max_length=200)
    min_duration_sec: int = Field(default=1200, ge=60, le=36000)
    max_age_days: int = Field(default=14, ge=1, le=365)
    exclude_regex: str = Field(default=DEFAULT_EXCLUDE_REGEX, max_length=400)
    enabled: bool = True


class PersonUpdate(BaseModel):
    name: str | None = Field(default=None, min_length=1, max_length=120)
    query: str | None = Field(default=None, max_length=200)
    min_duration_sec: int | None = Field(default=None, ge=60, le=36000)
    max_age_days: int | None = Field(default=None, ge=1, le=365)
    exclude_regex: str | None = Field(default=None, max_length=400)
    enabled: bool | None = None


class ChannelPolicyRequest(BaseModel):
    policy: str
    channel_title: str | None = None
    notes: str | None = None
    purge_existing: bool = False


class GrabEpisodeRequest(BaseModel):
    trust_channel: bool = False
    trust_mode: str = "trusted_matching"


class SettingsUpdate(BaseModel):
    ollama_enabled: bool | None = None
    ollama_model: str | None = None
    fallback_min_subscribers: int | None = Field(default=None, ge=0, le=100_000_000)
    scan_interval_minutes: int | None = Field(default=None, ge=15, le=10080)
    clip_dedupe_days: int | None = Field(default=None, ge=0, le=30)


def create_app(
    cfg: AppConfig | None = None,
    *,
    youtube_client: YouTubeClient | None = None,
    ollama_client: OllamaClient | None = None,
    podsync_client: PodSyncAdminClient | None = None,
    enable_scheduler: bool = True,
) -> FastAPI:
    cfg = cfg or AppConfig.from_env()
    db = Database(
        cfg.db_path,
        default_ollama_model=cfg.ollama_default_model,
        default_scan_interval_minutes=cfg.scan_interval_minutes,
    )
    owns_yt = youtube_client is None
    owns_ollama = ollama_client is None
    owns_podsync = podsync_client is None

    yt = youtube_client or YouTubeClient(cfg.youtube_api_keys)
    ollama = ollama_client or OllamaClient(cfg.ollama_url, timeout_sec=cfg.ollama_timeout_sec)
    podsync = podsync_client or PodSyncAdminClient(cfg.podsync_admin_url, cfg.podsync_admin_token, cfg.feed_id)
    scanner = Scanner(cfg=cfg, db=db, youtube=yt, ollama=ollama, podsync=podsync)
    background_tasks: set[asyncio.Task[Any]] = set()

    def _spawn_task(coro: Any) -> asyncio.Task[Any]:
        task = asyncio.create_task(coro)
        background_tasks.add(task)
        task.add_done_callback(background_tasks.discard)
        return task

    async def _scheduler_loop() -> None:
        await asyncio.sleep(15)
        while True:
            try:
                if cfg.youtube_api_keys:
                    logger.info("Starting scheduled YouTube scan")
                    summary = await scanner.run_scan(trigger="scheduled")
                    logger.info(
                        "Scheduled scan finished: status=%s candidates=%d added=%d filtered=%d",
                        summary.status,
                        summary.candidates,
                        summary.added,
                        summary.filtered,
                    )
                else:
                    logger.warning("Skipping scheduled scan: YOUTUBE_API_KEY is not set")
            except asyncio.CancelledError:
                raise
            except Exception:
                logger.exception("Unexpected error in scheduler loop")

            interval_min = db.get_settings().get("scan_interval_minutes", cfg.scan_interval_minutes)
            await asyncio.sleep(max(15, int(interval_min)) * 60)

    @asynccontextmanager
    async def lifespan(_app: FastAPI):
        # Ensure feed file exists on boot so PodSync never hits ENOENT on first poll
        await scanner.rebuild_feed_file_async()
        sched_task: asyncio.Task[None] | None = None
        if enable_scheduler:
            sched_task = asyncio.create_task(_scheduler_loop())
        try:
            yield
        finally:
            if sched_task is not None:
                sched_task.cancel()
                try:
                    await sched_task
                except asyncio.CancelledError:
                    pass
            for t in list(background_tasks):
                t.cancel()
            if background_tasks:
                await asyncio.gather(*background_tasks, return_exceptions=True)
            if owns_yt:
                await yt.aclose()
            if owns_ollama:
                await ollama.aclose()
            if owns_podsync:
                await podsync.aclose()
            db.close()

    app = FastAPI(title="PodSync Scout", lifespan=lifespan)
    app.state.cfg = cfg
    app.state.db = db
    app.state.scanner = scanner
    app.state.ollama = ollama
    app.state.podsync = podsync

    @app.middleware("http")
    async def security_headers_middleware(request: Request, call_next: Any):
        response = await call_next(request)
        response.headers.setdefault("X-Content-Type-Options", "nosniff")
        response.headers.setdefault("X-Frame-Options", "DENY")
        response.headers.setdefault("Referrer-Policy", "no-referrer")
        response.headers.setdefault("Content-Security-Policy", _CSP_HEADER)
        return response

    def require_auth(authorization: str | None = Header(default=None)) -> None:
        if not cfg.scout_token:
            return
        if not authorization or not authorization.lower().startswith("bearer "):
            raise HTTPException(status_code=401, detail="unauthorized")
        provided = authorization[7:]
        if not hmac.compare_digest(provided, cfg.scout_token):
            raise HTTPException(status_code=401, detail="unauthorized")

    def require_csrf(
        x_scout_request: str | None = Header(default=None),
        sec_fetch_site: str | None = Header(default=None),
    ) -> None:
        # Custom header forces CORS preflight and blocks cross-origin <form> / no-cors fetch CSRF.
        if x_scout_request != "1":
            raise HTTPException(status_code=403, detail="missing X-Scout-Request header")
        if sec_fetch_site and sec_fetch_site.lower() not in {"same-origin", "none"}:
            raise HTTPException(status_code=403, detail="cross-site request blocked")

    @app.get("/healthz")
    async def healthz() -> dict[str, Any]:
        return {"status": "ok", "feed_id": cfg.feed_id, "scanning": scanner.is_running}

    prefix = cfg.base_path  # e.g. "/scout" or ""
    router = APIRouter(prefix=prefix)

    @router.get("")
    async def redirect_slash(request: Request) -> RedirectResponse:
        return RedirectResponse(url=f"{prefix}/", status_code=307)

    @router.get("/")
    async def index_page() -> FileResponse:
        return FileResponse(STATIC_DIR / "index.html", headers={"Cache-Control": "no-store"})

    if STATIC_DIR.exists():
        app.mount(f"{prefix}/static", StaticFiles(directory=str(STATIC_DIR)), name="static")

    if prefix:
        @app.get("/")
        async def root_redirect() -> RedirectResponse:
            return RedirectResponse(url=f"{prefix}/", status_code=307)

    # --- API routes ---

    @router.get("/api/status", dependencies=[Depends(require_auth)])
    async def api_status() -> dict[str, Any]:
        return {
            "feed_id": cfg.feed_id,
            "feed_title": cfg.feed_title,
            "scanning": scanner.is_running,
            "latest_scan": db.latest_scan_run(),
            "youtube_configured": bool(cfg.youtube_api_keys),
            "ollama_url": cfg.ollama_url,
            "auth_required": bool(cfg.scout_token),
            "counts": db.episode_counts(),
        }

    @router.post("/api/scan", dependencies=[Depends(require_auth), Depends(require_csrf)])
    async def api_scan(wait: bool = Query(default=False)) -> Any:
        if wait:
            summary = await scanner.run_scan(trigger="manual")
            return {
                "run_id": summary.run_id,
                "status": summary.status,
                "candidates": summary.candidates,
                "added": summary.added,
                "filtered": summary.filtered,
                "error": summary.error,
                "sync_result": summary.sync_result,
            }

        if scanner.is_running:
            return JSONResponse(status_code=202, content={"status": "already_running", "scanning": True})

        _spawn_task(scanner.run_scan(trigger="manual"))
        return JSONResponse(status_code=202, content={"status": "started", "scanning": True})

    @router.get("/api/episodes", dependencies=[Depends(require_auth)])
    async def api_list_episodes(
        status: str | None = Query(default=None),
        limit: int = Query(default=100, ge=1, le=500),
    ) -> dict[str, Any]:
        return {"episodes": db.list_episodes(status=status, limit=limit)}

    @router.post("/api/episodes/{video_id}/grab", dependencies=[Depends(require_auth), Depends(require_csrf)])
    async def api_grab_episode(video_id: str, body: GrabEpisodeRequest | None = None) -> dict[str, Any]:
        ep = db.get_episode(video_id)
        if ep is None:
            raise HTTPException(status_code=404, detail="episode not found")

        req = body or GrabEpisodeRequest()
        if req.trust_channel:
            mode = req.trust_mode if req.trust_mode in {"trusted_matching", "trusted_all"} else "trusted_matching"
            db.upsert_channel(
                channel_id=ep["channel_id"],
                channel_title=ep["channel_title"],
                subscriber_count=ep["subscriber_count"],
                policy=mode,
            )

        updated = db.set_episode_status(
            video_id=video_id,
            status="added",
            filter_source="manual_grab",
            filter_reason="Manually rescued by user",
        )
        await scanner.rebuild_feed_file_async()
        sync_res = await podsync.trigger_sync()
        return {"episode": updated, "sync_result": sync_res}

    @router.post("/api/episodes/{video_id}/purge", dependencies=[Depends(require_auth), Depends(require_csrf)])
    async def api_purge_episode(video_id: str) -> dict[str, Any]:
        ep = db.get_episode(video_id)
        if ep is None:
            raise HTTPException(status_code=404, detail="episode not found")

        updated = db.set_episode_status(
            video_id=video_id,
            status="purged",
            filter_source="manual_purge",
            filter_reason="Removed by user",
        )
        await scanner.rebuild_feed_file_async()
        del_res = await podsync.delete_episode(video_id)
        return {"episode": updated, "podsync_delete": del_res}

    @router.get("/api/channels", dependencies=[Depends(require_auth)])
    async def api_list_channels(policy: str | None = Query(default=None)) -> dict[str, Any]:
        return {"channels": db.list_channels(policy=policy)}

    @router.put("/api/channels/{channel_id}", dependencies=[Depends(require_auth), Depends(require_csrf)])
    async def api_set_channel_policy(channel_id: str, body: ChannelPolicyRequest) -> dict[str, Any]:
        if body.policy not in VALID_CHANNEL_POLICIES:
            raise HTTPException(status_code=400, detail=f"invalid policy {body.policy!r}")

        existing = db.get_channel(channel_id)
        title = body.channel_title or (existing["channel_title"] if existing else channel_id)
        ch = db.upsert_channel(
            channel_id=channel_id,
            channel_title=title,
            policy=body.policy,
            notes=body.notes,
        )

        purged_ids: list[str] = []
        delete_results: dict[str, Any] = {}
        if body.policy == "blocked" and body.purge_existing:
            purged_ids = db.purge_channel_episodes(channel_id)
            if purged_ids:
                await scanner.rebuild_feed_file_async()
                for vid in purged_ids:
                    delete_results[vid] = await podsync.delete_episode(vid)

        return {
            "channel": ch,
            "purged_video_ids": purged_ids,
            "podsync_deletes": delete_results,
        }

    @router.get("/api/people", dependencies=[Depends(require_auth)])
    async def api_list_people() -> dict[str, Any]:
        return {"people": db.list_people()}

    @router.post("/api/people", dependencies=[Depends(require_auth), Depends(require_csrf)])
    async def api_create_person(body: PersonCreate) -> dict[str, Any]:
        if body.exclude_regex:
            try:
                re.compile(body.exclude_regex)
            except re.error as exc:
                raise HTTPException(status_code=400, detail=f"invalid exclude_regex: {exc}") from exc
        try:
            person = db.create_person(
                name=body.name,
                query=body.query,
                min_duration_sec=body.min_duration_sec,
                max_age_days=body.max_age_days,
                exclude_regex=body.exclude_regex,
                enabled=body.enabled,
            )
            return {"person": person}
        except sqlite3.IntegrityError as exc:
            raise HTTPException(status_code=409, detail="person with this name already exists") from exc
        except ValueError as exc:
            raise HTTPException(status_code=400, detail=str(exc)) from exc

    @router.put("/api/people/{person_id}", dependencies=[Depends(require_auth), Depends(require_csrf)])
    async def api_update_person(person_id: int, body: PersonUpdate) -> dict[str, Any]:
        updates = body.model_dump(exclude_none=True)
        if "exclude_regex" in updates and updates["exclude_regex"]:
            try:
                re.compile(updates["exclude_regex"])
            except re.error as exc:
                raise HTTPException(status_code=400, detail=f"invalid exclude_regex: {exc}") from exc
        try:
            person = db.update_person(person_id, updates)
        except sqlite3.IntegrityError as exc:
            raise HTTPException(status_code=409, detail="person with this name already exists") from exc
        except ValueError as exc:
            raise HTTPException(status_code=400, detail=str(exc)) from exc
        if person is None:
            raise HTTPException(status_code=404, detail="person not found")
        return {"person": person}

    @router.delete("/api/people/{person_id}", dependencies=[Depends(require_auth), Depends(require_csrf)])
    async def api_delete_person(person_id: int) -> dict[str, Any]:
        if not db.delete_person(person_id):
            raise HTTPException(status_code=404, detail="person not found")
        return {"deleted": True}

    @router.get("/api/settings", dependencies=[Depends(require_auth)])
    async def api_get_settings() -> dict[str, Any]:
        return {"settings": db.get_settings(), "ollama_url": cfg.ollama_url}

    @router.put("/api/settings", dependencies=[Depends(require_auth), Depends(require_csrf)])
    async def api_update_settings(body: SettingsUpdate) -> dict[str, Any]:
        updated = db.update_settings(body.model_dump(exclude_none=True))
        return {"settings": updated}

    @router.get("/api/ollama/models", dependencies=[Depends(require_auth)])
    async def api_ollama_models() -> dict[str, Any]:
        try:
            models = await ollama.list_models()
            return {"reachable": True, "models": models}
        except Exception as exc:
            return {"reachable": False, "models": [], "error": str(exc)}

    app.include_router(router)
    return app
