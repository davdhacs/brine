package store

import (
	"crypto/sha256"
	"encoding/hex"
)

func sha256Sum(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func hashOf(b []byte) string { return sha256Sum(b) }

func keysOf(m map[string][]byte) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}
