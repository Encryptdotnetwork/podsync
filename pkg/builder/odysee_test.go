package builder

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/mxpv/podsync/pkg/feed"
	"github.com/mxpv/podsync/pkg/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseOdyseeURL(t *testing.T) {
	tests := []struct {
		name      string
		url       string
		wantType  model.Type
		wantID    string
		wantError bool
	}{
		{
			name:     "simple channel",
			url:      "https://odysee.com/@theduran:e",
			wantType: model.TypeChannel,
			wantID:   "theduran:e",
		},
		{
			name:     "channel with video",
			url:      "https://odysee.com/@theduran:e/video-name:abc123",
			wantType: model.TypeChannel,
			wantID:   "theduran:e",
		},
		{
			name:      "invalid format",
			url:       "https://odysee.com/invalid",
			wantError: true,
		},
		{
			name:      "empty path",
			url:       "https://odysee.com",
			wantError: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			parsed, err := parseURL(tt.url)
			require.NoError(t, err)

			kind, id, err := parseOdyseeURL(parsed)

			if tt.wantError {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
				assert.Equal(t, tt.wantType, kind)
				assert.Equal(t, tt.wantID, id)
			}
		})
	}
}

func TestParseRSSPubDate(t *testing.T) {
	tests := []struct {
		name      string
		input     string
		wantError bool
	}{
		{
			name:      "RFC1123Z format",
			input:     "Mon, 02 Jan 2006 15:04:05 -0700",
			wantError: false,
		},
		{
			name:      "RFC3339 format",
			input:     "2006-01-02T15:04:05Z",
			wantError: false,
		},
		{
			name:      "invalid format",
			input:     "not a date",
			wantError: true,
		},
		{
			name:      "empty string",
			input:     "",
			wantError: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := parseRSSPubDate(tt.input)

			if tt.wantError {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

func TestParseRSSDuration(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    int64
	}{
		{
			name:  "seconds format",
			input: "300",
			want:  300,
		},
		{
			name:  "HH:MM:SS format",
			input: "00:05:30",
			want:  330,
		},
		{
			name:  "HH:MM:SS with hours",
			input: "01:30:45",
			want:  5445,
		},
		{
			name:  "empty string",
			input: "",
			want:  0,
		},
		{
			name:  "invalid format",
			input: "invalid",
			want:  0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := parseRSSDuration(tt.input)
			assert.Equal(t, tt.want, result)
		})
	}
}

func TestExtractOdyseeVideoID(t *testing.T) {
	tests := []struct {
		name string
		url  string
		want string
	}{
		{
			name: "typical video URL",
			url:  "https://odysee.com/@theduran:e/video-name:abc123",
			want: "video-name:abc123",
		},
		{
			name: "with trailing slash",
			url:  "https://odysee.com/@channel:id/video:id/",
			want: "video:id",
		},
		{
			name: "channel URL",
			url:  "https://odysee.com/@theduran:e",
			want: "@theduran:e",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := extractOdyseeVideoID(tt.url)
			assert.Equal(t, tt.want, result)
		})
	}
}

func TestOdyseeBuilderBuild(t *testing.T) {
	rssContent := `<?xml version="1.0" encoding="UTF-8"?>
<rss version="2.0">
  <channel>
    <title>Test Channel</title>
    <link>https://odysee.com/@testchannel:1</link>
    <description>Test channel description</description>
    <image>
      <url>https://example.com/image.jpg</url>
    </image>
    <item>
      <title>Test Video 1</title>
      <link>https://odysee.com/@testchannel:1/video-one:abc123</link>
      <description>First test video</description>
      <pubDate>Mon, 02 Jan 2006 15:04:05 -0700</pubDate>
      <duration>300</duration>
    </item>
    <item>
      <title>Test Video 2</title>
      <link>https://odysee.com/@testchannel:1/video-two:def456</link>
      <description>Second test video</description>
      <pubDate>Tue, 03 Jan 2006 10:00:00 -0700</pubDate>
      <duration>600</duration>
    </item>
  </channel>
</rss>`

	// Create test server
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/$/rss/@testchannel:1" {
			w.Header().Set("Content-Type", "application/rss+xml")
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(rssContent))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	// Create builder with custom client pointing to test server
	builder := &OdyseeBuilder{
		client: &http.Client{Timeout: 10 * time.Second},
	}

	// We need to test with a modified URL, but ParseURL expects odysee.com
	// For now, we just verify the RSS parsing works
	t.Run("parse RSS feed", func(t *testing.T) {
		rssURL := server.URL + "/$/rss/@testchannel:1"
		rssFeed, err := builder.fetchRSSFeed(nil, rssURL)
		require.NoError(t, err)
		assert.Equal(t, "Test Channel", rssFeed.Channel.Title)
		assert.Len(t, rssFeed.Channel.Items, 2)
		assert.Equal(t, "Test Video 1", rssFeed.Channel.Items[0].Title)
	})
}

func TestNewOdyseeBuilder(t *testing.T) {
	builder, err := NewOdyseeBuilder()
	require.NoError(t, err)
	assert.NotNil(t, builder)
	assert.NotNil(t, builder.client)
}

func TestOdyseeURLIntegration(t *testing.T) {
	// Test the full URL parsing pipeline
	info, err := ParseURL("https://odysee.com/@theduran:e")
	require.NoError(t, err)

	assert.Equal(t, model.ProviderOdysee, info.Provider)
	assert.Equal(t, model.TypeChannel, info.LinkType)
	assert.Equal(t, "theduran:e", info.ItemID)
}
