package main

import (
	"net"
	"strings"
	"testing"

	"github.com/ankit373/mainspring/internal/config"
)

func TestPreloadIDs(t *testing.T) {
	cfg := config.Config{Models: []config.Model{
		{ID: "a", Preload: true},
		{ID: "b"},
		{ID: "c", Preload: true},
	}}
	got := preloadIDs(cfg)
	if len(got) != 2 || got[0] != "a" || got[1] != "c" {
		t.Fatalf("preloadIDs = %v, want [a c]", got)
	}
	if len(preloadIDs(config.Config{})) != 0 {
		t.Fatal("no models => no preloads")
	}
}

func TestDefaultBackendName(t *testing.T) {
	if defaultBackendName(config.Config{}) != "llamacpp" {
		t.Fatal("empty backend should default to llamacpp")
	}
	if defaultBackendName(config.Config{Backend: "ollama"}) != "ollama" {
		t.Fatal("explicit backend should win")
	}
}

func TestCheckModels(t *testing.T) {
	cfg := config.Config{
		Backend: "ollama",
		Models: []config.Model{
			{ID: "a"},                 // → ollama (present)
			{ID: "b", Backend: "mlx"}, // backend absent
			{ID: "c", Backend: "llamacpp", Path: "/no/such/file.gguf"}, // present but weights missing
		},
	}
	present := map[string]bool{"ollama": true, "llamacpp": true, "mlx": false}
	lines := checkModels(cfg, present)
	if len(lines) != 3 {
		t.Fatalf("want 3 lines, got %v", lines)
	}
	if !strings.HasPrefix(lines[0], "✓") {
		t.Errorf("model a should be OK: %q", lines[0])
	}
	if !strings.Contains(lines[1], "backend not available") {
		t.Errorf("model b should flag absent backend: %q", lines[1])
	}
	if !strings.Contains(lines[2], "weights not found") {
		t.Errorf("model c should flag missing weights: %q", lines[2])
	}
}

func TestPortFree(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	if portFree(l.Addr().String()) {
		t.Fatalf("%s should report busy", l.Addr().String())
	}
}
