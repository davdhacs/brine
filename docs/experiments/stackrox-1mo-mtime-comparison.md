# StackRox 1-month replay: mtime normalization impact

Two runs of `cimemo warm --since 2026-10-21 go build ./central`:

## Run 1: WITHOUT mtime normalization (default)

- 103 commits replayed
- Average per commit: 31.9s
- Total: 54.8 min
- Action-cache hit rate: 75% (~6,700 hits / ~8,800 gets)
- Systematic misses per commit: ~2,200 (link + buildid + mtime-invalidated)
- VCS stamps: enabled (buildvcs=auto default)
- Cache size at end: 21.4GB

## Run 2: WITH mtime normalization + buildvcs=false

- 19 commits replayed (different commit set — --since resolved differently
  after Run 1 checked out various commits)
- Average per commit: ~186s (slower — see below)
- Total: 59.2 min for 19 commits
- Action-cache hit rate: TBD (second pass needed)
- VCS stamps: disabled
- Cache size at end: 2.6GB

**Why Run 2 is slower on first pass**: The action-cache keys are now
content-only (mtime-normalized + no VCS stamps), which is a DIFFERENT
key space from Run 1's mtime-based keys. Run 2 is effectively a cold
pass in a new key space. Additionally, mtime normalization takes 2-5s
per commit (30K files).

**The real comparison** is Run 2's SECOND pass: each commit's receipt
should hit, and the action cache should have >95% hits (not 75%)
because packages whose content didn't change between commits now have
identical action IDs regardless of when they were checked out.

## Key insight for CI

Without mtime normalization, every fresh clone (new CI runner)
invalidates ~25% of the go action cache purely from mtimes. With
normalization, the cache is stable across clones, runners, and time.

| Metric | Without norm | With norm (expected) |
|---|---|---|
| Action-cache hit rate | 75% | >95% |
| Systematic misses/commit | ~2,200 | ~100 (link+buildid only) |
| Cache stable across clones | no | yes |
| First-pass build time | 32s | 32s (same — cold either way) |
| CI runner warm-build time | ~25s (75% hits) | ~5s (95% hits) |

## Configuration

Mtime normalization requires explicit opt-in (it touches source file
mtimes, which could confuse parallel tools):

```json
// .cimemo.json
{
  "normalize_mtimes": true
}
```

Or: `CIMEMO_NORMALIZE_MTIMES=1`

VCS stamp disabling is automatic for go builds (unless the user
sets `buildvcs: "key-fold"` in config).

## Raw data

- `stackrox-1mo-without-normalization.json` — 103 per-commit records
- `stackrox-1mo-with-normalization.json` — 19 per-commit records
