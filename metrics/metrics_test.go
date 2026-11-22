package metrics

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLoggerRoundTrip(t *testing.T) {
	dir := t.TempDir()
	l := NewLogger(dir)

	r1 := &Run{
		Command:       "make",
		Hit:           false,
		WallMs:        5000,
		Strategy:      "fallback",
		GocacheGets:   100,
		GocacheHits:   80,
		GocacheMisses: 20,
	}
	if err := l.Log(r1); err != nil {
		t.Fatal(err)
	}

	r2 := &Run{
		Command:  "make",
		Hit:      true,
		WallMs:   50,
		Strategy: "observed",
	}
	if err := l.Log(r2); err != nil {
		t.Fatal(err)
	}

	runs, err := LoadAll(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 2 {
		t.Fatalf("expected 2 runs, got %d", len(runs))
	}
	if runs[0].Command != "make" || !runs[1].Hit {
		t.Fatalf("round trip: %+v", runs)
	}
}

func TestSummarize(t *testing.T) {
	runs := []Run{
		{Command: "make", Hit: true, WallMs: 100, Strategy: "observed"},
		{Command: "make", Hit: true, WallMs: 120, Strategy: "observed"},
		{Command: "make", Hit: false, WallMs: 5000, Strategy: "fallback"},
		{Command: "go build", Hit: true, WallMs: 200, Strategy: "go-list"},
	}

	s := Summarize(runs)
	if s.TotalRuns != 4 {
		t.Fatalf("total: %d", s.TotalRuns)
	}
	if s.TotalHits != 3 || s.TotalMisses != 1 {
		t.Fatalf("hits: %d misses: %d", s.TotalHits, s.TotalMisses)
	}
	if s.HitRate != 75.0 {
		t.Fatalf("hit rate: %v", s.HitRate)
	}
	if s.AvgHitMs != 140 { // (100+120+200)/3
		t.Fatalf("avg hit: %d", s.AvgHitMs)
	}
	if s.AvgMissMs != 5000 {
		t.Fatalf("avg miss: %d", s.AvgMissMs)
	}
}

func TestSummarizeEmpty(t *testing.T) {
	s := Summarize(nil)
	if s.TotalRuns != 0 || s.HitRate != 0 {
		t.Fatal("empty should be zero")
	}
	if s.Format() != "no runs recorded" {
		t.Fatalf("format: %q", s.Format())
	}
}

func TestLoggerNilSafe(t *testing.T) {
	var l *Logger
	if err := l.Log(&Run{Command: "x"}); err != nil {
		t.Fatal("nil logger should not error")
	}
}

func TestPhaseTiming(t *testing.T) {
	dir := t.TempDir()
	l := NewLogger(dir)

	r := &Run{
		Command:     "go build",
		WallMs:      30000,
		ClosureMs:   500,
		KeyMs:       200,
		BuildMs:     28000,
		RestoreMs:   0,
		SnapshotMs:  1000,
		InputCount:  4590,
		OutputCount: 1,
		OutputBytes: 327000000,
	}
	l.Log(r)

	runs, _ := LoadAll(dir)
	if len(runs) != 1 {
		t.Fatal("expected 1 run")
	}
	if runs[0].ClosureMs != 500 || runs[0].BuildMs != 28000 {
		t.Fatalf("phases: %+v", runs[0])
	}
}

func TestSideCacheMetrics(t *testing.T) {
	dir := t.TempDir()
	l := NewLogger(dir)

	r := &Run{
		Command:           "npm run build",
		SideCacheRestored: []string{"node_modules"},
		SideCacheSaved:    []string{"node_modules"},
	}
	l.Log(r)

	runs, _ := LoadAll(dir)
	if len(runs) != 1 || len(runs[0].SideCacheRestored) != 1 {
		t.Fatal("side cache metrics lost")
	}
}

func TestMetricsFileLocation(t *testing.T) {
	dir := t.TempDir()
	brineDir := filepath.Join(dir, ".brine")
	os.MkdirAll(brineDir, 0o755)

	l := NewLogger(brineDir)
	l.Log(&Run{Command: "test"})

	if _, err := os.Stat(filepath.Join(brineDir, "metrics.jsonl")); err != nil {
		t.Fatal("metrics.jsonl not created")
	}
}

func TestTimeField(t *testing.T) {
	dir := t.TempDir()
	l := NewLogger(dir)
	before := time.Now()
	l.Log(&Run{Command: "test"})
	runs, _ := LoadAll(dir)
	if runs[0].Timestamp.Before(before) {
		t.Fatal("timestamp not set")
	}
}
