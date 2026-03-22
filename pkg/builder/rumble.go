package builder

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"

	"github.com/pkg/errors"
	log "github.com/sirupsen/logrus"

	"github.com/mxpv/podsync/pkg/feed"
	"github.com/mxpv/podsync/pkg/model"
	"github.com/mxpv/podsync/pkg/ytdl"
)

type RumbleBuilder struct {
	client     *http.Client
	downloader Downloader
}

func NewRumbleBuilder(downloader Downloader) (*RumbleBuilder, error) {
	return &RumbleBuilder{
		client:     &http.Client{Timeout: 30 * time.Second},
		downloader: downloader,
	}, nil
}

func (rb *RumbleBuilder) Build(ctx context.Context, cfg *feed.Config) (*model.Feed, error) {
	info, err := ParseURL(cfg.URL)
	if err != nil {
		return nil, err
	}

	if info.Provider != model.ProviderRumble {
		return nil, errors.New("invalid URL provider for Rumble builder")
	}

	// Construct Rumble URL for yt-dlp
	var rumbleURL string
	switch info.LinkType {
	case model.TypeChannel:
		// Handle both channel name and c-ID formats
		if strings.HasPrefix(info.ItemID, "c-") {
			rumbleURL = fmt.Sprintf("https://rumble.com/c/%s", info.ItemID)
		} else {
			rumbleURL = fmt.Sprintf("https://rumble.com/c/%s", info.ItemID)
		}
	case model.TypePlaylist:
		// Single video
		rumbleURL = fmt.Sprintf("https://rumble.com/%s", info.ItemID)
	default:
		return nil, errors.New("unsupported Rumble link type")
	}

	_feed := &model.Feed{
		ItemID:          info.ItemID,
		Provider:        info.Provider,
		LinkType:        info.LinkType,
		Format:          cfg.Format,
		Quality:         cfg.Quality,
		CoverArtQuality: cfg.Custom.CoverArtQuality,
		PageSize:        cfg.PageSize,
		PrivateFeed:     cfg.PrivateFeed,
		UpdatedAt:       time.Now().UTC(),
	}

	if _feed.PageSize == 0 {
		_feed.PageSize = 50
	}

	// Get playlist metadata using yt-dlp
	metadata, err := rb.downloader.PlaylistMetadata(ctx, rumbleURL)
	if err != nil {
		return nil, errors.Wrapf(err, "failed to fetch Rumble playlist metadata from %s", rumbleURL)
	}

	log.Infof("Rumble metadata retrieved: title=%s, entries=%d, channel=%s", metadata.Title, len(metadata.Entries), metadata.Channel)

	// Set feed metadata from yt-dlp output
	_feed.Title = metadata.Title
	if _feed.Title == "" {
		_feed.Title = metadata.Channel
	}

	_feed.Description = metadata.Description
	if _feed.Description == "" {
		_feed.Description = fmt.Sprintf("Rumble channel: %s", metadata.Channel)
	}

	_feed.Author = metadata.Channel
	_feed.ItemURL = metadata.ChannelUrl
	if _feed.ItemURL == "" {
		_feed.ItemURL = rumbleURL
	}

	// Set cover art from thumbnails
	if len(metadata.Thumbnails) > 0 {
		// Use the highest quality thumbnail (last in the list)
		_feed.CoverArt = metadata.Thumbnails[len(metadata.Thumbnails)-1].Url
	}

	// Parse entries as episodes
	// Note: yt-dlp's flat-playlist mode doesn't populate entries in PlaylistMetadata
	// We need to handle entries parsing if available, otherwise episodes will be queried separately
	if err := rb.parseEpisodes(ctx, cfg, _feed, metadata); err != nil {
		log.WithError(err).Warnf("failed to parse episodes from metadata, continuing with empty episode list")
	}

	return _feed, nil
}

func (rb *RumbleBuilder) parseEpisodes(ctx context.Context, cfg *feed.Config, feedModel *model.Feed, metadata ytdl.PlaylistMetadata) error {
	if len(metadata.Entries) == 0 {
		log.Infof("Rumble feed initialized: %s with 0 episodes (no entries in metadata)", feedModel.Title)
		return nil
	}

	log.Infof("Processing %d Rumble entries into episodes", len(metadata.Entries))

	// Parse yt-dlp entries into episodes
	for i, entry := range metadata.Entries {
		if i >= feedModel.PageSize {
			log.Debugf("Reached page size limit (%d), stopping episode parsing", feedModel.PageSize)
			break
		}

		// For Rumble flat-playlist, we need to extract ID and title from the URL
		// URL format: https://rumble.com/vXXXXXX-title-slug.html?query=params

		// Extract video ID and title from URL
		episodeId, episodeTitle := extractRumbleIdAndTitle(entry.Url)

		if episodeId == "" {
			log.Warnf("Entry %d: unable to extract ID from URL %s", i, entry.Url)
			continue
		}

		if episodeTitle == "" {
			episodeTitle = episodeId
			log.Warnf("Entry %d: unable to extract title from URL, using ID as title", i)
		}

		// Use title as description since flat-playlist has no description
		description := episodeTitle

		// Debug logging: first 3 entries
		if i < 3 {
			log.Infof("Entry %d extracted: id=%q, title=%q, url=%q", i, episodeId, episodeTitle, entry.Url)
		}

		log.Debugf("Processing entry %d: id=%s, title=%s", i, episodeId, episodeTitle)

		// Parse upload date (YYYYMMDD format from yt-dlp)
		var pubDate time.Time
		if entry.UploadDate != "" {
			if t, err := time.Parse("20060102", entry.UploadDate); err == nil {
				pubDate = t
			} else {
				pubDate = time.Now().UTC()
			}
		} else {
			pubDate = time.Now().UTC()
		}

		// Duration in seconds
		duration := int64(entry.Duration)

		// Build video URL - use entry URL directly
		videoURL := entry.Url
		if videoURL == "" {
			videoURL = fmt.Sprintf("https://rumble.com/%s", episodeId)
		}

		episode := &model.Episode{
			ID:          episodeId,
			Title:       episodeTitle,
			Description: description,
			Thumbnail:   entry.Thumbnail,
			Duration:    duration,
			VideoURL:    videoURL,
			PubDate:     pubDate,
			Order:       fmt.Sprintf("%d", i),
			Status:      model.EpisodeNew,
		}

		feedModel.Episodes = append(feedModel.Episodes, episode)
	}

	log.Infof("Rumble feed initialized: %s with %d initial episodes", feedModel.Title, len(feedModel.Episodes))
	return nil
}

// extractRumbleIdAndTitle extracts video ID and title from a Rumble URL
// URL format: https://rumble.com/vXXXXXX-title-slug.html?query=params
// Returns: (videoId, title)
// Example: ("v778v9a", "The Lodge Card Club Raid Is A Witch Hunt...")
func extractRumbleIdAndTitle(rumbleUrl string) (string, string) {
	if rumbleUrl == "" {
		return "", ""
	}

	// Parse the URL to get the path
	parsedUrl, err := url.Parse(rumbleUrl)
	if err != nil {
		log.Debugf("Failed to parse URL: %s, error: %v", rumbleUrl, err)
		return "", ""
	}

	// Get the URL path and remove leading /
	urlPath := strings.TrimPrefix(parsedUrl.Path, "/")

	// Get just the filename (without extension and query params)
	filename := path.Base(urlPath)
	filename = strings.TrimSuffix(filename, ".html")

	// URL format: vXXXXXX-title-slug
	// Split on first dash to separate ID from title
	parts := strings.SplitN(filename, "-", 2)
	if len(parts) < 1 {
		return "", ""
	}

	videoId := parts[0]

	// Validate that we have a video ID starting with 'v'
	if !strings.HasPrefix(videoId, "v") || len(videoId) < 2 {
		return "", ""
	}

	// Extract and clean title from slug
	var title string
	if len(parts) > 1 {
		title = cleanTitleFromSlug(parts[1])
	}

	return videoId, title
}

// cleanTitleFromSlug converts URL slug to a proper title
// "the-lodge-card-club-raid-is-a-witch-hunt" -> "The Lodge Card Club Raid Is A Witch Hunt"
func cleanTitleFromSlug(slug string) string {
	if slug == "" {
		return ""
	}

	// Replace hyphens with spaces
	title := strings.ReplaceAll(slug, "-", " ")

	// Title case each word
	title = strings.Title(title) // nolint:staticcheck

	// Limit length to avoid excessively long titles
	if len(title) > 200 {
		title = title[:200]
		// Remove trailing partial word
		lastSpace := strings.LastIndex(title, " ")
		if lastSpace > 0 && lastSpace > 150 {
			title = title[:lastSpace] + "..."
		}
	}

	return title
}

// extractVideoIdFromRumbleUrl is a helper that extracts just the video ID
// Kept for backward compatibility
func extractVideoIdFromRumbleUrl(rumbleUrl string) string {
	id, _ := extractRumbleIdAndTitle(rumbleUrl)
	return id
}
