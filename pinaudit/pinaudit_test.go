package pinaudit

import (
	"os"
	"path/filepath"
	"testing"
)

func write(t *testing.T, path, content string) {
	t.Helper()
	os.MkdirAll(filepath.Dir(path), 0o755)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestLockfileFindings(t *testing.T) {
	dir := t.TempDir()

	// cargo without a lockfile → missing-lockfile
	write(t, filepath.Join(dir, "Cargo.toml"), "[package]\nname = \"x\"\n")
	if f := lockfileFindings(dir); len(f) != 1 || f[0].Kind != "missing-lockfile" {
		t.Fatalf("expected missing-lockfile, got %+v", f)
	}

	// lockfile present but untracked → untracked-lockfile
	write(t, filepath.Join(dir, "Cargo.lock"), "")
	if f := lockfileFindings(dir); len(f) != 1 || f[0].Kind != "untracked-lockfile" {
		t.Fatalf("expected untracked-lockfile (no git repo), got %+v", f)
	}

	// go with deps and no go.sum → missing-lockfile
	dir2 := t.TempDir()
	write(t, filepath.Join(dir2, "go.mod"), "module x\n\ngo 1.26\n\nrequire (\n\tgithub.com/y/z v1.0.0\n)\n")
	if f := lockfileFindings(dir2); len(f) != 1 || f[0].Kind != "missing-lockfile" {
		t.Fatalf("expected go missing-lockfile, got %+v", f)
	}

	// go with no deps and no go.sum → clean
	dir3 := t.TempDir()
	write(t, filepath.Join(dir3, "go.mod"), "module x\n\ngo 1.26\n")
	if f := lockfileFindings(dir3); len(f) != 0 {
		t.Fatalf("expected clean, got %+v", f)
	}
}

func TestFloatingRefFindings(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "Dockerfile"), "FROM alpine:3\nRUN echo hi\n")
	if f := floatingRefFindings(dir); len(f) != 0 {
		t.Fatalf("pinned-ish tag should be clean, got %+v", f)
	}

	write(t, filepath.Join(dir, "Dockerfile"), "FROM golang:latest\n")
	if f := floatingRefFindings(dir); len(f) != 1 || f[0].Kind != "floating-ref" {
		t.Fatalf("expected floating-ref for :latest, got %+v", f)
	}

	// digest-pinned :latest is NOT floating
	write(t, filepath.Join(dir, "Dockerfile"), "FROM foo:latest@sha256:0123456789012345678901234567890123456789012345678901234567890123\n")
	if f := floatingRefFindings(dir); len(f) != 0 {
		t.Fatalf("digest-pinned must be clean, got %+v", f)
	}

	write(t, filepath.Join(dir, "run.sh"), "go install golang.org/x/tools/cmd/stringer@latest\n")
	if f := floatingRefFindings(dir); len(f) != 1 || f[0].Kind != "floating-ref" {
		t.Fatalf("expected floating-ref for @latest, got %+v", f)
	}
}

func TestAuditClean(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "Makefile"), "all:\n\techo pinned work\n")
	if f := Audit(dir); len(f) != 0 {
		t.Fatalf("expected clean, got %+v", f)
	}
}
