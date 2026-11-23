// brine: content-addressed memoization for any build command.
//
// v0.4 architecture — multi-sensor with learned edges (see docs/sensors.md):
//
//	COLD RUN:  discover the read-set via the sensor stack (go list -deps
//	           for Go — exact; lsof observation otherwise — heuristic),
//	           run the command, snapshot ONLY the outputs, record
//	           provenance in the manifest, and record the build in the
//	           learned model (edges from co-changes).
//
//	WARM RUN:  validate the latest manifest (mtime fast path → hash
//	           verify → dir-listing check → toolchain check), restore
//	           outputs on hit.
//
//	--shadow:  compute learned predictions but do NOT trust them;
//	           build fully anyway, score predictions against ground
//	           truth, harvest evidence. Zero behavioral difference.
//	           Intended for master/nightly runs.
//
//	audit:     force a full rebuild, score the model, repair missed
//	           edges, report divergences. For release certainty.
package main

import (
	"crypto/sha256"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/davdhacs/brine/buildinfo"
	"github.com/davdhacs/brine/depfiles"
	"github.com/davdhacs/brine/dissect"
	"github.com/davdhacs/brine/gocache"
	"github.com/davdhacs/brine/key"
	"github.com/davdhacs/brine/metrics"
	"github.com/davdhacs/brine/model"
	"github.com/davdhacs/brine/observe"
	"github.com/davdhacs/brine/pinaudit"
	"github.com/davdhacs/brine/store"
)

const version = "0.4.0"

func main() {
	if len(os.Args) < 2 {
		usage()
	}

	// Global flags before the command.
	shadow := false
	args := os.Args[1:]
	for len(args) > 0 && strings.HasPrefix(args[0], "--") {
		switch args[0] {
		case "--shadow":
			shadow = true
		case "--trust-learned":
			// Reserved: allow learned edges to serve hits. Not yet
			// implemented — predictions stay advisory until they've
			// survived audit validation, and even then only per-output.
			fmt.Fprintln(os.Stderr, "[brine] --trust-learned is not implemented yet; predictions stay advisory")
		default:
			fmt.Fprintf(os.Stderr, "[brine] unknown flag %s\n", args[0])
			usage()
		}
		args = args[1:]
	}
	if len(args) == 0 {
		usage()
	}

	switch {
	case args[0] == "gocache":
		// GOCACHEPROG entry point: re-exec'ed by `go` as the external
		// build cache. Pass-through over the standard GOCACHE layout,
		// writing a miss log to BRINE_GOCACHE_STATS.
		if err := gocache.Serve(); err != nil {
			fatal("gocache: %v", err)
		}
	case args[0] == "status":
		cmdStatus()
	case args[0] == "gc":
		cmdGC(args[1:])
	case args[0] == "clear":
		cmdClear()
	case args[0] == "worker":
		cmdWorker(args[1:])
	case args[0] == "remote-build":
		cmdRemoteBuild(args[1:])
	case args[0] == "bazel-generate":
		cmdBazelGenerate(args[1:])
	case args[0] == "serve":
		cmdServe(args[1:])
	case args[0] == "manifest" && len(args) > 2 && args[1] == "create":
		cmdManifestCreate(args[2:])
	case args[0] == "stats":
		cmdStats()
	case args[0] == "warm" && len(args) > 1:
		cmdWarm(args[1:])
	case args[0] == "graph":
		PrintGraph(BuildCommandGraph(NewStoreAdapter(mustWd())))
	case args[0] == "record" && len(args) > 1:
		cmdRecord(args[1:])
	case args[0] == "dissect" && len(args) == 3:
		rep, err := dissect.Diff(args[1], args[2])
		if err != nil {
			fatal("dissect: %v", err)
		}
		fmt.Print(rep)
	case args[0] == "audit" && len(args) > 1:
		cmdMemoize(args[1:], true)
	default:
		cmdMemoize(args, shadow)
	}
}

func usage() {
	fmt.Fprintf(os.Stderr, "brine v%s — content-addressed memoization\n", version)
	fmt.Fprintf(os.Stderr, "\nUsage:\n")
	fmt.Fprintf(os.Stderr, "  brine [--shadow] <command> [args...]  memoize a command\n")
	fmt.Fprintf(os.Stderr, "  brine audit <command> [args...]       force full rebuild + score model\n")
	fmt.Fprintf(os.Stderr, "  brine status | gc | clear\n")
	fmt.Fprintf(os.Stderr, "\nFlags:\n")
	fmt.Fprintf(os.Stderr, "  --shadow   score learned predictions without trusting them\n")
	os.Exit(1)
}

// reportKey writes the receipt key to BRINE_KEY_OUT when set — the
// machine-readable channel brine worker uses to return receipt identity
// to a remote-build client.
func reportKey(k string) {
	if p := os.Getenv("BRINE_KEY_OUT"); p != "" {
		os.WriteFile(p, []byte(k), 0o644)
	}
}

func cmdMemoize(args []string, forceFull bool) {
	command := strings.Join(args, " ")
	wd, _ := os.Getwd()

	st, err := store.New(filepath.Join(wd, ".brine"))
	if err != nil {
		fatal("store: %v", err)
	}
	metricsLogger := metrics.NewLogger(filepath.Join(wd, ".brine"))
	// BRINE_REMOTE=1: receipts + output blobs in the cloud store, and
	// the gocache proxy's remote tier on for Go commands (both levels
	// share the backend and container).
	if os.Getenv("BRINE_REMOTE") == "1" {
		os.Setenv("BRINE_GOCACHE_REMOTE", "1")
		st.SetRemote(gocache.NewRemoteFromEnv())
	}
	mod, err := model.Load(filepath.Join(wd, ".brine", "model.json"))
	if err != nil {
		fatal("model: %v", err)
	}

	goPatterns := key.GoPatterns(args)
	isGo := goPatterns != nil
	isDocker := isDockerBuild(args)
	var toolchain string
	if isGo {
		// Toolchain + build config: CGO_ENABLED, GOOS, GOARCH, GOFLAGS
		// change outputs without changing any file content — they must
		// be in the key or a warm hit can serve a wrong-binary.
		toolchain = key.BuildConfig(wd)
	}

	start := time.Now()

	// Untracked config artifacts are candidate inputs — added to the
	// read-set BEFORE the writes filter: a config the build generates
	// gets removed by that filter (it is an output); a config written
	// outside brine (manual ./configure) stays and is hashed like any
	// input, catching config flips.
	// === Commit-receipt fast path ===
	// A clean tree at HEAD is exactly determined: SHA → tree → closure.
	// This skips `go list`, closure hashing, and (in CI) the module
	// cache + mtime normalization the content path needs. Falls
	// through silently to the slower paths when the tree is dirty or
	// no commit receipt exists.
	if !forceFull && !isDocker {
		keyCommand := command
		if ck, ok := commitReceiptKey(wd, keyCommand+keyTail(wd), toolchain); ok && st.HasManifest(ck) {
			if m, err := st.GetManifest(ck); err == nil && len(m.Blobs) > 0 {
				// Commit receipts vouch for the TREE, not untracked
				// configs: a config on disk that this receipt does not
				// produce is a hidden input it cannot vouch for.
				blobSet := map[string]bool{}
				for _, b := range m.Blobs {
					blobSet[b.Path] = true
				}
				skipFast := false
				for _, cfg := range untrackedConfigs(wd) {
					if !blobSet[cfg] {
						fmt.Printf("[brine] commit receipt skipped: untracked config %s is not produced by this receipt\n", cfg)
						skipFast = true
						break
					}
				}
				if !skipFast {
					verifyRestored(m, wd)
					if count, bytes, err := st.RestoreManifest(m, wd); err == nil {
						reportKey(ck)
						st.EnsureRemote(m) // synchronous: async raced process exit // hit paths never upload; share local receipts
						fmt.Printf("[brine] CACHE HIT (commit): %s — clean tree at HEAD implies closure\n", ck)
						fmt.Printf("[brine]   %d outputs, %.1f MB (%s)\n",
							count, float64(bytes)/1024/1024, time.Since(start).Round(time.Millisecond))
						recordHit(mod, command)
						return
					}
				}
			}
		}
	}

	// === Warm path (skipped for audit / shadow: both build fully) ===
	if !forceFull {
		if m := st.LatestManifestForCommand(command); m != nil {
			// Invisible-input classes (untracked configs, key env) are
			// not in the read-set — the warm path must compare the
			// recorded tail explicitly or it serves stale receipts
			// across config flips.
			tailNow := keyTail(wd)
			toolchainOK := !isGo || m.Toolchain == toolchain
			tailOK := m.KeyTail == tailNow
			vcsOK := !isGo || !vcsFoldActive() || m.VCSCommit == gitHead(wd)
			if !toolchainOK {
				fmt.Printf("[brine] MISS: toolchain changed\n")
			} else if !tailOK {
				fmt.Printf("[brine] MISS: untracked config or key env changed\n")
			} else if !vcsOK {
				fmt.Printf("[brine] MISS: commit changed (VCS-stamped build)\n")
			} else if valid, reasons := st.Validate(m, wd); valid {
				if len(m.ImageOutputs) > 0 {
					// Image receipt: re-tag instead of restoring files.
					allOK := true
					for _, ref := range m.ImageOutputs {
						if err := dockerRetag(ref.ImageID, ref.Tag); err != nil {
							fmt.Fprintf(os.Stderr, "[brine] retag %s failed: %v — rebuilding\n", ref.Tag, err)
							allOK = false
							break
						}
					}
					if allOK {
						fmt.Printf("[brine] CACHE HIT (image re-tag): %s\n", m.Key)
						for _, ref := range m.ImageOutputs {
							fmt.Printf("[brine]   %s -> %s\n", ref.ImageID[:19], ref.Tag)
						}
						recordHit(mod, command)
						return
					}
				}
				count, bytes, err := st.RestoreManifest(m, wd)
				if err == nil {
					reportKey(m.Key)
					st.EnsureRemote(m) // synchronous: async raced process exit // hit paths never upload; share local receipts
					fmt.Printf("[brine] CACHE HIT: %s\n", m.Key)
					fmt.Printf("[brine]   %d outputs, %.1f MB, strategy=%s (%s)\n",
						count, float64(bytes)/1024/1024, m.Strategy,
						time.Since(start).Round(time.Millisecond))
					storeCommitReceiptIfClean(st, m, wd, command, toolchain)
					recordHit(mod, command)
					if metricsLogger != nil {
						metricsLogger.Log(&metrics.Run{Command: command, Hit: true, WallMs: time.Since(start).Milliseconds()})
					}
					return
				}
				fmt.Fprintf(os.Stderr, "[brine] restore error: %v — rebuilding\n", err)
			} else {
				fmt.Printf("[brine] MISS: %s\n", store.FormatReasons(reasons))
			}
		}
	}

	// Docker keyed check BEFORE running: read-set is git-tracked
	// (computable pre-build), and the receipt re-tags instead of
	// restoring files.
	if isDocker && !forceFull {
		if ctxFiles := dockerContextClosure(wd, args); len(ctxFiles) > 0 {
			dk := key.FromFiles(wd, ctxFiles, dockerKeyCommand(command, args)+"|"+toolchain+vcsFold(wd)+keyTail(wd)+dockerKeyExtras(wd, args))
			if st.HasManifest(dk) {
				if m, err := st.GetManifest(dk); err == nil && len(m.ImageOutputs) > 0 {
					allOK := true
					for _, ref := range m.ImageOutputs {
						if err := dockerRetag(ref.ImageID, ref.Tag); err != nil {
							allOK = false
							break
						}
					}
					if allOK {
						// Also satisfy the tags this invocation asked for.
						for _, t := range dockerBuildTags(args) {
							dockerRetag(m.ImageOutputs[0].ImageID, t)
						}
						fmt.Printf("[brine] CACHE HIT (image re-tag): %s\n", dk)
						for _, ref := range m.ImageOutputs {
							fmt.Printf("[brine]   %s -> %s\n", ref.ImageID[:19], ref.Tag)
						}
						recordHit(mod, command)
						return
					}
				}
			}
		}
	}

	// === Miss: re-derive the read-set ===
	var readSet []string
	strategy := store.StrategyObserved
	closure := ""

	if isGo {
		strategy = store.StrategyGoList
		closure = "go list -deps " + strings.Join(goPatterns, " ")
		readSet, err = key.GoClosure(wd, goPatterns)
		if err != nil {
			fmt.Fprintf(os.Stderr, "[brine] %v — running un-memoized\n", err)
			runPlain(args)
			return
		}
		k := key.FromFiles(wd, readSet, dockerKeyCommand(command, args)+"|"+toolchain+vcsFold(wd)+keyTail(wd))
		if !forceFull && st.HasManifest(k) {
			if m, err := st.GetManifest(k); err == nil {
				count, bytes, err := st.RestoreManifest(m, wd)
				if err != nil {
					// Integrity failure or missing blob: visible, then
					// self-heal via rebuild (never serve wrong bytes).
					fmt.Fprintf(os.Stderr, "[brine] receipt restore failed: %v — rebuilding\n", err)
				}
				if err == nil {
					reportKey(k)
					st.EnsureRemote(m) // synchronous: async raced process exit // hit paths never upload; share local receipts
					verifyRestored(m, wd)
					fmt.Printf("[brine] CACHE HIT (keyed): %s — closure re-derived\n", k)
					fmt.Printf("[brine]   %d outputs, %.1f MB (%s)\n",
						count, float64(bytes)/1024/1024,
						time.Since(start).Round(time.Millisecond))
					storeCommitReceiptIfClean(st, m, wd, command, toolchain)
					recordHit(mod, command)
					if metricsLogger != nil {
						metricsLogger.Log(&metrics.Run{Command: command, Hit: true, WallMs: time.Since(start).Milliseconds()})
					}
					return
				}
			}
		}
	}

	// === Shadow: predict (advisory) before building ===
	var shadowPrediction map[string]bool
	var shadowChanged []string
	if forceFull {
		// Compute what changed since the last full build for scoring.
		current := probeInputs(wd, readSet)
		shadowChanged = mod.ChangedSince(model.BuildRecord{Inputs: current})
		if len(shadowChanged) > 0 {
			pred, conservative := mod.Predict(shadowChanged)
			shadowPrediction = pred
			if conservative {
				fmt.Printf("[brine] shadow: %d changed inputs include unknown files → prediction: conservative (all outputs)\n",
					len(shadowChanged))
			} else {
				fmt.Printf("[brine] shadow: %d changed inputs → prediction: %d output(s) affected\n",
					len(shadowChanged), len(pred))
			}
		}
	}

	// Side caches: restore lockfile-keyed directories (node_modules,
	// vendor) from the blob store before building. This is the CA
	// branch's vendor/UI caching, generalized.
	if os.Getenv("BRINE_REMOTE") == "1" {
		for _, sc := range defaultSideCaches(wd) {
			if restored, err := restoreSideCache(wd, sc, nil); err != nil {
				fmt.Fprintf(os.Stderr, "[brine] side-cache restore %s: %v\n", sc.Dir, err)
			} else if !restored && sc.RestoreCmd != "" {
				fmt.Printf("[brine] side-cache: %s not in store — running %s\n", sc.Dir, sc.RestoreCmd)
				// The restore command runs as part of the build
				// (it's not memoized separately — the side cache IS
				// the memoization of this step)
			}
		}
	}

	// Prepare go builds: mtime normalization + VCS stamp disabling.
	// These fix the systematic action-cache invalidation that git
	// checkout causes (mtime reset on unchanged files → package-index
	// hash differs → ~25% of cache entries miss on every commit).
	if isGo {
		prepareGoBuild(wd)
	}

	// Auto-configure protocol caches (ccache for C/C++, sccache for
	// Rust): closes the changed-input-cold-machine gap for the top
	// three languages alongside GOCACHEPROG for Go.
	pc := detectProtocolCache(wd, args)
	if pc != nil {
		fmt.Printf("[brine] protocol-cache: %s configured\n", pc.Tool)
	}

	// Write tracking (non-go commands): fsnotify events give exact
	// output semantics — read∧written = output (make freshness),
	// created-during-op = generated input consumed later in the same
	// invocation, immune to same-second mtime granularity.
	var writes *observe.Writes
	if !isGo && boolOr(loadConfig(wd).Writes, true) {
		writes = observe.NewWrites(wd)
		defer writes.Close()
	}

	// Pre-build keyed check for non-go commands (the go path has its
	// own, closure-based). Pre-build the read-set is git ∪ untracked
	// configs — anything the build REgenerates is excluded from the
	// STORED key (writes filter), so a leftover copy makes keys differ
	// → miss → safe. Absent (wiped) → matches → hit.
	if !isGo && !isDocker && !forceFull {
		if gitFiles := gitTrackedFiles(wd); gitFiles != nil {
			candidates := unionFiles(gitFiles, untrackedConfigs(wd))
			if len(candidates) > 0 {
				pk := key.FromFiles(wd, candidates, command+"|"+toolchain+vcsFold(wd)+keyTail(wd))
				if st.HasManifest(pk) {
					if m, err := st.GetManifest(pk); err == nil && len(m.Blobs) > 0 {
						// The stored read-set may be a SUBSET (generated
						// configs were filtered out at store time) — only
						// trust the hit if the stored set matches the
						// current candidate content.
						match := true
						for rel, want := range m.InputHashes {
							if got, ok := key.HashInput(wd, rel); !ok || got != want {
								match = false
								break
							}
						}
						if match {
							count, bytes, err := st.RestoreManifest(m, wd)
							if err == nil {
								fmt.Printf("[brine] CACHE HIT (keyed): %s\n", pk)
								fmt.Printf("[brine]   %d outputs, %.1f MB (%s)\n",
									count, float64(bytes)/1024/1024,
									time.Since(start).Round(time.Millisecond))
								storeCommitReceiptIfClean(st, m, wd, command, toolchain)
								recordHit(mod, command)
								return
							}
						}
					}
				}
			}
		}
	}

	// === Run the command ===
	if forceFull {
		fmt.Printf("[brine] audit: forcing full rebuild\n")
	} else {
		fmt.Printf("[brine] running: %s\n", command)
	}
	buildStart := time.Now()
	builtAt := buildStart.UnixNano()

	reads := observe.NewReads()
	cmd := exec.Command(args[0], args[1:]...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	// Go commands run through the GOCACHEPROG pass-through proxy:
	// cohabits the standard cache, produces the action miss log
	// (sensor #9). Failure to set it up is non-fatal — worst case is
	// go using its own cache directly.
	var statsPath string
	if isGo {
		if self, err := os.Executable(); err == nil {
			statsPath = filepath.Join(wd, ".brine", fmt.Sprintf("gocache-stats-%d.json", os.Getpid()))
			cmd.Env = append(os.Environ(),
				"GOCACHEPROG="+self+" gocache",
				"BRINE_GOCACHE_STATS="+statsPath,
				"BRINE_GOCACHE_SCOPE="+strings.Join(goPatterns, " "))
		}
	}

	applyProtocolCache(cmd, pc)
	if err := cmd.Start(); err != nil {
		fatal("start: %v", err)
	}
	if !isGo {
		// Linux: poll the whole process tree via /proc (workers do the
		// source reads; lsof on the top PID only sees the orchestrator).
		// Elsewhere: lsof on the top PID.
		if _, err := os.Stat("/proc"); err == nil {
			go reads.TrackProcTree(cmd.Process.Pid, 20*time.Millisecond)
		} else {
			go reads.Track(cmd.Process.Pid, 50*time.Millisecond)
		}
	}
	if err := cmd.Wait(); err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			os.Exit(exitErr.ExitCode())
		}
		fatal("run: %v", err)
	}
	buildTime := time.Since(buildStart)

	// Harvest the action miss log (sensor #9): exact work accounting.
	var actionStats *gocache.Stats
	if statsPath != "" {
		if st, err := gocache.ReadStats(statsPath); err == nil && st.Gets+st.Puts > 0 {
			actionStats = st
			fmt.Printf("[brine] gocache: %d gets (%d hit, %d miss), %d puts (%.1f MB)\n",
				st.Gets, st.Hits, st.Misses, st.Puts, float64(st.PutBytes)/1024/1024)
		}
		os.Remove(statsPath)
	}

	var writtenSet map[string]bool
	if writes != nil && writes.Active() {
		writtenSet = map[string]bool{}
		for _, f := range writes.Files(wd) {
			writtenSet[f] = true
		}
	}
	filterWrites := func(files []string) []string {
		if writtenSet == nil {
			return files
		}
		var out []string
		for _, f := range files {
			if !writtenSet[f] {
				out = append(out, f)
			}
		}
		return out
	}

	// Dep-files sensor (#7): if the build produced .d files (gcc/clang
	// -MD), parse them for the exact C/C++ closure — headers the
	// compiler actually included, not just what git tracks.
	var depFileClosure []string
	if !isGo && !isDocker {
		if found := depfiles.Find(wd); len(found) > 0 {
			depFileClosure = depfiles.Closure(wd)
			if len(depFileClosure) > 0 {
				fmt.Printf("[brine] dep-files: %d .d files → %d exact dependencies\n",
					len(found), len(depFileClosure))
			}
		}
	}

	// === Discover the read-set (non-Go commands) ===
	if isDocker {
		// The build context IS the input closure — including files
		// that are outputs of earlier commands (binaries COPYied into
		// the image). Git-tracked fallback is unsound for image builds
		// (untracked context files would be invisible to the key).
		readSet = dockerContextClosure(wd, args)
		strategy = store.StrategyFallback
	} else if !isGo {
		candidates := unionFiles(reads.Files(wd), untrackedConfigs(wd))
		if len(depFileClosure) > 0 {
			candidates = unionFiles(candidates, depFileClosure)
		}
		if observed := filterWrites(filterBuildState(wd, candidates, builtAt)); len(observed) > 0 {
			readSet = observed
			strategy = store.StrategyObserved
			// Orchestrator blindness: builds that delegate into
			// containers (make -> docker exec -> cmake) do their real
			// reads where the host cannot observe them — lsof saw ONE
			// file (the Makefile) for the collector build. Union with
			// git-tracked files so the key stays sound; observation
			// then only ADDS precision when it catches real reads.
			if gitFiles := gitTrackedFiles(wd); gitFiles != nil {
				readSet = unionFiles(readSet, filterWrites(gitFiles))
			} else {
				// no VCS: observed reads alone may miss files; add the
				// full-project walk as a sound superset
				readSet = unionFiles(readSet, filterWrites(noVCSFallback(wd)))
			}
		} else if gitFiles := gitTrackedFiles(wd); gitFiles != nil {
			// Same build-write invariant: tracked files rewritten by
			// the build (committed artifacts, make freshness targets)
			// are outputs of this build, not inputs.
			all := unionFiles(gitFiles, untrackedConfigs(wd))
			if len(depFileClosure) > 0 {
				all = unionFiles(all, depFileClosure)
				// dep-files give header-level precision — mark as
				// dep-files strategy when it's the primary source
				if len(depFileClosure) > len(gitFiles)/2 {
					strategy = "dep-files"
				}
			}
			readSet = filterWrites(filterBuildState(wd, all, builtAt))
			if strategy != "dep-files" {
				strategy = store.StrategyFallback
			}
		} else {
			// No VCS: hash all project files (zip-extracted projects,
			// tarballs, directories without .git). Over-inclusive but
			// sound; build outputs excluded by the writes filter.
			allFiles := noVCSFallback(wd)
			if len(allFiles) > 0 {
				readSet = filterWrites(filterBuildState(wd,
					unionFiles(allFiles, untrackedConfigs(wd)), builtAt))
				strategy = "no-vcs"
				fmt.Printf("[brine] no VCS found — using all %d project files as closure (over-inclusive, sound)\n", len(readSet))
			}
		}
	}
	if len(readSet) == 0 {
		fmt.Printf("[brine] no read-set derivable (observed raw=%d, git ok=%v) — output not cached\n",
			len(reads.Files(wd)), gitTrackedFiles(wd) != nil)
		return
	}

	// === Snapshot OUTPUTS ONLY and record provenance ===
	keyCommand := command
	extras := ""
	if isDocker {
		// Tags are output NAMES, not inputs: the same context built to
		// different tags is the same receipt (found via the divergence
		// detector: same closure, same image, different key).
		for _, t := range dockerBuildTags(args) {
			keyCommand = strings.ReplaceAll(keyCommand, " "+t+" ", " ")
		}
		extras = dockerKeyExtras(wd, args)
	}
	k := key.FromFiles(wd, readSet, keyCommand+"|"+toolchain+vcsFold(wd)+keyTail(wd)+extras)
	var m *store.Manifest
	if isDocker {
		// Image-level receipt: outputs are image IDs, not files.
		m = &store.Manifest{Key: k, Created: time.Now()}
		for _, tag := range dockerBuildTags(args) {
			id, ierr := dockerImageID(tag)
			if ierr != nil {
				fatal("docker inspect %s: %v", tag, ierr)
			}
			m.ImageOutputs = append(m.ImageOutputs, store.ImageRef{ImageID: id, Tag: tag})
		}
	} else {
		m, err = st.SnapshotOutputs(k, wd, readSet, builtAt)
		if err != nil {
			fatal("snapshot: %v", err)
		}
	}
	m.Command = command
	m.Strategy = strategy
	m.ReadSet = readSet
	// Every ancestor directory of every output is an output dir:
	// wiping any of them (cmake-build/, collector/container/bin) must
	// not invalidate input-dir listings.
	outputDirs := map[string]bool{}
	for _, b := range m.Blobs {
		p := b.Path
		for {
			if i := strings.LastIndexByte(p, '/'); i > 0 {
				p = p[:i]
				outputDirs[p] = true
			} else {
				break
			}
		}
	}
	for _, d := range m.OutputDirs {
		outputDirs[d] = true
		// also exclude all ANCESTORS of output dirs: "target/debug"
		// implies "target" must be excluded from the root listing too
		// (it was created by a PREVIOUS build, so its own mtime is old
		// and the walk doesn't record it — found via zed, where wiping
		// target/ invalidated the root listing)
		for p := d; ; {
			if i := strings.LastIndexByte(p, '/'); i > 0 {
				p = p[:i]
				outputDirs[p] = true
			} else {
				break
			}
		}
	}
	for _, b := range m.Blobs {
		outputDirs[b.Path] = true // flat projects: root-level output files
		for p := b.Path; ; {
			if i := strings.LastIndexByte(p, '/'); i > 0 {
				p = p[:i]
				outputDirs[p] = true
			} else {
				break
			}
		}
	}
	m.InputHashes, m.DirListings = store.BuildProvenanceFiltered(wd, readSet, outputDirs)
	m.Closure = closure
	m.Toolchain = toolchain
	m.BuiltAt = builtAt
	m.ActionStats = actionStats
	m.VCSCommit = gitHead(wd)
	m.KeyTail = keyTail(wd)
	m.ClosureHash = key.ClosureHash(wd, readSet)

	// Nondeterminism detector: same closure, different key or different
	// output = something invisible changed (or the build is
	// nondeterministic). Dissect and warn.
	if prev := st.LatestForClosure(m.ClosureHash, m.Key); prev != nil {
		warnDivergence(st, prev, m, wd)
	}

	// Buildinfo audit (tier 1): inspect go-binary outputs, record in
	// the manifest, warn on memoization-blind inputs.
	for _, ref := range m.Blobs {
		if info, err := buildinfo.Inspect(filepath.Join(wd, ref.Path)); err == nil && info != nil {
			m.BuildInfo = info
			for _, f := range buildinfo.Audit(info, command) {
				fmt.Printf("[brine] AUDIT %s: %s\n", f.Kind, f.Detail)
				fmt.Printf("[brine]   warn: %s\n", f.Warning)
			}
			break // first go binary is the story; others are similar
		}
	}
	reportKey(k)
	if err := st.PutManifest(m); err != nil {
		fatal("manifest store: %v", err)
	}
	// Commit receipt: same manifest under the git-HEAD key (blobs are
	// content-addressed — zero extra storage; just a second index).
	if !isDocker {
		storeCommitReceiptIfClean(st, m, wd, keyCommand, toolchain)
	}

	// Docker image: also save to blob store for cross-machine restore
	// (docker save → upload; docker load on fresh runners).
	if isDocker {
		for _, ref := range m.ImageOutputs {
			saveDockerImage(ref.ImageID, k, nil)
		}
	}

	// Side caches: save lockfile-keyed directories (node_modules, vendor)
	for _, sc := range defaultSideCaches(wd) {
		if os.Getenv("BRINE_REMOTE") == "1" {
			if err := saveSideCache(wd, sc, nil); err != nil {
				fmt.Fprintf(os.Stderr, "[brine] side-cache save %s: %v\n", sc.Dir, err)
			}
		}
	}

	// Effective config report: what's influencing keys that could be
	// persisted to .brine.json (or already is). Only prints when there
	// IS something non-default — silent for the zero-config case.
	reportEffectiveConfig(wd)

	// Metrics: record this run (build path = miss)
	metricsLogger.Log(&metrics.Run{
		Command:     command,
		Key:         k,
		Strategy:    strategy,
		Hit:         false,
		WallMs:      time.Since(start).Milliseconds(),
		BuildMs:     buildTime.Milliseconds(),
		InputCount:  int64(len(readSet)),
		OutputCount: int64(len(m.Blobs)),
		OutputBytes: sumSizes(m.Blobs),
	})

	// Pin audit (config determinism): floating dependency references
	// make builds nondeterministic and caches unreliable — name them.
	for _, f := range pinaudit.Audit(wd) {
		fmt.Printf("[brine] AUDIT %s (%s): %s\n", f.Kind, f.File, f.Detail)
		fmt.Printf("[brine]   warn: %s\n", f.Warning)
	}

	// Cross-command dependency graph: proactively invalidate the
	// receipts of all transitive dependents. Their inputs changed via
	// this command's outputs — their next run must be a MISS, not a
	// stale HIT.
	if !isDocker {
		graph := NewStoreAdapter(wd)
		edges := BuildCommandGraph(graph)
		if invalidated := InvalidateDependents(graph, edges, command); len(invalidated) > 0 {
			fmt.Printf("[brine] graph: invalidated %d dependent receipt(s):\n", len(invalidated))
			for _, dep := range invalidated {
				fmt.Printf("[brine]   %s\n", shortCmd(dep))
			}
		}
	}

	fmt.Printf("[brine] CACHED: %s\n", k)
	fmt.Printf("[brine]   %d outputs, %.1f MB, %d inputs, strategy=%s (build: %s)\n",
		len(m.Blobs), float64(sumSizes(m.Blobs))/1024/1024,
		len(readSet), strategy, buildTime.Round(time.Millisecond))

	// === Record in the learned model ===
	prev := mod.LastFullBuild()
	mod.RecordBuild(model.BuildRecord{
		Time:    time.Now(),
		Command: command,
		Full:    true,
		Shadow:  forceFull,
		Inputs:  m.InputHashes,
		Outputs: m.OutputHashes(),
	})

	// === Shadow/audit scoring ===
	if forceFull && prev != nil && len(shadowChanged) > 0 {
		actualChanged := diffOutputHashes(prev.Outputs, m.OutputHashes())
		rep := mod.Score(shadowChanged, actualChanged)
		fmt.Printf("[brine] audit: ground truth — %d output(s) changed; prediction had %d\n",
			len(actualChanged), len(shadowPrediction))
		if len(rep.MissedEdges) > 0 {
			fmt.Printf("[brine] audit: %d missed edge(s) repaired into model\n", len(rep.MissedEdges))
		}
		if rep.Spurious > 0 {
			fmt.Printf("[brine] audit: %d spurious prediction(s) (imprecision, not error)\n", rep.Spurious)
		}
		fmt.Printf("[brine] audit: recall %.0f%%\n", rep.Recall*100)
	} else if forceFull && prev == nil {
		fmt.Printf("[brine] audit: baseline recorded (no prior full build to score against)\n")
	}

	if err := mod.Save(); err != nil {
		fmt.Fprintf(os.Stderr, "[brine] model save: %v\n", err)
	}
}

// storeCommitReceiptIfClean writes the git-HEAD alias for the manifest
// just used/created, when the tree is clean. Also called on CONTENT-path
// hits: a clean tree + a valid closure receipt implies the commit
// receipt, so the first hit at any commit makes every later run at
// that commit take the fast path (no go list, no module cache).
func storeCommitReceiptIfClean(st *store.Store, m *store.Manifest, wd, keyCommand, toolchain string) {
	if m.KeyKind != "" {
		return // already an alias
	}
	if ck, ok := commitReceiptKey(wd, keyCommand+keyTail(wd), toolchain); ok {
		cm := *m
		cm.Key = ck
		cm.KeyKind = "commit"
		if err := st.PutManifest(&cm); err == nil {
			fmt.Printf("[brine] commit receipt: %s\n", ck)
		}
	}
}

// recordHit logs a cache-hit build in the model (Full=false: produces
// no evidence — see model package safety rules).
func recordHit(mod *model.Model, command string) {
	mod.RecordBuild(model.BuildRecord{Time: time.Now(), Command: command, Full: false})
	if err := mod.Save(); err != nil {
		fmt.Fprintf(os.Stderr, "[brine] model save: %v\n", err)
	}
}

// recordHitLogged records a hit AND writes a metrics entry.
func recordHitLogged(mod *model.Model, logger *metrics.Logger, run *metrics.Run) {
	recordHit(mod, run.Command)
	if logger != nil {
		logger.Log(run)
	}
}

// probeInputs hashes the read-set files without building anything —
// used to compute "what changed" for shadow scoring.
func probeInputs(wd string, readSet []string) map[string]string {
	out := make(map[string]string, len(readSet))
	for _, rel := range readSet {
		data, err := os.ReadFile(filepath.Join(wd, rel))
		if err != nil {
			continue
		}
		out[rel] = fmt.Sprintf("%x", sha256Sum(data))
	}
	return out
}

func diffOutputHashes(a, b map[string]string) []string {
	var out []string
	for k, v := range b {
		if av, ok := a[k]; !ok || av != v {
			out = append(out, k)
		}
	}
	for k := range a {
		if _, ok := b[k]; !ok {
			out = append(out, k)
		}
	}
	return out
}

// dockerKeyExtras folds base-image digests into the docker key
// fragment (mutable tags must not serve stale images).
func dockerKeyExtras(wd string, args []string) string {
	digests := dockerBaseDigests(wd, args)
	if len(digests) == 0 {
		return ""
	}
	return "|bases:" + strings.Join(digests, ",")
}

// dockerKeyCommand strips output tags from a docker build command for
// keying (tags are output names, not inputs).
func dockerKeyCommand(command string, args []string) string {
	if !isDockerBuild(args) {
		return command
	}
	for _, t := range dockerBuildTags(args) {
		command = strings.ReplaceAll(command, " "+t+" ", " ")
	}
	return command
}

// runPlain executes the command with no memoization.
func runPlain(args []string) {
	cmd := exec.Command(args[0], args[1:]...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			os.Exit(exitErr.ExitCode())
		}
		fatal("run: %v", err)
	}
}

func cmdStatus() {
	wd, _ := os.Getwd()
	storeDir := filepath.Join(wd, ".brine")
	if _, err := os.Stat(storeDir); os.IsNotExist(err) {
		fmt.Println("no cache (run brine <command> first)")
		return
	}

	var manifestCount, blobCount int
	var blobSize int64
	filepath.Walk(filepath.Join(storeDir, "manifests"), func(p string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() {
			manifestCount++
		}
		return nil
	})
	filepath.Walk(filepath.Join(storeDir, "blobs"), func(p string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() {
			blobCount++
			blobSize += info.Size()
		}
		return nil
	})

	fmt.Printf("store: %s\n", storeDir)
	fmt.Printf("  manifests: %d\n", manifestCount)
	fmt.Printf("  blobs:     %d (%.1f MB)\n", blobCount, float64(blobSize)/1024/1024)

	if mod, err := model.Load(filepath.Join(storeDir, "model.json")); err == nil {
		var validated int
		for _, e := range mod.Edges {
			if e.Validated {
				validated++
			}
		}
		fmt.Printf("  model:     %d edges (%d validated), %d builds\n",
			len(mod.Edges), validated, len(mod.Builds))
	}
}

func cmdGC(args []string) {
	maxAge := 720 * time.Hour
	for i := 0; i < len(args); i++ {
		if args[i] == "--max-age" && i+1 < len(args) {
			if d, err := time.ParseDuration(args[i+1]); err == nil {
				maxAge = d
			}
		}
	}
	wd, _ := os.Getwd()
	st, err := store.New(filepath.Join(wd, ".brine"))
	if err != nil {
		fatal("store: %v", err)
	}
	// BRINE_REMOTE=1: receipts + output blobs in the cloud store, and
	// the gocache proxy's remote tier on for Go commands (both levels
	// share the backend and container).
	if os.Getenv("BRINE_REMOTE") == "1" {
		os.Setenv("BRINE_GOCACHE_REMOTE", "1")
		st.SetRemote(gocache.NewRemoteFromEnv())
	}
	freed, removed, err := st.GC(maxAge)
	if err != nil {
		fatal("gc: %v", err)
	}
	fmt.Printf("[brine] GC: freed %.1f MB, removed %d items (max age: %s)\n",
		float64(freed)/1024/1024, removed, maxAge)
}

func cmdClear() {
	wd, _ := os.Getwd()
	if err := os.RemoveAll(filepath.Join(wd, ".brine")); err != nil {
		fatal("clear: %v", err)
	}
	fmt.Println("[brine] cache cleared")
}

func gitTrackedFiles(dir string) []string {
	// -s: stage mode prefixes. Submodule entries are gitlinks (mode
	// 160000): hashing them as files fails silently, making submodule
	// content invisible to the key (found via the fact case study —
	// its build reads protos from third_party/stackrox). Replace each
	// gitlink with the submodule's HEAD commit as a pseudo-input.
	// -c safe.directory: tar-copied and container checkouts trip git's
	// ownership check, silently degrading brine to no-cache (found in
	// the fact-in-container case study).
	cmd := exec.Command("git", "-C", dir, "-c", "safe.directory="+dir, "ls-files", "-s")
	out, err := cmd.Output()
	if err != nil {
		return nil
	}
	var files []string
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 4 {
			continue
		}
		mode, path := fields[0], fields[3]
		if mode == "160000" {
			// path is the submodule dir; its HEAD is the content key
			if head, err := exec.Command("git", "-C", filepath.Join(dir, path), "-c", "safe.directory="+filepath.Join(dir, path), "rev-parse", "HEAD").Output(); err == nil {
				files = append(files, path+"@gitlink:"+strings.TrimSpace(string(head)))
			}
			continue
		}
		files = append(files, path)
	}
	return files
}

func sumSizes(blobs []store.BlobRef) int64 {
	var total int64
	for _, b := range blobs {
		total += b.Size
	}
	return total
}

func sha256Sum(data []byte) []byte {
	sum := sha256.Sum256(data)
	return sum[:]
}

func fatal(format string, args ...interface{}) {
	fmt.Fprintf(os.Stderr, "[brine] ERROR: "+format+"\n", args...)
	os.Exit(1)
}

// vcsFoldActive reports whether VCS state participates in keys for
// this run: stamping not provably off (GOFLAGS) and not disabled via
// .brine.json.
func vcsFoldActive() bool {
	if strings.Contains(os.Getenv("GOFLAGS"), "-buildvcs=false") {
		return false
	}
	return loadConfig(".").BuildVCS != "off"
}

// untrackedConfigs finds config artifacts that may be build inputs:
// built-in patterns PLUS the user's declared key_files (which must be
// in the read-set for the cross-command graph to see the dependency).
func untrackedConfigs(wd string) []string {
	var out []string
	seen := map[string]bool{}
	add := func(rel string) {
		if !seen[rel] {
			seen[rel] = true
			out = append(out, rel)
		}
	}
	for _, pat := range []string{"config.h", "config.status", "CMakeCache.txt", "*.cmake", "local.mk", ".env"} {
		if files, _ := filepath.Glob(filepath.Join(wd, pat)); len(files) > 0 {
			for _, f := range files {
				if info, err := os.Stat(f); err == nil && info.Mode().IsRegular() {
					rel, _ := filepath.Rel(wd, f)
					add(filepath.ToSlash(rel))
				}
			}
		}
	}
	for _, f := range loadConfig(wd).KeyFiles {
		if _, err := os.Stat(filepath.Join(wd, f)); err == nil {
			add(f)
		}
	}
	return out
}

// noVCSFallback: when no VCS is available (zip-extracted project,
// no .git), hash ALL project files as the closure. Over-inclusive
// but sound — any change invalidates.
func noVCSFallback(wd string) []string {
	var files []string
	filepath.WalkDir(wd, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			name := d.Name()
			if strings.HasPrefix(name, ".") || name == "vendor" ||
				name == "node_modules" || name == "target" {
				return filepath.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		rel, _ := filepath.Rel(wd, path)
		files = append(files, filepath.ToSlash(rel))
		return nil
	})
	if len(files) > 50000 {
		files = files[:50000]
	}
	sort.Strings(files)
	return files
}

// gitHead returns the current commit, or "" outside a repo.
func gitHead(wd string) string {
	out, err := exec.Command("git", "-C", wd, "-c", "safe.directory="+wd, "rev-parse", "HEAD").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// vcsFold returns the key fragment that makes VCS-stamped builds
// sound: git HEAD. Skipped when stamping is provably off (GOFLAGS
// carries -buildvcs=false) or the user opted out via .brine.json
// ("deterministic": {"buildvcs": "off"} — a promise that they disable
// stamping themselves; cherry-picks then hit by content, correctly).
func vcsFold(wd string) string {
	if !vcsFoldActive() {
		return ""
	}
	out, err := exec.Command("git", "-C", wd, "-c", "safe.directory="+wd, "rev-parse", "HEAD").Output()
	if err != nil {
		return "" // not a git repo — nothing to fold
	}
	return "|commit:" + strings.TrimSpace(string(out))
}

// verifyRestored re-inspects a restored go binary against the
// manifest's recorded buildinfo — the semantic identity check.
func verifyRestored(m *store.Manifest, wd string) {
	if m.BuildInfo == nil {
		return
	}
	for _, ref := range m.Blobs {
		info, err := buildinfo.Inspect(filepath.Join(wd, ref.Path))
		if err != nil || info == nil {
			continue
		}
		if ok, mismatch := buildinfo.Verify(m.BuildInfo, info); !ok {
			fmt.Printf("[brine] VERIFY FAILED on %s: %s — manifest may be stale\n", ref.Path, mismatch)
		} else {
			fmt.Printf("[brine] verified: %s buildinfo matches manifest\n", ref.Path)
		}
		break
	}
}

// LatestForClosure-style divergence warning: compare the outputs of
// two builds with identical input closures.
func warnDivergence(st *store.Store, prev, cur *store.Manifest, wd string) {
	sameOutputs := len(prev.Blobs) == len(cur.Blobs)
	if sameOutputs {
		for i := range prev.Blobs {
			if prev.Blobs[i].Hash != cur.Blobs[i].Hash {
				sameOutputs = false
				break
			}
		}
	}
	fmt.Printf("[brine] DIVERGENCE: same input closure built twice with different results\n")
	fmt.Printf("[brine]   previous key %s, current key %s, outputs identical: %v\n",
		prev.Key, cur.Key, sameOutputs)
	if sameOutputs {
		fmt.Printf("[brine]   (outputs match — only the key moved: hidden input like commit/env/toolchain)\n")
		return
	}
	// Outputs differ with identical inputs: dissect.
	for i := range cur.Blobs {
		if i >= len(prev.Blobs) || prev.Blobs[i].Hash != cur.Blobs[i].Hash {
			a := filepath.Join(".brine", "blobs", prev.Blobs[i].Hash[:2], prev.Blobs[i].Hash)
			b := filepath.Join(wd, cur.Blobs[i].Path)
			if rep, err := dissect.Diff(a, b); err == nil {
				if rep.ContentEquivalent {
					fmt.Printf("[brine]   dissection of %s: content-equivalent (archive naming only) — not a divergence\n", cur.Blobs[i].Path)
					return
				}
				fmt.Printf("[brine]   dissection of %s:\n", cur.Blobs[i].Path)
				for _, line := range strings.Split(rep.String(), "\n") {
					if line != "" {
						fmt.Printf("[brine]     %s\n", line)
					}
				}
			}
			break
		}
	}
}

// filterBuildState removes observed reads that live under OUTPUT ROOTS:
// top-level directories containing build-fresh files (cargo's target/,
// brine CI's bin-exp/). Build systems read their own intermediate
// state there (cargo's fingerprints) — neither source nor stable
// input; keeping it makes receipt validation fail whenever outputs
// are wiped. Ancestor-mtime was tried first and is too fragile: any
// stray touch of a source dir nukes the whole read-set (fact case
// study).
func filterBuildState(wd string, files []string, builtAt int64) []string {
	roots := outputRoots(wd, builtAt)
	var out []string
	for _, f := range files {
		top := f
		if i := strings.IndexByte(f, '/'); i > 0 {
			top = f[:i]
		}
		if roots[top] {
			continue
		}
		// Files WRITTEN during the build are outputs, not inputs — even
		// when observed as reads: make opens its outputs to check
		// freshness before relinking (flat make projects have no output
		// dirs for the root filter to catch). A file modified by the
		// build cannot be an input to that same build.
		if info, err := os.Stat(filepath.Join(wd, f)); err == nil && info.ModTime().UnixNano() > builtAt {
			continue
		}
		out = append(out, f)
	}
	return out
}

// outputRoots finds top-level directories containing anything modified
// after builtAt (bounded walk).
func outputRoots(wd string, builtAt int64) map[string]bool {
	roots := map[string]bool{}
	entries, err := os.ReadDir(wd)
	if err != nil {
		return roots
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if dirHasRecent(filepath.Join(wd, e.Name()), builtAt, 0) {
			roots[e.Name()] = true
		}
	}
	return roots
}

func dirHasRecent(dir string, builtAt int64, depth int) bool {
	if depth > 6 {
		return false
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	for _, e := range entries {
		info, err := e.Info()
		if err != nil {
			continue
		}
		if info.ModTime().UnixNano() > builtAt {
			return true
		}
		if e.IsDir() && dirHasRecent(filepath.Join(dir, e.Name()), builtAt, depth+1) {
			return true
		}
	}
	return false
}

func unionFiles(a, b []string) []string {
	seen := make(map[string]bool, len(a)+len(b))
	var out []string
	for _, f := range a {
		if !seen[f] {
			seen[f] = true
			out = append(out, f)
		}
	}
	for _, f := range b {
		if !seen[f] {
			seen[f] = true
			out = append(out, f)
		}
	}
	return out
}

// cmdRecord: run with full instrumentation, then write discovered
// configuration to .brine.json. The "learn your build" command.
func cmdRecord(args []string) {
	command := strings.Join(args, " ")
	wd, _ := os.Getwd()

	// Load/store setup
	st, err := store.New(filepath.Join(wd, ".brine"))
	if err != nil {
		fatal("store: %v", err)
	}
	if os.Getenv("BRINE_REMOTE") == "1" {
		os.Setenv("BRINE_GOCACHE_REMOTE", "1")
		st.SetRemote(gocache.NewRemoteFromEnv())
	}

	goPatterns := key.GoPatterns(args)
	isGo := goPatterns != nil
	isDocker := isDockerBuild(args)

	fmt.Printf("[brine] record: running %s with full instrumentation\n", command)
	buildStart := time.Now()
	builtAt := buildStart.UnixNano()

	// writes tracking for non-go
	var writes *observe.Writes
	if !isGo && boolOr(loadConfig(wd).Writes, true) {
		writes = observe.NewWrites(wd)
		defer writes.Close()
	}
	writesActive := writes != nil && writes.Active()

	pc := detectProtocolCache(wd, args)
	if pc != nil {
		fmt.Printf("[brine] record: protocol-cache %s configured\n", pc.Tool)
	}

	reads := observe.NewReads()
	cmd := exec.Command(args[0], args[1:]...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if isGo {
		if self, err := os.Executable(); err == nil {
			statsPath := filepath.Join(wd, ".brine", fmt.Sprintf("gocache-stats-%d.json", os.Getpid()))
			cmd.Env = append(os.Environ(),
				"GOCACHEPROG="+self+" gocache",
				"BRINE_GOCACHE_STATS="+statsPath,
				"BRINE_GOCACHE_SCOPE="+strings.Join(goPatterns, " "))
		}
	}
	applyProtocolCache(cmd, pc)
	applyProtocolCache(cmd, pc)
	if err := cmd.Start(); err != nil {
		fatal("start: %v", err)
	}
	if !isGo {
		if _, err := os.Stat("/proc"); err == nil {
			go reads.TrackProcTree(cmd.Process.Pid, 20*time.Millisecond)
		} else {
			go reads.Track(cmd.Process.Pid, 50*time.Millisecond)
		}
	}
	if err := cmd.Wait(); err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			os.Exit(exitErr.ExitCode())
		}
		fatal("run: %v", err)
	}
	buildTime := time.Since(buildStart)

	// read-set discovery (same as cmdMemoize)
	var readSet []string
	if isDocker {
		readSet = dockerContextClosure(wd, args)
	} else if isGo {
		readSet, err = key.GoClosure(wd, goPatterns)
		if err != nil {
			fatal("closure: %v", err)
		}
	} else {
		var writtenSet map[string]bool
		if writesActive {
			writtenSet = map[string]bool{}
			for _, f := range writes.Files(wd) {
				writtenSet[f] = true
			}
		}
		filterWrites := func(files []string) []string {
			if writtenSet == nil {
				return files
			}
			var out []string
			for _, f := range files {
				if !writtenSet[f] {
					out = append(out, f)
				}
			}
			return out
		}
		observed := filterWrites(filterBuildState(wd, unionFiles(reads.Files(wd), untrackedConfigs(wd)), builtAt))
		if len(observed) > 0 {
			readSet = observed
			if gitFiles := gitTrackedFiles(wd); gitFiles != nil {
				readSet = unionFiles(readSet, filterWrites(gitFiles))
			} else {
				// no VCS: observed reads alone may miss files; add the
				// full-project walk as a sound superset
				readSet = unionFiles(readSet, filterWrites(noVCSFallback(wd)))
			}
		} else if gitFiles := gitTrackedFiles(wd); gitFiles != nil {
			readSet = filterWrites(filterBuildState(wd, unionFiles(gitFiles, untrackedConfigs(wd)), builtAt))
		}
	}

	if len(readSet) == 0 {
		fmt.Println("[brine] record: no read-set derivable")
		return
	}

	// store receipt
	toolchain := key.BuildConfig(wd)
	keyCommand := command
	if isDocker {
		for _, t := range dockerBuildTags(args) {
			keyCommand = strings.ReplaceAll(keyCommand, " "+t+" ", " ")
		}
	}
	k := key.FromFiles(wd, readSet, keyCommand+"|"+toolchain+vcsFold(wd)+keyTail(wd))

	var m *store.Manifest
	if isDocker {
		m = &store.Manifest{Key: k, Created: time.Now()}
		for _, tag := range dockerBuildTags(args) {
			if id, err := dockerImageID(tag); err == nil {
				m.ImageOutputs = append(m.ImageOutputs, store.ImageRef{ImageID: id, Tag: tag})
			}
		}
	} else {
		m, err = st.SnapshotOutputs(k, wd, readSet, builtAt)
		if err != nil {
			fatal("snapshot: %v", err)
		}
	}
	m.Command = command
	m.Strategy = "record"
	m.ReadSet = readSet
	m.BuiltAt = builtAt
	m.VCSCommit = gitHead(wd)
	m.KeyTail = keyTail(wd)
	outputDirs := map[string]bool{}
	for _, b := range m.Blobs {
		p := b.Path
		for {
			if i := strings.LastIndexByte(p, '/'); i > 0 {
				p = p[:i]
				outputDirs[p] = true
			} else {
				break
			}
		}
	}
	for _, d := range m.OutputDirs {
		outputDirs[d] = true
	}
	for _, b := range m.Blobs {
		outputDirs[b.Path] = true
	}
	m.InputHashes, m.DirListings = store.BuildProvenanceFiltered(wd, readSet, outputDirs)
	reportKey(k)
	if err := st.PutManifest(m); err != nil {
		fatal("manifest store: %v", err)
	}

	// buildinfo for advisory
	var buildInfoVCS string
	for _, ref := range m.Blobs {
		if info, err := buildinfo.Inspect(filepath.Join(wd, ref.Path)); err == nil && info != nil {
			buildInfoVCS = info.VCSRevision
			break
		}
	}

	fmt.Printf("[brine] record: built in %s, %d outputs, %d inputs\n",
		buildTime.Round(time.Millisecond), len(m.Blobs), len(readSet))

	// check for divergence (prior build with same closure)
	divergence := false
	if prev := st.LatestForClosure(key.ClosureHash(wd, readSet), k); prev != nil {
		divergence = true
	}

	// collect + record
	advisories := collectAdvisories(wd, isGo, readSet, divergence, buildInfoVCS, writesActive)
	if len(advisories) == 0 {
		fmt.Println("[brine] record: nothing to advise")
	} else {
		fmt.Printf("[brine] record: %d advisory(ies)\n", len(advisories))
	}
	if err := recordConfig(wd, advisories); err != nil {
		fmt.Fprintf(os.Stderr, "[brine] record: config write: %v\n", err)
	}
}

func cmdStats() {
	wd, _ := os.Getwd()
	runs, err := metrics.LoadAll(filepath.Join(wd, ".brine"))
	if err != nil {
		fmt.Println("no metrics (run brine <command> first)")
		return
	}
	s := metrics.Summarize(runs)
	fmt.Print(s.Format())

	// also show cache state
	cacheBytes, manifests, blobs := CacheStats(wd)
	fmt.Printf("\ncache: %d manifests, %d blobs, %.1f MB\n",
		manifests, blobs, float64(cacheBytes)/1024/1024)

	// recent runs
	fmt.Println("\nrecent runs:")
	count := len(runs)
	start := count - 5
	if start < 0 {
		start = 0
	}
	for _, r := range runs[start:] {
		status := "MISS"
		if r.Hit {
			status = "HIT "
		}
		fmt.Printf("  %s %s  %dms  %s\n",
			r.Timestamp.Format("15:04:05"), status, r.WallMs, r.Command)
	}
}
