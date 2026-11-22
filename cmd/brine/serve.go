// Remote compiler-cache server: exposes brine's blob store as an HTTP
// cache that ccache (4.4+), sccache, and other tools can use as their
// remote_storage backend. The same Azure blob store that serves
// GOCACHEPROG and brine receipts now serves any HTTP cache client.
//
// Protocol: trivially simple — GET /<hash>, PUT /<hash>
//
//	ccache:  remote_storage = http://brine:8080
//	sccache: SCCACHE_AZURE_* (native) or via the HTTP proxy
//	any tool that can GET/PUT a URL
//
// Key formats, two classes:
//   - content-addressed keys (bare 64-hex sha256, or Bazel's cas/<sha256>):
//     the key pins the content, verified by construction on every GET and
//     enforced on PUT — key substitution is cryptographically impossible.
//   - opaque keys (ccache's sha1 xx/rest layout, sccache, Bazel ac/<hash>
//     action entries): stored with a sha256 envelope (32-byte hash prefix
//     prepended to the body), verified on every fetch. Corruption or
//     tampering self-heals as a miss.
//
// Value-adds over a raw blob store:
//   - integrity verification on every GET (both key classes)
//   - first-write-wins PUTs (no cache poisoning)
//   - metrics (hit/miss, bytes, uptime) via /stats
//   - access control (optional bearer token)
//
// Usage:
//
//	brine serve                        # start on :8080
//	brine serve --port 9090           # custom port
//	brine serve --token <secret>      # require auth
//
// Client configuration:
//
//	ccache: export CCACHE_REMOTE_STORAGE=http://brine:8080
//	(or in ccache.conf: remote_storage = http://brine:8080)
package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/davdhacs/brine/gocache"
)

// CacheServer is the HTTP front-end for the blob store.
type CacheServer struct {
	remote  *gocache.Remote
	port    int
	token   string
	verbose bool // BRINE_SERVE_LOG=1: log every request
	started time.Time

	// metrics
	gets     atomic.Int64
	hits     atomic.Int64
	misses   atomic.Int64
	puts     atomic.Int64
	bytesIn  atomic.Int64
	bytesOut atomic.Int64
}

// NewCacheServer creates the server.
func NewCacheServer(port int, token string) *CacheServer {
	remote := gocache.NewRemoteFromEnv()
	if remote == nil {
		fatal("serve: remote not configured (set BRINE_GOCACHE_REMOTE=1, BRINE_AZURE_ACCOUNT, and a token via BRINE_AZURE_TOKEN or AZURE_* SPN env)")
	}
	return &CacheServer{
		remote:  remote,
		port:    port,
		token:   token,
		verbose: os.Getenv("BRINE_SERVE_LOG") == "1",
		started: time.Now(),
	}
}

// Start runs the HTTP server until interrupted.
func (s *CacheServer) Start() error {
	logReq := func(name string, h http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if s.verbose {
				fmt.Printf("[brine] %s %s\n", r.Method, r.URL.Path)
			}
			h(w, r)
		}
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/", logReq("cache", s.handleCache))
	mux.HandleFunc("/stats", logReq("stats", s.handleStats))
	mux.HandleFunc("/health", logReq("health", s.handleHealth))

	addr := fmt.Sprintf(":%d", s.port)
	fmt.Printf("[brine] serve: listening on %s (remote cache for ccache/sccache/any HTTP client)\n", addr)
	fmt.Printf("[brine]   ccache: export CCACHE_REMOTE_STORAGE=http://localhost:%d\n", s.port)
	fmt.Printf("[brine]   stats:  curl http://localhost:%d/stats\n", s.port)

	server := &http.Server{
		Addr:           addr,
		Handler:        mux,
		ReadTimeout:    30 * time.Second,
		WriteTimeout:   120 * time.Second, // large blobs
		MaxHeaderBytes: 1 << 20,
	}

	// graceful shutdown
	go func() {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
		<-sig
		fmt.Printf("\n[brine] serve: shutting down\n")
		s.printStats()
		server.Close()
		os.Exit(0)
	}()

	return server.ListenAndServe()
}

// handleCache serves GET/<hash> and PUT/<hash>.
func (s *CacheServer) handleCache(w http.ResponseWriter, r *http.Request) {
	if s.token != "" {
		auth := r.Header.Get("Authorization")
		if auth != "Bearer "+s.token {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
	}

	hash := strings.Trim(r.URL.Path, "/")
	if hash == "" {
		http.Error(w, "missing hash", http.StatusBadRequest)
		return
	}

	switch r.Method {
	case http.MethodGet:
		s.handleGet(w, r, hash)
	case http.MethodPut:
		s.handlePut(w, r, hash)
	case http.MethodHead:
		s.handleHead(w, r, hash)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// isSHA256Hex reports whether s is a bare lowercase sha256 hex digest.
func isSHA256Hex(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, c := range s {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return false
		}
	}
	return true
}

// contentHash extracts the sha256 a content-addressed key pins, or "".
// Two shapes: a bare 64-hex digest, or Bazel's CAS layout "cas/<64-hex>"
// (REAPI: CAS keys are sha256 of the content by construction). Bazel's
// "ac/<hash>" action keys are NOT content-addressed (the hash is of the
// action, not the entry) and stay in envelope mode; ac entries only carry
// digests pointing into CAS, so even a poisoned ac entry self-heals when
// Bazel verifies the referenced CAS blobs.
func contentHash(key string) string {
	if isSHA256Hex(key) {
		return key
	}
	if strings.HasPrefix(key, "cas/") && isSHA256Hex(strings.TrimPrefix(key, "cas/")) {
		return strings.TrimPrefix(key, "cas/")
	}
	return ""
}

// isSHA256Key kept for compatibility with tests.
func isSHA256Key(key string) bool {
	return isSHA256Hex(key)
}

// errHashMismatch marks a PUT whose body does not hash to its content-addressed key.
var errHashMismatch = errors.New("body hash does not match key")

// envelopeBody wraps body with its sha256 for opaque-key storage.
func envelopeBody(body []byte) []byte {
	sum := sha256.Sum256(body)
	return append(sum[:], body...)
}

// verifyEnvelope splits and verifies an envelope; returns the body, or nil.
func verifyEnvelope(blob []byte) []byte {
	if len(blob) < 32 {
		return nil
	}
	sum := sha256.Sum256(blob[32:])
	if !bytes.Equal(sum[:], blob[:32]) {
		return nil
	}
	return blob[32:]
}

// handleGet fetches a blob from the remote store.
func (s *CacheServer) handleGet(w http.ResponseWriter, r *http.Request, key string) {
	s.gets.Add(1)

	blob, err := s.remote.Get(key)
	if err != nil || blob == nil {
		s.misses.Add(1)
		http.Error(w, "not found", http.StatusNotFound)
		return
	}

	var body []byte
	if want := contentHash(key); want != "" {
		// Content-addressed key: the key pins the content by construction.
		sum := sha256.Sum256(blob)
		if hex.EncodeToString(sum[:]) == want {
			body = blob
		} else if legacy := verifyEnvelope(blob); legacy != nil {
			// Entry stored before direct CAS verification — envelope still
			// guards storage integrity.
			body = legacy
		} else {
			s.misses.Add(1)
			fmt.Fprintf(os.Stderr, "[brine] INTEGRITY: GET %s: content hash mismatch\n", key[:12])
			http.Error(w, "not found", http.StatusNotFound) // self-heal: client recompiles
			return
		}
	} else {
		// Opaque key: sha256 envelope, verified on every fetch.
		body = verifyEnvelope(blob)
		if body == nil {
			s.misses.Add(1)
			fmt.Fprintf(os.Stderr, "[brine] INTEGRITY: GET %s: envelope verification failed\n", key[:12])
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
	}

	s.hits.Add(1)
	s.bytesOut.Add(int64(len(body)))
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", fmt.Sprintf("%d", len(body)))
	w.Write(body)
}

// handlePut uploads a blob to the remote store (first-write-wins).
func (s *CacheServer) handlePut(w http.ResponseWriter, r *http.Request, key string) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 512<<20)) // 512MB cap
	if err != nil {
		http.Error(w, "read error", http.StatusBadRequest)
		return
	}

	store := func(blob []byte) error {
		if want := contentHash(key); want != "" {
			// Content-addressed key: the key must pin the content.
			sum := sha256.Sum256(blob)
			if hex.EncodeToString(sum[:]) != want {
				fmt.Fprintf(os.Stderr, "[brine] INTEGRITY: PUT %s: content hash mismatch — rejected\n", key[:12])
				return errHashMismatch
			}
			_, err := s.remote.PutIfNotExistPub(key, blob)
			return err
		}
		// Opaque key: store with sha256 envelope for fetch-time verification.
		_, err := s.remote.PutIfNotExistPub(key, envelopeBody(blob))
		return err
	}
	if err := store(body); err != nil {
		if err == errHashMismatch {
			http.Error(w, "hash mismatch", http.StatusBadRequest)
			return
		}
		fmt.Fprintf(os.Stderr, "[brine] STORE ERROR: PUT %s: %v\n", key[:12], err)
		http.Error(w, "storage error", http.StatusInternalServerError)
		return
	}

	s.puts.Add(1)
	s.bytesIn.Add(int64(len(body)))
	w.WriteHeader(http.StatusCreated)
}

// handleHead checks existence without fetching the body.
func (s *CacheServer) handleHead(w http.ResponseWriter, r *http.Request, key string) {
	s.gets.Add(1)
	body, err := s.remote.Get(key)
	if err != nil || body == nil {
		s.misses.Add(1)
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	s.hits.Add(1)
	s.bytesOut.Add(int64(len(body))) // HEAD fetched the body (no cheap exists check)
	w.Header().Set("Content-Length", fmt.Sprintf("%d", len(body)))
	w.WriteHeader(http.StatusOK)
}

// handleStats reports server metrics as JSON.
func (s *CacheServer) handleStats(w http.ResponseWriter, r *http.Request) {
	stats := map[string]interface{}{
		"uptime_s":  int64(time.Since(s.started).Seconds()),
		"gets":      s.gets.Load(),
		"hits":      s.hits.Load(),
		"misses":    s.misses.Load(),
		"puts":      s.puts.Load(),
		"bytes_in":  s.bytesIn.Load(),
		"bytes_out": s.bytesOut.Load(),
		"hit_rate":  s.hitRate(),
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(stats)
}

// hitRate returns the GET hit rate as a percentage string, or "n/a" before any requests.
func (s *CacheServer) hitRate() string {
	gets := s.gets.Load()
	if gets == 0 {
		return "n/a"
	}
	return fmt.Sprintf("%.1f%%", float64(s.hits.Load())/float64(gets)*100)
}

// handleHealth is a liveness probe.
func (s *CacheServer) handleHealth(w http.ResponseWriter, r *http.Request) {
	w.Write([]byte("ok"))
}

func (s *CacheServer) printStats() {
	fmt.Printf("[brine] serve: gets=%d hits=%d misses=%d puts=%d bytes_in=%d bytes_out=%d\n",
		s.gets.Load(), s.hits.Load(), s.misses.Load(), s.puts.Load(),
		s.bytesIn.Load(), s.bytesOut.Load())
}

// cmdServe starts the cache server.
func cmdServe(args []string) {
	port := 8080
	token := os.Getenv("BRINE_SERVE_TOKEN")

	for i := 0; i < len(args); i++ {
		if args[i] == "--port" && i+1 < len(args) {
			fmt.Sscanf(args[i+1], "%d", &port)
			i++
		} else if args[i] == "--token" && i+1 < len(args) {
			token = args[i+1]
			i++
		}
	}

	server := NewCacheServer(port, token)
	if err := server.Start(); err != nil {
		fatal("serve: %v", err)
	}
}
