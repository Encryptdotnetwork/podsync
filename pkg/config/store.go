package config

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"sync"

	"github.com/creachadair/tomledit"
	"github.com/pkg/errors"

	"github.com/mxpv/podsync/pkg/fs"
)

// ErrConflict is returned by Update when the caller's If-Match ETag does not
// match the current config file content, i.e. the file was modified since the
// caller last read it (concurrent admin edit or a hand edit on disk).
var ErrConflict = errors.New("config file was modified concurrently (ETag mismatch)")

// ErrWriteFailed indicates the mutated config passed validation but could not
// be persisted to disk. Callers should treat this as a server-side error.
var ErrWriteFailed = errors.New("failed to write config file")

// Store is the single owner of config.toml at runtime. All reads and writes go
// through it: edits are applied to the raw TOML document via tomledit (which
// preserves comments and key order, unlike go-toml's Tree serializer),
// validated, atomically written to disk, and published to the reload channel
// for the scheduler to pick up.
type Store struct {
	path string

	mu      sync.RWMutex
	current *Config // last validated snapshot
	etag    string  // SHA-256 hex digest of the file content backing the snapshot

	reload chan *Config // buffered(1); rapid successive edits coalesce to the newest
}

// NewStore loads and validates the config file at path. The initial load is not
// published to the reload channel; callers wire the startup config directly.
func NewStore(path string) (*Store, error) {
	s := &Store{
		path:   path,
		reload: make(chan *Config, 1),
	}

	if _, err := s.loadLocked(); err != nil {
		return nil, err
	}

	return s, nil
}

// Get returns the most recently validated configuration snapshot. The snapshot
// must be treated as read-only; use Update to change configuration.
func (s *Store) Get() *Config {
	s.mu.RLock()
	defer s.mu.RUnlock()

	return s.current
}

// ETag returns the SHA-256 hex digest of the config file content backing the
// current snapshot. Clients send it back via If-Match so concurrent edits are
// detected instead of silently overwriting each other.
func (s *Store) ETag() string {
	s.mu.RLock()
	defer s.mu.RUnlock()

	return s.etag
}

// ReloadCh delivers the new snapshot after every successful Update or Reload.
// The channel is buffered with capacity 1 and coalesces: if the consumer is
// busy, only the most recent snapshot is retained.
func (s *Store) ReloadCh() <-chan *Config {
	return s.reload
}

// Update applies mutate to the parsed TOML document, validates the result,
// atomically writes it to disk and publishes the new snapshot to the reload
// channel.
//
// ifMatch is the ETag the caller last observed; pass an empty string to skip
// the check. If it doesn't match the hash of the file content about to be
// mutated, ErrConflict is returned and nothing is written.
//
// The whole read → mutate → validate → write → publish sequence runs under an
// exclusive lock, so concurrent Updates serialize and readers never observe a
// half-applied state. If validation fails, the file on disk is untouched.
func (s *Store) Update(ifMatch string, mutate func(doc *tomledit.Document) error) (*Config, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	data, err := os.ReadFile(s.path)
	if err != nil {
		return nil, errors.Wrapf(err, "failed to read config file: %s", s.path)
	}

	if ifMatch != "" && ifMatch != etagOf(data) {
		return nil, ErrConflict
	}

	// Mutate the raw TOML document rather than the unmarshaled struct so that
	// comments and key ordering survive the round trip
	doc, err := tomledit.Parse(bytes.NewReader(data))
	if err != nil {
		return nil, errors.Wrap(err, "failed to parse config file")
	}

	if err := mutate(doc); err != nil {
		return nil, errors.Wrap(err, "config mutation failed")
	}

	var buf bytes.Buffer
	var formatter tomledit.Formatter
	if err := formatter.Format(&buf, doc); err != nil {
		return nil, errors.Wrap(err, "failed to serialize config")
	}
	out := buf.Bytes()

	// Validate the mutated document before anything touches disk
	cfg, err := parseConfig(out, s.path)
	if err != nil {
		return nil, errors.Wrap(err, "validation failed, config not saved")
	}

	if _, err := fs.WriteFileAtomic(s.path, bytes.NewReader(out), 0644); err != nil {
		return nil, errors.Wrapf(ErrWriteFailed, "%v", err)
	}

	s.current = cfg
	s.etag = etagOf(out)
	s.notify(cfg)

	return cfg, nil
}

// Reload re-reads the config file from disk (e.g. after a hand edit over SSH),
// validates it and publishes the new snapshot. The file is not modified.
func (s *Store) Reload() (*Config, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	cfg, err := s.loadLocked()
	if err != nil {
		return nil, err
	}

	s.notify(cfg)
	return cfg, nil
}

// loadLocked reads and validates the file, refreshing the snapshot and ETag.
// Callers must hold the write lock (or have exclusive access, as in NewStore).
func (s *Store) loadLocked() (*Config, error) {
	data, err := os.ReadFile(s.path)
	if err != nil {
		return nil, errors.Wrapf(err, "failed to read config file: %s", s.path)
	}

	cfg, err := parseConfig(data, s.path)
	if err != nil {
		return nil, err
	}

	s.current = cfg
	s.etag = etagOf(data)
	return cfg, nil
}

// notify publishes cfg, dropping any pending unconsumed snapshot so the
// consumer always observes the most recent configuration.
func (s *Store) notify(cfg *Config) {
	for {
		select {
		case s.reload <- cfg:
			return
		default:
			// Channel full: discard the stale pending snapshot and retry
			select {
			case <-s.reload:
			default:
			}
		}
	}
}

func etagOf(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
