// brine bazel-generate: generate Bazel BUILD files for a Go project by
// orchestrating gazelle (the maintained generator — we do not reimplement
// it), then validating the result against `go list` ground truth.
//
// The pipeline:
//   1. gazelle (PATH binary, or `go run ...@v0.53.0` fallback) generates
//      BUILD.bazel files for Go packages.
//   2. `go list -json ./...` provides per-package truth: exactly which
//      files compile into each package and which internal packages each
//      one imports.
//   3. The generated BUILD files are parsed and compared against that
//      truth. Mismatches are the silent under-declaration that sandbox
//      failures are made of — reported here, before you build.
//
// The feedback loop:
//   --fix       auto-repair missing srcs / missing deps findings by
//               editing the BUILD files in place, then re-validate.
//   --excludes  dirs never read by any observed build (receipts in
//               .brine/manifests) get tags = ["manual"] — recovering the
//               exclusion knowledge that hand-maintained setups encode
//               in gazelle directives.
//
// Usage:
//   brine bazel-generate                       # generate + validate
//   brine bazel-generate --validate            # validate only
//   brine bazel-generate --fix                 # validate + repair + re-validate
//   brine bazel-generate --excludes            # tag never-built dirs manual
//
// Gazelle version pinning: the first tag that installs standalone cleanly
// (v0.54.0 has a v1/v2 interface migration break in cmd/gazelle).

package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/davdhacs/brine/store"
)

const gazellePin = "v0.53.0"

// pkgTruth is the per-package ground truth from go list.
type pkgTruth struct {
	Dir            string
	ImportPath     string
	Name           string // "main" or library
	GoFiles        []string
	TestGoFiles    []string
	IgnoredGoFiles []string
	Imports        []string
}

// buildRule is one parsed rule from a BUILD file.
type buildRule struct {
	kind       string // go_library, go_binary, go_test, ...
	name       string
	srcs       []string
	deps       []string
	embed      []string
	importpath string
}

// finding is a structured validation result; --fix consumes these.
type finding struct {
	dir    string // package dir, relative
	kind   string // missing-src, missing-test-src, missing-dep, extra-src, no-build, parse
	detail string // filename or label
}

func cmdBazelGenerate(args []string) {
	validateOnly, fix, excludes := false, false, false
	gazellePath := ""
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--validate":
			validateOnly = true
		case "--fix":
			fix = true
		case "--excludes":
			excludes = true
		case "--parity":
			i++
			if i >= len(args) {
				fatal("bazel-generate: --parity needs a target (e.g. --parity //cmd/kwtest)")
			}
			cmdBazelParity(args[i])
			return
		case "--gazelle":
			i++
			if i < len(args) {
				gazellePath = args[i]
			}
		default:
			fatal("bazel-generate: unknown flag %s (use --validate, --fix, --excludes, --gazelle <path>)", args[i])
		}
	}

	// Ground truth first: go list must work regardless of bazel state.
	truth, modulePath := goListTruth()
	fmt.Printf("[brine] bazel-generate: %d packages (module %s)\n", len(truth), modulePath)

	if !validateOnly {
		if modulePath == "" {
			fatal("bazel-generate: no go module found (run in a Go module)")
		}
		ensureModuleBazel()
		runGazelle(gazellePath, modulePath)
	}

	if excludes {
		applyExcludesFromReceipts(truth)
	}

	findings := validateBuildFiles(truth, modulePath)
	printFindings(findings, len(truth))

	if fix && len(findings) > 0 {
		repaired := fixFindings(findings)
		fmt.Printf("[brine] bazel-generate: --fix repaired %d finding(s); re-validating\n", repaired)
		// ground truth is unchanged; re-validate against the same truth
		findings = validateBuildFiles(truth, modulePath)
		printFindings(findings, len(truth))
	}

	if len(findings) > 0 {
		os.Exit(1)
	}
}

// goListTruth runs `go list -json ./...` and returns per-package truth.
func goListTruth() ([]pkgTruth, string) {
	cmd := exec.Command("go", "list", "-json", "./...")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		fatal("bazel-generate: %v", err)
	}
	if err := cmd.Start(); err != nil {
		fatal("bazel-generate: go list failed to start: %v", err)
	}
	dec := json.NewDecoder(stdout)
	var pkgs []pkgTruth
	modulePath := ""
	for dec.More() {
		var p struct {
			Dir            string
			ImportPath     string
			Name           string
			GoFiles        []string
			TestGoFiles    []string
			IgnoredGoFiles []string
			Imports        []string
			Module         *struct {
				Path string
			}
			Error *struct {
				Err string
			}
		}
		if err := dec.Decode(&p); err != nil {
			break
		}
		if p.Error != nil {
			fmt.Fprintf(os.Stderr, "[brine] bazel-generate: skipping %s: %s\n", p.ImportPath, p.Error.Err)
			continue
		}
		if p.Module != nil && modulePath == "" {
			modulePath = p.Module.Path
		}
		if len(p.GoFiles) == 0 && len(p.TestGoFiles) == 0 {
			continue
		}
		pkgs = append(pkgs, pkgTruth{
			Dir: p.Dir, ImportPath: p.ImportPath, Name: p.Name,
			GoFiles: p.GoFiles, TestGoFiles: p.TestGoFiles,
			IgnoredGoFiles: p.IgnoredGoFiles, Imports: p.Imports,
		})
	}
	cmd.Wait()
	return pkgs, modulePath
}

// ensureModuleBazel creates a minimal MODULE.bazel if no bazel marker file
// exists — gazelle refuses to run without one.
func ensureModuleBazel() {
	if _, err := os.Stat("MODULE.bazel"); err == nil {
		return
	}
	if _, err := os.Stat("WORKSPACE"); err == nil {
		return
	}
	if _, err := os.Stat("WORKSPACE.bazel"); err == nil {
		return
	}
	// derive module name from go.mod
	name := "main"
	if data, err := os.ReadFile("go.mod"); err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			if strings.HasPrefix(line, "module ") {
				path := strings.TrimSpace(strings.TrimPrefix(line, "module "))
				parts := strings.Split(path, "/")
				name = parts[len(parts)-1]
				break
			}
		}
	}

	// full bzlmod wiring: rules_go (legacy repo name for gazelle's labels),
	// a matching Go SDK, and gazelle's go_deps resolved from go.mod.
	goVersion := strings.TrimPrefix(strings.TrimSpace(runCapture("go", "env", "GOVERSION")), "go")
	content := fmt.Sprintf(`module(name = %q, version = "v0.0.0")

bazel_dep(name = "rules_go", version = %q, repo_name = "io_bazel_rules_go")
bazel_dep(name = "gazelle", version = %q)

go_sdk = use_extension("@io_bazel_rules_go//go:extensions.bzl", "go_sdk")
go_sdk.download(version = %q)

go_deps = use_extension("@gazelle//:extensions.bzl", "go_deps")
go_deps.from_file(go_mod = "//:go.mod")
`, name, rulesGoPin, strings.TrimPrefix(gazellePin, "v"), goVersion)
	if err := os.WriteFile("MODULE.bazel", []byte(content), 0o644); err != nil {
		fatal("bazel-generate: %v", err)
	}
	fmt.Printf("[brine] bazel-generate: created MODULE.bazel (rules_go %s, gazelle %s, go %s)\n", rulesGoPin, gazellePin, goVersion)

	// bazel mod tidy fills the use_repo list from go_deps — the one piece
	// that genuinely needs bazel's own module resolution.
	if _, err := exec.LookPath("bazel"); err == nil {
		fmt.Printf("[brine] bazel-generate: running bazel mod tidy (fills use_repo from go.mod)\n")
		cmd := exec.Command("bazel", "mod", "tidy")
		cmd.Stdout = os.Stderr
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			fmt.Fprintf(os.Stderr, "[brine] bazel-generate: bazel mod tidy failed (%v); run it manually before building\n", err)
		}
	} else if _, err := exec.LookPath("bazelisk"); err == nil {
		fmt.Printf("[brine] bazel-generate: running bazelisk mod tidy (fills use_repo from go.mod)\n")
		cmd := exec.Command("bazelisk", "mod", "tidy")
		cmd.Stdout = os.Stderr
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			fmt.Fprintf(os.Stderr, "[brine] bazel-generate: bazelisk mod tidy failed (%v); run it manually before building\n", err)
		}
	} else {
		fmt.Printf("[brine] bazel-generate: no bazel/bazelisk on PATH; run 'bazel mod tidy' before building with bazel\n")
	}
}

// rulesGoPin: rules_go version paired with gazellePin. 0.59.0 is what the
// module graph resolves to under bazel 9; the toolchain tag was renamed
// (go_sdk.toolchain → go_sdk.download) at this version.
const rulesGoPin = "0.59.0"

// runCapture runs a command and returns its trimmed stdout.
func runCapture(name string, args ...string) string {
	out, err := exec.Command(name, args...).Output()
	if err != nil {
		return ""
	}
	return string(out)
}

// runGazelle invokes gazelle, preferring a PATH binary, falling back to a
// pinned `go run` invocation.
func runGazelle(explicit, modulePath string) {
	var cmd *exec.Cmd
	if explicit != "" {
		cmd = exec.Command(explicit)
	} else if path, err := exec.LookPath("gazelle"); err == nil {
		cmd = exec.Command(path)
	} else {
		cmd = exec.Command("go", "run", "github.com/bazelbuild/bazel-gazelle/cmd/gazelle@"+gazellePin)
		fmt.Printf("[brine] bazel-generate: no gazelle on PATH; using go run @%s (first run downloads)\n", gazellePin)
	}
	cmd.Args = append(cmd.Args, "-go_prefix", modulePath)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		fatal("bazel-generate: gazelle failed: %v", err)
	}
}

// applyExcludesFromReceipts unions the read-sets of all receipts in
// .brine/manifests and tags package dirs that were never read as
// "manual" — bazel then skips them in wildcard builds. This recovers,
// from observation, the exclusion knowledge that hand-maintained setups
// encode in gazelle directives (# gazelle:exclude, tags = ["manual"]).
func applyExcludesFromReceipts(truth []pkgTruth) {
	wd, _ := os.Getwd()
	manifestDir := filepath.Join(wd, ".brine", "manifests")
	entries, err := os.ReadDir(manifestDir)
	if err != nil || len(entries) == 0 {
		fmt.Printf("[brine] bazel-generate: --excludes: no receipts in .brine/manifests (wrap builds with brine first); skipping\n")
		return
	}

	readFiles := map[string]bool{}
	receiptCount := 0
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(manifestDir, e.Name()))
		if err != nil {
			continue
		}
		var m store.Manifest
		if err := json.Unmarshal(data, &m); err != nil {
			continue
		}
		receiptCount++
		for _, f := range m.ReadSet {
			readFiles[f] = true
		}
	}

	tagged := 0

	// truth dirs: packages go list actually compiles
	truthDirs := map[string]bool{}
	for _, pkg := range truth {
		rel, err := filepath.Rel(wd, pkg.Dir)
		if err == nil {
			truthDirs[rel] = true
		}
	}

	// walk every dir with a BUILD file: excluded = not in go-list truth
	// (build-tag-gated packages) or no file read by any observed build
	var buildDirs []string
	filepath.Walk(wd, func(p string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil //nolint
		}
		if info.Name() == "BUILD.bazel" {
			rel, _ := filepath.Rel(wd, filepath.Dir(p))
			if rel != "." && !strings.HasPrefix(rel, ".") {
				buildDirs = append(buildDirs, rel)
			} else if rel == "." {
				buildDirs = append(buildDirs, rel)
			}
		}
		return nil
	})

	for _, rel := range buildDirs {
		buildPath := filepath.Join(rel, "BUILD.bazel")
		if !truthDirs[rel] {
			// not even in go-list truth: build-tag-gated or broken package
			if tagAllRulesManual(buildPath) {
				tagged++
				fmt.Printf("[brine] bazel-generate: --excludes: %s not in go-list truth (build tags?) → tags = [\"manual\"]\n", rel)
			}
			continue
		}
		// in truth: excluded only if never read by any observed build
		var pkgFiles []string
		for _, pkg := range truth {
			if r, err := filepath.Rel(wd, pkg.Dir); err == nil && r == rel {
				pkgFiles = append(append(pkgFiles, pkg.GoFiles...), pkg.TestGoFiles...)
			}
		}
		read := false
		for _, f := range pkgFiles {
			if readFiles[filepath.ToSlash(filepath.Join(rel, f))] {
				read = true
				break
			}
		}
		if !read && tagAllRulesManual(buildPath) {
			tagged++
			fmt.Printf("[brine] bazel-generate: --excludes: %s never read by %d observed build(s) → tags = [\"manual\"]\n", rel, receiptCount)
		} else if read && untagManual(buildPath) {
			// loop convergence: a later-observed command reads this dir —
			// the manual tag this tool previously added is now wrong
			fmt.Printf("[brine] bazel-generate: --excludes: %s read by an observed build → manual tag removed\n", rel)
		}
	}
	if tagged == 0 {
		fmt.Printf("[brine] bazel-generate: --excludes: all packages were read by observed builds; nothing to exclude\n")
	}
}

// tagAllRulesManual adds tags = ["manual"] to every rule in a BUILD file.
// Returns true if the file was modified.
func tagAllRulesManual(path string) bool {
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	if strings.Contains(string(data), `"manual"`) {
		return false // already tagged
	}
	var out []string
	inRule := false
	changed := false
	for _, line := range strings.Split(string(data), "\n") {
		t := strings.TrimSpace(line)
		out = append(out, line)
		if strings.HasSuffix(t, "(") && isRuleKind(strings.TrimSuffix(t, "(")) {
			inRule = true
			// insert tags right after the rule opening
			out = append(out, `    tags = ["manual"],`)
			changed = true
		} else if inRule && t == ")" {
			inRule = false
		}
	}
	if !changed {
		return false
	}
	if err := os.WriteFile(path, []byte(strings.Join(out, "\n")), 0o644); err != nil {
		return false
	}
	return true
}

// untagManual removes "manual" from tags lists in a BUILD file, dropping
// the attribute entirely when it becomes empty. Only "manual" is touched;
// other tags are preserved. Handles both single-line and multi-line tags
// lists. Returns true if the file was modified.
func untagManual(path string) bool {
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	lines := strings.Split(string(data), "\n")
	var out []string
	changed := false
	for i := 0; i < len(lines); i++ {
		t := strings.TrimSpace(lines[i])
		var items []string
		consumed := 0
		if m := tagsLineRe.FindStringSubmatch(t); m != nil {
			items = splitListItems(m[1])
			consumed = 1
		} else if strings.HasPrefix(t, "tags = [") {
			// multi-line list: collect items until the closing bracket
			consumed = 1
			for i+consumed < len(lines) {
				nt := strings.TrimSpace(lines[i+consumed])
				items = append(items, splitListItems(nt)...)
				consumed++
				if strings.HasSuffix(nt, "]") || nt == "]" {
					break
				}
			}
		} else {
			out = append(out, lines[i])
			continue
		}
		var kept []string
		for _, it := range items {
			if it != "manual" {
				kept = append(kept, it)
			}
		}
		if len(kept) == len(items) {
			// no manual tag in this list: emit the lines unchanged
			out = append(out, lines[i:i+consumed]...)
			i += consumed - 1
			continue
		}
		changed = true
		if len(kept) > 0 {
			var quoted []string
			for _, k := range kept {
				quoted = append(quoted, fmt.Sprintf("%q", k))
			}
			out = append(out, fmt.Sprintf("    tags = [%s],", strings.Join(quoted, ", ")))
		}
		i += consumed - 1
	}
	if !changed {
		return false
	}
	if err := os.WriteFile(path, []byte(strings.Join(out, "\n")), 0o644); err != nil {
		return false
	}
	return true
}

var tagsLineRe = regexp.MustCompile(`^tags = \[(.*)\],?$`)

// fixFindings repairs missing-src / missing-test-src / missing-dep findings
// by editing BUILD files in place. Returns the number of repairs applied.
func fixFindings(findings []finding) int {
	repaired := 0
	for _, f := range findings {
		buildPath := filepath.Join(f.dir, "BUILD.bazel")
		switch f.kind {
		case "missing-src":
			if addToListAttr(buildPath, "go_library", "srcs", f.detail) {
				repaired++
			}
		case "missing-test-src":
			if addToListAttr(buildPath, "go_test", "srcs", f.detail) {
				repaired++
			}
		case "missing-dep":
			if addToListAttr(buildPath, "go_library", "deps", f.detail) {
				repaired++
			}
		}
	}
	return repaired
}

// addToListAttr inserts value into the given attribute list of the first
// rule of the given kind in the BUILD file. Handles both multi-line lists
// (insert an item line) and inline lists (extend within the line).
func addToListAttr(path, ruleKind, attr, value string) bool {
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}

	// duplicate check via the parser (covers multi-line lists)
	if rules, err := parseBuildFile(path); err == nil {
		for _, r := range rules {
			if r.kind == ruleKind {
				if contains(*attrList(&r, attr), value) {
					return false
				}
				break // first rule of the kind only
			}
		}
	}

	lines := strings.Split(string(data), "\n")

	// locate the first rule of ruleKind, then its attr list
	inRule := false
	curKind := ""
	attrLine := -1  // line index of the attr opening within the rule
	ruleClose := -1 // line index of the rule's closing paren
	for i, line := range lines {
		t := strings.TrimSpace(line)
		if strings.HasSuffix(t, "(") && isRuleKind(strings.TrimSuffix(t, "(")) {
			curKind = strings.TrimSuffix(t, "(")
			inRule = true
			attrLine = -1
			ruleClose = -1
			continue
		}
		if inRule && t == ")" {
			if curKind == ruleKind {
				ruleClose = i
				break // found our rule; attr (if any) recorded below
			}
			inRule = false
			continue
		}
		if inRule && curKind == ruleKind {
			if strings.HasPrefix(t, attr+" = ") {
				attrLine = i
				rest := strings.TrimPrefix(t, attr+" = ")
				rest = strings.TrimSuffix(strings.TrimSpace(rest), ",")
				// inline complete list: srcs = ["a", "b"]
				if strings.HasPrefix(rest, "[") && strings.HasSuffix(rest, "]") {
					inner := strings.TrimSuffix(strings.TrimPrefix(rest, "["), "]")
					items := splitListItems(inner)
					if contains(items, value) {
						return false
					}
					items = append(items, value)
					sort.Strings(items)
					var quoted []string
					for _, it := range items {
						quoted = append(quoted, fmt.Sprintf("%q", it))
					}
					pad := ""
					if j := strings.Index(line, attr+" = "); j > 0 {
						pad = line[:j]
					}
					lines[i] = fmt.Sprintf("%s%s = [%s],", pad, attr, strings.Join(quoted, ", "))
					writeLines(path, lines)
					return true
				}
				// multi-line list opening: insert item after this line
				break
			}
		}
	}
	if attrLine < 0 {
		// attribute absent from the rule: insert it before the closing paren
		if ruleClose < 0 {
			return false
		}
		attrDecl := fmt.Sprintf("    %s = [%q],", attr, value)
		newLines := append([]string{}, lines[:ruleClose]...)
		newLines = append(newLines, attrDecl)
		newLines = append(newLines, lines[ruleClose:]...)
		writeLines(path, newLines)
		return true
	}
	// multi-line: insert at the sorted position — gazelle emits srcs/deps in
	// sorted order, and preserving that order keeps compile order (and thus
	// function layout in the linked binary) stable across gazelle re-runs
	// and parity comparison.
	item := fmt.Sprintf("        %q,", value) // gazelle indents list items 8 spaces
	insertAt := attrLine + 1
	for i := attrLine + 1; i < len(lines); i++ {
		t := strings.TrimSpace(lines[i])
		if t == "]" || t == "]," {
			break
		}
		existing := unquote(t)
		if existing != "" && existing > value {
			insertAt = i
			break
		}
		insertAt = i + 1
	}
	newLines := append([]string{}, lines[:insertAt]...)
	newLines = append(newLines, item)
	newLines = append(newLines, lines[insertAt:]...)
	writeLines(path, newLines)
	return true
}

func writeLines(path string, lines []string) {
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")), 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "[brine] bazel-generate: --fix: %v\n", err)
	}
}

// parseBuildFile parses a gazelle-generated BUILD file. This is a minimal
// parser for gazelle's regular output format (rule blocks with name/srcs/
// deps/embed/importpath attributes) — not a general Starlark parser.
// Buildifier-reformatted files with unusual spacing may need attention.
func parseBuildFile(path string) ([]buildRule, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var rules []buildRule
	var cur *buildRule
	var curAttr string
	for _, line := range strings.Split(string(data), "\n") {
		t := strings.TrimSpace(line)
		if cur == nil {
			// start of a rule block: "go_library(" etc.
			if strings.HasSuffix(t, "(") && !strings.HasPrefix(t, "load(") {
				kind := strings.TrimSuffix(t, "(")
				if isRuleKind(kind) {
					rules = append(rules, buildRule{kind: kind})
					cur = &rules[len(rules)-1]
				}
			}
			continue
		}
		// inside a rule block
		if t == ")" {
			cur = nil
			curAttr = ""
			continue
		}
		switch {
		case strings.HasPrefix(t, "name = "):
			cur.name = unquote(strings.TrimPrefix(t, "name = "))
		case strings.HasPrefix(t, "importpath = "):
			cur.importpath = unquote(strings.TrimPrefix(t, "importpath = "))
		case strings.HasPrefix(t, "srcs = "):
			curAttr = "srcs"
			maybeCollectList(cur, &curAttr, strings.TrimPrefix(t, "srcs = "))
		case strings.HasPrefix(t, "deps = "):
			curAttr = "deps"
			maybeCollectList(cur, &curAttr, strings.TrimPrefix(t, "deps = "))
		case strings.HasPrefix(t, "embed = "):
			curAttr = "embed"
			maybeCollectList(cur, &curAttr, strings.TrimPrefix(t, "embed = "))
		case strings.HasPrefix(t, "data = "):
			curAttr = "data" // collected but unused
		case curAttr != "" && (strings.HasPrefix(t, "\"") || strings.HasPrefix(t, "glob(")):
			maybeCollectList(cur, &curAttr, t)
		}
	}
	return rules, nil
}

func isRuleKind(kind string) bool {
	switch kind {
	case "go_library", "go_binary", "go_test", "cc_library", "cc_binary",
		"cc_test", "proto_library", "genrule":
		return true
	}
	return false
}

// maybeCollectList handles both inline lists (`srcs = ["a"]`) and
// multi-line lists (`srcs = [` then items then `]`).
func maybeCollectList(r *buildRule, attr *string, rest string) {
	target := attrList(r, *attr)
	if strings.HasPrefix(rest, "glob(") {
		return // globs are validated by the missing-src check naturally
	}
	rest = strings.TrimSpace(rest)
	rest = strings.TrimSuffix(rest, ",") // trailing comma after "]"
	if strings.HasPrefix(rest, "[") {
		rest = strings.TrimPrefix(rest, "[")
		if strings.HasSuffix(rest, "]") {
			rest = strings.TrimSuffix(rest, "]")
			*attr = "" // complete inline list
		}
	}
	for _, item := range splitListItems(rest) {
		*target = append(*target, item)
	}
}

func attrList(r *buildRule, attr string) *[]string {
	switch attr {
	case "srcs":
		return &r.srcs
	case "deps":
		return &r.deps
	case "embed":
		return &r.embed
	}
	return &r.deps
}

// splitListItems splits "a", "b" into unquoted items.
func splitListItems(s string) []string {
	var items []string
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if q := unquote(part); q != "" {
			items = append(items, q)
		}
	}
	return items
}

func unquote(s string) string {
	s = strings.TrimSpace(s)
	s = strings.TrimSuffix(s, ",")
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		return s[1 : len(s)-1]
	}
	return ""
}

// validateBuildFiles compares generated BUILD files against go list truth.
func validateBuildFiles(truth []pkgTruth, modulePath string) []finding {
	var findings []finding
	wd, _ := os.Getwd()

	for _, pkg := range truth {
		rel, err := filepath.Rel(wd, pkg.Dir)
		if err != nil {
			continue
		}
		buildPath := filepath.Join(rel, "BUILD.bazel")
		if _, err := os.Stat(buildPath); err != nil {
			if len(pkg.GoFiles) > 0 {
				findings = append(findings, finding{rel, "no-build",
					fmt.Sprintf("package has %d source files", len(pkg.GoFiles))})
			}
			continue
		}
		rules, err := parseBuildFile(buildPath)
		if err != nil {
			findings = append(findings, finding{rel, "parse", err.Error()})
			continue
		}

		// Union of srcs across non-test rules (go_library/go_binary) and
		// test rules separately.
		var libSrcs, testSrcs []string
		for _, r := range rules {
			if r.kind == "go_test" || r.kind == "cc_test" {
				testSrcs = append(testSrcs, r.srcs...)
			} else {
				libSrcs = append(libSrcs, r.srcs...)
			}
		}

		// missing srcs: .go files go list compiles that no rule declares
		for _, f := range pkg.GoFiles {
			if !contains(libSrcs, f) {
				findings = append(findings, finding{rel, "missing-src", f})
			}
		}
		// missing test srcs
		for _, f := range pkg.TestGoFiles {
			if !contains(testSrcs, f) {
				findings = append(findings, finding{rel, "missing-test-src", f})
			}
		}
		// extra srcs: declared files that go list does not compile
		// (build-tag filtering differences — these work by accident or fail)
		compiled := append(append([]string{}, pkg.GoFiles...), pkg.TestGoFiles...)
		compiled = append(compiled, pkg.IgnoredGoFiles...)
		for _, f := range libSrcs {
			if strings.HasSuffix(f, ".go") && !contains(compiled, f) {
				findings = append(findings, finding{rel, "extra-src", f})
			}
		}

		// missing internal deps: imports within the module whose label
		// does not appear in any rule's deps
		var allDeps []string
		for _, r := range rules {
			allDeps = append(allDeps, r.deps...)
		}
		for _, imp := range pkg.Imports {
			if !strings.HasPrefix(imp, modulePath+"/") && imp != modulePath {
				continue // external dep — gazelle handles via go.mod mapping
			}
			label := importToLabel(imp, modulePath)
			if !hasDepLabel(allDeps, label) {
				findings = append(findings, finding{rel, "missing-dep", label})
			}
		}
	}
	return findings
}

// importToLabel maps an import path to the gazelle label convention:
// root package → "//:modname", subpackage → "//rel/path" (default target
// name is the last path element).
func importToLabel(imp, modulePath string) string {
	if imp == modulePath {
		// root package: label is //:name where name is the library target
		parts := strings.Split(modulePath, "/")
		return "//:" + parts[len(parts)-1]
	}
	rel := strings.TrimPrefix(imp, modulePath+"/")
	return "//" + rel
}

// hasDepLabel reports whether any dep label references the package at the
// expected label's directory. Gazelle names library targets after the Go
// package clause (e.g. //generated/api/v1:api), not the directory basename,
// so any "//dir" or "//dir:<name>" matches — the target name cannot be
// derived statically and any label rooted at the package dir is a hit.
// Labels at deeper paths ("//dir/sub") are different packages and do not
// match.
func hasDepLabel(deps []string, label string) bool {
	for _, d := range deps {
		if d == label || strings.HasPrefix(d, label+":") {
			return true
		}
	}
	return false
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func printFindings(findings []finding, total int) {
	if len(findings) == 0 {
		fmt.Printf("[brine] bazel-generate: validation clean — %d packages match go list ground truth\n", total)
		return
	}
	fmt.Printf("[brine] bazel-generate: %d finding(s):\n", len(findings))
	for _, f := range findings {
		switch f.kind {
		case "no-build":
			fmt.Printf("  ✗ %s: NO BUILD FILE (%s)\n", f.dir, f.detail)
		case "parse":
			fmt.Printf("  ✗ %s: parse error: %s\n", f.dir, f.detail)
		case "missing-src":
			fmt.Printf("  ✗ %s: MISSING SRC: %s compiles (go list) but no BUILD rule declares it\n", f.dir, f.detail)
		case "missing-test-src":
			fmt.Printf("  ✗ %s: MISSING TEST SRC: %s compiles (go list) but no go_test declares it\n", f.dir, f.detail)
		case "missing-dep":
			fmt.Printf("  ✗ %s: MISSING DEP: %s (imported, no dep label matches)\n", f.dir, f.detail)
		case "extra-src":
			fmt.Printf("  ✗ %s: EXTRA SRC: %s declared in BUILD but not compiled (build tags?)\n", f.dir, f.detail)
		}
	}
	fmt.Printf("\nThese are the silent under-declarations that Bazel sandbox\n")
	fmt.Printf("failures are made of. Fix before building with bazel, or re-run with --fix.\n")
}
