package admin

import (
	"embed"
	"net/http"
)

// The admin page is three static files compiled into the binary: nothing is
// read from disk at runtime, nothing is loaded from a third-party host, and
// nothing secret is in them (the token is typed by the user in the browser).
//
//go:embed ui/index.html ui/admin.js ui/admin.css
var uiFiles embed.FS

// pageCSP confines the page to its own origin: scripts and styles only from
// files served here (so no inline script or style), requests only back to this
// origin, no framing and no form posts.
const pageCSP = "default-src 'none'; script-src 'self'; style-src 'self'; connect-src 'self'; " +
	"base-uri 'none'; form-action 'none'; frame-ancestors 'none'"

// staticHandler serves one embedded file. The page and its assets are public
// by design; every API route they call is behind the bearer token.
func staticHandler(name, contentType string) http.HandlerFunc {
	data, err := uiFiles.ReadFile("ui/" + name)
	if err != nil {
		panic("admin: missing embedded asset " + name) // a build error, caught by tests
	}

	return func(w http.ResponseWriter, _ *http.Request) {
		h := w.Header()
		h.Set("Content-Type", contentType)
		h.Set("Content-Security-Policy", pageCSP)
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Cache-Control", "no-store")
		_, _ = w.Write(data)
	}
}
