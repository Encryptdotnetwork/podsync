package builder

import (
	"context"
	"encoding/xml"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/mxpv/podsync/pkg/feed"
	"github.com/pkg/errors"

	"github.com/mxpv/podsync/pkg/model"
)

type OdyseeBuilder struct {
	client *http.Client
}

// RSS structures for parsing Odysee RSS feeds
type rssFeed struct {
	Channel rssChannel `xml:"channel"`
}

type rssChannel struct {
	Title       string    `xml:"title"`
	Link        string    `xml:"link"`
	Description string    `xml:"description"`
	Image       rssImage  `xml:"image"`
	Items       []rssItem `xml:"item"`
}

type rssImage struct {
	URL string `xml:"url"`
}

type rssItem struct {
	Title       string    `xml:"title"`
	Link        string    `xml:"link"`
	Description string    `xml:"description"`
	PubDate     string    `xml:"pubDate"`
	Thumbnail   rssImage  `xml:"image"`
	Duration    string    `xml:"duration"`
	Enclosure   rssEnclosure `xml:"enclosure"`
}

type rssEnclosure struct {
	URL    string `xml:"url,attr"`
	Type   string `xml:"type,attr"`
	Length string `xml:"length,attr"`
}

func (o *OdyseeBuilder) Build(ctx context.Context, cfg *feed.Config) (*model.Feed, error) {
	info, err := ParseURL(cfg.URL)
	if err != nil {
		return nil, errors.Wrap(err, "failed to parse URL")
	}

	if info.Provider != model.ProviderOdysee {
		return nil, errors.New("invalid URL provider for Odysee builder")
	}

	// Construct RSS feed URL from channel ID
	// Example: @theduran:e -> https://odysee.com/$/rss/@theduran:e
	rssURL := fmt.Sprintf("https://odysee.com/$/rss/@%s", info.ItemID)

	// Fetch and parse the RSS feed
	rssFeed, err := o.fetchRSSFeed(ctx, rssURL)
	if err != nil {
		return nil, errors.Wrapf(err, "failed to fetch RSS feed from %s", rssURL)
	}

	feedModel := &model.Feed{
		ItemID:    info.ItemID,
		Provider:  info.Provider,
		LinkType:  info.LinkType,
		Format:    cfg.Format,
		Quality:   cfg.Quality,
		PageSize:  cfg.PageSize,
		UpdatedAt: time.Now().UTC(),
	}

	// Set feed metadata from RSS channel
	feedModel.Title = rssFeed.Channel.Title
	feedModel.Description = rssFeed.Channel.Description
	feedModel.ItemURL = rssFeed.Channel.Link
	feedModel.CoverArt = rssFeed.Channel.Image.URL

	// Parse episodes from RSS items
	var added = 0
	for _, item := range rssFeed.Channel.Items {
		pubDate, err := parseRSSPubDate(item.PubDate)
		if err != nil {
			pubDate = time.Now().UTC()
		}

		// Parse duration if available (format: HH:MM:SS or seconds)
		duration := parseRSSDuration(item.Duration)

		// Extract video URL from the item link
		videoURL := extractOdyseeVideoURL(item.Link)

		episode := &model.Episode{
			ID:          extractOdyseeVideoID(item.Link),
			Title:       item.Title,
			Description: item.Description,
			Duration:    duration,
			PubDate:     pubDate,
			VideoURL:    videoURL,
			Status:      model.EpisodeNew,
		}

		// Try to get thumbnail from enclosure or image
		if item.Thumbnail.URL != "" {
			episode.Thumbnail = item.Thumbnail.URL
		}

		// Rough size estimate based on duration and format
		// Assume ~1MB per minute for video, ~100KB per minute for audio
		if cfg.Format == model.FormatAudio {
			episode.Size = duration * 60 * 100000 / 60 // 100KB per second
		} else {
			episode.Size = duration * 1000000 // ~1MB per second (very rough)
		}

		feedModel.Episodes = append(feedModel.Episodes, episode)

		added++
		if added >= feedModel.PageSize {
			break
		}
	}

	if len(feedModel.Episodes) == 0 {
		return nil, model.ErrNotFound
	}

	return feedModel, nil
}

func (o *OdyseeBuilder) fetchRSSFeed(ctx context.Context, rssURL string) (*rssFeed, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", rssURL, nil)
	if err != nil {
		return nil, errors.Wrap(err, "failed to create request")
	}

	// Set a reasonable User-Agent to avoid being blocked
	req.Header.Set("User-Agent", "Podsync/2.0")

	resp, err := o.client.Do(req)
	if err != nil {
		return nil, errors.Wrap(err, "failed to fetch RSS feed")
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, errors.Errorf("RSS feed returned status %d", resp.StatusCode)
	}

	var rss rssFeed
	if err := xml.NewDecoder(resp.Body).Decode(&rss); err != nil {
		return nil, errors.Wrap(err, "failed to parse RSS feed")
	}

	return &rss, nil
}

func parseRSSPubDate(pubDate string) (time.Time, error) {
	// Try common RSS date formats
	formats := []string{
		time.RFC1123Z,
		time.RFC1123,
		time.RFC3339,
		"Mon, 02 Jan 2006 15:04:05 -0700",
		"Mon, 02 Jan 2006 15:04:05 MST",
	}

	for _, format := range formats {
		if t, err := time.Parse(format, pubDate); err == nil {
			return t, nil
		}
	}

	return time.Time{}, errors.New("unable to parse pubDate")
}

func parseRSSDuration(durationStr string) int64 {
	if durationStr == "" {
		return 0
	}

	// Try parsing as seconds
	var seconds int64
	fmt.Sscanf(strings.TrimSpace(durationStr), "%d", &seconds)
	if seconds > 0 {
		return seconds
	}

	// Try parsing HH:MM:SS format
	parts := strings.Split(durationStr, ":")
	if len(parts) == 3 {
		var h, m, s int64
		fmt.Sscanf(parts[0], "%d", &h)
		fmt.Sscanf(parts[1], "%d", &m)
		fmt.Sscanf(parts[2], "%d", &s)
		return h*3600 + m*60 + s
	}

	return 0
}

func extractOdyseeVideoURL(link string) string {
	// Ensure the video link points to odysee.com
	if strings.Contains(link, "odysee.com") {
		return link
	}

	// If it's a different host, try to reconstruct as an Odysee URL
	// This is a fallback; RSS should provide proper odysee.com links
	return link
}

func extractOdyseeVideoID(link string) string {
	// Extract video ID from Odysee URL
	// Typical format: https://odysee.com/@channel:id/video-name:claimid
	// We want the last segment (video-name:claimid)

	parts := strings.Split(strings.TrimSuffix(link, "/"), "/")
	if len(parts) > 0 {
		return parts[len(parts)-1]
	}

	return link
}

func NewOdyseeBuilder() (*OdyseeBuilder, error) {
	client := &http.Client{
		Timeout: 30 * time.Second,
	}

	return &OdyseeBuilder{client: client}, nil
}
