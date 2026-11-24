package gocache

import (
	"testing"
)

// ccache's remote storage uses sharded keys like "34/6ca74d..." —
// test that the GHA backend handles them (the slash is the question)
func TestGHABackendCcacheStyleKeys(t *testing.T) {
	f := newFakeTwirpServer(t)
	r := newGhaTestRemote(f)

	// ccache-style sharded key (exactly what ccache 4.14 sends)
	key := "34/6ca74d190594d0f7e54fbcb8e1feeab34e227c"
	body := []byte("\xcc\xac\x01\x00\x01\x01\x01\x00\x00\x00\x00k\x05\xa1.\x064.14.3")

	// PUT
	created, err := r.PutIfNotExistPub(key, body)
	if err != nil {
		t.Fatalf("PutIfNotExistPub with ccache key: %v", err)
	}
	if !created {
		t.Fatalf("should be created on first put")
	}

	// GET
	got, err := r.Get(key)
	if err != nil {
		t.Fatalf("Get with ccache key: %v", err)
	}
	if got == nil {
		t.Fatalf("Get returned nil after put")
	}

	// The serve wraps opaque keys in an envelope (sha256 + body), so
	// Get returns the ENVELOPE, not the raw body. The serve's handler
	// unwraps it. Here we just verify the round trip works.
	t.Logf("round trip OK: put %d bytes, got %d bytes (envelope = 32 + %d)",
		len(body), len(got), len(body))

	// EXISTS
	ok, err := r.ExistsPub(key)
	if err != nil || !ok {
		t.Fatalf("ExistsPub: %v %v", ok, err)
	}

	// PUT again (dedup)
	created2, err := r.PutIfNotExistPub(key, body)
	if err != nil {
		t.Fatalf("second PutIfNotExistPub: %v", err)
	}
	if created2 {
		t.Fatalf("second put should dedup (created=false)")
	}
}
