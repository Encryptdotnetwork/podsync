package update

import (
	"errors"
	"fmt"
	"sort"
	"time"
)

// SyncState is the user-visible state of a feed's most recent or current update.
type SyncState string

const (
	// StateIdle: the feed has not been updated since the daemon started.
	StateIdle SyncState = "idle"
	// StateQueued: waiting for its turn.
	StateQueued SyncState = "queued"
	// StateSyncing: an update is running now.
	StateSyncing SyncState = "syncing"
	// StateCompleted: the last update finished, found or changed something,
	// and no episode download failed.
	StateCompleted SyncState = "completed"
	// StateCompletedWithErrors: the update itself succeeded but at least one
	// episode download failed (other episodes may have been downloaded).
	StateCompletedWithErrors SyncState = "completed_with_errors"
	// StateNoNewEpisodes: the last update finished without new episodes,
	// downloads or failures.
	StateNoNewEpisodes SyncState = "no_new_episodes"
	// StateFailed: the last update failed; see FeedStatus.Error for the category.
	StateFailed SyncState = "failed"
	// StateRateLimited: the platform answered HTTP 429, or the feed is still
	// in its backoff window; see FeedStatus.RetryAfter.
	StateRateLimited SyncState = "rate_limited"
)

// Triggers recorded in FeedStatus.Trigger.
const (
	TriggerManual    = "manual"
	TriggerScheduled = "scheduled"
)

// Reasons carried by RefusedError.
const (
	ReasonRateLimited = "rate_limited" // the feed is in a rate-limit backoff window
	ReasonTooSoon     = "too_soon"     // the previous manual run of this feed was too recent
)

// ManualSyncSpacing is the minimum time between the end of one manual run of a
// feed and the next manual request for it.
const ManualSyncSpacing = 30 * time.Second

// ErrUnknownFeed is returned for a feed ID that is not in the configuration.
var ErrUnknownFeed = errors.New("unknown feed")

// RefusedError reports that a manual sync request was not accepted and when it
// may be retried.
type RefusedError struct {
	Reason     string
	RetryAfter time.Time
}

func (e *RefusedError) Error() string {
	return fmt.Sprintf("sync refused: %s, retry after %s", e.Reason, e.RetryAfter.Format(time.RFC3339))
}

// FeedStatus is a snapshot of one feed's sync state. It holds only counters,
// timestamps and fixed category strings, never raw error text.
type FeedStatus struct {
	ID    string    `json:"id"`
	State SyncState `json:"state"`
	// Trigger is "manual" or "scheduled": what started (or queued) the current
	// or last update.
	Trigger string `json:"trigger,omitempty"`
	// Pending is true while an update runs and another one is already queued
	// behind it.
	Pending    bool      `json:"pending"`
	QueuedAt   time.Time `json:"queued_at,omitzero"`
	StartedAt  time.Time `json:"started_at,omitzero"`
	FinishedAt time.Time `json:"finished_at,omitzero"`
	// Counters of the last finished run (episodes discovered, downloaded and
	// failed to download).
	Discovered int `json:"discovered"`
	Downloaded int `json:"downloaded"`
	Failed     int `json:"failed"`
	// Error is one of the ErrCategory constants when State is "failed".
	Error string `json:"error,omitempty"`
	// RetryAfter is when a rate-limited feed may be updated again.
	RetryAfter time.Time `json:"retry_after,omitzero"`
}

// SyncStatus is a snapshot of every configured feed.
type SyncStatus struct {
	// Busy is true while any feed is queued or syncing.
	Busy  bool         `json:"busy"`
	Feeds []FeedStatus `json:"feeds"`
}

// RefusedFeed is one feed that SyncAll did not queue.
type RefusedFeed struct {
	ID         string    `json:"id"`
	Reason     string    `json:"reason"`
	RetryAfter time.Time `json:"retry_after"`
}

// SyncAllResult lists what a SyncAll call did.
type SyncAllResult struct {
	// Queued holds the feeds that are now queued (including those that
	// already were).
	Queued  []string      `json:"queued"`
	Refused []RefusedFeed `json:"refused"`
}

// SyncNow asks for an immediate update of one feed. The request goes through
// the same queue and the same Manager.Update as scheduled updates; it only
// runs ahead of scheduled entries, at the next boundary between episode
// downloads. A feed that is already waiting for a manual run is not queued
// twice, and a request for the feed that is updating right now queues exactly
// one follow-up run.
//
// It returns ErrUnknownFeed for an unknown ID and a *RefusedError while the
// feed is in a rate-limit backoff window or was manually synced less than
// ManualSyncSpacing ago. It is safe to call from any goroutine.
func (s *Scheduler) SyncNow(feedID string) (FeedStatus, error) {
	// Existence is checked against the store, which the HTTP side may read at
	// any time, and not against s.feeds, which belongs to the Run goroutine.
	if _, ok := s.currentFeeds()[feedID]; !ok {
		return FeedStatus{}, ErrUnknownFeed
	}

	now := s.now()

	s.statusMu.Lock()
	defer s.statusMu.Unlock()

	if queued, manual := s.queue.pending(feedID); queued && manual {
		return s.snapshotLocked(feedID), nil
	}

	if until, ok := s.currentManager().inBackoff(feedID); ok {
		return s.snapshotLocked(feedID), &RefusedError{Reason: ReasonRateLimited, RetryAfter: until}
	}

	if last, ok := s.lastManual[feedID]; ok {
		if until := last.Add(ManualSyncSpacing); now.Before(until) {
			return s.snapshotLocked(feedID), &RefusedError{Reason: ReasonTooSoon, RetryAfter: until}
		}
	}

	if s.queue.push(feedID, true) {
		s.markQueuedLocked(feedID, TriggerManual, now)
	}
	return s.snapshotLocked(feedID), nil
}

// SyncAll requests a manual update of every configured feed, in ID order.
// Feeds that cannot be queued right now are listed in the result.
func (s *Scheduler) SyncAll() (SyncAllResult, error) {
	var result SyncAllResult

	for _, id := range s.sortedFeedIDs() {
		_, err := s.SyncNow(id)

		var refused *RefusedError
		switch {
		case err == nil:
			result.Queued = append(result.Queued, id)
		case errors.As(err, &refused):
			result.Refused = append(result.Refused, RefusedFeed{ID: id, Reason: refused.Reason, RetryAfter: refused.RetryAfter})
		case errors.Is(err, ErrUnknownFeed):
			// Removed between listing and queueing; nothing to do
		default:
			return result, err
		}
	}

	return result, nil
}

// Status returns a snapshot of every configured feed, sorted by ID. Feeds that
// have not run yet are reported as idle.
func (s *Scheduler) Status() SyncStatus {
	ids := s.sortedFeedIDs()

	s.statusMu.Lock()
	defer s.statusMu.Unlock()

	status := SyncStatus{Feeds: make([]FeedStatus, 0, len(ids))}
	for _, id := range ids {
		snap := s.snapshotLocked(id)
		if snap.State == StateQueued || snap.State == StateSyncing || snap.Pending {
			status.Busy = true
		}
		status.Feeds = append(status.Feeds, snap)
	}
	return status
}

// sortedFeedIDs returns the configured feed IDs in order.
func (s *Scheduler) sortedFeedIDs() []string {
	feeds := s.currentFeeds()

	ids := make([]string, 0, len(feeds))
	for id := range feeds {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// The helpers below require s.statusMu to be held.

func (s *Scheduler) snapshotLocked(feedID string) FeedStatus {
	st, ok := s.statuses[feedID]
	if !ok {
		return FeedStatus{ID: feedID, State: StateIdle}
	}

	snap := *st
	queued, manual := s.queue.pending(feedID)
	if snap.State == StateSyncing {
		snap.Pending = queued
	} else if queued {
		// A finished run may still have a follow-up waiting to be popped.
		// Keep the snapshot busy so clients do not stop polling between runs.
		snap.State = StateQueued
		snap.Trigger = triggerName(manual)
	}
	return snap
}

func (s *Scheduler) statusLocked(feedID string) *FeedStatus {
	st, ok := s.statuses[feedID]
	if !ok {
		st = &FeedStatus{ID: feedID, State: StateIdle}
		s.statuses[feedID] = st
	}
	return st
}

// markQueuedLocked records that a feed has entered (or been promoted within)
// the queue. A feed that is syncing keeps that state; its queued follow-up is
// visible as Pending.
func (s *Scheduler) markQueuedLocked(feedID, trigger string, now time.Time) {
	st := s.statusLocked(feedID)
	if st.State == StateSyncing {
		return
	}

	st.State = StateQueued
	st.Trigger = trigger
	st.QueuedAt = now
	st.StartedAt = time.Time{}
	st.FinishedAt = time.Time{}
	st.Discovered, st.Downloaded, st.Failed = 0, 0, 0
	st.Error = ""
	st.RetryAfter = time.Time{}
}

func (s *Scheduler) markSyncingLocked(feedID string, manual bool, now time.Time) {
	st := s.statusLocked(feedID)
	st.State = StateSyncing
	st.Trigger = triggerName(manual)
	st.StartedAt = now
	st.FinishedAt = time.Time{}
	st.Discovered, st.Downloaded, st.Failed = 0, 0, 0
	st.Error = ""
	st.RetryAfter = time.Time{}
}

func triggerName(manual bool) string {
	if manual {
		return TriggerManual
	}
	return TriggerScheduled
}

// scheduleEnqueue queues a scheduled update unless the feed is already waiting
// or is updating right now: a tick during a run would only repeat the provider
// lookup the run has just made.
func (s *Scheduler) scheduleEnqueue(feedID string) {
	now := s.now()

	s.statusMu.Lock()
	defer s.statusMu.Unlock()

	if st, ok := s.statuses[feedID]; ok && st.State == StateSyncing {
		return
	}

	if s.queue.push(feedID, false) {
		s.markQueuedLocked(feedID, TriggerScheduled, now)
	}
}

// popNext removes the next queue entry and marks it as syncing in one step, so
// a concurrent SyncNow can never see a feed that is neither queued nor running.
func (s *Scheduler) popNext() (feedID string, manual, ok bool) {
	s.statusMu.Lock()
	defer s.statusMu.Unlock()

	feedID, manual, ok = s.queue.pop()
	if ok {
		s.markSyncingLocked(feedID, manual, s.now())
	}
	return feedID, manual, ok
}

// recordFinish stores the outcome of a run and, for an interrupted scheduled
// run, puts the feed back at the head of the scheduled lane.
func (s *Scheduler) recordFinish(feedID string, manual bool, res Result, err error) {
	now := s.now()

	s.statusMu.Lock()
	defer s.statusMu.Unlock()

	st := s.statusLocked(feedID)
	st.FinishedAt = now
	st.Discovered, st.Downloaded, st.Failed = res.Discovered, res.Downloaded, res.Failed
	st.Error = ""
	st.RetryAfter = time.Time{}

	switch {
	case err != nil:
		st.State = StateFailed
		st.Error = res.ErrorCategory
		if st.Error == "" {
			st.Error = ErrCategoryInternal
		}
	case res.SkippedBackoff || res.RateLimited:
		st.State = StateRateLimited
		st.RetryAfter = res.RetryAfter
	case res.Interrupted:
		// The batch stopped early for a waiting manual request: the feed goes
		// back to the head of the scheduled lane and shows as queued again
		s.queue.requeueFront(feedID)
		st.State = StateQueued
		st.Trigger = TriggerScheduled
		if _, pendingManual := s.queue.pending(feedID); pendingManual {
			st.Trigger = TriggerManual
		}
		st.QueuedAt = now
	case res.Failed > 0:
		st.State = StateCompletedWithErrors
	case res.Discovered == 0 && res.Downloaded == 0:
		st.State = StateNoNewEpisodes
	default:
		st.State = StateCompleted
	}

	if manual {
		s.lastManual[feedID] = now
	}
}

// dropStatus forgets a feed's status, for example because a reload removed it.
func (s *Scheduler) dropStatus(feedID string) {
	s.statusMu.Lock()
	defer s.statusMu.Unlock()

	delete(s.statuses, feedID)
	delete(s.lastManual, feedID)
}

// pruneStatuses drops the status of every feed that is no longer configured.
func (s *Scheduler) pruneStatuses(feeds map[string]struct{}) {
	s.statusMu.Lock()
	defer s.statusMu.Unlock()

	for id := range s.statuses {
		if _, ok := feeds[id]; !ok {
			delete(s.statuses, id)
			delete(s.lastManual, id)
		}
	}
}
