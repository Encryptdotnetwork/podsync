package update

import (
	"context"
	"fmt"

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
// Cron entries enqueue feed IDs into a buffered channel drained by the single
// Run goroutine, so feed updates never run concurrently with each other — and
// config reloads, which are handled in the same loop, never interleave with an
// in-flight update.
type Scheduler struct {
	store *config.Store

	// Long-lived handles reused when the Manager is rebuilt on reload
	downloader Downloader
	db         db.Storage
	fs         fs.Storage

	// Owned by the Run goroutine after startup; rebuilt by applyReload
	manager *Manager
	feeds   map[string]*feed.Config
	cron    *cron.Cron
	entries map[string]cron.EntryID

	// The queue carries feed IDs, not config pointers: IDs are resolved against
	// the current feeds map at dequeue time, so items queued before a reload
	// run with the freshest settings (or are dropped if the feed was removed)
	updates chan string
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
		updates:    make(chan string, 16),
	}, nil
}

// Run builds the schedule, starts the cron and processes feed updates and
// config reloads until ctx is cancelled. Everything that mutates scheduler
// state happens on this goroutine.
func (s *Scheduler) Run(ctx context.Context) error {
	// Initial schedule: feeds without an explicit cron schedule get an
	// immediate first update
	implicit := s.rebuildSchedule()
	s.enqueue(ctx, implicit)

	s.cron.Start()
	defer func() {
		log.Info("shutting down cron")
		s.cron.Stop()
	}()

	for {
		select {
		case feedID := <-s.updates:
			s.processUpdate(ctx, feedID)
		case newCfg := <-s.store.ReloadCh():
			s.applyReload(ctx, newCfg)
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// processUpdate resolves a queued feed ID against the current configuration
// and runs the update. IDs whose feed was removed by a reload are dropped.
func (s *Scheduler) processUpdate(ctx context.Context, feedID string) {
	feedConfig, ok := s.feeds[feedID]
	if !ok {
		log.Debugf("skipping queued update for %q: feed no longer in config", feedID)
		return
	}

	if err := s.manager.Update(ctx, feedConfig); err != nil {
		log.WithError(err).Errorf("failed to update feed: %s", feedConfig.URL)
	} else if entryID, ok := s.entries[feedID]; ok {
		log.Infof("next update of %s: %s", feedID, s.cron.Entry(entryID).Next)
	}
}

// applyReload swaps in a new configuration: it rebuilds the key providers and
// the Manager (carrying over active rate-limit backoffs), replaces the feeds
// map and rebuilds the cron table. On any error the previous configuration
// stays active. Newly added feeds without an explicit cron schedule are queued
// for an immediate first update; existing feeds keep their regular cadence.
func (s *Scheduler) applyReload(ctx context.Context, newCfg *config.Config) {
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
	s.manager = manager
	s.feeds = newCfg.Feeds

	implicit := s.rebuildSchedule()

	var added []string
	for _, feedID := range implicit {
		if _, existed := oldFeeds[feedID]; !existed {
			added = append(added, feedID)
		}
	}
	s.enqueue(ctx, added)

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
		// Track if this feed has an explicit cron schedule
		hasExplicitCronSchedule := feedConfig.CronSchedule != ""

		if feedConfig.CronSchedule == "" {
			feedConfig.CronSchedule = fmt.Sprintf("@every %s", feedConfig.UpdatePeriod.String())
		}

		var (
			feedID   = feedConfig.ID
			schedule = feedConfig.CronSchedule
		)

		entryID, err := s.cron.AddFunc(schedule, func() {
			log.Debugf("adding %q to update queue", feedID)
			s.updates <- feedID
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

// enqueue queues feed IDs for update from a helper goroutine, because the Run
// goroutine must never block sending to its own queue (the sends would
// deadlock once the buffer fills up).
func (s *Scheduler) enqueue(ctx context.Context, feedIDs []string) {
	if len(feedIDs) == 0 {
		return
	}

	go func() {
		for _, feedID := range feedIDs {
			select {
			case s.updates <- feedID:
			case <-ctx.Done():
				return
			}
		}
	}()
}

// Close releases the update queue. Call only after Run has exited.
func (s *Scheduler) Close() {
	close(s.updates)
}
