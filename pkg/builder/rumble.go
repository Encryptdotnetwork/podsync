package builder

import (
	"context"
	"fmt"
	"net/http"
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

		// Debug logging: show first 3 entries in detail
		if i < 3 {
			log.Infof("Entry %d fields: id=%q, title=%q, desc=%q, duration=%d, url=%q, webpage_url=%q, upload_date=%q",
				i, entry.Id, entry.Title, entry.Description, entry.Duration, entry.Url, entry.WebpageUrl, entry.UploadDate)
		}

		// Handle missing title - use ID or URL as fallback
		title := entry.Title
		if title == "" {
			if entry.WebpageUrl != "" {
				// Extract video ID from URL if title is empty
				title = extractVideoIdFromRumbleUrl(entry.WebpageUrl)
			}
			if title == "" {
				title = entry.Id
			}
			log.Warnf("Entry %d missing title, using fallback: %q", i, title)
		}

		// Handle missing description
		description := entry.Description
		if description == "" {
			description = "No description available"
			log.Warnf("Entry %d missing description", i)
		}

		// Handle missing ID
		episodeId := entry.Id
		if episodeId == "" {
			episodeId = extractVideoIdFromRumbleUrl(entry.WebpageUrl)
			log.Warnf("Entry %d missing id, extracted from URL: %q", i, episodeId)
		}

		log.Debugf("Processing entry %d: id=%s, title=%s, duration=%d", i, episodeId, title, entry.Duration)

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

		// Build video URL if not provided
		videoURL := entry.Url
		if videoURL == "" {
			videoURL = entry.WebpageUrl
		}

		episode := &model.Episode{
			ID:          episodeId,
			Title:       title,
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

// extractVideoIdFromRumbleUrl extracts the video ID from a Rumble URL
// Examples:
// https://rumble.com/vXXXXXX-video-title.html -> vXXXXXX
// https://rumble.com/v/vXXXXXX -> vXXXXXX
func extractVideoIdFromRumbleUrl(url string) string {
	if url == "" {
		return ""
	}

	// Parse the path
	parts := strings.Split(url, "/")
	if len(parts) < 2 {
		return ""
	}

	// Get the last part of the path
	lastPart := parts[len(parts)-1]

	// Remove .html extension if present
	lastPart = strings.TrimSuffix(lastPart, ".html")

	// Extract video ID (format: vXXXXXX or vXXXXXX-title)
	if strings.HasPrefix(lastPart, "v") {
		// If it contains a dash, take everything before the dash
		if dashIdx := strings.Index(lastPart, "-"); dashIdx > 0 {
			return lastPart[:dashIdx]
		}
		return lastPart
	}

	return ""
}
