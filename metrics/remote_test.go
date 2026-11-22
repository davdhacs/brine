package metrics

import (
	"testing"
	"time"
)

func TestTransferStatsFormat(t *testing.T) {
	ts := TransferStats{
		DownloadedBlobs: 45,
		DownloadedBytes: 330 * 1024 * 1024,
		UploadedBlobs:   120,
		UploadedBytes:   180 * 1024 * 1024,
		RemoteHits:      6700,
	}
	out := ts.Format()
	if out == "no remote transfers" {
		t.Fatal("expected transfer info")
	}
	// check the key parts are there
	checks := []string{"downloaded: 45", "uploaded: 120", "330.0 MB", "180.0 MB", "6700"}
	for _, c := range checks {
		if !contains(out, c) {
			t.Errorf("missing %q in %q", c, out)
		}
	}
}

func TestTransferStatsEmpty(t *testing.T) {
	ts := TransferStats{}
	if ts.Format() != "no remote transfers" {
		t.Fatalf("empty: %q", ts.Format())
	}
}

func TestSessionRoundTrip(t *testing.T) {
	dir := t.TempDir()
	s := &Session{
		ID:        "test-123",
		Runner:    "runner-abc",
		StartedAt: time.Now(),
		Runs: []Run{
			{Command: "make", Hit: true, WallMs: 50},
			{Command: "make", Hit: false, WallMs: 5000},
		},
		Transfer: TransferStats{
			DownloadedBlobs: 10,
			UploadedBlobs:   5,
		},
	}

	if err := SaveSessionJSONL(dir, s); err != nil {
		t.Fatal(err)
	}

	sessions, err := LoadSessions(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 1 {
		t.Fatalf("expected 1 session, got %d", len(sessions))
	}
	if sessions[0].ID != "test-123" || len(sessions[0].Runs) != 2 {
		t.Fatalf("round trip: %+v", sessions[0])
	}
	if sessions[0].Transfer.DownloadedBlobs != 10 {
		t.Fatal("transfer stats lost")
	}
}

func TestAggregate(t *testing.T) {
	sessions := []*Session{
		{
			Runs: []Run{
				{Hit: true, WallMs: 100},
				{Hit: false, WallMs: 5000},
			},
			Transfer: TransferStats{DownloadedBlobs: 10, UploadedBlobs: 5},
		},
		{
			Runs: []Run{
				{Hit: true, WallMs: 120},
			},
			Transfer: TransferStats{DownloadedBlobs: 3, UploadedBlobs: 8},
		},
	}

	s := Aggregate(sessions)
	if s.TotalRuns != 3 || s.TotalHits != 2 {
		t.Fatalf("aggregate: %+v", s)
	}
	if s.Transfer.DownloadedBlobs != 13 || s.Transfer.UploadedBlobs != 13 {
		t.Fatalf("transfer aggregate: %+v", s.Transfer)
	}
}

func TestFormatExtended(t *testing.T) {
	runs := []Run{
		{Hit: true, WallMs: 100},
		{Hit: false, WallMs: 5000},
	}
	s := Summarize(runs)
	ts := TransferStats{DownloadedBlobs: 5, UploadedBlobs: 2}
	out := FormatExtended(s, ts)
	if !contains(out, "50.0%") {
		t.Errorf("hit rate missing in %q", out)
	}
	if !contains(out, "downloaded: 5") {
		t.Errorf("transfer missing in %q", out)
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(s) > 0 && containsAt(s, substr))
}

func containsAt(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
