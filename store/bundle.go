// Receipt bundles: small output blobs packed into single
// content-addressed archive entries for transport, amortizing the
// per-entry request cost (the same principle as the gocache bundles
// that solved the ~170 req/s request-bound cold fetch). Found live on
// the E9b fork replay: a 125-blob dep receipt (122 headers of 1-6KB)
// meant 125 twirp creates + PUTs + finalizes on upload and 125 remote
// fetches on restore — the request count, not bandwidth, dominated.
//
// Design:
//   - Files under bundleFileThreshold (1MB) pack into bundles of at
//     most bundleMaxBytes (64MB); larger files stay individual blobs
//     (streaming restores, selective fetch).
//   - A bundle is JUST another content-addressed blob in the store —
//     backend-agnostic (Azure and GHA get the same behavior).
//   - The manifest keeps per-file BlobRefs (restore semantics,
//     selective restore, verification unchanged) PLUS a Bundles index
//     mapping bundle hash → member hashes.
//   - Deterministic pack (members sorted by hash, no timestamps):
//     identical member sets produce identical bundle hashes, so the
//     dep receipt uploaded once dedups across every commit.
//   - Restore: fetch each needed bundle once (streaming, whole-bundle
//     hash verified), unpack members into the local store (each member
//     re-verified against its own hash — defense in depth), then the
//     normal per-file restore proceeds from local blobs.
//   - Backward compatible: manifests without Bundles behave exactly
//     as before (per-blob remote fetch).
package store

import (
	"bytes"
	"sync"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
)

const (
	bundleFileThreshold = 1 << 20 // files < 1MB pack into bundles
	bundleMaxBytes      = 64 << 20
	bundleMagic         = "BRINBND1"
)

// BundleRef describes one packed bundle: its content hash, size, and
// the member blob hashes inside.
type BundleRef struct {
	Hash    string   `json:"hash"`
	Size    int64    `json:"size"`
	Members []string `json:"members"`
}

// packBundle encodes members (hash → content) deterministically:
// magic, count, then per member sorted by hash: hash, size, data.
func packBundle(members map[string][]byte) ([]byte, string) {
	hashes := make([]string, 0, len(members))
	for h := range members {
		hashes = append(hashes, h)
	}
	sort.Strings(hashes)

	var buf bytes.Buffer
	buf.WriteString(bundleMagic)
	binary.Write(&buf, binary.LittleEndian, uint32(len(hashes)))
	for _, h := range hashes {
		data := members[h]
		raw, _ := hex.DecodeString(h)
		buf.Write(raw)
		binary.Write(&buf, binary.LittleEndian, uint64(len(data)))
		buf.Write(data)
	}
	sum := sha256.Sum256(buf.Bytes())
	return buf.Bytes(), hex.EncodeToString(sum[:])
}

// unpackBundle decodes a packed bundle and writes each member into
// the local blob store, verifying the member's own content hash.
func (s *Store) unpackBundle(b BundleRef) error {
	f, err := os.Open(s.blobPath(b.Hash))
	if err != nil {
		return fmt.Errorf("bundle open %s: %w", b.Hash[:12], err)
	}
	defer f.Close()
	r := io.Reader(f)
	magic := make([]byte, len(bundleMagic))
	if _, err := io.ReadFull(r, magic); err != nil || string(magic) != bundleMagic {
		return fmt.Errorf("bundle %s: bad magic", b.Hash[:12])
	}
	var count uint32
	if err := binary.Read(r, binary.LittleEndian, &count); err != nil {
		return fmt.Errorf("bundle %s: count: %w", b.Hash[:12], err)
	}
	for i := uint32(0); i < count; i++ {
		raw := make([]byte, 32)
		if _, err := io.ReadFull(r, raw); err != nil {
			return fmt.Errorf("bundle %s: member hash: %w", b.Hash[:12], err)
		}
		h := hex.EncodeToString(raw)
		var size uint64
		if err := binary.Read(r, binary.LittleEndian, &size); err != nil {
			return fmt.Errorf("bundle %s: member size: %w", b.Hash[:12], err)
		}
		data := make([]byte, size)
		if _, err := io.ReadFull(r, data); err != nil {
			return fmt.Errorf("bundle %s: member data: %w", b.Hash[:12], err)
		}
		// member verification: the blob key IS the content hash
		sum := sha256.Sum256(data)
		if hex.EncodeToString(sum[:]) != h {
			return fmt.Errorf("bundle %s: INTEGRITY FAILURE member %s — refusing to write", b.Hash[:12], h[:12])
		}
		p := s.blobPath(h)
		if _, err := os.Stat(p); err == nil {
			continue // already local (concurrent unpack / prior fetch)
		}
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, data, 0o644); err != nil {
			return err
		}
	}
	return nil
}

// prepareBundles packs the manifest's small blobs into bundles,
// stores the packed bytes locally, and records the index on the
// manifest. Idempotent (skips when already prepared). Called before
// the manifest is marshaled so both local and remote copies carry
// the bundle index.
func (s *Store) prepareBundles(m *Manifest) error {
	if len(m.Bundles) > 0 || len(m.Blobs) < 2 {
		return nil
	}
	var smalls []BlobRef
	for _, ref := range m.Blobs {
		if ref.Size > 0 && ref.Size < bundleFileThreshold {
			smalls = append(smalls, ref)
		}
	}
	if len(smalls) < 2 {
		return nil // a single small file gains nothing from bundling
	}
	sort.Slice(smalls, func(i, j int) bool { return smalls[i].Hash < smalls[j].Hash })

	var cur map[string][]byte
	var curMembers []string
	var curBytes int64
	flush := func() error {
		if len(cur) == 0 {
			return nil
		}
		packed, hash := packBundle(cur)
		if _, _, err := s.PutBlob(bytes.NewReader(packed)); err != nil {
			return err
		}
		m.Bundles = append(m.Bundles, BundleRef{Hash: hash, Size: int64(len(packed)), Members: curMembers})
		cur = nil
		curMembers = nil
		curBytes = 0
		return nil
	}
	for _, ref := range smalls {
		if curBytes+ref.Size > bundleMaxBytes {
			if err := flush(); err != nil {
				return err
			}
		}
		if cur == nil {
			cur = map[string][]byte{}
		}
		data, err := os.ReadFile(s.blobPath(ref.Hash))
		if err != nil {
			return fmt.Errorf("bundle read %s: %w", ref.Hash[:12], err)
		}
		cur[ref.Hash] = data
		curMembers = append(curMembers, ref.Hash)
		curBytes += ref.Size
	}
	return flush()
}

// fetchBundles brings bundle members into the local store before a
// restore: for each bundle with any member missing locally, fetch the
// bundle once (streaming, hash-verified) and unpack. After this, the
// per-blob restore is all local hits.
func (s *Store) fetchBundles(m *Manifest) error {
	if !s.hasRemote() {
		return nil
	}
	// Bundles fetch CONCURRENTLY (bounded): the kernel allmodconfig
	// receipt packs ~12 GB into ~190 bundles; the serial loop fetched
	// one at a time (fetch then unpack, no overlap) and dominated the
	// restore — measured 522-924s hits whose data moves in seconds on
	// SSD (E1 kernel experiment).
	var wg sync.WaitGroup
	sem := make(chan struct{}, 8)
	errCh := make(chan error, len(m.Bundles))
	for _, b := range m.Bundles {
		need := false
		for _, h := range b.Members {
			if _, err := os.Stat(s.blobPath(h)); err != nil {
				need = true
				break
			}
		}
		if !need {
			continue
		}
		wg.Add(1)
		sem <- struct{}{}
		go func(b BundleRef) {
			defer wg.Done()
			defer func() { <-sem }()
			if err := s.remoteFetchBlob(b.Hash); err != nil {
				errCh <- fmt.Errorf("bundle fetch: %w", err)
				return
			}
			if err := s.unpackBundle(b); err != nil {
				errCh <- err
			}
		}(b)
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		return err
	}
	return nil
}

// transportRefs returns the blobs to upload: bundle entries INSTEAD
// of their members (the request savings), plus unbundled blobs.
func transportRefs(m *Manifest) []BlobRef {
	bundled := map[string]bool{}
	var refs []BlobRef
	for _, b := range m.Bundles {
		refs = append(refs, BlobRef{Hash: b.Hash, Size: b.Size})
		for _, h := range b.Members {
			bundled[h] = true
		}
	}
	for _, ref := range m.Blobs {
		if !bundled[ref.Hash] {
			refs = append(refs, ref)
		}
	}
	return refs
}
