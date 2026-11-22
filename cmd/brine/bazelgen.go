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
// Usage:
//   brine bazel-generate              # generate + validate
//   brine bazel-generate --validate   # validate existing BUILD files only
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
	"strings"
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

func cmdBazelGenerate(args []string) {
	validateOnly := false
	gazellePath := ""
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--validate":
			validateOnly = true
		case "--gazelle":
			i++
			if i < len(args) {
				gazellePath = args[i]
			}
		default:
			fatal("bazel-generate: unknown flag %s (use --validate, --gazelle <path>)", args[i])
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

	findings := validateBuildFiles(truth, modulePath)
	printFindings(findings, len(truth))
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
	content := fmt.Sprintf("module(name = %q)\n", name)
	if err := os.WriteFile("MODULE.bazel", []byte(content), 0o644); err != nil {
		fatal("bazel-generate: %v", err)
	}
	fmt.Printf("[brine] bazel-generate: created MODULE.bazel (minimal; add rules_go deps before building with bazel)\n")
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
func validateBuildFiles(truth []pkgTruth, modulePath string) []string {
	var findings []string
	wd, _ := os.Getwd()

	for _, pkg := range truth {
		rel, err := filepath.Rel(wd, pkg.Dir)
		if err != nil {
			continue
		}
		buildPath := filepath.Join(rel, "BUILD.bazel")
		if _, err := os.Stat(buildPath); err != nil {
			if len(pkg.GoFiles) > 0 {
				findings = append(findings, fmt.Sprintf("%s: NO BUILD FILE (package has %d source files)",
					rel, len(pkg.GoFiles)))
			}
			continue
		}
		rules, err := parseBuildFile(buildPath)
		if err != nil {
			findings = append(findings, fmt.Sprintf("%s: parse error: %v", rel, err))
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
				findings = append(findings, fmt.Sprintf("%s: MISSING SRC: %s compiles (go list) but no BUILD rule declares it", rel, f))
			}
		}
		// missing test srcs
		for _, f := range pkg.TestGoFiles {
			if !contains(testSrcs, f) {
				findings = append(findings, fmt.Sprintf("%s: MISSING TEST SRC: %s compiles (go list) but no go_test declares it", rel, f))
			}
		}
		// extra srcs: declared files that go list does not compile
		// (build-tag filtering differences — these work by accident or fail)
		compiled := append(append([]string{}, pkg.GoFiles...), pkg.TestGoFiles...)
		compiled = append(compiled, pkg.IgnoredGoFiles...)
		for _, f := range libSrcs {
			if strings.HasSuffix(f, ".go") && !contains(compiled, f) {
				findings = append(findings, fmt.Sprintf("%s: EXTRA SRC: %s declared in BUILD but not compiled (build tags?)", rel, f))
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
				findings = append(findings, fmt.Sprintf("%s: MISSING DEP: %s (imported, no dep label matches)", rel, label))
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

func printFindings(findings []string, total int) {
	if len(findings) == 0 {
		fmt.Printf("[brine] bazel-generate: validation clean — %d packages match go list ground truth\n", total)
		return
	}
	fmt.Printf("[brine] bazel-generate: %d finding(s):\n", len(findings))
	for _, f := range findings {
		fmt.Printf("  ✗ %s\n", f)
	}
	fmt.Printf("\nThese are the silent under-declarations that Bazel sandbox\n")
	fmt.Printf("failures are made of. Fix before building with bazel.\n")
}
