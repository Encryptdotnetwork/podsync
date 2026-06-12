package update

import (
	"context"
	"testing"
	"time"

	"github.com/robfig/cron/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mxpv/podsync/pkg/config"
	"github.com/mxpv/podsync/pkg/feed"
)

// newTestScheduler builds a scheduler without a config store; tests drive
// applyReload/processUpdate directly instead of going through Run.
func newTestScheduler(t *testing.T, feeds map[string]*feed.Config) *Scheduler {
	t.Helper()

	for id, f := range feeds {
		f.ID = id
	}

	manager, err := NewUpdater(feeds, nil, "http://localhost", nil, nil, nil)
	require.NoError(t, err)

	return &Scheduler{
		manager: manager,
		feeds:   feeds,
		cron:    cron.New(cron.WithChain(cron.SkipIfStillRunning(cron.DiscardLogger))),
		entries: make(map[string]cron.EntryID),
		updates: make(chan string, 16),
	}
}

func testFeedConfigs(t *testing.T, feeds map[string]*feed.Config) map[string]*feed.Config {
	t.Helper()

	for id, f := range feeds {
		f.ID = id
	}
	return feeds
}

func receiveID(t *testing.T, ch <-chan string) string {
	t.Helper()

	select {
	case id := <-ch:
		return id
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for an enqueued feed ID")
		return ""
	}
}

func assertNoPendingID(t *testing.T, ch <-chan string) {
	t.Helper()

	select {
	case id := <-ch:
		t.Fatalf("unexpected feed ID enqueued: %q", id)
	case <-time.After(100 * time.Millisecond):
	}
}

func TestSchedulerRebuildSchedule(t *testing.T) {
	s := newTestScheduler(t, map[string]*feed.Config{
		"A": {URL: "https://youtube.com/channel/a", UpdatePeriod: time.Hour},
		"B": {URL: "https://youtube.com/channel/b", CronSchedule: "0 */6 * * *"},
	})

	implicit := s.rebuildSchedule()

	// Only the feed without an explicit cron schedule is an initial-update candidate
	assert.ElementsMatch(t, []string{"A"}, implicit)
	assert.Len(t, s.entries, 2)
	assert.Len(t, s.cron.Entries(), 2)
}

func TestSchedulerInvalidCronScheduleSkipsFeed(t *testing.T) {
	s := newTestScheduler(t, map[string]*feed.Config{
		"GOOD": {URL: "https://youtube.com/channel/good", UpdatePeriod: time.Hour},
		"BAD":  {URL: "https://youtube.com/channel/bad", CronSchedule: "definitely not a cron expression"},
	})

	// Must not panic or exit; the bad feed is simply left unscheduled
	implicit := s.rebuildSchedule()

	assert.ElementsMatch(t, []string{"GOOD"}, implicit)
	assert.Len(t, s.entries, 1)
	assert.Contains(t, s.entries, "GOOD")
	assert.NotContains(t, s.entries, "BAD")
	assert.Len(t, s.cron.Entries(), 1)
}

func TestSchedulerApplyReloadRebuildsCron(t *testing.T) {
	ctx := context.Background()

	s := newTestScheduler(t, map[string]*feed.Config{
		"A": {URL: "https://youtube.com/channel/a", UpdatePeriod: time.Hour},
		"B": {URL: "https://youtube.com/channel/b", CronSchedule: "0 */6 * * *"},
	})
	s.rebuildSchedule()
	oldManager := s.manager

	// Reload: A removed, B unchanged, C added (implicit schedule)
	newCfg := &config.Config{
		Feeds: testFeedConfigs(t, map[string]*feed.Config{
			"B": {URL: "https://youtube.com/channel/b", CronSchedule: "0 */6 * * *"},
			"C": {URL: "https://youtube.com/channel/c", UpdatePeriod: time.Hour},
		}),
	}
	s.applyReload(ctx, newCfg)

	// Manager rebuilt, feeds map swapped to the new config
	assert.NotSame(t, oldManager, s.manager)
	assert.Equal(t, newCfg.Feeds, s.feeds)

	// Cron table cleanly rebuilt: A gone, B and C scheduled, no stale entries
	assert.Len(t, s.entries, 2)
	assert.NotContains(t, s.entries, "A")
	assert.Contains(t, s.entries, "B")
	assert.Contains(t, s.entries, "C")
	assert.Len(t, s.cron.Entries(), 2)

	// Only the newly added implicit feed gets an immediate first update;
	// pre-existing feeds keep their regular cadence
	assert.Equal(t, "C", receiveID(t, s.updates))
	assertNoPendingID(t, s.updates)
}

func TestSchedulerProcessUpdateDropsStaleIDs(t *testing.T) {
	ctx := context.Background()

	s := newTestScheduler(t, map[string]*feed.Config{
		"A": {URL: "https://youtube.com/channel/a", UpdatePeriod: time.Hour},
	})
	s.rebuildSchedule()

	// "GONE" simulates an ID queued before a reload removed the feed. It must
	// be dropped without touching the manager — whose dependencies are nil here
	// and would panic if an update were attempted with no resolvable config.
	s.processUpdate(ctx, "GONE")

	// A resolvable ID runs through the manager using the current feeds map
	// (the nil-keyed manager fails the update gracefully, which is logged)
	s.processUpdate(ctx, "A")
}

func TestSchedulerReloadCarriesBackoffs(t *testing.T) {
	ctx := context.Background()

	s := newTestScheduler(t, map[string]*feed.Config{
		"A": {URL: "https://youtube.com/channel/a", UpdatePeriod: time.Hour},
	})
	s.rebuildSchedule()

	// Feed A is rate limited before the reload
	delay := s.manager.registerRateLimit("A")
	assert.Equal(t, rateLimitBackoffInitial, delay)
	until, ok := s.manager.inBackoff("A")
	require.True(t, ok)

	s.applyReload(ctx, &config.Config{
		Feeds: testFeedConfigs(t, map[string]*feed.Config{
			"A": {URL: "https://youtube.com/channel/a", UpdatePeriod: time.Hour},
		}),
	})

	// The cool-down survives the manager swap with its deadline intact
	got, ok := s.manager.inBackoff("A")
	require.True(t, ok, "active backoff must survive a config reload")
	assert.Equal(t, until, got)

	// And the exponential progression continues from the carried state
	assert.Equal(t, 2*rateLimitBackoffInitial, s.manager.registerRateLimit("A"))
}

func TestCarryBackoffs(t *testing.T) {
	prev, err := NewUpdater(nil, nil, "", nil, nil, nil)
	require.NoError(t, err)
	prev.backoffs["expired"] = &feedBackoff{delay: time.Minute, until: time.Now().Add(-time.Minute)}
	prev.backoffs["active"] = &feedBackoff{delay: time.Minute, until: time.Now().Add(time.Hour)}

	fresh, err := NewUpdater(nil, nil, "", nil, nil, nil)
	require.NoError(t, err)
	fresh.CarryBackoffs(prev)

	assert.Contains(t, fresh.backoffs, "active")
	assert.NotContains(t, fresh.backoffs, "expired", "expired backoffs are not carried")

	// Nil previous manager is a no-op
	fresh.CarryBackoffs(nil)
}
