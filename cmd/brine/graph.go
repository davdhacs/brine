// Cross-command dependency graph: derives build-step dependencies from
// receipts (what each command reads and writes) without requiring the
// user to declare them. This is the data-flow equivalent of a CI
// `needs:` chain, derived from observation.
//
// If command A produces libfoo.a and command B reads libfoo.a, then
// B depends on A. When A's receipt is invalidated (rebuilt), B's
// receipt is invalidated too — its input changed even though B's own
// git-tracked closure didn't.
//
// Proactive invalidation: when A rebuilds, B's receipt is REMOVED from
// the store. B's next run is a guaranteed MISS, not a potentially-
// stale HIT that happens to be caught by the input-hash check. This
// is the difference between "warns about the problem" and "prevents
// the problem".
//
// Cascade: A → B → C chains propagate. Invalidating A invalidates B,
// which invalidates C. The transitive closure is computed over the
// edge list.
//
// The graph lives in the .brine store as a simple edge list, rebuilt
// from receipts on demand. No persistence beyond the receipts
// themselves — the graph IS the receipts, connected.
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// DepEdge: command A produces a file that command B reads.
type DepEdge struct {
	Producer string `json:"producer"`
	Consumer string `json:"consumer"`
	File     string `json:"file"`
}

// BuildCommandGraph derives the cross-command dependency graph from
// all stored receipts.
func BuildCommandGraph(st *StoreAdapter) []DepEdge {
	var edges []DepEdge
	manifests := st.AllManifests()

	// output-path → producing-command
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

	// consumers: receipts whose read_set includes a producer's output
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

// TransitiveDependents returns ALL commands that transitively depend
// on the given command (A → B → C: invalidating A invalidates B and C).
func TransitiveDependents(edges []DepEdge, rebuilt string) map[string]bool {
	// BFS over consumer edges
	direct := map[string]map[string]bool{} // producer → set of consumers
	for _, e := range edges {
		if direct[e.Producer] == nil {
			direct[e.Producer] = map[string]bool{}
		}
		direct[e.Producer][e.Consumer] = true
	}

	affected := map[string]bool{}
	queue := []string{rebuilt}
	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]
		for consumer := range direct[current] {
			if !affected[consumer] {
				affected[consumer] = true
				queue = append(queue, consumer)
			}
		}
	}
	return affected
}

// InvalidateDependents proactively removes the receipts of all
// transitive dependents. Returns the commands whose receipts were
// removed.
func InvalidateDependents(st *StoreAdapter, edges []DepEdge, rebuilt string) []string {
	affected := TransitiveDependents(edges, rebuilt)
	if len(affected) == 0 {
		return nil
	}

	var invalidated []string
	for cmd := range affected {
		if st.RemoveManifestsForCommand(cmd) {
			invalidated = append(invalidated, cmd)
		}
	}
	sort.Strings(invalidated)
	return invalidated
}

// PrintGraph renders the graph for diagnostics.
func PrintGraph(edges []DepEdge) {
	if len(edges) == 0 {
		fmt.Println("[brine] graph: no cross-command dependencies found")
		return
	}
	fmt.Printf("[brine] graph: %d cross-command dependencies:\n", len(edges))
	for _, e := range edges {
		fmt.Printf("[brine]   %s → %s (via %s)\n",
			shortCmd(e.Producer), shortCmd(e.Consumer), e.File)
	}
}

func shortCmd(cmd string) string {
	parts := strings.Fields(cmd)
	if len(parts) == 0 {
		return cmd
	}
	return filepath.Base(parts[0]) + " " + strings.Join(parts[1:], " ")
}

// StoreAdapter wraps the store for the graph's needs (avoids import
// cycles with the store package).
type StoreAdapter struct {
	root string // .brine directory
}

func NewStoreAdapter(wd string) *StoreAdapter {
	return &StoreAdapter{root: filepath.Join(wd, ".brine")}
}

func (s *StoreAdapter) AllManifests() []map[string]interface{} {
	var out []map[string]interface{}
	manifestDir := filepath.Join(s.root, "manifests")
	entries, _ := os.ReadDir(manifestDir)
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(manifestDir, e.Name()))
		if err != nil {
			continue
		}
		var m map[string]interface{}
		if json.Unmarshal(data, &m) == nil && m["command"] != nil {
			out = append(out, m)
		}
	}
	return out
}

// RemoveManifestsForCommand deletes all manifests whose command matches.
// Returns true if any were removed.
func (s *StoreAdapter) RemoveManifestsForCommand(command string) bool {
	removed := false
	manifestDir := filepath.Join(s.root, "manifests")
	entries, _ := os.ReadDir(manifestDir)
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		p := filepath.Join(manifestDir, e.Name())
		data, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		var m map[string]interface{}
		if json.Unmarshal(data, &m) == nil {
			if cmd, _ := m["command"].(string); cmd == command {
				os.Remove(p)
				removed = true
			}
		}
	}
	return removed
}

func mustWd() string {
	wd, _ := os.Getwd()
	return wd
}
