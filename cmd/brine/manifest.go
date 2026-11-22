// Multi-arch manifest assembly: combine per-architecture image
// receipts into a single docker manifest that works on any platform.
//
// The CA branch had this via parallel per-arch jobs. brine's version:
//
//  1. Each arch builds independently (own runner, own receipt):
//     brine docker build --platform linux/amd64 -t app:amd64 .
//     brine docker build --platform linux/arm64 -t app:arm64 .
//
//  2. Assemble into a multi-arch manifest:
//     brine manifest create app:latest app:amd64 app:arm64
//
//  3. The manifest is content-addressed by the image digests —
//     same digests = same manifest = same path. Adding an arch or
//     changing an image creates a new manifest at a new path.
//
// The manifest receipt key is the hash of all image digests, so:
//   - Same images → same manifest (idempotent)
//   - Any image changes → new manifest (no overwrite possible)
//   - Removing an arch → new manifest (the old one still exists)
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
)

// ArchImage is one architecture's contribution to a manifest.
type ArchImage struct {
	Platform string `json:"platform"` // "linux/amd64", "linux/arm64"
	Image    string `json:"image"`    // image reference or digest
	Digest   string `json:"digest"`   // sha256 of the image
}

// ManifestReceipt records a multi-arch manifest assembly.
type ManifestReceipt struct {
	Tag    string      `json:"tag"`
	Images []ArchImage `json:"images"`
	Key    string      `json:"key"` // hash of sorted digests
}

// cmdManifestCreate assembles a multi-arch manifest.
func cmdManifestCreate(args []string) {
	if len(args) < 2 {
		fatal("manifest create: usage: brine manifest create <tag> <image1> <image2> ...")
	}

	tag := args[0]
	images := args[1:]
	wd, _ := os.Getwd()

	// Resolve each image to its digest
	var archImages []ArchImage
	for _, img := range images {
		digest, err := dockerInspectDigest(img)
		if err != nil {
			fatal("manifest: cannot resolve %s: %v", img, err)
		}
		platform, _ := dockerInspectPlatform(img)
		archImages = append(archImages, ArchImage{
			Platform: platform,
			Image:    img,
			Digest:   digest,
		})
	}

	// Compute the manifest key from the sorted digests
	key := manifestKey(archImages)

	// Check if we already have this manifest
	if _, err := os.Stat(manifestReceiptPath(wd, key)); err == nil {
		fmt.Printf("[brine] manifest: %s already assembled (key %s)\n", tag, key[:12])
		return
	}

	// Create the docker manifest
	dockerArgs := append([]string{"manifest", "create", tag}, images...)
	cmd := exec.Command("docker", dockerArgs...)
	if out, err := cmd.CombinedOutput(); err != nil {
		fatal("docker manifest create: %v\n%s", err, out)
	}

	// Optionally annotate platforms
	for _, ai := range archImages {
		if ai.Platform != "" {
			annotateCmd := exec.Command("docker", "manifest", "annotate", tag, ai.Image,
				"--os", strings.Split(ai.Platform, "/")[0],
				"--arch", strings.Split(ai.Platform, "/")[1])
			annotateCmd.Run() // best-effort
		}
	}

	// Push the manifest
	pushCmd := exec.Command("docker", "manifest", "push", tag)
	if out, err := pushCmd.CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "[brine] manifest push: %v\n%s", err, out)
		// Not fatal — the local manifest exists even if push fails
	} else {
		fmt.Printf("[brine] manifest: pushed %s\n", tag)
	}

	// Record the receipt
	receipt := ManifestReceipt{
		Tag:    tag,
		Images: archImages,
		Key:    key,
	}
	if err := saveManifestReceipt(wd, &receipt); err != nil {
		fmt.Fprintf(os.Stderr, "[brine] manifest receipt save: %v\n", err)
	}

	fmt.Printf("[brine] manifest: %s assembled from %d images (key %s)\n",
		tag, len(archImages), key[:12])
	for _, ai := range archImages {
		fmt.Printf("[brine]   %s: %s (%s)\n", ai.Platform, ai.Image, ai.Digest[:19])
	}
}

// manifestKey computes the content-addressed key from sorted image digests.
func manifestKey(images []ArchImage) string {
	h := sha256.New()
	var digests []string
	for _, ai := range images {
		digests = append(digests, ai.Digest)
	}
	sort.Strings(digests)
	for _, d := range digests {
		h.Write([]byte(d))
		h.Write([]byte("\n"))
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}

// manifestReceiptPath returns the on-disk path for a manifest receipt.
func manifestReceiptPath(wd, key string) string {
	return wd + "/.brine/manifests-receipts/" + key + ".json"
}

// saveManifestReceipt writes a manifest receipt to disk.
func saveManifestReceipt(wd string, r *ManifestReceipt) error {
	os.MkdirAll(filepath.Dir(manifestReceiptPath(wd, r.Key)), 0o755)
	data, _ := json.MarshalIndent(r, "", "  ")
	return os.WriteFile(manifestReceiptPath(wd, r.Key), data, 0o644)
}

// dockerInspectDigest gets the RepoDigest of an image.
func dockerInspectDigest(image string) (string, error) {
	out, err := exec.Command("docker", "manifest", "inspect", "--verbose", image).Output()
	if err != nil {
		// fall back to local inspect
		out, err = exec.Command("docker", "inspect", "--format",
			"{{index .RepoDigests 0}}", image).Output()
		if err != nil {
			return "", fmt.Errorf("inspect %s: %w", image, err)
		}
		return strings.TrimSpace(string(out)), nil
	}
	// parse the manifest inspect output for the digest
	for _, line := range strings.Split(string(out), "\n") {
		if strings.Contains(line, "\"Digest\"") {
			parts := strings.Split(line, ":")
			if len(parts) >= 2 {
				d := strings.Trim(strings.TrimSpace(parts[len(parts)-1]), "\",")
				if strings.HasPrefix(d, "sha256:") {
					return d, nil
				}
			}
		}
	}
	return "", fmt.Errorf("digest not found in manifest inspect output")
}

// dockerInspectPlatform gets the platform of an image.
func dockerInspectPlatform(image string) (string, error) {
	out, err := exec.Command("docker", "inspect", "--format",
		"{{.Os}}/{{.Architecture}}", image).Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}
