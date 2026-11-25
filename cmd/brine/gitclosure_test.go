package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// TestGitTrackedFilesSkipsBrineStore: brine's own store must never be
// closure input. A user who runs `git add -A` in a repo where .brine/
// exists commits the store itself; without the skip, the closure then
// self-references files that a fresh-machine run deletes (.brine/ is
// wiped to simulate a new CI runner), so the keyed receipt compares
// input hashes against missing files and mismatches forever — found
// live during the E1 kernel experiment setup on a remote worker.
func TestGitTrackedFilesSkipsBrineStore(t *testing.T) {
	dir := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "-q")
	run("config", "user.email", "t@t")
	run("config", "user.name", "t")

	for _, f := range []string{"in.txt", ".brine/blobs/aa/bb", ".brine/model.json", ".brine/manifests/x.json"} {
		p := filepath.Join(dir, f)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	run("add", "-A")
	run("commit", "-qm", "poisoned: store committed")

	files := gitTrackedFiles(dir)
	for _, f := range files {
		if len(f) >= 7 && f[:7] == ".brine/" {
			t.Fatalf("gitTrackedFiles returned store path %q — closure must never include .brine/ (got: %v)", f, files)
		}
	}
	found := false
	for _, f := range files {
		if f == "in.txt" {
			found = true
		}
	}
	if !found {
		t.Fatalf("in.txt missing from closure: %v", files)
	}
}
