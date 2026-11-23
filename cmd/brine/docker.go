// Docker image receipts: `brine docker build -t name:tag .` memoized
// at the IMAGE level. The receipt records the built image ID; a hit
// re-tags it instead of rebuilding. This is the CA branch's proven
// fast path (receipt → re-tag), generalized.
//
// Scope: local-machine receipts (image must exist locally to re-tag).
// Cross-machine: `docker save` the image into the blob store on
// record, `docker load` on restore — the CA work's docker layer cache
// (gocache-serve docker-push/pull) is the ported form of exactly that.
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/davdhacs/brine/key"
)

// dockerBuildTags extracts -t/--tag values from a docker/buildx build
// command line.
func dockerBuildTags(args []string) []string {
	var tags []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "-t" || a == "--tag" {
			if i+1 < len(args) {
				tags = append(tags, args[i+1])
				i++
			}
			continue
		}
		if strings.HasPrefix(a, "-t=") || strings.HasPrefix(a, "--tag=") {
			tags = append(tags, strings.SplitN(a, "=", 2)[1])
		}
	}
	return tags
}

// isDockerBuild reports whether args is a docker (or buildx) build.
func isDockerBuild(args []string) bool {
	if len(args) < 3 {
		return false
	}
	switch args[0] {
	case "docker":
		return args[1] == "build" || (args[1] == "buildx" && args[2] == "build")
	case "buildx":
		return args[1] == "build"
	}
	return false
}

// dockerImageID returns the ID of the most recently relevant image for
// a tag (digest if available).
func dockerImageID(tag string) (string, error) {
	out, err := exec.Command("docker", "inspect", "--format", "{{.Id}}", tag).Output()
	if err != nil {
		return "", fmt.Errorf("docker inspect %s: %w", tag, err)
	}
	return strings.TrimSpace(string(out)), nil
}

// dockerRetag re-tags a stored image ID with the given tag.
func dockerRetag(imageID, tag string) error {
	return exec.Command("docker", "tag", imageID, tag).Run()
}

// dockerContextClosure hashes the build context directory (every
// regular file, bounded). For image builds the context IS the input —
// including files that are outputs of previous commands (binaries
// COPYied into the image are inputs to THIS command). Git-tracked
// fallback is unsound here: untracked context files would be invisible.
func dockerContextClosure(wd string, args []string) []string {
	// find the context arg: last non-flag arg of the docker build cmd
	ctx := "."
	for i := len(args) - 1; i >= 2; i-- {
		a := args[i]
		if strings.HasPrefix(a, "-") {
			continue
		}
		ctx = a
		break
	}
	var files []string
	filepath.WalkDir(filepath.Join(wd, ctx), func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		rel, rerr := filepath.Rel(wd, path)
		if rerr != nil {
			return nil
		}
		rel = filepath.ToSlash(rel)
		// Skip brine's own cache (self-referential: every run rewrites
		// manifests — permanent misses) and .git (stable but huge).
		// MUST run before the regular-file filter — dirs hit this path.
		if rel == ".brine" || rel == ".git" {
			return filepath.SkipDir
		}
		if !d.Type().IsRegular() {
			return nil
		}
		files = append(files, rel)
		return nil
	})
	sort.Strings(files)
	if len(files) > 20000 {
		files = files[:20000] // bound; dockerignore handling is future work
	}
	return files
}

// commitReceiptKey derives the git-commit fast-path key: a clean tree
// at HEAD is EXACTLY determined (SHA → tree → contents), so the
// content closure is implied without hashing files or running
// `go list`. Sound only with a clean tree — callers must verify.
func commitReceiptKey(wd, keyCommand, toolchain string) (string, bool) {
	out, err := exec.Command("git", "-C", wd, "-c", "safe.directory="+wd, "rev-parse", "HEAD").Output()
	if err != nil {
		return "", false
	}
	head := strings.TrimSpace(string(out))
	// clean tree? (uncommitted changes fall back to the content path)
	if err := exec.Command("git", "-C", wd, "-c", "safe.directory="+wd,
		"diff-index", "--quiet", "HEAD", "--").Run(); err != nil {
		return "", false
	}
	h := sha256.Sum256([]byte(head + "\x00" + keyCommand + "\x00" + toolchain))
	return hex.EncodeToString(h[:])[:16], true
}

// dockerBaseDigests resolves the CURRENT registry digest of every FROM
// image. Registry-current (not locally-cached) so a moved base tag
// changes the key and triggers a rebuild — the "check for updates".
// Unpinned tags (alpine:3) are also AUDIT-reported: the Dockerfile is
// nondeterministic config; pinning with @sha256 makes it exact.
func dockerBaseDigests(wd string, args []string) []string {
	dockerfile := "Dockerfile"
	for i := 0; i < len(args); i++ {
		if args[i] == "-f" || args[i] == "--file" {
			if i+1 < len(args) {
				dockerfile = args[i+1]
			}
		}
	}
	data, err := os.ReadFile(filepath.Join(wd, dockerfile))
	if err != nil {
		return nil
	}
	var out []string
	for _, line := range strings.Split(string(data), "\n") {
		f := strings.Fields(line)
		if len(f) >= 2 && strings.EqualFold(f[0], "FROM") {
			image := f[1]
			if strings.Contains(image, "@sha256:") {
				out = append(out, image) // digest-pinned: exact config
				continue
			}
			digest := registryDigest(image)
			if digest == "" {
				// registry unreachable: fall back to the locally-cached
				// digest (protects local drift; blind to registry moves)
				digest = localDigest(image)
				if digest == "" {
					digest = image
				}
			}
			fmt.Printf("[brine] AUDIT docker-base-unpinned: %s resolves to %s — mutable tag; pin with @%s for deterministic config\n",
				image, digest, digest)
			out = append(out, digest)
		}
	}
	return out
}

// registryDigest resolves the CURRENT digest of a tag from the
// registry (docker manifest inspect --verbose).
func registryDigest(image string) string {
	out, err := exec.Command("docker", "manifest", "inspect", "--verbose", image).Output()
	if err != nil {
		return ""
	}
	var m struct {
		Descriptor struct {
			Digest string `json:"digest"`
		} `json:"Descriptor"`
	}
	if json.Unmarshal(out, &m) != nil || m.Descriptor.Digest == "" {
		return ""
	}
	return image + "@" + m.Descriptor.Digest
}

// localDigest returns the digest of the locally-cached image.
func localDigest(image string) string {
	out, err := exec.Command("docker", "inspect", "--format", "{{index .RepoDigests 0}}", image).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// keyTail folds every INPUT CLASS INVISIBLE TO GIT-BASED KEYS into one
// key fragment:
//
//  1. env vars named in BRINE_KEY_ENV ("CFLAGS,CXXFLAGS,...") — env
//     changes output without changing any file
//  2. files matching BRINE_KEY_FILES globs — user escape hatch
//  3. UNTRACKED CONFIG ARTIFACTS (config.h written by ./configure,
//     CMakeCache.txt outside build dirs, local.mk, .env) — autotools
//     convention puts build config in untracked source-tree files;
//     git-based keys and commit receipts are blind to them (a clean
//     tree at HEAD says nothing about config.h)
//
// Each found config also AUDIT-warns: it is keyed, but it is not
// shared (uncommitted) — pin it for team-wide determinism.

// dockerExecContainer extracts the container name from a `docker exec`
// command line, or "" if the command isn't docker exec.
func dockerExecContainer(args []string) string {
	if len(args) < 3 || args[0] != "docker" || args[1] != "exec" {
		return ""
	}
	// flags that consume the following argument
	valueFlags := map[string]bool{
		"-e": true, "--env": true, "-u": true, "--user": true,
		"-w": true, "--workdir": true, "--env-file": true,
	}
	for i := 2; i < len(args); i++ {
		a := args[i]
		if !strings.HasPrefix(a, "-") {
			return a // first non-flag argument = container name
		}
		if strings.Contains(a, "=") {
			continue // --env=VAL form: self-contained
		}
		if valueFlags[a] {
			i++ // skip the consumed value
		}
	}
	return ""
}

// dockerExecKeyExtras folds the exec container's environment identity
// into the receipt key: image digest (the content address for the
// entire toolchain — compilers, headers, sysroot — one hash instead
// of hashing the image contents), the mount table (host↔container
// mapping), and run-time container env (sorted). `docker exec` is
// otherwise opaque to observation: the build reads mounted sources
// and image files inside the container, invisible to host-side
// sensors. Not cached: a build that recreates its own container must
// re-resolve at key time.
func dockerExecKeyExtras(args []string) string {
	container := dockerExecContainer(args)
	if container == "" {
		return ""
	}
	out, err := exec.Command("docker", "inspect",
		"--format", "{{.Image}}\n{{json .Config.Env}}\n{{json .Mounts}}",
		container).Output()
	if err != nil {
		// Container missing or docker unavailable: the exec itself
		// will fail in that case — nothing sound to fold here.
		return ""
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) != 3 {
		return ""
	}
	image := strings.TrimSpace(lines[0])
	envHash := sha256Hex(normalizeJSONList(lines[1]))
	mountHash := sha256Hex(normalizeMounts(lines[2]))
	if image == "" {
		return ""
	}
	return "execenv:image=" + image + ",env=" + envHash[:16] + ",mounts=" + mountHash[:16]
}

// normalizeJSONList parses a JSON array, sorts its elements, and
// joins them — an order-insensitive digest input.
func normalizeJSONList(s string) string {
	var arr []interface{}
	if json.Unmarshal([]byte(s), &arr) != nil {
		return s
	}
	parts := make([]string, 0, len(arr))
	for _, el := range arr {
		b, _ := json.Marshal(el)
		parts = append(parts, string(b))
	}
	sort.Strings(parts)
	return strings.Join(parts, "\x00")
}

// normalizeMounts parses `docker inspect .Mounts`, drops host-side
// Source paths (machine-specific; content identity lives in the file
// hashes), keeps destination/type/mode/RW, sorts by destination.
// Dropping Source makes receipts shareable across workers that keep
// the repo at different paths.
func normalizeMounts(s string) string {
	var mounts []map[string]interface{}
	if json.Unmarshal([]byte(s), &mounts) != nil {
		return s
	}
	type mtab struct {
		dest, typ, mode string
		rw              bool
	}
	var tabs []mtab
	for _, m := range mounts {
		rw := false
		if v, ok := m["RW"].(bool); ok {
			rw = v
		}
		tabs = append(tabs, mtab{
			dest: fmt.Sprint(m["Destination"]),
			typ:  fmt.Sprint(m["Type"]),
			mode: fmt.Sprint(m["Mode"]),
			rw:   rw,
		})
	}
	sort.Slice(tabs, func(i, j int) bool { return tabs[i].dest < tabs[j].dest })
	var b strings.Builder
	for _, t := range tabs {
		fmt.Fprintf(&b, "%s:%s:%s:rw=%v\n", t.dest, t.typ, t.mode, t.rw)
	}
	return b.String()
}

func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func keyTail(wd string, args []string) string {
	var parts []string

	cfg := loadConfig(wd)
	for _, name := range cfg.KeyEnv {
		parts = append(parts, "env:"+name+"="+os.Getenv(name))
	}

	for _, g := range cfg.KeyFiles {
		if files, _ := filepath.Glob(filepath.Join(wd, g)); len(files) > 0 {
			for _, f := range files {
				if v, ok := key.HashInput(wd, filepath.Base(f)); ok {
					parts = append(parts, "file:"+filepath.Base(f)+"="+v)
				}
			}
		}
	}

	// Docker exec: the container's image digest, mount table, and env
	// are invisible inputs — observation cannot see inside the
	// container, and the git-tracked closure does not cover the image.
	if ex := dockerExecKeyExtras(args); ex != "" {
		parts = append(parts, ex)
	}

	// Declared image refs (docker run/build shapes the command parser
	// doesn't cover): resolved to local digests at key time.
	for _, ref := range cfg.KeyImages {
		if d := localDigest(ref); d != "" {
			parts = append(parts, "image:"+ref+"="+d)
		} else {
			// Unresolvable image in key_images: fold the ref itself so
			// the key still changes if the config changes.
			parts = append(parts, "image:"+ref+"=unresolved")
		}
	}

	// Output scope is part of the receipt's identity: same inputs with
	// different output_patterns produce different manifests — without
	// this they collide under one key and the divergence detector
	// reports a false positive.
	if len(cfg.OutputPatterns) > 0 {
		parts = append(parts, "outscope:"+strings.Join(cfg.OutputPatterns, "\x00"))
	}

	// Untracked-config patterns are NOT keyed here: a config the build
	// GENERATES is an output (keying it conflicts — the stale-serve
	// found in the generated-config case); a config written OUTSIDE
	// brine belongs to the READ-SET, where the writes filter
	// disambiguates. Env and user globs stay keyed: env influences
	// builds through no file any sensor can observe.

	if len(parts) == 0 {
		return ""
	}
	return "|tail:" + strings.Join(parts, "\x00")
}
