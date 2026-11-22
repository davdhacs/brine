# Feature comparison: custom-CA vs brine

What the custom-CA (az-gocache/ca-build branch) had, what brine has,
and what brine has that CA didn't.

## Replicated from CA

| Feature | CA | brine | Notes |
|---|---|---|---|
| GOCACHEPROG proxy with remote tier | ✓ | ✓ | brine: `gocache` package, zero-dep Azure backend |
| Content-addressed receipts (L1 fast path) | ✓ | ✓ | brine: content + commit + keyed receipt types |
| Binary blob caching | ✓ | ✓ | brine: `store` package, SHA-256 sharded |
| Mtime normalization (action-ID drift fix) | ✓ CI workflow step | ✓ automatic (opt-in) | brine: `normalize.go`, gated behind config |
| VCS stamp stabilization | ✓ stable stamps | ✓ buildvcs=false | brine: automatic for go builds |
| Prefetch/manifest machinery | ✓ per job+branding+arch | ✓ scoped per target | brine: simpler key, same dedup |
| Integrity verification | ✓ protocol-level | ✓ blob-level + ground-truth CI gate | brine: stronger (verify-on-restore) |
| Streaming large blobs | ✓ | ✓ | brine: `stream.go`, constant memory |
| Adaptive timeouts | ✓ P95-based | ✓ P95-based | brine: `timeout.go` |
| Negatives/phantom entries | ✓ | ✓ | brine: `negatives.go` |
| Local-first with remote fallback | ✓ | ✓ | brine: same pattern |
| Concurrent upload flush | ✓ | ✓ | brine: `flushUploadsConcurrent` |

## Missing from brine (CA has it)

| Feature | Status | Effort |
|---|---|---|
| Docker save/load (cross-machine image receipts) | TODO | small — `docker save` to blob store on record, `docker load` on restore |
| UI caching (npm/JS side cache) | TODO | small — lockfile-keyed directory snapshot, same pattern as ccache |
| Vendor directory caching | TODO | small — same pattern |
| Multi-arch manifest assembly | TODO | medium — `docker manifest create` from per-arch receipts |
| Trust-scoped blob namespaces | TODO | medium — pr/ vs trusted/ prefixes, access control |
| Blob GC at scale | partial | brine has basic GC by age; Azure lifecycle policies are the CA approach |
| Full metrics infrastructure | TODO | CA had push/report; brine has per-run stats |

## brine has, CA didn't

| Feature | Why it matters |
|---|---|
| Divergence detector + binary dissector | catches nondeterminism; names the cause (ELF/Mach-O/ar section diff) |
| Buildinfo audit | catches VCS stamps, external ldflags in go binaries |
| Pin audit | catches floating dependency references (unpinned FROMs, missing lockfiles) |
| Record mode | `brine record <cmd>` discovers hidden inputs, writes config |
| Cross-command dependency graph | replaces CI `needs:` chains, derived from observation |
| Warm mode | `brine warm --last N` replays history, shows warming curve |
| No-VCS fallback | works on zip-extracted projects (all-files closure) |
| Dep-files sensor | exact C/C++ closures from .d files |
| Config tiers (0/1/2) | zero-config → project → CI, with env overrides |
| Effective-config reporting | shows what's keyed, suggests what to persist |
| Protocol cache auto-config | detects ccache/sccache, wires env vars |
| Write tracking (fsnotify) | exact output semantics, immune to mtime granularity |
| Cache size limiting (LRU eviction) | prevents disk exhaustion on long replays |
| Commit receipts | clean tree at SHA implies closure — skips closure computation |

## Summary

brine replicates the core CA mechanisms (receipts, GOCACHEPROG,
integrity, streaming, prefetch, mtime normalization) and adds the
safety/intelligence layers CA lacked (divergence detection, audits,
record mode, cross-command graph, warm mode). The remaining gaps are
mostly operational (docker save/load, trust scoping, metrics) rather
than architectural.
