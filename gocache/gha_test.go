package gocache

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// fakeTwirpServer implements the cache v2 Results API protocol plus
// blob storage: CreateCacheEntry / FinalizeCacheEntryUpload /
// GetCacheEntryDownloadURL + direct blob PUT/GET on the returned URLs.
type fakeTwirpServer struct {
	mu      sync.Mutex
	blobs   map[string][]byte // key → body (as stored, may be gzipped)
	finaled map[string]bool
	srv     *httptest.Server
}

func newFakeTwirpServer(t *testing.T) *fakeTwirpServer {
	f := &fakeTwirpServer{blobs: map[string][]byte{}, finaled: map[string]bool{}}
	mux := http.NewServeMux()
	twirp := "github.actions.results.api.v1.CacheService/"
	mux.HandleFunc("POST /twirp/"+twirp+"CreateCacheEntry", func(w http.ResponseWriter, r *http.Request) {
		var req ghaCreateReq
		json.NewDecoder(r.Body).Decode(&req)
		f.mu.Lock()
		defer f.mu.Unlock()
		if _, exists := f.blobs[req.Key]; exists {
			json.NewEncoder(w).Encode(ghaCreateResp{OK: false, Message: "cache entry with the same key, version, and scope already exists"})
			return
		}
		// reserve: empty blob until uploaded
		f.blobs[req.Key] = nil
		json.NewEncoder(w).Encode(ghaCreateResp{OK: true, SignedUploadURL: f.srv.URL + "/blob/" + req.Key + "?sig=fake"})
	})
	mux.HandleFunc("POST /twirp/"+twirp+"FinalizeCacheEntryUpload", func(w http.ResponseWriter, r *http.Request) {
		var req ghaFinalizeReq
		json.NewDecoder(r.Body).Decode(&req)
		f.mu.Lock()
		defer f.mu.Unlock()
		f.finaled[req.Key] = true
		json.NewEncoder(w).Encode(ghaFinalizeResp{OK: true, EntryID: "1"})
	})
	mux.HandleFunc("POST /twirp/"+twirp+"GetCacheEntryDownloadURL", func(w http.ResponseWriter, r *http.Request) {
		var req ghaGetDLReq
		json.NewDecoder(r.Body).Decode(&req)
		f.mu.Lock()
		defer f.mu.Unlock()
		if body, exists := f.blobs[req.Key]; !exists || !f.finaled[req.Key] || body == nil {
			json.NewEncoder(w).Encode(ghaGetDLResp{OK: false})
			return
		}
		json.NewEncoder(w).Encode(ghaGetDLResp{OK: true, SignedDownloadURL: f.srv.URL + "/blob/" + req.Key + "?sig=fake", MatchedKey: req.Key})
	})
	mux.HandleFunc("/blob/", func(w http.ResponseWriter, r *http.Request) {
		key := r.URL.Path[len("/blob/"):]
		f.mu.Lock()
		defer f.mu.Unlock()
		switch r.Method {
		case "PUT":
			body, _ := io.ReadAll(r.Body)
			if _, exists := f.blobs[key]; !exists {
				http.Error(w, "no reservation", http.StatusConflict)
				return
			}
			f.blobs[key] = body
			w.WriteHeader(http.StatusCreated)
		case "GET":
			body, exists := f.blobs[key]
			if !exists || body == nil {
				http.Error(w, "not found", http.StatusNotFound)
				return
			}
			w.Write(body)
		default:
			http.Error(w, "bad method", http.StatusMethodNotAllowed)
		}
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func newGhaTestRemote(f *fakeTwirpServer) *Remote {
	return &Remote{
		backend: "gha",
		ghaBase: f.srv.URL,
		ghaTok:  "test-token",
		ghaVer:  ghaVersion(),
		client:  &http.Client{},
	}
}

func TestGHABackendRoundTrip(t *testing.T) {
	f := newFakeTwirpServer(t)
	r := newGhaTestRemote(f)

	// PutIfNotExist: first write created, second rejected
	created, err := r.PutIfNotExistPub("store/b/abc123", []byte("blob-content"))
	if err != nil || !created {
		t.Fatalf("first PutIfNotExist: created=%v err=%v", created, err)
	}
	created, err = r.PutIfNotExistPub("store/b/abc123", []byte("other"))
	if err != nil || created {
		t.Fatalf("duplicate PutIfNotExist must return false: created=%v err=%v", created, err)
	}

	// Exists
	ok, err := r.ExistsPub("store/b/abc123")
	if err != nil || !ok {
		t.Fatalf("Exists: %v %v", ok, err)
	}
	ok, _ = r.ExistsPub("store/b/never")
	if ok {
		t.Fatalf("Exists on absent key must be false")
	}

	// Get (raw path — putIfNotExist stores verbatim)
	body, err := r.Get("store/b/abc123")
	if err != nil || string(body) != "blob-content" {
		t.Fatalf("Get: %q %v", string(body), err)
	}
	body, err = r.Get("store/b/never")
	if err != nil || body != nil {
		t.Fatalf("Get absent: %v %v", body, err)
	}

	// PutPub: gzip at rest, transparent gunzip on Get
	if err := r.PutPub("store/m/manifest1", []byte(`{"key":"m1"}`)); err != nil {
		t.Fatalf("PutPub: %v", err)
	}
	body, err = r.Get("store/m/manifest1")
	if err != nil || string(body) != `{"key":"m1"}` {
		t.Fatalf("PutPub/Get round-trip: %q %v", string(body), err)
	}

	// GetStreamingRaw with hash verification
	content := bytes.Repeat([]byte("stream"), 100000) // 600KB
	sum := sha256.Sum256(content)
	if _, err := r.PutIfNotExistPub("store/b/big", content); err != nil {
		t.Fatalf("big put: %v", err)
	}
	dest := filepath.Join(t.TempDir(), "big.bin")
	if err := r.GetStreamingRaw("store/b/big", hex.EncodeToString(sum[:]), dest); err != nil {
		t.Fatalf("GetStreamingRaw: %v", err)
	}
	got, _ := os.ReadFile(dest)
	if !bytes.Equal(got, content) {
		t.Fatalf("streamed content mismatch: %d bytes", len(got))
	}
	// wrong hash must fail loudly
	if err := r.GetStreamingRaw("store/b/big", "deadbeef", dest+".bad"); err == nil {
		t.Fatalf("hash mismatch must fail")
	}

	// ValidateAuth: successful round trip even on miss
	if err := r.ValidateAuth(); err != nil {
		t.Fatalf("ValidateAuth: %v", err)
	}
}

func TestGHATwirpAuthHeader(t *testing.T) {
	f := newFakeTwirpServer(t)
	var gotAuth string
	f.srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		http.Error(w, "nope", http.StatusUnauthorized)
	})
	r := newGhaTestRemote(f)
	err := r.ghaValidateAuth()
	if err == nil {
		t.Fatalf("unauthorized twirp must error")
	}
	if gotAuth != "Bearer test-token" {
		t.Fatalf("auth header: %q", gotAuth)
	}
}

func TestGHAVersionIsSha256Hex(t *testing.T) {
	v := ghaVersion()
	if len(v) != 64 {
		t.Fatalf("version must be 64 hex chars (service rejects short strings): %q", v)
	}
	if _, err := hex.DecodeString(v); err != nil {
		t.Fatalf("version must be hex: %v", err)
	}
	fmt.Fprintln(os.Stderr, "ok")
}
