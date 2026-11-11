# "Sync now" implementation handoff

Written 2026-11-12 for an implementer who has not seen the review. Read the
code before applying anything here; where this brief and the code disagree,
the code wins and the difference goes in your report.

## 1. Status

Verified in the repository on 2026-11-12:

- `origin/main` is `fbd9e91` (merged PR 3). Branch from it. The local `main`
  ref is stale at `45f60cc`.
- Already merged, so do not redo: context checks in `downloadEpisodes`,
  `Scheduler.safeUpdate` (panic recovery), mandatory admin token of 32+
  characters in `pkg/config/config.go`, feed-ID and filter-regex validation,
  the YouTube API key sent as a header, and the updater test harness in
  `services/update/updater_test.go`.
- Nothing for "Sync now" exists: no queue, no status, no sync endpoint, and
  `html/admin.html` is not in the image or served by any listener.

Observed in production on 2026-11-11 (recheck at deploy time, do not rely on):

- Image `podsync-local:fixes-20261111-fbd9e91` was running and healthy.
- Nine feeds; app listens on 8089; no `[admin]` block; `config.toml` is a
  read-only single-file bind mount.
- `podsync-netguard` admits exactly one inbound port (8089).
- Traefik has a public router (GET/HEAD, feed and media paths only) and a
  LAN/VPN-allowlisted router for the same host, both to 8089.
- `PODSYNC_ADMIN_TOKEN` is in the Portainer stack Env but is not passed into
  the container. Its length has not been checked.

## 2. Requirements

- Per-feed "Sync now"; "Sync all" secondary.
- A manual sync calls the same `Manager.Update` as the scheduler, with the
  same feed config. No second pipeline.
- De-duplicating queue; never two updates at once.
- A waiting manual request runs at the next boundary between episode
  downloads. An active download is never interrupted.
- Feedback states: queued, syncing, completed, no new episodes, failed, and
  rate limited.
- Schedules, filters, retention, media, GUIDs and enclosure URLs unchanged.
- Existing per-feed 429 backoff is respected; a request during it is refused
  with the retry time.
- Admin page at `/admin/` on the existing port, bearer token on every API
  route, nothing secret in the page, logs or responses.
- Editing a feed must not drop fields such as hooks.

Exclusions: no change to playlist ordering, `page_size` or discovery; no
"recent" ordering; no retry-policy redesign; no database cleanup; no
dependency changes; no unrelated refactoring; no edits to production.

## 3. Files and integration points

- `services/update/scheduler.go`: `updates chan string` (cap 16), cron
  callbacks that block on it, `Run`, `processUpdate`, `safeUpdate`,
  `applyReload`, `Close`.
- `services/update/updater.go`: `Manager.Update`, `inBackoff`,
  `downloadEpisodes`.
- `services/admin/server.go`, `handlers.go`: mux, `requireAuth`, config
  routes.
- `services/web/server.go`: `New`, mux, `securityHeadersMiddleware`.
- `cmd/podsync/main.go`: wiring at lines 176-250, `defer scheduler.Close()`.
- `pkg/config/config.go`: `Admin` struct, `validate`.
- `html/admin.html`: legacy editor; `FEED_MAP` drops both hook fields.
- Tests: `services/update/*_test.go`, `services/admin/server_test.go`.

## 4. Implementation steps

1. **Queue.** New `services/update/queue.go`: mutex-guarded, non-blocking,
   two lanes (manual first, then scheduled), at most one pending entry per
   feed, a 1-buffered wake channel. A manual push promotes a pending
   scheduled entry. Replace the channel; cron callbacks push and return.
   Remove `Close` and its `defer`. `Run` still handles reloads and
   cancellation between items.
2. **Result.** `Manager.Update` returns `(Result, error)`: new episodes
   discovered, downloaded, failed, whether the run was skipped for backoff
   (with the time), whether it was interrupted. Update the scheduler,
   headless mode and tests. Pipeline behaviour must not change.
3. **Yield.** `downloadEpisodes` checks an optional `shouldYield` callback
   beside the existing context check. The scheduler sets it for scheduled
   runs only; it returns true when a manual request is waiting. On yield:
   stop the batch, still run cleanup and feed generation, mark the result
   interrupted, and re-queue the feed at the head of the scheduled lane as a
   normal update.
4. **Status and API on the scheduler.** `SyncNow(feedID)`, `SyncAll()`,
   `Status()`, backed by a mutex-guarded in-memory map written by the run
   loop. Check feed existence through `store.Get()`, not `s.feeds`. A
   request for the feed currently updating queues exactly one follow-up.
   Refuse during backoff, and within 30 seconds of that feed's last manual
   run. Errors exposed as fixed categories; raw error text goes to the log
   only.
5. **Admin routes.** `POST /admin/feeds/{id}/sync` (202, 404, 429 with
   `Retry-After`), `POST /admin/sync`, `GET /admin/sync`. `admin.New` takes
   a small interface so tests use a fake.
6. **Same-port mount.** New `admin.mount_on_server` (default false, existing
   separate listener unchanged). When true, mount the admin handler under
   `/admin/` on the web server and do not start the second listener. Reserve
   feed ID `admin` in `validate`. Add `admin.allow_config_edit` (default
   false); when false the PUT/DELETE config routes return 403.
7. **Page.** Embedded assets (`go:embed`) served at `/admin/`: one HTML
   file, one script, one stylesheet, no third-party resources, CSP
   `default-src 'none'; script-src 'self'; style-src 'self'; connect-src
   'self'`. Token typed by the user, kept in `sessionStorage`, sent as
   `Authorization`. Table of feeds with status, "Sync now", "Sync all";
   poll `GET /admin/sync` every 2 seconds while anything is queued or
   syncing. Render with `textContent` only.
8. **Hooks.** In `html/admin.html` map `PostEpisodeDownload` and
   `OnEpisodeDownloadError` to `post_episode_download` and
   `on_episode_download_error` (`command`, `timeout`) instead of dropping
   them.
9. **Docs.** Update `CLAUDE.md` and `config.toml.example`.

## 5. Acceptance tests

1. Equivalence: identical database copies and fake provider, one run through
   each lane; episode records and feed XML identical apart from
   `lastBuildDate`.
2. Existing GUIDs and enclosures unchanged after a manual run that finds
   nothing and after one that adds an item.
3. Feed A blocked mid-download, five scheduled entries queued, `SyncNow(B)`:
   A's download completes, B runs next, A resumes afterwards.
4. Ten rapid requests for one feed produce one run.
5. Scheduled tick while a manual entry is pending: one run.
6. Request for the feed in flight: exactly one follow-up.
7. Backoff active: refused with retry time, provider not called.
8. Shutdown with 24 feeds queued: no panic, no retry increments.
9. HTTP: 401 without or with a wrong token on every `/admin` API route, 404
   unknown feed, 403 on config writes by default; an injected error
   containing `key=SECRET` never appears in a response.
10. Routing: every registered admin route and asset path, run against
    `^/[^/]+\.(xml|opml|jpe?g|png|webp)$|^/[^/]+/[^/]+\.(mp3|m4a|mp4|aac|ogg|opus|webm)$`,
    does not match.
11. Cron `Next` times unchanged by a manual run.

## 6. Local environment

- Two tests fail here only (loopback refused): `TestOdyseeBuilderBuild` and
  `TestExecuteHook_CurlWebhook`. Never use `httptest.NewServer`; use
  `httptest.NewRecorder` or in-process fakes.
- No race detector locally; it runs in CI.
- Working tree is CRLF. Lint and test a clean export: `git -c
  core.autocrlf=false archive HEAD | tar -x -C <dir>`, then `golangci-lint
  run ./...` and `go test ./...`.
- No module downloads are possible; add no dependencies.

## 7. Deployment and rollback (for the owner's review, not for the worker)

- Stack change beyond the image line: pass `PODSYNC_ADMIN_TOKEN` to the
  `podsync` service. Confirm the token is 32+ characters without printing it.
- Config change on the host: `[admin] enabled = true`, `mount_on_server =
  true`. A missing or short token now stops start-up, so apply the Env
  change first.
- Isolated-container test against a database copy before deploy; fresh
  GUID baseline before and after.
- External check from outside the LAN, about five requests, each expecting
  the proxy's 403 or 404 and never a 401 from PodSync: `/admin/`,
  `/admin/sync`, the script, a POST to a sync route, and
  `/admin/feeds%2FID6.mp3`.
- Rollback: restore the previous image and stack file through Portainer and
  remove the `[admin]` block. No data migration is involved; status is in
  memory only.

## 8. Open decisions, with defaults

- **Feed editing in the new page.** Default: not in this version. The
  production config is read-only, so edits cannot succeed there; the hooks
  fix in step 8 covers the legacy editor.
- **Public router narrowing** (`&& !PathPrefix(`/admin`)`). Default: propose
  it as optional hardening; it is a stack label change.
- **Manual-request spacing.** Default 30 seconds per feed.

## 9. Not in scope, listed for later

Retry-policy redesign, "Retry failed" action, health check semantics,
subprocess group termination, init process in the container, S3-mode signal
handling, log volume, documentation corrections in `CLAUDE.md`.
