# bazel-generate: additive Bazel onboarding

`brine bazel-generate` generates Bazel BUILD files for a Go project by
orchestrating [gazelle](https://github.com/bazel-contrib/bazel-gazelle) —
the maintained generator — and then validating the result against
`go list` ground truth. brine does not reimplement generation; it adds
the verification layer nobody else has.

## Usage

```
brine bazel-generate              # generate (gazelle) + validate
brine bazel-generate --validate   # validate existing BUILD files only
```

Gazelle is found on PATH or run via a pinned `go run` (v0.53.0 — the
first tag that installs standalone cleanly; v0.54.0 has a v1/v2
interface migration break). A minimal `MODULE.bazel` is created if no
bazel marker file exists.

## What validation checks

Per package, `go list -json ./...` is the truth; the generated BUILD
files are parsed (minimal parser for gazelle's regular format) and
compared:

- **MISSING SRC** — a `.go` file go list compiles that no BUILD rule
  declares. This is the silent under-declaration that Bazel sandbox
  failures are made of.
- **MISSING TEST SRC** — same, for `_test.go` files.
- **MISSING DEP** — an internal import whose package dir has no matching
  dep label in any rule. Gazelle names targets after the Go package
  clause (e.g. `//generated/api/v1:api`), so any label rooted at the
  package dir matches.
- **EXTRA SRC** — a declared `.go` file that go list neither compiles
  nor reports as constraint-ignored (`IgnoredGoFiles`). Usually a
  build-tag filtering difference.

Exit code is non-zero when findings exist — usable as a CI gate.

## Results

StackRox (1,832 packages): gazelle generation + brine validation clean —
zero missing srcs, deps, or test srcs. Tamper tests (remove a src, remove
a dep from generated BUILD files) are caught by the validator.

Known gazelle-side issues observed during generation (not brine bugs):
`go:embed` fixture patterns warn when files are absent, and imports of
newer stdlib packages (`testing/synctest`) crash gazelle v0.53's module
resolution — fixed upstream after v0.53.

## Parity loop

`brine bazel-generate --parity //cmd/kwtest` proves the Bazel build
compiles the same machine code as `go build`:

1. `go build -trimpath -ldflags="-s -w -buildid=redacted"` — flags
   matched to rules_go (`-buildid=redacted` discovered via
   `bazel aquery`; rules_go passes it literally).
2. `bazel build` the target.
3. Extract `__text`/`.text` from both and compare word-by-word. Every
   differing word must be an address-forming immediate (ADRP page,
   ADD/SUB imm12, LDR/STR offset) — rules_go's longer path strings make
   gopclntab ~11.6KB larger, shifting later sections and every address
   immediate with them.

Measured on k8swatch (darwin/arm64): 23,929 differing words, 100%
explained by address immediates, zero unexplained — the instruction
streams are identical. The gate fails on real differences (different
code, flags, or sources) and conservatively fails on function
reordering (word-aligned comparison cannot prove equivalence across
permutations). Adversarially validated: removing a src that survives
dead-code elimination passes parity (correctly — the final code is
identical) while the go-list validator still flags the under-declaration;
`--fix` repairs it in sorted position to keep gazelle's canonical
ordering stable.

MODULE.bazel wiring is automated: `brine bazel-generate` writes the full
bzlmod module (rules_go with `repo_name = "io_bazel_rules_go"` for
gazelle's legacy labels, `go_sdk.download` matching the local toolchain,
gazelle `go_deps.from_file`) and runs `bazel mod tidy` to fill the
use_repo list. Fresh clone → one command → `bazel build` succeeds
(k8swatch: 121 actions, 43.5s cold).

## Fidelity experiment: cel-go

Method: take a project with hand/gazelle-maintained BUILD files, delete
them, regenerate with brine+gazelle, compare. cel-go (42 BUILD files,
project uses gazelle itself with hand-written directives) gives:

| class | dirs | note |
|---|---|---|
| structurally identical | 26 (63%) | same rules, srcs, deps |
| label convention only | 10 (24%) | `:go_default_library` (their older gazelle) vs `:pkg` (bzlmod era) — semantically the same targets |
| proto granularity | 2 (5%) | 3 proto_library rules vs 1 — both valid |
| real gaps | 3 (7%) | `tags = ["manual", "notap"]` exclusion markers, one empty dir |

~87% semantically equivalent. The real gaps are exactly the human
knowledge encoded in directives — which dirs not to build. That
knowledge is recoverable by observation: dirs never read during observed
builds are the excludes. This is the observation-vs-declaration thesis
in miniature: declaration encodes what humans know; observation
discovers what builds actually do.

Landscape note: many modern Go projects (google/go-cmp, google/uuid)
carry no BUILD files at all — either no Bazel or generation at build
time — so regeneration-from-source is the only path to a Bazel setup
for them.

## Positioning

Composition, not competition: gazelle owns static generation for
Go/proto; brine owns observation (C/C++ depfiles, genrule candidates,
hermeticity flags via the divergence detector) and verification. The
next increment is the build-parity loop: build via the original system
(receipt = cached ground truth) vs `bazel build`, compare outputs after
path normalization.
