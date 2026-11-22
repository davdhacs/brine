// brine worker: an execution node for remote builds. Runs in a repo
// checkout and executes brine-wrapped commands on request, uploading
// receipts and output blobs to the shared blob store (BRINE_REMOTE=1).
// The client fetches outputs by receipt key — content-addressed, verified
// on restore — and never needs the sources compiled locally.
//
//	# machine A (worker, repo checked out):
//	BRINE_REMOTE=1 brine worker --port 8081
//
//	# machine B (client, same repo checked out):
//	BRINE_REMOTE=1 brine remote-build --worker http://a:8081 -- go build -o kwtest ./cmd/kwtest
//
// The worker's compute plus the shared cache means: cache hit → worker
// restores in milliseconds; cache miss → worker compiles (its gocache
// also hits the shared store) and the client receives the outputs without
// ever running the toolchain.
package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// executeRequest is the client's build request.
type executeRequest struct {
	Args   []string `json:"args"`   // command to run, e.g. ["go","build","-o","kwtest","./cmd/kwtest"]
	Commit string   `json:"commit"` // optional: checkout this commit first
}

// executeResponse reports the build result and receipt identity.
type executeResponse struct {
	OK       bool   `json:"ok"`
	Key      string `json:"key"` // receipt key — the client restores by this
	WallMs   int64  `json:"wall_ms"`
	Output   string `json:"output"`   // tail of the build log on failure
	Strategy string `json:"strategy"` // receipt strategy when cached
}

func cmdWorker(args []string) {
	port := 8081
	token := os.Getenv("BRINE_SERVE_TOKEN")
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--port":
			i++
			fmt.Sscanf(args[i], "%d", &port)
		case "--token":
			i++
			token = args[i]
		}
	}

	wd, _ := os.Getwd()
	brineBin, err := os.Executable()
	if err != nil {
		fatal("worker: %v", err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte("ok"))
	})
	mux.HandleFunc("/execute", func(w http.ResponseWriter, r *http.Request) {
		if token != "" && r.Header.Get("Authorization") != "Bearer "+token {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		var req executeRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || len(req.Args) == 0 {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		if req.Commit != "" {
			if out, err := exec.Command("git", "-C", wd, "-c", "safe.directory="+wd,
				"checkout", "--quiet", req.Commit).CombinedOutput(); err != nil {
				json.NewEncoder(w).Encode(executeResponse{OK: false, Output: string(out)})
				return
			}
		}

		// run the command under brine with the receipt key reported to a file
		keyFile := filepath.Join(os.TempDir(), fmt.Sprintf("brine-worker-key-%d", time.Now().UnixNano()))
		cmd := exec.Command(brineBin, req.Args...)
		cmd.Dir = wd
		cmd.Env = append(os.Environ(), "BRINE_KEY_OUT="+keyFile)
		start := time.Now()
		out, err := cmd.CombinedOutput()
		wall := time.Since(start).Milliseconds()
		key := ""
		if data, rerr := os.ReadFile(keyFile); rerr == nil {
			key = string(data)
		}
		os.Remove(keyFile)

		resp := executeResponse{OK: err == nil, Key: key, WallMs: wall}
		if err != nil {
			tail := string(out)
			if len(tail) > 2000 {
				tail = tail[len(tail)-2000:]
			}
			resp.Output = tail
		} else {
			// strategy from the log line for observability
			resp.Output = lastLineContaining(string(out), "strategy=")
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	})

	fmt.Printf("[brine] worker: listening on :%d (repo %s)\n", port, wd)
	fmt.Printf("[brine]   POST /execute {\"args\": [\"go\",\"build\",...]}\n")
	if err := http.ListenAndServe(fmt.Sprintf(":%d", port), mux); err != nil {
		fatal("worker: %v", err)
	}
}

func lastLineContaining(s, substr string) string {
	var found string
	for _, line := range splitLines(s) {
		if strings.Contains(line, substr) {
			found = line
		}
	}
	return found
}

func splitLines(s string) []string {
	var out []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	out = append(out, s[start:])
	return out
}
