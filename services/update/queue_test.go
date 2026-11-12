package update

import (
	"fmt"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func drainQueue(q *updateQueue) (ids []string, manual []bool) {
	for {
		id, m, ok := q.pop()
		if !ok {
			return ids, manual
		}
		ids = append(ids, id)
		manual = append(manual, m)
	}
}

func TestQueueManualLaneRunsFirst(t *testing.T) {
	q := newUpdateQueue()
	for _, id := range []string{"A", "B", "C", "D", "E"} {
		q.push(id, false)
	}
	q.push("Z", true)

	ids, manual := drainQueue(q)
	assert.Equal(t, []string{"Z", "A", "B", "C", "D", "E"}, ids)
	assert.Equal(t, []bool{true, false, false, false, false, false}, manual)
}

// Ten rapid requests for one feed leave exactly one pending entry.
func TestQueueDeduplicates(t *testing.T) {
	q := newUpdateQueue()

	assert.True(t, q.push("A", true))
	for i := 0; i < 9; i++ {
		assert.False(t, q.push("A", true))
	}
	assert.False(t, q.push("A", false), "a scheduled tick for a pending manual entry is a no-op")

	ids, _ := drainQueue(q)
	assert.Equal(t, []string{"A"}, ids)
}

// A scheduled tick while a manual entry is pending, or the other way round,
// yields a single run in the manual lane.
func TestQueueManualPromotesScheduled(t *testing.T) {
	q := newUpdateQueue()
	q.push("A", false)
	q.push("B", false)

	assert.False(t, q.push("A", false))
	assert.True(t, q.push("B", true), "promotion counts as a change")

	ids, manual := drainQueue(q)
	assert.Equal(t, []string{"B", "A"}, ids)
	assert.Equal(t, []bool{true, false}, manual)
}

func TestQueueRequeueFront(t *testing.T) {
	q := newUpdateQueue()
	q.push("B", false)
	q.push("C", false)

	q.requeueFront("A")
	q.requeueFront("C")
	ids, _ := drainQueue(q)
	assert.Equal(t, []string{"C", "A", "B"}, ids)

	// A pending manual entry already covers the feed
	q.push("M", true)
	q.requeueFront("M")
	ids, manual := drainQueue(q)
	assert.Equal(t, []string{"M"}, ids)
	assert.Equal(t, []bool{true}, manual)
}

func TestQueueHasManual(t *testing.T) {
	q := newUpdateQueue()
	assert.False(t, q.hasManual())

	q.push("S", false)
	assert.False(t, q.hasManual(), "scheduled entries do not count")

	q.push("A", true)
	assert.True(t, q.hasManual())

	queued, manual := q.pending("S")
	assert.True(t, queued)
	assert.False(t, manual)
}

// Push never blocks, however many entries are pending, and the wake channel
// holds at most one token.
func TestQueueNeverBlocks(t *testing.T) {
	q := newUpdateQueue()
	for i := 0; i < 1000; i++ {
		q.push(fmt.Sprintf("F%d", i), i%2 == 0)
	}
	assert.Equal(t, 1000, q.len())
	assert.Len(t, q.wake, 1)
}

func TestQueueConcurrentUse(t *testing.T) {
	q := newUpdateQueue()

	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				q.push(fmt.Sprintf("F%d", i%25), (i+g)%3 == 0)
				q.hasManual()
				if i%7 == 0 {
					q.pop()
				}
			}
		}(g)
	}
	wg.Wait()

	ids, _ := drainQueue(q)
	seen := map[string]bool{}
	for _, id := range ids {
		require.False(t, seen[id], "duplicate entry %q", id)
		seen[id] = true
	}
}
