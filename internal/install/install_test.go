package install

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// withHome points ManagedDir at a throwaway directory for the duration of a test.
//
// Setting HOME alone is not enough: on Windows os.UserHomeDir reads %USERPROFILE%
// and os.UserConfigDir reads %AppData%, so these tests were not isolated there at
// all — they wrote into the real user profile and leaked into each other. That is
// how a "failed install left llama-server.exe behind" failure appeared: the file
// was left by the *previous* test.
func withHome(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	if runtime.GOOS == "windows" {
		t.Setenv("USERPROFILE", dir)
		t.Setenv("AppData", dir)
	}
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
	// Binary present and runnable. What "runnable" means is platform-specific:
	// Windows has no execute bit (os.Chmod there only toggles read-only), and
	// executability comes from the .exe extension instead.
	fi, err := os.Stat(rec.Path)
	if err != nil {
		t.Fatalf("installed binary missing: %v", err)
	}
	if runtime.GOOS == "windows" {
		if filepath.Ext(rec.Path) != ".exe" {
			t.Fatalf("installed binary %q needs a .exe suffix to be executable", rec.Path)
		}
	} else if fi.Mode()&0o111 == 0 {
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
