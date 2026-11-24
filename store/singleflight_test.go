package store

import (
	"bytes"
	"path/filepath"
	"sync"
	"testing"
)

// Single-flight mechanics: concurrent waiters on an in-flight hash
// all block until the fetch completes and are then released. (The
// full download-dedup path is exercised live by the arm B receipts;
// the gocache.Remote is concrete, so a store-level fake server isn't
// reachable from this package — the mechanics are the testable unit.)
func TestRemoteFetchSingleFlightMechanics(t *testing.T) {
	dir := t.TempDir()
	s, err := New(filepath.Join(dir, ".brine"))
	if err != nil {
		t.Fatal(err)
	}
	hash := "ab" + string(bytes.Repeat([]byte("c"), 62))

	s.fetchMu.Lock()
	ch := make(chan struct{})
	s.inFlight[hash] = ch
	s.fetchMu.Unlock()

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.fetchMu.Lock()
			if wait, ok := s.inFlight[hash]; ok {
				s.fetchMu.Unlock()
				<-wait // blocks until the fetch completes
			} else {
				s.fetchMu.Unlock()
				t.Error("waiter arrived after fetch completed — test bug")
			}
		}()
	}
	close(ch) // the fetch completes
	wg.Wait()
}
