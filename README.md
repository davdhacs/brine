# cimemo

Content-addressed memoization for build commands. Wrap any build; on unchanged inputs, cimemo restores the outputs — the build tool never runs.

```
$ cimemo go build -o bin/app ./cmd/app   # cold: builds, stores receipt
$ cimemo go build -o bin/app ./cmd/app   # unchanged: CACHE HIT in ms
```

## What it does

- **Receipts**: hash the input closure (git tree + configs + env), store outputs as SHA-256 blobs. On hit, restore in milliseconds.
- **Commit fast path**: clean tree at a known commit skips closure computation entirely.
- **Docker image receipts**: `cimemo docker build` re-tags on hit instead of rebuilding.
- **Go compile-cache proxy** (GOCACHEPROG): shares go's action cache across machines via Azure Blob.
- **Write tracking**: fsnotify during builds gives exact output semantics (immune to mtime granularity).
- **Config discovery**: `cimemo record <cmd>` runs instrumented, finds hidden inputs, writes `.cimemo.json`.

## Safety

- Every blob re-verified on restore (corruption → refuse, never serve wrong bytes)
- Divergence detector: same inputs, different output → binary dissection (ELF/Mach-O/ar section diff) with likely causes named
- Buildinfo audit: VCS stamps, external ldflags, unpinned docker bases
- Pin audit: floating dependency references (missing lockfiles, `:latest`, `@master`)

## Tested on

Go (up to 2M lines), Rust + eBPF, C/C++ (dockerized builder, flat make), npm/JS, Docker, Python, shell scripts. StackRox CI: 2.7s receipt for 3 binaries + image (189s cold), byte-identical to plain build.

## Quick start

```
go install github.com/davdhacs/cimemo/cmd/cimemo@latest
cd your-project
cimemo make        # or cargo build, npm run build, docker build, ...
cimemo make        # hit
```

Requires: Go 1.24+, git. Optional: Azure Blob for cross-machine sharing (`CIMEMO_REMOTE=1`).

Full design notes and measured results: [SENSORS.md](SENSORS.md).
