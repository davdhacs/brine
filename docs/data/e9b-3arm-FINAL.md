# E9 three-arm comparison: FINAL clean data (2025-11-24)

10 commits of curl master, each arm in its own repo (separate 10GB
cache quota), ephemeral runners, calibration probe per run.

## Medians (warm — excluding first cold run)

| arm | strategy | median | range |
|---|---|---|---|
| A | actions/cache (incumbent) | 34.5s | 23.6-43.6s |
| B | ccache through brine serve | 80.0s | 49.4-119.9s |
| C | receipt + mtime incremental | **21.7s** | 7.0-25.2s |

## Arm C is the winner: 1.6× faster than the incumbent

## Arm B finding: high hit rate, wrong granularity

ccache achieves a **99% remote hit rate** (2 misses per commit average
across 116 compile units) through brine serve on the GHA cache, but is
NET SLOWER than no caching at all. The problem: configure runs ~50
small test compiles, each paying the GHA cache's per-request latency
(~1-2s per remote lookup through serve → twirp → SAS URL → blob
→ verify). The make phase (2-4 compiles) benefits, but configure
dominates wall time.

This is a granularity mismatch: ccache's per-file caching is the right
abstraction for local builds but pays 50+ network round trips on an
ephemeral runner. Arm C's bundle restore (one 37MB fetch for the
entire .o tree) is the right granularity for remote caching —
one request, not fifty.

## The `CCACHE_COMPILERCHECK=content` finding (F19)

ccache defaults to `compiler_check = mtime`, which on ephemeral
runners means every fresh gcc install has a different mtime → all
remote hits are invalidated. Setting `CCACHE_COMPILERCHECK=content`
(hash the compiler binary) is REQUIRED for any cross-runner ccache
setup. Without it, ccache shows 0 hits regardless of remote storage
correctness. Found by reading `ccache -p` output during diagnosis.

## Per-commit detail

| commit | A | B | B miss | C |
|---|---|---|---|---|
| 93e2d374 | 33.5 | 226.9 | 116 | 49.3 |
| ff38e43e | 34.5 | 49.4 | 1 | 10.8 |
| 3d2ebfbb | 23.9 | 101.6 | 2 | 21.7 |
| 5a898cb2 | 23.6 | 80.0 | 4 | 25.0 |
| caa0c008 | 41.4 | 74.0 | 0 | 25.2 |
| 64d03b25 | 43.6 | 77.7 | 0 | 7.0 |
| 6714c494 | 27.0 | 55.2 | 3 | 12.0 |
| 7a80cc6c | 34.5 | 119.9 | 3 | 22.8 |
| b79c279f | 35.4 | 94.6 | 2 | 18.2 |
| caa0bc78 | 31.4 | 90.0 | 2 | 22.0 |
