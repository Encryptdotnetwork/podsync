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
		log.Infof("Rumble feed initialized: %s with 0 episodes", feedModel.Title)
		return nil
	}

	// Parse yt-dlp entries into episodes
	for i, entry := range metadata.Entries {
		if i >= feedModel.PageSize {
			break
		}

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
			ID:          entry.Id,
			Title:       entry.Title,
			Description: entry.Description,
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
