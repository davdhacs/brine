// Config tiers for brine: from zero-config-safe to CI-tuned.
//
// Precedence: env var > .brine.json > defaults.
//
// TIER 0 (default — no config, no env): local store, every sensor on,
//
//	full verification, no env/file keying. Safe to try on anything.
//
// TIER 1 (project — .brine.json checked into the repo): declares the
//
//	project's build identity so the whole team shares determinism:
//
//	  {
//	    "key_env":  ["FEATURE", "CFLAGS"],        // env that changes output
//	    "key_files": ["flags.conf"],              // extra files to key
//	    "buildvcs": "key-fold"                    // off | key-fold
//	  }
//
// TIER 2 (CI — env overrides on top of tier 1): remote store, scope,
//
//	and where a ground-truth gate exists, verification trade-offs:
//
//	  BRINE_REMOTE=1 BRINE_AZURE_ACCOUNT=... BRINE_GOCACHE_CONTAINER=...
//	  BRINE_GOCACHE_SCOPE comes from the command automatically
//	  BRINE_TRUST_LOCAL=1   (only with a byte-identity CI gate)
//
// Everything is independently settable; tiers are conventions, not modes.
package main

import (
	"encoding/json"
	"os"
	"strings"
)

// Config is the project-level brine configuration (.brine.json).
type Config struct {
	// KeyEnv: environment variables that influence build output and
	// must be folded into every key (the B2 case: configs generated
	// from env; CFLAGS for make). Env override: BRINE_KEY_ENV=a,b
	KeyEnv []string `json:"key_env,omitempty"`

	// KeyFiles: file globs to fold into keys beyond the tracked tree
	// (escape hatch for anything the sensors miss).
	// Env override: BRINE_KEY_FILES=glob1,glob2
	KeyFiles []string `json:"key_files,omitempty"`

	// KeyImages: docker image refs folded into keys beyond the tracked
	// tree (resolved to local digests at key time). Escape hatch for
	// docker run/build shapes; `docker exec` containers are detected
	// from the command automatically. Env override: BRINE_KEY_IMAGES
	KeyImages []string `json:"key_images,omitempty"`

	// OutputPatterns: scope the receipt snapshot to deliverables
	// (substring or glob, same semantics as `brine restore`).
	// The collector: 898 artifacts / 1.6GB → 2 binaries / 85MB.
	// Default (unset): snapshot every output — full incremental-
	// rebuild state. Env override: BRINE_OUTPUT_PATTERNS
	OutputPatterns []string `json:"output_patterns,omitempty"`

	// Closure: "key_files" switches the receipt closure to EXACTLY the
	// key_files globs — nothing observed, nothing walked. For builds
	// whose inputs are a declared artifact set (a tarball-driven dep
	// build): the key is computable identically at check time and
	// store time on a fresh machine, which the observed/noVCS paths
	// cannot guarantee (extraction-inside-the-wrap pollutes observed
	// read-sets with files that don't exist at check time).
	Closure string `json:"closure,omitempty"`

	// Incremental: on a receipt miss, restore the outputs of the most
	// recent manifest for the SAME command before running — the build
	// tool's own freshness logic (make: .o vs source mtimes) decides
	// what to rebuild. Pairs with source-mtime normalization (checkout
	// files stamped with their commit times) so restored intermediates
	// are correctly fresh for unchanged sources, stale for changed
	// ones. The receipt vouches for the bytes; make vouches for the
	// rebuild decision.
	Incremental bool `json:"incremental,omitempty"`

	// BuildVCS: "off" disables VCS key-folding (promise that stamping
	// is disabled in your build). Default "key-fold".
	BuildVCS string `json:"buildvcs,omitempty"`

	// Writes: enable fsnotify write-tracking during non-go builds
	// (exact output semantics; costs one watch per directory, capped).
	// Default true. Env override: BRINE_WRITES=0 to disable.
	Writes *bool `json:"writes,omitempty"`

	// TrustLocal: skip local blob re-verification on restore
	// (~1s/GB). Only with an external byte-identity gate.
	// Env override: BRINE_TRUST_LOCAL=1
	TrustLocal *bool `json:"trust_local,omitempty"`

	// Remote: the Azure blob store (tier 2). Env overrides:
	// BRINE_REMOTE=1 + BRINE_AZURE_ACCOUNT + BRINE_GOCACHE_CONTAINER
	Remote *RemoteConfig `json:"remote,omitempty"`

	// Gocache: the go compile-cache proxy tier.
	Gocache *GocacheConfig `json:"gocache,omitempty"`
}

type RemoteConfig struct {
	Account   string `json:"account,omitempty"`
	Container string `json:"container,omitempty"`
}

type GocacheConfig struct {
	// Scope manifests per build target (default true; the CI
	// serve-change leg prefetched 17K actions for a 5K build without
	// it). Env override: BRINE_GOCACHE_SCOPE (set by brine itself).
	Scope *bool `json:"scope,omitempty"`
	// Prefetch the working set at proxy start (default true).
	// Env override: BRINE_GOCACHE_NOPREFETCH=1
	Prefetch *bool `json:"prefetch,omitempty"`
}

// configFile is the legacy field name kept working.
type legacyConfigFile struct {
	Deterministic struct {
		BuildVCS string `json:"buildvcs"`
	} `json:"deterministic"`
}

func loadConfig(wd string) Config {
	var c Config
	data, err := os.ReadFile(wd + "/.brine.json")
	if err == nil {
		json.Unmarshal(data, &c)
		// legacy shape
		var l legacyConfigFile
		if json.Unmarshal(data, &l) == nil && l.Deterministic.BuildVCS != "" && c.BuildVCS == "" {
			c.BuildVCS = l.Deterministic.BuildVCS
		}
	}

	// env overrides
	if v := os.Getenv("BRINE_KEY_ENV"); v != "" {
		c.KeyEnv = splitTrim(v)
	}
	if v := os.Getenv("BRINE_KEY_FILES"); v != "" {
		c.KeyFiles = splitTrim(v)
	}
	if v := os.Getenv("BRINE_KEY_IMAGES"); v != "" {
		c.KeyImages = splitTrim(v)
	}
	if v := os.Getenv("BRINE_OUTPUT_PATTERNS"); v != "" {
		c.OutputPatterns = splitTrim(v)
	}
	if v := os.Getenv("BRINE_WRITES"); v == "0" {
		f := false
		c.Writes = &f
	}
	if v := os.Getenv("BRINE_TRUST_LOCAL"); v == "1" {
		f := true
		c.TrustLocal = &f
	}
	return c
}

func splitTrim(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func boolOr(b *bool, def bool) bool {
	if b == nil {
		return def
	}
	return *b
}
