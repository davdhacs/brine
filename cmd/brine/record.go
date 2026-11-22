// Record mode: run a build with full instrumentation, observe what the
// sensors find, and write the discovered configuration to .brine.json
// so every later run benefits. The "learn your build" command.
//
//	brine record make          # run, observe, record
//	brine record go build ...  # works for any command brine wraps
//
// What it observes and records:
//
//	buildvcs:     VCS stamping detected in output → "key-fold" (or
//	              "off" if GOFLAGS carries -buildvcs=false)
//	key_files:    untracked configs that ended up in read-sets
//	              (project-specific config paths beyond the built-in
//	              patterns)
//	advisories:   divergence-with-no-file-cause (likely env-driven →
//	              suggests key_env), write-tracking disabled by scale,
//	              stale read-set entries that keep missing
//
// Record never *guesses* env vars (it cannot see them); it names the
// symptom and asks the user to declare them.
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Advisory is one improvement suggestion found during a run.
type Advisory struct {
	Kind    string // "env-suspected" | "config-found" | "writes-off" | "buildvcs"
	Message string
	Config  string // the JSON fragment to add, if applicable
}

// recordConfig merges discovered settings into .brine.json.
func recordConfig(wd string, advisories []Advisory) error {
	cfgPath := filepath.Join(wd, ".brine.json")

	// load current (raw map for merge)
	var raw map[string]interface{}
	if data, err := os.ReadFile(cfgPath); err == nil {
		json.Unmarshal(data, &raw)
	}
	if raw == nil {
		raw = map[string]interface{}{}
	}

	changed := false

	for _, a := range advisories {
		switch a.Kind {
		case "buildvcs":
			if raw["buildvcs"] == nil {
				raw["buildvcs"] = a.Config
				changed = true
				fmt.Printf("[brine] record: buildvcs = %q\n", a.Config)
			}
		case "config-found":
			var files []string
			if existing, ok := raw["key_files"].([]interface{}); ok {
				for _, f := range existing {
					if s, ok := f.(string); ok {
						files = append(files, s)
					}
				}
			}
			found := false
			for _, f := range files {
				if f == a.Config {
					found = true
				}
			}
			if !found {
				files = append(files, a.Config)
				raw["key_files"] = files
				changed = true
				fmt.Printf("[brine] record: key_files += %q\n", a.Config)
			}
		case "env-suspected":
			// Cannot guess which var — just report.
			fmt.Printf("[brine] record: %s\n", a.Message)
		case "writes-off":
			fmt.Printf("[brine] record: %s\n", a.Message)
		}
	}

	if !changed {
		fmt.Println("[brine] record: no new config to record")
		return nil
	}

	data, err := json.MarshalIndent(raw, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(cfgPath, data, 0o644)
}

// collectAdvisories runs after a build to gather what the sensors
// observed. Called from the record path.
func collectAdvisories(wd string, isGo bool, readSet []string,
	divergence bool, buildInfoVCS string, writesActive bool) []Advisory {
	var out []Advisory

	if isGo {
		if buildInfoVCS != "" && !strings.Contains(os.Getenv("GOFLAGS"), "-buildvcs=false") {
			out = append(out, Advisory{Kind: "buildvcs", Config: "key-fold"})
		}
		if strings.Contains(os.Getenv("GOFLAGS"), "-buildvcs=false") {
			out = append(out, Advisory{Kind: "buildvcs", Config: "off"})
		}
	}

	for _, f := range readSet {
		if isUntrackedConfigCandidate(wd, f) {
			out = append(out, Advisory{Kind: "config-found", Config: f})
		}
	}
	for _, f := range discoverIgnoredConfigs(wd) {
		out = append(out, Advisory{
			Kind:    "config-found",
			Config:  f,
			Message: "gitignored config " + f + " — invisible to every closure",
		})
	}
	if divergence {
		out = append(out, Advisory{
			Kind:    "env-suspected",
			Message: "same inputs, different output with no file cause — declare env vars in key_env",
		})
	}
	if !isGo && !writesActive {
		out = append(out, Advisory{
			Kind:    "writes-off",
			Message: "write-tracking inactive (scale) — mtime fallback in effect",
		})
	}
	return out
}

func isUntrackedConfigCandidate(wd, rel string) bool {
	for _, pat := range []string{"config.h", "config.status", "CMakeCache.txt", ".env"} {
		if rel == pat {
			return false
		}
	}
	if tracked := gitTrackedFiles(wd); tracked != nil {
		for _, t := range tracked {
			if t == rel {
				return false
			}
		}
	}
	base := filepath.Base(rel)
	for _, suffix := range []string{".conf", ".cfg", ".config", ".ini", ".env"} {
		if strings.HasSuffix(base, suffix) {
			return true
		}
	}
	return strings.HasPrefix(base, "config.") || strings.Contains(base, "config")
}

// discoverIgnoredConfigs: files in .gitignore that look like build
// configs. These are invisible to every closure — the most dangerous
// class of hidden input.
func discoverIgnoredConfigs(wd string) []string {
	var out []string
	data, err := os.ReadFile(filepath.Join(wd, ".gitignore"))
	if err != nil {
		return nil
	}
	tracked := gitTrackedFiles(wd)
	isTracked := func(f string) bool {
		for _, t := range tracked {
			if t == f {
				return true
			}
		}
		return false
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "!")
		line = strings.TrimSuffix(line, "/")
		if line == "" {
			continue
		}
		var matches []string
		if strings.ContainsAny(line, "*?[") {
			matches, _ = filepath.Glob(filepath.Join(wd, line))
		} else if _, err := os.Stat(filepath.Join(wd, line)); err == nil {
			matches = []string{filepath.Join(wd, line)}
		}
		for _, m := range matches {
			info, err := os.Stat(m)
			if err != nil || !info.Mode().IsRegular() {
				continue
			}
			rel, _ := filepath.Rel(wd, m)
			rel = filepath.ToSlash(rel)
			if isTracked(rel) {
				continue
			}
			base := filepath.Base(rel)
			configish := false
			for _, suffix := range []string{".h", ".conf", ".cfg", ".config", ".inc", ".flags", ".def"} {
				if strings.HasSuffix(base, suffix) {
					configish = true
					break
				}
			}
			if strings.HasPrefix(base, "config") || strings.Contains(base, "flags") || strings.Contains(base, "version") {
				configish = true
			}
			for _, excluded := range []string{"program", "main", "app", "test", "a.out", "brine"} {
				if base == excluded {
					configish = false
					break
				}
			}
			if configish {
				out = append(out, rel)
			}
		}
	}
	return out
}

// reportEffectiveConfig prints the non-default key configuration in
// effect, framed as what could be saved to .brine.json. Silent when
// everything is default (tier 0 — zero config, zero noise).
func reportEffectiveConfig(wd string) {
	cfg := loadConfig(wd)
	var lines []string
	var hint []string

	if len(cfg.KeyEnv) > 0 {
		envList := `"` + strings.Join(cfg.KeyEnv, `", "`) + `"`
		lines = append(lines, fmt.Sprintf("  key_env:   [%s]", envList))
	}
	if len(cfg.KeyFiles) > 0 {
		fileList := `"` + strings.Join(cfg.KeyFiles, `", "`) + `"`
		lines = append(lines, fmt.Sprintf("  key_files: [%s]", fileList))
	}
	if cfg.BuildVCS != "" {
		lines = append(lines, fmt.Sprintf("  buildvcs:  %q", cfg.BuildVCS))
	}
	if t := os.Getenv("BRINE_TRUST_LOCAL"); t == "1" {
		lines = append(lines, "  trust_local: true (env)")
	}

	// check for untracked configs the built-in scan catches but that
	// aren't in key_files (they're in the read-set, working, but the
	// user might not know)
	for _, cfgFile := range untrackedConfigs(wd) {
		alreadyKeyed := false
		for _, kf := range cfg.KeyFiles {
			if kf == cfgFile {
				alreadyKeyed = true
				break
			}
		}
		if !alreadyKeyed {
			hint = append(hint, fmt.Sprintf("  %q (untracked, auto-detected — add to key_files to make explicit)", cfgFile))
		}
	}

	// env vars currently set that are in KeyEnv (so the user sees the values)
	if len(cfg.KeyEnv) > 0 {
		for _, name := range cfg.KeyEnv {
			if v := os.Getenv(name); v != "" {
				hint = append(hint, fmt.Sprintf("  env %s=%q is active", name, v))
			}
		}
	}

	if len(lines) > 0 || len(hint) > 0 {
		fmt.Println("[brine] config:")
		for _, l := range lines {
			fmt.Println(l)
		}
		for _, h := range hint {
			fmt.Printf("  suggest: %s\n", h)
		}
		if _, err := os.Stat(filepath.Join(wd, ".brine.json")); os.IsNotExist(err) && len(lines) > 0 {
			fmt.Println("  persist: brine record <command>  (or copy the above into .brine.json)")
		}
	}
}
