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
- [~] curl validation (tone) — libtool stress test in flight.
  Expected finding: `/bin/sh ../libtool --mode=compile gcc ...` wrappers
  defeat the classifier; needs embedded-command extraction.
- [~] prometheus gazelle re-validation (tone) — 114 packages; a run was
  accidentally started (worker cwd); needs a clean, complete run +
  `bazel build` of a target + findings review.
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
