// Cross-command dependency graph: derives build-step dependencies from
// receipts (what each command reads and writes) without requiring the
// user to declare them. This is the data-flow equivalent of a CI
// `needs:` chain, derived from observation.
//
// If command A produces libfoo.a and command B reads libfoo.a, then
// B depends on A. When A's receipt is invalidated (rebuilt), B's
// receipt should be invalidated too — its input changed even though
// B's own closure (git-tracked files) didn't.
//
// The graph lives in the .cimemo store as a simple edge list, rebuilt
// from receipts on demand. No persistence beyond the receipts
// themselves — the graph IS the receipts, connected.
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// DepEdge: command A produces a file that command B reads.
type DepEdge struct {
	Producer string // command that writes the file
	Consumer string // command that reads the file
	File     string // the file that connects them
}

// BuildCommandGraph derives the cross-command dependency graph from
// all stored receipts. A receipt's Blobs are its outputs; another
// receipt's ReadSet entries that match those output paths are the
// edges.
func BuildCommandGraph(st interface {
	AllManifests() []map[string]interface{}
}) []DepEdge {
	var edges []DepEdge
	manifests := st.AllManifests()

	// Build output-path → producing-command map.
	outputProducers := map[string]string{}
	for _, m := range manifests {
		cmd, _ := m["command"].(string)
		if cmd == "" {
			continue
		}
		blobs, _ := m["blobs"].([]interface{})
		for _, b := range blobs {
			if blob, ok := b.(map[string]interface{}); ok {
				if path, ok := blob["path"].(string); ok {
					outputProducers[path] = cmd
				}
			}
		}
	}

	// Find consumers: receipts whose ReadSet includes a producer's output.
	for _, m := range manifests {
		consumer, _ := m["command"].(string)
		if consumer == "" {
			continue
		}
		readSet, _ := m["read_set"].([]interface{})
		for _, r := range readSet {
			if rel, ok := r.(string); ok {
				if producer, exists := outputProducers[rel]; exists && producer != consumer {
					edges = append(edges, DepEdge{
						Producer: producer,
						Consumer: consumer,
						File:     rel,
					})
				}
			}
		}
	}

	sort.Slice(edges, func(i, j int) bool {
		if edges[i].Producer != edges[j].Producer {
			return edges[i].Producer < edges[j].Producer
		}
		return edges[i].Consumer < edges[j].Consumer
	})
	return edges
}

// InvalidateDependents finds commands that consume any of the changed
// producer's outputs and whose receipts should be invalidated.
// Returns the commands to invalidate.
func InvalidateDependents(edges []DepEdge, rebuiltCommand string) []string {
	var consumers []string
	seen := map[string]bool{}
	for _, e := range edges {
		if e.Producer == rebuiltCommand && !seen[e.Consumer] {
			seen[e.Consumer] = true
			consumers = append(consumers, e.Consumer)
		}
	}
	sort.Strings(consumers)
	return consumers
}

// PrintGraph renders the graph for diagnostics.
func PrintGraph(edges []DepEdge) {
	if len(edges) == 0 {
		fmt.Println("[cimemo] graph: no cross-command dependencies found")
		return
	}
	fmt.Printf("[cimemo] graph: %d cross-command dependencies:\n", len(edges))
	for _, e := range edges {
		fmt.Printf("[cimemo]   %s → %s (via %s)\n",
			shortCmd(e.Producer), shortCmd(e.Consumer), e.File)
	}
}

func shortCmd(cmd string) string {
	// strip path prefixes for readability
	parts := strings.Fields(cmd)
	if len(parts) == 0 {
		return cmd
	}
	return filepath.Base(parts[0]) + " " + strings.Join(parts[1:], " ")
}

func mustWd() string {
	wd, _ := os.Getwd()
	return wd
}
