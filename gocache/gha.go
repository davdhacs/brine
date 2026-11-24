// GitHub Actions cache v2 backend ("gha"). Speaks the Results API
// twirp protocol (verified live 2025-11-24, see
// docs/data/storage-benchmark-2025-11-24.md):
//
//	POST {ACTIONS_RESULTS_URL}/twirp/github.actions.results.api.v1.CacheService/
//	     CreateCacheEntry         {key, version} → {ok, signed_upload_url}
//	     FinalizeCacheEntryUpload {key, version, size_bytes} → {ok, entry_id}
//	     GetCacheEntryDownloadURL {key, version} → {ok, signed_download_url}
//
// with Bearer ACTIONS_RUNTIME_TOKEN auth. The SAS URLs point at Azure
// Blob storage — uploads are direct PUTs, downloads direct GETs (range
// requests work: 206). Entries are immutable: a duplicate
// CreateCacheEntry returns ok=false with "already exists" — exactly
// PutIfNotExist semantics. The runtime token/URL are only present in
// action processes; the env-bootstrap action
// (.github/actions/env-bootstrap) forwards them via GITHUB_ENV.
//
// Key mapping: brine keys ride verbatim in the twirp key field (JSON
// string; slashes included). The version must look like the toolkit's
// sha256-hex — a short string is rejected ("version invalid length").
package gocache

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
	"os"
)

const ghaTwirpService = "github.actions.results.api.v1.CacheService"

func ghaVersion() string {
	sum := sha256.Sum256([]byte("brine-remote-v1"))
	return hex.EncodeToString(sum[:])
}

type ghaCreateReq struct {
	Key     string `json:"key"`
	Version string `json:"version"`
}

type ghaCreateResp struct {
	OK               bool   `json:"ok"`
	SignedUploadURL  string `json:"signed_upload_url"`
	Message          string `json:"message"`
}

type ghaFinalizeReq struct {
	Key       string `json:"key"`
	Version   string `json:"version"`
	SizeBytes string `json:"size_bytes"`
}

type ghaFinalizeResp struct {
	OK      bool   `json:"ok"`
	EntryID string `json:"entry_id"`
	Message string `json:"message"`
}

type ghaGetDLReq struct {
	Key     string `json:"key"`
	Version string `json:"version"`
}

type ghaGetDLResp struct {
	OK                bool   `json:"ok"`
	SignedDownloadURL string `json:"signed_download_url"`
	MatchedKey        string `json:"matched_key"`
}

// twirpError carries the twirp error envelope so callers can branch
// on code (e.g. already_exists = dedup, not failure).
type twirpError struct {
	Method string
	Status int
	Code   string
	Msg    string
}

func (e *twirpError) Error() string {
	return fmt.Sprintf("gha %s: HTTP %d: %s: %s", e.Method, e.Status, e.Code, e.Msg)
}

// ghaTwirp POSTs one twirp JSON RPC and decodes the response.
func (r *Remote) ghaTwirp(method string, req any, out any) error {
	body, err := json.Marshal(req)
	if err != nil {
		return err
	}
	hreq, err := http.NewRequest("POST",
		r.ghaBase+"/twirp/"+ghaTwirpService+"/"+method, bytes.NewReader(body))
	if err != nil {
		return err
	}
	hreq.Header.Set("Authorization", "Bearer "+r.ghaTok)
	hreq.Header.Set("Content-Type", "application/json")
	resp, err := r.client.Do(hreq)
	if err != nil {
		return fmt.Errorf("gha %s: %w", method, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 400))
		var te twirpError
		te.Method, te.Status = method, resp.StatusCode
		if json.Unmarshal(b, &struct {
			Code *string `json:"code"`
			Msg  *string `json:"msg"`
		}{&te.Code, &te.Msg}) != nil || te.Code == "" {
			te.Msg = string(b)
		}
		return &te
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("gha %s: decode: %w", method, err)
	}
	return nil
}

// ghaBlobPut uploads body to the SAS URL (direct Azure Blob PUT),
// retrying transient failures once — a reserved-but-empty entry is
// immutable and poisons the key (see ghaStore).
func (r *Remote) ghaBlobPut(sasURL string, body []byte) error {
	var lastErr error
	for attempt := 0; attempt < 2; attempt++ {
		hreq, err := http.NewRequest("PUT", sasURL, bytes.NewReader(body))
		if err != nil {
			return err
		}
		hreq.Header.Set("x-ms-blob-type", "BlockBlob")
		resp, err := r.client.Do(hreq)
		if err != nil {
			lastErr = fmt.Errorf("gha blob PUT: %w", err)
			continue
		}
		resp.Body.Close()
		if resp.StatusCode == http.StatusCreated {
			return nil
		}
		lastErr = fmt.Errorf("gha blob PUT: HTTP %d", resp.StatusCode)
	}
	return lastErr
}

// ghaCreate reserves a cache entry. Returns (uploadURL, created,
// err): created=false means the key already exists (immutable — the
// PutIfNotExist primitive). The service signals duplicates BOTH as
// 200+ok=false AND as HTTP 409 already_exists (found live: the
// finalized-entry form is the 409) — both map to created=false.
func (r *Remote) ghaCreate(key string) (string, bool, error) {
	var resp ghaCreateResp
	if err := r.ghaTwirp("CreateCacheEntry", ghaCreateReq{Key: key, Version: r.ghaVer}, &resp); err != nil {
		var te *twirpError
		if errors.As(err, &te) && (te.Code == "already_exists" || te.Status == http.StatusConflict) {
			return "", false, nil
		}
		return "", false, err
	}
	if !resp.OK {
		return "", false, nil // duplicate (or policy denial) — no upload URL
	}
	return resp.SignedUploadURL, true, nil
}

// ghaStore: create → upload → finalize → verify; dedup-skips are
// verified too. body is stored verbatim.
//
// The verification closes the poisoned-key hole: GHA entries are
// IMMUTABLE, and a failed PUT can leave a reserved-but-empty entry.
// An unverified dedup-skip would count it as success and the manifest
// would reference broken bytes forever (found live: INTEGRITY FAILURE
// on restore of an empty libmbedcrypto.a). Policy: verify every
// referenced entry; broken → repair (delete + repush) when possible,
// else REFUSE the store so the manifest is never uploaded with a
// broken reference (the receipt stays local — correct, just unshared).
func (r *Remote) ghaStore(key string, body []byte) (created bool, err error) {
	url, created, err := r.ghaCreate(key)
	if err != nil {
		return false, err
	}
	if created {
		if err := r.ghaPutEntry(url, key, body); err != nil {
			return true, err
		}
		return true, nil
	}
	// dedup: the entry exists — verify it is LIVE (serves bytes; the
	// poisoned case is a reserved-but-EMPTY entry). Content correctness
	// is enforced at restore time (every fetch hash-verifies); here we
	// only refuse to reference dead entries. Different content under
	// the same key is legitimate PutIfNotExist semantics — existing
	// wins, the caller gets created=false. Retries cover the metadata
	// propagation lag on just-finalized entries.
	var verr error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			time.Sleep(time.Duration(attempt) * 500 * time.Millisecond)
		}
		if verr = r.ghaVerifyEntryLive(key); verr == nil {
			return false, nil // healthy duplicate — the common case
		}
	}
	// broken existing entry: repair, or refuse to reference it
	if rerr := r.ghaRepairEntry(key, body); rerr == nil {
		return true, nil
	}
	return false, fmt.Errorf("gha store %s: existing entry broken and unrepairable — refusing to store a manifest referencing it", key)
}

// ghaPutEntry uploads body to a fresh SAS URL and verifies the result.
func (r *Remote) ghaPutEntry(sasURL, key string, body []byte) error {
	if err := r.ghaBlobPut(sasURL, body); err != nil {
		return err
	}
	var fin ghaFinalizeResp
	if err := r.ghaTwirp("FinalizeCacheEntryUpload",
		ghaFinalizeReq{Key: key, Version: r.ghaVer, SizeBytes: fmt.Sprintf("%d", len(body))}, &fin); err != nil {
		return err
	}
	if !fin.OK {
		return fmt.Errorf("gha finalize %s: %s", key, fin.Message)
	}
	// range-verify with propagation retries: the cache service's
	// metadata (entry→blob mapping) is eventually consistent — a
	// just-finalized entry may not be visible to GetCacheEntryDownloadURL
	// for a moment (found live: healthy uploads treated as broken,
	// receipts never shared; the 500ms retry removed with the
	// full-read-back was protecting exactly this).
	if err := r.ghaVerifyEntryRetry(key, body); err != nil {
		// immutable entries cannot be re-written in place — repair by
		// DELETE (REST, needs the workflow's github.token) + re-create.
		// NOTE: the REST delete works off the ~5-min index — fresh
		// entries cannot be deleted; with the retries above, verification
		// normally passes before repair is attempted.
		if rerr := r.ghaRepairEntry(key, body); rerr != nil {
			return fmt.Errorf("gha verify %s: %w (repair unavailable: %v)", key, err, rerr)
		}
	}
	return nil
}

// ghaVerifyEntryRetry runs ghaVerifyEntry with propagation backoff
// (500ms, 1s, 2s) — the data plane (Azure Blob) is consistent, but
// the cache service's entry metadata lags finalize slightly.
func (r *Remote) ghaVerifyEntryRetry(key string, stored []byte) error {
	var err error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			time.Sleep(time.Duration(attempt) * 500 * time.Millisecond)
		}
		if err = r.ghaVerifyEntry(key, stored); err == nil {
			return nil
		}
	}
	return err
}

// ghaRepairEntry heals a poisoned immutable key: delete the broken
// entry via the REST API, then create + put + finalize + verify fresh.
func (r *Remote) ghaRepairEntry(key string, body []byte) error {
	if err := r.ghaDeleteEntry(key); err != nil {
		return err
	}
	url, created, err := r.ghaCreate(key)
	if err != nil || !created {
		return fmt.Errorf("re-create after delete: %v (created=%v)", err, created)
	}
	if err := r.ghaBlobPut(url, body); err != nil {
		return err
	}
	var fin ghaFinalizeResp
	if err := r.ghaTwirp("FinalizeCacheEntryUpload",
		ghaFinalizeReq{Key: key, Version: r.ghaVer, SizeBytes: fmt.Sprintf("%d", len(body))}, &fin); err != nil {
		return err
	}
	if !fin.OK {
		return fmt.Errorf("gha finalize %s: %s", key, fin.Message)
	}
	return r.ghaVerifyEntry(key, body)
}

// ghaVerifyEntryLive proves a dedup-skipped entry actually serves
// bytes: it has a download URL and a non-zero Content-Range total.
// The poisoned-key failure mode (create succeeded, PUT never did)
// presents as no-URL or total=0. Deliberately weaker than
// ghaVerifyEntry: content correctness is the restore side's job
// (every fetch hash-verifies against the content-addressed key).
func (r *Remote) ghaVerifyEntryLive(key string) error {
	url, err := r.ghaLookupURL(key)
	if err != nil {
		return err
	}
	if url == "" {
		return fmt.Errorf("entry has no download URL")
	}
	hreq, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return err
	}
	hreq.Header.Set("Range", "bytes=0-0")
	resp, err := r.client.Do(hreq)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusPartialContent {
		return fmt.Errorf("range GET: HTTP %d (want 206)", resp.StatusCode)
	}
	cr := resp.Header.Get("Content-Range")
	var start, total int64
	if _, err := fmt.Sscanf(cr, "bytes %d-%d/%d", &start, new(int64), &total); err != nil {
		return fmt.Errorf("Content-Range parse: %q", cr)
	}
	if total == 0 {
		return fmt.Errorf("entry serves 0 bytes")
	}
	return nil
}

// ghaVerifyEntry proves the stored entry is live and correct with a
// 64-byte ranged GET: the prefix bytes must match what we uploaded
// and the Content-Range total must equal the stored size. This catches
// reserved-but-empty entries (the immutable-poisoning failure mode)
// at negligible transfer cost.
func (r *Remote) ghaVerifyEntry(key string, stored []byte) error {
	url, err := r.ghaLookupURL(key)
	if err != nil {
		return err
	}
	if url == "" {
		return fmt.Errorf("entry absent after finalize")
	}
	hreq, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return err
	}
	n := 64
	if len(stored) < n {
		n = len(stored)
	}
	hreq.Header.Set("Range", fmt.Sprintf("bytes=0-%d", n-1))
	resp, err := r.client.Do(hreq)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusPartialContent {
		return fmt.Errorf("range GET: HTTP %d (want 206)", resp.StatusCode)
	}
	// Content-Range: bytes 0-63/<total>
	cr := resp.Header.Get("Content-Range")
	var total int64
	if _, err := fmt.Sscanf(cr, "bytes 0-%d/%d", new(int64), &total); err != nil {
		return fmt.Errorf("Content-Range parse: %q", cr)
	}
	if total != int64(len(stored)) {
		return fmt.Errorf("entry size %d, uploaded %d", total, len(stored))
	}
	prefix := make([]byte, n)
	if _, err := io.ReadFull(resp.Body, prefix); err != nil {
		return err
	}
	if !bytes.Equal(prefix, stored[:n]) {
		return fmt.Errorf("entry prefix mismatch")
	}
	return nil
}

// ghaDeleteEntry removes a cache entry via the REST API — the repair
// path for immutable poisoned keys. Requires the workflow to expose
// the job token (BRINE_GH_TOKEN or GITHUB_TOKEN env) and the runner's
// GITHUB_REPOSITORY / GITHUB_API_URL.
func (r *Remote) ghaDeleteEntry(key string) error {
	token := os.Getenv("BRINE_GH_TOKEN")
	if token == "" {
		token = os.Getenv("GITHUB_TOKEN")
	}
	repo := os.Getenv("GITHUB_REPOSITORY")
	if token == "" || repo == "" {
		return fmt.Errorf("no token/repo for entry deletion")
	}
	api := os.Getenv("GITHUB_API_URL")
	if api == "" {
		api = "https://api.github.com"
	}
	hreq, err := http.NewRequest("DELETE",
		api+"/repos/"+repo+"/actions/caches?key="+url.QueryEscape(key), nil)
	if err != nil {
		return err
	}
	hreq.Header.Set("Authorization", "Bearer "+token)
	hreq.Header.Set("Accept", "application/vnd.github+json")
	resp, err := r.client.Do(hreq)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("entry delete: HTTP %d", resp.StatusCode)
	}
	return nil
}

// ghaLookupURL returns the signed download URL, or "" when absent.
func (r *Remote) ghaLookupURL(key string) (string, error) {
	var resp ghaGetDLResp
	if err := r.ghaTwirp("GetCacheEntryDownloadURL",
		ghaGetDLReq{Key: key, Version: r.ghaVer}, &resp); err != nil {
		return "", err
	}
	if !resp.OK {
		return "", nil
	}
	return resp.SignedDownloadURL, nil
}

// --- Remote method implementations (gha backend) ---

func (r *Remote) ghaGet(key string) ([]byte, error) {
	url, err := r.ghaLookupURL(key)
	if err != nil || url == "" {
		return nil, err
	}
	hreq, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := r.client.Do(hreq)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, nil // SAS URLs expire — treat as absent, caller rebuilds
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("gha GET %s: HTTP %d", key, resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	return maybeGunzip(body), nil
}

func (r *Remote) ghaExists(key string) (bool, error) {
	url, err := r.ghaLookupURL(key)
	return url != "", err
}

func (r *Remote) ghaPut(key string, body []byte) error {
	// gzip at rest for parity with the azure put path (ghaGet
	// gunzips). Immutable entries: a duplicate create is a no-op —
	// first writer wins, which is sound for content-derived keys.
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	zw.Write(body)
	zw.Close()
	_, err := r.ghaStore(key, buf.Bytes())
	return err
}

func (r *Remote) ghaValidateAuth() error {
	// A successful twirp round trip (even a miss) proves token + URL.
	var resp ghaGetDLResp
	return r.ghaTwirp("GetCacheEntryDownloadURL",
		ghaGetDLReq{Key: "brine-auth-probe-nonexistent", Version: r.ghaVer}, &resp)
}
