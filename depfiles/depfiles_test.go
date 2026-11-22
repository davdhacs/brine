package depfiles

import (
	"os"
	"path/filepath"
	"sort"
	"testing"
)

func write(t *testing.T, path, content string) {
	t.Helper()
	os.MkdirAll(filepath.Dir(path), 0o755)
	os.WriteFile(path, []byte(content), 0o644)
}

func TestParseBasic(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "foo.d"), "foo.o: foo.c header.h\n")

	deps := Parse(filepath.Join(dir, "foo.d"), dir)
	if len(deps) != 2 {
		t.Fatalf("expected 2 deps, got %v", deps)
	}
	sort.Strings(deps)
	if deps[0] != "foo.c" || deps[1] != "header.h" {
		t.Fatalf("got %v", deps)
	}
}

func TestParseSystemHeaders(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "foo.d"),
		"foo.o: foo.c /usr/include/stdio.h /usr/include/math.h local.h\n")

	deps := Parse(filepath.Join(dir, "foo.d"), dir)
	if len(deps) != 2 {
		t.Fatalf("system headers must be excluded, got %v", deps)
	}
}

func TestParseLineContinuation(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "foo.d"),
		"foo.o: foo.c \\\n  header1.h \\\n  header2.h\n")

	deps := Parse(filepath.Join(dir, "foo.d"), dir)
	if len(deps) != 3 {
		t.Fatalf("line continuations must be joined, got %v", deps)
	}
}

func TestParseMultipleTargets(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "multi.d"),
		"foo.o bar.o: common.h foo.c bar.c\n")

	deps := Parse(filepath.Join(dir, "multi.d"), dir)
	if len(deps) != 3 {
		t.Fatalf("multiple targets, got %v", deps)
	}
}

func TestParseRelativePaths(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "src"), 0o755)
	os.MkdirAll(filepath.Join(dir, "include"), 0o755)
	write(t, filepath.Join(dir, "build", "foo.d"),
		"foo.o: ../src/foo.c ../include/header.h\n")

	deps := Parse(filepath.Join(dir, "build", "foo.d"), dir)
	sort.Strings(deps)
	if len(deps) != 2 {
		t.Fatalf("relative paths, got %v", deps)
	}
	if deps[0] != "include/header.h" || deps[1] != "src/foo.c" {
		t.Fatalf("resolved: %v", deps)
	}
}

func TestParseEmptyAndComments(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "empty.d"), "# comment\n\n")
	if deps := Parse(filepath.Join(dir, "empty.d"), dir); len(deps) != 0 {
		t.Fatalf("empty, got %v", deps)
	}
}

func TestFind(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "build", "foo.d"), "x: y\n")
	write(t, filepath.Join(dir, "build", "bar.d"), "x: y\n")
	write(t, filepath.Join(dir, "src", "main.c"), "int main(){}")
	write(t, filepath.Join(dir, ".git", "ignored.d"), "x: y\n") // must be skipped

	found := Find(dir)
	if len(found) != 2 {
		t.Fatalf("expected 2 .d files, got %v", found)
	}
}

func TestClosure(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "a.d"), "a.o: a.c common.h")
	write(t, filepath.Join(dir, "b.d"), "b.o: b.c common.h")
	write(t, filepath.Join(dir, "c.d"), "c.o: c.c a.c") // overlap with a.d

	closure := Closure(dir)
	sort.Strings(closure)
	// union: a.c, b.c, c.c, common.h = 4 unique
	if len(closure) != 4 {
		t.Fatalf("closure should deduplicate, got %v", closure)
	}
}
