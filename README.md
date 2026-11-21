# cimemo

I kept watching CI rebuild the same binaries from the same source and
wondered why nobody memoized the whole command. So this.

It wraps any build command. On unchanged inputs it restores the outputs
from a content-addressed store. The build tool never runs.

```
$ cimemo go build -o app ./cmd/app
$ cimemo go build -o app ./cmd/app     # CACHE HIT, ~50ms
```

## The parts that actually matter

The input closure. A go build's closure is `go list -deps` — exact,
recomputable, no guessing. For everything else it's git-tracked files
plus whatever the sensors pick up. A clean tree at a known commit is
also a closure (the SHA determines the content), so there's a fast
path that skips closure computation entirely.

The output snapshot. Only files the build wrote — not the source, not
the user's uncommitted edits. fsnotify tells us what was written during
the build; mtime is the fallback when the tree is too large to watch.

The hard part turned out to be everything that's *invisible to git*:
untracked config files a `./configure` step wrote, env vars that
change the output, submodule contents behind a gitlink. Each one
needed its own mechanism, and each one was found by a test case that
broke before the fix.

The safety net is the divergence detector: same inputs, different
output → dissect the binaries (ELF/Mach-O/ar section diff) and name
the likely cause. It caught a rust rlib archive-format quirk, an
upstream bug in a project I was testing against, and several of my
own mistakes before I understood them.

## What I've run it against

Go up to 2M lines (StackRox), Rust + eBPF in a container, C/C++ in a
dockerized builder, flat C make, npm/JS, docker builds, python
setuptools. The StackRox CI experiment: 2.7s to restore three binaries
plus an image receipt from a fresh runner, 189s cold, byte-identical
to a plain build. That number is gated in CI — it fails if they differ.

## Honest edges

- Needs a git repo (or explicit key_files config) for the closure
- Outputs must land inside the working directory
- Env vars can't be observed, only declared (`key_env` in config)
- Bazel/Gradle compose but I haven't exercised them
- Write-tracking caps at 4096 directories (mtime fallback works)

## If you want to try it

```
go install github.com/davdhacs/cimemo/cmd/cimemo@latest
cimemo make      # or cargo, npm, docker build, whatever
cimemo make      # hit
cimemo record make   # discovers your project's hidden inputs, writes config
```

Everything I measured, every bug I found, every case study with real
numbers: [SENSORS.md](SENSORS.md).
