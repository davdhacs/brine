# brine

Content-addressed memoization for build commands.

```
$ brine go build -o app ./cmd/app   # cold: builds, stores receipt
$ brine go build -o app ./cmd/app   # CACHE HIT
```

Input closures: `go list -deps` for Go (exact, recomputable); git-tracked
files plus sensors for everything else. Clean tree at a known commit is
itself a closure, so there's a fast path that skips closure computation.

Output snapshots capture only what the build wrote. fsnotify gives exact
write semantics during the build; mtime is the fallback for large trees.

Things git can't see — untracked configs, env vars, submodule contents —
each have their own mechanism (read-set patterns, `key_env` declaration,
gitlink hashing). `brine record <cmd>` discovers a project's hidden
inputs and writes `.brine.json`.

Divergence detector: same inputs, different output → binary dissection
(ELF/Mach-O/ar section diff) with likely causes named. Every blob is
re-verified on restore. CI gate compares receipt-restored binaries
against plain builds, byte-for-byte.

Tested: Go (2M lines), Rust + eBPF, C/C++ (dockerized builder, flat
make), npm/JS, Docker, Python. StackRox CI: 2.7s to restore 3 binaries
+ image from a fresh runner (189s cold).

Edges: needs git (or explicit `key_files`), outputs must land in the
working directory, env vars are declared not observed, Bazel/Gradle
untested, write-tracking caps at 4096 dirs.

```
go install github.com/davdhacs/brine/cmd/brine@latest
brine make
brine make           # hit
brine record make    # discovers hidden inputs, writes config
```

Design notes: [docs/sensors.md](docs/sensors.md) ·
[docs/certainty.md](docs/certainty.md) ·
[docs/case-studies.md](docs/case-studies.md) ·
[docs/positioning.md](docs/positioning.md) ·
[docs/config.md](docs/config.md)
