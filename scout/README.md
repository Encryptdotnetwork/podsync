# PodSync Scout

Companion Docker service for PodSync that automatically discovers full-length YouTube interviews for watched guests/topics, filters out clips, non-English dubbed mirrors, and clickbait/AI-reupload channels, writes an atomic local JSON playlist feed for PodSync (`file:///app/scout/AutoPodcasts.json`), and serves a LAN-only web UI and REST API at `/scout/`.

## Features

- **YouTube Data API v3 Discovery:** Searches for long-form videos matching configured people/queries (`relevanceLanguage=en`, supports space-separated key rotation) and fetches exact durations and channel subscriber counts.
- **3-Tier Channel Policies (by YouTube `channel_id`):**
  - **Trusted (matching guests):** Auto-adds videos matching any watched person, bypassing LLM checks.
  - **Trusted (all uploads):** Polls the channel's uploads playlist for all long-form videos.
  - **Blocked:** Drops silently forever and can immediately purge existing episodes from PodSync.
- **Multi-Layer Noise Filtering:**
  - **English-only pre-filter:** Drops non-English `defaultAudioLanguage`/`defaultLanguage` metadata, foreign-language mirror channel names, and non-Latin titles before calling Ollama.
  - **Same-channel clip detector:** Evaluates longest videos first and filters shorter excerpts (`<= 65%` duration and `>= 10 min` shorter) uploaded by the same channel for the same guest within `clip_dedupe_days` (default `5` days; configurable in the UI).
  - **Local Ollama Spam Shield:** Classifies unknown channels in back-to-back batches using your local Ollama server (configurable model selector via `/api/tags`, graceful subscriber-count fallback if Ollama is offline).
- **Web UI (`/scout/`) & REST API:**
  - Review **Added** episodes with one-tap **Block Channel & Purge**, **Trust Channel**, or **Remove Episode Only**.
  - Rescue false positives from **Filtered Out** with **Grab Anyway** or **Grab + Trust Channel**.
  - Manage watched people, channel policies, Ollama model, clip window, and trigger background scans (**Scan Now**).
  - Protected with CSP, `X-Frame-Options: DENY`, CSRF header checks (`X-Scout-Request: 1`), and optional Bearer auth (`SCOUT_TOKEN`).

## Environment Variables

| Variable | Default | Description |
| --- | --- | --- |
| `YOUTUBE_API_KEY` | *(required)* | YouTube Data API v3 key(s), space-separated (also reads `PODSYNC_YOUTUBE_API_KEY`) |
| `PODSYNC_ADMIN_URL` | `http://podsync:8095` | Internal PodSync admin API base URL (use `server.port`, e.g. `8080`, when `admin.mount_on_server = true`) |
| `PODSYNC_ADMIN_TOKEN` | *(recommended)* | Matches PodSync `PODSYNC_ADMIN_TOKEN` for immediate sync & episode purge |
| `OLLAMA_URL` | `http://localhost:11434` | Local Ollama server base URL |
| `OLLAMA_MODEL` | `llama3.1:8b` | Default Ollama model (changeable in UI from live `/api/tags`) |
| `SCOUT_FEED_ID` | `AutoPodcasts` | PodSync feed ID & output filename (`<id>.json`) |
| `SCOUT_FEED_TITLE` | `Auto Podcasts` | Podcast feed title |
| `SCOUT_FEED_MAX_EPISODES` | `100` | Max episodes written to JSON (match with `page_size = 100` in PodSync) |
| `SCOUT_BASE_PATH` | `/scout` | URL prefix served by FastAPI/reverse proxy (do **not** use Traefik `StripPrefix`) |
| `SCOUT_TOKEN` | *(empty)* | Optional Bearer token; recommended whenever other containers share the Docker network |
| `SCOUT_SCAN_INTERVAL_MINUTES` | `360` | Background scan interval in minutes |

## REST API

All mutating (`POST`/`PUT`/`DELETE`) requests require the header `X-Scout-Request: 1` (and `Authorization: Bearer <SCOUT_TOKEN>` if `SCOUT_TOKEN` is set).

- `GET /healthz` — Liveness probe
- `GET /scout/api/status` — Feed status, active scan state, episode counts
- `POST /scout/api/scan` — Start a background scan (`202 Accepted`; add `?wait=true` to wait synchronously)
- `GET /scout/api/episodes?status=added|filtered|purged` — List episodes
- `POST /scout/api/episodes/{video_id}/grab` — Rescue a filtered episode (`{"trust_channel": false}`)
- `POST /scout/api/episodes/{video_id}/purge` — Remove one episode from Scout and PodSync
- `GET /scout/api/channels` — List trusted/blocked channels
- `PUT /scout/api/channels/{channel_id}` — Set policy (`neutral`, `trusted_matching`, `trusted_all`, `blocked`) with optional `purge_existing: true`
- `GET/POST /scout/api/people`, `PUT/DELETE /scout/api/people/{id}` — Manage watched people
- `GET/PUT /scout/api/settings` — Update Ollama toggle/model, fallback subscriber threshold, scan interval, and `clip_dedupe_days`
- `GET /scout/api/ollama/models` — Live model list from Ollama `/api/tags`

## Docker Deployment Notes

1. Pre-create host bind mounts owned by `10001:10001` (matching PodSync's non-root UID/GID):
   - `/path/to/podsync-scout/db:/data/db`
   - `/path/to/podsync-scout/feeds:/data/feeds`
2. Mount `/path/to/podsync-scout/feeds:/app/scout:ro` into your PodSync container and add a feed in `config.toml`:
   ```toml
   [feeds.AutoPodcasts]
   url = "file:///app/scout/AutoPodcasts.json"
   page_size = 100
   update_period = "1h"
   quality = "high"
   format = "audio"
   clean = { keep_last = 30 }
   ```
3. Expose `/scout` **only** to your private LAN/VPN via reverse proxy (for example Traefik `Host(\`podsync.example.com\`) && PathPrefix(\`/scout\`)` with an IP allowlist middleware and **no** `StripPrefix` — see [../docker-compose.traefik.example.yml](../docker-compose.traefik.example.yml)).

## Development & Tests

```bash
pip install -r requirements-dev.txt
pytest -v
```
