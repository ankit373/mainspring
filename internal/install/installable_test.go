package install

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/ankit373/mainspring/internal/backend"
)

// unreachable is a URL nothing can serve. Every rejection test pins it so that a
// removed guard fails on a connection error instead of passing quietly — reaching
// the network at all is the bug being tested for.
const unreachable = "http://127.0.0.1:1/never-fetched"

func rejects(t *testing.T, name string) error {
	t.Helper()
	_, err := Install(context.Background(), name, Spec{
		URL:    unreachable,
		SHA256: strings.Repeat("00", 32),
	}, "", io.Discard)
	if err == nil {
		t.Fatalf("Install(%q) succeeded; it must be refused", name)
	}
	return err
}

// TestAdoptBackendsAreRefused is the regression test for #233. `install ollama
// --url … --sha256 …` used to download, verify and write a binary the ollama
// backend never reads, printing "installed →" while the same CLI reported
// "adopt-only; Mainspring will not install it".
func TestAdoptBackendsAreRefused(t *testing.T) {
	for _, name := range backend.AdoptNames {
		t.Run(name, func(t *testing.T) {
			if err := rejects(t, name); !strings.Contains(err.Error(), "adopt-only") {
				t.Fatalf("refused for the wrong reason: %v", err)
			}
		})
	}
}

// TestManagedButNotADownloadableBinaryIsRefused covers the narrower half of the
// same bug: mlx is managed as a subprocess but is a Python module, so there is no
// artifact to fetch and newBackendByName never consults a managed binary for it.
// Installing one wrote bytes nothing would read.
func TestManagedButNotADownloadableBinaryIsRefused(t *testing.T) {
	for name := range notDistributedAsBinary {
		t.Run(name, func(t *testing.T) {
			err := rejects(t, name)
			if !strings.Contains(err.Error(), "not installed as a binary") {
				t.Fatalf("refused for the wrong reason: %v", err)
			}
			// The remedy has to be actionable, not just a refusal.
			if !strings.Contains(err.Error(), "pip install") {
				t.Errorf("error gives no remedy: %v", err)
			}
		})
	}
}

func TestUnknownBackendIsRefusedBeforeAnyDownload(t *testing.T) {
	err := rejects(t, "nosuchengine")
	if !strings.Contains(err.Error(), "unknown backend") {
		t.Fatalf("refused for the wrong reason: %v", err)
	}
	for _, want := range installableNames {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not name installable backend %q: %v", want, err)
		}
	}
}

// TestInstallableBackendsStillReachResolution proves the guard rejects only what it
// should: an installable backend with no manifest entry and no --url must still
// fail on *resolution* — the pre-existing behaviour — not on the new validation.
func TestInstallableBackendsStillReachResolution(t *testing.T) {
	withHome(t)
	for _, name := range installableNames {
		t.Run(name, func(t *testing.T) {
			_, err := Install(context.Background(), name, Spec{}, "", io.Discard)
			if err == nil {
				t.Fatalf("Install(%q) unexpectedly succeeded with an empty manifest", name)
			}
			if !strings.Contains(err.Error(), "no manifest entry") {
				t.Fatalf("Install(%q) did not reach manifest resolution: %v", name, err)
			}
		})
	}
}

// TestEveryManagedBackendIsClassified forces a decision when a managed backend is
// added: it is either something install can fetch, or it carries an explanation of
// why not. Falling through to "unknown backend" for a backend the server happily
// runs would be a lie, and that silent third state is what this prevents.
func TestEveryManagedBackendIsClassified(t *testing.T) {
	for _, name := range backend.ManagedNames {
		installable := slices.Contains(installableNames, name)
		_, explained := notDistributedAsBinary[name]
		if installable == explained {
			t.Errorf("managed backend %q: installable=%v explained=%v — it must be exactly one",
				name, installable, explained)
		}
	}
	// And nothing may claim to be installable without being a real backend.
	for _, name := range installableNames {
		if !backend.IsManaged(name) {
			t.Errorf("%q is listed installable but is not a managed backend", name)
		}
	}
}

// TestStaleReceiptForANonInstallableBackendIsInert is the regression test for
// #235. `mainspring install ollama` was possible before #234, and the receipt it
// left behind made `backends`/`doctor` report an adopted daemon's SOURCE as
// "managed" — permanently, since nothing revisits a receipt. Reporting where an
// engine came from is the one thing that field must not get wrong.
func TestStaleReceiptForANonInstallableBackendIsInert(t *testing.T) {
	withHome(t)

	// A receipt indistinguishable from one a real pre-#234 install would leave.
	for _, name := range append(slices.Clone(backend.AdoptNames), "mlx") {
		t.Run(name, func(t *testing.T) {
			dir := filepath.Join(ManagedDir(), name, "pinned")
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			bin := filepath.Join(dir, name)
			if err := os.WriteFile(bin, []byte("stub"), 0o755); err != nil {
				t.Fatal(err)
			}
			rec, err := json.Marshal(Receipt{
				Backend: name, Version: "pinned", Path: bin,
				SHA256: "deadbeef", InstalledAt: "2026-07-01T00:00:00Z",
			})
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(ManagedDir(), name, "receipt.json"), rec, 0o644); err != nil {
				t.Fatal(err)
			}

			if p, ok := ManagedPath(name); ok {
				t.Fatalf("ManagedPath(%q) resolved to %q from a stale receipt; nothing loads a managed binary for it", name, p)
			}
		})
	}
}
