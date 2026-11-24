# Podsync

![Podsync](docs/img/logo.png)

[![CI](https://github.com/Encryptdotnetwork/podsync/actions/workflows/ci.yml/badge.svg)](https://github.com/Encryptdotnetwork/podsync/actions?query=workflow%3ACI)
[![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)

Podsync converts YouTube, Vimeo, Odysee, Rumble, SoundCloud, Twitch, and local JSON playlist sources into RSS podcast feeds so you can download, sync, and listen to episodes offline in any podcast client.

> **Fork Notice:** Maintained fork of [`mxpv/podsync`](https://github.com/mxpv/podsync) adding **Odysee**, **Rumble**, and **Local JSON** feed support, an **Admin API & Web UI** (live hot-reload, **Sync now**, failed-episode retry, and episode purge), **time-based retry backoff**, **custom filename templates + migration**, hardened non-root Docker execution, and the optional [**PodSync Scout**](./scout/README.md) companion service.

---

## ✨ Features

- **Platforms:** YouTube (channels, handles, playlists), Vimeo, Odysee, Rumble (channels, playlists, single videos), SoundCloud playlists, Twitch archives, and local read-only JSON feeds (`file:///...json`).
- **Media & Encoding:** Configurable audio/video format, quality (`high`/`low`), `max_height`, custom `yt-dlp` formats/args, MP3 encoding, and automatic `yt-dlp` self-updates.
- **Scheduling & Reliability:** Per-feed `update_period` or `cron_schedule`, HTTP 429 exponential rate-limit backoff, and time-based episode download retry backoff (automatically resets when `yt-dlp` updates).
- **Admin API & Web UI (`/admin/`):**
  - **Sync now:** Trigger immediate manual updates for one or all feeds with live sync status.
  - **Failed episode management:** Inspect failed downloads with retry schedules and trigger immediate per-episode retries.
  - **Episode deletion/purge:** Remove unwanted downloaded media files and rebuild the RSS feed immediately (`DELETE /admin/feeds/{id}/episodes/{episode}`).
  - **Config hot-reload:** Optional runtime config management with atomic TOML writes, ETag concurrency control, and read-only default (`allow_config_edit = false`).
- **Filtering & Customization:** Regex title/description include/exclude filters, min/max duration and age filters ([docs/filters.md](./docs/filters.md)), custom RSS metadata/artwork, OPML export, rolling cleanup (`keep_last`), configurable `filename_template`, and download/error exec hooks.
- **Companion Service (`scout/`):** Optional [PodSync Scout](./scout/README.md) container for automated YouTube interview discovery with local Ollama spam filtering, English-only filtering, same-channel clip detection, and a LAN triage UI.
- **Hardened Docker Runtime:** Runs as non-root UID/GID `10001`, supports a read-only root filesystem, strict CSP / DOM-safe web UI, and an SSRF-protected image-only `/thumbnail` proxy.

---

## 📋 Dependencies

When running from source (outside Docker), install `yt-dlp`, `ffmpeg`, and `go`:

```bash
brew install yt-dlp ffmpeg go
```

---

## 📖 Documentation

- [Episode Filters Guide](./docs/filters.md)
- [Schedule Updates with Cron](./docs/cron.md)
- [How to Get a YouTube API Key](./docs/how_to_get_youtube_api_key.md)
- [How to Get a Vimeo API Token](./docs/how_to_get_vimeo_token.md)
- [PodSync Scout Companion Service](./scout/README.md)
- [Podsync on QNAP NAS](./docs/how_to_setup_podsync_on_qnap_nas.md)
- [Podsync on Synology NAS](./docs/how_to_setup_podsync_on_synology_nas.md)

---

## 🔑 Platform Tokens

- **YouTube:** YouTube Data API v3 key (supports space-separated keys for rotation)
- **Vimeo:** Vimeo access token
- **Twitch:** `CLIENT_ID:CLIENT_SECRET`
- **Odysee, Rumble, SoundCloud, Local JSON:** No API key required

---

## ⚙️ Configuration

Create a `config.toml` file (see [config.toml.example](./config.toml.example) for all keys). Minimal example:

```toml
[server]
port = 8080
hostname = "https://podsync.example.com"
no_index = true

[storage]
  [storage.local]
  data_dir = "/app/data"

[tokens]
youtube = "PASTE_YOUR_API_KEY_HERE" # Prefer PODSYNC_YOUTUBE_API_KEY env var

[feeds]
  [feeds.ID1]
  url = "https://www.youtube.com/channel/UCxC5Ls6DwqV0e-CYcAKkExQ"
  update_period = "1h"
  quality = "high"
  format = "audio"

  # Odysee example (no API key required)
  [feeds.ID2]
  url = "https://odysee.com/@example:1"

  # Rumble example (no API key required)
  [feeds.ID3]
  url = "https://rumble.com/c/example"

  # Local JSON playlist feed (e.g. written by PodSync Scout, no API key required)
  [feeds.AutoPodcasts]
  url = "file:///app/scout/AutoPodcasts.json"
  page_size = 100
  update_period = "1h"
  quality = "high"
  format = "audio"
  clean = { keep_last = 30 }

[admin]
enabled = true
mount_on_server = true    # Serves /admin/ on server.port; set false to listen on admin.port (8095)
allow_config_edit = false # Keep false unless you want config.toml editable via the API
```

### 🛡️ Admin API & Web UI

Enable `[admin]` with a bearer token of at least 32 characters via `PODSYNC_ADMIN_TOKEN`. Restrict `/admin/` to your private LAN/VPN at your reverse proxy (never publish it to the public internet).

Key endpoints (all require `Authorization: Bearer <token>` except `/healthz`):

- `GET /admin/` — Web UI for **Sync now**, feed status, and failed episode retries
- `POST /admin/feeds/{id}/sync`, `POST /admin/sync`, `GET /admin/sync` — Manual sync & live status
- `GET /admin/feeds/{id}/episodes/failed` — List failed downloads and next retry timestamps
- `POST /admin/feeds/{id}/episodes/{episode}/retry` — Reset failure backoff and queue immediate retry
- `DELETE /admin/feeds/{id}/episodes/{episode}` — Delete media file, mark episode cleaned, and rebuild feed XML
- `GET /admin/config`, `GET /admin/feeds`, `POST /admin/reload` — Read redacted config and hot-reload from disk
- `PUT/DELETE /admin/config/{section}`, `PUT/DELETE /admin/feeds/{id}` — Live config edits (requires `allow_config_edit = true` and `If-Match` ETag)

### 🌍 Environment Variables

| Variable Name | Description | Example |
| --- | --- | --- |
| `PODSYNC_CONFIG_PATH` | Path to `config.toml` (overrides `--config`) | `/app/config/config.toml` |
| `PODSYNC_ADMIN_TOKEN` | Bearer token for `/admin/*` (min 32 chars, takes precedence over config) | `$(openssl rand -hex 32)` |
| `PODSYNC_YOUTUBE_API_KEY` | YouTube API key(s), space-separated for rotation | `key1 key2` |
| `PODSYNC_VIMEO_API_KEY` | Vimeo API key(s), space-separated for rotation | `key1 key2` |
| `PODSYNC_SOUNDCLOUD_API_KEY` | SoundCloud API key(s), optional | `key1` |
| `PODSYNC_TWITCH_API_KEY` | Twitch `CLIENT_ID:CLIENT_SECRET` | `id1:secret1` |

---

## 🚀 Running Podsync

### Build & Run from Source

```bash
make build
./bin/podsync --config config.toml
```

### Run via Docker Compose

The included [docker-compose.yml](./docker-compose.yml) runs Podsync as non-root UID/GID `10001` with a read-only root filesystem, an executable `tmpfs` at `/run/yt-dlp`, and an optional `scout` profile for [PodSync Scout](./scout/README.md).

```bash
# Pre-create writable host directories owned by UID/GID 10001
mkdir -p data db logs scout-db scout-feeds
chown -R 10001:10001 data db logs scout-db scout-feeds

# Start Podsync alone
docker compose up -d --build

# Or start Podsync + PodSync Scout together
docker compose --profile scout up -d --build
```

Pass secrets via environment variables or a `.env` file (`PODSYNC_ADMIN_TOKEN`, `PODSYNC_YOUTUBE_API_KEY`, and optionally `OLLAMA_URL`, `OLLAMA_MODEL`, `SCOUT_TOKEN`). Both `/admin/` (port `8095` standalone, or `/admin/` on `8080` with `mount_on_server = true`) and `/scout/` (port `8096`, bound to `127.0.0.1` by default) should only be exposed on your private LAN/VPN.

### 🗂️ Filename Migration

If you change `filename_template` and want to rename existing downloaded files:

```bash
./bin/podsync --config config.toml --migrate-filenames --migrate-filenames-dry-run
./bin/podsync --config config.toml --migrate-filenames
```

---

## 📄 License

This project is licensed under the MIT License — see [LICENSE](LICENSE) for details.
