package admin

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mxpv/podsync/pkg/config"
)

const adminTestConfig = `# Podsync admin test configuration

[server]
port = 8080

[storage]
  [storage.local]
  # Don't change if you run podsync via docker
  data_dir = "/app/data"

[tokens]
youtube = "youtube_key_1234"

[feeds]
  [feeds.A]
  url = "https://youtube.com/channel/test1"
  page_size = 10
`

type testEnv struct {
	srv      *Server
	store    *config.Store
	path     string
	reloaded *bool
}

func setupServer(t *testing.T, token string) testEnv {
	t.Helper()

	path := filepath.Join(t.TempDir(), "config.toml")
	require.NoError(t, os.WriteFile(path, []byte(adminTestConfig), 0644))

	store, err := config.NewStore(path)
	require.NoError(t, err)

	reloaded := false
	srv := New(config.Admin{Enabled: true, Token: token}, store, func() error {
		reloaded = true
		_, err := store.Reload()
		return err
	})

	return testEnv{srv: srv, store: store, path: path, reloaded: &reloaded}
}

func (e testEnv) do(t *testing.T, method, target, body string, header map[string]string) *httptest.ResponseRecorder {
	t.Helper()

	var reader *strings.Reader
	if body != "" {
		reader = strings.NewReader(body)
	} else {
		reader = strings.NewReader("")
	}

	req := httptest.NewRequest(method, target, reader)
	for k, v := range header {
		req.Header.Set(k, v)
	}

	rec := httptest.NewRecorder()
	e.srv.Handler.ServeHTTP(rec, req)
	return rec
}

func TestAdminAuth(t *testing.T) {
	env := setupServer(t, "secret-token")

	// No credentials
	rec := env.do(t, http.MethodGet, "/api/config", "", nil)
	assert.Equal(t, http.StatusUnauthorized, rec.Code)

	// Wrong token
	rec = env.do(t, http.MethodGet, "/api/config", "", map[string]string{"Authorization": "Bearer wrong"})
	assert.Equal(t, http.StatusUnauthorized, rec.Code)

	// Correct token
	rec = env.do(t, http.MethodGet, "/api/config", "", map[string]string{"Authorization": "Bearer secret-token"})
	assert.Equal(t, http.StatusOK, rec.Code)

	// healthz is intentionally unauthenticated
	rec = env.do(t, http.MethodGet, "/healthz", "", nil)
	assert.Equal(t, http.StatusOK, rec.Code)
}

func TestAdminGetConfigRedactsTokens(t *testing.T) {
	env := setupServer(t, "")

	rec := env.do(t, http.MethodGet, "/api/config", "", nil)
	require.Equal(t, http.StatusOK, rec.Code)

	body := rec.Body.String()
	assert.Contains(t, body, "****1234", "token should be redacted to last 4 chars")
	assert.NotContains(t, body, "youtube_key_1234", "full token must never appear")

	assert.Equal(t, env.store.ETag(), rec.Header().Get("ETag"))
}

func TestAdminGetSection(t *testing.T) {
	env := setupServer(t, "")

	rec := env.do(t, http.MethodGet, "/api/config/server", "", nil)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), `"Port":8080`)

	rec = env.do(t, http.MethodGet, "/api/config/tokens", "", nil)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), "****1234")
	assert.NotContains(t, rec.Body.String(), "youtube_key_1234")

	rec = env.do(t, http.MethodGet, "/api/config/bogus", "", nil)
	assert.Equal(t, http.StatusNotFound, rec.Code)
}

func TestAdminPutSection(t *testing.T) {
	env := setupServer(t, "")

	rec := env.do(t, http.MethodPut, "/api/config/tokens", `{"youtube": "replacement_key_9999"}`, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), `"applied":true`)

	// Snapshot and disk updated
	require.Len(t, env.store.Get().Tokens["youtube"], 1)
	assert.Equal(t, "replacement_key_9999", env.store.Get().Tokens["youtube"][0])

	data, err := os.ReadFile(env.path)
	require.NoError(t, err)
	assert.Contains(t, string(data), "replacement_key_9999")

	// Comments outside the replaced section survive
	assert.Contains(t, string(data), "# Don't change if you run podsync via docker")
	assert.Contains(t, string(data), "# Podsync admin test configuration")
}

func TestAdminPutSectionRestartRequired(t *testing.T) {
	env := setupServer(t, "")

	rec := env.do(t, http.MethodPut, "/api/config/log", `{"debug": true}`, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), `"applied":false`)
	assert.Contains(t, rec.Body.String(), `"restart_required":true`)
}

func TestAdminPutSectionETagConflict(t *testing.T) {
	env := setupServer(t, "")

	rec := env.do(t, http.MethodPut, "/api/config/tokens", `{"youtube": "k"}`,
		map[string]string{"If-Match": `"0000000000000000000000000000000000000000000000000000000000000000"`})
	assert.Equal(t, http.StatusPreconditionFailed, rec.Code)

	// With the current ETag the same request succeeds
	rec = env.do(t, http.MethodPut, "/api/config/tokens", `{"youtube": "k"}`,
		map[string]string{"If-Match": env.store.ETag()})
	assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
}

func TestAdminPutSectionValidationFailure(t *testing.T) {
	env := setupServer(t, "")

	before, err := os.ReadFile(env.path)
	require.NoError(t, err)

	rec := env.do(t, http.MethodPut, "/api/config/storage", `{"type": "bogus"}`, nil)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "validation failed")

	// Disk untouched
	after, err := os.ReadFile(env.path)
	require.NoError(t, err)
	assert.Equal(t, before, after)
}

func TestAdminFeedCRUD(t *testing.T) {
	env := setupServer(t, "")

	// Create
	rec := env.do(t, http.MethodPut, "/api/feeds/NEW",
		`{"url": "https://youtube.com/channel/new", "page_size": 25, "format": "audio"}`, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	newFeed := env.store.Get().Feeds["NEW"]
	require.NotNil(t, newFeed)
	assert.Equal(t, "https://youtube.com/channel/new", newFeed.URL)
	assert.EqualValues(t, 25, newFeed.PageSize, "json.Number must round-trip as TOML integer")

	// Read single + list
	rec = env.do(t, http.MethodGet, "/api/feeds/NEW", "", nil)
	assert.Equal(t, http.StatusOK, rec.Code)

	rec = env.do(t, http.MethodGet, "/api/feeds", "", nil)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), `"id":"A"`)
	assert.Contains(t, rec.Body.String(), `"id":"NEW"`)

	// Replace: the old block (page_size) is fully replaced, not merged
	rec = env.do(t, http.MethodPut, "/api/feeds/NEW", `{"url": "https://youtube.com/channel/new2"}`, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	replaced := env.store.Get().Feeds["NEW"]
	assert.Equal(t, "https://youtube.com/channel/new2", replaced.URL)

	// Delete
	rec = env.do(t, http.MethodDelete, "/api/feeds/NEW", "", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.NotContains(t, env.store.Get().Feeds, "NEW")

	rec = env.do(t, http.MethodGet, "/api/feeds/NEW", "", nil)
	assert.Equal(t, http.StatusNotFound, rec.Code)

	// Deleting an unknown feed is a 404
	rec = env.do(t, http.MethodDelete, "/api/feeds/NOPE", "", nil)
	assert.Equal(t, http.StatusNotFound, rec.Code)

	// Deleting the last feed fails validation and leaves the config intact
	rec = env.do(t, http.MethodDelete, "/api/feeds/A", "", nil)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, env.store.Get().Feeds, "A")
}

func TestAdminPutFeedWithNestedTables(t *testing.T) {
	env := setupServer(t, "")

	// Nested objects become [feeds.X.filters] style sub-tables
	rec := env.do(t, http.MethodPut, "/api/feeds/NESTED",
		`{"url": "https://youtube.com/channel/n", "filters": {"not_title": "(?i)live"}}`, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	nested := env.store.Get().Feeds["NESTED"]
	require.NotNil(t, nested)
	assert.Equal(t, "(?i)live", nested.Filters.NotTitle)

	// Replacing the feed removes the nested table too (no orphaned sub-tables)
	rec = env.do(t, http.MethodPut, "/api/feeds/NESTED", `{"url": "https://youtube.com/channel/n"}`, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Empty(t, env.store.Get().Feeds["NESTED"].Filters.NotTitle)

	data, err := os.ReadFile(env.path)
	require.NoError(t, err)
	assert.NotContains(t, string(data), "not_title")
}

func TestAdminReload(t *testing.T) {
	env := setupServer(t, "")

	rec := env.do(t, http.MethodPost, "/api/reload", "", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.True(t, *env.reloaded, "reload trigger must be invoked")
}
