// Remote tier for the gocache proxy: Azure Blob storage via plain
// net/http (zero dependencies), ported from the stackrox CA work's
// gocache-serve backend.
//
// Cloud layout (content-addressed, mirroring the local one):
//
//	d/<outputIDhex>   output data blob (dedups across actions)
//	a/<actionIDhex>   JSON {"output_id":...,"size":...} mapping
//
// GET path: local first, then cloud; cloud bodies are verified
// against their OutputID (sha256) before entering the local cache —
// a corrupt or poisoned blob degrades to a miss, never to wrong
// build output (the CA-work integrity lesson).
//
// PUT path: local synchronously (the build needs it), cloud
// asynchronously (best-effort, flushed on close). Last-write-wins is
// benign: keys are content hashes.
//
// Auth: SPN client credentials (AZURE_TENANT_ID / AZURE_CLIENT_ID /
// AZURE_CLIENT_SECRET — the CI path), or a pre-minted bearer token in
// BRINE_AZURE_TOKEN (the `az account get-access-token` dev path).
package gocache

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"path/filepath"

	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

// Remote is the cloud backend. A nil *Remote means "local only".
type Remote struct {
	backend string // "azure" (default) | "gha"

	account   string
	container string
	tenantID  string
	clientID  string
	secret    string
	staticTok string

	// gha backend: Results API (cache v2) — base URL, runtime token,
	// and the cache-entry version (sha256-hex; the service rejects
	// short strings). Set by the env-bootstrap action in CI.
	ghaBase string
	ghaTok  string
	ghaVer  string

	client *http.Client

	tokenMu   sync.Mutex
	token     string
	tokenExp  time.Time
	upMu      sync.Mutex
	uploadQ   []func()
	uploaded  int64
	uploadErr int64
	lastErr   string
}

// NewRemoteFromEnv returns a Remote when remote mode is enabled
// (BRINE_GOCACHE_REMOTE=1), else nil. Backend selection: explicit
// BRINE_STORE_BACKEND wins; otherwise the GHA Results API is used
// when the runner env (ACTIONS_RESULTS_URL + ACTIONS_RUNTIME_TOKEN,
// forwarded by the env-bootstrap action) is present; otherwise Azure
// when BRINE_AZURE_ACCOUNT is configured.
func NewRemoteFromEnv() *Remote {
	if os.Getenv("BRINE_GOCACHE_REMOTE") != "1" {
		return nil
	}
	ghaBase := os.Getenv("ACTIONS_RESULTS_URL")
	ghaTok := os.Getenv("ACTIONS_RUNTIME_TOKEN")
	account := os.Getenv("BRINE_AZURE_ACCOUNT")
	backend := os.Getenv("BRINE_STORE_BACKEND")
	if backend == "" {
		switch {
		case ghaBase != "" && ghaTok != "":
			backend = "gha"
		case account != "":
			backend = "azure"
		default:
			return nil // remote off without any backend — never assume one
		}
	}
	r := &Remote{
		backend:   backend,
		account:   account,
		container: envOr("BRINE_GOCACHE_CONTAINER", "brine"),
		tenantID:  os.Getenv("AZURE_TENANT_ID"),
		clientID:  os.Getenv("AZURE_CLIENT_ID"),
		secret:    os.Getenv("AZURE_CLIENT_SECRET"),
		staticTok: os.Getenv("BRINE_AZURE_TOKEN"),
		ghaBase:   ghaBase,
		ghaTok:    ghaTok,
		ghaVer:    ghaVersion(),
		client: &http.Client{
			Timeout: 30 * time.Second,
			// Default Transport keeps only 2 idle conns per host — with
			// hundreds of concurrent action GETs that serializes to ~2
			// connections and the remote tier crawls (measured 47 req/s
			// despite unbounded goroutines). Pool generously instead.
			Transport: &http.Transport{
				MaxIdleConns:        128,
				MaxIdleConnsPerHost: 128,
				MaxConnsPerHost:     0,
				IdleConnTimeout:     90 * time.Second,
			},
		},
	}
	if backend == "gha" && (r.ghaBase == "" || r.ghaTok == "") {
		return nil // forced gha without runner env — no backend
	}
	return r
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func (r *Remote) containerURL() string {
	return fmt.Sprintf("https://%s.blob.core.windows.net/%s", r.account, r.container)
}

func (r *Remote) getToken() (string, error) {
	if r.backend == "gha" {
		if r.ghaTok == "" {
			return "", fmt.Errorf("gha backend without runtime token")
		}
		return r.ghaTok, nil
	}
	if r.staticTok != "" {
		return r.staticTok, nil
	}
	r.tokenMu.Lock()
	defer r.tokenMu.Unlock()
	if r.token != "" && time.Now().Before(r.tokenExp.Add(-time.Minute)) {
		return r.token, nil
	}
	if r.tenantID == "" || r.clientID == "" || r.secret == "" {
		return "", fmt.Errorf("remote: no credentials (set AZURE_TENANT_ID/CLIENT_ID/CLIENT_SECRET or BRINE_AZURE_TOKEN)")
	}
	tokenURL := fmt.Sprintf("https://login.microsoftonline.com/%s/oauth2/v2.0/token", r.tenantID)
	// url.Values.Encode, NOT raw Sprintf: secrets contain '+&%' that
	// raw interpolation would corrupt (port note from gocache-serve).
	form := url.Values{
		"grant_type":    {"client_credentials"},
		"client_id":     {r.clientID},
		"client_secret": {r.secret},
		"scope":         {"https://storage.azure.com/.default"},
	}.Encode()
	resp, err := r.client.Post(tokenURL, "application/x-www-form-urlencoded", strings.NewReader(form))
	if err != nil {
		return "", fmt.Errorf("remote token: %w", err)
	}
	defer resp.Body.Close()
	var tr struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	body, _ := io.ReadAll(resp.Body)
	if err := json.Unmarshal(body, &tr); err != nil || tr.AccessToken == "" {
		redacted := strings.ReplaceAll(string(body), r.secret, "***")
		return "", fmt.Errorf("remote token failed (AAD: %s)", redacted)
	}
	r.token, r.tokenExp = tr.AccessToken, time.Now().Add(time.Duration(tr.ExpiresIn)*time.Second)
	return r.token, nil
}

// GetTokenPub returns a storage token, or the auth error. Public wrapper
// for startup sanity checks (worker): a credential problem should be
// visible before builds run, not discovered in lost receipts.
func (r *Remote) GetTokenPub() (string, error) {
	return r.getToken()
}

// GetStreamingRaw streams key to dest with sha256 verification and
// STALL detection instead of a total timeout. A fixed deadline (the 30s
// client default, or TimeoutTracker's 3-15s range) kills legitimate
// multi-hundred-MB receipt blobs on modest links — a 148MB prometheus
// binary timed out at exactly the client cap while the blob sat in the
// store, and the caller masked it as "not found". Cancels only when no
// bytes arrive for stallLimit.
func (r *Remote) GetStreamingRaw(key, expectHash, dest string) error {
	if r.backend == "gha" {
		url, err := r.ghaLookupURL(key)
		if err != nil {
			return err
		}
		if url == "" {
			return fmt.Errorf("remote GET %s: not found", key)
		}
		return r.streamURLToDest(url, nil, key, expectHash, dest)
	}
	token, err := r.getToken()
	if err != nil {
		return err
	}
	return r.streamURLToDest(r.containerURL()+"/"+key,
		map[string]string{
			"Authorization": "Bearer " + token,
			"x-ms-version":  "2023-11-03",
		}, key, expectHash, dest)
}

// streamURLToDest streams a URL to dest with sha256 verification and
// STALL detection instead of a total timeout (see GetStreamingRaw).
func (r *Remote) streamURLToDest(url string, headers map[string]string, key, expectHash, dest string) error {
	// no-total-timeout client sharing the transport pool
	client := &http.Client{Transport: r.client.Transport}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}

	const stallLimit = 30 * time.Second
	timer := time.AfterFunc(stallLimit, cancel) // armed; reset on every read
	defer timer.Stop()

	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return fmt.Errorf("remote GET %s: not found", key)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("remote GET %s: HTTP %d", key, resp.StatusCode)
	}

	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}
	tmp := dest + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	h := sha256.New()
	buf := make([]byte, 256<<10)
	fetchErr := func() error {
		for {
			n, rerr := resp.Body.Read(buf)
			if n > 0 {
				timer.Reset(stallLimit)
				h.Write(buf[:n])
				if _, werr := f.Write(buf[:n]); werr != nil {
					return werr
				}
			}
			if rerr == io.EOF {
				return nil
			}
			if rerr != nil {
				return rerr
			}
		}
	}()
	f.Close()
	if fetchErr != nil {
		os.Remove(tmp)
		return fetchErr
	}
	if hex.EncodeToString(h.Sum(nil)) != expectHash {
		os.Remove(tmp)
		return fmt.Errorf("integrity: %s content does not hash to key", key)
	}
	return os.Rename(tmp, dest)
}

// ValidateAuth verifies credentials against the actual container: fetches
// a key that never exists — 401/403 means bad credentials, 404 means the
// token is valid and the container reachable. Static tokens are otherwise
// returned unchecked (no AAD round-trip), which made an invalid token
// look "OK" at worker startup (found the hard way).
func (r *Remote) ValidateAuth() error {
	if r.backend == "gha" {
		return r.ghaValidateAuth()
	}
	token, err := r.getToken()
	if err != nil {
		return err
	}
	probe := "brine-auth-probe-nonexistent"
	req, err := http.NewRequest("GET", r.containerURL()+"/"+probe, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("x-ms-version", "2023-11-03")
	resp, err := r.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusNotFound:
		return nil // token valid, key absent — exactly what we asked for
	case http.StatusUnauthorized, http.StatusForbidden:
		return fmt.Errorf("container rejected credentials (HTTP %d)", resp.StatusCode)
	default:
		return fmt.Errorf("unexpected probe status HTTP %d", resp.StatusCode)
	}
}

// Get returns the blob at key, or nil,nil when absent.
func (r *Remote) Get(key string) ([]byte, error) {
	if r.backend == "gha" {
		return r.ghaGet(key)
	}
	token, err := r.getToken()
	if err != nil {
		return nil, err
	}
	req, _ := http.NewRequest("GET", r.containerURL()+"/"+key, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("x-ms-version", "2023-11-03")
	resp, err := r.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, nil
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("remote GET %s: HTTP %d", key, resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	return maybeGunzip(body), nil
}

// put uploads body at key (gzip at rest), last-write-wins.
func (r *Remote) put(key string, body []byte) error {
	if r.backend == "gha" {
		return r.ghaPut(key, body)
	}
	token, err := r.getToken()
	if err != nil {
		return err
	}
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	zw.Write(body)
	zw.Close()
	req, err := http.NewRequest("PUT", r.containerURL()+"/"+key, bytes.NewReader(buf.Bytes()))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("x-ms-blob-type", "BlockBlob")
	req.Header.Set("x-ms-version", "2023-11-03")
	resp, err := r.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		return fmt.Errorf("remote PUT %s: HTTP %d", key, resp.StatusCode)
	}
	return nil
}

// putIfNotExist writes only when absent; returns false if it exists.
func (r *Remote) putIfNotExist(key string, body []byte) (bool, error) {
	if r.backend == "gha" {
		return r.ghaStore(key, body) // raw bytes; duplicate → (false, nil)
	}
	token, err := r.getToken()
	if err != nil {
		return false, err
	}
	req, err := http.NewRequest("PUT", r.containerURL()+"/"+key, bytes.NewReader(body))
	if err != nil {
		return false, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("x-ms-blob-type", "BlockBlob")
	req.Header.Set("x-ms-version", "2023-11-03")
	req.Header.Set("If-None-Match", "*")
	resp, err := r.client.Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusCreated:
		return true, nil
	case http.StatusConflict, http.StatusPreconditionFailed:
		return false, nil
	default:
		return false, fmt.Errorf("remote PUT-if-none-match %s: HTTP %d", key, resp.StatusCode)
	}
}

// remoteGetAction fetches an action from the cloud and, if valid,
// writes it into the local standard layout. Returns ok=false on any
// absence, error, or integrity failure (degrades to a local miss).
// The data blob STREAMS to disk through a hasher — constant memory
// regardless of size (the CA-work OOM lesson). Actions known absent
// (negatives) skip the cloud entirely.
func remoteGetAction(r *Remote, cacheDir, actionIDHex string, neg *Negatives, tt *TimeoutTracker) bool {
	if neg.Has(actionIDHex) {
		return false
	}
	rec, err := r.Get("a/" + actionIDHex)
	if err != nil || rec == nil {
		if rec == nil {
			neg.Add(actionIDHex) // known absent: stop re-probing
		}
		return false
	}
	var ar struct {
		OutputID string `json:"output_id"`
		Size     int64  `json:"size"`
	}
	if json.Unmarshal(rec, &ar) != nil || len(ar.OutputID) != 64 {
		return false
	}
	dest := dataPath(cacheDir, ar.OutputID)
	if _, err := os.Stat(dest); err == nil {
		// Data already local (e.g. concurrent fetch) — just need the record.
		writeActionRecord(cacheDir, actionIDHex, ar.OutputID, ar.Size)
		neg.Clear(actionIDHex)
		return true
	}
	size, ok := r.getStreamingVerify("d/"+ar.OutputID, ar.OutputID, dest, tt)
	if !ok {
		return false
	}
	if ar.Size > 0 && size != ar.Size {
		os.Remove(dest)
		return false
	}
	writeActionRecord(cacheDir, actionIDHex, ar.OutputID, size)
	neg.Clear(actionIDHex)
	return true
}

// queueCloudPut schedules an async upload of an action's data file to
// the cloud. Streams from the local cache file (constant memory —
// no body buffering for large link outputs).
func queueCloudPut(r *Remote, actionIDHex, outputIDHex, dataFilePath string, size int64) {
	r.upMu.Lock()
	r.uploadQ = append(r.uploadQ, func() {
		if dataFilePath != "" {
			if _, err := r.putFileIfNotExistGzip("d/"+outputIDHex, dataFilePath); err != nil {
				r.upMu.Lock()
				r.uploadErr++
				r.lastErr = err.Error()
				r.upMu.Unlock()
				return
			}
		}
		rec, _ := json.Marshal(struct {
			OutputID string `json:"output_id"`
			Size     int64  `json:"size"`
		}{outputIDHex, size})
		if err := r.put("a/"+actionIDHex, rec); err != nil {
			r.upMu.Lock()
			r.uploadErr++
			r.lastErr = err.Error()
			r.upMu.Unlock()
			return
		}
		r.upMu.Lock()
		r.uploaded++
		r.upMu.Unlock()
	})
	r.upMu.Unlock()
}

// PutIfNotExistPub / PutPub / ExistsPub: exported wrappers for the
// brine store package (receipts + output blobs share this backend).
func (r *Remote) PutIfNotExistPub(key string, body []byte) (bool, error) {
	return r.putIfNotExist(key, body)
}

func (r *Remote) PutPub(key string, body []byte) error {
	return r.put(key, body)
}

func (r *Remote) ExistsPub(key string) (bool, error) {
	return r.exists(key)
}

// flushUploadsConcurrent drains the queue with bounded parallelism.
// Sequential draining measured 476s for a 3,654-action Prometheus
// build (2 PUTs per action at ~65ms each) — the upload must not be
// the tail latency of a build that just finished.
func (r *Remote) flushUploadsConcurrent() {
	r.upMu.Lock()
	q := r.uploadQ
	r.uploadQ = nil
	r.upMu.Unlock()
	var wg sync.WaitGroup
	sem := make(chan struct{}, 16)
	for _, f := range q {
		wg.Add(1)
		sem <- struct{}{}
		go func(f func()) {
			defer wg.Done()
			defer func() { <-sem }()
			f()
		}(f)
	}
	wg.Wait()
}

func (r *Remote) uploadCountsWithDetail() (uploaded, failed int64, lastErr string) {
	r.upMu.Lock()
	defer r.upMu.Unlock()
	return r.uploaded, r.uploadErr, r.lastErr
}

func maybeGunzip(body []byte) []byte {
	zr, err := gzip.NewReader(bytes.NewReader(body))
	if err != nil {
		return body // not gzip (legacy/raw)
	}
	out, err := io.ReadAll(zr)
	if err != nil {
		return body
	}
	return out
}
