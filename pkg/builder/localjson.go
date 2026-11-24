package builder

import (
	"context"
	"encoding/json"
	"io"
	"net/netip"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/pkg/errors"
	log "github.com/sirupsen/logrus"

	"github.com/mxpv/podsync/pkg/feed"
	"github.com/mxpv/podsync/pkg/model"
)

// maxLocalJSONBytes caps the local feed JSON file size to prevent excessive memory use.
const maxLocalJSONBytes = 16 << 20 // 16 MiB

// validEpisodeIDRegex enforces safe single-segment episode IDs for BadgerDB keys,
// admin routes and filenames.
var validEpisodeIDRegex = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

type LocalJSONBuilder struct{}

type localJSONFeed struct {
	Title       string             `json:"title"`
	Description string             `json:"description"`
	Author      string             `json:"author"`
	Link        string             `json:"link"`
	CoverArt    string             `json:"cover_art"`
	Episodes    []localJSONEpisode `json:"episodes"`
}

type localJSONEpisode struct {
	ID          string `json:"id"`
	Title       string `json:"title"`
	Description string `json:"description"`
	VideoURL    string `json:"video_url"`
	Thumbnail   string `json:"thumbnail"`
	Duration    int64  `json:"duration"`
	PubDate     string `json:"pub_date"`
}

func NewLocalJSONBuilder() (*LocalJSONBuilder, error) {
	return &LocalJSONBuilder{}, nil
}

func (b *LocalJSONBuilder) Build(ctx context.Context, cfg *feed.Config) (*model.Feed, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	info, err := ParseURL(cfg.URL)
	if err != nil {
		return nil, errors.Wrap(err, "failed to parse local JSON feed URL")
	}

	if info.Provider != model.ProviderLocalJSON {
		return nil, errors.New("invalid URL provider for local JSON builder")
	}

	doc, err := readLocalJSONFeed(info.ItemID)
	if err != nil {
		return nil, err
	}

	now := time.Now().UTC()
	feedModel := &model.Feed{
		ItemID:          info.ItemID,
		Provider:        info.Provider,
		LinkType:        info.LinkType,
		Format:          cfg.Format,
		Quality:         cfg.Quality,
		CoverArtQuality: cfg.Custom.CoverArtQuality,
		PageSize:        cfg.PageSize,
		PlaylistSort:    cfg.PlaylistSort,
		PrivateFeed:     cfg.PrivateFeed,
		Title:           strings.TrimSpace(doc.Title),
		Description:     strings.TrimSpace(doc.Description),
		Author:          strings.TrimSpace(doc.Author),
		ItemURL:         sanitizeHTTPURL(doc.Link),
		CoverArt:        sanitizeHTTPURL(doc.CoverArt),
		PubDate:         now,
		UpdatedAt:       now,
	}

	if feedModel.Title == "" {
		feedModel.Title = cfg.ID
	}
	if feedModel.Description == "" {
		feedModel.Description = feedModel.Title
	}

	limit := len(doc.Episodes)
	if cfg.PageSize > 0 && cfg.PageSize < limit {
		limit = cfg.PageSize
	}

	episodes := make([]*model.Episode, 0, limit)
	seen := make(map[string]struct{}, limit)
	var newestPub time.Time

	for idx, item := range doc.Episodes {
		if cfg.PageSize > 0 && len(episodes) >= cfg.PageSize {
			break
		}

		id := strings.TrimSpace(item.ID)
		videoURL := strings.TrimSpace(item.VideoURL)
		if id == "" || videoURL == "" {
			log.WithField("index", idx).Warn("skipping local JSON episode missing id or video_url")
			continue
		}
		if !validEpisodeIDRegex.MatchString(id) {
			log.WithField("episode_id", id).Warn("skipping local JSON episode with unsafe id")
			continue
		}
		if !isPublicHTTPURL(videoURL) {
			log.WithFields(log.Fields{"episode_id": id, "video_url": videoURL}).Warn("skipping local JSON episode with disallowed video_url")
			continue
		}
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}

		pubDate, err := parseLocalJSONDate(item.PubDate)
		if err != nil {
			log.WithFields(log.Fields{"episode_id": id, "pub_date": item.PubDate}).Warn("failed to parse local JSON pub_date, using current time")
			pubDate = now
		}
		if pubDate.After(newestPub) {
			newestPub = pubDate
		}

		dur := item.Duration
		if dur < 1 {
			dur = 1
		}

		title := strings.TrimSpace(item.Title)
		if title == "" {
			title = id
		}

		episodes = append(episodes, &model.Episode{
			ID:          id,
			Title:       title,
			Description: item.Description,
			Thumbnail:   sanitizeHTTPURL(item.Thumbnail),
			Duration:    dur,
			Size:        estimateLocalJSONSize(dur, cfg.Format, cfg.Quality),
			VideoURL:    videoURL,
			PubDate:     pubDate,
			Order:       strconv.Itoa(idx),
			Status:      model.EpisodeNew,
		})
	}

	if !newestPub.IsZero() {
		feedModel.PubDate = newestPub
	}
	feedModel.Episodes = episodes

	return feedModel, nil
}

func readLocalJSONFeed(path string) (*localJSONFeed, error) {
	// Reject symlinks and special files (FIFOs, sockets, devices) before opening
	// so a compromised writer cannot escape the mount or block the scheduler loop.
	linfo, err := os.Lstat(path)
	if err != nil {
		return nil, errors.Wrapf(err, "failed to stat local JSON feed %q", path)
	}
	if linfo.Mode()&os.ModeSymlink != 0 {
		return nil, errors.Errorf("local JSON feed %q must not be a symlink", path)
	}
	if !linfo.Mode().IsRegular() {
		return nil, errors.Errorf("local JSON feed %q must be a regular file", path)
	}

	f, err := os.Open(path)
	if err != nil {
		return nil, errors.Wrapf(err, "failed to open local JSON feed %q", path)
	}
	defer f.Close()

	finfo, err := f.Stat()
	if err != nil {
		return nil, errors.Wrapf(err, "failed to stat opened local JSON feed %q", path)
	}
	if !finfo.Mode().IsRegular() || !os.SameFile(linfo, finfo) {
		return nil, errors.Errorf("local JSON feed %q changed or is not a regular file", path)
	}

	data, err := io.ReadAll(io.LimitReader(f, maxLocalJSONBytes+1))
	if err != nil {
		return nil, errors.Wrapf(err, "failed to read local JSON feed %q", path)
	}
	if len(data) > maxLocalJSONBytes {
		return nil, errors.Errorf("local JSON feed %q exceeds maximum size of %d bytes", path, maxLocalJSONBytes)
	}

	var doc localJSONFeed
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, errors.Wrapf(err, "failed to parse local JSON feed %q", path)
	}

	return &doc, nil
}

func parseLocalJSONDate(s string) (time.Time, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, errors.New("empty pub_date")
	}
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t.UTC(), nil
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t.UTC(), nil
	}
	return time.Time{}, errors.Errorf("unsupported date format: %q", s)
}

func sanitizeHTTPURL(raw string) string {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" || !isHTTPURL(trimmed) {
		return ""
	}
	return trimmed
}

func isHTTPURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	return (u.Scheme == "http" || u.Scheme == "https") && u.Host != ""
}

// isPublicHTTPURL requires http(s) and rejects loopback, private, link-local,
// multicast IP literals and obvious internal hostnames.
func isPublicHTTPURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return false
	}
	host := strings.ToLower(strings.TrimSuffix(u.Hostname(), "."))
	if host == "" || host == "localhost" || strings.HasSuffix(host, ".localhost") ||
		strings.HasSuffix(host, ".local") || strings.HasSuffix(host, ".internal") {
		return false
	}
	if addr, err := netip.ParseAddr(host); err == nil {
		addr = addr.Unmap()
		if addr.IsLoopback() || addr.IsPrivate() || addr.IsLinkLocalUnicast() ||
			addr.IsLinkLocalMulticast() || addr.IsMulticast() || addr.IsUnspecified() {
			return false
		}
	}
	return true
}

func estimateLocalJSONSize(duration int64, format model.Format, quality model.Quality) int64 {
	if format == model.FormatAudio {
		if quality == model.QualityHigh {
			return duration * highAudioBytesPerSecond
		}
		return duration * lowAudioBytesPerSecond
	}
	if quality == model.QualityHigh {
		return duration * hdBytesPerSecond
	}
	return duration * ldBytesPerSecond
}
