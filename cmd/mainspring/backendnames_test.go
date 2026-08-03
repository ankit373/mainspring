package main

import (
	"strings"
	"testing"

	"github.com/ankit373/mainspring/internal/backend"
	"github.com/ankit373/mainspring/internal/config"
)

// TestEveryAdvertisedBackendIsConstructible closes the drift loop between the
// backend name list and the constructor that switches on it. Before #233 there
// were three hardcoded copies of that list and one of them — the `backend:` yaml
// comment — had gone stale at four of the six names, so the docs disagreed with
// the code and nothing noticed.
//
// Adding a backend to newBackendByName without adding it to backend.AllNames now
// fails here, and so does the reverse.
func TestEveryAdvertisedBackendIsConstructible(t *testing.T) {
	if len(backend.AllNames) == 0 {
		t.Fatal("backend.AllNames is empty")
	}
	for _, name := range backend.AllNames {
		b, err := newBackendByName(name, config.Default())
		if err != nil {
			t.Errorf("backend.AllNames advertises %q but newBackendByName rejects it: %v", name, err)
			continue
		}
		if b == nil {
			t.Errorf("newBackendByName(%q) returned a nil backend and no error", name)
		}
	}
}

func TestUnknownBackendIsRejectedAndNamesTheValidSet(t *testing.T) {
	_, err := newBackendByName("nosuchengine", config.Default())
	if err == nil {
		t.Fatal("newBackendByName accepted an unknown backend")
	}
	// The error has to be actionable: it is the only place a typo surfaces.
	for _, want := range backend.AllNames {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not mention valid backend %q: %v", want, err)
		}
	}
}

// TestManagedAndAdoptPartitionAllNames guards the invariant the install gate now
// depends on: every backend is exactly one of managed or adopt. A name in neither
// would be silently uninstallable *and* undetectable; a name in both would let
// install write a binary for a daemon that ignores it, which is #233.
func TestManagedAndAdoptPartitionAllNames(t *testing.T) {
	for _, name := range backend.AllNames {
		managed, adopt := backend.IsManaged(name), backend.IsAdopt(name)
		if managed == adopt {
			t.Errorf("%q is managed=%v adopt=%v — it must be exactly one", name, managed, adopt)
		}
	}
	if got, want := len(backend.AllNames), len(backend.ManagedNames)+len(backend.AdoptNames); got != want {
		t.Errorf("AllNames has %d entries, ManagedNames+AdoptNames have %d — the sets overlap or one is missing", got, want)
	}
}
