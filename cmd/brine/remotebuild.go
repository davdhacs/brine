// brine remote-build: dispatch a build to a brine worker and restore the
// outputs locally from the shared blob store. The client never runs the
// toolchain — it verifies and places content-addressed outputs.
//
//	BRINE_REMOTE=1 brine remote-build --worker http://worker:8081 -- go build -o kwtest ./cmd/kwtest
//
// Flow:
//  1. POST the command to the worker.
//  2. Worker executes it under brine (receipts + gocache hit the shared
//     blob store), reports the receipt key.
//  3. Client fetches the manifest by key (remote store), restores output
//     blobs with verify-on-restore, places them at the manifest's
//     relative paths.
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/davdhacs/brine/gocache"
	"github.com/davdhacs/brine/store"
)

func cmdRemoteBuild(args []string) {
	workerURL := ""
	token := os.Getenv("BRINE_SERVE_TOKEN")
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
		case "--":
			cmd = args[i+1:]
			i = len(args)
		default:
			// allow the command without "--" as well
			cmd = append(cmd, args[i])
		}
	}
	if workerURL == "" || len(cmd) == 0 {
		fatal("remote-build: usage: brine remote-build --worker URL [--token T] -- <command...>")
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

	// 2. restore locally from the shared store
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
	count, bytes_, err := st.RestoreManifest(m, wd)
	if err != nil {
		fatal("remote-build: restore failed: %v (integrity failures are visible by design)", err)
	}
	fmt.Printf("[brine] remote-build: ✓ %d outputs, %.1f MB restored and verified (%s total)\n",
		count, float64(bytes_)/1024/1024, time.Since(start).Round(time.Millisecond))
}
