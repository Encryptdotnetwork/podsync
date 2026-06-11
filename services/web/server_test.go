package web

import (
	"bytes"
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mxpv/podsync/pkg/fs"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type mockFileSystem struct{}

func (m *mockFileSystem) Open(name string) (http.File, error) {
	return nil, http.ErrMissingFile
}

func TestDebugEndpointDisabledByDefault(t *testing.T) {
	cfg := Config{
		Port: 8080,
		Path: "feeds",
	}

	srv := New(cfg, &mockFileSystem{}, nil)

	req := httptest.NewRequest(http.MethodGet, "/debug/vars", nil)
	rec := httptest.NewRecorder()

	srv.Handler.ServeHTTP(rec, req)

	// Should return 404 when debug endpoints are disabled
	assert.Equal(t, http.StatusNotFound, rec.Code)
	// Should NOT contain expvar data
	assert.False(t, strings.Contains(rec.Body.String(), "cmdline"))
}

func TestDebugEndpointEnabledWhenConfigured(t *testing.T) {
	cfg := Config{
		Port:           8080,
		Path:           "feeds",
		DebugEndpoints: true,
	}

	srv := New(cfg, &mockFileSystem{}, nil)

	req := httptest.NewRequest(http.MethodGet, "/debug/vars", nil)
	rec := httptest.NewRecorder()

	srv.Handler.ServeHTTP(rec, req)

	// Should return 200 and JSON content when debug endpoints are enabled
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Header().Get("Content-Type"), "application/json")
	// Verify it contains expvar data (cmdline is always present)
	assert.True(t, strings.Contains(rec.Body.String(), "cmdline"))
}

func TestNoIndexDisabledByDefault(t *testing.T) {
	cfg := Config{
		Port: 8080,
		Path: "feeds",
	}

	srv := New(cfg, &mockFileSystem{}, nil)

	// robots.txt should return 404 when disabled
	req := httptest.NewRequest(http.MethodGet, "/robots.txt", nil)
	rec := httptest.NewRecorder()
	srv.Handler.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusNotFound, rec.Code)

	// X-Robots-Tag header should not be present on feed requests
	req = httptest.NewRequest(http.MethodGet, "/feeds/test.xml", nil)
	rec = httptest.NewRecorder()
	srv.Handler.ServeHTTP(rec, req)
	assert.Empty(t, rec.Header().Get("X-Robots-Tag"))
}

func TestNoIndexEnabledWhenConfigured(t *testing.T) {
	cfg := Config{
		Port:    8080,
		Path:    "feeds",
		NoIndex: true,
	}

	srv := New(cfg, &mockFileSystem{}, nil)

	// robots.txt should return disallow all
	req := httptest.NewRequest(http.MethodGet, "/robots.txt", nil)
	rec := httptest.NewRecorder()
	srv.Handler.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "text/plain", rec.Header().Get("Content-Type"))
	assert.Contains(t, rec.Body.String(), "User-agent: *")
	assert.Contains(t, rec.Body.String(), "Disallow: /")

	// X-Robots-Tag header should be present on all responses
	req = httptest.NewRequest(http.MethodGet, "/feeds/test.xml", nil)
	rec = httptest.NewRecorder()
	srv.Handler.ServeHTTP(rec, req)
	assert.Equal(t, "noindex, nofollow", rec.Header().Get("X-Robots-Tag"))
}

func TestNoListingDisabledByDefault(t *testing.T) {
	tmpDir := t.TempDir()

	// Create storage with NoListing disabled (default)
	storage, err := fs.NewLocal(tmpDir, false, false)
	require.NoError(t, err)

	// Create a file inside a subdirectory
	_, err = storage.Create(context.Background(), "feeds/episode.mp3", bytes.NewReader([]byte("audio content")))
	require.NoError(t, err)

	cfg := Config{
		Port: 8080,
		Path: "",
	}

	srv := New(cfg, storage, nil)

	// Accessing a directory should return 200 with directory listing
	req := httptest.NewRequest(http.MethodGet, "/feeds/", nil)
	rec := httptest.NewRecorder()
	srv.Handler.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), "episode.mp3")

	// Accessing root should also return 200 with directory listing
	req = httptest.NewRequest(http.MethodGet, "/", nil)
	rec = httptest.NewRecorder()
	srv.Handler.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), "feeds")

	// Accessing a file should work
	req = httptest.NewRequest(http.MethodGet, "/feeds/episode.mp3", nil)
	rec = httptest.NewRecorder()
	srv.Handler.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "audio content", rec.Body.String())
}

func TestThumbnailHostAllowed(t *testing.T) {
	allowed := []string{
		"i.ytimg.com",
		"yt3.ggpht.com",
		"yt3.googleusercontent.com",
		"sp.rmbl.ws",
		"hugh.cdn.rumble.cloud",
		"1a-1791.com",
		"thumbs.odycdn.com",
		"thumbnails.lbry.com",
		"spee.ch",
		"i.vimeocdn.com",
		"I.SNDCDN.COM", // case-insensitive
	}
	for _, host := range allowed {
		assert.True(t, thumbnailHostAllowed(host), "expected %q to be allowed", host)
	}

	denied := []string{
		"evil-ytimg.com",         // suffix without dot boundary
		"ytimg.com.attacker.net", // allowlisted name as subdomain of attacker
		"localhost",
		"127.0.0.1",
		"192.168.4.6",
		"example.com",
		"",
	}
	for _, host := range denied {
		assert.False(t, thumbnailHostAllowed(host), "expected %q to be denied", host)
	}
}

func TestIsDisallowedIP(t *testing.T) {
	disallowed := []string{"127.0.0.1", "10.0.0.1", "172.16.5.5", "192.168.4.6", "169.254.1.1", "0.0.0.0", "::1", "fe80::1"}
	for _, s := range disallowed {
		ip := net.ParseIP(s)
		require.NotNil(t, ip)
		assert.True(t, isDisallowedIP(ip), "expected %q to be disallowed", s)
	}

	allowed := []string{"142.250.70.78", "205.250.1.1", "2607:f8b0::1"}
	for _, s := range allowed {
		ip := net.ParseIP(s)
		require.NotNil(t, ip)
		assert.False(t, isDisallowedIP(ip), "expected %q to be allowed", s)
	}
}

func TestThumbnailProxyRejectsDisallowedHost(t *testing.T) {
	cfg := Config{Port: 8080, Path: "feeds"}
	srv := New(cfg, &mockFileSystem{}, nil)

	// Missing url parameter
	req := httptest.NewRequest(http.MethodGet, "/thumbnail", nil)
	rec := httptest.NewRecorder()
	srv.Handler.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusBadRequest, rec.Code)

	// Non-allowlisted host
	req = httptest.NewRequest(http.MethodGet, "/thumbnail?url=http://192.168.4.6:8090/secret", nil)
	rec = httptest.NewRecorder()
	srv.Handler.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusForbidden, rec.Code)

	// Invalid scheme
	req = httptest.NewRequest(http.MethodGet, "/thumbnail?url=file:///etc/passwd", nil)
	rec = httptest.NewRecorder()
	srv.Handler.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestNoListingEnabledWhenConfigured(t *testing.T) {
	tmpDir := t.TempDir()

	storage, err := fs.NewLocal(tmpDir, false, true)
	require.NoError(t, err)

	// Create a file inside a subdirectory
	_, err = storage.Create(context.Background(), "feeds/episode.mp3", bytes.NewReader([]byte("audio content")))
	require.NoError(t, err)

	cfg := Config{
		Port: 8080,
		Path: "",
	}

	srv := New(cfg, storage, nil)

	// Accessing a directory should return 404
	req := httptest.NewRequest(http.MethodGet, "/feeds/", nil)
	rec := httptest.NewRecorder()
	srv.Handler.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusNotFound, rec.Code)

	// Accessing root should also return 404
	req = httptest.NewRequest(http.MethodGet, "/", nil)
	rec = httptest.NewRecorder()
	srv.Handler.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusNotFound, rec.Code)

	// Accessing a file should still work
	req = httptest.NewRequest(http.MethodGet, "/feeds/episode.mp3", nil)
	rec = httptest.NewRecorder()
	srv.Handler.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "audio content", rec.Body.String())
}
