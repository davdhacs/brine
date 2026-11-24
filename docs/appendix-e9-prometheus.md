# Appendix E9-P: Four-arm comparison on prometheus (Go)

## Motivation

The curl four-arm comparison (Appendix E9) showed that mtime restoration
(a technique within brine) beat the incumbent caching approach for
C/C++ builds. This experiment repeats the methodology on a large Go
project to test whether the result generalizes across languages and
build systems.

Prometheus is a fitting subject: a widely-used monitoring system with
~114 Go packages, a 143 MB binary, and an upstream CI that already
caches `GOCACHE` and `GOMODCACHE` via `actions/cache` with
`restore-keys` — the well-optimized incumbent case.

## Setup

Ten consecutive commits from `prometheus/prometheus` master
(1-13 files changed each). Four arms, one GitHub repository per arm,
ephemeral `ubuntu-latest` runners, identical workflow shape:

| arm | technique |
|-----|-----------|
| A | `actions/cache@v6` on `GOCACHE` + `GOMODCACHE` (the incumbent) |
| B | A + binary output caching keyed on the checked-out commit SHA |
| C | brine `gocache` (GOCACHEPROG proxy to the GHA cache v2 backend) |
| D | brine receipt memoization on the whole `go build` command |

All arms include a runner calibration probe (sqlite amalgamation
compile, median of 3) and `cpuinfo` recording. Each arm ran the
10-commit sequence cold → warm; the build step is timed to the
millisecond and the binary is verified with `./prometheus --version`.

Notable methodology bugs caught during validation (before the batch):
- `restore-keys` on a binary cache restores a *stale* binary from a
  previous commit — the version check passes because any valid binary
  reports a version. Fixed by exact-SHA keys only.
- `github.sha` in a `workflow_dispatch` build refers to the workflow
  repository's commit, not the checked-out project commit; binary keys
  must use `inputs.commit`.
- brine receipt arm D without `GOMODCACHE` caching spent ~45 s
  downloading modules just to compute the closure for the receipt key.

## Results

| commit | files | A | B | C | D |
|--------|-------|-----|-----|-----|-----|
| 03bfb649 | 13 | 177.6 | 0.0* | 161.9 | 361.1 |
| 2b311b97 | 3 | 9.4 | 7.8 | 16.8 | 28.4 |
| 6c2b2964 | 1 | 18.2 | 18.4 | 14.6 | 43.2 |
| 06170caa | 2 | 22.8 | 20.7 | 24.1 | 75.7 |
| ae44e72e | 4 | 19.3 | 20.7 | 15.4 | 52.4 |
| f1486b67 | 2 | 17.8 | 14.7 | 24.9 | 81.1 |
| a630a4c4 | 2 | 25.5 | 27.3 | 21.8 | 76.5 |
| 99581e6d | 5 | 14.6 | 25.8 | 26.0 | 43.8 |
| a5022ef6 | 4 | 16.2 | 14.9 | 15.8 | 74.3 |
| ad8f26ee | 1 | 25.1 | 16.3 | 16.2 | 35.2 |

\* binary-cache hit (same-commit re-run; the cold run for arm B was
invalidated by the two key bugs above and is not reported — its true
cold cost equals arm A plus ~5 s of binary-store overhead).

Warm statistics over the nine incremental commits:

| arm | median | mean | min | max |
|-----|--------|------|-----|-----|
| A: actions/cache | 18.2 s | 18.8 s | 9.4 s | 25.5 s |
| B: A + binary cache | 18.4 s | 18.5 s | 7.8 s | 25.8 s |
| C: brine gocache | 16.8 s | 19.5 s | 14.6 s | 26.0 s |
| D: brine receipt | 52.4 s | 56.7 s | 28.4 s | 81.1 s |

Runner calibration medians were 18.9-20.0 s across arms (sqlite probe),
so the fleet was comparable between arms.

## Findings

**F20 — Go's incremental compilation is already content-addressed,
and external receipt memoization is redundant overhead for it.**
Arms A, B, and C are statistically equivalent (~18 s median). Go's
`GOCACHE` keys packages by content hash, so restoring the cache
directory alone yields correct incremental builds regardless of the
transport (actions/cache tarball or brine's bundle store). Whole-command
receipts (arm D) add ~34 s median overhead: the 143 MB binary upload
with verify-after-upload dominates, and the receipt can never hit
across commits because the closure changes every commit. The technique
that won for C/C++ (restore outputs, let the build system's freshness
check skip work) wins nothing for Go because the build system's own
cache already skips the work at finer granularity.

**The cold start inverts the warm picture slightly.** brine's gocache
(arm C, 161.9 s) beat actions/cache (arm A, 177.6 s) on the first
build: bundle uploads pack many small GOCACHE entries into few large
blobs, avoiding the per-entry latency that actions/cache's directory
snapshot does not suffer but that naive per-entry remote stores do.
Arm D's cold start (361.1 s) pays for both the GOCACHE and the binary
receipt.

**Same-commit re-runs remain the one case where binary caching wins
outright** (arm B's 0 s hit): no compiler invocation, no linking. But
CI re-runs of an unchanged commit are rare compared to incremental
builds of new commits.

## Threats to validity

- Ten commits from one repository; Go projects with heavier code
  generation or cgo might behave differently.
- Runner CPU variance (calibration range 15-26 s) exceeds the gap
  between arms A, B, and C; only arm D's deficit is unambiguous.
- Arm B's cold-run data point was lost to the key bugs; its warm data
  is from a separate batch run after the fix.
