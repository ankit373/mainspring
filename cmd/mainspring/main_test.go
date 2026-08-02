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

func TestParseModelFlag(t *testing.T) {
	// A weights-loading backend needs a path.
	m, err := parseModelFlag("qwen=/models/qwen.gguf", "llamacpp")
	if err != nil {
		t.Fatalf("id=path on llamacpp: %v", err)
	}
	if m.ID != "qwen" || m.Path != "/models/qwen.gguf" {
		t.Fatalf("parsed %+v", m)
	}

	// An adopt-only backend already holds the weights: "id=" is valid (the README
	// Ollama quick start).
	m, err = parseModelFlag("qwen2.5-coder:7b=", "ollama")
	if err != nil {
		t.Fatalf("empty path on ollama should be accepted: %v", err)
	}
	if m.ID != "qwen2.5-coder:7b" || m.Path != "" {
		t.Fatalf("parsed %+v, want id only", m)
	}

	// …but llamacpp has nothing to load, so it must still be rejected, naming the
	// backends that do allow it.
	_, err = parseModelFlag("qwen2.5-coder:7b=", "llamacpp")
	if err == nil {
		t.Fatal("empty path on llamacpp must be rejected")
	}
	for _, want := range []string{"llamacpp", "ollama", "lmstudio", "llamafile", "gpt4all"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should name %q: %v", want, err)
		}
	}

	// No "=" at all, or no id, is invalid on any backend.
	for _, bad := range []string{"qwen", "=/models/qwen.gguf", ""} {
		if _, err := parseModelFlag(bad, "ollama"); err == nil {
			t.Errorf("parseModelFlag(%q) should fail", bad)
		}
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
