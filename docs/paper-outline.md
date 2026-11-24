# Paper outline: brine

Working title: "Memoizing Builds by Observation: Content-Addressed
Caching Without a Build Language"

Format: ACM acmart (tectonic), plain 1996-era academic style, real
citations only (docs/prior-art.md). No LICENSE-repo self-promotion;
brine is the artifact, the technique is the contribution.

---

## Part 1 — What the paper must demonstrate (decided first)

A scientific paper needs: a falsifiable thesis, controlled
experiments against baselines, repeated measurements with variance,
reproducible artifacts, and threats to validity. Our current record
is strong on REAL-WORLD CASE STUDIES (single-run demonstrations on
real projects) and weak on CONTROLLED COMPARISON and STATISTICS.
That gap defines the remaining work.

### Thesis

Build outputs can be memoized safely and profitably WITHOUT
requiring a project to adopt a build system's dependency model:
a content-addressed store keyed on observed input closures, with
layered verification, produces large wall-clock reductions on real
projects while detecting — rather than silently serving — the
failure modes that make ad-hoc build caching untrustworthy.

Three testable sub-claims:

  C1 (soundness): the observed closure is a sound key basis —
     over-inclusive where uncertain, never under-inclusive; ground
     truth (byte-identical rebuilds) confirms hits across real
     project history.
  C2 (efficacy): warm receipt hits reduce wall-clock by an order
     of magnitude+ on real projects, including projects with no
     build system adoption (no BUILD files, no Tupfiles, no Nix
     expressions written).
  C3 (trustworthiness): each verification layer catches a real
     failure mode that naive caching serves silently (poisoning,
     truncation, stale mtime, silent fallback corruption, hidden
     inputs like env and builder images).

### Claims → experiments → status

**Demonstration projects (decided 2025-11-23): the evaluation runs
on PUBLIC, POPULAR projects with REPORTED/optimized CI — not on our
own testbeds. "Project X, famous for build investment, still gets
N×" beats "our toy got faster."**

Primary: **Linux kernel** (make; THE canonical build benchmark;
Phoronix `build-linux-kernel` has 2,330+ public results to anchor
our cold-build baseline as "normal for the hardware"; public ccache
studies = related work AND baseline; in nixpkgs for the Nix
comparison; eorlov.org documents kernel+ccache numbers).
Secondary: **LLVM/Clang** (cmake — the collector path; Phoronix
`build-llvm` 905+ public results; large C++).
Tertiary (language spread, existing): prometheus (Go), collector
(Docker'd cmake), sqlite/curl (tarball/no-VCS), k8swatch.

The CI-variance angle (from public data): the same kernel build
took 27m23s on GitHub Actions vs 9m03s on comparable vCPUs
elsewhere — CI build time is not just slow, it is NONDETERMINISTIC
across infrastructure. Receipt hits are a constant
(content-addressed download), independent of machine — a claim no
build system makes. Fold into C2 as: "deterministic build time."

| # | experiment | demonstrates | status |
|---|-----------|--------------|--------|
| **E9** | **Public CI replay (headline)**: pick a project with publicly visible per-commit CI timings; replay the SAME commits through brine; report per-commit: their public CI time vs our cold vs our warm receipt. Methodology mirrors the StackRox 70/70 history replay but with an EXTERNAL public baseline — "their CI says X per commit (public data); the same commits through receipts take Y." | C2 + the CI-variance claim | DATA SOURCE VALIDATED: GitHub Actions REST API exposes per-commit → per-workflow → per-JOB → per-STEP durations publicly (no auth; 60 req/hr unauth, 5k authed). curl: 32 jobs/workflow, 0.5-7.8 min each. NEED: scrape N commits of curl's Linux (autotools) workflow + replay through brine on tone; optionally LLVM (big numbers, expensive replay). Hardware confound documented via our own cold baseline on the same commits. |
| E1 | End-to-end acceleration: cold / warm / 1-file-change on the kernel + LLVM (public projects, Phoronix-anchored baselines) + the existing case-study spread | C2 | NEED: kernel + LLVM runs on tone (32-thread 7950X3D, 74GB free — kernel fits, LLVM tight). HAVE the case-study numbers. Both need repeated runs (n≥5, median+IQR) and the public-data comparison row. NOTE: Phoronix build-linux-kernel/build-llvm are COLD-build hardware benchmarks (2,330+/905+ public results) — they anchor our cold baseline as "normal for the hardware" but contain NO warm-cache data (nobody publishes warm-cache benchmarks — itself a motivating observation for the paper). Kernel on tone: GCC 16.2 vs kernel 6.12 EFI-stub C23 errors — use kernel 6.19 or -std=gnu17; cold defconfig build measured 57s @ 32 threads (fast machine, small config — consider allmodconfig for a longer baseline or accept it). |
| E2 | Ground-truth gate: replay real project history, fresh build vs receipt restore, byte-identity for every output | C1 | HAVE StackRox master 70/70 green (CA system); brine tamper/regression tests. NEED: port the history-replay harness to brine receipts; ≥100 commits of ≥2 projects. |
| E3 | Adversarial evaluation: attack the cache (poisoned blob, truncated blob, stale-mtime serve, 0-blob overwrite, env flip, builder-image swap) — detection/rejection rates | C3 | HAVE the mechanisms (serve PUT hash check 400s; verify-on-restore; divergence detector; key_env advisories; execenv keying; 0-blob guards) + the live poisoning incident as motivating case study. NEED: consolidated single table, each attack × each layer, pass/fail. Some attacks lack measurements (e.g., truncation detection timing). |
| E4 | Baseline comparison: brine vs Nix binary cache vs tup vs ccache(+remote) vs plain rebuild vs GitHub-Actions-cache-style blob upload, same projects, same machine | C2, C1 | HAVE ccache 100% through serve; Bazel 969/969 actions through serve; plain-rebuild numbers. MISSING: Nix and tup entirely. NEED: Nix binary-cache warm/cold/delta on 2+ case-study projects (all are in nixpkgs); tup on one C project (needs Tupfiles written — itself evidence for the adoption-cost claim). |
| E5 | Adoption cost: time-to-first-cache-hit and lines of configuration per system on a project with NO build files (sqlite amalgamation from tarball, curl from tarball) | C2 | PARTIAL (curl/sqlite case studies exist from brine side). NEED: the same projects through Nix (write derivations or use nixpkgs — count effort) and tup (write Tupfiles). This experiment makes "zero adoption" quantitative. |
| E6 | The dirty-tree delta: 1-file uncommitted change — brine (receipt miss → targeted rebuild) vs Nix (derivation invalidation semantics) vs tup (incremental) | C1, C2 | MISSING. This is the differentiator experiment: observation keys on actual content; declaration keys on expressions. Need: same change, measure all three. |
| E7 | Composition: brine as Bazel remote cache (action-cache hit rates, integrity vs stock cache) + parity gates (masked-immediate instruction-identity proof) | C2 (ecosystem claim) | HAVE 969/969, Abseil cold 2,743 actions, ARM64 parity 23,929/23,929 words explained. NEED: baseline comparison vs a stock Bazel remote cache (e.g., BuildBarn or bazel-remote) for the same build — hit-rate and integrity behavior. |
| E8 | Warming curves: cache utility as a function of history depth (1-month StackRox replay: 38× receipt speedup, 75% action-cache plateau) | C2 | HAVE 1-month data + the mtime-noise finding (75% plateau caused by mtime-keyed action cache). The 6-month replay strengthens it. NEED: decide 1-month suffices or run 6-month (roadmap item). |

### The observation-vs-declaration findings (unique evidence)

These are the paper's most original empirical material — cases where
observed reality diverged from declared/expected structure:

  F1 sqlite shell.c header closure: the amalgamation's includes are
     commented out; shell.c is the sole header reader — only
     observation found the true input closure. (docs: sqlrep)
  F2 curl's 19 findings: libtool wrappers, automake VPATH fragments,
     make compound commands, .lo/.la semantics, --start-lib archive
     groups — each a gap between make's view and the true build.
  F3 gogo-protobuf vs gazelle: gazelle excludes .pb.go from srcs
     (expects proto_library generation); prometheus's checked-in
     .pb.go ARE sources — declaration tooling wrong about reality.
  F4 The 75% mtime plateau: mtime-keyed caching loses 25% of hits
     to mtime noise on a 1-month real history — content addressing
     fixes it (receipt speedup 38×).
  F5 Docker-exec opacity: observation cannot see inside containers;
     the builder image is a hidden input. Fix: docker inspect
     digests as free content addresses (image digest = environment
     closure in one hash).
  F6 The 0-blob poisoning incident (live, 2025-11-23): a silent
     restore failure fell through to a no-op rebuild whose empty
     snapshot replaced the real receipt; the warm path then served
     empty "hits" forever. Motivating case study for the certainty
     model's "loud failure" layer. (roadmap: full write-up)
  F7 Write-tracking invariant: a file modified during the build is
     an output, never an input — resolves the read∧write ambiguity
     that breaks lsof-based approaches.

---

## Part 2 — Research still needed (before or during writing)

  R1 Nix experiments (E4/E5/E6) — biggest gap. All case-study
     projects are in nixpkgs; measure cold/warm/delta; count
     derivation-writing effort for the tarball projects.
     [NEW WORK]
  R2 tup experiment (E4/E6) — write Tupfiles for sqlite; measure
     incremental rebuild. The Tupfile-writing effort itself is
     E5 data. [NEW WORK]
  R3 Statistics discipline (E1) — repeat all headline numbers
     n≥5 on fixed hardware; report median/IQR; document specs.
     [MECHANICAL]
  R4 History replay on brine (E2) — port the CA system's 70/70
     ground-truth harness to brine receipts; extend to ≥100
     commits × ≥2 projects. [MODERATE WORK]
  R5 Baseline for E7 — run bazel-remote or BuildBarn for the same
     Abseil build; compare hit rates and integrity behavior.
     [NEW WORK]
  R6 Read à la Carte carefully; write the formal positioning
     paragraph (brine = verifying rebuilder at command granularity
     + monotone-key store; scheduler-agnostic by construction).
     Consider a short proposition: over-inclusive closure +
     deterministic command ⇒ sound memoization; where determinism
     fails, divergence detection bounds the damage. [WRITING]
  R7 Verify remaining citations (Miller 1998 exact reference;
     Potvin & Levesque CACM 2016; Lamb & Zacchiroli DOI; the
     arXiv Bazel CI study 2405.00796 details). [MECHANICAL]
  R8 Decide the 6-month replay (E8): run it in background during
     writing, include if it lands in time. [BACKGROUND]
  R9 Adversarial table (E3): script each attack against a live
     store; record detection/rejection. Mostly exists as unit
     tests — consolidate and measure. [MODERATE WORK]

---

## Part 3 — Section outline

### 1. Introduction (1.5 pp)
- The problem: CI rebuilds identical artifacts; wall-clock is THE
  CI metric; existing acceleration requires adoption (BUILD files,
  derivations, Tupfiles) or trusts fragile keys (mtimes, paths).
- One concrete hook: the 21m→1.1s collector number or the 38×
  StackRox receipt speedup. [pick at writing]
- Thesis + contributions list (C1-C3, the findings F1-F7 as
  empirical contributions, brine as artifact).
- [NEEDS: R3 for whatever number leads]

### 2. Background and related work (1.5 pp)
- Build-system design space via à la Carte's three axes
  [R6]; declared (Make/Bazel/Shake/Nix) vs observed (Tup,
  Fabricate) dependency knowledge.
- Content-addressed caching lineage: Vesta → Nix → Bazel remote
  caches; distributed compilation (distcc, goma).
- Reproducible builds as the trust foundation (Lamb & Zacchiroli).
- Positioning paragraph: Tup proved syscall observation yields
  exact dependencies but bound it to its build system; Vesta/Nix/
  Bazel proved content addressing but behind declaration barriers;
  brine unbinds observation and applies it to memoization — where,
  unlike incremental building, over-inclusion is sound.
- [NEEDS: R6, R7]

### 3. Design (3 pp)
- The receipt model: input closure, key derivation (content hashes
  + command + toolchain + hidden inputs), content-addressed
  outputs, manifests.
- The sensor stack and its soundness rules: git-tracked fallback
  (over-inclusive), go list (exact for Go), proc/lsof observation,
  depfiles, the write-tracking invariant (F7).
- Hidden inputs: key_env, key_files, key_images; docker-exec
  environment keying (F5).
- The certainty model — four layers:
    L1 construction (over-inclusive closures, content addressing)
    L2 verification (verify-on-restore, ground-truth gates)
    L3 detection (divergence detector, richest-first selection)
    L4 loud failure (no silent fallbacks — motivated by F6)
- Architecture: local store, shared blob store, serve (HTTP remote
  cache speaking ccache/Bazel protocols), worker + remote-build
  (report-only + selective restore — the REAPI ActionResult/CAS
  separation).
- [NEEDS: nothing — design is done and validated]

### 4. Implementation (1 p)
- brine: Go, package/line counts, config tiers, the collector
  .brine.json as a worked example (5 lines of config for a 455-file
  C++/CMake/Docker build).
- [NEEDS: current line counts]

### 5. Evaluation (4-5 pp)
- 5.1 Methodology: projects table (size, language, build system),
  hardware, n≥5 runs, median/IQR. [R3]
- 5.2 E1 acceleration results.
- 5.3 E2 ground truth over history. [R4]
- 5.4 E3 adversarial table. [R9]
- 5.5 E4/E5/E6 baselines: Nix, tup, ccache, plain. [R1, R2]
- 5.6 E7 Bazel composition. [R5]
- 5.7 E8 warming curves + the mtime finding (F4).
- 5.8 The observation-vs-declaration findings (F1-F3, F5-F6) —
  where reality diverged from declaration.

### 6. Discussion (1 p)
- Limitations: env vars irreducibly opt-in; orchestrator blindness
  for dockerized builds (mitigated, not solved, by image keying);
  test selection out of scope; sound-over-inclusive closures pay
  a hashing cost (measured: 455 files/67ms — cheap; the collector
  "20K file" scare was a submodule-counting artifact).
- Cost model: storage per receipt, upload bandwidth (85MB scoped
  vs 1.6GB unscoped — the output_patterns result).

### 7. Threats to validity (0.5 p)
- Internal: single-machine warm measurements; token/network
  variance on Azure.
- External: case studies skew C/C++/Go; npm/cargo covered but
  thin.
- Construct: wall-clock as the metric (matches CI reality but
  ignores cost).

### 8. Conclusion and future work (0.5 p)
- Future: eBPF observation through bind mounts; observation-
  derived external dependencies; Bazel BUILD-file generation
  convergence; 6-month warming data.

Total: ~14 pp main body + references. Target venue decision at
draft time (ICSE/FSE infrastructure track or a systems venue;
à la Carte lineage suggests ICFP is wrong home — we're empirical,
not theoretical).

---

## Order of work

1. R3 statistics harness (mechanical, unblocks E1 table)
2. R1+R2 Nix/tup experiments (biggest scientific gap; also E5/E6)
3. R4 history replay on brine + R9 adversarial table
4. R5 Bazel baseline
5. Writing pass (intro/related first — R6/R7 while experiments run)
6. R8 6-month replay in background throughout
