# brine roadmap

Working task list — maintained so any session can pick up the thread.
Status legend: [x] done+validated, [~] in progress, [ ] next up.

## Bazel-additive arc

- [x] `brine serve` — HTTP remote cache for ccache/sccache/Bazel clients.
  ccache 100% remote hit; Bazel 969/969 actions; CAS direct verification;
  sha256 envelope for opaque keys (ccache sha1 `xx/` layout).
- [x] `brine bazel-generate` (Go) — gazelle v0.53.0 orchestration + go-list
  validation. StackRox 1,832 packages clean; tamper tests pass.
  `--fix` (sorted insert), `--excludes` (receipt read-sets, un-tag
  convergence), MODULE.bazel wiring + `bazel mod tidy`.
- [x] `brine bazel-generate --parity` — provable code equivalence:
  `-buildid=redacted` flag match, ARM64 address-immediate masking,
  23,929/23,929 words explained. Validated both directions.
- [x] `brine worker` + `brine remote-build` — execution layer.
  Real two-machine run: mac → tone (Linux homelab), prometheus 148MB,
  cold 197s / receipt-hit 2s / verified restore 33s.
  Hardened: startup auth probe, /restart endpoint, upload-failure
  passthrough, GetStreamingRaw (stall detection), EnsureRemote.
- [x] `brine bazel-generate` (C/C++) — dry-run discovery, depfile capture,
  flag-signature libraries, combined compile+link, linkopts.
  sqlite 3.50.4 PARITY-IDENTICAL on tone (make == bazel version output).
- [x] curl validation (tone) — **PARITY-IDENTICAL** (curl 8.16.0,
  make == bazel version output). 465 compiles, 7 packages, autotools +
  libtool + automake. The full campaign: 20+ findings, all fixed — see
  "curl campaign findings" below.
- [x] prometheus gazelle re-validation (tone) — **114 packages,
  validation clean** after --fix closed a real gap: gazelle excludes
  .pb.go files from srcs (expects proto_library to generate them) but
  prometheus uses gogo-protobuf codegen — the checked-in .pb.go ARE
  sources. Go-list validation caught it; --fix repaired 4 findings.
  Full bazel build blocked on gazelle-bzlmod external-repo gap:
  go_deps downloads k8s.io/client-go without BUILD files for every
  package (kubernetes/typed/discovery/v1 et al.) — prometheus itself
  works around this with custom scripts. Documented as known limitation;
  fix = run gazelle against external repos or use go_deps' gazelle
  generation attributes.
- [x] **collector remote-build end-to-end (mac → tone)** — cmake build
  inside Docker builder container, receipt hit (494f54a632ce6410),
  898 outputs / 1.6GB restored to mac with sha256 verification,
  82MB ELF x86-64 binary runs. Total 21m9s = 13.6min closure
  (git-tracked fallback: docker exec is opaque to observation,
  ~20K files) + 7.5min eager download of all outputs (898 files
  where only the binary matters). Two optimization targets below.
- [~] Docker-native closure (`key_images`) — fold the builder image
  digest into the receipt key. `docker inspect` gives:
  `.Image` (sha256 content address for the entire toolchain —
  gRPC, protobuf, gcc, sysroot — one hash replaces hashing
  thousands of system files) and `.Mounts` (the bind-mount table,
  folded in as the host↔container mapping). The containerized-
  builder pattern bind-mounts source (never COPY: no context
  transfer, outputs flow back out, source mutates / image is
  immutable), so the closure splits cleanly: image digest covers
  the environment half, normal host-side hashing covers the
  mounted source half. Remaining source-side cost needs
  key_files/depfile precision (~200 files from depfiles vs 20K
  git-tracked) — the collector case study already proved 1.6s
  receipt hits with that closure. Design: `key_images` config
  listing image refs (resolved via docker inspect at closure
  time) + mount table from the running container; no hashing
  inside the image ever happens.
- [~] Remote-build report-only + selective restore — separate
  "build succeeded" from "fetch the bytes" (Bazel REAPI model:
  ActionResult vs CAS fetching). Plan:
  1. `remote-build` default becomes report-only: dispatch →
     receipt key → print output summary (count, bytes, notable
     paths from the manifest, which is already fetched — ~270KB
     metadata) → exit. No blob downloads.
  2. New `brine restore <key> [pattern]` — selective fetch by
     path pattern, verify-on-restore, place at manifest-relative
     paths. `brine restore <key>` = everything.
  3. `remote-build --restore` keeps the eager behavior for
     callers that want it.
  Collector numbers: full eager 7.5min/1.6GB → report-only ~0s
  + selective binary fetch ~30s/82MB. Combined with the closure
  fix, warm remote-build target: ~30s end-to-end.
- [ ] cel-go-style fidelity experiment for a C/C++ project.

## Real-world C/C++ findings so far (keep for the paper)

1. make continuations: `\` + trailing whitespace + newline (sqlite).
2. `$(CURDIR)` absolute paths; macOS /tmp symlink divergence.
3. Combined compile+link commands (sqlite shell: `cc -o bin a.c b.c`).
4. Same source, two flag sets → separate cc_library targets.
5. System include leakage (`/usr/include`); `includes = ["."]` banned.
6. `-l/-L` linkopts required for the link to succeed.
7. Header closures existing ONLY in the link's compilation (sqlite:
   amalgamation includes commented out; shell.c is the sole header
   reader — observation was the only way to find it).

## Infrastructure

- [~] tone worker (192.168.1.135:8081, no sshd — everything via
  /execute dispatches, base64-encode scripts to avoid quoting bugs).
  SPN secret invalid (AADSTS7000215); bridged with mac-minted user
  token (hourly expiry). Durable fix: SPN secret VALUE in worker.env.
- [ ] serve batch endpoint — one round-trip for multi-blob restores.
- [ ] CC parity gate — extend masked-immediate comparison beyond
  Go/ARM64 (C objects on ELF x86_64 via tone builds).

## Larger roadmap (from earlier planning)

- [ ] 6-month StackRox warm replay + normalized second pass (hit-rate
  comparison vs the 75% mtime-noise plateau).
- [ ] Public-project warm experiments with known CI timings (prometheus).
- [ ] Paper: warming curves as first-class metric, certainty model,
  security analysis, observation-vs-declaration findings (sqlite #7!).
- [ ] cmake/ninja discovery polish (BRINE_CC_DRY exists; classify
  `ninja -t commands` output shapes).
- [ ] Generated-header distinction in CC mode (currently both in hdrs).
- [ ] Per-file copts splitting for mixed-flag directories.

## Process lessons (this arc)

- Never chain `git push` after a failed `go build` — two broken commits
  on master this arc, both from exactly this.
- Shell-quoting bugs in one-off dispatch scripts: python variable names
  expanded by the shell wrote an EMPTY token (use sys.argv); comments
  containing literal backslash-newline break out of `//` comments.
- The tamper/regression tests (cctest, sqlrep) catch generator bugs
  faster than real-project runs — run them before every push.
- az blob list paginates — always use --prefix.
