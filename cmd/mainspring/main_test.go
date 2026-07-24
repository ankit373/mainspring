package main

import (
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
