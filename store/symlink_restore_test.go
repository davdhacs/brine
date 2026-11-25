package store

import (
	"os"
	"path/filepath"
	"testing"
)

// TestSymlinkOutputsSurviveRestore: symlinks in the output tree must be
// recreated as symlinks (not regular copies, not silently dropped). The
// kernel make O= build creates build/source -> ..; os.Open follows it,
// the directory read fails, and without the symlink branch the link
// vanishes from the receipt — after restore, make re-runs prepare and
// the warm no-op becomes a partial rebuild (found live in E1).
func TestSymlinkOutputsSurviveRestore(t *testing.T) {
	dir := t.TempDir()
	st, err := New(filepath.Join(dir, ".brine"))
	if err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "build")
	if err := os.MkdirAll(out, 0o755); err != nil {
		t.Fatal(err)
	}
	// the kernel pattern: relative dir symlink
	if err := os.Symlink("..", filepath.Join(out, "source")); err != nil {
		t.Fatal(err)
	}
	// plus a file symlink and an executable
	if err := os.Symlink("app.bin", filepath.Join(out, "app")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(out, "app.bin"), []byte("binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	// hidden outputs: the kernel build's .config and .foo.o.cmd depfiles
	// must be captured (a blanket dot-skip dropped them; without .config
	// a restored tree made make re-run config and fail)
	if err := os.WriteFile(filepath.Join(out, ".config"), []byte("CONFIG_X=y"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(out, ".app.bin.o.cmd"), []byte("cmdline"), 0o644); err != nil {
		t.Fatal(err)
	}

	m, err := st.SnapshotOutputs("k", out, nil, 0, []string{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	found := map[string]bool{}
	for _, b := range m.Blobs {
		found[b.Path] = true
	}
	if !found["source"] || !found["app"] {
		t.Fatalf("symlinks missing from receipt: %v", found)
	}
	if !found[".config"] || !found[".app.bin.o.cmd"] {
		t.Fatalf("hidden outputs missing from receipt: %v", found)
	}

	// wipe and restore into a fresh dir
	fresh := filepath.Join(dir, "build2")
	if err := os.MkdirAll(fresh, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.RestoreManifest(m, fresh); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Lstat(filepath.Join(fresh, "source"))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode()&os.ModeSymlink == 0 {
		t.Fatal("build/source restored as a regular file, not a symlink")
	}
	if got, _ := os.Readlink(filepath.Join(fresh, "source")); got != ".." {
		t.Fatalf("source target = %q, want ..", got)
	}
	fi, err = os.Lstat(filepath.Join(fresh, "app"))
	if err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("app not restored as symlink: %v %v", fi, err)
	}
	if cfg, err := os.ReadFile(filepath.Join(fresh, ".config")); err != nil || string(cfg) != "CONFIG_X=y" {
		t.Fatalf(".config restored as %q, %v", cfg, err)
	}
	bin := filepath.Join(fresh, "app.bin")
	fi, err = os.Stat(bin)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode()&0o111 == 0 {
		t.Fatal("exec bit lost on restore")
	}
}
