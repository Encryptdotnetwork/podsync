package admin

import (
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mxpv/podsync/pkg/config"
	"github.com/mxpv/podsync/services/update"
)

const testToken = "test-token-0123456789abcdef-fake"

var bearer = map[string]string{"Authorization": "Bearer " + testToken}

// fakeSyncer records calls and returns canned answers.
type fakeSyncer struct {
	mu       sync.Mutex
	calls    []string
	syncErr  error
	allErr   error
	allReply update.SyncAllResult
}

func (f *fakeSyncer) SyncNow(feedID string) (update.FeedStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.calls = append(f.calls, feedID)
	if f.syncErr != nil {
		return update.FeedStatus{ID: feedID}, f.syncErr
	}
	return update.FeedStatus{ID: feedID, State: update.StateQueued, Trigger: update.TriggerManual}, nil
}

func (f *fakeSyncer) SyncAll() (update.SyncAllResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.calls = append(f.calls, "*")
	return f.allReply, f.allErr
}

func (f *fakeSyncer) Status() update.SyncStatus {
	return update.SyncStatus{Busy: true, Feeds: []update.FeedStatus{
		{ID: "A", State: update.StateSyncing, Discovered: 2},
	}}
}

func (f *fakeSyncer) callList() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

// concretePath turns a route pattern into a request: it drops the method and
// fills in the wildcards.
func concretePath(pattern string) (method, path string) {
	method, path, _ = strings.Cut(pattern, " ")
	path = strings.NewReplacer("{id}", "A", "{section}", "server", "{$}", "").Replace(path)
	return method, path
}

// Acceptance 9: every registered API route rejects a missing or wrong token
// with 401, before doing anything else.
func TestEveryAPIRouteRequiresAuth(t *testing.T) {
	syncer := &fakeSyncer{}
	env := setupServerWith(t, config.Admin{Enabled: true, Token: testToken, AllowConfigEdit: true}, syncer)

	checked := 0
	for _, pattern := range env.srv.Routes() {
		method, path := concretePath(pattern)
		if !strings.HasPrefix(path, "/admin/") || isStaticRoute(path) {
			continue
		}
		checked++

		for name, header := range map[string]map[string]string{
			"no token":     nil,
			"wrong token":  {"Authorization": "Bearer wrong-token"},
			"wrong scheme": {"Authorization": "Basic " + testToken},
			"empty bearer": {"Authorization": "Bearer "},
		} {
			rec := env.do(t, method, path, "{}", header)
			assert.Equal(t, http.StatusUnauthorized, rec.Code, "%s %s (%s)", method, path, name)
		}
	}

	assert.GreaterOrEqual(t, checked, 11, "all API routes must be covered")
	assert.Empty(t, syncer.callList(), "nothing may reach the scheduler without a valid token")
}

// isStaticRoute reports whether path is one of the unauthenticated page assets.
func isStaticRoute(path string) bool {
	return path == "/admin/" || path == "/admin/admin.js" || path == "/admin/admin.css"
}

func TestSyncFeedAccepted(t *testing.T) {
	syncer := &fakeSyncer{}
	env := setupServerWith(t, config.Admin{Enabled: true, Token: testToken}, syncer)

	rec := env.do(t, http.MethodPost, "/admin/feeds/A/sync", "", bearer)
	require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())

	var status update.FeedStatus
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &status))
	assert.Equal(t, "A", status.ID)
	assert.Equal(t, update.StateQueued, status.State)
	assert.Equal(t, []string{"A"}, syncer.callList())
	assert.Equal(t, "no-store", rec.Header().Get("Cache-Control"))
}

func TestSyncFeedUnknownIs404(t *testing.T) {
	syncer := &fakeSyncer{syncErr: update.ErrUnknownFeed}
	env := setupServerWith(t, config.Admin{Enabled: true, Token: testToken}, syncer)

	rec := env.do(t, http.MethodPost, "/admin/feeds/NOPE/sync", "", bearer)
	assert.Equal(t, http.StatusNotFound, rec.Code)
}

func TestSyncFeedRefusedIs429WithRetryAfter(t *testing.T) {
	until := time.Now().Add(90 * time.Second)
	syncer := &fakeSyncer{syncErr: &update.RefusedError{Reason: update.ReasonRateLimited, RetryAfter: until}}
	env := setupServerWith(t, config.Admin{Enabled: true, Token: testToken}, syncer)

	rec := env.do(t, http.MethodPost, "/admin/feeds/A/sync", "", bearer)
	require.Equal(t, http.StatusTooManyRequests, rec.Code)

	retry := rec.Header().Get("Retry-After")
	require.NotEmpty(t, retry)
	assert.Regexp(t, `^(89|90|91)$`, retry)

	var body syncRefusal
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.Equal(t, update.ReasonRateLimited, body.Reason)
	assert.WithinDuration(t, until, body.RetryAfter, time.Second)
}

// An error from the scheduler is logged but never echoed to the client.
func TestSyncErrorsNeverLeakDetails(t *testing.T) {
	leaky := errors.New("provider said no: https://example.test/api?key=SECRET")
	syncer := &fakeSyncer{syncErr: leaky, allErr: leaky}
	env := setupServerWith(t, config.Admin{Enabled: true, Token: testToken}, syncer)

	for _, target := range []struct{ method, path string }{
		{http.MethodPost, "/admin/feeds/A/sync"},
		{http.MethodPost, "/admin/sync"},
	} {
		rec := env.do(t, target.method, target.path, "", bearer)
		assert.Equal(t, http.StatusInternalServerError, rec.Code, target.path)
		assert.NotContains(t, rec.Body.String(), "SECRET", target.path)
		assert.NotContains(t, rec.Body.String(), "key=", target.path)
		for name, values := range rec.Header() {
			assert.NotContains(t, strings.Join(values, ","), "SECRET", name)
		}
	}
}

func TestSyncAll(t *testing.T) {
	t.Run("queued", func(t *testing.T) {
		syncer := &fakeSyncer{allReply: update.SyncAllResult{Queued: []string{"A", "B"}}}
		env := setupServerWith(t, config.Admin{Enabled: true, Token: testToken}, syncer)

		rec := env.do(t, http.MethodPost, "/admin/sync", "", bearer)
		require.Equal(t, http.StatusAccepted, rec.Code)
		assert.JSONEq(t, `{"queued":["A","B"],"refused":[]}`, rec.Body.String())
	})

	t.Run("partially refused still accepted", func(t *testing.T) {
		syncer := &fakeSyncer{allReply: update.SyncAllResult{
			Queued:  []string{"A"},
			Refused: []update.RefusedFeed{{ID: "B", Reason: update.ReasonTooSoon, RetryAfter: time.Now().Add(time.Minute)}},
		}}
		env := setupServerWith(t, config.Admin{Enabled: true, Token: testToken}, syncer)

		rec := env.do(t, http.MethodPost, "/admin/sync", "", bearer)
		require.Equal(t, http.StatusAccepted, rec.Code)
		assert.Contains(t, rec.Body.String(), `"reason":"too_soon"`)
	})

	t.Run("everything refused", func(t *testing.T) {
		syncer := &fakeSyncer{allReply: update.SyncAllResult{
			Refused: []update.RefusedFeed{
				{ID: "A", Reason: update.ReasonRateLimited, RetryAfter: time.Now().Add(10 * time.Minute)},
				{ID: "B", Reason: update.ReasonTooSoon, RetryAfter: time.Now().Add(20 * time.Second)},
			},
		}}
		env := setupServerWith(t, config.Admin{Enabled: true, Token: testToken}, syncer)

		rec := env.do(t, http.MethodPost, "/admin/sync", "", bearer)
		require.Equal(t, http.StatusTooManyRequests, rec.Code)
		assert.Regexp(t, `^(19|20|21)$`, rec.Header().Get("Retry-After"), "earliest retry time wins")
	})
}

func TestSyncStatus(t *testing.T) {
	env := setupServerWith(t, config.Admin{Enabled: true, Token: testToken}, &fakeSyncer{})

	rec := env.do(t, http.MethodGet, "/admin/sync", "", bearer)
	require.Equal(t, http.StatusOK, rec.Code)

	var status update.SyncStatus
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &status))
	assert.True(t, status.Busy)
	require.Len(t, status.Feeds, 1)
	assert.Equal(t, update.StateSyncing, status.Feeds[0].State)
}

func TestSyncRoutesWithoutSchedulerAre503(t *testing.T) {
	env := setupServerWith(t, config.Admin{Enabled: true, Token: testToken}, nil)

	for _, target := range []struct{ method, path string }{
		{http.MethodPost, "/admin/feeds/A/sync"},
		{http.MethodPost, "/admin/sync"},
		{http.MethodGet, "/admin/sync"},
	} {
		rec := env.do(t, target.method, target.path, "", bearer)
		assert.Equal(t, http.StatusServiceUnavailable, rec.Code, target.path)
	}
}

// Acceptance 9: config writes are refused with 403 unless admin.allow_config_edit
// is set; reads, reload and sync keep working, and unauthenticated callers
// still get 401 first.
func TestConfigWritesForbiddenByDefault(t *testing.T) {
	syncer := &fakeSyncer{}
	env := setupServerWith(t, config.Admin{Enabled: true, Token: testToken}, syncer)

	before, err := os.ReadFile(env.path)
	require.NoError(t, err)

	for _, target := range []struct{ method, path, body string }{
		{http.MethodPut, "/admin/config/tokens", `{"youtube": "replacement_key_9999"}`},
		{http.MethodPut, "/admin/feeds/A", `{"url": "https://youtube.com/channel/x"}`},
		{http.MethodPut, "/admin/feeds/NEW", `{"url": "https://youtube.com/channel/x"}`},
		{http.MethodDelete, "/admin/feeds/A", ""},
	} {
		rec := env.do(t, target.method, target.path, target.body, bearer)
		assert.Equal(t, http.StatusForbidden, rec.Code, "%s %s", target.method, target.path)

		rec = env.do(t, target.method, target.path, target.body, nil)
		assert.Equal(t, http.StatusUnauthorized, rec.Code, "%s %s without a token", target.method, target.path)
	}

	after, err := os.ReadFile(env.path)
	require.NoError(t, err)
	assert.Equal(t, before, after, "config.toml must be untouched")

	assert.Equal(t, http.StatusOK, env.do(t, http.MethodGet, "/admin/config", "", bearer).Code)
	assert.Equal(t, http.StatusOK, env.do(t, http.MethodGet, "/admin/feeds", "", bearer).Code)
	assert.Equal(t, http.StatusOK, env.do(t, http.MethodPost, "/admin/reload", "", bearer).Code)
	assert.Equal(t, http.StatusAccepted, env.do(t, http.MethodPost, "/admin/feeds/A/sync", "", bearer).Code)
}

func TestConfigWritesAllowedWhenEnabled(t *testing.T) {
	env := setupServerWith(t, config.Admin{Enabled: true, Token: testToken, AllowConfigEdit: true}, nil)

	rec := env.do(t, http.MethodPut, "/admin/feeds/NEW", `{"url": "https://youtube.com/channel/new"}`, bearer)
	assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
}

// Acceptance 10: none of the admin paths (API routes and page assets, with
// sample IDs) matches the public reverse-proxy router's file patterns, so a
// public router can never be tricked into exposing them as media or feeds.
func TestAdminRoutesAvoidPublicRouterPatterns(t *testing.T) {
	public := regexp.MustCompile(`^/[^/]+\.(xml|opml|jpe?g|png|webp)$|^/[^/]+/[^/]+\.(mp3|m4a|mp4|aac|ogg|opus|webm)$`)

	env := setupServerWith(t, config.Admin{Enabled: true, Token: testToken}, &fakeSyncer{})

	routes := env.srv.Routes()
	require.GreaterOrEqual(t, len(routes), 15)

	for _, pattern := range routes {
		_, path := concretePath(pattern)
		for _, id := range []string{"ID6", "feed.xml", "x.mp3", "UC123"} {
			sample := strings.ReplaceAll(strings.ReplaceAll(pattern, "{id}", id), "{section}", "server")
			_, sample = concretePath(strings.ReplaceAll(sample, "{$}", ""))
			assert.False(t, public.MatchString(sample), "%s matches the public router", sample)
		}
		assert.False(t, public.MatchString(path), "%s matches the public router", path)
	}

	// The page assets are routes too
	for _, asset := range []string{"/admin/", "/admin/admin.js", "/admin/admin.css"} {
		assert.False(t, public.MatchString(asset), asset)
	}
}
