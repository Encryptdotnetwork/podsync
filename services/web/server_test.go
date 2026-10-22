package web

import (
	"bytes"
	"context"
	"io"
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

	// Special-purpose ranges beyond the net.IP helpers, plus IPv4-mapped loopback
	special := []string{"100.64.1.1", "198.18.0.1", "192.0.0.8", "224.0.0.1", "240.0.0.1", "::ffff:127.0.0.1", "64:ff9b::a00:1", "2002:c0a8:406::1", "ff02::1"}
	for _, s := range special {
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

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

// stubThumbnailUpstream points the proxy client at a canned upstream response
// for the duration of the test.
func stubThumbnailUpstream(t *testing.T, status int, contentType string, body []byte) {
	t.Helper()
	orig := thumbnailProxyClient
	thumbnailProxyClient = &http.Client{
		Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			header := http.Header{}
			if contentType != "" {
				header.Set("Content-Type", contentType)
			}
			return &http.Response{
				StatusCode:    status,
				Header:        header,
				Body:          io.NopCloser(bytes.NewReader(body)),
				ContentLength: int64(len(body)),
				Request:       req,
			}, nil
		}),
	}
	t.Cleanup(func() { thumbnailProxyClient = orig })
}

func fetchThumbnail(t *testing.T, method string) *httptest.ResponseRecorder {
	t.Helper()
	srv := New(Config{Port: 8080}, &mockFileSystem{}, nil)
	req := httptest.NewRequest(method, "/thumbnail?url=https%3A%2F%2Fi.ytimg.com%2Fvi%2Fabc%2Fmaxresdefault.jpg", nil)
	rec := httptest.NewRecorder()
	srv.Handler.ServeHTTP(rec, req)
	return rec
}

var (
	pngBytes  = append([]byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR"), make([]byte, 600)...)
	jpegBytes = append([]byte("\xff\xd8\xff\xe0\x00\x10JFIF\x00"), make([]byte, 100)...)
	webpBytes = []byte("RIFF\x24\x00\x00\x00WEBPVP8 \x18\x00\x00\x00")
	htmlBytes = []byte("<!DOCTYPE html><html><head><title>marker</title></head><body>podsync-inert-marker</body></html>")
	svgBytes  = []byte(`<?xml version="1.0"?><svg xmlns="http://www.w3.org/2000/svg"><text>podsync-inert-marker</text></svg>`)
)

func TestThumbnailProxyServesValidatedImages(t *testing.T) {
	cases := []struct {
		name        string
		declared    string
		body        []byte
		wantType    string
		wantPayload []byte
	}{
		{"png", "image/png", pngBytes, "image/png", pngBytes},
		{"jpeg without content type", "", jpegBytes, "image/jpeg", jpegBytes},
		{"webp as octet-stream", "application/octet-stream", webpBytes, "image/webp", webpBytes},
		{"jpeg mislabelled as png", "image/png", jpegBytes, "image/jpeg", jpegBytes},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stubThumbnailUpstream(t, http.StatusOK, tc.declared, tc.body)
			rec := fetchThumbnail(t, http.MethodGet)
			assert.Equal(t, http.StatusOK, rec.Code)
			assert.Equal(t, tc.wantType, rec.Header().Get("Content-Type"))
			assert.Equal(t, tc.wantPayload, rec.Body.Bytes())
			assert.Equal(t, "nosniff", rec.Header().Get("X-Content-Type-Options"))
			assert.Equal(t, "default-src 'none'; sandbox", rec.Header().Get("Content-Security-Policy"))
		})
	}
}

func TestThumbnailProxyRejectsActiveContent(t *testing.T) {
	cases := []struct {
		name     string
		declared string
		body     []byte
	}{
		{"html page", "text/html; charset=utf-8", htmlBytes},
		{"html labelled as jpeg", "image/jpeg", htmlBytes},
		{"html as octet-stream", "application/octet-stream", htmlBytes},
		{"svg", "image/svg+xml", svgBytes},
		{"svg labelled as png", "image/png", svgBytes},
		{"svg without content type", "", svgBytes},
		{"javascript", "application/javascript", []byte("alert(1)")},
		{"invalid content type", "image/png; ===", pngBytes},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stubThumbnailUpstream(t, http.StatusOK, tc.declared, tc.body)
			rec := fetchThumbnail(t, http.MethodGet)
			assert.Equal(t, http.StatusUnsupportedMediaType, rec.Code)
			assert.NotContains(t, rec.Body.String(), "podsync-inert-marker")
			assert.NotContains(t, rec.Header().Get("Content-Type"), "html")
			assert.Equal(t, "nosniff", rec.Header().Get("X-Content-Type-Options"))
		})
	}
}

func TestThumbnailProxyUpstreamFailures(t *testing.T) {
	stubThumbnailUpstream(t, http.StatusNotFound, "text/html", htmlBytes)
	rec := fetchThumbnail(t, http.MethodGet)
	assert.Equal(t, http.StatusBadGateway, rec.Code)
	assert.NotContains(t, rec.Body.String(), "podsync-inert-marker")

	stubThumbnailUpstream(t, http.StatusOK, "image/png", make([]byte, maxThumbnailBytes+1))
	rec = fetchThumbnail(t, http.MethodGet)
	assert.Equal(t, http.StatusBadGateway, rec.Code)

	stubThumbnailUpstream(t, http.StatusOK, "image/png", nil)
	rec = fetchThumbnail(t, http.MethodGet)
	assert.Equal(t, http.StatusBadGateway, rec.Code)

	rec = fetchThumbnail(t, http.MethodPost)
	assert.Equal(t, http.StatusMethodNotAllowed, rec.Code)
}

func TestThumbnailProxyHead(t *testing.T) {
	stubThumbnailUpstream(t, http.StatusOK, "image/png", pngBytes)
	rec := fetchThumbnail(t, http.MethodHead)
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "image/png", rec.Header().Get("Content-Type"))
	assert.Empty(t, rec.Body.Bytes())
}

func TestSecurityHeaders(t *testing.T) {
	srv := New(Config{Port: 8080, WebUIEnabled: true}, &mockFileSystem{}, nil)

	for _, p := range []string{"/", "/index.html"} {
		rec := httptest.NewRecorder()
		srv.Handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, p, nil))
		assert.Equal(t, webUICSP, rec.Header().Get("Content-Security-Policy"), p)
		assert.Equal(t, "nosniff", rec.Header().Get("X-Content-Type-Options"), p)
	}

	rec := httptest.NewRecorder()
	srv.Handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/ID1.xml", nil))
	assert.Empty(t, rec.Header().Get("Content-Security-Policy"))
	assert.Equal(t, "nosniff", rec.Header().Get("X-Content-Type-Options"))

	// No UI policy when the web UI is disabled
	srv = New(Config{Port: 8080}, &mockFileSystem{}, nil)
	rec = httptest.NewRecorder()
	srv.Handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	assert.Empty(t, rec.Header().Get("Content-Security-Policy"))
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
