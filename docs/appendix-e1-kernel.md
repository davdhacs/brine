# Appendix E1: Linux kernel cold/warm/delta (allmodconfig)

## Setup

Linux 7.3 (tarball, git-initialised), allmodconfig, `make -j32` on an
AMD Ryzen 9 7950X3D (32 threads, 187 GB RAM, SATA SSD). Receipts
stored via the filesystem backend (`BRINE_STORE_BACKEND=fs`) on a
second partition of the same physical disk — the experiment
deliberately isolates brine's own costs from network bandwidth (a
pilot on the Azure backend measured the home uplink at 2.6 MB/s
single-stream: receipt restores were network-bound at 65 s for 465 MB,
measuring the internet rather than the system).

Phases (medians; n in parens):

| phase | n | median | notes |
|---|---|---|---|
| cold build | 3 | 1467 s | plain `sh` build, no brine |
| brine-wrapped build | 1 | 2975 s | +103% wrap overhead (snapshot, observation, bundle, store) |
| receipt hit, fresh machine | 5 | 661 s | wipes build/ and .brine/; restore from fs store |
| delta via receipt | 3 | 3208 s | parent restore + 1-file change + rebuild + NEW receipt store |
| make no-op on restored tree | 5 | 20.5 s | |
| make 1-file delta on restored tree | 5 | 67.5 s | kernel/sched/core.c comment appended |

The receipt covers ~12 GB of outputs (20k+ files, 190+ bundles);
each delta's changed member re-packs its whole 64 MB bundle.

## Findings

**E1.1 — The receipt composes with the build system's own
incrementality.** The interesting composition is restore + make's own
delta: a fresh machine reaches a 1-file-change build in
661 + 68 = 729 s ≈ 2.0× the cold time — the receipt creates the warm
state that make's incremental logic (67.5 s, 21.7× vs cold) then
exploits. The incumbent's 68 s delta exists ONLY on an already-warm
tree; the receipt is what manufactures that tree on a fresh machine.

**E1.2 — Store overhead dominates at kernel scale (the negative
result).** The brine-wrapped delta (3208 s) is SLOWER than cold
(1467 s): each run re-snapshots 12 GB (every output read and hashed
even when the blob already exists — PutBlob dedups the write but not
the read), re-packs changed bundles, and writes blobs to both the
local store and the fs backend. First-store overhead is +103%
(2975 vs 1467 s); subsequent stores cost ~2000 s each. At kernel
scale the store path, not the restore path, is the bottleneck.

**E1.3 — Restore cost is implementation, not bandwidth.** The 661 s
hit restores 12 GB from the same disk that serves it in ~60-100 s of
raw copy time. Decomposition: serial bundle fetches (~190 round
trips), a double hash (streaming fetch verifies, the restore loop
re-verifies), and a local-blob round trip (bundle → .brine/blobs →
output). The first fix — concurrent bundle fetches (ab79adc) — was
measured on the same receipts, same disk: 661 s → 372 s median
(n=3: 340/372/402), a 1.8× improvement and 3.9× vs cold. The
remaining overhead has a clear path to ~100 s (restore directly from
bundle members, single verify).

**E1.4 — Post-restore no-op retains a residue (open).** A make no-op
on the restored tree takes 20.5 s vs ~4 s on a natively-built
(defconfig) tree. Some prepare-chain step still re-runs. Minor, but
it bounds how "invisible" a restore can be to the build system.

## Bugs found and fixed during the experiment

The experiment pipeline itself surfaced five brine bugs, each
validated by a regression test or a live re-measurement:

- F26 symlinks silently dropped from receipts (kernel `O=` builds
  create `build/source -> ..`; `os.Open` follows it into a directory,
  the blob read fails, the link vanishes — restored trees re-ran the
  whole prepare chain). Fixed: symlinks stored as target-path
  content, recreated with `os.Symlink`.
- F27 hidden output files dropped by a blanket dot-file skip —
  `build/.config` and every `.foo.o.cmd` depfile were missing, so a
  restored tree could not even serve a make no-op. Fixed: capture;
  stale noise is filtered by mtime/write rules.
- F24 duplicate-hash existence checks (2,569 empty outputs sharing
  one hash) paid a serial round trip each — 197 s added to hits.
  Fixed: dedup + parallel.
- F25 the filesystem backend itself (added): home-server uplinks make
  network stores the wrong default for local builds.
- Concurrent bundle fetches (E1.3).

## Threats to validity

- Single machine, single kernel version; allmodconfig maximizes
  output volume (~12 GB), which stresses the store path more than
  typical defconfig builds (~470 MB).
- The delta phase ran n=3 before disk exhaustion ended n=4/5; the
  three values (2797/3208/3535 s) are consistent.
- Wrap-overhead attribution (snapshot vs observation vs bundle-pack)
  is not yet decomposed; the end-to-end number is reported.
