# Case studies

Every number measured; every finding recorded with its fix.

## StackRox (Go, 2M lines)

CI experiment (fresh runners, all from cloud):

| leg | wall |
|---|---|
| control (cold) | 326s |
| populate (cold + upload) | 402s |
| serve-receipt | **5.7s** |
| serve-change (1 file) | 183s (5 recompiles) |
| pipeline (3 binaries + image) | cold 189s / receipt **2.7s** |

Cross-runner drift root cause: go's package-index hash embeds file
MTIMES — fixed by mtime normalization in CI.

## fact (Rust + eBPF, in Linux container)

- receipt restore 581ms (641MB), cargo accepts 100% (0 recompiles)
- sccache local: 5.1s rebuild (3.1×); cross-machine via Azure: 100% hits
- rust rlibs NOT byte-reproducible: ar member names carry random
  suffixes; content is deterministic. Dissect gained ar-archive mode.
- eBPF objects ARE byte-identical across rebuilds
- fix upstream bug found: `fact` binary E0600 compile error at repo tip

## collector (C/C++, dockerized builder)

- receipt: 1.6s vs 210s cold (1.5GB restored)
- ccache via PATH shims: 31.3s rebuild (6.7×), 98.9% compile hits
- 99.85% restore acceptance (2 kernel-header-dependent files)

## Prometheus (Go, 412K lines)

- receipt 422ms (136MB binary restored on revert)
- integration CI (public repo): 355ms receipt vs 132s cold, byte-identical,
  373× speedup

## sqlite (C, flat make)

- receipt 76ms vs 9s; make accepts restore
- found the flat-project gap: make opens outputs for freshness →
  read-set classification conflict → fixed by the write-tracking
  invariant

## Vite+TS (npm)

- receipt 41ms vs 2.4s; source and package-lock changes both miss

## Docker builds

- image receipts: same context → any tag hits in 71ms (re-tag)
- base-digest keying: registry-current digest folded into the key
- context closure: every context file hashed (untracked included)

## Config scenarios (autotools-style)

Three cases: config written outside cimemo (read-set catches flips),
config generated in-build from tracked source (output, restored with
receipt), config generated from env (irreducibly requires key_env
declaration). All validated with fixtures.
