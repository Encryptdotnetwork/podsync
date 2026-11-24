package builder

import (
	"context"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mxpv/podsync/pkg/feed"
	"github.com/mxpv/podsync/pkg/model"
)

func fileURL(path string) string {
	slash := filepath.ToSlash(path)
	if len(slash) > 0 && slash[0] != '/' {
		slash = "/" + slash
	}
	return (&url.URL{Scheme: "file", Path: slash}).String()
}

func TestParseURL_LocalJSON(t *testing.T) {
	info, err := ParseURL("file:///app/scout/AutoPodcasts.json")
	require.NoError(t, err)
	assert.Equal(t, model.ProviderLocalJSON, info.Provider)
	assert.Equal(t, model.TypePlaylist, info.LinkType)
	assert.Equal(t, filepath.Clean(filepath.FromSlash("/app/scout/AutoPodcasts.json")), info.ItemID)

	_, err = ParseURL("file:///app/scout/../secret.json")
	require.Error(t, err)

	_, err = ParseURL("file:///app/scout/not-json.txt")
	require.Error(t, err)

	_, err = ParseURL("file://remote-host/share/feed.json")
	require.Error(t, err)
}

func TestLocalJSONBuilder_SecurityValidation(t *testing.T) {
	dir := t.TempDir()
	feedFile := filepath.Join(dir, "security.json")

	payload := `{
		"title": "Security Feed",
		"link": "javascript:alert(1)",
		"cover_art": "file:///etc/passwd",
		"episodes": [
			{
				"id": "../escape",
				"title": "Bad traversal ID",
				"video_url": "https://www.youtube.com/watch?v=abc",
				"duration": 1200,
				"pub_date": "2026-11-24T01:00:00Z"
			},
			{
				"id": "a/b",
				"title": "Bad slash ID",
				"video_url": "https://www.youtube.com/watch?v=def",
				"duration": 1200,
				"pub_date": "2026-11-24T01:00:00Z"
			},
			{
				"id": "ssrf_loopback",
				"title": "Loopback SSRF",
				"video_url": "http://127.0.0.1:8089/admin/config",
				"duration": 1200,
				"pub_date": "2026-11-24T01:00:00Z"
			},
			{
				"id": "ssrf_private",
				"title": "Private IP SSRF",
				"video_url": "http://10.0.0.10:11434/api/tags",
				"duration": 1200,
				"pub_date": "2026-11-24T01:00:00Z"
			},
			{
				"id": "ssrf_localhost",
				"title": "Localhost SSRF",
				"video_url": "http://localhost:8080/secret",
				"duration": 1200,
				"pub_date": "2026-11-24T01:00:00Z"
			},
			{
				"id": "good_ep1",
				"title": "Valid Episode",
				"video_url": "https://www.youtube.com/watch?v=good_ep1",
				"thumbnail": "javascript:alert(1)",
				"duration": 1800,
				"pub_date": "not-a-date"
			}
		]
	}`
	require.NoError(t, os.WriteFile(feedFile, []byte(payload), 0o644))

	b, err := NewLocalJSONBuilder()
	require.NoError(t, err)

	got, err := b.Build(context.Background(), &feed.Config{
		ID:       "AutoPodcasts",
		URL:      fileURL(feedFile),
		Format:   model.FormatAudio,
		Quality:  model.QualityHigh,
		PageSize: 10,
	})
	require.NoError(t, err)
	assert.Empty(t, got.ItemURL, "non-http feed link must be stripped")
	assert.Empty(t, got.CoverArt, "non-http cover_art must be stripped")
	require.Len(t, got.Episodes, 1)
	assert.Equal(t, "good_ep1", got.Episodes[0].ID)
	assert.Empty(t, got.Episodes[0].Thumbnail, "non-http thumbnail must be stripped")
	assert.False(t, got.Episodes[0].PubDate.IsZero(), "bad pub_date should fall back to current time")

	// Reject directory named *.json (non-regular file).
	fakeDir := filepath.Join(dir, "dir.json")
	require.NoError(t, os.Mkdir(fakeDir, 0o755))
	_, err = b.Build(context.Background(), &feed.Config{
		ID:       "AutoPodcasts",
		URL:      fileURL(fakeDir),
		Format:   model.FormatAudio,
		Quality:  model.QualityHigh,
		PageSize: 10,
	})
	require.Error(t, err)
}

func TestLocalJSONBuilder_Build(t *testing.T) {
	dir := t.TempDir()
	feedFile := filepath.Join(dir, "AutoPodcasts.json")

	payload := `{
		"title": "Auto Podcasts",
		"description": "Curated full interviews",
		"author": "PodSync Scout",
		"link": "https://podsync.example.com/scout/",
		"cover_art": "https://i.ytimg.com/vi/abc123/hqdefault.jpg",
		"episodes": [
			{
				"id": "vid1",
				"title": "Guest Alpha Full Interview",
				"description": "Deep dive discussion",
				"video_url": "https://www.youtube.com/watch?v=vid1",
				"thumbnail": "https://i.ytimg.com/vi/vid1/hqdefault.jpg",
				"duration": 1800,
				"pub_date": "2026-11-24T01:00:00Z"
			},
			{
				"id": "bad_scheme",
				"title": "Should be skipped",
				"video_url": "file:///etc/passwd",
				"duration": 1200,
				"pub_date": "2026-11-23T01:00:00Z"
			},
			{
				"id": "vid1",
				"title": "Duplicate should be skipped",
				"video_url": "https://www.youtube.com/watch?v=vid1",
				"duration": 1800,
				"pub_date": "2026-11-24T01:00:00Z"
			},
			{
				"id": "vid2",
				"title": "Guest Beta Full Interview",
				"description": "Long-form episode",
				"video_url": "https://www.youtube.com/watch?v=vid2",
				"thumbnail": "https://i.ytimg.com/vi/vid2/hqdefault.jpg",
				"duration": 3600,
				"pub_date": "2026-11-22T12:00:00Z"
			}
		]
	}`
	require.NoError(t, os.WriteFile(feedFile, []byte(payload), 0o644))

	b, err := NewLocalJSONBuilder()
	require.NoError(t, err)

	cfg := &feed.Config{
		ID:       "AutoPodcasts",
		URL:      fileURL(feedFile),
		Format:   model.FormatAudio,
		Quality:  model.QualityHigh,
		PageSize: 10,
	}

	got, err := b.Build(context.Background(), cfg)
	require.NoError(t, err)
	assert.Equal(t, "Auto Podcasts", got.Title)
	assert.Equal(t, "Curated full interviews", got.Description)
	assert.Equal(t, "PodSync Scout", got.Author)
	assert.Equal(t, model.ProviderLocalJSON, got.Provider)
	assert.Equal(t, time.Date(2026, 11, 24, 1, 0, 0, 0, time.UTC), got.PubDate)

	require.Len(t, got.Episodes, 2)
	assert.Equal(t, "vid1", got.Episodes[0].ID)
	assert.Equal(t, int64(1800), got.Episodes[0].Duration)
	assert.Equal(t, int64(1800*highAudioBytesPerSecond), got.Episodes[0].Size)
	assert.Equal(t, "vid2", got.Episodes[1].ID)
	assert.Equal(t, int64(3600), got.Episodes[1].Duration)
}

func TestLocalJSONBuilder_EmptyFeed(t *testing.T) {
	dir := t.TempDir()
	feedFile := filepath.Join(dir, "empty.json")
	require.NoError(t, os.WriteFile(feedFile, []byte(`{"title":"Auto Podcasts","episodes":[]}`), 0o644))

	b, err := NewLocalJSONBuilder()
	require.NoError(t, err)

	got, err := b.Build(context.Background(), &feed.Config{
		ID:       "AutoPodcasts",
		URL:      fileURL(feedFile),
		Format:   model.FormatAudio,
		Quality:  model.QualityHigh,
		PageSize: 10,
	})
	require.NoError(t, err)
	assert.Equal(t, "Auto Podcasts", got.Title)
	assert.Empty(t, got.Episodes)
}
