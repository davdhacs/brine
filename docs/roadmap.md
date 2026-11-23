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
- [x] Docker-native closure (`key_images`) — DONE and validated
  end-to-end on Docker Desktop: `docker exec <container> ...`
  commands now fold the container's image digest (content address
  for the whole toolchain), mount table (host Source paths dropped —
  receipts share across workers with the repo at different paths),
  and run-time env (sorted) into the receipt key automatically, via
  one `docker inspect` at key time. Validation: same container →
  commit-receipt hit; recreated from busybox → MISS (key moved,
  divergence detector correctly classified "hidden input, outputs
  identical"); mount moved to /data → MISS. `key_images` config
  (BRINE_KEY_IMAGES) is the manual form for docker run shapes.
  Folded through keyTail so every key path (pre-build check, warm
  path, store, record, commit fast-path) stays consistent.
  NOTE: old receipts computed without execenv won't match (one-time
  invalidation — correct: they didn't track the image). Remaining
  source-side cost needs key_files/depfile precision (~200 files
  from depfiles vs 20K git-tracked) — still open, next item.
- [x] Remote-build report-only + selective restore — DONE and
  validated end-to-end (local worker + Azure): remote-build default
  now dispatches and reports the manifest summary only (client holds
  metadata, zero blobs); `brine restore <key> [pattern]` fetches
  selectively (substring or glob match, verify-on-restore);
  `--restore` keeps eager behavior. Collector numbers: eager
  7.5min/1.6GB → report-only ~0s + selective 82MB binary 18.9s.
- [x] **Collector closure precision + the real bottleneck hunt** —
  MEASURED, not theorized: the closure was never the problem (455
  git-tracked files, 67ms to hash — the "20K files" estimate counted
  submodule working trees that `git ls-files` doesn't recurse into).
  The 817s worker time decomposed into a bug cluster found live on
  tone:
  1. **0-blob receipt poisoning**: silent restore failures (root-
     owned output dirs from docker builds — the wrap running as
     `house` cannot write into root-owned `cmake-build/`) fell
     through to no-op rebuilds whose 0-blob snapshots OVERWROTE the
     real receipts (flat PutManifest locally, PutPub remotely) —
     then the warm path served the empty receipts as "hits" forever.
     Fixes (8f852ed): restore failures are now LOUD on every hit
     path (commit fast-path, keyed check, warm path); richer-receipt
     guards on both the content store and the commit alias; warm
     path prefers the RICHEST manifest for a command, not the
     latest.
  2. **Docker-builder ownership pattern**: builds inside docker
     write as root; the host-side wrap cannot restore over them.
     Operational fix: `docker exec <builder> chown -R $(id -u)
     <mount>` after each real build (no-op rebuilds don't rewrite
     outputs, so ownership persists). Future work: automate.
  3. BRINE_DEBUG now prints the exact FromFiles cmd string — the
     divergence-hunting ground truth (proved key stability: byte-
     identical cmd strings across runs; the "two keys" were content
     vs commit-alias keys all along).
  Final measured numbers (tone worker, collector cmake build):
  cold rebuild 13.4s (relink + scoped snapshot + 85MB upload),
  warm dispatch 0.77-0.86s (commit-receipt hit, both binaries
  restored+verified), remote-build report-only end-to-end 1.1s,
  selective binary restore to mac 18.9s. Original: 21m9s.
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
