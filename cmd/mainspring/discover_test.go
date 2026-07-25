package main

import (
	"context"
	"errors"
	"testing"

	"github.com/ankit373/mainspring/internal/backend"
)

// listerBackend is a fake backend that enumerates a fixed model list (or errors).
type listerBackend struct {
	name string
	ids  []string
	err  error
}

func (b *listerBackend) Name() string { return b.name }
func (b *listerBackend) Detect(context.Context) backend.Availability {
	return backend.Availability{Name: b.name, Present: true}
}
func (b *listerBackend) Start(context.Context, backend.ModelSpec) (backend.Runner, error) {
	return nil, errors.New("not used")
}
func (b *listerBackend) ListModels(context.Context) ([]string, error) { return b.ids, b.err }

// plainBackend implements Backend but NOT ModelLister (should be skipped).
type plainBackend struct{ name string }

func (b *plainBackend) Name() string { return b.name }
func (b *plainBackend) Detect(context.Context) backend.Availability {
	return backend.Availability{Name: b.name, Present: true}
}
func (b *plainBackend) Start(context.Context, backend.ModelSpec) (backend.Runner, error) {
	return nil, errors.New("not used")
}

func TestDiscoverModelsMergesConfiguredWins(t *testing.T) {
	backends := map[string]backend.Backend{
		"ollama":   &listerBackend{name: "ollama", ids: []string{"qwen2.5", "llama3", "dup"}},
		"lmstudio": &listerBackend{name: "lmstudio", ids: []string{"phi3"}},
		"llamacpp": &plainBackend{name: "llamacpp"}, // no ModelLister => skipped
	}
	// "dup" is already configured (on llamacpp) → configured wins, not re-added.
	existing := []backend.ModelSpec{{ID: "dup", Backend: "llamacpp"}}

	found := discoverModels(context.Background(), backends, existing)

	byID := map[string]string{}
	for _, sp := range found {
		if _, seen := byID[sp.ID]; seen {
			t.Fatalf("duplicate discovered id %q", sp.ID)
		}
		byID[sp.ID] = sp.Backend
	}
	if _, ok := byID["dup"]; ok {
		t.Fatal("configured id must not be re-discovered")
	}
	if byID["qwen2.5"] != "ollama" || byID["llama3"] != "ollama" || byID["phi3"] != "lmstudio" {
		t.Fatalf("discovered specs wrong: %v", byID)
	}
	if len(found) != 3 { // qwen2.5, llama3, phi3
		t.Fatalf("expected 3 discovered, got %d: %v", len(found), byID)
	}
}

func TestDiscoverModelsSkipsListerErrors(t *testing.T) {
	backends := map[string]backend.Backend{
		"ollama":   &listerBackend{name: "ollama", err: errors.New("down")},
		"lmstudio": &listerBackend{name: "lmstudio", ids: []string{"phi3"}},
	}
	found := discoverModels(context.Background(), backends, nil)
	if len(found) != 1 || found[0].ID != "phi3" {
		t.Fatalf("a failing lister must be skipped, healthy one kept: %v", found)
	}
}
