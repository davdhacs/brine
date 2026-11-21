# cimemo sensor architecture

Multi-sensor dependency discovery + fusion, with learned edges validated
on master/nightly runs (`--shadow`) and on demand (`cimemo audit`).

## Sensors

Active (exercised in tests):

| # | Sensor | Tier | Package | When |
|---|--------|------|---------|------|
| 1 | go-list closure (`go list -deps`, embed/cgo/test files expanded) | exact | `key.GoClosure` | pre-exec |
| 2 | lsof read observation | heuristic | `observe` | during |
| 3 | git-tracked fallback | sound/conservative | `cmd` | pre-exec |
| 4 | learned edges (changed→output history) | heuristic→validated | `model` | pre-exec |
| 5 | fsnotify change distribution | meta | `watch` | continuous |
| 6 | output content diffs (blob store) | ground truth | `store` | post-exec |

Planned (TODO, not exercised yet — see `sensor/sensor.go`):

| # | Sensor | Tier | Notes |
|---|--------|------|-------|
| 7 | compiler dep-files (gcc/clang `-MD`, rustc `dep-info`) | exact | TODO: c/c++ case study needed |
| 8 | dry-run/verbose traces (`go build -x`, `make -n`, `tsc --listFiles`) | exact | TODO |
| 9 | GOCACHEPROG pass-through proxy + miss log | exact | ACTIVE: `gocache` package, see below |
| 10 | coverage profiles (test selection) | exact | TODO |
| 11 | git history mining (seed model from commit co-changes) | statistical | TODO |
| 12 | BuildKit/Docker layer provenance | exact | TODO |

## Fusion rules

1. Union for coverage; exact beats heuristic. A heuristic never removes
   an edge an exact sensor found.
2. Negative evidence ("rebuilt to identical bytes") never becomes a
   learned absence — only downgrades urgency, never correctness.
3. Unknown files (no edges, not in any closure) → conservative: full rebuild.
4. Cheapest-sufficient: fsnotify/mtime delta → learned edges → closures.

## Validation modes

- `--shadow`: compute predictions, do NOT trust them, build fully,
  score predictions against ground truth, harvest evidence. Zero
  behavioral difference. Intended: always-on on master/nightly runs.
- `cimemo audit <cmd>`: force full rebuild now, score, repair model,
  mark edges validated, report divergences. For release certainty.

## Correctness rules (learned from v0.3 bug)

- Snapshots store OUTPUTS ONLY (files created/modified during the
  build that are not inputs). Restoring must never touch user files.
- Warm-path hit requires: read-set unchanged (mtime+hash), input
  directories unchanged (listing hash), toolchain unchanged.
- Learned edges serve hits only after surviving an audit/shadow
  validation (default off: `--trust-learned`).

## Test ladder

1. k8swatch (small): full scenario matrix + model learning + shadow + audit.
2. Prometheus (mid): same matrix, compare against v0.1 numbers.
3. StackRox (large): local main-binary builds, compare timing/cache
   against the custom CA solution (az-gocache / ca-build branches).

## Results (v0.4.0, 2025-11-20, macOS, local disk blob store)

| Scenario | k8swatch | Prometheus | StackRox (central, 4,590 inputs) |
|---|---|---|---|
| cold (full build + snapshot) | 138ms | 12.0s | 70.5s1 |
| warm hit (validate + restore) | ~0ms | 79ms | 203ms |
| README/non-input change -> hit | yes, edit survives restore | yes | yes |
| non-dep .go change -> hit | - | yes (promtool) | yes (roxctl) |
| dep .go change -> miss+rebuild | 147ms | ~11s | 32s |
| revert -> keyed hit (full restore) | 67ms | 422ms (136MB) | 1.6s (327MB) |
| audit: prediction + recall | 100% | 100% | 100% |
| model after audits | 1 edge, validated | 1 edge, validated | 1 edge, validated |

1 machine's go build cache partially warm; CI-cold would be minutes.

vs v0.1 (git-tracked keys): Prometheus warm 280ms -> 79ms; README and
non-dep changes now hit instead of missing (exact closure vs whole-tree).

vs custom CA solution (CI): structurally identical mechanisms
(content-addressed blobs, exact closures, per-input keys, warm
validation, provenance) achieved generically -- no Makefile recipes, no
per-project tooling. Absolute times are not comparable (local disk vs
CI runners + docker assembly), but the warm path does the same work:
validate inputs, restore/link outputs, skip the build.

## Case studies: UI (vite/npm) and flat C (sqlite, 2026-11-22)

**Vite+TS (npm run build)** — receipt 41ms vs 2.4s; source AND
package-lock changes miss correctly (dependency identity is sound via
the tracked lockfile). The generic path (observed∪git fallback +
commit receipts) covers JS toolchains with zero language-specific
code.

**sqlite amalgamation (plain make, FLAT project)** — receipt 76ms vs
9s; make accepts the restore ("up to date"). Found and fixed the
flat-project gap, three parts:
1. make opens its OUTPUTS to check freshness → lsof sees them as
   reads → the observed∪git union classified .o files as inputs →
   snapshot excluded them (empty receipt) AND wiping them invalidated.
   Fix: the build-write invariant — a file modified during the build
   (mtime > builtAt) is an OUTPUT of that build, never an input,
   applied to read-set filtering (both observed and git-fallback
   paths, which also covers committed artifacts) and to the snapshot's
   input-exclusion rule.
2. root-level output FILES (no output dir) changed the "." listing →
   listing exclusion now covers output blob paths, not just dirs.
Divergence detector earned its keep here too: it flagged the empty
receipt ("same closure, different outputs") before the fix.

## Invisible inputs: the final architecture (2026-11-22)

Question: can write-tracking observation replace the config/env
mechanisms? Answer: no — each covers a disjoint blindness. Validated
across three cases (fixtures in git history of this section):

**A. config written OUTSIDE cimemo** (manual ./configure; cimemo make):
config.h is an INPUT. Handled by the read-set (untracked-config
patterns join git∪observed before the writes filter). Flip →
"MISS: changed: config.h" → correct rebuild. Commit receipts are
guarded: an untracked config on disk that the receipt does not
produce (not a blob) skips the fast path.

**B1. config generated IN-BUILD from TRACKED source** (make runs
configure reading a committed flag): config.h is an OUTPUT whose
content is a function of tracked inputs. The writes filter removes it
from the read-set; the snapshot captures it; the receipt restores
config.h + binary together (validated: wipe both → HIT → both
restored, exec bit preserved). Flip the tracked flag → miss. Flip
back → keyed hit (non-go commands gained a pre-build keyed check with
stored-InputHashes verification).

**B2. config generated IN-BUILD from ENV**: the output depends on
something NO file sensor can observe. Without CIMEMO_KEY_ENV the
receipt is stale (irreducible — this is where Bazel is strictly
stronger: it sandboxes the environment, making undeclared env
influence impossible; cimemo wraps existing builds and cannot
sandbox). With CIMEMO_KEY_ENV=FEATURE: flip → "MISS: untracked config
or key env changed" → correct rebuild; flip back → receipt hit.
Honest caveat found en route: after the env-flip MISS, cimemo re-runs
the build — but MAKE ITSELF may decline to rebuild (program: program.c
has no env dependency; also make's same-second staleness). cimemo
detects what it can see; the build system's own dependency
declarations remain the floor below which nothing can help.

Mechanism inventory (final):
- WRITES (fsnotify during build): outputs — read∧written = output
  (make freshness), created-in-op = generated input, immune to mtime
  granularity. Best-effort (dir cap; mtime invariant is the fallback).
- UNTRACKED CONFIGS → read-set (input when pre-existing; output when
  generated — the writes filter is the disambiguator). NOT keyed:
  keying a generated config conflicts with its output role.
- CIMEMO_KEY_ENV / CIMEMO_KEY_FILES: irreducible opt-in declarations
  for influence that flows through no file.
- File mode (exec bit) now recorded per-blob and restored.

## Invisible inputs: untracked configs and env (2026-11-22)

Found live (autotools-style fixture): `./configure --enable-foo` writes
config.h into the source tree — UNTRACKED (often gitignored), existing
before the build (not a build-write), invisible to git-based keys.
Worse: git diff-index ignores untracked files, so even COMMIT receipts
("clean tree at HEAD") say nothing about config.h. Result before the
fix: flip the config, receipt HIT, WRONG-CONFIGURATION BINARY SERVED —
a stale serve, not a post-hoc divergence.

Fix — keyTail folds invisible input classes into EVERY key (fast path,
keyed, commit, warm-comparison, docker):
1. untracked config artifacts (config.h, config.status, CMakeCache.txt,
   *.cmake, local.mk, .env...) — hashed + AUDIT-warned ("keyed, but
   uncommitted — commit it for shared determinism")
2. CIMEMO_KEY_ENV="CFLAGS,CXXFLAGS,..." — named env vars folded in
   (env changes output without changing any file)
3. CIMEMO_KEY_FILES="*.conf" — user escape hatch for anything else

The warm path needed its own guard: it validates the read-set without
recomputing keys, so the tail is stored on the manifest (KeyTail) and
compared — mismatch = "MISS: untracked config or key env changed".

Validated: config flip → MISS → correct rebuild; flip back → receipt
HIT; env flip → MISS. Also observed en passant: make's own same-second
staleness (dependency rewritten within the same second as the target =
"up to date") — cimemo inherits the build system's granularity for
rebuilds, but the keyed path is content-hashed and immune to it.

## Positioning: cimemo vs Bazel (2026-11-22)

Bazel/Buck2 solve the key-closure problem by CONSTRUCTION: explicit
declared dependencies, hermetic actions, content-addressed remote
caching and execution. cimemo retrofits memoization onto whatever
build system you already have. The trade:

| | Bazel | cimemo |
|---|---|---|
| migration | rewrite the build (BUILD files for every package; person-years at StackRox scale) | none — wrap the command |
| soundness | by construction (declared deps) | discovered (sensors + audits + ground-truth gates) |
| caching granularity | actions | whole outputs (receipts) + protocol tiers |
| remote execution | yes | no (caching only) |
| build systems | the ones you port to it | go, cargo, npm, make, cmake, docker, ... |

Composition, not just competition:
- cimemo receipts sit ABOVE bazel invocations (memoize the whole
  `bazel build` including analysis phase and workspace setup);
  Bazel's remote cache sits within. No conflict — different layers.
- cimemo's GOCACHEPROG/ccache/sccache tiers are the no-migration
  equivalent of Bazel's remote cache for teams that cannot port.
- The sensor stack (observation, divergence detection, ground truth)
  exists precisely because legacy builds don't declare dependencies:
  it is the amortized cost of avoiding the Bazel migration, paid in
  tooling rather than in BUILD files. Bazel's answer to the same
  problem is to make the question impossible to ask.

Where Bazel wins outright: hermetic reproducibility guarantees,
remote execution (distributed compilation), polyglot graph queries.
Where cimemo wins: zero adoption cost, works on the build you have
today, receipt-level caching (Bazel still runs its analysis phase on
every invocation; a receipt skips the tool entirely).

## Certainty model (2026-11-22)

Question: are receipts byte-for-byte certain? Answer now enforced and
measured at three levels:

**Guaranteed (by construction):**
- remote-sourced blobs: sha256(content) == key on arrival; corrupt or
  poisoned blobs degrade to misses, never serve wrong bytes
- ALL blobs re-verified on restore (local ones too — bit rot in the
  local store previously would have served silently; opt-out
  CIMEMO_TRUST_LOCAL=1 for ~1s/GB)
- keyed receipts hash every input file (no mtime trust); commit
  receipts are git-certain (clean tree at SHA ⟹ exact contents)
- docker image receipts fold the REGISTRY-CURRENT base digest
  (docker manifest inspect): a moved base tag changes the key and
  rebuilds — the update check. Unresolvable registries fall back to
  the locally-cached digest (local-drift protection only).

**Measured (CI gate, every run):**
- GROUND TRUTH leg: the receipt-restored binary is byte-compared
  against a PLAIN no-cimemo build of the same tree — independent of
  our pipeline on the producing side. (Prior round-trip checks only
  proved our own store consistent with itself.)
- buildinfo Verify on restore (vcs.revision, ldflags)

**Advisory (audit layers):**
- buildinfo audit: VCS stamps, external ldflags
- pin audit (`pinaudit`): floating dependency references — unpinned
  docker FROMs (with the resolved digest named in the warning),
  missing/untracked lockfiles (cargo/npm; go+go.sum is the exemplar
  and reports clean by design), and floating refs in build scripts
  (:latest, @master/@main, go install @latest, shallow clones of
  moving branches). Real findings in the case-study repos: stackrox
  deploy yamls reference :latest images; collector's vendored grpc
  Dockerfiles use golang:latest.

**Residual known risks (documented, not yet closed):**
- non-go toolchain env in receipt keys (gcc version, CGO_CFLAGS for
  cgo builds) — the receipt sits above the toolchain; go's own action
  IDs catch it for the action cache, receipts do not. Mitigation
  path: toolchain fingerprint folding for non-go commands.
- the local warm path (LatestManifestForCommand → Validate) trusts
  mtimes; superseded by the hash-certain keyed path on fresh machines
  but still reachable locally.

## Full pipeline leg (2026-11-22): three binaries + image

The CA-scope equivalent: `./central` + `./sensor/kubernetes` +
`./sensor/admission-control` built via receipts, then a docker image
bundling them (itself an image receipt).

| pass | wall | detail |
|---|---|---|
| cold | 189.2s | 3 binaries + image; shared closure dedups live (sensor hit 2,522 actions uploaded by the CONCURRENT populate job) |
| **receipt** | **2.7s** | all 3 binary receipts + image re-tag; ALL BINARIES IDENTICAL |

vs custom-CA whole-job warm ~36s (incl ~30s checkout/setup): the cimemo
command work is now ~free; remaining job wall is structural setup
(checkout + setup-go + module-cache restore + mtime normalization
~40s). Docker image receipts are keyed on the CONTEXT closure (hash of
every context file — untracked files included, the sound choice for
image builds; .cimemo/.git excluded to avoid self-reference; the
COPYied binaries are inputs to the image command even though they are
outputs of the binary commands).

Remaining for full CA-feature parity (roadmap): roxctl multi-platform
matrix, UI build caching, the real image recipe (gen-dockerfile),
assemble-manifest, cross-machine image receipts (docker save/load).

## Docker builds: how they fit (2026-11-22)

Container builds DO fit the cimemo model — at two levels — and the
docs-recommended composition is three nested caches:

1. **Image-level receipts (cimemo)**: `cimemo docker build -t x .` —
   key = context closure (git-tracked; the daemon reads the context
   where the host cannot observe), output = image ID, restore =
   re-tag. Validated: same-context build to any tag hits in ~70ms;
   context change misses. Tags are stripped from the key (output
   names, not inputs — found via the divergence detector). This is
   the CA branch's proven receipt→re-tag fast path, generalized.
   Local receipts need the image present; cross-machine = docker
   save into the blob store / registry push keyed by receipt (the CA
   work's docker layer cache is the ported form).
2. **Layer cache (docker's own)**: BuildKit `--cache-from/--cache-to`
   IS a content-addressed remote cache — use it alongside cimemo, not
   under it (changed-context builds skip unchanged layers).
3. **Inner compiler caches**: RUN steps that compile (go/ccache/sccache)
   carry their own protocol tiers inside the layers.

What does NOT fit naively: host-side observation of docker builds
(structurally blind — the daemon reads the context), and file-output
snapshots (outputs are images, hence the ImageOutputs manifest type).

## C/C++ case study (collector, 2026-11-21-22)

Dockerized-builder pattern: host make → docker exec → cmake inside a
builder container with the source MOUNTED — outputs land back on the
host. Results (host-level cimemo wrapping `make -C collector collector`):

| scenario | plain | cimemo |
|---|---|---|
| unchanged | ~26s (make no-op) | **HIT 1.6s** |
| build tree wiped | 210s | receipt restores 1.5GB incl cmake-build |
| build tree wiped + ccache | 210s | **31.3s** (98.9% compile hits) |

- receipt restore: make/cmake accept it except 2 kernel-header-dependent
  files (scap_bpf.c, scap_kmod.c) — 99.85% acceptance
- ccache via PATH shims in the builder (no cmake changes), CCACHE_DIR
  outside the repo so caches stay orthogonal
- ccache 4.11 has only file-based remote storage; cross-machine ccache
  = sync CCACHE_DIR through the blob store (files are content-addressed)
  — future side-cache work
- orchestrator blindness fix: observed read-sets are unioned with
  git-tracked (host lsof saw ONE file — the Makefile — for the whole
  build; the real reads happen inside the container)

## sccache (rust protocol tier, 2026-11-22)

- local: fact-api+ebpf cold 16.1s → wiped-target rebuild **5.1s**
  (116/116 hits, 100%)
- **cross-machine via sccache's native Azure backend**: fresh
  container, fresh cache dir → 116/116 hits from the `sccache` blob
  container (env: SCCACHE_AZURE_CONNECTION_STRING +
  SCCACHE_AZURE_BLOB_CONTAINER — note the SCCACHE_ prefix; the
  unprefixed names fail SILENTLY, zero cache-write errors)
- the rust layer stack: unchanged → cimemo receipt 0.58s; wiped tree
  → sccache 5.1s; cold machine → azure-sccache 18.5s (cross-region
  from a laptop VM; in-region CI will be far faster)

## Rust case study (fact, 2026-11-21) — full findings

Full `fact-api` + `fact-ebpf` (eBPF compiles — needs Linux: container
with clang + vendored libbpf headers via CPATH; Debian's libbpf is too
old for bpf_ksym_exists). Note: the `fact` BINARY itself is broken at
repo tip (E0600: `!running` on &OutputStatus with no Not impl —
upstream bug, not ours).

Receipts (in-container, 641MB target tree):
- cold build 12.4s → receipt restore **581ms, cargo accepts 100%**
  (0 recompiles) after the mtime-preserving restore fix
- warm: HIT

Determinism findings:
- **rust rlibs are NOT byte-reproducible**: ar member names carry
  per-build random suffixes (rustc codegen-unit temp names) and
  lib.rmeta-link embeds them; member CONTENT is 100% deterministic.
  Consequence: byte-keyed rlib caching cannot dedupe; cimemo's
  divergence detector would false-positive → dissect gained ar-archive
  mode (member-content multiset, name-listing members ignored) and
  reports ContentEquivalent.
- **eBPF objects (clang -target bpf) ARE byte-identical** across clean
  rebuilds (dissect: format=elf, no differing sections).

Robustness fixes born from this case study (each found live):
1. gitlink blindness: submodules are gitlinks in `git ls-files`;
   hashed-as-file = silently invisible → gitlink pseudo-inputs
   (`path@gitlink:<HEAD>`) hashed via `git rev-parse` everywhere.
2. git safe.directory: tar-copied/container checkouts trip git's
   ownership check → cimemo silently degraded to no-cache → all git
   calls carry `-c safe.directory=`.
3. observed read-sets contained cargo's target/** state → filtered by
   output roots (ancestor-mtime was too fragile — a stray dir touch
   nuked the whole set).
4. lsof-on-top-PID only sees the orchestrator (cargo), not workers
   (rustc) → Linux observation now polls the process TREE via /proc.
5. cargo's mtime-based freshness rejected restored outputs (15/170
   recompiles) → restores preserve build-time mtimes → 0 recompiles.

## Nondeterminism detector + dissector (2025-11-21)

`cimemo` now warns when binaries change but files don't: manifests
carry a ClosureHash (inputs only); a build whose closure matches a
previous manifest's but whose key or outputs differ triggers a
DIVERGENCE warning and an automatic dissection (section-level diff via
stdlib ELF/Mach-O parsers + buildinfo comparison + date-string scan).
Validated: a `-ldflags -X main.buildStamp=$(date)` build is caught and
named ("buildinfo mismatch: ldflags ... != ..."), identical repeats
stay silent. `cimemo dissect A B` runs it manually. Rust/C++ cause
knowledge (vergen, __DATE__, DWARF paths, ar member mtimes) documented
in dissect/dissect.go pending case studies.

## CI receipt legs (go build ./central, fresh runners)

Best run 2026-11-22 (per-target manifest scope + upload fix):

| leg | wall | detail |
|---|---|---|
| control (cold) | 326.2s | baseline |
| populate | 401.7s | cold + upload 3.4GB / 6.8K actions + receipt |
| serve-receipt | **5.7s** | receipt hit: binary restored, go never ran, byte-identical |
| serve-change | **183.1s** | receipt miss, 6.7K actions prefetched, **5 recompiles** (2.8MB) + link |

vs custom-CA (CI): warm 36s whole job / 1-binary change 7-10min.
cimemo beats both legs for the go-build scope. The remaining
serve-change time is go's warm-build floor: action-ID hashing of the
whole closure + link (~160s on 4-core runners; ~25s on an M1).

Earlier runs: 13.8s/210.6s (2026-11-21), 284.7s serve pre-mtime-fix.
Regression fixed en route: putFileGzip Stat()'d a closed handle —
every CI action upload failed silently for one run (found via the
uploads: N FAILED log line).

## Cross-runner action-ID drift — root cause & fix (2025-11-21, CI)

Symptom: fresh-runner serves from the cloud recompiled ~1,255 of 5,196
compile actions on EVERY run, deterministically, despite identical go
version, GOFLAGS, gcc, runner image, CPU, and go env. Same-runner
builds were stable (bisect: 2nd build on one runner = 4 puts).

Root cause (via GODEBUG=gocachehash=1 probes on two runners, diffed):
**go's package-index hash ("moduleIndex") embeds file NAME + MTIME +
SIZE for the stdlib source tree, the module cache, and the source
tree.** Each runner extracts/restores these at a different wall-clock
time, so every action whose ID includes moduleIndex differs per
runner. Content hashes keep outputs byte-identical — only cache
matching breaks. (The GOGCCFLAGS /tmp/go-buildNNN prefix-map diff is a
red herring: per-invocation but normalized in action IDs, proven by
the same-runner bisect.)

Fix: normalize mtimes to a fixed epoch before building:
  sudo find /opt/hostedtoolcache/go -exec touch -d @946684800 {} +
  find "$(go env GOMODCACHE)" -exec touch -d @946684800 {} +
  find . -exec touch -d @946684800 {} +
Result: serve = 5,185 hits / 4 puts (link+buildid only), byte-identical.

Post-fix CI numbers (go build ./central, fresh runner, fresh cache,
all from cloud): serve 231s = 38s prefetch (manifest bloated with
drifted-era junk; clean store → ~12s) + ~190s go warm-build floor
(action-ID hashing of ~8,800 files + link). That floor is why Level-1
receipts exist: unchanged inputs skip go entirely.

## gocache (sensor #9) results — v0.4.0, 2025-11-20

Local pass-through proxy over the standard GOCACHE layout (cohabits
bare `go` builds in both directions — verified). go 1.27 accepts the
protocol (put responses MUST carry DiskPath+Size). Miss log feeds
cimemo manifests: exact work accounting per build (e.g. Prometheus
one-file change = 2 misses / 4 puts / 41MB; StackRox central = 8,838
actions tracked). BuildConfig (CGO_ENABLED/GOOS/GOARCH/GOFLAGS) folded
into the key — closes the env-change false-hit vector.

Remote tier (Azure, zero-dep net/http port of gocache-serve):
concurrent GET dispatch, concurrent upload flush, integrity check on
cloud GETs, module-identity manifests (go.mod + build config) with
startup prefetch. Correctness proven end-to-end: byte-identical
binaries served to a fresh local cache from the cloud, at k8swatch
(903 actions) and Prometheus (3,654-action closure) scale.

Honest performance finding: from this machine (220ms RTT to
westus3), the remote tier LOSES to local recompile (k8swatch: 19.7s
prefetch vs 5.3s recompile; Prometheus: 162s vs 40s). Root cause is
not bandwidth or the backend (86+ req/s available from parallel curl)
but request-rate: go's GOCACHEPROG client asks at ~GOMAXPROCS rate,
and cross-region RTT bounds bulk prefetch. The tier's value is
in-region (CI runners, 2-5ms RTT): the CA branch measured the same
architecture at 4.3m wall-clock vs hours of cold builds there.
Remote mode is opt-in (CIMEMO_GOCACHE_REMOTE=1); local pass-through
is the default and always on for Go commands.

Bugs found and fixed during the port: response-write race between
the main loop and dispatch goroutines (interleaved JSON killed a
Prometheus build — caught live, all writes now mutex-serialized,
-race clean); sequential upload flush (476s for one Prometheus
populate — now concurrent); empty-body put markers polluting the
manifest (330 of 1,233 actions uncacheable by nature — filtered);
container delete/create race in the test harness (all PUTs 404'd —
harness bug, not proxy).

Known wrinkle: the manifest merge is a union, so historically
recorded empty actions persist (one wasted tiny GET each at
prefetch); repopulate with a fresh m/ blob to clean.

Known bug fixed during this pass: filepath.SkipDir on a dot-FILE
(.DS_Store) in the repo root silently ended the tree walk -- outputs
were never captured. Dot-files are now skipped individually; only
dot-directories prune the walk (same fix applied to SnapshotDir and
key.Compute).
