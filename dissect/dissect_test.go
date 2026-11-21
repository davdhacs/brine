package dissect

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestDiffIdenticalFiles(t *testing.T) {
	dir := t.TempDir()
	a, b := filepath.Join(dir, "a"), filepath.Join(dir, "b")
	os.WriteFile(a, []byte("same bytes"), 0o644)
	os.WriteFile(b, []byte("same bytes"), 0o644)
	rep, err := Diff(a, b)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Sections) != 0 || len(rep.LikelyCauses) != 0 {
		t.Fatalf("identical files: %+v", rep)
	}
}

func TestDiffDifferingRawFiles(t *testing.T) {
	dir := t.TempDir()
	a, b := filepath.Join(dir, "a"), filepath.Join(dir, "b")
	os.WriteFile(a, []byte("same prefix A"), 0o644)
	os.WriteFile(b, []byte("same prefix B"), 0o644)
	rep, err := Diff(a, b)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Format != "raw" || len(rep.LikelyCauses) == 0 {
		t.Fatalf("expected raw diff with a cause, got %+v", rep)
	}
}

func TestDiffSelfBinary(t *testing.T) {
	// Same binary twice: no differing sections (also an ELF/Mach-O
	// parse smoke test on this platform).
	dir := t.TempDir()
	src, err := os.Executable()
	if err != nil {
		t.Skip("no executable path")
	}
	data, err := os.ReadFile(src)
	if err != nil {
		t.Skip("unreadable self")
	}
	os.WriteFile(filepath.Join(dir, "s1"), data, 0o755)
	os.WriteFile(filepath.Join(dir, "s2"), data, 0o755)
	rep, err := Diff(filepath.Join(dir, "s1"), filepath.Join(dir, "s2"))
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Sections) != 0 {
		t.Fatalf("self-diff should have no differing sections, got %d", len(rep.Sections))
	}
}

func TestARMembersDeterministicNaming(t *testing.T) {
	// The rust rlib pattern: identical member CONTENT under random
	// member NAMES must report ContentEquivalent (byte-different but
	// semantically identical). Fixtures built with the system ar.
	if _, err := exec.LookPath("ar"); err != nil {
		t.Skip("no ar command")
	}
	dir := t.TempDir()
	mkAr := func(path, memberName string) {
		named := filepath.Join(dir, memberName)
		os.WriteFile(named, []byte("package archive bytes"), 0o644)
		// rust writes BSD-format archives on every platform; GNU ar
		// defaults to its own format on Linux — ask for BSD to match.
		if err := exec.Command("ar", "rc", "--format=bsd", path, named).Run(); err != nil {
			if err2 := exec.Command("ar", "rc", path, named).Run(); err2 != nil {
				t.Fatal(err, err2)
			}
		}
	}
	mkAr(filepath.Join(dir, "a.rlib"), "crate1.0abcdefgh.rcgu.o")
	mkAr(filepath.Join(dir, "b.rlib"), "crate1.0ZYXWVUTS.rcgu.o")

	rep, err := Diff(filepath.Join(dir, "a.rlib"), filepath.Join(dir, "b.rlib"))
	if err != nil {
		t.Fatal(err)
	}
	if !rep.ContentEquivalent {
		t.Fatalf("ar with identical content, random names must be ContentEquivalent: %+v", rep)
	}
}
