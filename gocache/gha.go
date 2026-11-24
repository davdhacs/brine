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
	"fmt"
	"io"
	"net/http"
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
		return fmt.Errorf("gha %s: HTTP %d: %s", method, resp.StatusCode, b)
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("gha %s: decode: %w", method, err)
	}
	return nil
}

// ghaBlobPut uploads body to the SAS URL (direct Azure Blob PUT).
func (r *Remote) ghaBlobPut(sasURL string, body []byte) error {
	hreq, err := http.NewRequest("PUT", sasURL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	hreq.Header.Set("x-ms-blob-type", "BlockBlob")
	resp, err := r.client.Do(hreq)
	if err != nil {
		return fmt.Errorf("gha blob PUT: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		return fmt.Errorf("gha blob PUT: HTTP %d", resp.StatusCode)
	}
	return nil
}

// ghaCreate reserves a cache entry. Returns (uploadURL, created,
// err): created=false means the key already exists (immutable — the
// PutIfNotExist primitive).
func (r *Remote) ghaCreate(key string) (string, bool, error) {
	var resp ghaCreateResp
	if err := r.ghaTwirp("CreateCacheEntry", ghaCreateReq{Key: key, Version: r.ghaVer}, &resp); err != nil {
		return "", false, err
	}
	if !resp.OK {
		return "", false, nil // duplicate (or policy denial) — no upload URL
	}
	return resp.SignedUploadURL, true, nil
}

// ghaStore: create → upload → finalize. body is stored verbatim.
func (r *Remote) ghaStore(key string, body []byte) (created bool, err error) {
	url, created, err := r.ghaCreate(key)
	if err != nil || !created {
		return created, err
	}
	if err := r.ghaBlobPut(url, body); err != nil {
		return created, err
	}
	var fin ghaFinalizeResp
	if err := r.ghaTwirp("FinalizeCacheEntryUpload",
		ghaFinalizeReq{Key: key, Version: r.ghaVer, SizeBytes: fmt.Sprintf("%d", len(body))}, &fin); err != nil {
		return created, err
	}
	if !fin.OK {
		return created, fmt.Errorf("gha finalize %s: %s", key, fin.Message)
	}
	return true, nil
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
