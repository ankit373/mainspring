package install

import (
	"crypto/ed25519"
	"encoding/base64"
	"fmt"
	"os"
)

// VerifyManifest checks a detached ed25519 signature over the manifest bytes
// against a base64-encoded public key. Both sig and pubKey are base64 (standard
// encoding). This is the trust root for a curated manifest: if it doesn't
// verify, the URLs/digests inside it must not be trusted.
func VerifyManifest(manifest []byte, sigB64, pubKeyB64 string) error {
	pub, err := base64.StdEncoding.DecodeString(pubKeyB64)
	if err != nil {
		return fmt.Errorf("decode public key: %w", err)
	}
	if len(pub) != ed25519.PublicKeySize {
		return fmt.Errorf("invalid ed25519 public key size %d (want %d)", len(pub), ed25519.PublicKeySize)
	}
	sig, err := base64.StdEncoding.DecodeString(sigB64)
	if err != nil {
		return fmt.Errorf("decode signature: %w", err)
	}
	if len(sig) != ed25519.SignatureSize {
		return fmt.Errorf("invalid ed25519 signature size %d (want %d)", len(sig), ed25519.SignatureSize)
	}
	if !ed25519.Verify(ed25519.PublicKey(pub), manifest, sig) {
		return fmt.Errorf("manifest signature verification failed")
	}
	return nil
}

// verifyManifestFile enforces signature checking for an install when a public
// key is configured: the override manifest at manifestPath must carry a valid
// detached signature (sigPath, defaulting to manifestPath+".sig").
func verifyManifestFile(manifestPath, sigPath, pubKeyB64 string) error {
	if pubKeyB64 == "" {
		return nil // no key configured → signing not required
	}
	if manifestPath == "" {
		return fmt.Errorf("manifest signature required (public key configured) but no --manifest file given")
	}
	if sigPath == "" {
		sigPath = manifestPath + ".sig"
	}
	manifest, err := os.ReadFile(manifestPath)
	if err != nil {
		return fmt.Errorf("read manifest for verification: %w", err)
	}
	sigB64, err := os.ReadFile(sigPath)
	if err != nil {
		return fmt.Errorf("read manifest signature %s: %w", sigPath, err)
	}
	return VerifyManifest(manifest, trimSpace(string(sigB64)), pubKeyB64)
}

func trimSpace(s string) string {
	start, end := 0, len(s)
	for start < end && (s[start] == ' ' || s[start] == '\n' || s[start] == '\r' || s[start] == '\t') {
		start++
	}
	for end > start && (s[end-1] == ' ' || s[end-1] == '\n' || s[end-1] == '\r' || s[end-1] == '\t') {
		end--
	}
	return s[start:end]
}
