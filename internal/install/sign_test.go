package install

import (
	"crypto/ed25519"
	"encoding/base64"
	"testing"
)

func TestVerifyManifest(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	pubB64 := base64.StdEncoding.EncodeToString(pub)
	manifest := []byte(`{"backends":{"llamacpp":{"version":"b1"}}}`)
	sigB64 := base64.StdEncoding.EncodeToString(ed25519.Sign(priv, manifest))

	if err := VerifyManifest(manifest, sigB64, pubB64); err != nil {
		t.Fatalf("valid signature should verify: %v", err)
	}

	// Tampered manifest → fail.
	if err := VerifyManifest([]byte(`{"backends":{}}`), sigB64, pubB64); err == nil {
		t.Fatal("tampered manifest must fail verification")
	}

	// Wrong key → fail.
	otherPub, _, _ := ed25519.GenerateKey(nil)
	if err := VerifyManifest(manifest, sigB64, base64.StdEncoding.EncodeToString(otherPub)); err == nil {
		t.Fatal("wrong public key must fail verification")
	}

	// Malformed inputs → error, not panic.
	if err := VerifyManifest(manifest, "not-base64!!", pubB64); err == nil {
		t.Fatal("bad signature encoding should error")
	}
	if err := VerifyManifest(manifest, sigB64, "short"); err == nil {
		t.Fatal("bad public key should error")
	}
}

func TestVerifyManifestFileNoKeySkips(t *testing.T) {
	// No public key configured → verification is skipped (returns nil).
	if err := verifyManifestFile("", "", ""); err != nil {
		t.Fatalf("no key should skip verification, got %v", err)
	}
}

func TestVerifyManifestFileKeyButNoManifest(t *testing.T) {
	pub, _, _ := ed25519.GenerateKey(nil)
	err := verifyManifestFile("", "", base64.StdEncoding.EncodeToString(pub))
	if err == nil {
		t.Fatal("public key set but no manifest file must error")
	}
}
