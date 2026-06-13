// Package store persists tokenctl's runtime state to an embedded BoltDB file:
// the per-group consumed counters and an append-only audit log of admit /
// deny / throttle / preempt / release events.
//
// The store implements the budget.State contract; budget.NewTree probes for it
// via a type assertion so passing nil (or any value that does not implement
// the contract) degrades gracefully to in-memory mode (used by tests and
// dry-runs).
//
// Layout of the BoltDB file:
//
//	counters bucket   key=group_path           value=json{consumed,window_start}
//	audit    bucket   key=monotonic_id (8B BE) value=json(AuditEvent)
//
// Counter writes are best-effort: the proxy hot path never blocks on disk —
// SaveCounter buffers into an in-memory map and a background flusher commits
// every flushInterval (or on Close). Audit appends are written synchronously
// inside a short bbolt transaction because losing one is a compliance hole.
package store

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	bolt "go.etcd.io/bbolt"

	"github.com/SuperMarioYL/tokenctl/internal/budget"
)

const (
	bucketCounters = "counters"
	bucketAudit    = "audit"

	flushInterval = 2 * time.Second
	openTimeout   = 3 * time.Second
)

// Store is the BoltDB-backed persister.
type Store struct {
	db   *bolt.DB
	path string

	mu      sync.Mutex
	dirty   map[string]counterRecord
	auditID uint64

	stop    chan struct{}
	stopped chan struct{}

	log *slog.Logger
}

// counterRecord is the JSON shape held in the counters bucket.
type counterRecord struct {
	Consumed    int64     `json:"consumed"`
	WindowStart time.Time `json:"window_start"`
}

// Open creates (or opens) the BoltDB file at path and starts the flusher.
//
// The directory containing path is created if missing. Returns an error when
// the file is held open by another process (bbolt acquires an exclusive flock
// and times out after openTimeout).
func Open(path string) (*Store, error) {
	if path == "" {
		return nil, errors.New("store: empty path")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("store: mkdir: %w", err)
	}
	db, err := bolt.Open(path, 0o600, &bolt.Options{Timeout: openTimeout})
	if err != nil {
		return nil, fmt.Errorf("store: open %s: %w", path, err)
	}
	if err := db.Update(func(tx *bolt.Tx) error {
		for _, name := range []string{bucketCounters, bucketAudit} {
			if _, berr := tx.CreateBucketIfNotExists([]byte(name)); berr != nil {
				return berr
			}
		}
		return nil
	}); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("store: init buckets: %w", err)
	}

	s := &Store{
		db:      db,
		path:    path,
		dirty:   map[string]counterRecord{},
		stop:    make(chan struct{}),
		stopped: make(chan struct{}),
		log:     slog.Default().With("subsystem", "store"),
	}
	if err := s.loadAuditCursor(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("store: load audit cursor: %w", err)
	}
	go s.flushLoop()
	return s, nil
}

// Path returns the BoltDB file path.
func (s *Store) Path() string { return s.path }

// Close stops the flusher, flushes pending counters, and releases the file.
func (s *Store) Close() error {
	close(s.stop)
	<-s.stopped
	if err := s.flush(); err != nil {
		s.log.Warn("final flush failed", "err", err)
	}
	return s.db.Close()
}

// LoadCounter implements budget.State.
func (s *Store) LoadCounter(group string) (int64, time.Time, error) {
	var rec counterRecord
	var found bool
	err := s.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(bucketCounters))
		if b == nil {
			return nil
		}
		raw := b.Get([]byte(group))
		if raw == nil {
			return nil
		}
		found = true
		return json.Unmarshal(raw, &rec)
	})
	if err != nil {
		return 0, time.Time{}, err
	}
	if !found {
		return 0, time.Time{}, nil
	}
	return rec.Consumed, rec.WindowStart, nil
}

// SaveCounter implements budget.State. Buffers the write into the dirty set;
// the flusher commits to disk on the next tick.
func (s *Store) SaveCounter(group string, consumed int64, windowStart time.Time) error {
	s.mu.Lock()
	s.dirty[group] = counterRecord{Consumed: consumed, WindowStart: windowStart}
	s.mu.Unlock()
	return nil
}

// AppendAudit implements budget.State. Synchronous — audit log integrity beats
// hot-path latency.
func (s *Store) AppendAudit(e budget.AuditEvent) error {
	payload, err := json.Marshal(e)
	if err != nil {
		return fmt.Errorf("store: marshal audit event: %w", err)
	}
	return s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(bucketAudit))
		if b == nil {
			return errors.New("store: audit bucket missing")
		}
		s.mu.Lock()
		s.auditID++
		id := s.auditID
		s.mu.Unlock()
		key := make([]byte, 8)
		binary.BigEndian.PutUint64(key, id)
		return b.Put(key, payload)
	})
}

// flushLoop drains the dirty counter set into BoltDB on a ticker.
func (s *Store) flushLoop() {
	defer close(s.stopped)
	t := time.NewTicker(flushInterval)
	defer t.Stop()
	for {
		select {
		case <-s.stop:
			return
		case <-t.C:
			if err := s.flush(); err != nil {
				s.log.Warn("flush failed", "err", err)
			}
		}
	}
}

func (s *Store) flush() error {
	s.mu.Lock()
	if len(s.dirty) == 0 {
		s.mu.Unlock()
		return nil
	}
	pending := s.dirty
	s.dirty = map[string]counterRecord{}
	s.mu.Unlock()

	return s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(bucketCounters))
		if b == nil {
			return errors.New("store: counters bucket missing")
		}
		for group, rec := range pending {
			payload, err := json.Marshal(rec)
			if err != nil {
				return fmt.Errorf("store: marshal %s: %w", group, err)
			}
			if err := b.Put([]byte(group), payload); err != nil {
				return err
			}
		}
		return nil
	})
}

// loadAuditCursor reads the highest audit key so AppendAudit issues a
// monotonic id after a restart.
func (s *Store) loadAuditCursor() error {
	return s.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(bucketAudit))
		if b == nil {
			return nil
		}
		c := b.Cursor()
		k, _ := c.Last()
		if k == nil || len(k) != 8 {
			return nil
		}
		s.auditID = binary.BigEndian.Uint64(k)
		return nil
	})
}
