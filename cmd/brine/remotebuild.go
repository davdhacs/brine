// brine remote-build: dispatch a build to a brine worker. The client
// never runs the toolchain — the worker executes under brine (receipts
// + gocache hit the shared blob store) and reports the receipt key.
//
//	BRINE_REMOTE=1 brine remote-build --worker http://worker:8081 -- go build -o kwtest ./cmd/kwtest
//
// Flow:
//  1. POST the command to the worker.
//  2. Worker executes it under brine, reports the receipt key.
//  3. Client fetches the manifest by key (metadata only) and reports
//     what's available. Outputs stay in the shared store until
//     something needs them — `brine restore <key> [pattern]` fetches
//     selectively (verify-on-restore); `--restore` fetches everything
//     eagerly (the Bazel REAPI model: action result separated from
//     CAS blob fetching).
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/davdhacs/brine/gocache"
	"github.com/davdhacs/brine/store"
)

func cmdRemoteBuild(args []string) {
	workerURL := ""
	token := os.Getenv("BRINE_SERVE_TOKEN")
	restore := false
	var cmd []string
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--worker":
			i++
			if i < len(args) {
				workerURL = strings.TrimSuffix(args[i], "/")
			}
		case "--token":
			i++
			token = args[i]
		case "--restore":
			restore = true
		case "--":
			cmd = args[i+1:]
			i = len(args)
		default:
			// allow the command without "--" as well
			cmd = append(cmd, args[i])
		}
	}
	if workerURL == "" || len(cmd) == 0 {
		fatal("remote-build: usage: brine remote-build --worker URL [--token T] [--restore] -- <command...>")
	}

	wd, _ := os.Getwd()

	// 1. dispatch
	body, _ := json.Marshal(executeRequest{Args: cmd})
	req, _ := http.NewRequest("POST", workerURL+"/execute", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	fmt.Printf("[brine] remote-build: dispatching to %s: %s\n", workerURL, strings.Join(cmd, " "))
	start := time.Now()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		fatal("remote-build: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 2000))
		fatal("remote-build: worker HTTP %d: %s", resp.StatusCode, b)
	}
	var er executeResponse
	if err := json.NewDecoder(resp.Body).Decode(&er); err != nil {
		fatal("remote-build: %v", err)
	}
	if !er.OK {
		fatal("remote-build: worker build failed:\n%s", er.Output)
	}
	if er.Key == "" {
		fatal("remote-build: worker succeeded but reported no receipt key (command not memoizable?)")
	}
	fmt.Printf("[brine] remote-build: worker done in %dms (receipt %s)\n", er.WallMs, er.Key)

	// 2. fetch the manifest (metadata only — no output blobs)
	st, err := store.New(filepath.Join(wd, ".brine"))
	if err != nil {
		fatal("remote-build: %v", err)
	}
	if os.Getenv("BRINE_REMOTE") == "1" {
		os.Setenv("BRINE_GOCACHE_REMOTE", "1") // NewRemoteFromEnv requires this gate
		st.SetRemote(gocache.NewRemoteFromEnv())
	}
	m, err := st.GetManifest(er.Key)
	if err != nil || m == nil {
		fatal("remote-build: receipt %s not found in local or shared store (is BRINE_REMOTE=1 set on both sides?)", er.Key)
	}

	if !restore {
		// Report-only: outputs stay in the shared store; the consumer
		// fetches what it needs with `brine restore`.
		var total int64
		for _, b := range m.Blobs {
			total += b.Size
		}
		top := append([]store.BlobRef{}, m.Blobs...)
		sort.Slice(top, func(i, j int) bool { return top[i].Size > top[j].Size })
		n := 3
		if len(top) < n {
			n = len(top)
		}
		fmt.Printf("[brine] remote-build: ✓ completed (receipt %s)\n", er.Key)
		fmt.Printf("[brine]   %d outputs, %.1f MB in shared store\n", len(m.Blobs), float64(total)/1024/1024)
		for _, t := range top[:n] {
			fmt.Printf("[brine]   %s (%.1f MB)\n", t.Path, float64(t.Size)/1024/1024)
		}
		fmt.Printf("[brine]   fetch with: brine restore %s [pattern]\n", er.Key)
		fmt.Printf("[brine]   (%s total)\n", time.Since(start).Round(time.Millisecond))
		return
	}

	// 3. eager restore: download and place all outputs locally
	count, bytes_, err := st.RestoreManifest(m, wd)
	if err != nil {
		fatal("remote-build: restore failed: %v (integrity failures are visible by design)", err)
	}
	fmt.Printf("[brine] remote-build: ✓ %d outputs, %.1f MB restored and verified (%s total)\n",
		count, float64(bytes_)/1024/1024, time.Since(start).Round(time.Millisecond))
}

// cmdRestore fetches receipt outputs from the local/shared store.
// Without a pattern: everything. With a pattern: substring match on
// the manifest-relative path, or glob match if it contains * ? [.
//
//	brine restore 494f54a632ce6410                          # everything
//	brine restore 494f54a632ce6410 cmake-build/collector    # by substring
//	brine restore 494f54a632ce6410 "*.o"                    # by glob
func cmdRestore(args []string) {
	if len(args) < 1 || len(args) > 2 {
		fatal("restore: usage: brine restore <key> [pattern]")
	}
	keyID := args[0]
	pattern := ""
	if len(args) == 2 {
		pattern = args[1]
	}
	wd, _ := os.Getwd()
	st, err := store.New(filepath.Join(wd, ".brine"))
	if err != nil {
		fatal("restore: %v", err)
	}
	if os.Getenv("BRINE_REMOTE") == "1" {
		os.Setenv("BRINE_GOCACHE_REMOTE", "1") // NewRemoteFromEnv requires this gate
		st.SetRemote(gocache.NewRemoteFromEnv())
	}
	m, err := st.GetManifest(keyID)
	if err != nil || m == nil {
		fatal("restore: receipt %s not found (is BRINE_REMOTE=1 set?)", keyID)
	}

	var count int
	var bytes_ int64
	if pattern == "" {
		count, bytes_, err = st.RestoreManifest(m, wd)
	} else {
		count, bytes_, err = st.RestoreManifestMatching(m, wd, pattern)
	}
	if err != nil {
		fatal("restore: %v (integrity failures are visible by design)", err)
	}
	fmt.Printf("[brine] restore: ✓ %d outputs, %.1f MB verified (receipt %s)\n",
		count, float64(bytes_)/1024/1024, keyID)
}
