package update

import (
	"context"
	"fmt"
	"runtime/debug"
	"sync"
	"time"

	"github.com/pkg/errors"
	"github.com/robfig/cron/v3"
	log "github.com/sirupsen/logrus"

	"github.com/mxpv/podsync/pkg/config"
	"github.com/mxpv/podsync/pkg/db"
	"github.com/mxpv/podsync/pkg/feed"
	"github.com/mxpv/podsync/pkg/fs"
	"github.com/mxpv/podsync/pkg/model"
)

// Scheduler owns the feed update queue, the cron table and the update Manager.
// Cron entries and manual requests push feed IDs into a de-duplicating,
// non-blocking queue drained by the single Run goroutine, so feed updates
// never run concurrently with each other, and config reloads, which are
// handled in the same loop, never interleave with an in-flight update.
type Scheduler struct {
	store *config.Store

	// Long-lived handles reused when the Manager is rebuilt on reload
	downloader Downloader
	db         db.Storage
	fs         fs.Storage

	// Owned by the Run goroutine after startup; rebuilt by applyReload. The
	// manager pointer is also read by HTTP handlers (SyncNow checks the
	// rate-limit backoff), so it is guarded by mu: applyReload writes it under
	// the write lock and every other goroutine reads it via currentManager.
	// The Run goroutine itself, being the only writer, may read it directly.
	mu      sync.RWMutex
	manager *Manager

	// Owned by the Run goroutine only: HTTP handlers resolve feeds through
	// currentFeeds (the store snapshot) instead
	feeds   map[string]*feed.Config
	cron    *cron.Cron
	entries map[string]cron.EntryID

	// The queue carries feed IDs, not config pointers: IDs are resolved against
	// the current feeds map at dequeue time, so items queued before a reload
	// run with the freshest settings (or are dropped if the feed was removed).
	// It is safe for concurrent use.
	queue *updateQueue

	// Sync status, written by the Run goroutine and by producers (cron
	// callbacks, SyncNow) and read by Status; statusMu guards statuses and
	// lastManual. It is always taken before the queue's own lock, never after,
	// and queue changes that must be consistent with a status change happen
	// while holding it.
	statusMu   sync.Mutex
	statuses   map[string]*FeedStatus
	lastManual map[string]time.Time // end of each feed's last manual run

	// currentFeeds returns the live configuration's feed map (store.Get().Feeds
	// in production). It is read-only: the snapshot is shared with other
	// readers.
	currentFeeds func() map[string]*feed.Config

	// now is the clock; replaced in tests
	now func() time.Time

	// afterRun, when set (tests only, before Run starts), is called on the Run
	// goroutine after each update with its outcome
	afterRun func(feedID string, manual bool, res Result, err error)
}

// KeyProviders builds API key providers from configured tokens and registers
// no-op providers for the platforms that don't require API keys.
func KeyProviders(tokens map[model.Provider]config.StringSlice) (map[model.Provider]feed.KeyProvider, error) {
	keys := make(map[model.Provider]feed.KeyProvider)

	for name, list := range tokens {
		provider, err := feed.NewKeyProvider(list)
		if err != nil {
			return nil, errors.Wrapf(err, "failed to create key provider for %q", name)
		}
		keys[name] = provider
	}

	for _, provider := range []model.Provider{model.ProviderOdysee, model.ProviderSoundcloud, model.ProviderRumble} {
		if _, ok := keys[provider]; !ok {
			keys[provider] = feed.NewNoOpKeyProvider()
		}
	}

	return keys, nil
}

// NewScheduler builds a scheduler from the store's current configuration.
func NewScheduler(store *config.Store, downloader Downloader, db db.Storage, fs fs.Storage) (*Scheduler, error) {
	cfg := store.Get()

	keys, err := KeyProviders(cfg.Tokens)
	if err != nil {
		return nil, err
	}

	manager, err := NewUpdater(cfg.Feeds, keys, cfg.Server.Hostname, downloader, db, fs)
	if err != nil {
		return nil, err
	}

	return &Scheduler{
		store:      store,
		downloader: downloader,
		db:         db,
		fs:         fs,
		manager:    manager,
		feeds:      cfg.Feeds,
		cron:       cron.New(cron.WithChain(cron.SkipIfStillRunning(cron.DiscardLogger))),
		entries:    make(map[string]cron.EntryID),
		queue:      newUpdateQueue(),
		statuses:   make(map[string]*FeedStatus),
		lastManual: make(map[string]time.Time),
		currentFeeds: func() map[string]*feed.Config {
			return store.Get().Feeds
		},
		now: time.Now,
	}, nil
}

// Run builds the schedule, starts the cron and processes feed updates and
// config reloads until ctx is cancelled. Everything that mutates scheduler
// state happens on this goroutine.
func (s *Scheduler) Run(ctx context.Context) error {
	// Initial schedule: feeds without an explicit cron schedule get an
	// immediate first update
	implicit := s.rebuildSchedule()
	s.enqueue(implicit)

	s.cron.Start()
	defer func() {
		log.Info("shutting down cron")
		s.cron.Stop()
	}()

	for {
		select {
		case <-s.queue.wake:
			if err := s.drain(ctx); err != nil {
				return err
			}
		case newCfg := <-s.store.ReloadCh():
			s.applyReload(newCfg)
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// drain processes queued updates one at a time until the queue is empty. A
// pending reload is applied before each item so that the item runs with the
// freshest configuration, and cancellation is honoured between items.
func (s *Scheduler) drain(ctx context.Context) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}

		select {
		case newCfg := <-s.store.ReloadCh():
			s.applyReload(newCfg)
		default:
		}

		feedID, manual, ok := s.popNext()
		if !ok {
			return nil
		}

		s.processUpdate(ctx, feedID, manual)
	}
}

// processUpdate resolves a queued feed ID against the current configuration
// and runs the update. IDs whose feed was removed by a reload are dropped.
func (s *Scheduler) processUpdate(ctx context.Context, feedID string, manual bool) {
	feedConfig, ok := s.feeds[feedID]
	if !ok {
		log.Debugf("skipping queued update for %q: feed no longer in config", feedID)
		s.dropStatus(feedID)
		return
	}

	// Scheduled runs step aside between episodes when any manual request is
	// waiting (for this feed too, so it gets a fresh provider lookup); manual
	// runs always run their batch to the end
	var shouldYield func() bool
	if !manual {
		shouldYield = func() bool { return s.queue.hasManual() }
	}

	logger := log.WithFields(log.Fields{"feed_id": feedID, "trigger": triggerName(manual)})

	result, err := s.safeUpdate(ctx, feedConfig, shouldYield)
	s.recordFinish(feedID, manual, result, err)
	if s.afterRun != nil {
		s.afterRun(feedID, manual, result, err)
	}

	switch {
	case err != nil:
		logger.WithError(err).Errorf("failed to update feed: %s", feedConfig.URL)
	case result.Interrupted:
		logger.Info("update paused for a manual sync request, feed queued again")
	default:
		if entryID, ok := s.entries[feedID]; ok {
			log.Infof("next update of %s: %s", feedID, s.cron.Entry(entryID).Next)
		}
	}
}

// safeUpdate runs a feed update and converts a panic raised inside it (for
// example a nil dereference in a provider) into an error, so one misbehaving
// feed cannot kill the daemon or stop the other feeds from updating.
func (s *Scheduler) safeUpdate(ctx context.Context, feedConfig *feed.Config, shouldYield func() bool) (result Result, err error) {
	defer func() {
		if r := recover(); r != nil {
			log.WithField("feed_id", feedConfig.ID).
				Errorf("panic while updating feed: %v\n%s", r, debug.Stack())
			result.ErrorCategory = ErrCategoryInternal
			err = errors.Errorf("panic during update: %v", r)
		}
	}()

	return s.manager.UpdateYielding(ctx, feedConfig, shouldYield)
}

// applyReload swaps in a new configuration: it rebuilds the key providers and
// the Manager (carrying over active rate-limit backoffs), replaces the feeds
// map and rebuilds the cron table. On any error the previous configuration
// stays active. Newly added feeds without an explicit cron schedule are queued
// for an immediate first update; existing feeds keep their regular cadence.
func (s *Scheduler) applyReload(newCfg *config.Config) {
	keys, err := KeyProviders(newCfg.Tokens)
	if err != nil {
		log.WithError(err).Error("config reload failed: invalid API tokens, keeping previous configuration")
		return
	}

	manager, err := NewUpdater(newCfg.Feeds, keys, newCfg.Server.Hostname, s.downloader, s.db, s.fs)
	if err != nil {
		log.WithError(err).Error("config reload failed: could not create updater, keeping previous configuration")
		return
	}

	// Active 429 cool-downs must survive the reload
	manager.CarryBackoffs(s.manager)

	oldFeeds := s.feeds
	s.mu.Lock()
	s.manager = manager
	s.mu.Unlock()
	s.feeds = newCfg.Feeds

	known := make(map[string]struct{}, len(newCfg.Feeds))
	for feedID := range newCfg.Feeds {
		known[feedID] = struct{}{}
	}
	s.pruneStatuses(known)

	implicit := s.rebuildSchedule()

	var added []string
	for _, feedID := range implicit {
		if _, existed := oldFeeds[feedID]; !existed {
			added = append(added, feedID)
		}
	}
	s.enqueue(added)

	log.Infof("configuration reloaded: %d feed(s) scheduled, %d new", len(s.entries), len(added))
}

// rebuildSchedule drops all cron entries and rebuilds them from the current
// feeds map. It returns the IDs of feeds without an explicit cron schedule
// (candidates for an immediate first update). Must only be called from the Run
// goroutine.
func (s *Scheduler) rebuildSchedule() []string {
	for feedID, entryID := range s.entries {
		s.cron.Remove(entryID)
		delete(s.entries, feedID)
	}

	var implicit []string
	for _, feedConfig := range s.feeds {
		// The feed configs belong to the store's shared snapshot (also read by the
		// admin API), so derive the effective schedule locally instead of writing
		// it back into feedConfig.
		var (
			feedID                  = feedConfig.ID
			schedule                = feedConfig.CronSchedule
			hasExplicitCronSchedule = schedule != ""
		)

		if !hasExplicitCronSchedule {
			schedule = fmt.Sprintf("@every %s", feedConfig.UpdatePeriod.String())
		}

		entryID, err := s.cron.AddFunc(schedule, func() {
			log.Debugf("adding %q to update queue", feedID)
			s.scheduleEnqueue(feedID)
		})
		if err != nil {
			// A bad schedule (e.g. from an admin API edit) must never kill the
			// daemon: leave this feed unscheduled and keep everything else running
			log.WithError(err).Errorf("invalid cron schedule %q for feed %q, feed will not be scheduled", schedule, feedID)
			continue
		}

		s.entries[feedID] = entryID
		log.Debugf("-> %s (update '%s')", feedID, schedule)

		// Only feeds without an explicit cron schedule get an immediate update
		// This prevents unwanted updates when using fixed schedules in Docker deployments
		if !hasExplicitCronSchedule {
			implicit = append(implicit, feedID)
		}
	}

	return implicit
}

// enqueue queues feed IDs for a scheduled update. It never blocks.
func (s *Scheduler) enqueue(feedIDs []string) {
	for _, feedID := range feedIDs {
		s.scheduleEnqueue(feedID)
	}
}

// currentManager returns the live Manager for goroutines other than Run.
func (s *Scheduler) currentManager() *Manager {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.manager
}
