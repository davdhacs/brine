// Remote metrics: per-run and aggregated stats uploaded to the blob
// store for CI aggregation. The `brine stats` command shows local
// stats; `brine stats --remote` pulls and merges remote stats.
//
// Upload format: one JSON blob per session (keyed by timestamp),
// containing the session's runs. CI can download all sessions for
// cross-run analysis.
//
// The user asked specifically for:
//
//	"X/X loaded from remote and Y uploaded" — the transfer summary
//	"metrics in logs in addition to uploaded metrics" — both local
//	JSONL and remote blob
package metrics

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// TransferStats tracks blob-store transfer activity for one run.
type TransferStats struct {
	DownloadedBlobs int64 `json:"downloaded_blobs"`
	DownloadedBytes int64 `json:"downloaded_bytes"`
	UploadedBlobs   int64 `json:"uploaded_blobs"`
	UploadedBytes   int64 `json:"uploaded_bytes"`
	UploadFailed    int64 `json:"upload_failed"`
	DownloadMs      int64 `json:"download_ms"`
	UploadMs        int64 `json:"upload_ms"`
	RemoteHits      int64 `json:"remote_hits"` // GOCACHEPROG remote hits
	RemotePuts      int64 `json:"remote_puts"` // GOCACHEPROG remote puts
}

// Session is a collection of runs with a common identity (one CI job,
// one local dev session). Uploaded as a single blob.
type Session struct {
	ID        string        `json:"id"`     // timestamp-based
	Runner    string        `json:"runner"` // hostname or CI runner ID
	StartedAt time.Time     `json:"started_at"`
	Runs      []Run         `json:"runs"`
	Transfer  TransferStats `json:"transfer"`
}

// FormatTransfer renders the "X/X loaded, Y uploaded" summary.
func (t *TransferStats) Format() string {
	if t.DownloadedBlobs == 0 && t.UploadedBlobs == 0 {
		return "no remote transfers"
	}
	parts := []string{}
	if t.DownloadedBlobs > 0 {
		parts = append(parts, fmt.Sprintf("downloaded: %d blobs (%.1f MB)",
			t.DownloadedBlobs, float64(t.DownloadedBytes)/1024/1024))
	}
	if t.UploadedBlobs > 0 {
		parts = append(parts, fmt.Sprintf("uploaded: %d blobs (%.1f MB)",
			t.UploadedBlobs, float64(t.UploadedBytes)/1024/1024))
	}
	if t.UploadFailed > 0 {
		parts = append(parts, fmt.Sprintf("upload failures: %d", t.UploadFailed))
	}
	if t.RemoteHits > 0 {
		parts = append(parts, fmt.Sprintf("gocache remote hits: %d", t.RemoteHits))
	}
	return strings.Join(parts, ", ")
}

// RemoteUploader pushes session metrics to the blob store.
type RemoteUploader interface {
	PutIfNotExistPub(key string, body []byte) (bool, error)
	Get(key string) ([]byte, error)
}

// UploadSession pushes a session's metrics to the blob store.
// Key: "metrics/<runner>/<session-id>"
func UploadSession(uploader RemoteUploader, s *Session) error {
	if uploader == nil {
		return nil
	}
	data, err := json.Marshal(s)
	if err != nil {
		return err
	}
	key := "metrics/" + s.Runner + "/" + s.ID
	_, err = uploader.PutIfNotExistPub(key, data)
	if err != nil {
		return fmt.Errorf("metrics upload: %w", err)
	}
	fmt.Printf("[brine] metrics: session uploaded to blob store (%s)\n", key)
	return nil
}

// ListRemoteSessions lists available session IDs from the blob store.
// Requires listing capability (not all blob stores support this).
func ListRemoteSessions(uploader RemoteUploader, runner string) []string {
	if uploader == nil {
		return nil
	}
	// This is a best-effort listing — some blob stores don't support
	// prefix listing through the simple Get interface.
	// For now, return empty (sessions are discovered by convention).
	return nil
}

// Aggregate combines multiple sessions into a single summary.
func Aggregate(sessions []*Session) *Summary {
	var allRuns []Run
	var totalTransfer TransferStats
	for _, s := range sessions {
		allRuns = append(allRuns, s.Runs...)
		totalTransfer.DownloadedBlobs += s.Transfer.DownloadedBlobs
		totalTransfer.DownloadedBytes += s.Transfer.DownloadedBytes
		totalTransfer.UploadedBlobs += s.Transfer.UploadedBlobs
		totalTransfer.UploadedBytes += s.Transfer.UploadedBytes
		totalTransfer.UploadFailed += s.Transfer.UploadFailed
	}
	s := Summarize(allRuns)
	s.Transfer = totalTransfer
	return s
}

// ExtendedSummary includes transfer stats.
type ExtendedSummary struct {
	Summary
	Transfer TransferStats `json:"transfer"`
}

// FormatExtended renders the summary with transfer info.
func FormatExtended(s *Summary, t TransferStats) string {
	out := s.Format()
	if t.DownloadedBlobs > 0 || t.UploadedBlobs > 0 {
		out += fmt.Sprintf("\nremote: %s\n", t.Format())
	}
	return out
}

// SaveSessionJSONL appends a session to a local JSONL file for
// aggregation across sessions.
func SaveSessionJSONL(brineDir string, s *Session) error {
	path := filepath.Join(brineDir, "sessions.jsonl")
	data, err := json.Marshal(s)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return nil // never break the build for metrics
	}
	defer f.Close()
	_, err = f.Write(append(data, '\n'))
	return err
}

// LoadSessions reads all sessions from the local JSONL file.
func LoadSessions(brineDir string) ([]*Session, error) {
	path := filepath.Join(brineDir, "sessions.jsonl")
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var sessions []*Session
	for _, line := range splitLines(string(data)) {
		if line == "" {
			continue
		}
		var s Session
		if err := json.Unmarshal([]byte(line), &s); err == nil {
			sessions = append(sessions, &s)
		}
	}
	sort.Slice(sessions, func(i, j int) bool {
		return sessions[i].StartedAt.Before(sessions[j].StartedAt)
	})
	return sessions, nil
}
