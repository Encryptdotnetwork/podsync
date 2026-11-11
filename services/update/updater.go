package update

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"sort"
	"sync"
	"time"

	"github.com/hashicorp/go-multierror"
	"github.com/pkg/errors"
	log "github.com/sirupsen/logrus"

	"github.com/mxpv/podsync/pkg/builder"
	"github.com/mxpv/podsync/pkg/db"
	"github.com/mxpv/podsync/pkg/feed"
	"github.com/mxpv/podsync/pkg/fs"
	"github.com/mxpv/podsync/pkg/model"
	"github.com/mxpv/podsync/pkg/ytdl"
)

type Downloader interface {
	Download(ctx context.Context, feedConfig *feed.Config, episode *model.Episode) (io.ReadCloser, error)
	PlaylistMetadata(ctx context.Context, url string) (metadata ytdl.PlaylistMetadata, err error)
	PlaylistEntries(ctx context.Context, url string, limit int) (metadata ytdl.PlaylistMetadata, err error)
}

type TokenList []string

// builderFactory creates the provider-specific feed builder. It is a field on
// Manager so tests can substitute a fake provider instead of calling real APIs.
type builderFactory func(ctx context.Context, provider model.Provider, key string, downloader builder.Downloader) (builder.Builder, error)

const (
	// maxDownloadAttempts is how many times a failing episode is retried before
	// being skipped permanently (deleted, geo-blocked or private videos).
	maxDownloadAttempts = 5

	// Exponential backoff applied to a feed after the platform responds with
	// HTTP 429 (Too Many Requests): 15m, 30m, 1h, 2h, 2h, ...
	rateLimitBackoffInitial = 15 * time.Minute
	rateLimitBackoffMax     = 2 * time.Hour
)

// Error categories reported in Result.ErrorCategory. They are fixed strings so
// that callers (and the admin API) can expose them without leaking raw error
// text, which may contain URLs or API keys.
const (
	ErrCategorySource    = "source"     // resolving the feed or querying the provider failed
	ErrCategoryDatabase  = "database"   // reading the episode database failed
	ErrCategoryDownload  = "download"   // downloading or storing an episode failed
	ErrCategoryFeedBuild = "feed_build" // writing the feed XML or OPML failed
	ErrCategoryInternal  = "internal"   // unexpected failure (for example a recovered panic)
	ErrCategoryCancelled = "cancelled"  // shutdown interrupted the update
)

// Result describes the outcome of a single feed update.
type Result struct {
	// Discovered is the number of episodes newly added to the database.
	Discovered int
	// Downloaded is the number of episodes downloaded during this run.
	Downloaded int
	// Failed is the number of episode downloads that failed during this run.
	Failed int
	// SkippedBackoff is true when the update did not run because the feed is
	// in a rate-limit backoff window.
	SkippedBackoff bool
	// RateLimited is true when this run itself ran into HTTP 429.
	RateLimited bool
	// RetryAfter is when the backoff window ends. Set with SkippedBackoff and
	// RateLimited.
	RetryAfter time.Time
	// Interrupted is true when the download batch was stopped early to let a
	// waiting manual request run. The feed XML was still rebuilt; the caller
	// is expected to queue the feed again.
	Interrupted bool
	// ErrorCategory is one of the ErrCategory constants when Update returned
	// an error.
	ErrorCategory string
}

// downloadResult is the outcome of one download batch.
type downloadResult struct {
	downloaded  int
	failed      int
	rateLimited bool
	yielded     bool
}

type feedBackoff struct {
	delay time.Duration
	until time.Time
}

type Manager struct {
	hostname   string
	downloader Downloader
	db         db.Storage
	fs         fs.Storage
	feeds      map[string]*feed.Config
	keys       map[model.Provider]feed.KeyProvider
	newBuilder builderFactory

	backoffMu sync.Mutex
	backoffs  map[string]*feedBackoff // per-feed rate-limit backoff state
}

func NewUpdater(
	feeds map[string]*feed.Config,
	keys map[model.Provider]feed.KeyProvider,
	hostname string,
	downloader Downloader,
	db db.Storage,
	fs fs.Storage,
) (*Manager, error) {
	return &Manager{
		hostname:   hostname,
		downloader: downloader,
		db:         db,
		fs:         fs,
		feeds:      feeds,
		keys:       keys,
		newBuilder: builder.New,
		backoffs:   make(map[string]*feedBackoff),
	}, nil
}

// inBackoff reports whether feed updates are temporarily suspended due to rate limiting.
func (u *Manager) inBackoff(feedID string) (time.Time, bool) {
	u.backoffMu.Lock()
	defer u.backoffMu.Unlock()

	b, ok := u.backoffs[feedID]
	if !ok || time.Now().After(b.until) {
		return time.Time{}, false
	}

	return b.until, true
}

// registerRateLimit doubles the feed's backoff window (up to rateLimitBackoffMax)
// and returns the delay applied.
func (u *Manager) registerRateLimit(feedID string) time.Duration {
	u.backoffMu.Lock()
	defer u.backoffMu.Unlock()

	b, ok := u.backoffs[feedID]
	if !ok {
		b = &feedBackoff{delay: rateLimitBackoffInitial}
		u.backoffs[feedID] = b
	} else {
		b.delay *= 2
		if b.delay > rateLimitBackoffMax {
			b.delay = rateLimitBackoffMax
		}
	}

	b.until = time.Now().Add(b.delay)
	return b.delay
}

func (u *Manager) clearBackoff(feedID string) {
	u.backoffMu.Lock()
	defer u.backoffMu.Unlock()

	delete(u.backoffs, feedID)
}

// CarryBackoffs copies the still-active rate-limit backoff state from prev so
// that a configuration reload cannot be used to bypass a 429 cool-down window.
func (u *Manager) CarryBackoffs(prev *Manager) {
	if prev == nil {
		return
	}

	prev.backoffMu.Lock()
	defer prev.backoffMu.Unlock()

	u.backoffMu.Lock()
	defer u.backoffMu.Unlock()

	now := time.Now()
	for feedID, b := range prev.backoffs {
		if now.After(b.until) {
			continue // expired, no need to carry
		}
		u.backoffs[feedID] = b
	}
}

// Update runs a full update of one feed: query the provider, download new
// episodes, clean up and rebuild the feed XML and OPML.
func (u *Manager) Update(ctx context.Context, feedConfig *feed.Config) (Result, error) {
	return u.UpdateYielding(ctx, feedConfig, nil)
}

// UpdateYielding is Update with an optional shouldYield callback. It is
// consulted between episode downloads (never during one); when it returns
// true the rest of the batch is skipped, cleanup and feed generation still
// run, and the result is marked Interrupted.
func (u *Manager) UpdateYielding(ctx context.Context, feedConfig *feed.Config, shouldYield func() bool) (Result, error) {
	var result Result

	if until, ok := u.inBackoff(feedConfig.ID); ok {
		log.WithField("feed_id", feedConfig.ID).
			Warnf("skipping update: rate limited, next attempt after %s", until.Format(time.RFC3339))
		result.SkippedBackoff = true
		result.RetryAfter = until
		return result, nil
	}

	log.WithFields(log.Fields{
		"feed_id": feedConfig.ID,
		"format":  feedConfig.Format,
		"quality": feedConfig.Quality,
	}).Infof("-> updating %s", feedConfig.URL)

	started := time.Now()
	rateLimited := false

	discovered, err := u.updateFeed(ctx, feedConfig)
	result.Discovered = discovered
	if err != nil {
		if !errors.Is(err, ytdl.ErrTooManyRequests) {
			result.ErrorCategory = categoryFor(ctx, ErrCategorySource)
			return result, errors.Wrap(err, "update failed")
		}

		// Rate limited during metadata fetch: skip downloads this cycle, but fall
		// through to rebuild the XML from the database so the served feed stays intact.
		delay := u.registerRateLimit(feedConfig.ID)
		log.WithField("feed_id", feedConfig.ID).
			Warnf("rate limited while fetching feed metadata, backing off for %s", delay)
		rateLimited = true
	}

	if !rateLimited {
		// Fetch episodes for download
		episodesToDownload, err := u.fetchEpisodes(ctx, feedConfig)
		if err != nil {
			result.ErrorCategory = categoryFor(ctx, ErrCategoryDatabase)
			return result, errors.Wrap(err, "fetch episodes failed")
		}

		outcome, err := u.downloadEpisodes(ctx, feedConfig, episodesToDownload, shouldYield)
		result.Downloaded = outcome.downloaded
		result.Failed = outcome.failed
		result.Interrupted = outcome.yielded
		if err != nil {
			result.ErrorCategory = categoryFor(ctx, ErrCategoryDownload)
			return result, errors.Wrap(err, "download failed")
		}

		rateLimited = outcome.rateLimited
		if rateLimited {
			delay := u.registerRateLimit(feedConfig.ID)
			log.WithField("feed_id", feedConfig.ID).
				Warnf("rate limited while downloading episodes, backing off for %s", delay)
		}
	}

	if err := u.cleanup(ctx, feedConfig); err != nil {
		log.WithError(err).Error("cleanup failed")
	}

	if err := u.buildXML(ctx, feedConfig); err != nil {
		result.ErrorCategory = categoryFor(ctx, ErrCategoryFeedBuild)
		return result, errors.Wrap(err, "xml build failed")
	}

	if err := u.buildOPML(ctx); err != nil {
		result.ErrorCategory = categoryFor(ctx, ErrCategoryFeedBuild)
		return result, errors.Wrap(err, "opml build failed")
	}

	if rateLimited {
		result.RateLimited = true
		if until, ok := u.inBackoff(feedConfig.ID); ok {
			result.RetryAfter = until
		}
	} else {
		u.clearBackoff(feedConfig.ID)
	}

	elapsed := time.Since(started)
	log.Infof("successfully updated feed in %s", elapsed)
	return result, nil
}

// categoryFor reports a shutdown as "cancelled" regardless of the stage that
// noticed it.
func categoryFor(ctx context.Context, category string) string {
	if ctx.Err() != nil {
		return ErrCategoryCancelled
	}
	return category
}

// updateFeed pulls API for new episodes and saves them to database. It returns
// the number of episodes that were not in the database before.
func (u *Manager) updateFeed(ctx context.Context, feedConfig *feed.Config) (int, error) {
	info, err := builder.ParseURL(feedConfig.URL)
	if err != nil {
		return 0, errors.Wrapf(err, "failed to parse URL: %s", feedConfig.URL)
	}

	keyProvider, ok := u.keys[info.Provider]
	if !ok {
		return 0, errors.Errorf("key provider %q not loaded", info.Provider)
	}

	// Create an updater for this feed type
	provider, err := u.newBuilder(ctx, info.Provider, keyProvider.Get(), u.downloader)
	if err != nil {
		return 0, err
	}

	// Query API to get episodes
	log.Debug("building feed")
	result, err := provider.Build(ctx, feedConfig)
	if err != nil {
		return 0, err
	}

	log.Debugf("received %d episode(s) for %q", len(result.Episodes), result.Title)

	var (
		episodeSet = make(map[string]struct{})
		known      = make(map[string]struct{})
	)
	if err := u.db.WalkEpisodes(ctx, feedConfig.ID, func(episode *model.Episode) error {
		known[episode.ID] = struct{}{}
		if episode.Status != model.EpisodeDownloaded && episode.Status != model.EpisodeCleaned {
			episodeSet[episode.ID] = struct{}{}
		}
		return nil
	}); err != nil {
		return 0, err
	}

	discovered := 0
	for _, episode := range result.Episodes {
		if _, ok := known[episode.ID]; !ok {
			discovered++
			known[episode.ID] = struct{}{}
		}
	}

	if err := u.db.AddFeed(ctx, feedConfig.ID, result); err != nil {
		return 0, err
	}

	for _, episode := range result.Episodes {
		delete(episodeSet, episode.ID)
	}

	// removing episodes that are no longer available in the feed and not downloaded or cleaned
	for id := range episodeSet {
		log.Infof("removing episode %q", id)
		err := u.db.DeleteEpisode(feedConfig.ID, id)
		if err != nil {
			return discovered, err
		}
	}

	log.Debug("successfully saved updates to storage")
	return discovered, nil
}

func (u *Manager) fetchEpisodes(ctx context.Context, feedConfig *feed.Config) ([]*model.Episode, error) {
	var (
		feedID       = feedConfig.ID
		downloadList []*model.Episode
		pageSize     = feedConfig.PageSize
	)

	log.WithField("page_size", pageSize).Info("fetching episodes for download")

	// Build the list of files to download
	err := u.db.WalkEpisodes(ctx, feedID, func(episode *model.Episode) error {
		var (
			logger = log.WithFields(log.Fields{"episode_id": episode.ID})
		)
		if episode.Status != model.EpisodeNew && episode.Status != model.EpisodeError {
			// File already downloaded
			logger.Infof("skipping due to already downloaded")
			return nil
		}

		if episode.Status == model.EpisodeError && episode.Retries >= maxDownloadAttempts {
			logger.Warnf("skipping: download failed %d times, giving up", episode.Retries)
			return nil
		}

		if !matchFilters(episode, &feedConfig.Filters) {
			return nil
		}

		// Limit the number of episodes downloaded at once
		pageSize--
		if pageSize < 0 {
			return nil
		}

		log.Debugf("adding %s (%q) to queue", episode.ID, episode.Title)
		downloadList = append(downloadList, episode)
		return nil
	})

	if err != nil {
		return nil, errors.Wrapf(err, "failed to build update list")
	}

	return downloadList, nil
}

// downloadEpisodes downloads the given episodes to storage. The result reports
// rateLimited if the remote server responded with HTTP 429 and the batch was
// cut short, and yielded if shouldYield asked to stop between episodes. The
// counters are valid even when an error is returned.
func (u *Manager) downloadEpisodes(
	ctx context.Context,
	feedConfig *feed.Config,
	downloadList []*model.Episode,
	shouldYield func() bool,
) (res downloadResult, err error) {
	var (
		downloadCount = len(downloadList)
		feedID        = feedConfig.ID
	)

	if downloadCount > 0 {
		log.Infof("download count: %d", downloadCount)
	} else {
		log.Info("no episodes to download")
		return res, nil
	}

	// Download pending episodes

	for idx, episode := range downloadList {
		var (
			logger      = log.WithFields(log.Fields{"index": idx, "episode_id": episode.ID})
			episodeName = feed.EpisodeName(feedConfig, episode)
		)

		// Shutting down: leave the remaining episodes untouched
		if ctx.Err() != nil {
			return res, ctx.Err()
		}

		// A manual request is waiting: stop between episodes (never in the
		// middle of one) so it can run. The caller queues this feed again.
		if shouldYield != nil && shouldYield() {
			logger.Info("yielding to a waiting manual sync request")
			res.yielded = true
			return res, nil
		}

		// Check whether episode already exists
		size, err := u.fs.Size(ctx, fmt.Sprintf("%s/%s", feedID, episodeName))
		if err == nil {
			logger.Infof("episode %q already exists on disk", episode.ID)

			// File already exists, update file status and disk size
			if err := u.db.UpdateEpisode(feedID, episode.ID, func(episode *model.Episode) error {
				episode.Size = size
				episode.Status = model.EpisodeDownloaded
				return nil
			}); err != nil {
				logger.WithError(err).Error("failed to update file info")
				return res, err
			}

			continue
		} else if os.IsNotExist(err) {
			// Will download, do nothing here
		} else {
			logger.WithError(err).Error("failed to stat file")
			return res, err
		}

		// Download episode to disk
		// We download the episode to a temp directory first to avoid downloading this file by clients
		// while still being processed by youtube-dl (e.g. a file is being downloaded from YT or encoding in progress)

		logger.Infof("! downloading episode %s", episode.VideoURL)
		tempFile, err := u.downloader.Download(ctx, feedConfig, episode)
		if err != nil {
			// YouTube might block host with HTTP Error 429: Too Many Requests
			// We still need to generate XML, so just stop sending download requests and
			// retry after the backoff window
			if errors.Is(err, ytdl.ErrTooManyRequests) {
				logger.Warn("server responded with a 'Too Many Requests' error")
				res.rateLimited = true
				return res, nil
			}

			// The download was interrupted by shutdown, not by a problem with the episode:
			// don't count an attempt or run the error hooks
			if ctx.Err() != nil {
				logger.Info("download interrupted: context cancelled")
				return res, ctx.Err()
			}

			// Execute episode download error hooks
			if len(feedConfig.OnEpisodeDownloadError) > 0 {
				env := []string{
					"FEED_NAME=" + feedID,
					"EPISODE_TITLE=" + episode.Title,
					"ERROR_MESSAGE=" + err.Error(),
				}

				for i, hook := range feedConfig.OnEpisodeDownloadError {
					if hookErr := hook.Invoke(env); hookErr != nil {
						logger.Errorf("failed to execute episode download error hook %d: %v", i+1, hookErr)
					} else {
						logger.Infof("episode download error hook %d executed successfully", i+1)
					}
				}
			}

			if err := u.db.UpdateEpisode(feedID, episode.ID, func(episode *model.Episode) error {
				episode.Status = model.EpisodeError
				episode.Retries++
				if episode.Retries >= maxDownloadAttempts {
					logger.Warnf("episode failed %d times, it will no longer be retried", episode.Retries)
				}
				return nil
			}); err != nil {
				return res, err
			}

			res.failed++
			continue
		}

		logger.Debug("copying file")
		fileSize, err := u.fs.Create(ctx, fmt.Sprintf("%s/%s", feedID, episodeName), tempFile)
		tempFile.Close()
		if err != nil {
			logger.WithError(err).Error("failed to copy file")
			return res, err
		}

		// Execute post episode download hooks
		if len(feedConfig.PostEpisodeDownload) > 0 {
			env := []string{
				"EPISODE_FILE=" + fmt.Sprintf("%s/%s", feedID, episodeName),
				"FEED_NAME=" + feedID,
				"EPISODE_TITLE=" + episode.Title,
			}

			for i, hook := range feedConfig.PostEpisodeDownload {
				if err := hook.Invoke(env); err != nil {
					logger.Errorf("failed to execute post episode download hook %d: %v", i+1, err)
				} else {
					logger.Infof("post episode download hook %d executed successfully", i+1)
				}
			}
		}

		// Update file status in database

		logger.Infof("successfully downloaded file %q", episode.ID)
		if err := u.db.UpdateEpisode(feedID, episode.ID, func(episode *model.Episode) error {
			episode.Size = fileSize
			episode.Status = model.EpisodeDownloaded
			return nil
		}); err != nil {
			return res, err
		}

		res.downloaded++
	}

	log.Infof("downloaded %d episode(s)", res.downloaded)
	return res, nil
}

func (u *Manager) buildXML(ctx context.Context, feedConfig *feed.Config) error {
	f, err := u.db.GetFeed(ctx, feedConfig.ID)
	if err != nil {
		return err
	}

	// Build iTunes XML feed with data received from builder
	log.Debug("building iTunes podcast feed")
	podcast, err := feed.Build(ctx, f, feedConfig, u.hostname)
	if err != nil {
		return err
	}

	var (
		reader  = bytes.NewReader([]byte(podcast.String()))
		xmlName = fmt.Sprintf("%s.xml", feedConfig.ID)
	)

	if _, err := u.fs.Create(ctx, xmlName, reader); err != nil {
		return errors.Wrap(err, "failed to upload new XML feed")
	}

	return nil
}

func (u *Manager) buildOPML(ctx context.Context) error {
	// Build OPML with data received from builder
	log.Debug("building podcast OPML")
	opml, err := feed.BuildOPML(ctx, u.feeds, u.db, u.hostname)
	if err != nil {
		return err
	}

	var (
		reader  = bytes.NewReader([]byte(opml))
		xmlName = fmt.Sprintf("%s.opml", "podsync")
	)

	if _, err := u.fs.Create(ctx, xmlName, reader); err != nil {
		return errors.Wrap(err, "failed to upload OPML")
	}

	return nil
}

func (u *Manager) cleanup(ctx context.Context, feedConfig *feed.Config) error {
	var (
		feedID = feedConfig.ID
		logger = log.WithField("feed_id", feedID)
		list   []*model.Episode
		result *multierror.Error
	)

	if feedConfig.Clean == nil {
		logger.Debug("no cleanup policy configured")
		return nil
	}

	count := feedConfig.Clean.KeepLast
	if count < 1 {
		logger.Info("nothing to clean")
		return nil
	}

	logger.WithField("count", count).Info("running cleaner")
	if err := u.db.WalkEpisodes(ctx, feedConfig.ID, func(episode *model.Episode) error {
		if episode.Status == model.EpisodeDownloaded {
			list = append(list, episode)
		}
		return nil
	}); err != nil {
		return err
	}

	if count > len(list) {
		return nil
	}

	sort.Slice(list, func(i, j int) bool {
		return list[i].PubDate.After(list[j].PubDate)
	})

	for _, episode := range list[count:] {
		logger.WithField("episode_id", episode.ID).Infof("deleting %q", episode.Title)

		var (
			episodeName = feed.EpisodeName(feedConfig, episode)
			path        = fmt.Sprintf("%s/%s", feedConfig.ID, episodeName)
		)

		err := u.fs.Delete(ctx, path)
		if err != nil {
			if !errors.Is(err, os.ErrNotExist) {
				logger.WithError(err).Errorf("failed to delete episode file: %s", episode.ID)
				result = multierror.Append(result, errors.Wrapf(err, "failed to delete episode: %s", episode.ID))
				continue
			}

			logger.WithField("episode_id", episode.ID).Info("episode was not found - file does not exist")
		}

		if err := u.db.UpdateEpisode(feedID, episode.ID, func(episode *model.Episode) error {
			episode.Status = model.EpisodeCleaned
			episode.Title = ""
			episode.Description = ""
			return nil
		}); err != nil {
			result = multierror.Append(result, errors.Wrapf(err, "failed to set state for cleaned episode: %s", episode.ID))
			continue
		}
	}

	return result.ErrorOrNil()
}
