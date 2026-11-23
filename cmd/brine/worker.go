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
//
// Endpoints:
//
//	GET  /health    liveness
//	POST /execute   {"args": [...], "commit": "..."} → build result
//	POST /restart   re-exec the worker with the same env (self-restart
//	                without pkill pattern games — a pkill whose pattern
//	                matches the restarting shell's own cmdline kills the
//	                restarter instead of the target; found the hard way)
package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/davdhacs/brine/gocache"
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
	Output   string `json:"output"`   // build log tail on failure; warnings on success
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

	// restart child: wait for the old process to release the port
	if os.Getenv("BRINE_WORKER_RESTART_SLEEP") != "" {
		time.Sleep(time.Second)
	}

	wd, _ := os.Getwd()
	brineBin, err := os.Executable()
	if err != nil {
		fatal("worker: %v", err)
	}

	// Auth sanity at startup: a worker with broken Azure credentials
	// executes builds fine but every receipt upload fails silently into
	// the log — the worst failure mode (found the hard way: an invalid
	// SPN secret produced a fully "working" worker that never shared).
	if os.Getenv("BRINE_REMOTE") == "1" {
		if r := gocache.NewRemoteFromEnv(); r != nil {
			if err := r.ValidateAuth(); err != nil {
				fmt.Fprintf(os.Stderr, "[brine] worker: ⚠ Azure auth FAILED — receipts will NOT upload to the shared store: %v\n", err)
				fmt.Fprintf(os.Stderr, "[brine] worker:   check BRINE_AZURE_TOKEN / AZURE_TENANT_ID+CLIENT_ID+CLIENT_SECRET; builds still execute locally\n")
			} else {
				fmt.Printf("[brine] worker: Azure auth OK (shared store reachable)\n")
			}
		} else {
			fmt.Fprintf(os.Stderr, "[brine] worker: ⚠ BRINE_REMOTE=1 but no remote configured (BRINE_AZURE_ACCOUNT missing?) — receipts stay local\n")
		}
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte("ok"))
	})
	mux.HandleFunc("/restart", func(w http.ResponseWriter, _ *http.Request) {
		// Re-exec the worker as a detached child with the same env and
		// arguments, then exit. The child sleeps to let the port free.
		w.Write([]byte("restarting\n"))
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		cmd := exec.Command(brineBin, "worker", "--port", fmt.Sprint(port))
		if token != "" {
			cmd.Args = append(cmd.Args, "--token", token)
		}
		cmd.Dir = wd
		cmd.Env = append(os.Environ(), "BRINE_WORKER_RESTART_SLEEP=1")
		cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true} // detach: survives our exit
		cmd.Stdout, _ = os.OpenFile(os.DevNull, os.O_WRONLY, 0)
		cmd.Stderr = os.Stderr
		if err := cmd.Start(); err == nil {
			go func() {
				// reap the child so it does not linger as a zombie
				cmd.Wait()
			}()
		}
		os.Exit(0)
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
			// Surface the receipt-upload health even on success — a
			// "working" build whose receipt never reached the shared store
			// is a failed remote build from the client's perspective.
			resp.Output = lastLineContaining(string(out), "strategy=")
			if up := lastLineContaining(string(out), "upload failed"); up != "" {
				resp.Output = up
			}
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	})

	fmt.Printf("[brine] worker: listening on :%d (repo %s)\n", port, wd)
	fmt.Printf("[brine]   POST /execute {\"args\": [\"go\",\"build\",...]}\n")
	fmt.Printf("[brine]   POST /restart (self-restart, same env)\n")
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
