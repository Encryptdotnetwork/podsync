package admin

import (
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mxpv/podsync/pkg/config"
	"github.com/mxpv/podsync/services/web"
)

type emptyFileSystem struct{}

func (emptyFileSystem) Open(string) (http.File, error) { return nil, os.ErrNotExist }

func TestPageIsServedWithoutAuthAndStrictCSP(t *testing.T) {
	env := setupServerWith(t, config.Admin{Enabled: true, Token: testToken}, &fakeSyncer{})

	for path, contentType := range map[string]string{
		"/admin/":          "text/html; charset=utf-8",
		"/admin/admin.js":  "text/javascript; charset=utf-8",
		"/admin/admin.css": "text/css; charset=utf-8",
	} {
		rec := env.do(t, http.MethodGet, path, "", nil)
		require.Equal(t, http.StatusOK, rec.Code, path)
		assert.Equal(t, contentType, rec.Header().Get("Content-Type"), path)
		assert.Equal(t, pageCSP, rec.Header().Get("Content-Security-Policy"), path)
		assert.Equal(t, "nosniff", rec.Header().Get("X-Content-Type-Options"), path)
		assert.Equal(t, "no-store", rec.Header().Get("Cache-Control"), path)
		assert.NotEmpty(t, rec.Body.Bytes(), path)
	}

	assert.Contains(t, pageCSP, "default-src 'none'")
	assert.Contains(t, pageCSP, "script-src 'self'")
	assert.Contains(t, pageCSP, "style-src 'self'")
	assert.Contains(t, pageCSP, "connect-src 'self'")
	assert.NotContains(t, pageCSP, "unsafe-inline")
	assert.NotContains(t, pageCSP, "http")

	// "/admin/{$}" is exactly the page: unknown assets are not served
	assert.Equal(t, http.StatusNotFound, env.do(t, http.MethodGet, "/admin/other.js", "", nil).Code)
}

// The page carries no secret, loads nothing from other hosts, has no inline
// script and writes dynamic content as text only.
func TestPageAssetsAreSelfContainedAndSafe(t *testing.T) {
	env := setupServerWith(t, config.Admin{Enabled: true, Token: testToken}, &fakeSyncer{})

	body := func(path string) string { return env.do(t, http.MethodGet, path, "", nil).Body.String() }
	html, js, css := body("/admin/"), body("/admin/admin.js"), body("/admin/admin.css")

	for name, content := range map[string]string{"html": html, "js": js, "css": css} {
		assert.NotContains(t, content, testToken, name)
		assert.NotRegexp(t, `https?://`, content, "%s must not reference other hosts", name)
		assert.NotContains(t, content, "@import", name)
	}

	// No inline scripts or handlers in the page
	for _, script := range regexp.MustCompile(`(?is)<script\b[^>]*>`).FindAllString(html, -1) {
		assert.Contains(t, script, `src="admin.js"`)
	}
	assert.NotRegexp(t, `(?i)\son[a-z]+\s*=`, html)
	assert.NotContains(t, html, "style=")

	for _, banned := range []string{"innerHTML", "outerHTML", "insertAdjacentHTML", "document.write", "eval(", "new Function", "localStorage"} {
		assert.NotContains(t, js, banned)
	}
	assert.Contains(t, js, "textContent")
	assert.Contains(t, js, "Completed with errors")
	assert.Contains(t, js, "completed_with_errors")
	assert.Contains(t, css, ".state-completed_with_errors")
	assert.Contains(t, js, "sessionStorage")
	assert.Contains(t, js, "Authorization")
}

// With the admin handler mounted on the web server, /admin/ is served by it
// (with its own CSP), API routes still demand the token, and media paths keep
// going to the file server.
func TestMountedOnWebServer(t *testing.T) {
	env := setupServerWith(t, config.Admin{Enabled: true, Token: testToken}, &fakeSyncer{})
	srv := web.New(web.Config{Port: 8080, WebUIEnabled: true}, emptyFileSystem{}, nil, web.WithAdminHandler(env.srv.Handler))

	get := func(method, path string, header map[string]string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(""))
		for k, v := range header {
			req.Header.Set(k, v)
		}
		rec := httptest.NewRecorder()
		srv.Handler.ServeHTTP(rec, req)
		return rec
	}

	rec := get(http.MethodGet, "/admin/", nil)
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, pageCSP, rec.Header().Get("Content-Security-Policy"))
	assert.Equal(t, "nosniff", rec.Header().Get("X-Content-Type-Options"))

	assert.Equal(t, http.StatusOK, get(http.MethodGet, "/admin/admin.js", nil).Code)

	assert.Equal(t, http.StatusUnauthorized, get(http.MethodGet, "/admin/sync", nil).Code)
	assert.Equal(t, http.StatusUnauthorized, get(http.MethodPost, "/admin/feeds/A/sync", nil).Code)
	assert.Equal(t, http.StatusUnauthorized, get(http.MethodGet, "/admin/config", nil).Code)
	assert.Equal(t, http.StatusOK, get(http.MethodGet, "/admin/sync", bearer).Code)
	assert.Equal(t, http.StatusAccepted, get(http.MethodPost, "/admin/feeds/A/sync", bearer).Code)

	// The unauthenticated liveness probe is not exposed on the public listener
	assert.Equal(t, http.StatusNotFound, get(http.MethodGet, "/healthz", nil).Code)

	// Feed and media paths are untouched: they still reach the file server
	assert.Equal(t, http.StatusNotFound, get(http.MethodGet, "/ID1.xml", nil).Code)
	assert.Equal(t, http.StatusNotFound, get(http.MethodGet, "/ID1/e1.mp3", nil).Code)
}
