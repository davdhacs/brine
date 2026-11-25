package store

import (
	"encoding/json"
	"sync"
	"fmt"
	"os"

	"github.com/davdhacs/brine/gocache"
)

// Remote tier for the brine blob store: manifests and output blobs in
// Azure, namespaced under store/ (the gocache proxy uses a/, d/, m/ —
// no collision). Level-1 receipts become cross-machine: a fresh runner
// computes the input key, fetches the manifest, restores the outputs —
// the build tool never runs.
//
// Integrity: blob keys ARE content hashes; every remote-sourced blob
// is re-verified on arrival (the CA-work lesson — a corrupt or
// poisoned blob must degrade to a miss, never serve wrong bytes).

const remoteManifestPrefix = "store/m/"
const remoteBlobPrefix = "store/b/"

// SetRemote attaches a cloud backend to the store.
func (s *Store) SetRemote(r *gocache.Remote) {
	s.remote = r
}

func (s *Store) hasRemote() bool { return s.remote != nil }

// remoteUploadBlobs uploads receipt blobs concurrently (bounded 16).
// Sequential uploads measured ~60s on the arm B dep receipt (125
// blobs × ~0.5s per twirp create+PUT+finalize) — the upload must not
// be the tail of a build that just finished. Error semantics match
// the sequential form: any failure aborts with the first error.
func (s *Store) remoteUploadBlobs(blobs []BlobRef) error {
	var wg sync.WaitGroup
	sem := make(chan struct{}, 16)
	errCh := make(chan error, len(blobs))
	for _, ref := range blobs {
		wg.Add(1)
		sem <- struct{}{}
		go func(ref BlobRef) {
			defer wg.Done()
			defer func() { <-sem }()
			data, err := s.GetBlob(ref.Hash)
			if err != nil {
				return // blob pruned locally; manifest still references it
			}
			if _, err := s.remote.PutIfNotExistPub(remoteBlobPrefix+ref.Hash, data); err != nil {
				errCh <- fmt.Errorf("blob upload %s: %w", ref.Hash[:12], err)
			}
		}(ref)
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		return err
	}
	return nil
}

// remoteUploadManifest pushes the manifest JSON and every referenced
// blob (content-addressed, If-None-Match dedup). Small blobs travel
// as bundles (transportRefs) — the request-count amortization.
func (s *Store) remoteUploadManifest(m *Manifest) error {
	if err := s.remoteUploadBlobs(transportRefs(m)); err != nil {
		return err
	}
	data, err := json.Marshal(m)
	if err != nil {
		return err
	}
	return s.remote.PutPub(remoteManifestPrefix+m.Key, data)
}

// remoteFetchManifest retrieves a manifest by key, or nil when absent.
func (s *Store) remoteFetchManifest(key string) (*Manifest, error) {
	data, err := s.remote.Get(remoteManifestPrefix + key)
	if err != nil {
		return nil, err
	}
	if data == nil {
		return nil, nil
	}
	var m Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("remote manifest parse %s: %w", key, err)
	}
	return &m, nil
}

// EnsureRemote pushes a locally-known receipt to the shared store when
// it is absent there. Hit paths never upload — a receipt built while the
// remote was down (or its auth broken) stays local forever without this,
// invisible to other machines. Existence-checked per blob so a hit does
// not re-transfer 100MB+ bodies that are already shared.
func (s *Store) EnsureRemote(m *Manifest) error {
	if !s.hasRemote() || m == nil {
		return nil
	}
	var wg sync.WaitGroup
	sem := make(chan struct{}, 16)
	// Dedup by hash: outputs with identical content (2569 empty files
	// in a kernel build all share the empty-content hash) otherwise pay
	// one serial existence-check round trip EACH — measured 197s on a
	// commit-receipt hit whose restore proper took seconds (E1 kernel
	// experiment).
	seen := map[string]bool{}
	var refs []BlobRef
	for _, ref := range transportRefs(m) {
		if !seen[ref.Hash] {
			seen[ref.Hash] = true
			refs = append(refs, ref)
		}
	}
	errCh := make(chan error, len(refs))
	// existence checks run concurrently too (bounded): distinct refs
	// each cost one round trip and the serial loop dominated restores
	// of manifests with many mid-size unbundled blobs
	exists := make([]bool, len(refs))
	var ewg sync.WaitGroup
	esem := make(chan struct{}, 16)
	for i, ref := range refs {
		ewg.Add(1)
		esem <- struct{}{}
		go func(i int, hash string) {
			defer ewg.Done()
			defer func() { <-esem }()
			if ok, err := s.remote.ExistsPub(remoteBlobPrefix + hash); err == nil {
				exists[i] = ok
			}
		}(i, ref.Hash)
	}
	ewg.Wait()
	for i, ref := range refs {
		if exists[i] {
			continue
		}
		wg.Add(1)
		sem <- struct{}{}
		go func(ref BlobRef) {
			defer wg.Done()
			defer func() { <-sem }()
			data, err := s.GetBlob(ref.Hash)
			if err != nil {
				return // blob pruned locally; manifest still references it
			}
			if _, err := s.remote.PutIfNotExistPub(remoteBlobPrefix+ref.Hash, data); err != nil {
				errCh <- fmt.Errorf("blob upload %s: %w", ref.Hash[:12], err)
			}
		}(ref)
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		return err
	}
	if exists, err := s.remote.ExistsPub(remoteManifestPrefix + m.Key); err == nil && exists {
		return nil
	}
	data, err := json.Marshal(m)
	if err != nil {
		return err
	}
	return s.remote.PutPub(remoteManifestPrefix+m.Key, data)
}

// remoteFetchBlob pulls a blob into the local store, verifying its
// content hash (the key) before accepting it. Streams to disk with stall
// detection — receipt blobs are routinely 100MB+ and a fixed client
// timeout kills legitimate transfers (masked, before this fix, as "blob
// not found").
func (s *Store) remoteFetchBlob(hash string) error {
	// Check the file directly — calling GetBlob here would recurse
	// (GetBlob calls remoteFetchBlob on local miss).
	if _, err := os.Stat(s.blobPath(hash)); err == nil {
		return nil // already local
	}
	// single-flight: one download per hash; concurrent callers wait
	// for the in-flight fetch instead of racing a second download
	s.fetchMu.Lock()
	if ch, ok := s.inFlight[hash]; ok {
		s.fetchMu.Unlock()
		<-ch
		if _, err := os.Stat(s.blobPath(hash)); err == nil {
			return nil
		}
		return fmt.Errorf("blob fetch %s: in-flight fetch did not produce the blob", hash[:12])
	}
	ch := make(chan struct{})
	s.inFlight[hash] = ch
	s.fetchMu.Unlock()
	defer func() {
		s.fetchMu.Lock()
		delete(s.inFlight, hash)
		s.fetchMu.Unlock()
		close(ch) // wake waiters
	}()
	// stream to disk with stall detection: receipt blobs are 100MB+
	// and a fixed client timeout kills legitimate transfers on modest
	// links (was masked as "blob not found" — the prometheus 148MB
	// binary timed out at exactly the 30s cap while sitting in the store)
	if err := s.remote.GetStreamingRaw(remoteBlobPrefix+hash, hash, s.blobPath(hash)); err != nil {
		return fmt.Errorf("blob fetch %s: %w", hash[:12], err)
	}
	s.markBlob(hash)
	return nil
}
