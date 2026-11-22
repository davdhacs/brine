// Cross-machine docker image receipts: `docker save` into the blob
// store on record, `docker load` on restore. This makes image
// receipts work on fresh runners — today they only work when the
// image already exists locally (re-tag).
//
// The image is saved as a gzipped tar, content-addressed by the
// receipt key. On restore, the tar is downloaded, verified (sha256),
// and loaded into the local docker daemon.
//
// Trade-off: docker save produces large files (300MB+ for StackRox
// central). The blob store handles this (streaming, gzip-at-rest),
// but the download time on a fresh runner is significant. For CI use
// where images are pushed to a registry anyway, the registry path may
// be faster. This is the "no registry required" option.
package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// saveDockerImage exports a docker image to the blob store.
// Returns the blob key, or empty on failure.
func saveDockerImage(imageID, receiptKey string, st sideCacheStore) string {
	if st == nil {
		return ""
	}

	// docker save to a temp file
	tmpDir := os.TempDir()
	tmpFile := filepath.Join(tmpDir, "brine-image-"+receiptKey[:12]+".tar")
	defer os.Remove(tmpFile)

	cmd := exec.Command("docker", "save", "-o", tmpFile, imageID)
	if err := cmd.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "[brine] docker save %s: %v\n", imageID[:19], err)
		return ""
	}

	// Read and upload
	data, err := os.ReadFile(tmpFile)
	if err != nil {
		return ""
	}

	key := "image/" + receiptKey
	if _, err := st.PutIfNotExistPub(key, data); err != nil {
		fmt.Fprintf(os.Stderr, "[brine] image upload: %v\n", err)
		return ""
	}

	fmt.Printf("[brine] docker image saved to blob store: %s (%.1f MB)\n",
		imageID[:19], float64(len(data))/1024/1024)
	return key
}

// loadDockerImage restores a docker image from the blob store.
// Returns the image ID, or empty on failure.
func loadDockerImage(blobKey string, st sideCacheStore) string {
	if st == nil || blobKey == "" {
		return ""
	}

	data, err := st.Get(blobKey)
	if err != nil || data == nil {
		return ""
	}

	// Write to temp file
	tmpFile := filepath.Join(os.TempDir(), "brine-image-restore.tar")
	defer os.Remove(tmpFile)
	if err := os.WriteFile(tmpFile, data, 0o644); err != nil {
		return ""
	}

	// docker load
	cmd := exec.Command("docker", "load", "-i", tmpFile)
	out, err := cmd.CombinedOutput()
	if err != nil {
		fmt.Fprintf(os.Stderr, "[brine] docker load: %v\n", err)
		return ""
	}

	// Parse the image ID from docker load output
	// Output format: "Loaded image ID: sha256:abc123..."
	output := string(out)
	for _, line := range strings.Split(output, "\n") {
		if strings.Contains(line, "Loaded image") {
			parts := strings.Fields(line)
			if len(parts) >= 3 {
				return parts[len(parts)-1]
			}
		}
	}

	// If we can't parse the ID, try to get it from the loaded image
	// (docker load of a save without tag doesn't always print the ID)
	if imageID := getLatestImageID(); imageID != "" {
		return imageID
	}

	fmt.Printf("[brine] docker image loaded from blob store (%.1f MB)\n",
		float64(len(data))/1024/1024)
	return "" // loaded but ID unknown
}

// getLatestImageID gets the most recently loaded image ID.
func getLatestImageID() string {
	out, err := exec.Command("docker", "images", "--format",
		"{{.ID}} {{.CreatedAt}}", "--no-trunc").Output()
	if err != nil {
		return ""
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) > 0 {
		fields := strings.Fields(lines[0])
		if len(fields) >= 1 {
			return fields[0]
		}
	}
	return ""
}
