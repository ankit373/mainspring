package install

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
)

// withHome points ManagedDir at a temp HOME for the duration of a test.
func withHome(t *testing.T) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
}

func serveBytes(t *testing.T, body []byte) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func sha256hex(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func TestInstallVerifiesAndRecords(t *testing.T) {
	withHome(t)
	payload := []byte("#!/bin/sh\necho fake llama-server\n")
	srv := serveBytes(t, payload)

	rec, err := Install(context.Background(), "llamacpp",
		Spec{URL: srv.URL + "/bin", SHA256: sha256hex(payload), Version: "b1"},
		"", io.Discard)
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	if rec.SHA256 != sha256hex(payload) {
		t.Fatalf("receipt sha mismatch: %s", rec.SHA256)
	}
	// Binary present + executable.
	fi, err := os.Stat(rec.Path)
	if err != nil {
		t.Fatalf("installed binary missing: %v", err)
	}
	if fi.Mode()&0o111 == 0 {
		t.Fatal("installed binary should be executable")
	}
	// ManagedPath resolves the receipt.
	if p, ok := ManagedPath("llamacpp"); !ok || p != rec.Path {
		t.Fatalf("ManagedPath = %q, %v; want %q", p, ok, rec.Path)
	}
}

func TestInstallRejectsBadChecksum(t *testing.T) {
	withHome(t)
	srv := serveBytes(t, []byte("real content"))

	_, err := Install(context.Background(), "llamacpp",
		Spec{URL: srv.URL + "/bin", SHA256: sha256hex([]byte("different"))},
		"", io.Discard)
	if err == nil {
		t.Fatal("expected checksum mismatch error")
	}
	if _, ok := ManagedPath("llamacpp"); ok {
		t.Fatal("failed install must not leave a managed binary")
	}
}

func TestURLRequiresSHA(t *testing.T) {
	withHome(t)
	_, err := Install(context.Background(), "llamacpp", Spec{URL: "http://x/y"}, "", io.Discard)
	if err == nil {
		t.Fatal("--url without --sha256 must error")
	}
}

func TestNoManifestEntry(t *testing.T) {
	withHome(t)
	_, err := Install(context.Background(), "llamacpp", Spec{}, "", io.Discard)
	if err == nil {
		t.Fatal("empty manifest + no url should error, not silently do nothing")
	}
}
