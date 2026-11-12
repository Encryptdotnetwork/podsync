package update

import "sync"

// updateQueue is the scheduler's work queue of feed IDs. It never blocks:
// producers (cron callbacks, HTTP handlers, the Run goroutine itself) push and
// return, and the single Run goroutine drains it.
//
// There are two lanes. Manual requests ("Sync now") are always popped before
// scheduled ones, and each feed has at most one pending entry across both
// lanes: pushing a feed that is already pending is a no-op, except that a
// manual push promotes a pending scheduled entry to the manual lane.
//
// All fields are guarded by mu. wake has a buffer of one: a token means "the
// queue may be non-empty", so a consumer woken by a stale token simply finds
// nothing and goes back to sleep.
type updateQueue struct {
	mu        sync.Mutex
	manual    []string
	scheduled []string

	wake chan struct{}
}

func newUpdateQueue() *updateQueue {
	return &updateQueue{wake: make(chan struct{}, 1)}
}

// notify wakes the consumer without ever blocking the caller.
func (q *updateQueue) notify() {
	select {
	case q.wake <- struct{}{}:
	default:
	}
}

func indexOf(list []string, feedID string) int {
	for i, id := range list {
		if id == feedID {
			return i
		}
	}
	return -1
}

func removeAt(list []string, i int) []string {
	return append(list[:i], list[i+1:]...)
}

// push adds feedID to the manual or scheduled lane. It reports whether the
// call changed the queue (a new entry or a promotion); false means the feed
// was already pending.
func (q *updateQueue) push(feedID string, manual bool) bool {
	q.mu.Lock()
	changed := q.pushLocked(feedID, manual)
	q.mu.Unlock()

	if changed {
		q.notify()
	}
	return changed
}

func (q *updateQueue) pushLocked(feedID string, manual bool) bool {
	if indexOf(q.manual, feedID) >= 0 {
		return false
	}

	if i := indexOf(q.scheduled, feedID); i >= 0 {
		if !manual {
			return false
		}
		q.scheduled = removeAt(q.scheduled, i)
	}

	if manual {
		q.manual = append(q.manual, feedID)
	} else {
		q.scheduled = append(q.scheduled, feedID)
	}
	return true
}

// requeueFront puts an interrupted scheduled update back at the head of the
// scheduled lane. A feed that is already pending keeps its place, except that
// a pending scheduled entry moves to the front.
func (q *updateQueue) requeueFront(feedID string) {
	q.mu.Lock()
	switch {
	case indexOf(q.manual, feedID) >= 0:
		// A manual run is already waiting and will pick the feed up
	case indexOf(q.scheduled, feedID) >= 0:
		q.scheduled = removeAt(q.scheduled, indexOf(q.scheduled, feedID))
		q.scheduled = append([]string{feedID}, q.scheduled...)
	default:
		q.scheduled = append([]string{feedID}, q.scheduled...)
	}
	q.mu.Unlock()

	q.notify()
}

// pop removes the next entry: manual lane first, then scheduled.
func (q *updateQueue) pop() (feedID string, manual bool, ok bool) {
	q.mu.Lock()
	defer q.mu.Unlock()

	if len(q.manual) > 0 {
		feedID, q.manual = q.manual[0], q.manual[1:]
		return feedID, true, true
	}
	if len(q.scheduled) > 0 {
		feedID, q.scheduled = q.scheduled[0], q.scheduled[1:]
		return feedID, false, true
	}
	return "", false, false
}

// hasManual reports whether any manual request is waiting.
func (q *updateQueue) hasManual() bool {
	q.mu.Lock()
	defer q.mu.Unlock()

	return len(q.manual) > 0
}

// pending reports whether feedID has an entry, and in which lane.
func (q *updateQueue) pending(feedID string) (queued, manual bool) {
	q.mu.Lock()
	defer q.mu.Unlock()

	if indexOf(q.manual, feedID) >= 0 {
		return true, true
	}
	return indexOf(q.scheduled, feedID) >= 0, false
}

// len returns the number of pending entries.
func (q *updateQueue) len() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.manual) + len(q.scheduled)
}
