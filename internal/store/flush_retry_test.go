package store

import (
	"path/filepath"
	"testing"
	"time"
)

// newTestStore opens a Store on a throwaway BoltDB file in t.TempDir().
func newTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "tokenctl-test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	return s
}

// TestFlush_RoundTripPersistsCounter is the baseline: a buffered counter is
// committed by flush and reloads with the same value.
func TestFlush_RoundTripPersistsCounter(t *testing.T) {
	s := newTestStore(t)
	defer s.Close()

	ws := time.Now().UTC().Truncate(time.Second)
	if err := s.SaveCounter("org.team.dev", 4242, ws); err != nil {
		t.Fatalf("SaveCounter: %v", err)
	}
	if err := s.flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}
	got, gotWS, err := s.LoadCounter("org.team.dev")
	if err != nil {
		t.Fatalf("LoadCounter: %v", err)
	}
	if got != 4242 || !gotWS.Equal(ws) {
		t.Fatalf("LoadCounter = %d @ %v, want 4242 @ %v", got, gotWS, ws)
	}
}

// TestFlush_RequeuesOnTxError is the regression for
// fix-flush-drops-counters-on-tx-error: when the underlying db.Update
// transaction fails, the records flush had swapped out of s.dirty must be
// merged back so the NEXT flush retries them — otherwise that window's
// per-node + __wallet__ counters are lost and the hard cap silently resets
// toward 0 on the next restore. We force the tx error by closing the bbolt DB
// out from under the flusher.
func TestFlush_RequeuesOnTxError(t *testing.T) {
	s := newTestStore(t)
	// Stop the background flusher so it can't race us by draining the dirty set.
	close(s.stop)
	<-s.stopped

	if err := s.SaveCounter("org.team.dev", 7777, time.Now()); err != nil {
		t.Fatalf("SaveCounter: %v", err)
	}
	if err := s.SaveCounter("__wallet__", 9999, time.Now()); err != nil {
		t.Fatalf("SaveCounter wallet: %v", err)
	}

	// Close the DB so the next db.Update returns an error (database not open).
	if err := s.db.Close(); err != nil {
		t.Fatalf("close db: %v", err)
	}

	if err := s.flush(); err == nil {
		t.Fatalf("flush against a closed db should error, got nil")
	}

	// The two buffered records must still be queued for retry — NOT dropped.
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.dirty) != 2 {
		t.Fatalf("dirty set after failed flush = %d entries, want 2 (records must be re-queued, not lost)", len(s.dirty))
	}
	if rec, ok := s.dirty["org.team.dev"]; !ok || rec.Consumed != 7777 {
		t.Fatalf("org.team.dev not re-queued correctly: %+v (ok=%v)", rec, ok)
	}
	if rec, ok := s.dirty["__wallet__"]; !ok || rec.Consumed != 9999 {
		t.Fatalf("__wallet__ not re-queued correctly: %+v (ok=%v)", rec, ok)
	}
}

// TestFlush_RetrySucceedsAfterTransientError models the realistic recovery: a
// flush fails (records re-queued), then a later flush against a healthy DB
// commits the re-queued records — i.e. no spend is lost across a transient
// failure, only deferred. We can't reopen the same bbolt file mid-test cleanly,
// so we assert the two halves: (1) a failed flush leaves the record queued, and
// (2) a subsequent successful flush on a fresh store commits an equivalent
// record. The first half is the load-bearing guarantee; this adds the "retry
// actually lands" half end to end.
func TestFlush_RetrySucceedsAfterTransientError(t *testing.T) {
	// Half 1: failed flush re-queues.
	s := newTestStore(t)
	close(s.stop)
	<-s.stopped
	if err := s.SaveCounter("org.team.dev", 321, time.Now()); err != nil {
		t.Fatalf("SaveCounter: %v", err)
	}
	if err := s.db.Close(); err != nil {
		t.Fatalf("close db: %v", err)
	}
	if err := s.flush(); err == nil {
		t.Fatalf("flush against closed db should error")
	}
	s.mu.Lock()
	_, queued := s.dirty["org.team.dev"]
	s.mu.Unlock()
	if !queued {
		t.Fatalf("record was dropped instead of re-queued after a transient flush error")
	}

	// Half 2: a healthy flush commits and the value survives a reload.
	s2 := newTestStore(t)
	defer s2.Close()
	ws := time.Now().UTC().Truncate(time.Second)
	if err := s2.SaveCounter("org.team.dev", 321, ws); err != nil {
		t.Fatalf("SaveCounter (s2): %v", err)
	}
	if err := s2.flush(); err != nil {
		t.Fatalf("flush (s2): %v", err)
	}
	got, _, err := s2.LoadCounter("org.team.dev")
	if err != nil || got != 321 {
		t.Fatalf("retry did not land: LoadCounter = %d (err=%v), want 321", got, err)
	}
}
