// C/C++ BUILD generation for brine bazel-generate — gazelle's blind spot.
// Static C/C++ analysis famously cannot recover include paths, defines,
// and generated headers; the observed build can. This generator runs the
// project's build in dry-run mode, executes the compile commands with
// depfile capture (-MD), and derives BUILD files from the exact
// dependency truth those depfiles contain.
//
// Pipeline:
//  1. Discovery: `make -Bn` (or $BRINE_CC_DRY) — the full command list
//     without executing it. Compile commands (cc -c), archives (ar),
//     and links (cc -o) are classified by shape.
//  2. Depfile capture: each compile command re-runs with -MD -MF
//     redirected to a temp file (and -o to a throwaway object), giving
//     the exact header closure per object — the C/C++ ground truth.
//  3. Generation: objects grouped by source directory become
//     cc_library rules (srcs, hdrs from same-dir depfile headers, deps
//     from cross-dir header reads, defines/includes from flags).
//     Link commands become cc_binary rules wired to the libraries that
//     provide their objects. Archives (ar) are recorded as the library
//     grouping hint when present.
//  4. Validation: same finding discipline as the Go path — every
//     depfile header must be declared (hdrs or reachable via deps),
//     every source in srcs, defines preserved.
package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/davdhacs/brine/depfiles"
)

// ccCompile is one observed compilation.
type ccCompile struct {
	Cmd     []string // full argv
	Src     string   // the .c/.cc file (project-relative)
	Obj     string   // the -o target (project-relative)
	Headers []string // project-relative headers from the depfile
	Defines []string // -D flags
	IncDirs []string // -I flags (project-relative when in-project)
}

// ccLink is one observed link or archive step.
type ccLink struct {
	Cmd     []string
	Out     string   // binary or archive
	Objects []string // .o files consumed
	IsAr    bool
}

// ccDiscovery parses a dry-run build into compile and link commands.
func ccDiscovery(dryCmd []string, wd string) ([]ccCompile, []ccLink, error) {
	cmd := exec.Command(dryCmd[0], dryCmd[1:]...)
	cmd.Dir = wd
	out, err := cmd.Output()
	if err != nil {
		return nil, nil, fmt.Errorf("dry run failed: %v", err)
	}
	var compiles []ccCompile
	var links []ccLink
	// join backslash continuations first: real Makefiles wrap link lines
	// across several lines (sqlite wraps its link commands with
	// trailing backslashes)
	text := strings.ReplaceAll(string(out), "\\\n", " ")
	for _, line := range strings.Split(text, "\n") {
		fields := splitCommand(line)
		if len(fields) < 2 {
			continue
		}
		if c := classifyCompile(fields); c != nil {
			relativizeCC(c, wd)
			compiles = append(compiles, *c)
		} else if l := classifyLink(fields); l != nil {
			relativizeCCLink(l, wd)
			links = append(links, *l)
		}
	}
	return compiles, links, nil
}

// relativizeCC converts absolute source/object paths to project-relative
// (autosetup/cmake builds emit absolute paths — sqlite: "cc -c
// /home/.../sqlite3.c -o sqlite3.o"). Paths outside the project are left
// absolute and will not group into libraries.
func relativizeCC(c *ccCompile, wd string) {
	c.Src = relTo(wd, c.Src)
	c.Obj = relTo(wd, c.Obj)
}

func relativizeCCLink(l *ccLink, wd string) {
	l.Out = relTo(wd, l.Out)
	for i, o := range l.Objects {
		l.Objects[i] = relTo(wd, o)
	}
}

func relTo(wd, p string) string {
	if !filepath.IsAbs(p) {
		return p
	}
	rel, err := filepath.Rel(wd, p)
	if err != nil || strings.HasPrefix(rel, "..") {
		return p // outside the project: leave as-is
	}
	return filepath.ToSlash(rel)
}

// splitCommand splits a command line, honoring double quotes.
func splitCommand(line string) []string {
	var fields []string
	var cur strings.Builder
	inQuote := false
	for _, r := range line {
		switch {
		case r == '"':
			inQuote = !inQuote
		case (r == ' ' || r == '\t') && !inQuote:
			if cur.Len() > 0 {
				fields = append(fields, cur.String())
				cur.Reset()
			}
		default:
			cur.WriteRune(r)
		}
	}
	if cur.Len() > 0 {
		fields = append(fields, cur.String())
	}
	return fields
}

var ccCompilerRe = regexp.MustCompile(`^(cc|gcc|clang|gcc-[0-9]+|clang-[0-9]+|g\+\+|clang\+\+|c\+\+)$`)

// classifyCompile recognizes a compilation command: compiler ... -c src -o obj
func classifyCompile(f []string) *ccCompile {
	if !ccCompilerRe.MatchString(f[0]) {
		return nil
	}
	var src, obj string
	var defines, incs []string
	for i := 1; i < len(f); i++ {
		switch {
		case f[i] == "-c":
			// compilation marker
		case f[i] == "-o" && i+1 < len(f):
			obj = f[i+1]
			i++
		case strings.HasPrefix(f[i], "-D"):
			defines = append(defines, strings.TrimPrefix(f[i], "-D"))
		case strings.HasPrefix(f[i], "-I"):
			incs = append(incs, strings.TrimPrefix(f[i], "-I"))
		case strings.HasSuffix(f[i], ".c"), strings.HasSuffix(f[i], ".cc"),
			strings.HasSuffix(f[i], ".cpp"), strings.HasSuffix(f[i], ".cxx"):
			src = f[i]
		}
	}
	if src == "" || obj == "" {
		return nil
	}
	return &ccCompile{Cmd: f, Src: src, Obj: obj, Defines: defines, IncDirs: incs}
}

// classifyLink recognizes a link (compiler -o bin objs...) or archive (ar).
func classifyLink(f []string) *ccLink {
	if f[0] == "ar" || strings.HasSuffix(f[0], "/ar") {
		// ar rcs libfoo.a objs...
		var out string
		var objs []string
		for i := 1; i < len(f); i++ {
			if strings.HasSuffix(f[i], ".a") && out == "" {
				out = f[i]
			} else if strings.HasSuffix(f[i], ".o") {
				objs = append(objs, f[i])
			}
		}
		if out == "" {
			return nil
		}
		return &ccLink{Cmd: f, Out: out, Objects: objs, IsAr: true}
	}
	if !ccCompilerRe.MatchString(f[0]) {
		return nil
	}
	var out string
	var objs []string
	for i := 1; i < len(f); i++ {
		if f[i] == "-o" && i+1 < len(f) {
			if i+2 <= len(f) {
				out = f[i+1]
				i++
			}
			continue
		}
		if strings.HasSuffix(f[i], ".o") {
			objs = append(objs, f[i])
		}
	}
	if out == "" || len(objs) == 0 || strings.HasSuffix(out, ".o") {
		return nil // a compile, not a link
	}
	return &ccLink{Cmd: f, Out: out, Objects: objs}
}

// captureDepfiles re-runs each compile with -MD -MF to a temp depfile
// (and -o to a throwaway), then parses them. The observed build's own
// depfiles may not exist (no -MD in the project), so we generate them.
func captureDepfiles(compiles []ccCompile, wd string) error {
	tmpDir, err := os.MkdirTemp("", "brine-ccdeps")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmpDir)
	for i := range compiles {
		c := &compiles[i]
		depFile := filepath.Join(tmpDir, fmt.Sprintf("dep%d.d", i))
		throwObj := filepath.Join(tmpDir, fmt.Sprintf("throw%d.o", i))
		args := stripCCFlags(append([]string{}, c.Cmd...), "-o", "-MD", "-MMD", "-MF", "-MT", "-MQ", "-no-canonical-prefixes")
		args = append(args, "-MD", "-MF", depFile, "-o", throwObj)
		cmd := exec.Command(args[0], args[1:]...)
		cmd.Dir = wd
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("depfile capture for %s: %v\nargv: %s\n%s", c.Src, err, strings.Join(args, " "), out)
		}
		c.Headers = depfiles.Parse(depFile, wd)
		// drop the source itself from headers (Parse excludes the target
		// only when it matches the depfile target; with -MF redirect the
		// target differs, so filter here)
		filtered := c.Headers[:0]
		for _, h := range c.Headers {
			if h != c.Src {
				filtered = append(filtered, h)
			}
		}
		c.Headers = filtered
	}
	return nil
}

// stripCCFlags removes flag (and its argument, for two-token flags)
// occurrences from a compiler argv — depfile capture appends its own
// -MD/-MF/-o and duplicate flags have divergent semantics between
// compilers (macOS clang tolerates double -o, Linux gcc does not —
// found via sqlite on a Linux worker).
func stripCCFlags(args []string, flags ...string) []string {
	flagSet := map[string]bool{}
	for _, f := range flags {
		flagSet[f] = true
	}
	// flags that consume the following token as their argument
	twoToken := map[string]bool{"-o": true, "-MF": true, "-MT": true, "-MQ": true}
	var out []string
	for i := 0; i < len(args); i++ {
		if flagSet[args[i]] {
			if twoToken[args[i]] && i+1 < len(args) {
				i++ // skip the argument too
			}
			continue
		}
		out = append(out, args[i])
	}
	return out
}

// ccLibrary is a generated cc_library: one per source directory.
type ccLibrary struct {
	Dir     string
	Srcs    []string
	Hdrs    []string // same-dir headers
	ExtHdrs []string // cross-dir project headers (dep edges)
	Defines []string
	IncDirs []string
}

// generateCCBuilds writes BUILD.bazel files from the observed model.
func generateCCBuilds(compiles []ccCompile, links []ccLink, wd string) []finding {
	// group compiles by source directory
	libs := map[string]*ccLibrary{}
	objOwner := map[string]string{} // obj path -> lib dir
	for _, c := range compiles {
		dir := filepath.ToSlash(filepath.Dir(c.Src))
		if dir == "." {
			dir = ""
		}
		lib, ok := libs[dir]
		if !ok {
			lib = &ccLibrary{Dir: dir}
			libs[dir] = lib
		}
		lib.Srcs = append(lib.Srcs, filepath.Base(c.Src))
		objOwner[c.Obj] = dir
		for _, h := range c.Headers {
			hdir := filepath.ToSlash(filepath.Dir(h))
			if hdir == "." {
				hdir = ""
			}
			if hdir == dir {
				if !contains(lib.Hdrs, filepath.Base(h)) {
					lib.Hdrs = append(lib.Hdrs, filepath.Base(h))
				}
			} else if !contains(lib.ExtHdrs, h) {
				lib.ExtHdrs = append(lib.ExtHdrs, h)
			}
		}
		for _, d := range c.Defines {
			if !contains(lib.Defines, d) {
				lib.Defines = append(lib.Defines, d)
			}
		}
		for _, inc := range c.IncDirs {
			if !contains(lib.IncDirs, inc) {
				lib.IncDirs = append(lib.IncDirs, inc)
			}
		}
	}

	var findings []finding

	// write each library's BUILD file
	dirs := make([]string, 0, len(libs))
	for d := range libs {
		dirs = append(dirs, d)
	}
	sort.Strings(dirs)
	for _, dir := range dirs {
		lib := libs[dir]
		var b strings.Builder
		b.WriteString("load(\"@rules_cc//cc:defs.bzl\", \"cc_library\", \"cc_binary\")\n\n")
		name := ccLibName(dir)
		b.WriteString("cc_library(\n")
		fmt.Fprintf(&b, "    name = %q,\n", name)
		writeSortedStrings(&b, "srcs", lib.Srcs)
		if len(lib.Hdrs) > 0 {
			writeSortedStrings(&b, "hdrs", lib.Hdrs)
		}
		// deps: cross-dir header reads -> the library owning that dir
		deps := map[string]bool{}
		for _, h := range lib.ExtHdrs {
			hdir := filepath.ToSlash(filepath.Dir(h))
			if hdir == "." {
				hdir = ""
			}
			if _, ok := libs[hdir]; ok && hdir != dir {
				deps[ccLabel(hdir, ccLibName(hdir))] = true
			} else {
				findings = append(findings, finding{dirOrEmpty(dir), "cc-unowned-header", h})
			}
		}
		if len(deps) > 0 {
			var dl []string
			for d := range deps {
				dl = append(dl, d)
			}
			writeSortedStrings(&b, "deps", dl)
		}
		if len(lib.Defines) > 0 {
			writeSortedStrings(&b, "defines", lib.Defines)
		}
		if len(lib.IncDirs) > 0 {
			writeSortedStrings(&b, "includes", lib.IncDirs)
		}
		b.WriteString("    visibility = [\"//visibility:public\"],\n")
		b.WriteString(")\n")

		buildDir := dir
		if buildDir == "" {
			buildDir = "."
		}
		if err := os.WriteFile(filepath.Join(wd, buildDir, "BUILD.bazel"), []byte(b.String()), 0o644); err != nil {
			findings = append(findings, finding{dirOrEmpty(dir), "write-error", err.Error()})
		}
	}
	// binaries: one cc_binary per link command, in the output's directory
	// (the link's own-dir objects come via that dir's library; cross-dir
	// objects via their libraries)
	for _, l := range links {
		if l.IsAr {
			continue // archives are grouping hints, not targets
		}
		binDir := filepath.ToSlash(filepath.Dir(l.Out))
		if binDir == "." {
			binDir = ""
		}
		var b strings.Builder
		b.WriteString("cc_binary(\n")
		fmt.Fprintf(&b, "    name = %q,\n", filepath.Base(l.Out))
		b.WriteString("    deps = [\n")
		seen := map[string]bool{}
		for _, o := range l.Objects {
			od, ok := objOwner[o]
			if !ok {
				continue // object from an unobserved compile (prebuilt?)
			}
			lbl := ccLabel(od, ccLibName(od))
			if !seen[lbl] {
				seen[lbl] = true
				fmt.Fprintf(&b, "        %q,\n", lbl)
			}
		}
		b.WriteString("    ],\n)\n")

		buildDir := binDir
		if buildDir == "" {
			buildDir = "."
		}
		path := filepath.Join(wd, buildDir, "BUILD.bazel")
		if data, err := os.ReadFile(path); err == nil {
			// append to the existing library BUILD
			os.WriteFile(path, append(data, []byte("\n\n"+b.String())...), 0o644)
		} else {
			full := "load(\"@rules_cc//cc:defs.bzl\", \"cc_binary\")\n\n" + b.String()
			os.WriteFile(path, []byte(full), 0o644)
		}
	}
	return findings
}

func dirOrEmpty(dir string) string {
	if dir == "" {
		return "."
	}
	return dir
}

// ccLibName derives a library name from its directory.
func ccLibName(dir string) string {
	if dir == "" {
		return "lib"
	}
	base := filepath.Base(dir)
	base = strings.ReplaceAll(base, "-", "_")
	return base + "_lib"
}

// ccLabel maps a dir and target name to a Bazel label. Always explicit
// about the target name — the implicit default (//dir = //dir:basename)
// does not match our generated names (lib/ -> lib_lib) and produces
// confusing analysis errors.
func ccLabel(dir, name string) string {
	if dir == "" {
		return "//:" + name
	}
	return "//" + dir + ":" + name
}

func writeSortedStrings(b *strings.Builder, attr string, items []string) {
	sorted := append([]string{}, items...)
	sort.Strings(sorted)
	fmt.Fprintf(b, "    %s = [\n", attr)
	for _, s := range sorted {
		fmt.Fprintf(b, "        %q,\n", s)
	}
	b.WriteString("    ],\n")
}

// validateCCBuilds re-derives the depfile model and compares EXISTING
// BUILD files against it — the adversarial check that a tampered or
// hand-edited BUILD still declares everything the observed build reads.
// Mirrors the Go validator's finding discipline.
func validateCCBuilds(compiles []ccCompile, links []ccLink, wd string) []finding {
	var findings []finding
	// observed model: dir -> srcs, same-dir hdrs, cross-dir deps, defines
	type dirModel struct {
		srcs, hdrs, deps, defines []string
	}
	model := map[string]*dirModel{}
	objOwner := map[string]string{}
	for _, c := range compiles {
		dir := filepath.ToSlash(filepath.Dir(c.Src))
		if dir == "." {
			dir = ""
		}
		m, ok := model[dir]
		if !ok {
			m = &dirModel{}
			model[dir] = m
		}
		if !contains(m.srcs, filepath.Base(c.Src)) {
			m.srcs = append(m.srcs, filepath.Base(c.Src))
		}
		objOwner[c.Obj] = dir
		for _, h := range c.Headers {
			hdir := filepath.ToSlash(filepath.Dir(h))
			if hdir == "." {
				hdir = ""
			}
			if hdir == dir {
				if !contains(m.hdrs, filepath.Base(h)) {
					m.hdrs = append(m.hdrs, filepath.Base(h))
				}
			} else if _, known := model[hdir]; known {
				lbl := ccLabel(hdir, ccLibName(hdir))
				if !contains(m.deps, lbl) {
					m.deps = append(m.deps, lbl)
				}
			}
		}
		for _, d := range c.Defines {
			if !contains(m.defines, d) {
				m.defines = append(m.defines, d)
			}
		}
	}
	// binary deps from links
	binDeps := map[string][]string{} // binary name -> required lib labels
	for _, l := range links {
		if l.IsAr {
			continue
		}
		var deps []string
		for _, o := range l.Objects {
			if od, ok := objOwner[o]; ok {
				lbl := ccLabel(od, ccLibName(od))
				if !contains(deps, lbl) {
					deps = append(deps, lbl)
				}
			}
		}
		binDeps[filepath.Base(l.Out)] = deps
	}

	for dir, m := range model {
		buildDir := dir
		if buildDir == "" {
			buildDir = "."
		}
		rules, err := parseBuildFile(filepath.Join(wd, buildDir, "BUILD.bazel"))
		if err != nil {
			findings = append(findings, finding{dirOrEmpty(dir), "no-build", "no BUILD file for observed compiles"})
			continue
		}
		var libRule *buildRule
		for i := range rules {
			if rules[i].kind == "cc_library" {
				libRule = &rules[i]
				break
			}
		}
		if libRule == nil {
			findings = append(findings, finding{dirOrEmpty(dir), "no-build", "no cc_library for observed compiles"})
			continue
		}
		for _, src := range m.srcs {
			if !contains(libRule.srcs, src) {
				findings = append(findings, finding{dirOrEmpty(dir), "missing-src", src})
			}
		}
		for _, hdr := range m.hdrs {
			if !contains(libRule.hdrs, hdr) {
				findings = append(findings, finding{dirOrEmpty(dir), "missing-hdr", hdr})
			}
		}
		for _, dep := range m.deps {
			if !hasDepLabel(libRule.deps, dep) {
				findings = append(findings, finding{dirOrEmpty(dir), "missing-dep", dep})
			}
		}
		for _, def := range m.defines {
			if !contains(libRule.defines, def) {
				findings = append(findings, finding{dirOrEmpty(dir), "missing-define", def})
			}
		}
	}
	// binaries: declared with required deps
	for bin, wantDeps := range binDeps {
		binDir := ""
		for _, l := range links {
			if filepath.Base(l.Out) == bin && !l.IsAr {
				binDir = filepath.ToSlash(filepath.Dir(l.Out))
				if binDir == "." {
					binDir = ""
				}
				break
			}
		}
		buildDir := binDir
		if buildDir == "" {
			buildDir = "."
		}
		rules, err := parseBuildFile(filepath.Join(wd, buildDir, "BUILD.bazel"))
		if err != nil {
			continue // library findings already cover a missing BUILD
		}
		var binRule *buildRule
		for i := range rules {
			if rules[i].kind == "cc_binary" && rules[i].name == bin {
				binRule = &rules[i]
				break
			}
		}
		if binRule == nil {
			findings = append(findings, finding{dirOrEmpty(binDir), "missing-binary", bin})
			continue
		}
		for _, dep := range wantDeps {
			if !hasDepLabel(binRule.deps, dep) {
				findings = append(findings, finding{dirOrEmpty(binDir), "missing-dep", dep + " (for binary " + bin + ")"})
			}
		}
	}
	return findings
}

// cmdBazelGenerateCC is the C/C++ entry: dry-run discovery, depfile
// capture, BUILD generation, then validation.
func cmdBazelGenerateCC(wd string, validateOnly bool) {
	dry := []string{"make", "-Bn"}
	if env := os.Getenv("BRINE_CC_DRY"); env != "" {
		dry = splitCommand(env)
	}

	fmt.Printf("[brine] bazel-generate: C/C++ mode — discovering build via %s\n", strings.Join(dry, " "))
	compiles, links, err := ccDiscovery(dry, wd)
	if err != nil {
		fatal("bazel-generate cc: %v", err)
	}
	if len(compiles) == 0 {
		fatal("bazel-generate cc: no compile commands discovered (is this a C/C++ make project?)")
	}
	fmt.Printf("[brine] bazel-generate: %d compiles, %d link/archive steps\n", len(compiles), len(links))

	if err := captureDepfiles(compiles, wd); err != nil {
		fatal("bazel-generate cc: %v", err)
	}
	totalHdrs := 0
	for _, c := range compiles {
		totalHdrs += len(c.Headers)
	}
	fmt.Printf("[brine] bazel-generate: depfiles captured — %d project header dependencies\n", totalHdrs)

	var findings []finding
	if validateOnly {
		findings = validateCCBuilds(compiles, links, wd)
	} else {
		findings = generateCCBuilds(compiles, links, wd)
		// generation followed by the adversarial re-check: parse what was
		// written and compare against the same depfile truth
		findings = validateCCBuilds(compiles, links, wd)
	}
	if len(findings) == 0 {
		fmt.Printf("[brine] bazel-generate: validation clean — %d compiles match depfile ground truth\n", len(compiles))
	} else {
		printFindings(findings, len(compiles))
	}

	// minimal MODULE.bazel for rules_cc
	if _, err := os.Stat(filepath.Join(wd, "MODULE.bazel")); os.IsNotExist(err) {
		os.WriteFile(filepath.Join(wd, "MODULE.bazel"), []byte("module(name = \"cproject\")\n\nbazel_dep(name = \"rules_cc\", version = \"0.2.17\")\n"), 0o644)
		fmt.Printf("[brine] bazel-generate: created MODULE.bazel (rules_cc)\n")
	}

	if len(findings) > 0 {
		os.Exit(1)
	}
}
