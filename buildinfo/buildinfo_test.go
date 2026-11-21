package buildinfo

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// buildTestBinary compiles a real go binary (optionally inside a git
// repo, optionally with ldflags) and returns its path. Skips when go
// is unavailable.
func buildTestBinary(t *testing.T, dir string, gitInit bool, ldflags string) string {
	t.Helper()
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go not available")
	}
	os.MkdirAll(filepath.Join(dir, "cmd", "app"), 0o755)
	os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module testapp\n\ngo 1.24\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "cmd", "app", "main.go"),
		[]byte("package main\n\nvar version string\n\nfunc main() { println(version) }\n"), 0o644)

	if gitInit {
		if _, err := exec.LookPath("git"); err != nil {
			t.Skip("git not available")
		}
		exec.Command("git", "-C", dir, "init", "-q").Run()
		exec.Command("git", "-C", dir, "-c", "user.email=t@t", "-c", "user.name=t",
			"add", "-A").Run()
		exec.Command("git", "-C", dir, "-c", "user.email=t@t", "-c", "user.name=t",
			"commit", "-q", "-m", "init").Run()
	}

	out := filepath.Join(dir, "app")
	args := []string{"build", "-o", out}
	if ldflags != "" {
		args = append(args, "-ldflags", ldflags)
	}
	args = append(args, "./cmd/app")
	cmd := exec.Command("go", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GOFLAGS=-buildvcs="+map[bool]string{true: "auto", false: "false"}[gitInit])
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("go build failed: %v: %s", err, out)
	}
	return out
}

func TestInspectVCSStamp(t *testing.T) {
	dir := t.TempDir()
	bin := buildTestBinary(t, dir, true, "")
	info, err := Inspect(bin)
	if err != nil || info == nil {
		t.Fatalf("inspect: %v %v", err, info)
	}
	if info.VCSRevision == "" {
		t.Fatal("git-init build must carry vcs.revision (buildvcs=auto)")
	}
	// And the audit must flag it (memoization-blind input).
	findings := Audit(info, "go build -o app ./cmd/app")
	found := false
	for _, f := range findings {
		if f.Kind == "vcs-stamp" {
			found = true
		}
	}
	if !found {
		t.Fatalf("vcs-stamp finding expected, got %+v", findings)
	}
}

func TestInspectNoVCS(t *testing.T) {
	dir := t.TempDir()
	bin := buildTestBinary(t, dir, false, "")
	info, err := Inspect(bin)
	if err != nil || info == nil {
		t.Fatalf("inspect: %v %v", err, info)
	}
	if info.VCSRevision != "" {
		t.Fatalf("buildvcs=false must not stamp, got revision %s", info.VCSRevision)
	}
}

func TestAuditLdflagsExternal(t *testing.T) {
	dir := t.TempDir()
	bin := buildTestBinary(t, dir, false, "-X main.version=1.2.3")
	info, _ := Inspect(bin)
	if info == nil || info.Ldflags == "" {
		t.Fatal("ldflags must be recorded in buildinfo")
	}
	// In the memoized command → keyed → no finding.
	for _, f := range Audit(info, "go build -ldflags -X main.version=1.2.3 ./cmd/app") {
		if f.Kind == "ldflags-external" {
			t.Fatal("ldflags present in command must not be flagged")
		}
	}
	// Absent from the command → flagged (sourced elsewhere).
	found := false
	for _, f := range Audit(info, "go build ./cmd/app") {
		if f.Kind == "ldflags-external" {
			found = true
		}
	}
	if !found {
		t.Fatal("external ldflags must be flagged")
	}
}

func TestVerify(t *testing.T) {
	dir := t.TempDir()
	bin := buildTestBinary(t, dir, false, "")
	a, _ := Inspect(bin)
	if ok, _ := Verify(a, a); !ok {
		t.Fatal("self-verify must pass")
	}
	// Different ldflags → mismatch.
	b := &Info{Ldflags: "-X other=1"}
	if ok, m := Verify(a, b); ok || !strings.Contains(m, "ldflags") {
		t.Fatalf("expected ldflags mismatch, got ok=%v m=%q", ok, m)
	}
	// nil-safe.
	if ok, _ := Verify(nil, a); !ok {
		t.Fatal("nil recorded must verify trivially")
	}
}

func TestInspectNonGoBinary(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "data.bin")
	os.WriteFile(p, []byte("not a go binary at all"), 0o644)
	info, err := Inspect(p)
	if err != nil {
		t.Fatalf("non-go file must not error: %v", err)
	}
	if info != nil {
		t.Fatal("non-go file must return nil info")
	}
}
