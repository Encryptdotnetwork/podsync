package builder

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"regexp"
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

	// User playlists: yt-dlp has no extractor for /playlists/ URLs, so we
	// fetch the page ourselves and hand individual video URLs back to yt-dlp.
	if info.LinkType == model.TypePlaylist && strings.HasPrefix(info.ItemID, "playlists/") {
		return rb.buildFromPlaylist(ctx, cfg, info)
	}

	// Construct Rumble URL for yt-dlp (channels and individual videos)
	var rumbleURL string
	switch info.LinkType {
	case model.TypeChannel:
		rumbleURL = fmt.Sprintf("https://rumble.com/c/%s", info.ItemID)
	case model.TypePlaylist:
		// Individual video (/vXXXXXX)
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

	// Get channel metadata without flat-playlist (for title, description, etc.)
	channelTitle, channelDesc, channelAuthor := rb.fetchChannelMetadata(ctx, rumbleURL)

	// Get playlist entries using flat-playlist
	metadata, err := rb.downloader.PlaylistMetadata(ctx, rumbleURL)
	if err != nil {
		return nil, errors.Wrapf(err, "failed to fetch Rumble playlist metadata from %s", rumbleURL)
	}

	log.Infof("Rumble channel metadata: title=%q, entries=%d", channelTitle, len(metadata.Entries))

	// Set feed title from channel metadata, fallback to channel variable
	_feed.Title = channelTitle
	if _feed.Title == "" {
		_feed.Title = metadata.Channel
	}
	if _feed.Title == "" {
		_feed.Title = "Rumble Channel"
	}

	// Set feed description from channel metadata
	_feed.Description = channelDesc
	if _feed.Description == "" {
		_feed.Description = fmt.Sprintf("Rumble channel: %s", _feed.Title)
	}

	// Set author
	_feed.Author = channelAuthor
	if _feed.Author == "" {
		_feed.Author = metadata.Channel
	}

	// Set URL
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

// rumbleOGTagRegex matches Open Graph meta tags in either attribute order.
var rumbleOGTagRegex = regexp.MustCompile(`<meta[^>]+property="og:([^"]+)"[^>]+content="([^"]*)"`)
var rumbleOGTagRegex2 = regexp.MustCompile(`<meta[^>]+content="([^"]*)"[^>]+property="og:([^"]+)"`)

// rumbleVideoPathRegex matches Rumble video href paths: /vXXXXXX-slug.html
var rumbleVideoPathRegex = regexp.MustCompile(`href="(/v[a-zA-Z0-9]+-[^"]+\.html)"`)

func parseRumbleOGTag(body []byte, name string) string {
	for _, m := range rumbleOGTagRegex.FindAllSubmatch(body, -1) {
		if string(m[1]) == name {
			return string(m[2])
		}
	}
	for _, m := range rumbleOGTagRegex2.FindAllSubmatch(body, -1) {
		if string(m[2]) == name {
			return string(m[1])
		}
	}
	return ""
}

func extractRumbleVideoURLsFromHTML(body []byte) []string {
	matches := rumbleVideoPathRegex.FindAllSubmatch(body, -1)
	seen := make(map[string]bool)
	var urls []string
	for _, match := range matches {
		p := string(match[1])
		if !seen[p] {
			seen[p] = true
			urls = append(urls, "https://rumble.com"+p)
		}
	}
	return urls
}

// buildFromPlaylist fetches a Rumble user playlist page directly (yt-dlp has no
// extractor for /playlists/ URLs) and populates the feed from the HTML.
// Individual video URLs are stored as VideoURL so yt-dlp can download them normally.
func (rb *RumbleBuilder) buildFromPlaylist(ctx context.Context, cfg *feed.Config, info model.Info) (*model.Feed, error) {
	playlistID := strings.TrimPrefix(info.ItemID, "playlists/")
	playlistURL := "https://rumble.com/playlists/" + playlistID

	req, err := http.NewRequestWithContext(ctx, "GET", playlistURL, nil)
	if err != nil {
		return nil, errors.Wrap(err, "failed to create playlist request")
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (compatible; Podsync/2.0)")
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")

	resp, err := rb.client.Do(req)
	if err != nil {
		return nil, errors.Wrap(err, "failed to fetch playlist page")
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, errors.Errorf("playlist page returned status %d", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, errors.Wrap(err, "failed to read playlist page")
	}

	title := parseRumbleOGTag(body, "title")
	title = strings.TrimSuffix(title, " - Rumble")
	if title == "" {
		title = "Rumble Playlist"
	}

	description := parseRumbleOGTag(body, "description")
	if description == "" {
		description = fmt.Sprintf("Rumble playlist: %s", title)
	}

	coverArt := parseRumbleOGTag(body, "image")

	pageSize := cfg.PageSize
	if pageSize == 0 {
		pageSize = 50
	}

	_feed := &model.Feed{
		ItemID:          info.ItemID,
		Provider:        info.Provider,
		LinkType:        info.LinkType,
		Format:          cfg.Format,
		Quality:         cfg.Quality,
		CoverArtQuality: cfg.Custom.CoverArtQuality,
		PageSize:        pageSize,
		PrivateFeed:     cfg.PrivateFeed,
		UpdatedAt:       time.Now().UTC(),
		Title:           title,
		Description:     description,
		CoverArt:        coverArt,
		ItemURL:         playlistURL,
	}

	videoURLs := extractRumbleVideoURLsFromHTML(body)
	log.Infof("Rumble playlist %q: found %d video(s)", title, len(videoURLs))

	for i, videoURL := range videoURLs {
		if i >= pageSize {
			break
		}
		episodeID, episodeTitle := extractRumbleIdAndTitle(videoURL)
		if episodeID == "" {
			log.Warnf("Rumble playlist entry %d: cannot extract ID from %s", i, videoURL)
			continue
		}
		if episodeTitle == "" {
			episodeTitle = episodeID
		}
		_feed.Episodes = append(_feed.Episodes, &model.Episode{
			ID:          episodeID,
			Title:       episodeTitle,
			Description: episodeTitle,
			VideoURL:    videoURL,
			PubDate:     time.Now().UTC(),
			Order:       fmt.Sprintf("%d", i),
			Status:      model.EpisodeNew,
		})
	}

	log.Infof("Rumble playlist feed %q initialised with %d episode(s)", title, len(_feed.Episodes))
	return _feed, nil
}

// fetchChannelMetadata makes a separate yt-dlp call without --flat-playlist
// to extract channel title, description, and author information
func (rb *RumbleBuilder) fetchChannelMetadata(ctx context.Context, rumbleURL string) (title, description, author string) {
	metadata, err := rb.downloader.ChannelMetadata(ctx, rumbleURL)
	if err != nil {
		log.Warnf("failed to fetch Rumble channel metadata: %v", err)
		return
	}

	title = metadata.Title
	if title == "" {
		title = metadata.Channel
	}
	description = metadata.Description
	author = metadata.Channel

	log.Debugf("Rumble channel info: title=%q, description=%q, author=%q", title, description, author)
	return
}
