package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const sampleBuild = `load("@io_bazel_rules_go//go:def.bzl", "go_library", "go_test")

go_library(
    name = "k8swatch",
    srcs = [
        "adapter.go",
        "client.go",
        "watcher.go",
    ],
    importpath = "github.com/stackrox/k8swatch",
    visibility = ["//visibility:public"],
    deps = [
        "//blobstore",
        "//generated/api/v1:api",
    ],
)

go_test(
    name = "k8swatch_test",
    srcs = ["adapter_test.go"],
    embed = [":k8swatch"],
)

go_binary(
    name = "kwtest",
    embed = [":kwtest_lib"],
    srcs = ["main.go"],
)
`

func writeTempBuild(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "BUILD.bazel")
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestParseBuildFile(t *testing.T) {
	rules, err := parseBuildFile(writeTempBuild(t, sampleBuild))
	if err != nil {
		t.Fatal(err)
	}
	if len(rules) != 3 {
		t.Fatalf("got %d rules, want 3", len(rules))
	}

	lib := rules[0]
	if lib.kind != "go_library" || lib.name != "k8swatch" {
		t.Errorf("rule 0: %s/%s", lib.kind, lib.name)
	}
	if len(lib.srcs) != 3 || lib.srcs[0] != "adapter.go" || lib.srcs[2] != "watcher.go" {
		t.Errorf("lib srcs: %v", lib.srcs)
	}
	if lib.importpath != "github.com/stackrox/k8swatch" {
		t.Errorf("importpath: %q", lib.importpath)
	}
	if len(lib.deps) != 2 || lib.deps[0] != "//blobstore" || lib.deps[1] != "//generated/api/v1:api" {
		t.Errorf("lib deps: %v", lib.deps)
	}

	test := rules[1]
	if test.kind != "go_test" || len(test.srcs) != 1 || test.srcs[0] != "adapter_test.go" {
		t.Errorf("test rule: %+v", test)
	}
	if len(test.embed) != 1 || test.embed[0] != ":k8swatch" {
		t.Errorf("test embed: %v", test.embed)
	}

	bin := rules[2]
	if len(bin.srcs) != 1 || bin.srcs[0] != "main.go" {
		t.Errorf("binary srcs: %v", bin.srcs) // inline list with trailing comma
	}
}

func TestHasDepLabel(t *testing.T) {
	cases := []struct {
		deps  []string
		label string
		want  bool
	}{
		{[]string{"//blobstore"}, "//blobstore", true},
		{[]string{"//blobstore:blobstore"}, "//blobstore", true},
		{[]string{"//generated/api/v1:api"}, "//generated/api/v1", true},
		// gazelle names targets after the package clause, not the dir basename
		{[]string{"//central/reports/scheduler/v2:scheduler"}, "//central/reports/scheduler/v2", true},
		// deeper path is a different package
		{[]string{"//central/reports/service/v2/node"}, "//central/reports/service/v2", false},
		{[]string{"//:k8swatch"}, "//:k8swatch", true},
		{[]string{}, "//blobstore", false},
	}
	for _, c := range cases {
		if got := hasDepLabel(c.deps, c.label); got != c.want {
			t.Errorf("hasDepLabel(%v, %q) = %v, want %v", c.deps, c.label, got, c.want)
		}
	}
}

func TestImportToLabel(t *testing.T) {
	if got := importToLabel("github.com/stackrox/k8swatch", "github.com/stackrox/k8swatch"); got != "//:k8swatch" {
		t.Errorf("root: %q", got)
	}
	if got := importToLabel("github.com/stackrox/k8swatch/blobstore", "github.com/stackrox/k8swatch"); got != "//blobstore" {
		t.Errorf("sub: %q", got)
	}
}

const sampleWithTags = `load("@io_bazel_rules_go//go:def.bzl", "go_library")

go_library(
    name = "lib",
    tags = ["manual"],
    srcs = ["a.go"],
)

go_test(
    name = "lib_test",
    tags = [
        "manual",
        "integration",
    ],
    srcs = ["a_test.go"],
)
`

func TestTagAllRulesManual(t *testing.T) {
	p := writeTempBuild(t, `go_library(
    name = "lib",
    srcs = ["a.go"],
)
`)
	if !tagAllRulesManual(p) {
		t.Fatal("expected modification")
	}
	if tagAllRulesManual(p) {
		t.Fatal("expected idempotent no-op on second run")
	}
	rules, _ := parseBuildFile(p)
	if len(rules) != 1 {
		t.Fatalf("parse after tag: %d rules", len(rules))
	}
}

func TestUntagManual(t *testing.T) {
	p := writeTempBuild(t, sampleWithTags)
	if !untagManual(p) {
		t.Fatal("expected modification")
	}
	data, _ := os.ReadFile(p)
	s := string(data)
	if strings.Contains(s, "manual") {
		t.Errorf("manual still present:\n%s", s)
	}
	if !strings.Contains(s, `"integration"`) {
		t.Errorf("other tags must be preserved:\n%s", s)
	}
	// go_library lost its tags line entirely; go_test keeps ["integration"]
	if strings.Contains(s, "tags = [") && !strings.Contains(s, "integration") {
		t.Errorf("empty tags list should be dropped:\n%s", s)
	}
}

func TestUntagManualNoTags(t *testing.T) {
	p := writeTempBuild(t, sampleBuild)
	if untagManual(p) {
		t.Fatal("no manual tags: expected no-op")
	}
}

func TestAddToListAttrMultiLine(t *testing.T) {
	p := writeTempBuild(t, sampleBuild)
	if !addToListAttr(p, "go_library", "srcs", "zzz.go") {
		t.Fatal("expected insert")
	}
	rules, _ := parseBuildFile(p)
	if !contains(rules[0].srcs, "zzz.go") {
		t.Errorf("zzz.go not in srcs: %v", rules[0].srcs)
	}
	// idempotent
	if addToListAttr(p, "go_library", "srcs", "zzz.go") {
		t.Error("expected no-op on duplicate")
	}
}

func TestAddToListAttrAbsent(t *testing.T) {
	// rule with no deps attribute: fix must create it
	p := writeTempBuild(t, `go_library(
    name = "lib",
    srcs = ["a.go"],
)
`)
	if !addToListAttr(p, "go_library", "deps", "//blobstore") {
		t.Fatal("expected attribute creation")
	}
	rules, _ := parseBuildFile(p)
	if !contains(rules[0].deps, "//blobstore") {
		t.Errorf("deps not created: %v", rules[0].deps)
	}
}
