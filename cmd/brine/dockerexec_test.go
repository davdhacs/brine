package main

import (
	"strings"
	"testing"
)

func TestDockerExecContainer(t *testing.T) {
	cases := map[string]struct {
		args  []string
		want  string
	}{
		"plain":            {[]string{"docker", "exec", "builder", "make"}, "builder"},
		"env flag":         {[]string{"docker", "exec", "-e", "FOO=bar", "builder", "make"}, "builder"},
		"env= form":        {[]string{"docker", "exec", "-e=FOO=bar", "builder", "make"}, "builder"},
		"workdir flag":     {[]string{"docker", "exec", "-w", "/src", "builder", "make"}, "builder"},
		"user flag":        {[]string{"docker", "exec", "-u", "root", "builder", "id"}, "builder"},
		"combined short":   {[]string{"docker", "exec", "-it", "builder", "sh"}, "builder"},
		"bool flag only":   {[]string{"docker", "exec", "--privileged", "builder", "sh"}, "builder"},
		"not docker":       {[]string{"make", "-j4"}, ""},
		"docker build":     {[]string{"docker", "build", "-t", "x", "."}, ""},
		"docker run":       {[]string{"docker", "run", "img"}, ""},
		"exec no container": {[]string{"docker", "exec"}, ""},
	}
	for name, tc := range cases {
		if got := dockerExecContainer(tc.args); got != tc.want {
			t.Errorf("%s: dockerExecContainer(%v) = %q, want %q", name, tc.args, got, tc.want)
		}
	}
}

func TestDockerExecKeyExtrasNonDocker(t *testing.T) {
	// Non-docker-exec commands must return "" without spawning docker.
	if got := dockerExecKeyExtras([]string{"make", "-j4"}); got != "" {
		t.Errorf("non-docker command returned %q, want \"\"", got)
	}
}

func TestNormalizeJSONList(t *testing.T) {
	a := `["PATH=/usr/bin","HOME=/root","LANG=C"]`
	b := `["LANG=C","HOME=/root","PATH=/usr/bin"]` // same elements, different order
	if normalizeJSONList(a) != normalizeJSONList(b) {
		t.Errorf("order-insensitivity failed: %q vs %q", normalizeJSONList(a), normalizeJSONList(b))
	}
	c := `["PATH=/usr/bin","HOME=/other"]`
	if normalizeJSONList(a) == normalizeJSONList(c) {
		t.Errorf("different env lists must normalize differently")
	}
	if normalizeJSONList("not-json") != "not-json" {
		t.Errorf("invalid JSON must pass through unchanged")
	}
}

func TestNormalizeMounts(t *testing.T) {
	// Same destination/type/RW but different host Source paths (two
	// workers, repo at different locations) must normalize IDENTICALLY:
	// receipts share across machines.
	a := `[{"Type":"bind","Source":"/home/alice/collector","Destination":"/tmp/collector","Mode":"","RW":true,"Propagation":"rprivate"}]`
	b := `[{"Type":"bind","Source":"/home/bob/repos/collector","Destination":"/tmp/collector","Mode":"","RW":true,"Propagation":"rslave"}]`
	if normalizeMounts(a) != normalizeMounts(b) {
		t.Errorf("host-path differences must not affect the mount digest:\n%s\n%s", normalizeMounts(a), normalizeMounts(b))
	}
	// Different destination = different build environment layout.
	c := `[{"Type":"bind","Source":"/home/alice/collector","Destination":"/src","Mode":"","RW":true}]`
	if normalizeMounts(a) == normalizeMounts(c) {
		t.Errorf("different destinations must normalize differently")
	}
	// Sorted output: multi-mount order must not matter.
	m1 := `[{"Destination":"/b","Type":"bind","RW":true},{"Destination":"/a","Type":"bind","RW":true}]`
	m2 := `[{"Destination":"/a","Type":"bind","RW":true},{"Destination":"/b","Type":"bind","RW":true}]`
	if normalizeMounts(m1) != normalizeMounts(m2) {
		t.Errorf("mount order must not affect the digest")
	}
	if strings.Contains(normalizeMounts(a), "/home/alice") {
		t.Errorf("host Source path must be dropped from the digest")
	}
}
