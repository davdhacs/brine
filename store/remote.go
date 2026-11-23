package store

import (
	"encoding/json"
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

// remoteUploadManifest pushes the manifest JSON and every referenced
// blob (content-addressed, If-None-Match dedup).
func (s *Store) remoteUploadManifest(m *Manifest) error {
	for _, ref := range m.Blobs {
		data, err := s.GetBlob(ref.Hash)
		if err != nil {
			continue // blob pruned locally; manifest still references it
		}
		if _, err := s.remote.PutIfNotExistPub(remoteBlobPrefix+ref.Hash, data); err != nil {
			return fmt.Errorf("blob upload %s: %w", ref.Hash[:12], err)
		}
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
	for _, ref := range m.Blobs {
		if exists, err := s.remote.ExistsPub(remoteBlobPrefix + ref.Hash); err == nil && exists {
			continue
		}
		data, err := s.GetBlob(ref.Hash)
		if err != nil {
			continue // blob pruned locally; manifest still references it
		}
		if _, err := s.remote.PutIfNotExistPub(remoteBlobPrefix+ref.Hash, data); err != nil {
			return fmt.Errorf("blob upload %s: %w", ref.Hash[:12], err)
		}
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
