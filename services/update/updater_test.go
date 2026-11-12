package update

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mxpv/podsync/pkg/builder"
	"github.com/mxpv/podsync/pkg/db"
	"github.com/mxpv/podsync/pkg/feed"
	"github.com/mxpv/podsync/pkg/fs"
	"github.com/mxpv/podsync/pkg/model"
	"github.com/mxpv/podsync/pkg/ytdl"
)

const testFeedID = "F"

// fakeBuilder stands in for a provider API. Each Build call returns a deep copy
// of feed, so the updater can never mutate the fixture between runs.
type fakeBuilder struct {
	feed  *model.Feed
	err   error
	calls int
}

func (b *fakeBuilder) Build(context.Context, *feed.Config) (*model.Feed, error) {
	b.calls++
	if b.err != nil {
		return nil, b.err
	}

	out := *b.feed
	out.Episodes = nil
	for _, episode := range b.feed.Episodes {
		clone := *episode
		out.Episodes = append(out.Episodes, &clone)
	}
	return &out, nil
}

// fakeDownloader stands in for yt-dlp. It records every download attempt and
// fails the episodes listed in errs.
type fakeDownloader struct {
	errs  map[string]error
	calls []string
	// onDownload, when set, runs at the start of every Download call
	onDownload func()
}

func (d *fakeDownloader) Download(_ context.Context, _ *feed.Config, episode *model.Episode) (io.ReadCloser, error) {
	d.calls = append(d.calls, episode.ID)
	if d.onDownload != nil {
		d.onDownload()
	}
	if err := d.errs[episode.ID]; err != nil {
		return nil, err
	}
	return io.NopCloser(strings.NewReader("media-" + episode.ID)), nil
}

func (d *fakeDownloader) PlaylistMetadata(context.Context, string) (ytdl.PlaylistMetadata, error) {
	return ytdl.PlaylistMetadata{}, nil
}

func (d *fakeDownloader) PlaylistEntries(context.Context, string, int) (ytdl.PlaylistMetadata, error) {
	return ytdl.PlaylistMetadata{}, nil
}

// updaterEnv is a Manager wired to a real Badger database and local storage in
// temporary directories, with the provider and the downloader replaced by fakes.
type updaterEnv struct {
	manager    *Manager
	db         *db.Badger
	provider   *fakeBuilder
	downloader *fakeDownloader
	cfg        *feed.Config
	dataDir    string
}

func newUpdaterEnv(t *testing.T, episodes ...*model.Episode) *updaterEnv {
	t.Helper()

	dir := t.TempDir()
	dataDir := filepath.Join(dir, "data")

	database, err := db.NewBadger(&db.Config{Dir: filepath.Join(dir, "db")})
	require.NoError(t, err)
	t.Cleanup(func() { _ = database.Close() })

	storage, err := fs.NewLocal(dataDir, false, false)
	require.NoError(t, err)

	key, err := feed.NewFixedKey("test-key")
	require.NoError(t, err)

	cfg := &feed.Config{
		ID:       testFeedID,
		URL:      "https://youtube.com/channel/UCtest",
		PageSize: 50,
		Format:   model.FormatAudio,
		Quality:  model.QualityHigh,
	}

	env := &updaterEnv{
		db:         database,
		provider:   &fakeBuilder{feed: testProviderFeed(episodes...)},
		downloader: &fakeDownloader{errs: map[string]error{}},
		cfg:        cfg,
		dataDir:    dataDir,
	}

	manager, err := NewUpdater(
		map[string]*feed.Config{testFeedID: cfg},
		map[model.Provider]feed.KeyProvider{model.ProviderYoutube: key},
		"http://localhost",
		env.downloader,
		database,
		storage,
	)
	require.NoError(t, err)

	manager.newBuilder = func(context.Context, model.Provider, string, builder.Downloader) (builder.Builder, error) {
		return env.provider, nil
	}
	env.manager = manager

	return env
}

func testProviderFeed(episodes ...*model.Episode) *model.Feed {
	// A zero publication date makes the RSS library fall back to the wall clock.
	return &model.Feed{
		ItemID:      "UCtest",
		Provider:    model.ProviderYoutube,
		LinkType:    model.TypeChannel,
		Format:      model.FormatAudio,
		Quality:     model.QualityHigh,
		Title:       "Test feed",
		Description: "Test feed description",
		ItemURL:     "https://youtube.com/channel/UCtest",
		PubDate:     time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		Episodes:    episodes,
	}
}

// testEpisode returns a new, not yet downloaded episode published age ago.
func testEpisode(id string, age time.Duration) *model.Episode {
	return &model.Episode{
		ID:          id,
		Title:       "Title " + id,
		Description: "Description " + id,
		Duration:    60,
		VideoURL:    "https://youtube.com/watch?v=" + id,
		PubDate:     time.Now().UTC().Add(-age).Truncate(time.Second),
		Status:      model.EpisodeNew,
	}
}

func (e *updaterEnv) update(t *testing.T) error {
	t.Helper()
	_, err := e.manager.Update(context.Background(), e.cfg)
	return err
}

// episodes returns the stored episodes of the test feed keyed by ID.
func (e *updaterEnv) episodes(t *testing.T) map[string]*model.Episode {
	t.Helper()

	out := map[string]*model.Episode{}
	require.NoError(t, e.db.WalkEpisodes(context.Background(), testFeedID, func(episode *model.Episode) error {
		out[episode.ID] = episode
		return nil
	}))
	return out
}

func (e *updaterEnv) mediaPath(episodeID string) string {
	return filepath.Join(e.dataDir, testFeedID, episodeID+".mp3")
}

func (e *updaterEnv) xml(t *testing.T) string {
	t.Helper()

	data, err := os.ReadFile(filepath.Join(e.dataDir, testFeedID+".xml"))
	require.NoError(t, err)
	return string(data)
}

var guidOrEnclosure = regexp.MustCompile(`<guid[^>]*>[^<]*</guid>|<enclosure[^>]*>`)

// identity returns the GUID and enclosure elements of the generated feed, which
// are what podcast clients use to recognise an episode and its media file.
func (e *updaterEnv) identity(t *testing.T) []string {
	t.Helper()
	return guidOrEnclosure.FindAllString(e.xml(t), -1)
}

func TestUpdateDownloadsNewEpisodes(t *testing.T) {
	env := newUpdaterEnv(t, testEpisode("e1", time.Hour), testEpisode("e2", 2*time.Hour))

	require.NoError(t, env.update(t))

	assert.ElementsMatch(t, []string{"e1", "e2"}, env.downloader.calls)

	episodes := env.episodes(t)
	require.Len(t, episodes, 2)
	for _, id := range []string{"e1", "e2"} {
		assert.Equal(t, model.EpisodeDownloaded, episodes[id].Status, id)
		assert.Equal(t, int64(len("media-"+id)), episodes[id].Size, id)

		content, err := os.ReadFile(env.mediaPath(id))
		require.NoError(t, err)
		assert.Equal(t, "media-"+id, string(content))
	}

	xml := env.xml(t)
	assert.Contains(t, xml, "<guid>e1</guid>")
	assert.Contains(t, xml, "<guid>e2</guid>")
	assert.Contains(t, xml, `url="http://localhost/F/e1.mp3"`)
	assert.Contains(t, xml, `url="http://localhost/F/e2.mp3"`)

	_, err := os.Stat(filepath.Join(env.dataDir, "podsync.opml"))
	assert.NoError(t, err)
}

// A repeated update with nothing new must not download anything again and must
// leave every GUID and enclosure exactly as it was.
func TestUpdateIsIdempotent(t *testing.T) {
	env := newUpdaterEnv(t, testEpisode("e1", time.Hour), testEpisode("e2", 2*time.Hour))

	require.NoError(t, env.update(t))
	before := env.identity(t)
	require.Len(t, before, 4)

	require.NoError(t, env.update(t))

	assert.Equal(t, 2, env.provider.calls)
	assert.Len(t, env.downloader.calls, 2, "second run must not download again")
	assert.Equal(t, before, env.identity(t))
}

// A newly discovered episode is added without disturbing the existing ones.
func TestUpdateAddsNewEpisodeKeepingExistingIdentity(t *testing.T) {
	env := newUpdaterEnv(t, testEpisode("e1", 2*time.Hour))

	require.NoError(t, env.update(t))
	before := env.identity(t)
	require.Len(t, before, 2)

	env.provider.feed = testProviderFeed(testEpisode("e2", time.Hour), testEpisode("e1", 2*time.Hour))
	require.NoError(t, env.update(t))

	assert.Equal(t, []string{"e1", "e2"}, env.downloader.calls)

	after := env.identity(t)
	require.Len(t, after, 4)
	for _, element := range before {
		assert.Contains(t, after, element)
	}
}

func TestUpdateAppliesFilters(t *testing.T) {
	skipped := testEpisode("e2", 2*time.Hour)
	skipped.Title = "Please SKIP this one"

	env := newUpdaterEnv(t, testEpisode("e1", time.Hour), skipped)
	env.cfg.Filters.NotTitle = "(?i)skip"

	require.NoError(t, env.update(t))

	assert.Equal(t, []string{"e1"}, env.downloader.calls)

	episodes := env.episodes(t)
	assert.Equal(t, model.EpisodeDownloaded, episodes["e1"].Status)
	assert.Equal(t, model.EpisodeNew, episodes["e2"].Status)
	assert.NotContains(t, env.xml(t), "<guid>e2</guid>")
}

func TestUpdateKeepLastCleansOldestEpisodes(t *testing.T) {
	env := newUpdaterEnv(t, testEpisode("new", time.Hour), testEpisode("old", 48*time.Hour))
	env.cfg.Clean = &feed.Cleanup{KeepLast: 1}

	require.NoError(t, env.update(t))

	episodes := env.episodes(t)
	assert.Equal(t, model.EpisodeDownloaded, episodes["new"].Status)
	assert.Equal(t, model.EpisodeCleaned, episodes["old"].Status)
	assert.Empty(t, episodes["old"].Title)

	_, err := os.Stat(env.mediaPath("new"))
	assert.NoError(t, err)
	_, err = os.Stat(env.mediaPath("old"))
	assert.True(t, os.IsNotExist(err), "cleaned episode file must be deleted")

	xml := env.xml(t)
	assert.Contains(t, xml, "<guid>new</guid>")
	assert.NotContains(t, xml, "<guid>old</guid>")

	// A cleaned episode is never downloaded again
	require.NoError(t, env.update(t))
	assert.Len(t, env.downloader.calls, 2)
}

// Episodes the provider no longer lists are dropped only if they were never
// downloaded; downloaded episodes stay in the database and in the feed.
func TestUpdateRemovesVanishedUndownloadedEpisodes(t *testing.T) {
	env := newUpdaterEnv(t, testEpisode("e1", time.Hour), testEpisode("e2", 2*time.Hour))
	env.downloader.errs["e2"] = errors.New("video unavailable")

	require.NoError(t, env.update(t))

	episodes := env.episodes(t)
	assert.Equal(t, model.EpisodeDownloaded, episodes["e1"].Status)
	assert.Equal(t, model.EpisodeError, episodes["e2"].Status)
	assert.Equal(t, 1, episodes["e2"].Retries)

	env.provider.feed = testProviderFeed(testEpisode("e3", 30*time.Minute))
	require.NoError(t, env.update(t))

	episodes = env.episodes(t)
	assert.Contains(t, episodes, "e1")
	assert.NotContains(t, episodes, "e2")
	assert.Equal(t, model.EpisodeDownloaded, episodes["e3"].Status)

	xml := env.xml(t)
	assert.Contains(t, xml, "<guid>e1</guid>")
	assert.Contains(t, xml, "<guid>e3</guid>")
}

// HTTP 429 during a download stops the batch without charging any episode,
// still writes the feed, and suspends the feed until the backoff expires.
func TestUpdateRateLimitedDownloadBacksOff(t *testing.T) {
	env := newUpdaterEnv(t, testEpisode("e1", time.Hour), testEpisode("e2", 2*time.Hour))
	env.downloader.errs["e1"] = ytdl.ErrTooManyRequests

	require.NoError(t, env.update(t))

	assert.Equal(t, []string{"e1"}, env.downloader.calls, "batch must stop at the first 429")
	for id, episode := range env.episodes(t) {
		assert.Equal(t, model.EpisodeNew, episode.Status, id)
		assert.Zero(t, episode.Retries, id)
	}

	_, inBackoff := env.manager.inBackoff(testFeedID)
	assert.True(t, inBackoff)
	assert.NotContains(t, env.xml(t), "<guid>")

	// While backing off, the provider and the downloader are left alone
	require.NoError(t, env.update(t))
	assert.Equal(t, 1, env.provider.calls)
	assert.Len(t, env.downloader.calls, 1)
}

// HTTP 429 from the provider skips downloads but rebuilds the feed from the
// database, so the served feed stays intact.
func TestUpdateRateLimitedProviderKeepsFeed(t *testing.T) {
	env := newUpdaterEnv(t, testEpisode("e1", time.Hour))

	require.NoError(t, env.update(t))
	before := env.identity(t)
	require.Len(t, before, 2)

	env.provider.err = ytdl.ErrTooManyRequests
	require.NoError(t, env.update(t))

	_, inBackoff := env.manager.inBackoff(testFeedID)
	assert.True(t, inBackoff)
	assert.Equal(t, before, env.identity(t))
	assert.Len(t, env.downloader.calls, 1)
}

func TestUpdateProviderErrorIsReturned(t *testing.T) {
	env := newUpdaterEnv(t, testEpisode("e1", time.Hour))
	env.provider.err = errors.New("provider unavailable")

	err := env.update(t)

	require.Error(t, err)
	assert.Empty(t, env.downloader.calls)
	assert.Empty(t, env.episodes(t))

	_, inBackoff := env.manager.inBackoff(testFeedID)
	assert.False(t, inBackoff)
}

// Documents the current retry cap: a failing episode is attempted once per
// update until maxDownloadAttempts is reached, then skipped.
func TestUpdateStopsRetryingAfterMaxAttempts(t *testing.T) {
	env := newUpdaterEnv(t, testEpisode("e1", time.Hour))
	env.downloader.errs["e1"] = errors.New("video unavailable")

	for i := 0; i < maxDownloadAttempts; i++ {
		require.NoError(t, env.update(t))
	}

	episode := env.episodes(t)["e1"]
	assert.Equal(t, model.EpisodeError, episode.Status)
	assert.Equal(t, maxDownloadAttempts, episode.Retries)
	assert.Len(t, env.downloader.calls, maxDownloadAttempts)

	require.NoError(t, env.update(t))
	assert.Len(t, env.downloader.calls, maxDownloadAttempts, "no attempt beyond the cap")
}

// Cancelling the context (daemon shutdown) mid-download must not charge the
// in-flight episode or any later one with a failed attempt.
func TestUpdateCancelledBatchChargesNoRetries(t *testing.T) {
	env := newUpdaterEnv(t,
		testEpisode("e1", time.Hour),
		testEpisode("e2", 2*time.Hour),
		testEpisode("e3", 3*time.Hour),
	)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	env.downloader.onDownload = cancel
	for _, id := range []string{"e1", "e2", "e3"} {
		env.downloader.errs[id] = errors.New("signal: killed")
	}

	_, err := env.manager.Update(ctx, env.cfg)

	require.Error(t, err)
	assert.True(t, errors.Is(err, context.Canceled), "got %v", err)
	assert.Len(t, env.downloader.calls, 1)

	episodes := env.episodes(t)
	require.Len(t, episodes, 3)
	for id, episode := range episodes {
		assert.Equal(t, model.EpisodeNew, episode.Status, id)
		assert.Zero(t, episode.Retries, id)
	}
}

// A failure while the context is still live keeps being recorded.
func TestUpdateFailureWithLiveContextStillCounts(t *testing.T) {
	env := newUpdaterEnv(t, testEpisode("e1", time.Hour))
	env.downloader.errs["e1"] = errors.New("video unavailable")

	require.NoError(t, env.update(t))

	episode := env.episodes(t)["e1"]
	assert.Equal(t, model.EpisodeError, episode.Status)
	assert.Equal(t, 1, episode.Retries)
}

func TestUpdateResultCounts(t *testing.T) {
	env := newUpdaterEnv(t, testEpisode("e1", time.Hour), testEpisode("e2", 2*time.Hour))
	env.downloader.errs["e2"] = errors.New("video unavailable")

	result, err := env.manager.Update(context.Background(), env.cfg)
	require.NoError(t, err)
	assert.Equal(t, 2, result.Discovered)
	assert.Equal(t, 1, result.Downloaded)
	assert.Equal(t, 1, result.Failed)
	assert.False(t, result.SkippedBackoff || result.RateLimited || result.Interrupted)
	assert.Empty(t, result.ErrorCategory)

	// Nothing new: e2 is retried (and fails again), nothing is discovered
	result, err = env.manager.Update(context.Background(), env.cfg)
	require.NoError(t, err)
	assert.Equal(t, Result{Failed: 1}, result)

	// One new episode is discovered and downloaded
	env.provider.feed = testProviderFeed(
		testEpisode("e3", 30*time.Minute), testEpisode("e1", time.Hour), testEpisode("e2", 2*time.Hour))
	result, err = env.manager.Update(context.Background(), env.cfg)
	require.NoError(t, err)
	assert.Equal(t, 1, result.Discovered)
	assert.Equal(t, 1, result.Downloaded)
}

func TestUpdateResultErrorCategories(t *testing.T) {
	env := newUpdaterEnv(t, testEpisode("e1", time.Hour))
	env.provider.err = errors.New("boom key=SECRET")

	result, err := env.manager.Update(context.Background(), env.cfg)
	require.Error(t, err)
	assert.Equal(t, ErrCategorySource, result.ErrorCategory)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	env.provider.err = nil
	env.downloader.onDownload = cancel
	env.downloader.errs["e1"] = errors.New("signal: killed")

	result, err = env.manager.Update(ctx, env.cfg)
	require.Error(t, err)
	assert.Equal(t, ErrCategoryCancelled, result.ErrorCategory)
}

func TestUpdateResultRateLimit(t *testing.T) {
	env := newUpdaterEnv(t, testEpisode("e1", time.Hour), testEpisode("e2", 2*time.Hour))
	env.downloader.errs["e1"] = ytdl.ErrTooManyRequests

	before := time.Now()
	result, err := env.manager.Update(context.Background(), env.cfg)
	require.NoError(t, err)
	assert.True(t, result.RateLimited)
	assert.False(t, result.SkippedBackoff)
	assert.WithinDuration(t, before.Add(rateLimitBackoffInitial), result.RetryAfter, time.Minute)

	// The next call is refused up front and reports the same retry time
	skipped, err := env.manager.Update(context.Background(), env.cfg)
	require.NoError(t, err)
	assert.True(t, skipped.SkippedBackoff)
	assert.Equal(t, result.RetryAfter, skipped.RetryAfter)
	assert.Equal(t, 1, env.provider.calls)
}

// Yielding stops the batch between episodes, still rebuilds the feed, and the
// next plain run finishes with exactly the state an uninterrupted run produces.
func TestUpdateYieldsBetweenEpisodes(t *testing.T) {
	episodes := func() []*model.Episode {
		return []*model.Episode{
			testEpisode("e1", time.Hour), testEpisode("e2", 2*time.Hour), testEpisode("e3", 3*time.Hour),
		}
	}

	env := newUpdaterEnv(t, episodes()...)
	yield := func() bool { return len(env.downloader.calls) >= 1 }

	result, err := env.manager.UpdateYielding(context.Background(), env.cfg, yield)
	require.NoError(t, err)
	assert.True(t, result.Interrupted)
	assert.Equal(t, 1, result.Downloaded)
	assert.Len(t, env.downloader.calls, 1, "the in-flight download completes, no further one starts")

	downloaded := 0
	for id, episode := range env.episodes(t) {
		if episode.Status == model.EpisodeDownloaded {
			downloaded++
			assert.Contains(t, env.xml(t), "<guid>"+id+"</guid>", "feed XML is rebuilt on yield")
		} else {
			assert.Equal(t, model.EpisodeNew, episode.Status, id)
			assert.Zero(t, episode.Retries, id)
		}
	}
	assert.Equal(t, 1, downloaded)

	// Resume as a normal update
	result, err = env.manager.Update(context.Background(), env.cfg)
	require.NoError(t, err)
	assert.False(t, result.Interrupted)
	assert.Equal(t, 2, result.Downloaded)
	assert.Equal(t, 0, result.Discovered)

	reference := newUpdaterEnv(t, episodes()...)
	require.NoError(t, reference.update(t))
	assert.ElementsMatch(t, reference.identity(t), env.identity(t))
}
