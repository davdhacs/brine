package main

import (
	"strings"
	"testing"
)

func TestSplitCommand(t *testing.T) {
	cases := map[string][]string{
		`cc -c -o foo.o foo.c`:          {"cc", "-c", "-o", "foo.o", "foo.c"},
		`cc -DNAME="a b" -c x.c -o x.o`: {"cc", "-DNAME=a b", "-c", "x.c", "-o", "x.o"},
		`ar rcs libfoo.a a.o b.o`:       {"ar", "rcs", "libfoo.a", "a.o", "b.o"},
		`single`:                        {"single"},
	}
	for line, want := range cases {
		got := splitCommand(line)
		if len(got) != len(want) {
			t.Errorf("splitCommand(%q) = %v, want %v", line, got, want)
			continue
		}
		for i := range got {
			if got[i] != want[i] {
				t.Errorf("splitCommand(%q)[%d] = %q, want %q", line, i, got[i], want[i])
			}
		}
	}
}

func TestClassifyCompile(t *testing.T) {
	c := classifyCompile(splitCommand("cc -Wall -DFEATURE_X=1 -DCONFIG_MULT=7 -Ilib -MD -c main.c -o main.o"))
	if c == nil {
		t.Fatal("compile not classified")
	}
	if c.Src != "main.c" || c.Obj != "main.o" {
		t.Errorf("src/obj: %q/%q", c.Src, c.Obj)
	}
	if len(c.Defines) != 2 || c.Defines[0] != "FEATURE_X=1" {
		t.Errorf("defines: %v", c.Defines)
	}
	if len(c.IncDirs) != 1 || c.IncDirs[0] != "lib" {
		t.Errorf("includes: %v", c.IncDirs)
	}

	// a link is not a compile
	if classifyCompile(splitCommand("cc -o app main.o lib/util.o")) != nil {
		t.Error("link misclassified as compile")
	}
	// non-compiler first token
	if classifyCompile(splitCommand("echo hello world -c x.c -o x.o")) != nil {
		t.Error("non-compiler classified")
	}
}

func TestClassifyLink(t *testing.T) {
	l := classifyLink(splitCommand("cc -o app main.o lib/util.o lib/extra.o"))
	if l == nil || l.IsAr {
		t.Fatal("link not classified")
	}
	if l.Out != "app" || len(l.Objects) != 3 {
		t.Errorf("out/objects: %q/%v", l.Out, l.Objects)
	}

	a := classifyLink(splitCommand("ar rcs libutil.a lib/util.o lib/extra.o"))
	if a == nil || !a.IsAr {
		t.Fatal("archive not classified")
	}
	if a.Out != "libutil.a" || len(a.Objects) != 2 {
		t.Errorf("archive out/objects: %q/%v", a.Out, a.Objects)
	}

	// a compile is not a link
	if classifyLink(splitCommand("cc -c main.c -o main.o")) != nil {
		t.Error("compile misclassified as link")
	}
}

func TestCcLibNameAndLabel(t *testing.T) {
	if ccLibName("") != "lib" {
		t.Errorf("root lib name: %q", ccLibName(""))
	}
	if ccLibName("lib") != "lib_lib" {
		t.Errorf("lib dir name: %q", ccLibName("lib"))
	}
	if ccLibName("src/my-dir") != "my_dir_lib" {
		t.Errorf("hyphen dir name: %q", ccLibName("src/my-dir"))
	}
	if ccLabel("", "lib") != "//:lib" {
		t.Errorf("root label: %q", ccLabel("", "lib"))
	}
	// labels must name the target explicitly: //lib resolves to //lib:lib,
	// not the generated lib_lib
	if ccLabel("lib", "lib_lib") != "//lib:lib_lib" {
		t.Errorf("dir label: %q", ccLabel("lib", "lib_lib"))
	}
}

func TestCCGeneratedBuildParses(t *testing.T) {
	// the generated form must round-trip through the shared parser
	build := `load("@rules_cc//cc:defs.bzl", "cc_library")

cc_library(
    name = "lib_lib",
    srcs = [
        "util.c",
    ],
    hdrs = [
        "util.h",
    ],
    defines = [
        "CONFIG_MULT=7",
    ],
    deps = [
        "//other:other_lib",
    ],
)
`
	p := writeTempBuild(t, build)
	rules, err := parseBuildFile(p)
	if err != nil || len(rules) != 1 {
		t.Fatalf("parse: %v, %d rules", err, len(rules))
	}
	r := rules[0]
	if r.kind != "cc_library" || r.name != "lib_lib" {
		t.Errorf("kind/name: %s/%s", r.kind, r.name)
	}
	if len(r.srcs) != 1 || r.srcs[0] != "util.c" {
		t.Errorf("srcs: %v", r.srcs)
	}
	if len(r.hdrs) != 1 || r.hdrs[0] != "util.h" {
		t.Errorf("hdrs: %v", r.hdrs)
	}
	if len(r.defines) != 1 || r.defines[0] != "CONFIG_MULT=7" {
		t.Errorf("defines: %v", r.defines)
	}
	if len(r.deps) != 1 || r.deps[0] != "//other:other_lib" {
		t.Errorf("deps: %v", r.deps)
	}
	if strings.Contains(build, "unused") {
		t.Error("placeholder")
	}
}
