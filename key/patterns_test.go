package key

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestGoPatterns(t *testing.T) {
	cases := []struct {
		args []string
		want []string // nil = not a go build command
	}{
		{[]string{"go", "build", "./..."}, []string{"./..."}},
		{[]string{"go", "build", "-o", "bin/app", "."}, []string{"."}}, // -o value skipped
		{[]string{"go", "build", "-ldflags", "-X x=y", "./cmd/a"}, []string{"./cmd/a"}},
		{[]string{"go", "build", "-ldflags", "-X x=y", "./cmd/a", "./cmd/b"}, []string{"./cmd/a", "./cmd/b"}},
		{[]string{"go", "test", "-run", "X", "./key/...", "./store/..."}, []string{"./key/...", "./store/..."}},
		{[]string{"go", "vet"}, []string{"."}}, // no pattern → cwd
		{[]string{"go", "env", "GOOS"}, nil},   // not a compile command
		{[]string{"cargo", "build"}, nil},
		{[]string{"go"}, nil},
	}
	for _, c := range cases {
		got := GoPatterns(c.args)
		if len(got) != len(c.want) {
			t.Errorf("GoPatterns(%v) = %v, want %v", c.args, got, c.want)
			continue
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Errorf("GoPatterns(%v)[%d] = %q, want %q", c.args, i, got[i], c.want[i])
			}
		}
	}
}

func TestHashInputGitlink(t *testing.T) {
	// A git repo with a submodule: the gitlink pseudo-input must hash
	// via the submodule HEAD, not a file read.
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	root := t.TempDir()
	sub := filepath.Join(root, "third_party")
	os.MkdirAll(sub, 0o755)

	for _, d := range []string{root, sub} {
		exec.Command("git", "-C", d, "init", "-q").Run()
		exec.Command("git", "-C", d, "-c", "user.email=t@t", "-c", "user.name=t", "add", "-A").Run()
		exec.Command("git", "-C", d, "-c", "user.email=t@t", "-c", "user.name=t",
			"commit", "-q", "-m", "x", "--allow-empty").Run()
	}
	// Wire the submodule manually (git submodule add needs a remote).
	head, _ := exec.Command("git", "-C", sub, "rev-parse", "HEAD").Output()
	gitmodules := filepath.Join(root, ".gitmodules")
	os.WriteFile(gitmodules, []byte("[submodule \"third_party\"]\n\tpath = third_party\n\turl = nowhere\n"), 0o644)
	// Simulate the gitlink pseudo-entry as gitTrackedFiles produces it.
	entry := "third_party" + GitlinkMarker + string(head[:12])

	v1, ok := HashInput(root, entry)
	if !ok || v1 != string(head[:len(head)-1]) {
		t.Fatalf("gitlink hash = %q ok=%v, want submodule HEAD %q", v1, ok, string(head[:12]))
	}
	// A plain missing file must fail (not silently hash empty).
	if _, ok := HashInput(root, "no/such/file"); ok {
		t.Fatal("missing file must not hash")
	}
}

func TestClosureHashStability(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "a.go"), []byte("package a"), 0o644)
	os.WriteFile(filepath.Join(dir, "b.go"), []byte("package b"), 0o644)

	h1 := ClosureHash(dir, []string{"a.go", "b.go"})
	h2 := ClosureHash(dir, []string{"b.go", "a.go"}) // order-independent
	if h1 != h2 {
		t.Fatal("closure hash must be order-independent")
	}

	os.WriteFile(filepath.Join(dir, "b.go"), []byte("package b // changed"), 0o644)
	if h3 := ClosureHash(dir, []string{"a.go", "b.go"}); h3 == h1 {
		t.Fatal("content change must change the closure hash")
	}
}

func TestFromFilesSkipsUnreadable(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "a.go"), []byte("x"), 0o644)
	// One readable, one missing: key still computed, stable.
	k1 := FromFiles(dir, []string{"a.go", "missing.go"}, "cmd")
	k2 := FromFiles(dir, []string{"a.go", "missing.go"}, "cmd")
	if k1 == "" || k1 != k2 {
		t.Fatalf("key stability: %q vs %q", k1, k2)
	}
	// Missing files contribute nothing: a set plus a missing file
	// hashes the same as the set without it.
	if k3 := FromFiles(dir, []string{"a.go"}, "cmd"); k3 != k1 {
		t.Fatal("missing file must not affect the key")
	}
	// Content changes do.
	os.WriteFile(filepath.Join(dir, "a.go"), []byte("y"), 0o644)
	if k4 := FromFiles(dir, []string{"a.go", "missing.go"}, "cmd"); k4 == k1 {
		t.Fatal("content change must change the key")
	}
}
