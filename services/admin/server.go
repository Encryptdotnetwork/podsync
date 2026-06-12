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
)

type Server struct {
	http.Server

	store  *config.Store
	reload func() error
	token  string
}

// New creates the admin API server. All configuration reads and edits go
// through the store; reload is invoked by POST /api/reload to force a re-read
// of config.toml from disk (e.g. after a hand edit over SSH).
func New(cfg config.Admin, store *config.Store, reload func() error) *Server {
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
		token:  cfg.Token,
	}

	mux := http.NewServeMux()

	// Liveness probe for the admin listener itself, intentionally unauthenticated
	mux.HandleFunc("GET /healthz", srv.handleHealthz)

	mux.HandleFunc("GET /api/config", srv.requireAuth(srv.handleGetConfig))
	mux.HandleFunc("GET /api/config/{section}", srv.requireAuth(srv.handleGetSection))
	mux.HandleFunc("PUT /api/config/{section}", srv.requireAuth(srv.handlePutSection))
	mux.HandleFunc("GET /api/feeds", srv.requireAuth(srv.handleListFeeds))
	mux.HandleFunc("GET /api/feeds/{id}", srv.requireAuth(srv.handleGetFeed))
	mux.HandleFunc("PUT /api/feeds/{id}", srv.requireAuth(srv.handlePutFeed))
	mux.HandleFunc("DELETE /api/feeds/{id}", srv.requireAuth(srv.handleDeleteFeed))
	mux.HandleFunc("POST /api/reload", srv.requireAuth(srv.handleReload))

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

func (s *Server) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}
