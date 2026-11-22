package gocache

import (
	"os"
	"testing"
)

// The manifest key scopes per build target (BRINE_GOCACHE_SCOPE):
// the CI serve-change leg once prefetched 17K actions for a 5K build
// under a module-identity-only key.
func TestManifestKeyScoping(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(dir+"/go.mod", []byte("module m\n\ngo 1.24\n"), 0o644)

	t.Setenv("BRINE_GOCACHE_SCOPE", "")
	base := manifestKey()

	t.Setenv("BRINE_GOCACHE_SCOPE", "./central")
	scoped := manifestKey()

	t.Setenv("BRINE_GOCACHE_SCOPE", "./sensor/kubernetes")
	scoped2 := manifestKey()

	if base == scoped || scoped == scoped2 {
		t.Fatalf("scoping must change the key: base=%s s1=%s s2=%s", base, scoped, scoped2)
	}

	// Same scope + same go.mod → stable.
	t.Setenv("BRINE_GOCACHE_SCOPE", "./central")
	if manifestKey() != scoped {
		t.Fatal("same scope must be stable")
	}

	// Different go.mod content → different key.
	os.WriteFile(dir+"/go.mod", []byte("module m2\n\ngo 1.24\n"), 0o644)
	oldWd, _ := os.Getwd()
	os.Chdir(dir)
	defer os.Chdir(oldWd)
	if manifestKey() == scoped {
		t.Fatal("different go.mod must change the key")
	}
}

func TestSaveSessionManifestMerge(t *testing.T) {
	// An empty session saves nothing (nil result).
	if m := saveMerge(nil, nil, nil); m != nil {
		t.Fatal("empty session must save nothing")
	}
	if m := saveMerge(&SessionManifest{Actions: []string{"a"}}, nil, nil); m != nil {
		t.Fatal("empty session must save nothing even with history")
	}
	old := &SessionManifest{Actions: []string{"a", "b"}}
	session := []string{"b", "c"}
	m := saveMerge(old, session, nil)
	if len(m.Actions) != 3 {
		t.Fatalf("union merge expected 3, got %v", m.Actions)
	}
}

func TestSaveSessionManifestBounded(t *testing.T) {
	old := &SessionManifest{}
	for i := 0; i < manifestMaxActions; i++ {
		old.Actions = append(old.Actions, "x")
	}
	m := saveMerge(old, []string{"new"}, nil)
	if len(m.Actions) > manifestMaxActions {
		t.Fatalf("merge must stay bounded, got %d", len(m.Actions))
	}
	if m.Actions[len(m.Actions)-1] != "new" {
		t.Fatal("most recent actions kept")
	}
}
