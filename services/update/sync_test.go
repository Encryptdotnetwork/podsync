package update

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mxpv/podsync/pkg/builder"
	"github.com/mxpv/podsync/pkg/config"
	"github.com/mxpv/podsync/pkg/db"
	"github.com/mxpv/podsync/pkg/feed"
	"github.com/mxpv/podsync/pkg/fs"
	"github.com/mxpv/podsync/pkg/model"
	"github.com/mxpv/podsync/pkg/ytdl"
)

// fakeClock is a manually advanced clock, safe for concurrent use.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// multiBuilder is a fake provider serving several feeds, keyed by feed ID. It
// records the order in which feeds were built, which is the order the
// scheduler ran their updates in.
type multiBuilder struct {
	mu    sync.Mutex
	feeds map[string]*model.Feed
	errs  map[string]error
	order []string
}

func (b *multiBuilder) Build(_ context.Context, cfg *feed.Config) (*model.Feed, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.order = append(b.order, cfg.ID)
	if err := b.errs[cfg.ID]; err != nil {
		return nil, err
	}

	src, ok := b.feeds[cfg.ID]
	if !ok {
		src = testProviderFeed()
	}

	out := *src
	out.Episodes = nil
	for _, episode := range src.Episodes {
		clone := *episode
		out.Episodes = append(out.Episodes, &clone)
	}
	return &out, nil
}

func (b *multiBuilder) set(feedID string, episodes ...*model.Episode) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.feeds[feedID] = testProviderFeed(episodes...)
}

func (b *multiBuilder) built() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]string(nil), b.order...)
}

// gate lets a test hold a download open: started is closed when the download
// begins, and the download finishes when release is closed.
type gate struct {
	started chan struct{}
	release chan struct{}
}

// gateDownloader is a fake yt-dlp whose downloads can be blocked per episode.
// It honours context cancellation like the real one.
type gateDownloader struct {
	mu    sync.Mutex
	gates map[string]*gate
	calls []string
}

func (d *gateDownloader) addGate(feedID, episodeID string) *gate {
	d.mu.Lock()
	defer d.mu.Unlock()

	g := &gate{started: make(chan struct{}), release: make(chan struct{})}
	d.gates[feedID+"/"+episodeID] = g
	return g
}

func (d *gateDownloader) Download(ctx context.Context, cfg *feed.Config, episode *model.Episode) (io.ReadCloser, error) {
	key := cfg.ID + "/" + episode.ID

	d.mu.Lock()
	d.calls = append(d.calls, key)
	g := d.gates[key]
	d.mu.Unlock()

	if g != nil {
		close(g.started)
		select {
		case <-g.release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return io.NopCloser(strings.NewReader("media-" + key)), nil
}

func (d *gateDownloader) PlaylistMetadata(context.Context, string) (ytdl.PlaylistMetadata, error) {
	return ytdl.PlaylistMetadata{}, nil
}

func (d *gateDownloader) PlaylistEntries(context.Context, string, int) (ytdl.PlaylistMetadata, error) {
	return ytdl.PlaylistMetadata{}, nil
}

// syncEnv is a real Scheduler (real config store, Badger database and local
// storage in a temp dir) with the provider and downloader faked. Feeds use a
// yearly cron schedule, so nothing runs unless the test queues it.
type syncEnv struct {
	t        *testing.T
	s        *Scheduler
	db       *db.Badger
	dataDir  string
	provider *multiBuilder
	dl       *gateDownloader
	clock    *fakeClock
}

func newSyncEnv(t *testing.T, feedIDs ...string) *syncEnv {
	t.Helper()

	dir := t.TempDir()
	dataDir := filepath.Join(dir, "data")

	var cfg strings.Builder
	cfg.WriteString("[tokens]\nyoutube = \"fake-test-key\"\n\n")
	cfg.WriteString("[storage]\ntype = \"local\"\n\n[storage.local]\ndata_dir = " + strconv.Quote(filepath.ToSlash(dataDir)) + "\n\n")
	cfg.WriteString("[feeds]\n")
	for _, id := range feedIDs {
		fmt.Fprintf(&cfg, "[feeds.%s]\nurl = \"https://youtube.com/channel/UC%s\"\nformat = \"audio\"\n"+
			"cron_schedule = \"0 0 1 1 *\"\npage_size = 50\n\n", id, id)
	}

	path := filepath.Join(dir, "config.toml")
	require.NoError(t, os.WriteFile(path, []byte(cfg.String()), 0o600))

	store, err := config.NewStore(path)
	require.NoError(t, err)

	database, err := db.NewBadger(&db.Config{Dir: filepath.Join(dir, "db")})
	require.NoError(t, err)
	t.Cleanup(func() { _ = database.Close() })

	storage, err := fs.NewLocal(dataDir, false, false)
	require.NoError(t, err)

	env := &syncEnv{
		t:        t,
		db:       database,
		dataDir:  dataDir,
		provider: &multiBuilder{feeds: map[string]*model.Feed{}, errs: map[string]error{}},
		dl:       &gateDownloader{gates: map[string]*gate{}},
		clock:    &fakeClock{t: time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)},
	}

	env.s, err = NewScheduler(store, env.dl, database, storage)
	require.NoError(t, err)
	env.s.manager.newBuilder = func(context.Context, model.Provider, string, builder.Downloader) (builder.Builder, error) {
		return env.provider, nil
	}
	env.s.now = env.clock.Now

	return env
}

// run starts the scheduler's Run loop and returns a function that cancels it
// and waits for it to exit. The function is also registered as cleanup.
func (e *syncEnv) run() (stop func() error) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- e.s.Run(ctx) }()

	var (
		once sync.Once
		err  error
	)
	stop = func() error {
		once.Do(func() {
			cancel()
			select {
			case err = <-done:
			case <-time.After(5 * time.Second):
				e.t.Error("scheduler did not stop after cancellation")
			}
		})
		return err
	}
	e.t.Cleanup(func() { _ = stop() })
	return stop
}

func (e *syncEnv) waitIdle() {
	e.t.Helper()
	require.Eventually(e.t, func() bool { return !e.s.Status().Busy }, 5*time.Second, 5*time.Millisecond)
}

func (e *syncEnv) waitStarted(g *gate) {
	e.t.Helper()
	select {
	case <-g.started:
	case <-time.After(5 * time.Second):
		e.t.Fatal("download did not start")
	}
}

func (e *syncEnv) status(feedID string) FeedStatus {
	e.t.Helper()
	for _, st := range e.s.Status().Feeds {
		if st.ID == feedID {
			return st
		}
	}
	e.t.Fatalf("no status for feed %q", feedID)
	return FeedStatus{}
}

func (e *syncEnv) episodes(feedID string) map[string]*model.Episode {
	e.t.Helper()
	out := map[string]*model.Episode{}
	require.NoError(e.t, e.db.WalkEpisodes(context.Background(), feedID, func(episode *model.Episode) error {
		out[episode.ID] = episode
		return nil
	}))
	return out
}

func (e *syncEnv) xml(feedID string) string {
	e.t.Helper()
	data, err := os.ReadFile(filepath.Join(e.dataDir, feedID+".xml"))
	require.NoError(e.t, err)
	return string(data)
}

func (e *syncEnv) identity(feedID string) []string {
	return guidOrEnclosure.FindAllString(e.xml(feedID), -1)
}

// syncEpisode builds an episode with a fixed publication date, so two
// environments fed the same fixtures produce byte-identical records.
func syncEpisode(id string, hoursAgo int) *model.Episode {
	return &model.Episode{
		ID:          id,
		Title:       "Title " + id,
		Description: "Description " + id,
		Duration:    60,
		VideoURL:    "https://youtube.com/watch?v=" + id,
		PubDate:     time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).Add(-time.Duration(hoursAgo) * time.Hour),
		Status:      model.EpisodeNew,
	}
}

var lastBuildDate = regexp.MustCompile(`<lastBuildDate>[^<]*</lastBuildDate>`)

// Acceptance 1: the manual lane and the scheduled lane produce the same
// episode records and the same feed XML (apart from lastBuildDate).
func TestSyncNowEquivalentToScheduledRun(t *testing.T) {
	scheduled := newSyncEnv(t, "A")
	manual := newSyncEnv(t, "A")
	scheduled.run()
	manual.run()

	seed := func(env *syncEnv, withNew bool) {
		eps := []*model.Episode{syncEpisode("e1", 5), syncEpisode("e2", 4)}
		if withNew {
			eps = append(eps, syncEpisode("e3", 1))
		}
		env.provider.set("A", eps...)
	}

	// Identical starting state, built the same way in both environments
	for _, env := range []*syncEnv{scheduled, manual} {
		seed(env, false)
		env.s.scheduleEnqueue("A")
		env.waitIdle()
	}
	require.Equal(t, scheduled.episodes("A"), manual.episodes("A"))

	// One new episode appears; one environment syncs from a tick, the other on demand
	seed(scheduled, true)
	seed(manual, true)
	scheduled.s.scheduleEnqueue("A")
	_, err := manual.s.SyncNow("A")
	require.NoError(t, err)
	scheduled.waitIdle()
	manual.waitIdle()

	assert.Equal(t, scheduled.episodes("A"), manual.episodes("A"))
	require.Len(t, manual.episodes("A"), 3)
	assert.Equal(t,
		lastBuildDate.ReplaceAllString(scheduled.xml("A"), ""),
		lastBuildDate.ReplaceAllString(manual.xml("A"), ""))
	assert.Equal(t, scheduled.identity("A"), manual.identity("A"))
}

// Acceptance 2: GUIDs and enclosure URLs of existing episodes survive a manual
// run that finds nothing and one that adds an item.
func TestSyncNowKeepsGUIDsAndEnclosures(t *testing.T) {
	env := newSyncEnv(t, "A")
	env.run()

	env.provider.set("A", syncEpisode("e1", 3), syncEpisode("e2", 2))
	env.s.scheduleEnqueue("A")
	env.waitIdle()

	baseline := env.identity("A")
	require.Len(t, baseline, 4)

	_, err := env.s.SyncNow("A")
	require.NoError(t, err)
	env.waitIdle()
	assert.Equal(t, StateNoNewEpisodes, env.status("A").State)
	assert.Equal(t, baseline, env.identity("A"))

	env.clock.Advance(time.Minute)
	env.provider.set("A", syncEpisode("e3", 1), syncEpisode("e1", 3), syncEpisode("e2", 2))
	_, err = env.s.SyncNow("A")
	require.NoError(t, err)
	env.waitIdle()

	after := env.identity("A")
	require.Len(t, after, 6)
	for _, element := range baseline {
		assert.Contains(t, after, element)
	}
	assert.Equal(t, StateCompleted, env.status("A").State)
	assert.Equal(t, 1, env.status("A").Discovered)
	assert.Equal(t, 1, env.status("A").Downloaded)
}

// Acceptance 3: a manual request for B waits for A's in-flight download, runs
// before the queued scheduled feeds, and A resumes afterwards.
func TestManualSyncRunsAtEpisodeBoundary(t *testing.T) {
	env := newSyncEnv(t, "A", "B", "C", "D", "E", "F")
	env.provider.set("A", syncEpisode("a1", 3), syncEpisode("a2", 2), syncEpisode("a3", 1))
	env.provider.set("B", syncEpisode("b1", 1))
	g := env.dl.addGate("A", "a1")

	for _, id := range []string{"A", "C", "D", "E", "F"} {
		env.s.scheduleEnqueue(id)
	}
	env.run()
	env.waitStarted(g)

	// A is mid-download, so the request is accepted but nothing else runs yet
	st, err := env.s.SyncNow("B")
	require.NoError(t, err)
	assert.Equal(t, StateQueued, st.State)
	assert.Equal(t, StateSyncing, env.status("A").State)
	assert.Equal(t, []string{"A"}, env.provider.built())

	close(g.release)
	env.waitIdle()

	assert.Equal(t, []string{"A", "B", "A", "C", "D", "E", "F"}, env.provider.built())
	for id, episode := range env.episodes("A") {
		assert.Equal(t, model.EpisodeDownloaded, episode.Status, id)
	}
	assert.Equal(t, model.EpisodeDownloaded, env.episodes("B")["b1"].Status)

	// The interrupted run was not treated as a failure and nothing was fetched twice
	for _, id := range []string{"a1", "a2", "a3"} {
		assert.Zero(t, env.episodes("A")[id].Retries)
	}
	assert.Equal(t, []string{"A/a1", "B/b1", "A/a2", "A/a3"}, env.dl.calls)
}

// Acceptance 4: ten rapid requests for one feed produce one run.
func TestRapidSyncRequestsRunOnce(t *testing.T) {
	env := newSyncEnv(t, "A")
	env.provider.set("A", syncEpisode("e1", 1))

	for i := 0; i < 10; i++ {
		_, err := env.s.SyncNow("A")
		require.NoError(t, err)
	}
	assert.Equal(t, 1, env.s.queue.len())

	env.run()
	env.waitIdle()

	assert.Equal(t, []string{"A"}, env.provider.built())
}

// Acceptance 5: a scheduled tick and a manual request for the same feed, in
// either order, share a single run.
func TestScheduledTickWhileManualPending(t *testing.T) {
	for name, order := range map[string]func(*syncEnv){
		"manual then tick": func(env *syncEnv) {
			_, err := env.s.SyncNow("A")
			require.NoError(t, err)
			env.s.scheduleEnqueue("A")
		},
		"tick then manual": func(env *syncEnv) {
			env.s.scheduleEnqueue("A")
			_, err := env.s.SyncNow("A")
			require.NoError(t, err)
		},
	} {
		t.Run(name, func(t *testing.T) {
			env := newSyncEnv(t, "A")
			order(env)
			assert.Equal(t, 1, env.s.queue.len())
			assert.Equal(t, TriggerManual, env.status("A").Trigger)

			env.run()
			env.waitIdle()
			assert.Equal(t, []string{"A"}, env.provider.built())
		})
	}
}

// Acceptance 6: requests for the feed that is updating right now queue exactly
// one follow-up run.
func TestSyncNowDuringRunQueuesOneFollowUp(t *testing.T) {
	env := newSyncEnv(t, "A")
	env.provider.set("A", syncEpisode("a1", 2), syncEpisode("a2", 1))
	g := env.dl.addGate("A", "a1")

	// The running update is itself manual, so it never yields
	_, err := env.s.SyncNow("A")
	require.NoError(t, err)
	env.run()
	env.waitStarted(g)

	for i := 0; i < 3; i++ {
		_, err := env.s.SyncNow("A")
		require.NoError(t, err)
	}
	st := env.status("A")
	assert.Equal(t, StateSyncing, st.State)
	assert.True(t, st.Pending)
	assert.Equal(t, 1, env.s.queue.len())

	close(g.release)
	env.waitIdle()

	assert.Equal(t, []string{"A", "A"}, env.provider.built())
	assert.False(t, env.status("A").Pending)
	assert.Equal(t, []string{"A/a1", "A/a2"}, env.dl.calls)
}

type runRecord struct {
	feedID      string
	manual      bool
	interrupted bool
}

// A scheduled run yields to a manual request for the SAME feed at the next
// episode boundary: the in-flight download completes, the feed is rebuilt, and
// exactly one manual run follows without a second scheduled one.
func TestScheduledRunYieldsToManualRequestForSameFeed(t *testing.T) {
	env := newSyncEnv(t, "A")
	env.provider.set("A", syncEpisode("a1", 3), syncEpisode("a2", 2), syncEpisode("a3", 1))
	g1 := env.dl.addGate("A", "a1")
	g2 := env.dl.addGate("A", "a2")

	var (
		mu   sync.Mutex
		runs []runRecord
	)
	env.s.afterRun = func(feedID string, manual bool, res Result, _ error) {
		mu.Lock()
		defer mu.Unlock()
		runs = append(runs, runRecord{feedID, manual, res.Interrupted})
	}

	env.s.scheduleEnqueue("A")
	env.run()
	env.waitStarted(g1)

	_, err := env.s.SyncNow("A")
	require.NoError(t, err)

	close(g1.release)
	// The second run reaches a2 only after the first one ended
	env.waitStarted(g2)

	mu.Lock()
	require.Len(t, runs, 1)
	assert.Equal(t, runRecord{"A", false, true}, runs[0], "the scheduled run is interrupted")
	mu.Unlock()

	assert.Equal(t, model.EpisodeDownloaded, env.episodes("A")["a1"].Status, "the in-flight download completed")
	assert.Contains(t, env.xml("A"), "<guid>a1</guid>", "the feed is rebuilt on yield")
	assert.Equal(t, TriggerManual, env.status("A").Trigger)
	assert.Zero(t, env.s.queue.len(), "no additional scheduled entry")

	close(g2.release)
	env.waitIdle()

	mu.Lock()
	assert.Equal(t, []runRecord{{"A", false, true}, {"A", true, false}}, runs)
	mu.Unlock()
	assert.Equal(t, []string{"A", "A"}, env.provider.built())
	assert.Equal(t, []string{"A/a1", "A/a2", "A/a3"}, env.dl.calls, "every episode downloaded exactly once")
	for id, episode := range env.episodes("A") {
		assert.Equal(t, model.EpisodeDownloaded, episode.Status, id)
	}
}

// Acceptance 7: during a rate-limit backoff the request is refused with the
// retry time and the provider is never called.
func TestSyncNowRefusedDuringBackoff(t *testing.T) {
	env := newSyncEnv(t, "A")
	env.provider.set("A", syncEpisode("e1", 1))

	env.s.manager.registerRateLimit("A")
	until, ok := env.s.manager.inBackoff("A")
	require.True(t, ok)

	_, err := env.s.SyncNow("A")
	var refused *RefusedError
	require.ErrorAs(t, err, &refused)
	assert.Equal(t, ReasonRateLimited, refused.Reason)
	assert.Equal(t, until, refused.RetryAfter)
	assert.Zero(t, env.s.queue.len())

	env.run()
	time.Sleep(50 * time.Millisecond)
	assert.Empty(t, env.provider.built())
}

// A backoff that starts after the request was queued still skips the provider
// and is reported as rate limited with its retry time.
func TestQueuedSyncHitsBackoff(t *testing.T) {
	env := newSyncEnv(t, "A")
	env.provider.set("A", syncEpisode("e1", 1))

	_, err := env.s.SyncNow("A")
	require.NoError(t, err)
	env.s.manager.registerRateLimit("A")

	env.run()
	env.waitIdle()

	st := env.status("A")
	assert.Equal(t, StateRateLimited, st.State)
	assert.False(t, st.RetryAfter.IsZero())
	assert.Empty(t, env.provider.built())
}

// Acceptance 8: shutting down with many feeds queued neither panics nor
// charges retries, and nothing after the in-flight feed starts.
func TestShutdownWithQueuedFeeds(t *testing.T) {
	var ids []string
	for i := 1; i <= 24; i++ {
		ids = append(ids, fmt.Sprintf("F%02d", i))
	}

	t.Run("download in flight", func(t *testing.T) {
		env := newSyncEnv(t, ids...)
		for _, id := range ids {
			env.provider.set(id, syncEpisode("e1", 2), syncEpisode("e2", 1))
			env.s.scheduleEnqueue(id)
		}
		g := env.dl.addGate("F01", "e1")

		stop := env.run()
		env.waitStarted(g)

		require.NotPanics(t, func() { assert.ErrorIs(t, stop(), context.Canceled) })

		assert.Equal(t, []string{"F01"}, env.provider.built())
		for id, episode := range env.episodes("F01") {
			assert.Equal(t, model.EpisodeNew, episode.Status, id)
			assert.Zero(t, episode.Retries, id)
		}
	})

	t.Run("cancelled before the first item", func(t *testing.T) {
		env := newSyncEnv(t, ids...)
		for _, id := range ids {
			env.s.scheduleEnqueue(id)
		}

		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		require.NotPanics(t, func() { assert.ErrorIs(t, env.s.Run(ctx), context.Canceled) })
		assert.Empty(t, env.provider.built())
	})
}

// Acceptance 11: a manual run does not move any cron entry's next time.
func TestManualRunDoesNotChangeCronNext(t *testing.T) {
	env := newSyncEnv(t, "A", "B")
	env.provider.set("A", syncEpisode("e1", 1))
	env.run()

	nextTimes := func() []time.Time {
		entries := env.s.cron.Entries()
		out := make([]time.Time, 0, len(entries))
		for _, entry := range entries {
			out = append(out, entry.Next)
		}
		return out
	}

	require.Eventually(t, func() bool {
		next := nextTimes()
		return len(next) == 2 && !next[0].IsZero() && !next[1].IsZero()
	}, 5*time.Second, 5*time.Millisecond)
	before := nextTimes()

	_, err := env.s.SyncNow("A")
	require.NoError(t, err)
	env.waitIdle()
	require.Equal(t, []string{"A"}, env.provider.built())

	assert.Equal(t, before, nextTimes())
}

func TestSyncNowUnknownFeed(t *testing.T) {
	env := newSyncEnv(t, "A")

	_, err := env.s.SyncNow("NOPE")
	assert.ErrorIs(t, err, ErrUnknownFeed)
	assert.Zero(t, env.s.queue.len())
}

func TestSyncNowSpacing(t *testing.T) {
	env := newSyncEnv(t, "A")
	env.provider.set("A", syncEpisode("e1", 1))
	env.run()

	_, err := env.s.SyncNow("A")
	require.NoError(t, err)
	env.waitIdle()

	// Too soon after the manual run that just ended
	env.clock.Advance(10 * time.Second)
	_, err = env.s.SyncNow("A")
	var refused *RefusedError
	require.ErrorAs(t, err, &refused)
	assert.Equal(t, ReasonTooSoon, refused.Reason)
	assert.Equal(t, env.clock.Now().Add(ManualSyncSpacing-10*time.Second), refused.RetryAfter)

	// Scheduled runs are not limited by it
	env.s.scheduleEnqueue("A")
	env.waitIdle()
	assert.Equal(t, []string{"A", "A"}, env.provider.built())

	env.clock.Advance(ManualSyncSpacing)
	_, err = env.s.SyncNow("A")
	require.NoError(t, err)
	env.waitIdle()
	assert.Equal(t, []string{"A", "A", "A"}, env.provider.built())
}

func TestSyncAll(t *testing.T) {
	env := newSyncEnv(t, "A", "B", "C")
	env.s.manager.registerRateLimit("B")

	result, err := env.s.SyncAll()
	require.NoError(t, err)
	assert.Equal(t, []string{"A", "C"}, result.Queued)
	require.Len(t, result.Refused, 1)
	assert.Equal(t, "B", result.Refused[0].ID)
	assert.Equal(t, ReasonRateLimited, result.Refused[0].Reason)

	env.run()
	env.waitIdle()
	assert.Equal(t, []string{"A", "C"}, env.provider.built())
}

// The status exposes fixed categories only: raw provider error text, which may
// carry secrets, never reaches it.
func TestStatusReportsFailureCategoryOnly(t *testing.T) {
	env := newSyncEnv(t, "A", "B")
	env.provider.errs["A"] = errors.New("quota exceeded for https://example.test/api?key=SECRET")
	env.run()

	assert.Equal(t, StateIdle, env.status("B").State)

	_, err := env.s.SyncNow("A")
	require.NoError(t, err)
	env.waitIdle()

	st := env.status("A")
	assert.Equal(t, StateFailed, st.State)
	assert.Equal(t, ErrCategorySource, st.Error)

	raw, err := json.Marshal(env.s.Status())
	require.NoError(t, err)
	assert.NotContains(t, string(raw), "SECRET")
}

// A panic in a provider is reported as an internal failure of that feed only.
func TestStatusAfterPanic(t *testing.T) {
	env := newSyncEnv(t, "A", "B")
	env.provider.set("B", syncEpisode("b1", 1))
	env.s.manager.newBuilder = func(context.Context, model.Provider, string, builder.Downloader) (builder.Builder, error) {
		panic("provider exploded")
	}
	env.run()

	_, err := env.s.SyncNow("A")
	require.NoError(t, err)
	env.waitIdle()

	st := env.status("A")
	assert.Equal(t, StateFailed, st.State)
	assert.Equal(t, ErrCategoryInternal, st.Error)
}

// Removing a feed in a reload drops its queued entry and its status.
func TestReloadPrunesStatus(t *testing.T) {
	env := newSyncEnv(t, "A", "B")
	env.s.scheduleEnqueue("B")
	require.NotEmpty(t, env.status("B").QueuedAt)

	env.s.applyReload(&config.Config{Feeds: testFeedConfigs(t, map[string]*feed.Config{
		"A": {URL: "https://youtube.com/channel/UCA", UpdatePeriod: time.Hour},
	})})

	env.s.statusMu.Lock()
	_, kept := env.s.statuses["B"]
	env.s.statusMu.Unlock()
	assert.False(t, kept)

	// The stale queue entry is dropped when it is dequeued
	env.s.drainForTest(t)
	assert.Empty(t, env.provider.built())
}

func (s *Scheduler) drainForTest(t *testing.T) {
	t.Helper()
	require.NoError(t, s.drain(context.Background()))
}

func TestStatusSortedAndComplete(t *testing.T) {
	env := newSyncEnv(t, "C", "A", "B")

	feeds := env.s.Status().Feeds
	ids := make([]string, 0, len(feeds))
	for _, st := range feeds {
		ids = append(ids, st.ID)
		assert.Equal(t, StateIdle, st.State)
	}
	assert.True(t, sort.StringsAreSorted(ids))
	assert.Len(t, ids, 3)
}
