package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/creachadair/tomledit"
	"github.com/creachadair/tomledit/parser"
	"github.com/pkg/errors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const storeTestConfig = `# Podsync test configuration

[server]
port = 8080

[storage]
  [storage.local]
  # Don't change if you run podsync via docker
  data_dir = "/app/data"

[feeds]
  [feeds.A]
  url = "https://youtube.com/channel/test1"
  page_size = 10
`

func setupStore(t *testing.T) (*Store, string) {
	t.Helper()

	path := filepath.Join(t.TempDir(), "config.toml")
	require.NoError(t, os.WriteFile(path, []byte(storeTestConfig), 0644))

	store, err := NewStore(path)
	require.NoError(t, err)
	return store, path
}

// setPageSize returns a mutator that sets feeds.A.page_size to the given value.
func setPageSize(t *testing.T, size int) func(doc *tomledit.Document) error {
	t.Helper()

	return func(doc *tomledit.Document) error {
		e := doc.First("feeds", "A", "page_size")
		if e == nil || e.KeyValue == nil {
			return errors.New("feeds.A.page_size not found")
		}
		e.KeyValue.Value = parser.MustValue(fmt.Sprintf("%d", size))
		return nil
	}
}

func TestStoreGet(t *testing.T) {
	store, _ := setupStore(t)

	cfg := store.Get()
	require.NotNil(t, cfg)
	assert.EqualValues(t, 8080, cfg.Server.Port)
	require.Contains(t, cfg.Feeds, "A")
	assert.EqualValues(t, 10, cfg.Feeds["A"].PageSize)

	// SHA-256 hex digest
	assert.Len(t, store.ETag(), 64)
}

func TestStoreUpdate(t *testing.T) {
	store, path := setupStore(t)
	oldETag := store.ETag()

	cfg, err := store.Update("", setPageSize(t, 25))
	require.NoError(t, err)
	assert.EqualValues(t, 25, cfg.Feeds["A"].PageSize)

	// Snapshot swapped
	assert.Same(t, cfg, store.Get())
	assert.NotEqual(t, oldETag, store.ETag())

	// Disk content updated, comments preserved
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Contains(t, string(data), "page_size = 25")
	assert.Contains(t, string(data), "# Don't change if you run podsync via docker")
	assert.Contains(t, string(data), "# Podsync test configuration")

	// Key ordering preserved: [server] still precedes [feeds]
	content := string(data)
	assert.Less(t, indexOf(t, content, "[server]"), indexOf(t, content, "[feeds]"))

	// Reload channel received the new snapshot
	select {
	case got := <-store.ReloadCh():
		assert.Same(t, cfg, got)
	default:
		t.Fatal("expected a snapshot on the reload channel")
	}

	// Written file must itself be loadable through the normal path
	reloaded, err := LoadConfig(path)
	require.NoError(t, err)
	assert.EqualValues(t, 25, reloaded.Feeds["A"].PageSize)
}

func TestStoreUpdateInvalidConfigRejected(t *testing.T) {
	store, path := setupStore(t)

	before, err := os.ReadFile(path)
	require.NoError(t, err)
	oldCfg := store.Get()
	oldETag := store.ETag()

	// Removing the URL fails validation ("URL is required")
	_, err = store.Update("", func(doc *tomledit.Document) error {
		e := doc.First("feeds", "A", "url")
		require.NotNil(t, e)
		require.True(t, e.Remove())
		return nil
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "validation failed")

	// Disk untouched, snapshot and ETag unchanged, no reload published
	after, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, before, after)
	assert.Same(t, oldCfg, store.Get())
	assert.Equal(t, oldETag, store.ETag())

	select {
	case <-store.ReloadCh():
		t.Fatal("rejected update must not publish a reload")
	default:
	}
}

func TestStoreUpdateMutateErrorAborts(t *testing.T) {
	store, path := setupStore(t)

	before, err := os.ReadFile(path)
	require.NoError(t, err)

	boom := errors.New("boom")
	_, err = store.Update("", func(doc *tomledit.Document) error {
		// Mutate first, then fail: nothing must be written
		e := doc.First("server", "port")
		require.NotNil(t, e)
		e.KeyValue.Value = parser.MustValue("9999")
		return boom
	})
	require.Error(t, err)
	assert.True(t, errors.Is(err, boom))

	after, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, before, after)
}

func TestStoreETagIfMatch(t *testing.T) {
	store, _ := setupStore(t)

	// Stale ETag is rejected
	stale := "0000000000000000000000000000000000000000000000000000000000000000"
	_, err := store.Update(stale, setPageSize(t, 25))
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrConflict))

	// Current ETag is accepted
	current := store.ETag()
	_, err = store.Update(current, setPageSize(t, 25))
	require.NoError(t, err)

	// Reusing the pre-write ETag now conflicts (lost-update protection)
	_, err = store.Update(current, setPageSize(t, 30))
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrConflict))
}

func TestStoreReloadPicksUpExternalEdits(t *testing.T) {
	store, path := setupStore(t)

	// Simulate a hand edit on disk behind the store's back
	edited := []byte(`
[storage]
  [storage.local]
  data_dir = "/app/data"

[feeds]
  [feeds.A]
  url = "https://youtube.com/channel/test1"
  page_size = 99
`)
	require.NoError(t, os.WriteFile(path, edited, 0644))

	cfg, err := store.Reload()
	require.NoError(t, err)
	assert.EqualValues(t, 99, cfg.Feeds["A"].PageSize)
	assert.Same(t, cfg, store.Get())

	select {
	case got := <-store.ReloadCh():
		assert.Same(t, cfg, got)
	default:
		t.Fatal("expected a snapshot on the reload channel")
	}
}

func TestStoreReloadChCoalesces(t *testing.T) {
	store, _ := setupStore(t)

	for _, size := range []int{20, 30} {
		_, err := store.Update("", setPageSize(t, size))
		require.NoError(t, err)
	}

	// Only the most recent snapshot is pending
	select {
	case got := <-store.ReloadCh():
		assert.EqualValues(t, 30, got.Feeds["A"].PageSize)
	default:
		t.Fatal("expected a snapshot on the reload channel")
	}

	select {
	case <-store.ReloadCh():
		t.Fatal("stale snapshot should have been coalesced away")
	default:
	}
}

// Run with -race: N writers adding distinct feeds must all survive, since every
// Update re-reads the file under the exclusive lock.
func TestStoreConcurrentUpdates(t *testing.T) {
	store, path := setupStore(t)

	const writers = 10

	var wg sync.WaitGroup
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()

			feedID := fmt.Sprintf("G%d", n)
			_, err := store.Update("", func(doc *tomledit.Document) error {
				doc.Sections = append(doc.Sections, &tomledit.Section{
					Heading: &parser.Heading{Name: parser.Key{"feeds", feedID}},
					Items: []parser.Item{
						&parser.KeyValue{
							Name:  parser.Key{"url"},
							Value: parser.MustValue(fmt.Sprintf("%q", "https://youtube.com/channel/"+feedID)),
						},
					},
				})
				return nil
			})
			assert.NoError(t, err)
		}(i)
	}
	wg.Wait()

	// All writes survived: original feed + one per writer
	cfg := store.Get()
	assert.Len(t, cfg.Feeds, writers+1)

	// File on disk agrees and still parses through the normal path
	reloaded, err := LoadConfig(path)
	require.NoError(t, err)
	assert.Len(t, reloaded.Feeds, writers+1)

	// The pending coalesced snapshot is from the chronologically last update,
	// which by construction contains all feeds
	select {
	case got := <-store.ReloadCh():
		assert.Len(t, got.Feeds, writers+1)
	default:
		t.Fatal("expected a snapshot on the reload channel")
	}
}

func indexOf(t *testing.T, haystack, needle string) int {
	t.Helper()

	idx := strings.Index(haystack, needle)
	require.GreaterOrEqual(t, idx, 0, "expected %q to be present", needle)
	return idx
}
