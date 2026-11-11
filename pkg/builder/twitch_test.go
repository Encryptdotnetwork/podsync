package builder

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/nicklaw5/helix"
	"github.com/stretchr/testify/require"

	"github.com/mxpv/podsync/pkg/feed"
	"github.com/mxpv/podsync/pkg/model"
)

func TestParseURL_TwitchUser(t *testing.T) {
	info, err := ParseURL("https://www.twitch.tv/samueletienne")
	require.NoError(t, err)
	require.Equal(t, model.TypeUser, info.LinkType)
	require.Equal(t, model.ProviderTwitch, info.Provider)
	require.Equal(t, "samueletienne", info.ItemID)

	info, err = ParseURL("https://twitch.tv/testuser")
	require.NoError(t, err)
	require.Equal(t, model.TypeUser, info.LinkType)
	require.Equal(t, model.ProviderTwitch, info.Provider)
	require.Equal(t, "testuser", info.ItemID)
}

func TestParseURL_TwitchInvalidLink(t *testing.T) {
	_, err := ParseURL("https://www.twitch.tv/")
	require.Error(t, err)
	require.Contains(t, err.Error(), "invalid id")

	_, err = ParseURL("https://www.twitch.tv//")
	require.Error(t, err)
	require.Contains(t, err.Error(), "invald twitch user path")

	_, err = ParseURL("https://www.twitch.tv/user/extra/path")
	require.Error(t, err)
	require.Contains(t, err.Error(), "invald twitch user path")
}

func TestNewTwitchBuilder_InvalidKey(t *testing.T) {
	_, err := NewTwitchBuilder("invalid_key")
	require.Error(t, err)
	require.Contains(t, err.Error(), "invalid twitch key")

	_, err = NewTwitchBuilder("only_one_part")
	require.Error(t, err)
	require.Contains(t, err.Error(), "invalid twitch key")

	_, err = NewTwitchBuilder("")
	require.Error(t, err)
	require.Contains(t, err.Error(), "invalid twitch key")
}

// fakeTwitchHTTP is an in-process helix.HTTPClient: no sockets are opened.
// Routes are keyed by request path; a nil response with an error simulates a
// transport failure.
type fakeTwitchHTTP struct {
	routes map[string]fakeTwitchReply
}

type fakeTwitchReply struct {
	status int
	body   string
	err    error
}

func (f fakeTwitchHTTP) Do(req *http.Request) (*http.Response, error) {
	reply, ok := f.routes[req.URL.Path]
	if !ok {
		return nil, errors.New("unexpected request: " + req.URL.Path)
	}
	if reply.err != nil {
		return nil, reply.err
	}

	return &http.Response{
		StatusCode: reply.status,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(reply.body)),
		Request:    req,
	}, nil
}

func newFakeTwitchBuilder(t *testing.T, routes map[string]fakeTwitchReply) *TwitchBuilder {
	t.Helper()

	client, err := helix.NewClient(&helix.Options{
		ClientID:   "test-client",
		HTTPClient: fakeTwitchHTTP{routes: routes},
	})
	require.NoError(t, err)

	return &TwitchBuilder{client: client}
}

func buildTwitchFeed(t *testing.T, b *TwitchBuilder) (*model.Feed, error) {
	t.Helper()

	var (
		result *model.Feed
		err    error
	)
	require.NotPanics(t, func() {
		result, err = b.Build(context.Background(), &feed.Config{
			URL:      "https://www.twitch.tv/testuser",
			PageSize: 10,
		})
	})
	return result, err
}

const twitchOneUser = `{"data":[{"id":"42","login":"testuser","display_name":"TestUser"}]}`

func TestTwitchBuild_UnknownUser(t *testing.T) {
	b := newFakeTwitchBuilder(t, map[string]fakeTwitchReply{
		"/helix/users": {status: 200, body: `{"data":[]}`},
	})

	_, err := buildTwitchFeed(t, b)
	require.Error(t, err)
	require.True(t, errors.Is(err, model.ErrNotFound), "got: %v", err)
	require.Contains(t, err.Error(), "testuser")
}

func TestTwitchBuild_UserLookupAPIError(t *testing.T) {
	b := newFakeTwitchBuilder(t, map[string]fakeTwitchReply{
		"/helix/users": {status: 401, body: `{"error":"Unauthorized","status":401,"message":"Invalid OAuth token"}`},
	})

	_, err := buildTwitchFeed(t, b)
	require.Error(t, err)
	require.False(t, errors.Is(err, model.ErrNotFound), "an API failure is not a missing user")
	require.Contains(t, err.Error(), "401")
	require.Contains(t, err.Error(), "Invalid OAuth token")
}

func TestTwitchBuild_UserLookupServerError(t *testing.T) {
	b := newFakeTwitchBuilder(t, map[string]fakeTwitchReply{
		"/helix/users": {status: 503, body: ""},
	})

	_, err := buildTwitchFeed(t, b)
	require.Error(t, err)
	require.False(t, errors.Is(err, model.ErrNotFound))
	require.Contains(t, err.Error(), "503")
}

func TestTwitchBuild_StreamsLookupFailure(t *testing.T) {
	tests := map[string]fakeTwitchReply{
		"transport error": {err: errors.New("connection refused")},
		"api error":       {status: 500, body: ""},
		"auth error":      {status: 401, body: `{"error":"Unauthorized","status":401,"message":"Invalid OAuth token"}`},
	}

	for name, streams := range tests {
		t.Run(name, func(t *testing.T) {
			b := newFakeTwitchBuilder(t, map[string]fakeTwitchReply{
				"/helix/users":   {status: 200, body: twitchOneUser},
				"/helix/streams": streams,
				// A failed streams lookup must stop the build before any archive is listed
				"/helix/videos": {err: errors.New("videos must not be requested")},
			})

			result, err := buildTwitchFeed(t, b)
			require.Error(t, err)
			require.Nil(t, result)
			require.Contains(t, err.Error(), "failed to get streams for user")
			require.NotContains(t, err.Error(), "videos must not be requested")
		})
	}
}

func TestTwitchBuild_Success(t *testing.T) {
	b := newFakeTwitchBuilder(t, map[string]fakeTwitchReply{
		"/helix/users":   {status: 200, body: twitchOneUser},
		"/helix/streams": {status: 200, body: `{"data":[]}`},
		"/helix/videos":  {status: 200, body: `{"data":[]}`},
	})

	result, err := buildTwitchFeed(t, b)
	require.NoError(t, err)
	require.Equal(t, "TestUser", result.Title)
	require.Empty(t, result.Episodes)
}
