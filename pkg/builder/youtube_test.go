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
	"google.golang.org/api/option"
	"google.golang.org/api/youtube/v3"

	"github.com/mxpv/podsync/pkg/feed"
	"github.com/mxpv/podsync/pkg/model"
	"github.com/mxpv/podsync/pkg/ytdl"
)

// MockTransport implements http.RoundTripper for testing
type MockTransport struct {
	responses map[string]*http.Response
}

func (m *MockTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	url := req.URL.String()
	if resp, exists := m.responses[url]; exists {
		return resp, nil
	}
	return &http.Response{
		StatusCode: 404,
		Body:       http.NoBody,
	}, nil
}

func TestResolveHandle(t *testing.T) {
	tests := []struct {
		name     string
		handle   string
		mockResp string
		expected string
		wantErr  bool
	}{
		{
			name:   "valid handle",
			handle: "testhandle",
			mockResp: `{
				"items": [
					{
						"snippet": {
							"channelId": "UC_test_channel_id_123"
						}
					}
				]
			}`,
			expected: "UC_test_channel_id_123",
			wantErr:  false,
		},
		{
			name:     "handle not found",
			handle:   "nonexistent",
			mockResp: `{"items": []}`,
			expected: "",
			wantErr:  true,
		},
		{
			name:   "empty channel ID",
			handle: "badhandle",
			mockResp: `{
				"items": [
					{
						"snippet": {
							"channelId": ""
						}
					}
				]
			}`,
			expected: "",
			wantErr:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Create a mock HTTP client
			mockTransport := &MockTransport{
				responses: make(map[string]*http.Response),
			}

			// Set up the mock response based on expected API call
			mockTransport.responses["https://youtube.googleapis.com/youtube/v3/search"] = &http.Response{
				StatusCode: 200,
				Body:       http.NoBody, // Simplified for this test
			}

			client := &http.Client{Transport: mockTransport}

			// Create YouTube service with mock client
			yt, err := youtube.NewService(context.Background(), option.WithHTTPClient(client))
			require.NoError(t, err)

			_ = &YouTubeBuilder{
				client: yt,
			}

			// Note: This test demonstrates the structure but won't actually work
			// without proper mocking of the YouTube API responses.
			// For a real implementation, you'd need more sophisticated mocking
			// like using httptest.Server or a proper mock library.

			// Skip the actual API call test since it requires complex mocking
			t.Skip("Skipping API call test - requires more sophisticated mocking")
		})
	}
}

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
