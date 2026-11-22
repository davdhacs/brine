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

// writeDep writes a .d file AND creates the referenced source files.
func writeDep(t *testing.T, dir, depContent string, referenced ...string) {
	t.Helper()
	for _, f := range referenced {
		write(t, filepath.Join(dir, f), "// content\n")
	}
	write(t, filepath.Join(dir, "test.d"), depContent)
}

func TestParseBasic(t *testing.T) {
	dir := t.TempDir()
	writeDep(t, dir, "foo.o: foo.c header.h\n", "foo.c", "header.h")
	deps := Parse(filepath.Join(dir, "test.d"), dir)
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
	writeDep(t, dir, "foo.o: foo.c /usr/include/stdio.h local.h\n", "foo.c", "local.h")
	deps := Parse(filepath.Join(dir, "test.d"), dir)
	if len(deps) != 2 {
		t.Fatalf("system headers excluded, got %v", deps)
	}
}

func TestParseLineContinuation(t *testing.T) {
	dir := t.TempDir()
	writeDep(t, dir, "foo.o: foo.c \\\n  header1.h \\\n  header2.h\n",
		"foo.c", "header1.h", "header2.h")
	deps := Parse(filepath.Join(dir, "test.d"), dir)
	if len(deps) != 3 {
		t.Fatalf("continuations joined, got %v", deps)
	}
}

func TestParseMultipleTargets(t *testing.T) {
	dir := t.TempDir()
	writeDep(t, dir, "foo.o bar.o: common.h foo.c bar.c\n",
		"common.h", "foo.c", "bar.c")
	deps := Parse(filepath.Join(dir, "test.d"), dir)
	if len(deps) != 3 {
		t.Fatalf("multi-target, got %v", deps)
	}
}

func TestParseRelativePaths(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "src", "foo.c"), "// c\n")
	write(t, filepath.Join(dir, "include", "header.h"), "// h\n")
	write(t, filepath.Join(dir, "build", "test.d"),
		"foo.o: ../src/foo.c ../include/header.h\n")
	deps := Parse(filepath.Join(dir, "build", "test.d"), dir)
	sort.Strings(deps)
	if len(deps) != 2 {
		t.Fatalf("relative, got %v", deps)
	}
	if deps[0] != "include/header.h" || deps[1] != "src/foo.c" {
		t.Fatalf("resolved: %v", deps)
	}
}

func TestParseRustFormat(t *testing.T) {
	// cargo/rustc .d files use absolute paths for targets and
	// project-root-relative paths for deps
	dir := t.TempDir()
	write(t, filepath.Join(dir, "src", "main.rs"), "fn main() {}\n")
	write(t, filepath.Join(dir, "target", "debug", "deps", "test.d"),
		dir+"/target/debug/deps/myapp-abc123: src/main.rs\n\nsrc/main.rs:\n")
	deps := Parse(filepath.Join(dir, "target", "debug", "deps", "test.d"), dir)
	if len(deps) != 1 || deps[0] != "src/main.rs" {
		t.Fatalf("rust format, got %v", deps)
	}
}

func TestParseEmptyAndComments(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "test.d"), "# comment\n\n")
	if deps := Parse(filepath.Join(dir, "test.d"), dir); len(deps) != 0 {
		t.Fatalf("empty, got %v", deps)
	}
}

func TestFind(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "build", "foo.d"), "x: y\n")
	write(t, filepath.Join(dir, "build", "bar.d"), "x: y\n")
	write(t, filepath.Join(dir, "src", "main.c"), "int main(){}")
	write(t, filepath.Join(dir, ".git", "ignored.d"), "x: y\n")
	found := Find(dir)
	if len(found) != 2 {
		t.Fatalf("find, got %v", found)
	}
}

func TestClosure(t *testing.T) {
	dir := t.TempDir()
	for _, f := range []string{"a.c", "b.c", "c.c", "common.h"} {
		write(t, filepath.Join(dir, f), "// content\n")
	}
	write(t, filepath.Join(dir, "a.d"), "a.o: a.c common.h")
	write(t, filepath.Join(dir, "b.d"), "b.o: b.c common.h")
	write(t, filepath.Join(dir, "c.d"), "c.o: c.c a.c")
	closure := Closure(dir)
	sort.Strings(closure)
	if len(closure) != 4 {
		t.Fatalf("closure dedup, got %v", closure)
	}
}
