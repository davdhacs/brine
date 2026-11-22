// Package depfiles parses compiler dependency files (.d files) — the
// exact C/C++ input closure, emitted by gcc/clang with -MD.
//
// This is sensor #7 from the registry: the C/C++ equivalent of `go
// list -deps`. Any project using make/cmake with -MD in CFLAGS already
// generates these files during compilation; parsing them post-build
// gives the precise set of headers each object depends on.
//
// Format (make-style rule):
//
//	foo.o: foo.c header1.h header2.h /usr/include/stdio.h ...
//
// Multiple targets and line continuations (backslash-newline) are
// common. Paths may be relative or absolute; only project-relative
// paths are kept (system headers are not inputs).
package depfiles

import (
	"os"
	"path/filepath"
	"strings"
)

// Parse reads a .d file and returns the project-relative dependency
// paths (excluding the target itself and system headers). Paths in
// .d files are relative to the compiler's working directory — usually
// the .d file's parent (build dir) or the project root; both are tried.
func Parse(depFile, projectRoot string) []string {
	data, err := os.ReadFile(depFile)
	if err != nil {
		return nil
	}
	depDir := filepath.Dir(depFile)
	return parseWithBases(data, projectRoot, depDir)
}

// parseWithBases resolves relative paths against multiple bases
// (project root and the .d file's directory), keeping whichever lands
// inside the project.
func parseWithBases(data []byte, projectRoot, depDir string) []string {
	// Join line continuations
	text := strings.ReplaceAll(string(data), "\\\n", " ")

	var deps []string
	seen := map[string]bool{}

	resolve := func(dep string) (string, bool) {
		if filepath.IsAbs(dep) {
			abs := filepath.Clean(dep)
			rel, err := filepath.Rel(projectRoot, abs)
			if err != nil || strings.HasPrefix(rel, "..") {
				return "", false
			}
			return filepath.ToSlash(rel), true
		}
		// Try the .d file's directory first (cmake: ../src/foo.c from
		// build/), then the project root (make/cargo: src/foo.c from
		// root). Prefer the base where the file actually exists —
		// gcc paths are relative to the build dir, cargo/rustc paths
		// to the project root, and guessing wrong creates phantom paths.
		for _, base := range []string{depDir, projectRoot} {
			abs := filepath.Clean(filepath.Join(base, dep))
			if _, err := os.Stat(abs); err != nil {
				continue // doesn't exist at this base
			}
			rel, err := filepath.Rel(projectRoot, abs)
			if err == nil && !strings.HasPrefix(rel, "..") {
				return filepath.ToSlash(rel), true
			}
		}
		return "", false
	}

	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		colonIdx := strings.Index(line, ":")
		if colonIdx < 0 {
			continue
		}

		depPart := line[colonIdx+1:]
		for _, dep := range strings.Fields(depPart) {
			if dep == "" {
				continue
			}
			if rel, ok := resolve(dep); ok && !seen[rel] {
				seen[rel] = true
				deps = append(deps, rel)
			}
		}
	}

	return deps
}

// Find scans a directory tree for .d files and returns their paths.
// Respects common build directories and skips vendor/node_modules.
func Find(root string) []string {
	var out []string
	filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			name := d.Name()
			if strings.HasPrefix(name, ".") || name == "vendor" ||
				name == "node_modules" || name == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasSuffix(d.Name(), ".d") {
			rel, _ := filepath.Rel(root, path)
			out = append(out, filepath.ToSlash(rel))
		}
		return nil
	})
	return out
}

// Closure reads all .d files under root and returns the union of all
// project-relative dependencies — the exact C/C++ input closure.
func Closure(root string) []string {
	depFiles := Find(root)
	seen := map[string]bool{}
	var out []string

	for _, df := range depFiles {
		deps := Parse(filepath.Join(root, df), root)
		for _, dep := range deps {
			if !seen[dep] {
				seen[dep] = true
				out = append(out, dep)
			}
		}
	}

	return out
}
