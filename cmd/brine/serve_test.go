package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"testing"
)

func TestIsSHA256Key(t *testing.T) {
	cases := map[string]bool{
		"bbd289fab1da72f468ae53c4591860b3a3a7eca1":       false, // ccache sha1 (40 hex)
		"94/73c76354318a45b9c99451fac8e55044c8387b":      false, // ccache xx/ layout
		"6d121acbc2cf2dccbd6e2b304f60849dc905e3eff63e38d028cbaaea5eb9fe93": true, // sha256
		"":                     false,
		"short":                false,
		"6D121ACBC2CF2DCCBD6E2B304F60849DC905E3EFF63E38D028CBAAEA5EB9FE93": false, // uppercase
		"zz121acbc2cf2dccbd6e2b304f60849dc905e3eff63e38d028cbaaea5eb9fe93":  false, // non-hex
	}
	for key, want := range cases {
		if got := isSHA256Key(key); got != want {
			t.Errorf("isSHA256Key(%q) = %v, want %v", key, got, want)
		}
	}
}

func TestEnvelopeRoundTrip(t *testing.T) {
	for _, body := range [][]byte{
		[]byte("hello"),
		{},
		bytes.Repeat([]byte{0xff}, 1<<20),
	} {
		env := envelopeBody(body)
		got := verifyEnvelope(env)
		if !bytes.Equal(got, body) {
			t.Errorf("round trip failed for len=%d", len(body))
		}
	}
}

func TestVerifyEnvelopeRejectsCorruption(t *testing.T) {
	body := []byte("trust me")
	env := envelopeBody(body)

	// flip a byte in the body region
	corrupt := append([]byte{}, env...)
	corrupt[len(corrupt)-1] ^= 0xff
	if verifyEnvelope(corrupt) != nil {
		t.Error("corrupted envelope verified")
	}

	// flip a byte in the hash region
	corrupt = append([]byte{}, env...)
	corrupt[0] ^= 0xff
	if verifyEnvelope(corrupt) != nil {
		t.Error("tampered hash prefix verified")
	}

	// truncated below the hash prefix
	if verifyEnvelope(env[:31]) != nil {
		t.Error("truncated envelope verified")
	}

	// random garbage
	if verifyEnvelope([]byte("garbage that is longer than 32 bytes total")) != nil {
		t.Error("garbage verified")
	}
}

func TestEnvelopeBodiesUnverifiableAsContentAddressed(t *testing.T) {
	// A ccache entry stored with an envelope must never verify as if it were
	// raw content-addressed data: the envelope prefix makes it a different
	// blob, so even if an attacker PUTs an envelope at a sha256 key path,
	// handleGet's sha256 check rejects it.
	body := []byte("content")
	env := envelopeBody(body)
	sum := sha256.Sum256(env)
	if hex.EncodeToString(sum[:]) == hex.EncodeToString(envelopeHash(body)) {
		t.Error("envelope body collides with plain body hash")
	}
}

func envelopeHash(body []byte) []byte {
	sum := sha256.Sum256(body)
	return sum[:]
}
