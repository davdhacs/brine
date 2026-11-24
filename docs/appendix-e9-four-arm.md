# Appendix E9: Four-Arm Incremental Build Comparison on curl

## Overview

We compare four incremental-build strategies on 10 consecutive commits
of curl/curl (master, November 2026), replayed chronologically on
GitHub Actions ephemeral runners (ubuntu-latest, 4 vCPU). Each arm
runs in its own repository with its own 10 GB cache quota, isolating
cache state. A calibration probe (pinned sqlite amalgamation compile)
and cpuinfo are recorded per run to normalize for the 6 distinct CPU
models observed across the runner fleet.

The build mirrors one linux.yml job from curl's own CI: a
mbedtls-latest dependency build plus a curl autotools build
(autoreconf + configure + make with --enable-unity). No changes to
curl's build system, no new Makefile targets, no repository
modifications beyond a GitHub Actions workflow file.

## The Four Arms

### Arm A: actions/cache (the incumbent)

Curl's own CI caching pattern: actions/cache for the mbedtls
dependency (version-keyed), no build-tree caching. The curl build
itself starts from zero on every commit. This is the baseline.

Configuration:
- actions/cache@v6 for ~/pkg/mbedtls (key: bench-Linux-mbedtls-4.3.0-X64)
- Full rebuild of curl on every commit (autoreconf + configure + make)

### Arm B: ccache through brine serve

Per-file content-addressed compilation caching. ccache 4.12.3 with
CCACHE_REMOTE_STORAGE pointing at a local brine serve instance that
fronts the GitHub Actions cache v2 backend (Results API).

Configuration:
- CC="ccache gcc", CCACHE_COMPILERCHECK=content (hash compiler binary,
  not mtime — required for ephemeral runners)
- CCACHE_REMOTE_STORAGE=http://localhost:7899 (brine serve)
- Same dep handling as arm A (actions/cache for mbedtls)

### Arm C: brine receipt + mtime normalization

Whole-tree restoration via content-addressed receipts. On a receipt
miss, brine's incremental mode restores the parent commit's bld/
directory (one bundle fetch), then make's own freshness logic decides
what to rebuild.

Configuration:
- .brine.json: {"incremental": true, "output_patterns": ["bld/**",
  "configure", "*.in", "missing", "install-sh", "compile", "depcomp",
  "aclocal.m4"]}
- Source mtimes normalized to commit dates (git log --format=%ct)
- Restored bld/ stamped to (current commit date - 30s)
- Dep handled by brine receipt (closure=key_files on tarball content)

### Arm D: actions/cache + mtime normalization

The incumbent's infrastructure (actions/cache) combined with the mtime
technique from arm C. This isolates "what does mtime normalization
alone buy you within the existing paradigm" from "what requires brine's
content-addressed receipts."

Configuration:
- actions/cache@v6 for bld/ (key: bench-bld-X64-<sha>, restore-keys
  prefix fallback for cross-commit restore)
- actions/cache@v6 for ~/pkg/mbedtls (same as arm A)
- Source mtimes normalized to commit dates
- Restored bld/ stamped to (current commit date - 30s)
- Always runs autoreconf (~7s); skips configure when bld/Makefile exists

## Results

### Medians (warm — excluding first cold run, n=9 each)

| arm | strategy | median | IQR | range |
|-----|----------|--------|-----|-------|
| A | actions/cache (incumbent) | 34.5s | 25.5–38.4 | 23.6–43.6 |
| B | ccache + serve | 80.0s | 64.6–98.1 | 49.4–119.9 |
| C | brine receipt + mtime | 21.7s | 11.4–23.9 | 7.0–25.2 |
| D | **actions/cache + mtime** | **15.7s** | 10.3–23.1 | 6.8–27.9 |

### Per-commit detail (build phase, seconds)

| commit | diff (files/lines) | A | B | C | D |
|--------|-------------------|---|---|---|---|
| m~10 | (cold start) | 33.5 | 226.9 | 49.3 | 12.8 |
| m~9 | 2/4 | 34.5 | 49.4 | 10.8 | 7.9 |
| m~8 | 2/12 | 23.9 | 101.6 | 21.7 | 24.0 |
| m~7 | 9/280 | 23.6 | 80.0 | 25.0 | 27.9 |
| m~6 | 1/4 | 41.4 | 74.0 | 25.2 | 13.4 |
| m~5 | 1/2 | 43.6 | 77.7 | 7.0 | 6.8 |
| m~4 | 1/13 | 27.0 | 55.2 | 12.0 | 12.8 |
| m~3 | 1/16 | 34.5 | 119.9 | 22.8 | 15.9 |
| m~2 | 1/9 | 35.4 | 94.6 | 18.2 | 15.7 |
| m~1 | 1/3 | 31.4 | 90.0 | 22.0 | 22.1 |

### Normalized (build seconds / calibration seconds)

| arm | median normalized | interpretation |
|-----|------------------|----------------|
| A | 1.56 | baseline |
| B | 3.56 | slower than no caching |
| C | 0.96 | near-parity with calibration |
| D | 0.72 | faster than calibration (incremental) |

## Key Findings

### Finding 1: mtime normalization is the primary technique

Arms C and D both use source-mtime normalization (stamping files with
their last-change commit date) and restored-output stamping (bld/
stamped to just before the current commit). This single technique
enables make's own freshness logic to work across ephemeral runner
boundaries, turning any build-tree cache into an incremental cache.
Both arms beat the incumbent by >35%.

Without mtime normalization (arm A), even a perfectly restored bld/
directory would be useless: the checkout stamps all sources with
"now", which is newer than any restored .o file, causing a full
rebuild.

### Finding 2: arm D (incumbent + mtime) is the fastest

Arm D uses the same GitHub Actions cache as the incumbent, just with
the build tree cached and mtimes normalized. It achieves 15.7s median
(warm), 55% faster than the incumbent's 34.5s.

Arm D beats arm C (brine receipts, 21.7s) because it avoids the
receipt store cost. actions/cache saves bld/ as one tar.gz (single
upload, no per-file verification), while brine receipts snapshot +
hash + upload each changed blob individually with verify-after-upload.
For CI where every commit stores a new receipt, the store cost adds
5-10s per commit.

### Finding 3: ccache's per-file model is architecturally mismatched
for ephemeral runners

Arm B achieves a 99% remote hit rate (2 misses per commit average
across 116 compile units) — the caching works correctly. But it is
net slower than no caching at all. The problem: configure runs ~50
small test compiles, each paying the GHA cache's per-request latency
(~1-2s per remote lookup through serve → twirp → SAS → blob →
verify). The make phase (2-4 compiles) benefits, but configure
dominates wall time.

This is a granularity result, not a tool result. ccache's per-file
content addressing is the right abstraction for local builds (instant
local disk lookups). On GHA's ephemeral runners, the per-request
latency dominates. The right abstraction for remote caching is bundle
restoration (one large fetch), not per-file lookup.

### Finding 4: CCACHE_COMPILERCHECK=content is required for ephemeral
runners (F19)

ccache defaults to compiler_check = mtime. On ephemeral runners, every
fresh gcc install has a different mtime, silently invalidating ALL
remote hits. Setting CCACHE_COMPILERCHECK=content (hash the compiler
binary) is mandatory for any cross-runner ccache setup. Without it,
ccache shows 0 hits regardless of remote storage correctness.

### Finding 5: the incumbent's specific waste (W1-W4)

Reading the incumbent's workflow against the results:

- W1 (biggest miss): bld/ never cached — 100% of compile work repeated
  every commit. Fixed by arm D (cache bld/ with restore-keys).
- W2: autoreconf + configure re-ran even when bld/Makefile could be
  restored. Arm D skips configure (~15-20s) when bld/ exists.
- W3: no mtime normalization — even with a bld/ cache, checkout stamps
  all sources "now" > restored .o files → full rebuild. The mtime
  technique (arms C and D) fixes this.
- W4: dep cache keyed on version string (not content) — correct but
  a re-release of the same version would serve stale results. Brine's
  content-addressed dep receipt (arm C) hashes the tarball content.

## Methodology Notes

- **Runner heterogeneity**: 6 CPU models observed (Xeon 8573C, Xeon
  6973P-C, Xeon 6975P-C, EPYC 7763, EPYC 9V45, EPYC 9V74). The
  calibration probe (compile sqlite amalgamation, median of 3) varies
  15.7s–24.9s across the fleet. Per-commit CPU assignment is random
  and uncontrolled; normalized results (build/calibration) control for
  this.

- **The unity build**: curl uses --enable-unity, concatenating all
  sources into a single compile unit. Any source change recompiles the
  entire unity file. This caps the minimum incremental build time at
  ~5-7s (one unity compile + link) regardless of diff size. Without
  unity builds, 1-file changes would compile in <2s.

- **The cold run**: arm B's cold run (227s) is inflated by 116 remote
  PUTs (each with CreateCacheEntry + upload + Finalize + verify). Arm
  C's cold (49s) includes receipt snapshot + upload of 625 blobs. Arm
  D's cold (13s) is fast because actions/cache saves in the background
  post-step (not counted in the build timer).

- **Contamination**: arm B's clean batch required two prior batches
  (one with a missing serve step, one with default COMPILERCHECK).
  Arm D's first two attempts failed on autotools-output handling
  before the always-autoreconf approach stabilized.

## Graph Data (JSON, per-commit chronological)

See `docs/data/e9-4arm-graph.json` for machine-readable per-commit
data with build_ms, dep_ms, calib_ms, cpu model, and conclusion for
all four arms.

## Implications for brine

The four-arm comparison clarifies brine's positioning:

1. **The mtime technique is the key innovation** — it works within any
   caching paradigm (proven by arms C and D both using it
   successfully). This technique alone provides 55% improvement over
   the incumbent.

2. **Brine's value-add is correctness, not raw speed** for this
   workload. Arm D (actions/cache + mtime) is faster than arm C (brine
   + mtime) because it skips receipt-store overhead. Brine's advantages
   are:
   - Content verification (verify-on-restore, divergence detection)
   - Content-addressed keys (immune to stale caches on version re-releases)
   - Selective restore (fetch only what's needed)
   - Cross-repository sharing (same content = same key)
   - Remote execution support (worker + remote-build)

3. **The ccache finding validates bundle-granularity caching**: the
   per-file model (ccache) fails on GHA's latency profile, while the
   bundle model (arm C/D's single tar.gz or brine's bundle receipts)
   succeeds. This is a general result about caching granularity on
   ephemeral CI infrastructure.
