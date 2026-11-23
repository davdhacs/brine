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
	Cmd     []string // full argv (original relative paths, run in CtxDir)
	CtxDir  string   // absolute make-recursion context (Entering directory)
	Src     string   // the .c/.cc file (project-relative)
	Obj     string   // the -o target (project-relative)
	Headers []string // project-relative headers from the depfile
	Defines []string // -D flags
	IncDirs []string // -I flags (project-relative when in-project)
}

// ccLink is one observed link or archive step.
type ccLink struct {
	Cmd        []string
	Out        string              // binary or archive
	Objects    []string            // .o files consumed, or .c/.cc sources (combined compile+link)
	Defines    []string            // observed on combined compile+link commands
	LinkOpts   []string            // -l/-L flags: system libraries the link needs
	IncDirs    []string            // -I flags observed on the link line
	SrcHeaders map[string][]string // per-source headers (depfile-captured)
	CtxDir     string              // absolute make-recursion context
	LibRefs    []string            // .la/.a library inputs (project libraries, not objects)
	IsAr       bool
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
	// across several lines, and make appends trailing spaces after the
	// backslash before the newline (sqlite: "\ <space> <newline>") — a
	// literal backslash-newline match misses those
	text := contRe.ReplaceAllString(string(out), " ")
	// automake VPATH quoting wraps every source in a shell fragment:
	// `test -f 'altsvc.c' || echo './'`altsvc.c — the fragment contains
	// spaces, so field splitting shatters it into argv debris that breaks
	// re-execution. Strip the idiom at line level; the bare filename
	// after the closing backtick remains.
	text = automakeFragmentRe.ReplaceAllString(text, "")
	// GNU make recursion markers: commands from subdirectory makes carry
	// paths relative to THEIR directory (curl: "altsvc.c" means lib/).
	ctxStack := []string{wd}
	for _, line := range strings.Split(text, "\n") {
		if m := makeDirRe.FindStringSubmatch(line); m != nil {
			if m[1] == "Entering" {
				ctxStack = append(ctxStack, m[2])
			} else if len(ctxStack) > 1 {
				ctxStack = ctxStack[:len(ctxStack)-1]
			}
			continue
		}
		ctxDir := ctxStack[len(ctxStack)-1]
		for _, cmd := range splitCompound(line) {
			fields := splitCommand(cmd)
			// libtool wrappers: /bin/sh ../libtool --silent --tag=CC
			// --mode=compile gcc ... — unwrap to the embedded command
			// (curl and every autotools+libtool project)
			if unwrapped := unwrapLibtool(fields); unwrapped != nil {
				fields = unwrapped
			}
			if len(fields) < 2 {
				continue
			}
			if c := classifyCompile(fields); c != nil {
				c.CtxDir = ctxDir
				relativizeCC(c, wd)
				compiles = append(compiles, *c)
			} else if l := classifyLink(fields); l != nil {
				relativizeCCLink(l, wd, ctxDir)
				links = append(links, *l)
			}
		}
	}
	return compiles, links, nil
}

// splitCompound splits a make -n line on semicolons and && chains,
// keeping only compiler-shaped segments. Real recipes read:
//
//	depbase=...; gcc ... -c foo.c && mv -f $depbase.Tpo $depbase.Po
//
// (automake's depfile idiom — unexpanded make variables, mv steps, and
// echo labels are all dropped; only the gcc command classifies).
// Quote-aware: labels contain spaces.
func splitCompound(line string) []string {
	var segs []string
	var cur strings.Builder
	inQuote := false
	flush := func() {
		if cur.Len() > 0 {
			segs = append(segs, cur.String())
			cur.Reset()
		}
	}
	for _, r := range line {
		switch {
		case r == '"':
			inQuote = !inQuote
			cur.WriteRune(r)
		case r == ';' && !inQuote:
			flush()
		case r == '&' && !inQuote && strings.HasSuffix(cur.String(), "&"):
			// "&&": drop the single & just written, split here
			t := cur.String()
			cur.Reset()
			cur.WriteString(t[:len(t)-1])
			flush()
		default:
			cur.WriteRune(r)
		}
	}
	flush()
	var out []string
	for _, s := range segs {
		s = strings.TrimSpace(s)
		if s == "" || strings.HasPrefix(s, "echo ") {
			continue
		}
		fields := splitCommand(s)
		if len(fields) == 0 {
			continue
		}
		first := filepath.Base(fields[0])
		if ccCompilerRe.MatchString(first) || first == "sh" || first == "bash" || strings.HasPrefix(first, "libtool") {
			out = append(out, s)
		}
		// everything else (depbase=..., mv -f $x, test -f ...) is make
		// bookkeeping with unexpanded variables — not runnable, dropped
	}
	return out
}

// unwrapLibtool extracts the embedded compiler command from a libtool
// wrapper: [/bin/sh] [path/]libtool [--silent] [--tag=CC]
// --mode=compile|link gcc ... Returns nil when not a libtool invocation.
func unwrapLibtool(fields []string) []string {
	isLibtool := false
	for _, f := range fields {
		base := filepath.Base(f)
		if base == "libtool" || strings.HasPrefix(base, "libtool ") {
			isLibtool = true
			break
		}
	}
	if !isLibtool {
		return nil
	}
	for i, f := range fields {
		if f == "--mode=compile" || f == "--mode=link" {
			if i+1 < len(fields) {
				return fields[i+1:]
			}
		}
	}
	return nil
}

// relativizeCC converts absolute source/object paths to project-relative
// (autosetup/cmake builds emit absolute paths — sqlite: "cc -c
// /home/.../sqlite3.c -o sqlite3.o"). Paths outside the project are left
// absolute and will not group into libraries.
func relativizeCC(c *ccCompile, wd string) {
	c.Src = relTo(wd, joinCtx(c.CtxDir, c.Src))
	c.Obj = relTo(wd, joinCtx(c.CtxDir, c.Obj))
}

func relativizeCCLink(l *ccLink, wd, ctxDir string) {
	l.CtxDir = ctxDir
	l.Out = relTo(wd, joinCtx(ctxDir, l.Out))
	for i, o := range l.Objects {
		l.Objects[i] = relTo(wd, joinCtx(ctxDir, o))
	}
}

// joinCtx resolves a possibly-relative path against the make context
// directory (subdir makes emit paths relative to themselves).
func joinCtx(ctxDir, p string) string {
	if ctxDir == "" || filepath.IsAbs(p) {
		return p
	}
	return filepath.Join(ctxDir, p)
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

// cleanAutomakeSource strips automake's VPATH shell-quoting from a
// source argument: `test -f 'altsvc.c' || echo './'`altsvc.c → altsvc.c
// (exec'ing the capture command has no shell to evaluate the backticks).
func cleanAutomakeSource(arg string) string {
	if !strings.Contains(arg, "`") {
		return arg
	}
	if idx := strings.LastIndex(arg, "`"); idx >= 0 && idx+1 < len(arg) {
		return arg[idx+1:]
	}
	return arg
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

// contRe matches a make line continuation: backslash, optional trailing
// whitespace, newline.
var contRe = regexp.MustCompile(`\\\s*\n`)

// makeDirRe matches GNU make recursion markers:
// make[1]: Entering directory '/path/to/lib'
var makeDirRe = regexp.MustCompile(`^make\[\d+\]: (Entering|Leaving) directory '(.+)'`)

// automakeFragmentRe matches automake's VPATH source quoting:
// `test -f 'x.c' || echo './'`x.c → stripped whole; x.c remains.
var automakeFragmentRe = regexp.MustCompile("\x60test -f '[^']*' \\|\\| echo '[^']*'\x60")

var ccCompilerRe = regexp.MustCompile(`^(cc|gcc|clang|gcc-[0-9]+|clang-[0-9]+|g\+\+|clang\+\+|c\+\+)$`)

// classifyCompile recognizes a compilation command: compiler ... -c src -o obj
func classifyCompile(f []string) *ccCompile {
	if !ccCompilerRe.MatchString(f[0]) {
		return nil
	}
	var src, obj string
	hasC := false
	var defines, incs []string
	for i := 1; i < len(f); i++ {
		switch {
		case f[i] == "-c":
			hasC = true // compilation marker — REQUIRED: without it a
			// combined compile+link ("cc -o sqlite3 shell.c sqlite3.c")
			// would classify as a compile and starve the link classifier
		case f[i] == "-o" && i+1 < len(f):
			obj = f[i+1]
			i++
		case strings.HasPrefix(f[i], "-D"):
			defines = append(defines, strings.TrimPrefix(f[i], "-D"))
		case strings.HasPrefix(f[i], "-I"):
			incs = append(incs, strings.TrimPrefix(f[i], "-I"))
		case strings.HasSuffix(f[i], ".c"), strings.HasSuffix(f[i], ".cc"),
			strings.HasSuffix(f[i], ".cpp"), strings.HasSuffix(f[i], ".cxx"):
			src = cleanAutomakeSource(f[i])
			// rewrite the argv so depfile capture re-runs without the
			// shell fragment (exec has no shell to evaluate backticks)
			if src != f[i] {
				f[i] = src
			}
		}
	}
	if src == "" || obj == "" || !hasC {
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
			} else if strings.HasSuffix(f[i], ".o") || strings.HasSuffix(f[i], ".lo") {
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
	var objs, libRefs []string
	var defines, linkOpts, incDirs []string
	for i := 1; i < len(f); i++ {
		if f[i] == "-o" && i+1 < len(f) {
			if i+2 <= len(f) {
				out = f[i+1]
				i++
			}
			continue
		}
		if strings.HasSuffix(f[i], ".o") || strings.HasSuffix(f[i], ".lo") {
			objs = append(objs, f[i]) // .lo: libtool object naming (curl)
		} else if strings.HasSuffix(f[i], ".la") || strings.HasSuffix(f[i], ".a") {
			// project library inputs: the consuming binary deps on the
			// package that builds them (curl links ../lib/libcurlu.la)
			libRefs = append(libRefs, f[i])
		} else if strings.HasSuffix(f[i], ".c") || strings.HasSuffix(f[i], ".cc") ||
			strings.HasSuffix(f[i], ".cpp") || strings.HasSuffix(f[i], ".cxx") {
			// combined compile+link (sqlite's shell: "cc -o sqlite3
			// shell.c sqlite3.c ... -lreadline") — sources compile straight
			// into the binary
			objs = append(objs, f[i])
		} else if strings.HasPrefix(f[i], "-D") {
			defines = append(defines, strings.TrimPrefix(f[i], "-D"))
		} else if strings.HasPrefix(f[i], "-l") || strings.HasPrefix(f[i], "-L") {
			linkOpts = append(linkOpts, f[i])
		} else if strings.HasPrefix(f[i], "-I") {
			incDirs = append(incDirs, strings.TrimPrefix(f[i], "-I"))
		}
	}
	if out == "" || len(objs) == 0 || strings.HasSuffix(out, ".o") {
		return nil // a compile, not a link
	}
	return &ccLink{Cmd: f, Out: out, Objects: objs, Defines: defines, LinkOpts: linkOpts, IncDirs: incDirs, SrcHeaders: map[string][]string{}, LibRefs: libRefs}
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
		// drop unexpanded make/shell variables (automake depbase idiom:
		// "-MT $depbase.Tpo") — exec has no shell to expand them
		var clean []string
		for _, a := range args {
			if !strings.Contains(a, "$") {
				clean = append(clean, a)
			}
		}
		args = append(clean, "-MD", "-MF", depFile, "-o", throwObj)
		cmd := exec.Command(args[0], args[1:]...)
		cmd.Dir = c.CtxDir
		if cmd.Dir == "" {
			cmd.Dir = wd
		}
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("depfile capture for %s: %v\nargv: %s\n%s", c.Src, err, strings.Join(args, " "), out)
		}
		// depfile paths are relative to the COMPILE's directory (the make
		// context), not the project root — and ..-prefixed paths from
		// sibling contexts (curl: ../lib/curlx/base64.h from src/) land
		// inside the project only through that context
		c.Headers = depfiles.ParseCtx(depFile, wd, c.CtxDir)
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

// captureLinkSourceDepfiles depfile-captures the sources of combined
// compile+link commands (they are never compiled separately, so their
// header closures exist nowhere else — sqlite's shell.c reads sqlite3.h
// only here, and without it the generated rule resolves the header from
// /usr/include instead: version mismatch, found via parity).
func captureLinkSourceDepfiles(links []ccLink, wd string) error {
	tmpDir, err := os.MkdirTemp("", "brine-ccdeps")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmpDir)
	for li := range links {
		l := &links[li]
		if l.IsAr || l.SrcHeaders == nil {
			continue
		}
		si := 0
		for _, o := range l.Objects {
			if !isCSource(o) {
				continue
			}
			src := relTo(wd, o)
			depFile := filepath.Join(tmpDir, fmt.Sprintf("link%d-%d.d", li, si))
			throwObj := filepath.Join(tmpDir, fmt.Sprintf("linkthrow%d-%d.o", li, si))
			args := []string{l.Cmd[0], "-c", joinCtx(l.CtxDir, o)}
			for _, d := range l.Defines {
				args = append(args, "-D"+d)
			}
			for _, inc := range l.IncDirs {
				args = append(args, "-I"+inc)
			}
			args = append(args, "-MD", "-MF", depFile, "-o", throwObj)
			cmd := exec.Command(args[0], args[1:]...)
			cmd.Dir = l.CtxDir
			if cmd.Dir == "" {
				cmd.Dir = wd
			}
			if out, err := cmd.CombinedOutput(); err != nil {
				return fmt.Errorf("link-source depfile capture for %s: %v\nargv: %s\n%s", src, err, strings.Join(args, " "), out)
			}
			headers := depfiles.ParseCtx(depFile, wd, l.CtxDir)
			l.SrcHeaders[src] = headers
			si++
		}
	}
	return nil
}

func isCSource(p string) bool {
	return strings.HasSuffix(p, ".c") || strings.HasSuffix(p, ".cc") ||
		strings.HasSuffix(p, ".cpp") || strings.HasSuffix(p, ".cxx")
}

// ccLibrary is a generated cc_library: one per source directory.
type ccLibrary struct {
	Dir     string
	Name    string
	Srcs    []string
	Hdrs    []string // same-dir headers
	ExtHdrs []string // cross-dir project headers (dep edges)
	Defines []string
	IncDirs []string
}

// MatchesLink reports whether a combined compile+link command's defines
// are all provided by this library — the link's sources compile the same
// way through the library as they would inline.
func (l *ccLibrary) MatchesLink(link ccLink) bool {
	for _, d := range link.Defines {
		if !contains(l.Defines, d) {
			return false
		}
	}
	return true
}

func dedup(items []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, x := range items {
		if !seen[x] {
			seen[x] = true
			out = append(out, x)
		}
	}
	return out
}

// relLabelPath converts an absolute source path to a BUILD-relative
// label path (../../foo.c from another dir, or plain foo.c).
func relLabelPath(wd, p string) string {
	rel := relTo(wd, p)
	return rel
}

// mergeCyclicDirs repeatedly merges child directories into an ancestor
// when their libraries reference each other's headers (a dependency
// cycle). Only ancestor-descendant pairs merge (cross-package ../ srcs
// are illegal in Bazel); sibling cycles surface as findings instead.
func mergeCyclicDirs(dirLibs map[string][]*ccLibrary, objOwner map[string]string, libs map[string]*ccLibrary) {
	refs := func(dir string) map[string]bool {
		out := map[string]bool{}
		for _, lib := range dirLibs[dir] {
			for _, h := range lib.ExtHdrs {
				hdir := filepath.ToSlash(filepath.Dir(h))
				if hdir == "." {
					hdir = ""
				}
				if hdir != dir {
					out[hdir] = true
				}
			}
		}
		return out
	}
	changed := true
	for changed {
		changed = false
		for dirA := range dirLibs {
			depsA := refs(dirA)
			for dirB := range depsA {
				if dirB == dirA {
					continue
				}
				if _, ok := dirLibs[dirB]; !ok {
					continue
				}
				depsB := refs(dirB)
				if !depsB[dirA] {
					continue // not mutual
				}
				// merge child into ancestor (or lexicographic lesser)
				parent, child := dirA, dirB
				if !isAncestor(parent, child) {
					if isAncestor(child, parent) {
						parent, child = child, parent
					} else {
						continue // sibling cycle: leave for findings
					}
				}
				prefix := ""
				if child != "" {
					prefix = strings.TrimPrefix(child, parent+"/") + "/"
				}
				for _, lib := range dirLibs[child] {
					lib.Dir = parent
					for i, src := range lib.Srcs {
						lib.Srcs[i] = prefix + src
					}
					for i, h := range lib.Hdrs {
						lib.Hdrs[i] = prefix + h
					}
					// headers under either merged dir are SAME-PACKAGE now:
					// declare them on the referencing library (Bazel allows
					// hdrs from any subdir of the package) instead of a dep
					// edge — internal deps would recreate the cycle the
					// merge exists to break, and a separate rule's hdrs are
					// not action inputs for this rule's compiles
					var ext []string
					for _, h := range lib.ExtHdrs {
						hdir := filepath.ToSlash(filepath.Dir(h))
						if hdir == "." {
							hdir = ""
						}
						if hdir == parent || hdir == child || isAncestor(parent, hdir) {
							rel := h
							if parent != "" {
								rel = strings.TrimPrefix(h, parent+"/")
							}
							if !contains(lib.Hdrs, rel) {
								lib.Hdrs = append(lib.Hdrs, rel)
							}
						} else {
							ext = append(ext, h)
						}
					}
					lib.ExtHdrs = ext
					lib.Hdrs = dedup(lib.Hdrs)
					dirLibs[parent] = append(dirLibs[parent], lib)
				}
				delete(dirLibs, child)
				changed = true
				break
			}
			if changed {
				break
			}
		}
	}
	// final pass: parent-side libraries also reference merged-away child
	// headers — convert those to same-package hdrs too
	for _, ls := range dirLibs {
		for _, lib := range ls {
			var ext []string
			for _, h := range lib.ExtHdrs {
				hdir := filepath.ToSlash(filepath.Dir(h))
				if hdir == "." {
					hdir = ""
				}
				if _, live := dirLibs[hdir]; !live && isAncestor(lib.Dir, hdir) {
					rel := h
					if lib.Dir != "" {
						rel = strings.TrimPrefix(h, lib.Dir+"/")
					}
					if !contains(lib.Hdrs, rel) {
						lib.Hdrs = append(lib.Hdrs, rel)
					}
				} else {
					ext = append(ext, h)
				}
			}
			lib.ExtHdrs = ext
			lib.Hdrs = dedup(lib.Hdrs)
		}
	}
}

// resolvePkgDir walks a header's directory up to the nearest LIVE
// package dir — merged-away subdirs (lib/curlx merged into lib) must
// resolve to the package that now owns their headers.
func resolvePkgDir(dirLibs map[string][]*ccLibrary, dir string) string {
	if dir == "." {
		dir = ""
	}
	for {
		if _, ok := dirLibs[dir]; ok {
			return dir
		}
		i := strings.LastIndexByte(dir, '/')
		if i < 0 {
			return dir // root or unmatched: caller reports the finding
		}
		dir = dir[:i]
	}
}

// isAncestor reports whether dir a is an ancestor (prefix) of dir b.
func isAncestor(a, b string) bool {
	if a == "" {
		return true // root is ancestor of all
	}
	return strings.HasPrefix(b, a+"/")
}

// flagSignature identifies a compile's flag set: sorted defines+includes.
func flagSignature(c ccCompile) string {
	all := append(append([]string{}, c.Defines...), c.IncDirs...)
	sort.Strings(all)
	return strings.Join(all, ",")
}

// primaryLibForDir returns the first-created library in a dir (deps and
// headers resolve against it).
func primaryLibForDir(dirLibs map[string][]*ccLibrary, dir string) *ccLibrary {
	ls := dirLibs[dir]
	if len(ls) == 0 {
		return nil
	}
	// the package's dominant build: most sources wins (curl's lib package
	// has curlx_lib alphabetically first with 13 sources, but the package
	// IS lib_lib with ~90 — cross-package deps must reach the main build)
	best := ls[0]
	for _, l := range ls[1:] {
		if len(l.Srcs) > len(best.Srcs) {
			best = l
		}
	}
	return best
}

// isSystemInclude filters toolchain/system -I paths from generated
// includes (compiler territory, not project structure).
func isSystemInclude(inc string) bool {
	if !filepath.IsAbs(inc) {
		return false
	}
	for _, p := range []string{"/usr/", "/opt/", "/Applications/"} {
		if strings.HasPrefix(inc, p) {
			return true
		}
	}
	return false
}

// generateCCBuilds writes BUILD.bazel files from the observed model.
// One file per directory, written once (idempotent regeneration): all of
// the dir's cc_library rules (one per flag signature) followed by the
// cc_binary rules whose outputs live there.
func generateCCBuilds(compiles []ccCompile, links []ccLink, wd string) []finding {
	debug := os.Getenv("BRINE_CC_DEBUG") == "1"

	var findings []finding

	// group compiles by (dir, flag signature): the same source compiled
	// twice with different defines (sqlite compiles its amalgamation for
	// both the library and the shell) cannot live in one cc_library —
	// different flag sets get different targets.
	libs := map[string]*ccLibrary{} // key: dir + "|" + flag signature
	objOwner := map[string]string{} // obj path -> lib key
	for _, c := range compiles {
		dir := filepath.ToSlash(filepath.Dir(c.Src))
		if dir == "." {
			dir = ""
		}
		key := dir + "|" + flagSignature(c)
		lib, ok := libs[key]
		if !ok {
			lib = &ccLibrary{Dir: dir, Name: ccLibName(dir)}
			libs[key] = lib
		}
		if !contains(lib.Srcs, filepath.Base(c.Src)) {
			lib.Srcs = append(lib.Srcs, filepath.Base(c.Src))
		}
		if debug {
			fmt.Fprintf(os.Stderr, "[cc-debug] %s -> %s | hdrs=%d sig=%s\n",
				c.Src, key, len(c.Headers), flagSignature(c))
		}
		objOwner[c.Obj] = key
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
			if isSystemInclude(inc) {
				continue // /usr/include and friends are toolchain territory
			}
			if inc == "." {
				continue // same-dir quoted includes resolve implicitly in
				// Bazel (sources and hdrs share the sandbox dir); an
				// explicit "." is rejected as a workspace-root include
			}
			if !contains(lib.IncDirs, inc) {
				lib.IncDirs = append(lib.IncDirs, inc)
			}
		}
	}
	// suffix flag-set variants within a dir: lib, lib_2, lib_3...
	dirLibs := map[string][]*ccLibrary{}
	for _, lib := range libs {
		dirLibs[lib.Dir] = append(dirLibs[lib.Dir], lib)
	}
	// merge mutually-dependent directories: curl's lib/ and lib/curlx/
	// (and vauth/vquic/vssh/vtls) include each other's headers, which
	// Bazel rejects as a dependency cycle. A child dir with no BUILD of
	// its own belongs to the parent's package — merge child sources into
	// the ancestor library with subdir-relative paths.
	mergeCyclicDirs(dirLibs, objOwner, libs)
	for dir, ls := range dirLibs {
		sort.Slice(ls, func(i, j int) bool { return ls[i].Name < ls[j].Name })
		for i := 1; i < len(ls); i++ {
			ls[i].Name = fmt.Sprintf("%s_%d", ccLibName(dir), i+1)
		}
	}

	// headers referenced by OTHER packages' compiles but read by no
	// compile of this package (curl: curlx/curlx.h is included only by
	// src/tool_setup.h — no lib/ depfile ever lists it, so no rule
	// declares it and dependents' includes fail). Declare such headers on
	// the owning package's primary library.
	extReferenced := map[string]map[string]bool{} // pkg dir -> header set
	for _, ls := range dirLibs {
		for _, lib := range ls {
			for _, h := range lib.ExtHdrs {
				pdir := resolvePkgDir(dirLibs, filepath.ToSlash(filepath.Dir(h)))
				if extReferenced[pdir] == nil {
					extReferenced[pdir] = map[string]bool{}
				}
				extReferenced[pdir][h] = true
			}
		}
	}
	for pdir, hdrs := range extReferenced {
		ls := dirLibs[pdir]
		if len(ls) == 0 {
			continue
		}
		for h := range hdrs {
			rel := h
			if pdir != "" {
				rel = strings.TrimPrefix(h, pdir+"/")
			}
			for _, lib := range ls {
				if !contains(lib.Hdrs, rel) {
					lib.Hdrs = append(lib.Hdrs, rel)
				}
			}
		}
	}
	// within a package, every flag-variant lib declares the union of the
	// package's headers: a variant's compile may read a header another
	// variant declared (sandbox inputs are per-rule), and dependents
	// resolving through the primary need every header present on it
	for _, ls := range dirLibs {
		if len(ls) < 2 {
			continue
		}
		var union []string
		for _, lib := range ls {
			union = append(union, lib.Hdrs...)
		}
		union = dedup(union)
		for _, lib := range ls {
			lib.Hdrs = union
		}
	}

	// binaries by output dir
	binsByDir := map[string][]ccLink{}
	for _, l := range links {
		if l.IsAr || strings.HasSuffix(l.Out, ".so") || strings.HasSuffix(l.Out, ".dylib") || strings.HasSuffix(l.Out, ".la") {
			continue // archives, shared libs, and libtool .la libraries are
			// grouping hints; cc_library covers compilation
		}
		binDir := filepath.ToSlash(filepath.Dir(l.Out))
		if binDir == "." {
			binDir = ""
		}
		binsByDir[binDir] = append(binsByDir[binDir], l)
	}

	// one BUILD file per dir: libraries + binaries composed, single write
	allDirs := map[string]bool{}
	for dir := range dirLibs {
		allDirs[dir] = true
	}
	for dir := range binsByDir {
		allDirs[dir] = true
	}
	sortedDirs := make([]string, 0, len(allDirs))
	for dir := range allDirs {
		sortedDirs = append(sortedDirs, dir)
	}
	sort.Strings(sortedDirs)

	for _, dir := range sortedDirs {
		var b strings.Builder
		hasLib := len(dirLibs[dir]) > 0
		if hasLib {
			b.WriteString("load(\"@rules_cc//cc:defs.bzl\", \"cc_library\", \"cc_binary\")\n\n")
		} else {
			b.WriteString("load(\"@rules_cc//cc:defs.bzl\", \"cc_binary\")\n\n")
		}
		// libraries in this dir
		for _, lib := range dirLibs[dir] {
			b.WriteString("cc_library(\n")
			fmt.Fprintf(&b, "    name = %q,\n", lib.Name)
			writeSortedStrings(&b, "srcs", lib.Srcs)
			if len(lib.Hdrs) > 0 {
				writeSortedStrings(&b, "hdrs", lib.Hdrs)
			}
			deps := map[string]bool{}
			for _, h := range lib.ExtHdrs {
				hdir := resolvePkgDir(dirLibs, filepath.ToSlash(filepath.Dir(h)))
				if primary := primaryLibForDir(dirLibs, hdir); primary != nil && hdir != dir {
					deps[ccLabel(hdir, primary.Name)] = true
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
			b.WriteString(")\n\n")
		}
		// binaries whose outputs live in this dir
		for _, l := range binsByDir[dir] {
			var depList []string
			seen := map[string]bool{}
			addDep := func(lbl string) {
				if !seen[lbl] {
					seen[lbl] = true
					depList = append(depList, lbl)
				}
			}
			// project library inputs (.la/.a): dep on the owning package
			for _, ref := range l.LibRefs {
				rdir := resolvePkgDir(dirLibs, filepath.ToSlash(filepath.Dir(relTo(wd, joinCtx(l.CtxDir, ref)))))
				if primary := primaryLibForDir(dirLibs, rdir); primary != nil {
					addDep(ccLabel(rdir, primary.Name))
				}
			}
			// combined compile+link sources that no same-signature library
			// owns: collected into a dedicated link-signature library so
			// they compile with the link's defines AND resolve their
			// headers from the project (sqlite's shell.c otherwise picks
			// up /usr/include/sqlite3.h — version mismatch, found via
			// parity on a Linux worker)
			linkLibSrcs := map[string][]string{} // dir -> srcs for the link lib
			for _, o := range l.Objects {
				if key, ok := objOwner[o]; ok {
					// an observed .o: dep on the library that compiles it
					if lib, ok := libs[key]; ok {
						addDep(ccLabel(lib.Dir, lib.Name))
					}
					continue
				}
				// unowned object: automake places some objects under paths
				// that differ from the compile's -o form (src/Makefile
				// builds curlx objects into ../lib/curlx/ while naming
				// them curl-timeval.o) — match by basename when unique
				if strings.HasSuffix(o, ".o") || strings.HasSuffix(o, ".lo") {
					base := filepath.Base(o)
					var match string
					ambiguous := false
					for k := range objOwner {
						if filepath.Base(k) == base {
							if match != "" {
								ambiguous = true
								break
							}
							match = k
						}
					}
					if !ambiguous && match != "" {
						if lib, ok := libs[objOwner[match]]; ok {
							addDep(ccLabel(lib.Dir, lib.Name))
						}
						continue
					}
				}
				if strings.HasSuffix(o, ".c") || strings.HasSuffix(o, ".cc") ||
					strings.HasSuffix(o, ".cpp") || strings.HasSuffix(o, ".cxx") {
					src := relTo(wd, o)
					base := filepath.Base(src)
					sdir := filepath.ToSlash(filepath.Dir(src))
					if sdir == "." {
						sdir = ""
					}
					matched := false
					for _, lib := range dirLibs[sdir] {
						if contains(lib.Srcs, base) && lib.MatchesLink(l) {
							addDep(ccLabel(lib.Dir, lib.Name))
							matched = true
							break
						}
					}
					if !matched {
						linkLibSrcs[sdir] = append(linkLibSrcs[sdir], base)
					}
				}
			}
			// emit the link-signature library before the binary
			if len(linkLibSrcs) > 0 {
				var sdirs []string
				for d := range linkLibSrcs {
					sdirs = append(sdirs, d)
				}
				sort.Strings(sdirs)
				for _, sdir := range sdirs {
					srcs := dedup(linkLibSrcs[sdir])
					libName := filepath.Base(l.Out) + "_lib"
					// headers: same-dir headers of the sources, known from
					// the depfile model of same-dir libraries and from the
					// link-source depfile capture (shell.c's sqlite3.h)
					var hdrs []string
					for _, lib := range dirLibs[sdir] {
						hdrs = append(hdrs, lib.Hdrs...)
					}
					for _, src := range srcs {
						for _, h := range l.SrcHeaders[src] {
							hdir := filepath.ToSlash(filepath.Dir(h))
							if hdir == "." {
								hdir = ""
							}
							if hdir == sdir {
								hdrs = append(hdrs, filepath.Base(h))
							}
						}
					}
					b.WriteString("cc_library(\n")
					fmt.Fprintf(&b, "    name = %q,\n", libName)
					writeSortedStrings(&b, "srcs", srcs)
					if len(hdrs) > 0 {
						writeSortedStrings(&b, "hdrs", dedup(hdrs))
					}
					if len(l.Defines) > 0 {
						writeSortedStrings(&b, "defines", l.Defines)
					}
					for _, lib := range dirLibs[sdir] {
						if len(lib.IncDirs) > 0 {
							writeSortedStrings(&b, "includes", lib.IncDirs)
							break
						}
					}
					b.WriteString("    visibility = [\"//visibility:public\"],\n")
					b.WriteString(")\n\n")
					if sdir == dir {
						addDep(":" + libName)
					} else {
						addDep(ccLabel(sdir, libName))
					}
				}
			}
			// the binary rule itself — AFTER any link-signature libraries
			// emitted above (writing it earlier spliced the library into
			// the middle of the binary rule)
			b.WriteString("cc_binary(\n")
			fmt.Fprintf(&b, "    name = %q,\n", filepath.Base(l.Out))
			if len(depList) > 0 {
				writeSortedStrings(&b, "deps", depList)
			}
			// system libraries the observed link used (-lreadline -lz ...):
			// without them the link fails on undefined references
			if len(l.LinkOpts) > 0 {
				writeSortedStrings(&b, "linkopts", l.LinkOpts)
			}
			b.WriteString(")\n\n")
		}
		buildDir := dir
		if buildDir == "" {
			buildDir = "."
		}
		if err := os.WriteFile(filepath.Join(wd, buildDir, "BUILD.bazel"), []byte(b.String()), 0o644); err != nil {
			findings = append(findings, finding{dirOrEmpty(dir), "write-error", err.Error()})
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
			} else {
				pdir := hdir
				for {
					if _, known := model[pdir]; known || pdir == "" {
						break
					}
					i := strings.LastIndexByte(pdir, '/')
					if i < 0 {
						break
					}
					pdir = pdir[:i]
				}
				if _, known := model[pdir]; known {
					lbl := ccLabel(pdir, ccLibName(pdir))
					if !contains(m.deps, lbl) {
						m.deps = append(m.deps, lbl)
					}
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
		if l.IsAr || strings.HasSuffix(l.Out, ".so") || strings.HasSuffix(l.Out, ".dylib") || strings.HasSuffix(l.Out, ".la") {
			continue // same skip as generation: libraries are covered by cc_library
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
		// all cc_library rules in the dir: a source/header/define is
		// satisfied when ANY of them declares it (flag-signature variants
		// split one dir's compiles across several libraries)
		var libRules []*buildRule
		for i := range rules {
			if rules[i].kind == "cc_library" {
				libRules = append(libRules, &rules[i])
			}
		}
		if len(libRules) == 0 {
			findings = append(findings, finding{dirOrEmpty(dir), "no-build", "no cc_library for observed compiles"})
			continue
		}
		anyDeclares := func(get func(*buildRule) []string, want string) bool {
			for _, r := range libRules {
				if contains(get(r), want) {
					return true
				}
			}
			return false
		}
		for _, src := range m.srcs {
			if !anyDeclares(func(r *buildRule) []string { return r.srcs }, src) {
				findings = append(findings, finding{dirOrEmpty(dir), "missing-src", src})
			}
		}
		for _, hdr := range m.hdrs {
			if !anyDeclares(func(r *buildRule) []string { return r.hdrs }, hdr) {
				findings = append(findings, finding{dirOrEmpty(dir), "missing-hdr", hdr})
			}
		}
		for _, dep := range m.deps {
			found := false
			for _, r := range libRules {
				if hasDepLabel(r.deps, dep) {
					found = true
					break
				}
			}
			if !found {
				findings = append(findings, finding{dirOrEmpty(dir), "missing-dep", dep})
			}
		}
		for _, def := range m.defines {
			if !anyDeclares(func(r *buildRule) []string { return r.defines }, def) {
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
	// make's $(CURDIR) and os.Getwd can disagree through symlinks
	// (macOS: /tmp vs /private/tmp) — relativize against the resolved
	// physical path or every observed path stays absolute
	if resolved, err := filepath.EvalSymlinks(wd); err == nil {
		wd = resolved
	}
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
	if err := captureLinkSourceDepfiles(links, wd); err != nil {
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
		// generation followed by the adversarial re-check: parse what was
		// written and compare against the same depfile truth. Both sets
		// reported — a write error must not be masked by validation.
		findings = append(findings, generateCCBuilds(compiles, links, wd)...)
		findings = append(findings, validateCCBuilds(compiles, links, wd)...)
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
