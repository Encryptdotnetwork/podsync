package builder

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/mxpv/podsync/pkg/feed"
	"github.com/mxpv/podsync/pkg/model"
	"github.com/mxpv/podsync/pkg/ytdl"
)

func TestParseURLWithHandles(t *testing.T) {
	tests := []struct {
		name     string
		url      string
		expected model.Info
		wantErr  bool
	}{
		{
			name: "valid handle URL",
			url:  "https://www.youtube.com/@testhandle",
			expected: model.Info{
				LinkType: model.TypeHandle,
				Provider: model.ProviderYoutube,
				ItemID:   "testhandle",
			},
			wantErr: false,
		},
		{
			name: "handle URL with videos path",
			url:  "https://youtube.com/@mychannel/videos",
			expected: model.Info{
				LinkType: model.TypeHandle,
				Provider: model.ProviderYoutube,
				ItemID:   "mychannel",
			},
			wantErr: false,
		},
		{
			name:    "invalid handle URL",
			url:     "https://www.youtube.com/@",
			wantErr: true,
		},
		{
			name: "regular channel URL still works",
			url:  "https://www.youtube.com/channel/UC_test_channel",
			expected: model.Info{
				LinkType: model.TypeChannel,
				Provider: model.ProviderYoutube,
				ItemID:   "UC_test_channel",
			},
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := ParseURL(tt.url)

			if tt.wantErr {
				require.Error(t, err)
				return
			}

			require.NoError(t, err)
			require.Equal(t, tt.expected.LinkType, result.LinkType)
			require.Equal(t, tt.expected.Provider, result.Provider)
			require.Equal(t, tt.expected.ItemID, result.ItemID)
		})
	}
}

const testAPIKey = "fake-test-key-0000"

// recordingTransport is a fake http.RoundTripper that records requests and answers them from the handler.
type recordingTransport struct {
	mu       sync.Mutex
	requests []*http.Request
	handler  func(req *http.Request) (*http.Response, error)
}

func (f *recordingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	f.mu.Lock()
	f.requests = append(f.requests, req)
	f.mu.Unlock()
	return f.handler(req)
}

func (f *recordingTransport) recorded() []*http.Request {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]*http.Request(nil), f.requests...)
}

func jsonResponse(req *http.Request, body string) (*http.Response, error) {
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(body)),
		Request:    req,
	}, nil
}

func newTestBuilder(t *testing.T, handler func(req *http.Request) (*http.Response, error), dl Downloader) (*YouTubeBuilder, *recordingTransport) {
	t.Helper()
	fake := &recordingTransport{handler: handler}
	yt, err := newYouTubeBuilder(testAPIKey, fake, dl)
	require.NoError(t, err)
	return yt, fake
}

func TestAPIKeyNotLeakedInTransportError(t *testing.T) {
	yt, fake := newTestBuilder(t, func(req *http.Request) (*http.Response, error) {
		return nil, errors.New("connection refused")
	}, nil)

	_, err := yt.GetVideoCount(context.Background(), &model.Info{LinkType: model.TypeChannel, ItemID: "UC_test"})
	require.Error(t, err)
	require.Len(t, fake.recorded(), 1)

	// The transport error still carries the request URL, but never the key
	require.Contains(t, err.Error(), "youtube/v3/channels")
	require.NotContains(t, err.Error(), testAPIKey)
}

func TestAPIKeySentInHeader(t *testing.T) {
	yt, fake := newTestBuilder(t, func(req *http.Request) (*http.Response, error) {
		return jsonResponse(req, `{"items": [{"id": "UC_test", "statistics": {"videoCount": "42"}}]}`)
	}, nil)

	count, err := yt.GetVideoCount(context.Background(), &model.Info{LinkType: model.TypeChannel, ItemID: "UC_test"})
	require.NoError(t, err)
	require.EqualValues(t, 42, count)

	reqs := fake.recorded()
	require.Len(t, reqs, 1)
	require.Equal(t, testAPIKey, reqs[0].Header.Get("X-Goog-Api-Key"))
	require.False(t, reqs[0].URL.Query().Has("key"))
	require.NotContains(t, reqs[0].URL.String(), testAPIKey)
}

// fakeDownloader is a Downloader that counts PlaylistMetadata calls.
type fakeDownloader struct {
	metadata ytdl.PlaylistMetadata
	err      error
	calls    int
}

func (f *fakeDownloader) PlaylistMetadata(ctx context.Context, url string) (ytdl.PlaylistMetadata, error) {
	f.calls++
	return f.metadata, f.err
}

func (f *fakeDownloader) PlaylistEntries(ctx context.Context, url string, limit int) (ytdl.PlaylistMetadata, error) {
	return ytdl.PlaylistMetadata{}, errors.New("unexpected PlaylistEntries call")
}

// playlistAPIHandler answers the playlist, playlist items and videos endpoints with one video.
func playlistAPIHandler(req *http.Request) (*http.Response, error) {
	switch {
	case strings.HasSuffix(req.URL.Path, "/playlists"):
		return jsonResponse(req, `{"items": [{"id": "PL_test", "snippet": {
			"title": "Playlist", "description": "Desc", "publishedAt": "2020-01-02T03:04:05Z", "channelTitle": "Chan",
			"thumbnails": {"default": {"url": "https://img.example/api-thumb.jpg"}}}}]}`)
	case strings.HasSuffix(req.URL.Path, "/playlistItems"):
		return jsonResponse(req, `{"items": [{"snippet": {"publishedAt": "2020-02-01T00:00:00Z", "position": 0,
			"resourceId": {"videoId": "vid1"}}}]}`)
	case strings.HasSuffix(req.URL.Path, "/videos"):
		return jsonResponse(req, `{"items": [{"id": "vid1", "snippet": {"title": "Video 1", "publishedAt": "2020-01-15T00:00:00Z",
			"thumbnails": {"default": {"url": "https://img.example/vid1.jpg"}}}, "contentDetails": {"duration": "PT1M"}}]}`)
	}

	return nil, errors.New("unexpected request: " + req.URL.String())
}

func TestBuildPlaylistMetadataFailureFallsBack(t *testing.T) {
	dl := &fakeDownloader{err: errors.New("yt-dlp failed")}
	yt, _ := newTestBuilder(t, playlistAPIHandler, dl)

	f, err := yt.Build(context.Background(), &feed.Config{URL: "https://www.youtube.com/playlist?list=PL_test"})
	require.NoError(t, err)
	require.Equal(t, 1, dl.calls)
	require.Equal(t, "https://img.example/api-thumb.jpg", f.CoverArt)
	require.Len(t, f.Episodes, 1)
	require.Equal(t, "vid1", f.Episodes[0].ID)
}

func TestBuildPlaylistMetadataCoverArt(t *testing.T) {
	dl := &fakeDownloader{metadata: ytdl.PlaylistMetadata{Thumbnails: []ytdl.PlaylistMetadataThumbnail{
		{Url: "https://img.example/small.jpg"},
		{Url: "https://img.example/best.jpg"},
	}}}
	yt, _ := newTestBuilder(t, playlistAPIHandler, dl)

	f, err := yt.Build(context.Background(), &feed.Config{URL: "https://www.youtube.com/playlist?list=PL_test"})
	require.NoError(t, err)
	require.Equal(t, "https://img.example/best.jpg", f.CoverArt)
}

func TestBuildPlaylistCustomCoverSkipsMetadata(t *testing.T) {
	dl := &fakeDownloader{err: errors.New("must not be called")}
	yt, _ := newTestBuilder(t, playlistAPIHandler, dl)

	cfg := &feed.Config{
		URL:    "https://www.youtube.com/playlist?list=PL_test",
		Custom: feed.Custom{CoverArt: "https://img.example/custom.jpg"},
	}

	f, err := yt.Build(context.Background(), cfg)
	require.NoError(t, err)
	require.Equal(t, 0, dl.calls)
	require.Len(t, f.Episodes, 1)
}

// handleAPIHandler answers like the API does for an @handle feed, and fails on anything unexpected (e.g. search).
func handleAPIHandler(req *http.Request) (*http.Response, error) {
	if strings.HasSuffix(req.URL.Path, "/channels") {
		return jsonResponse(req, `{"items": [{"id": "UC_test", "snippet": {"title": "Chan", "description": "Desc",
			"publishedAt": "2020-01-02T03:04:05Z", "thumbnails": {"default": {"url": "https://img.example/chan.jpg"}}},
			"contentDetails": {"relatedPlaylists": {"uploads": "UU_test"}}}]}`)
	}

	return playlistAPIHandler(req)
}

func TestHandleLookupUsesForHandle(t *testing.T) {
	yt, fake := newTestBuilder(t, handleAPIHandler, nil)

	f, err := yt.Build(context.Background(), &feed.Config{URL: "https://www.youtube.com/@testhandle"})
	require.NoError(t, err)
	require.Equal(t, "UU_test", f.ItemID)
	require.Equal(t, "@testhandle", f.Author)
	require.Len(t, f.Episodes, 1)

	var channelReqs int
	for _, req := range fake.recorded() {
		require.False(t, strings.HasSuffix(req.URL.Path, "/search"), "unexpected search request: %s", req.URL)

		if strings.HasSuffix(req.URL.Path, "/channels") {
			channelReqs++
			require.Equal(t, "@testhandle", req.URL.Query().Get("forHandle"))
			require.False(t, req.URL.Query().Has("id"))
		}
	}

	require.Equal(t, 1, channelReqs)
}

func TestHandleNotFound(t *testing.T) {
	yt, fake := newTestBuilder(t, func(req *http.Request) (*http.Response, error) {
		return jsonResponse(req, `{"items": []}`)
	}, nil)

	_, err := yt.GetVideoCount(context.Background(), &model.Info{LinkType: model.TypeHandle, ItemID: "nonexistent"})
	require.ErrorIs(t, err, model.ErrNotFound)

	reqs := fake.recorded()
	require.Len(t, reqs, 1)
	require.True(t, strings.HasSuffix(reqs[0].URL.Path, "/channels"))
	require.Equal(t, "@nonexistent", reqs[0].URL.Query().Get("forHandle"))
}

func TestBuildSkipsVideosMissingFromPlaylist(t *testing.T) {
	yt, _ := newTestBuilder(t, func(req *http.Request) (*http.Response, error) {
		if strings.HasSuffix(req.URL.Path, "/videos") {
			// The API also returns a video that is not part of the playlist items
			return jsonResponse(req, `{"items": [
				{"id": "vid_unknown", "snippet": {"title": "Unknown", "publishedAt": "2020-01-10T00:00:00Z",
					"thumbnails": {"default": {"url": "https://img.example/unknown.jpg"}}}, "contentDetails": {"duration": "PT1M"}},
				{"id": "vid1", "snippet": {"title": "Video 1", "publishedAt": "2020-01-15T00:00:00Z",
					"thumbnails": {"default": {"url": "https://img.example/vid1.jpg"}}}, "contentDetails": {"duration": "PT1M"}}]}`)
		}

		return playlistAPIHandler(req)
	}, &fakeDownloader{})

	f, err := yt.Build(context.Background(), &feed.Config{URL: "https://www.youtube.com/playlist?list=PL_test"})
	require.NoError(t, err)
	require.Len(t, f.Episodes, 1)
	require.Equal(t, "vid1", f.Episodes[0].ID)
}
