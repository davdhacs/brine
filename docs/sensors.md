# Sensor architecture

Multi-sensor dependency discovery: each sensor answers "which files does
this command depend on?" with a different soundness/cost profile.

## Active sensors (implemented and exercised)

| # | Sensor | Tier | Package | When |
|---|--------|------|---------|------|
| 1 | go-list closure (`go list -deps`, embed/cgo/test files expanded) | exact | `key.GoClosure` | pre-exec |
| 2 | lsof read observation | heuristic | `observe` | during |
| 3 | git-tracked fallback | sound/conservative | `cmd` | pre-exec |
| 4 | learned edges (changed→output history) | heuristic→validated | `model` | pre-exec |
| 5 | fsnotify write tracking (exact output semantics) | exact | `observe/writes.go` | during |
| 6 | output content diffs (blob store + divergence detector) | ground truth | `store` + `dissect` | post-exec |
| 9 | GOCACHEPROG pass-through proxy + miss log | exact | `gocache` | during (go only) |

## Cross-command dependency graph

Receipts record what each command reads (read_set) and writes (blobs).
Connecting them gives the data-flow graph — the equivalent of a CI
`needs:` chain, derived from observation:

```
cimemo graph
  configure.sh → cc -o program main.c (via generated.h)
```

When a command is rebuilt, the graph identifies dependent commands
whose receipts are now stale (their inputs changed via the rebuilt
command's outputs, even though their own git-tracked closure didn't
change).

## Planned sensors

| # | Sensor | Tier | Status |
|---|--------|------|--------|
| 7 | compiler dep-files (gcc/clang `-MD`, rustc `dep-info`) | exact | stub in `sensor/sensor.go`; needs a C/C++ case study that generates .d files |
| 8 | dry-run/verbose traces (`make -n`, `tsc --listFiles`) | exact | stub; needs a Makefile-based project |
| ~~10~~ | ~~coverage profiles (test selection)~~ | — | **out of scope**: selection changes what runs (a prediction); cimemo memoizes what ran (deterministic). Coverage data may feed closure discovery; the selection logic is a different tool. |
| 11 | git history mining (seed model from co-changes) | statistical | stub |
| 12 | BuildKit/Docker layer provenance | exact | docker image receipts exist; layer-level provenance is the deeper form |

## Fusion rules

1. Union for coverage; exact beats heuristic. A heuristic never removes
   an edge an exact sensor found.
2. Negative evidence ("rebuilt to identical bytes") never becomes a
   learned absence — only downgrades urgency, never correctness.
3. Unknown files (no edges, not in any closure) → conservative: full rebuild.
4. Cheapest-sufficient: fsnotify/mtime delta → learned edges → closures.

## Write tracking (the output-side invariant)

A file modified during the build (fsnotify event or mtime > builtAt) is
an OUTPUT of that build — never an input — regardless of what the
read-set says. This resolves: make's freshness-check opens (reads of
outputs), configs generated inside the invocation, committed artifacts
rewritten by the build. See `observe/writes.go` for implementation;
`filterBuildState` in `cmd/cimemo/main.go` applies it.

## Invisible input classes

Things git cannot see, each with its own mechanism:

| Class | Mechanism | Found by |
|---|---|---|
| untracked config files | read-set patterns (`config.h`, `CMakeCache.txt`, ...) | case A/B testing |
| env vars | `key_env` declaration (cannot be observed) | B2: stale serve |
| submodule contents | gitlink pseudo-inputs (`path@gitlink:<HEAD>`) | fact case study |
| gitignored configs | `key_files` in `.cimemo.json` (record mode discovers) | gitignore scan |
