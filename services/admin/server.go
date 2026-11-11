// Package admin implements the internal administrative HTTP API. It runs as a
// listener completely separate from the public feed server, exposes only
// management endpoints, and is intended to be reachable from trusted networks
// only (e.g. behind a Traefik IP allowlist, never published to the internet).
package admin

import (
	"crypto/subtle"
	"fmt"
	"net/http"
	"strings"

	"github.com/mxpv/podsync/pkg/config"
	"github.com/mxpv/podsync/services/update"
)

// Syncer is the part of the update scheduler the admin API drives: manual
// "Sync now" requests and their status. *update.Scheduler implements it; tests
// substitute a fake.
type Syncer interface {
	SyncNow(feedID string) (update.FeedStatus, error)
	SyncAll() (update.SyncAllResult, error)
	Status() update.SyncStatus
}

type Server struct {
	http.Server

	store  *config.Store
	reload func() error
	syncer Syncer
	token  string

	// allowConfigEdit gates the routes that rewrite config.toml
	allowConfigEdit bool

	routes []string
}

// New creates the admin API server. All configuration reads and edits go
// through the store; reload is invoked by POST /admin/reload to force a
// re-read of config.toml from disk (e.g. after a hand edit over SSH); syncer
// serves the manual sync routes (nil disables them with a 503).
func New(cfg config.Admin, store *config.Store, reload func() error, syncer Syncer) *Server {
	port := cfg.Port
	if port == 0 {
		port = config.DefaultAdminPort
	}

	bindAddress := cfg.BindAddress
	if bindAddress == "*" {
		bindAddress = ""
	}

	srv := &Server{
		store:  store,
		reload: reload,
		syncer: syncer,
		token:  cfg.Token,

		allowConfigEdit: cfg.AllowConfigEdit,
	}

	mux := http.NewServeMux()
	handle := func(pattern string, h http.HandlerFunc) {
		srv.routes = append(srv.routes, pattern)
		mux.HandleFunc(pattern, h)
	}

	// Liveness probe for the admin listener itself, intentionally unauthenticated
	handle("GET /healthz", srv.handleHealthz)

	// Management routes live under /admin: the public web UI fetches relative
	// paths on the feed server (podsync.opml, feed XML, /thumbnail), and using a
	// distinct prefix keeps host-level reverse proxy rules collision-free.
	handle("GET /admin/config", srv.requireAuth(srv.handleGetConfig))
	handle("GET /admin/config/{section}", srv.requireAuth(srv.handleGetSection))
	handle("PUT /admin/config/{section}", srv.requireAuth(srv.requireConfigEdit(srv.handlePutSection)))
	handle("GET /admin/feeds", srv.requireAuth(srv.handleListFeeds))
	handle("GET /admin/feeds/{id}", srv.requireAuth(srv.handleGetFeed))
	handle("PUT /admin/feeds/{id}", srv.requireAuth(srv.requireConfigEdit(srv.handlePutFeed)))
	handle("DELETE /admin/feeds/{id}", srv.requireAuth(srv.requireConfigEdit(srv.handleDeleteFeed)))
	handle("POST /admin/reload", srv.requireAuth(srv.handleReload))

	// Manual sync ("Sync now"): queue requests and read the status
	handle("POST /admin/feeds/{id}/sync", srv.requireAuth(srv.handleSyncFeed))
	handle("POST /admin/sync", srv.requireAuth(srv.handleSyncAll))
	handle("GET /admin/sync", srv.requireAuth(srv.handleSyncStatus))

	// The page and its assets are public (no secrets in them); the token is
	// typed in the browser and sent with each API call. "{$}" matches only
	// "/admin/" itself.
	handle("GET /admin/{$}", staticHandler("index.html", "text/html; charset=utf-8"))
	handle("GET /admin/admin.js", staticHandler("admin.js", "text/javascript; charset=utf-8"))
	handle("GET /admin/admin.css", staticHandler("admin.css", "text/css; charset=utf-8"))

	srv.Addr = fmt.Sprintf("%s:%d", bindAddress, port)
	srv.Handler = mux

	return srv
}

// requireAuth enforces "Authorization: Bearer <token>" when a token is
// configured. With an empty token the network layer (Traefik IP allowlist,
// Docker network isolation) is the only gate.
func (s *Server) requireAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.token == "" {
			next(w, r)
			return
		}

		const prefix = "Bearer "
		auth := r.Header.Get("Authorization")
		if len(auth) <= len(prefix) ||
			!strings.EqualFold(auth[:len(prefix)], prefix) ||
			subtle.ConstantTimeCompare([]byte(auth[len(prefix):]), []byte(s.token)) != 1 {
			w.Header().Set("WWW-Authenticate", `Bearer realm="podsync-admin"`)
			writeError(w, http.StatusUnauthorized, "unauthorized")
			return
		}

		next(w, r)
	}
}

// requireConfigEdit refuses a config-writing route with 403 unless
// admin.allow_config_edit is set. It is applied inside requireAuth, so callers
// without a valid token still get 401.
func (s *Server) requireConfigEdit(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.allowConfigEdit {
			writeError(w, http.StatusForbidden, "config editing is disabled (admin.allow_config_edit)")
			return
		}

		next(w, r)
	}
}

// Routes returns every registered route pattern ("METHOD /path" with {name}
// wildcards). Tests use it to check the routes against the public router's
// path patterns.
func (s *Server) Routes() []string {
	return append([]string(nil), s.routes...)
}

func (s *Server) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}
