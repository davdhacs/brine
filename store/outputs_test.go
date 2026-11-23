package store

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// snapshotFixture: a dir with an INPUT (pre-existing), a user file
// edited before the build, an output created after builtAt, a
// build-created hidden dir with state, and an empty output dir.
func snapshotFixture(t *testing.T) (dir string, builtAt int64) {
	t.Helper()
	dir = t.TempDir()
	os.WriteFile(filepath.Join(dir, "input.go"), []byte("package main"), 0o644)
	os.WriteFile(filepath.Join(dir, "README.md"), []byte("user edit before build"), 0o644)

	time.Sleep(50 * time.Millisecond) // pre-existing files strictly older
	builtAt = time.Now().UnixNano()
	time.Sleep(50 * time.Millisecond) // outputs strictly newer

	os.MkdirAll(filepath.Join(dir, "out"), 0o755)
	os.WriteFile(filepath.Join(dir, "out", "app.bin"), []byte("binary output"), 0o644)
	// cargo-style state in a build-created hidden dir
	os.MkdirAll(filepath.Join(dir, "out", ".fingerprint"), 0o755)
	os.WriteFile(filepath.Join(dir, "out", ".fingerprint", "state"), []byte("f1"), 0o644)
	// empty output dir (collector's container/libs)
	os.MkdirAll(filepath.Join(dir, "out", "libs"), 0o755)
	return
}

func TestSnapshotOutputsOutputOnly(t *testing.T) {
	dir, builtAt := snapshotFixture(t)
	s, _ := New(filepath.Join(dir, ".brine"))

	readSet := []string{"input.go"}
	m, err := s.SnapshotOutputs("k", dir, readSet, builtAt, nil)
	if err != nil {
		t.Fatal(err)
	}
	paths := blobPaths(m)
	want := map[string]bool{
		"out/app.bin":            true,
		"out/.fingerprint/state": true,  // build-created hidden dir: descended
		"README.md":              false, // edited BEFORE the build: not an output
		"input.go":               false, // input: never an output
	}
	for p, wantPresent := range want {
		if paths[p] != wantPresent {
			t.Errorf("blob %q: present=%v, want %v", p, paths[p], wantPresent)
		}
	}
	// Empty output dirs are recorded in OutputDirs (their absence must
	// not invalidate input-dir listings — the collector case).
	if !containsStr(m.OutputDirs, "out/libs") {
		t.Errorf("OutputDirs must record the empty output dir out/libs, got %v", m.OutputDirs)
	}
}

func TestRestoreVerifiesBlobIntegrity(t *testing.T) {
	dir, builtAt := snapshotFixture(t)
	s, _ := New(filepath.Join(dir, ".brine"))
	m, err := s.SnapshotOutputs("k", dir, []string{"input.go"}, builtAt, nil)
	if err != nil {
		t.Fatal(err)
	}

	// Corrupt one stored blob on disk (bit rot after the fact).
	var blobPath string
	for _, b := range m.Blobs {
		if b.Path == "out/app.bin" {
			blobPath = s.blobPath(b.Hash)
		}
	}
	if blobPath == "" {
		t.Fatal("app.bin blob missing")
	}
	data, _ := os.ReadFile(blobPath)
	data[0] ^= 0xFF
	os.WriteFile(blobPath, data, 0o644)

	// Restore MUST refuse (never serve wrong bytes).
	t.Setenv("BRINE_TRUST_LOCAL", "")
	if _, _, err := s.RestoreManifest(m, dir); err == nil {
		t.Fatal("corrupted local blob must make RestoreManifest fail")
	}

	// And with trust-local set (explicit opt-out), it proceeds.
	t.Setenv("BRINE_TRUST_LOCAL", "1")
	if _, _, err := s.RestoreManifest(m, dir); err != nil {
		t.Fatalf("trust-local restore should proceed: %v", err)
	}
}

func TestValidateIgnoresOutputDirListings(t *testing.T) {
	// The collector case: nested output roots (out/libs), wiped
	// outputs, path-vs-name keyspace, root-dir join.
	dir, builtAt := snapshotFixture(t)
	s, _ := New(filepath.Join(dir, ".brine"))
	// After filterBuildState, read-sets exclude output-tree entries
	// (fact case study: cargo fingerprint state is not an input).
	readSet := []string{"input.go"}
	m, err := s.SnapshotOutputs("k", dir, readSet, builtAt, nil)
	if err != nil {
		t.Fatal(err)
	}
	m.BuiltAt = builtAt // cmd sets these post-snapshot
	m.ReadSet = readSet
	outputRoots := map[string]bool{}
	for _, b := range m.Blobs {
		p := b.Path
		for {
			if i := lastIndexByte(p, '/'); i > 0 {
				p = p[:i]
				outputRoots[p] = true
			} else {
				break
			}
		}
	}
	for _, d := range m.OutputDirs {
		outputRoots[d] = true
	}
	m.InputHashes, m.DirListings = BuildProvenanceFiltered(dir, readSet, outputRoots)

	// Wipe ALL outputs (nested dirs, empty dirs, everything).
	os.RemoveAll(filepath.Join(dir, "out"))

	valid, reasons := s.Validate(m, dir)
	if !valid {
		t.Fatalf("wiping outputs must not invalidate: %v", reasons)
	}

	// But changing a real input must invalidate.
	os.WriteFile(filepath.Join(dir, "input.go"), []byte("package main // changed"), 0o644)
	valid, reasons = s.Validate(m, dir)
	if valid {
		t.Fatal("changed input must invalidate")
	}
	if len(reasons) == 0 || reasons[0][:8] != "changed:" {
		t.Fatalf("expected changed: input.go, got %v", reasons)
	}
}

func TestValidateRootDirListing(t *testing.T) {
	// The "." dir-listing case: the output root sits at the repo root;
	// the listing hash must exclude it via path.Join semantics
	// ("./out" keyspace bug).
	dir, builtAt := snapshotFixture(t)
	s, _ := New(filepath.Join(dir, ".brine"))
	readSet := []string{"input.go"}
	m, _ := s.SnapshotOutputs("k", dir, readSet, builtAt, nil)
	m.BuiltAt = builtAt
	m.ReadSet = readSet
	outputDirs := map[string]bool{"out": true}
	for _, d := range m.OutputDirs {
		outputDirs[d] = true
	}
	m.InputHashes, m.DirListings = BuildProvenanceFiltered(dir, readSet, outputDirs)
	if _, ok := m.DirListings["."]; !ok {
		t.Fatal("root listing expected (input.go lives there)")
	}

	os.RemoveAll(filepath.Join(dir, "out"))
	if valid, reasons := s.Validate(m, dir); !valid {
		t.Fatalf("root listing must exclude the wiped output root: %v", reasons)
	}
}

func TestLatestForClosure(t *testing.T) {
	dir, builtAt := snapshotFixture(t)
	s, _ := New(filepath.Join(dir, ".brine"))
	readSet := []string{"input.go"}

	m1, _ := s.SnapshotOutputs("k1", dir, readSet, builtAt, nil)
	m1.ClosureHash = "closureX"
	m1.Command = "build"
	s.PutManifest(m1)

	m2, _ := s.SnapshotOutputs("k2", dir, readSet, builtAt, nil)
	m2.ClosureHash = "closureX"
	m2.Command = "build"
	s.PutManifest(m2)

	got := s.LatestForClosure("closureX", "k1")
	if got == nil || got.Key != "k2" {
		t.Fatalf("expected latest k2 for closure, got %+v", got)
	}
	if s.LatestForClosure("closureY", "") != nil {
		t.Fatal("unknown closure must return nil")
	}
}

func blobPaths(m *Manifest) map[string]bool {
	out := map[string]bool{}
	for _, b := range m.Blobs {
		out[b.Path] = true
	}
	return out
}

func containsStr(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

func lastIndexByte(s string, b byte) int {
	for i := len(s) - 1; i >= 0; i-- {
		if s[i] == b {
			return i
		}
	}
	return -1
}
