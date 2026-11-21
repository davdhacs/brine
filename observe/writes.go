// Write tracking during builds: fsnotify events captured while the
// command runs. This refines the mtime>builtAt invariant with exact
// event semantics:
//
//	read ∧ written-during-op  → OUTPUT (make's freshness check on a
//	                             target it then relinks; a config the
//	                             build generates and consumes)
//	created-during-op         → OUTPUT that didn't exist before
//	                             (configure-generated config.h inside
//	                             a single invocation)
//	written, content-identical→ still an OUTPUT (mtime granularity
//	                             and same-second writes are immune —
//	                             events, not timestamps)
//
// Cost control: one watch per directory — big trees can exceed
// inotify/kqueue limits, so watching is best-effort with a cap; when
// the tree is too large the mtime invariant remains the fallback.
package observe

import (
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/fsnotify/fsnotify"
)

const maxWatchedDirs = 4096

// Writes records files written or created during a build.
type Writes struct {
	mu     sync.Mutex
	files  map[string]bool // absolute paths written/created
	watch  *fsnotify.Watcher
	active bool
}

// NewWrites starts a recursive watcher under root (best-effort:
// hidden dirs, vendor/node_modules/target skipped, dir count capped).
func NewWrites(root string) *Writes {
	w := &Writes{files: make(map[string]bool)}
	fsn, err := fsnotify.NewWatcher()
	if err != nil {
		return w
	}
	w.watch = fsn

	real, err := filepath.EvalSymlinks(root)
	if err != nil {
		real = root
	}
	dirs := 0
	skip := func(name string) bool {
		return strings.HasPrefix(name, ".") || name == "vendor" ||
			name == "node_modules" || name == "target"
	}
	filepath.WalkDir(real, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if !d.IsDir() {
			return nil
		}
		if path != real && skip(d.Name()) {
			return filepath.SkipDir
		}
		if dirs >= maxWatchedDirs {
			return filepath.SkipAll
		}
		if err := fsn.Add(path); err == nil {
			dirs++
		}
		return nil
	})
	if dirs == 0 {
		fsn.Close()
		return w
	}
	w.active = true
	go w.loop(real)
	return w
}

func (w *Writes) loop(root string) {
	for {
		select {
		case ev, ok := <-w.watch.Events:
			if !ok {
				return
			}
			if ev.Op&(fsnotify.Write|fsnotify.Create|fsnotify.Rename) == 0 {
				continue
			}
			// Only files under root; ignore the watcher's own churn.
			rel, err := filepath.Rel(root, ev.Name)
			if err != nil || strings.HasPrefix(rel, "..") {
				continue
			}
			w.mu.Lock()
			w.files[ev.Name] = true
			w.mu.Unlock()
		case <-w.watch.Errors:
			return
		}
	}
}

// Files returns the written/created set, relative to root.
func (w *Writes) Files(root string) []string {
	real, err := filepath.EvalSymlinks(root)
	if err != nil {
		real = root
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	var out []string
	for path := range w.files {
		if info, err := os.Stat(path); err != nil || !info.Mode().IsRegular() {
			continue
		}
		if rel, err := filepath.Rel(real, path); err == nil && !strings.HasPrefix(rel, "..") {
			out = append(out, rel)
		}
	}
	return out
}

// Active reports whether the watcher started (false = too large or
// unsupported; callers fall back to the mtime invariant).
func (w *Writes) Active() bool { return w.active }

// Close stops watching.
func (w *Writes) Close() {
	if w.watch != nil {
		w.watch.Close()
	}
}
