package install

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A failed verification must not disturb what is already installed.
//
// The download used to be renamed onto the final path *before* its checksum was
// compared, so a second install of the same version atomically replaced a
// verified binary with unverified bytes and only then discovered the mismatch.
// The rollback was `_ = os.Remove(dest)`, whose error was discarded — so the
// good binary was destroyed either way, and on a failed remove the attacker's
// bytes were left sitting at the path the receipt still vouched for.
func TestFailedInstallLeavesTheGoodBinaryIntact(t *testing.T) {
	withHome(t)
	good := []byte("#!/bin/sh\necho good\n")
	srv := serveBytes(t, good)

	rec, err := Install(context.Background(), "llamacpp",
		Spec{URL: srv.URL + "/bin", SHA256: sha256hex(good), Version: "b1"}, "", io.Discard)
	if err != nil {
		t.Fatalf("first install: %v", err)
	}

	// Same backend, same version — so the same destination path — but the URL now
	// serves something else. This is a compromised mirror, or a tag repointed
	// under a stable filename.
	evil := []byte("#!/bin/sh\necho evil\n")
	evilSrv := serveBytes(t, evil)
	_, err = Install(context.Background(), "llamacpp",
		Spec{URL: evilSrv.URL + "/bin", SHA256: sha256hex(good), Version: "b1"}, "", io.Discard)
	if err == nil {
		t.Fatal("install of mismatched content must fail")
	}

	onDisk, readErr := os.ReadFile(rec.Path)
	if readErr != nil {
		t.Fatalf("the previously verified binary was destroyed by a failed install: %v", readErr)
	}
	if string(onDisk) != string(good) {
		t.Fatalf("verified binary was replaced with unverified content:\n got %q\nwant %q", onDisk, good)
	}
	if p, ok := ManagedPath("llamacpp"); !ok || p != rec.Path {
		t.Fatalf("receipt should still resolve after a failed install: %q %v", p, ok)
	}
}

// Nothing unverified may ever appear at the destination path, even transiently:
// the temp file is what gets hashed, and only a match earns the rename.
func TestUnverifiedContentNeverReachesTheDestination(t *testing.T) {
	withHome(t)
	body := []byte("some payload")
	srv := serveBytes(t, body)

	_, err := Install(context.Background(), "llamacpp",
		Spec{URL: srv.URL + "/bin", SHA256: sha256hex([]byte("other")), Version: "b1"}, "", io.Discard)
	if err == nil {
		t.Fatal("expected checksum mismatch")
	}

	dest := filepath.Join(ManagedDir(), "llamacpp", "b1", "llama-server")
	if _, err := os.Stat(dest); !os.IsNotExist(err) {
		t.Fatalf("unverified download is present at %s (stat err = %v)", dest, err)
	}
	// The staging file must be cleaned up too — a failed install should not leave
	// the payload lying around under a different name.
	entries, err := os.ReadDir(filepath.Dir(dest))
	if err != nil {
		t.Fatalf("read dest dir: %v", err)
	}
	for _, e := range entries {
		t.Fatalf("failed install left %q behind", e.Name())
	}
}

// An install must not be able to fill the disk. The overage is reported as an
// overage, not as a checksum mismatch — those are different failures, and
// conflating them sends the operator hunting for a corrupt artifact that is
// really an oversized one.
func TestDownloadIsBounded(t *testing.T) {
	withHome(t)
	// Shrink the cap rather than pushing the production 2 GiB through a loopback
	// socket; what is under test is the bound, not its value.
	defer func(orig int64) { maxDownloadBytes = orig }(maxDownloadBytes)
	maxDownloadBytes = 1 << 20

	// The handler reads this local, never the package var. Deferred restores run
	// before t.Cleanup closes the server, so a handler goroutine still writing
	// when the test returns would otherwise read maxDownloadBytes concurrently
	// with the restore — a real race, and one that only trips sometimes, which is
	// the kind that turns up as an unexplained red CI run rather than a failure
	// you can reproduce.
	limit := maxDownloadBytes

	// Serve past the cap, stopping as soon as the client hangs up.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		chunk := make([]byte, 1<<20)
		for sent := int64(0); sent <= limit; sent += int64(len(chunk)) {
			if _, err := w.Write(chunk); err != nil {
				return
			}
			if r.Context().Err() != nil {
				return
			}
		}
	}))
	t.Cleanup(srv.Close)

	_, err := Install(context.Background(), "llamacpp",
		Spec{URL: srv.URL + "/bin", SHA256: strings.Repeat("a", 64), Version: "b1"}, "", io.Discard)
	if err == nil {
		t.Fatal("an oversized download must fail")
	}
	if !strings.Contains(err.Error(), "too large") {
		t.Fatalf("an oversized download should say so, got: %v", err)
	}
}

// http.DefaultClient has no timeout and `mainspring install` runs on a context
// with no deadline, so a server that accepts and never answers would hang the
// command forever. This asserts the bounds exist; their enforcement is stdlib's.
func TestDownloadClientIsBounded(t *testing.T) {
	c := newDownloadClient()
	if c.Timeout <= 0 {
		t.Error("download client needs an overall timeout; http.DefaultClient has none")
	}
	tr, ok := c.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("expected *http.Transport, got %T", c.Transport)
	}
	if tr.ResponseHeaderTimeout <= 0 {
		t.Error("download client needs a response-header timeout")
	}
	// Proxy settings are how a lot of environments reach the internet at all;
	// building a custom Transport silently drops them unless it is set.
	if tr.Proxy == nil {
		t.Error("custom transport must keep honouring HTTP(S)_PROXY")
	}
}
