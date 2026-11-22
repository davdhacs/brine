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

## Positioning

Composition, not competition: gazelle owns static generation for
Go/proto; brine owns observation (C/C++ depfiles, genrule candidates,
hermeticity flags via the divergence detector) and verification. The
next increment is the build-parity loop: build via the original system
(receipt = cached ground truth) vs `bazel build`, compare outputs after
path normalization.
