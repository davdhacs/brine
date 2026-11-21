# cimemo

Content-addressed memoization for build commands. Wrap any build
command; cimemo discovers its input closure, stores outputs in a
content-addressed blob store, and on unchanged inputs restores them —
the build tool never runs.

```
$ cimemo go build -o bin/app ./cmd/app     # cold: builds, stores receipt
$ cimemo go build -o bin/app ./cmd/app     # unchanged: CACHE HIT in ms
```

Measured (see SENSORS.md for methodology and full numbers):

| scenario | result |
|---|---|
| unchanged inputs, fresh machine (3 binaries + image) | 2.7s vs 189s cold |
| one-file change, fresh runner | 183s vs 326s cold |
| byte-for-byte certainty | CI gate: receipt == plain build, every run |

## How it works

Three receipt kinds, tried in order of cost:

1. **Commit receipts** — a clean git tree at HEAD is exactly determined
   (SHA → tree → contents); the receipt key is the commit. Skips
   closure computation entirely.
2. **Content receipts** — the input closure (go: `go list -deps`;
   else: observation + git-tracked union), every file hashed.
3. **Warm validation** — manifests record input hashes and directory
   listings for incremental checking.

Outputs are stored as content-addressed blobs (SHA-256 sharded), with
a local disk store and an optional Azure Blob remote (zero
dependencies — plain net/http). Every blob is re-verified on restore;
remote-sourced blobs verify on arrival.

## The sensor stack

Input closure discovery is multi-sensor, per toolchain:

- **go list -deps** — exact closure (source, cgo, embed patterns), pre-execution
- **/proc process-tree observation** (Linux) — what the build actually read
- **git-tracked fallback** — sound superset; unioned with observation
  (orchestrator builds read inside containers where hosts can't see)
- **GOCACHEPROG pass-through proxy** — go's compile cache as a remote-
  capable tier (cohabits the standard GOCACHE layout; miss log feeds
  the learned model)

## Safety layers

- **Divergence detector + dissector** — same inputs, different output
  triggers section-level binary dissection (ELF/Mach-O/ar) with likely
  causes named (rust ar member naming, VCS stamps, debug paths)
- **buildinfo audit** — go binaries inspected post-build for
  memoization-blind inputs (vcs stamps, external ldflags); VCS state
  key-folded so cherry-picks miss correctly
- **pin audit** — floating dependency references reported as
  nondeterministic build config (unpinned docker FROMs with resolved
  digests, missing/untracked lockfiles, :latest/@master refs)
- **shadow / audit modes** — predictions scored without trust
  (`--shadow`), forced rebuilds with repair (`cimemo audit`)

## Requirements

Go 1.24+ (GOCACHEPROG tier), git. Optional: Azure storage account for
the remote tier (`CIMEMO_GOCACHE_REMOTE=1` + env config).

Full design notes, measured results, and case studies (k8swatch,
Prometheus, StackRox central, stackrox/fact with eBPF,
stackrox/collector): [SENSORS.md](SENSORS.md).
