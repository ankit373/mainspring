// Package config loads and saves Mainspring's server configuration
// (~/.config/mainspring/config.yaml by default). Flags on `mainspring serve`
// override file values.
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"gopkg.in/yaml.v3"
)

// Model is one servable model entry.
type Model struct {
	ID        string   `yaml:"id"`
	Backend   string   `yaml:"backend,omitempty"` // per-model backend; empty => server default
	Path      string   `yaml:"path"`
	Ctx       int      `yaml:"ctx,omitempty"`
	GPULayers int      `yaml:"gpu_layers,omitempty"`
	Args      []string `yaml:"args,omitempty"`
}

// Tenant is a named principal with an API key, role, and quotas.
type Tenant struct {
	Name        string `yaml:"name"`
	Key         string `yaml:"key"`
	Role        string `yaml:"role,omitempty"`         // "admin" | "inference" (default inference)
	RateRPM     int    `yaml:"rate_rpm,omitempty"`     // requests/min; 0 = unlimited
	TokenBudget int64  `yaml:"token_budget,omitempty"` // tokens per window; 0 = unlimited
	WindowSec   int    `yaml:"window_sec,omitempty"`   // token-budget window; <=0 => 60
}

// Config is the full server configuration.
type Config struct {
	Addr             string   `yaml:"addr"`
	APIKeys          []string `yaml:"api_keys,omitempty"`
	Tenants          []Tenant `yaml:"tenants,omitempty"`
	KeepAliveSeconds int      `yaml:"keep_alive_seconds"`
	MaxLoaded        int      `yaml:"max_loaded"`
	MaxResidentMB    int      `yaml:"max_resident_mb,omitempty"`
	UsageLedger      string   `yaml:"usage_ledger,omitempty"` // JSONL path; empty => default location
	Backend          string   `yaml:"backend,omitempty"`      // "llamacpp" (default) | "ollama" | "mlx"
	LlamaServerPath  string   `yaml:"llama_server_path,omitempty"`
	OllamaHost       string   `yaml:"ollama_host,omitempty"` // e.g. http://127.0.0.1:11434
	MLXPython        string   `yaml:"mlx_python,omitempty"`  // python interpreter for mlx_lm.server
	Models           []Model  `yaml:"models,omitempty"`
}

// Default returns the baseline configuration.
func Default() Config {
	return Config{
		Addr:             ":11500",
		KeepAliveSeconds: 300,
		MaxLoaded:        1,
	}
}

// KeepAlive returns the idle-unload duration (0 disables it).
func (c Config) KeepAlive() time.Duration {
	if c.KeepAliveSeconds <= 0 {
		return 0
	}
	return time.Duration(c.KeepAliveSeconds) * time.Second
}

// MaxBytes returns the resident byte budget (0 = no byte cap).
func (c Config) MaxBytes() int64 {
	if c.MaxResidentMB <= 0 {
		return 0
	}
	return int64(c.MaxResidentMB) << 20
}

// DefaultPath is ~/.config/mainspring/config.yaml.
func DefaultPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return "config.yaml"
	}
	return filepath.Join(home, ".config", "mainspring", "config.yaml")
}

// DefaultLedgerPath is ~/.config/mainspring/usage.jsonl.
func DefaultLedgerPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return "usage.jsonl"
	}
	return filepath.Join(home, ".config", "mainspring", "usage.jsonl")
}

// Load reads config from path, layered over Default. An empty path uses
// DefaultPath and treats a missing file as "just defaults" (no error). An
// explicit, missing path is an error.
func Load(path string) (Config, error) {
	cfg := Default()
	explicit := path != ""
	if !explicit {
		path = DefaultPath()
	}
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) && !explicit {
			return cfg, nil
		}
		return cfg, fmt.Errorf("read config %s: %w", path, err)
	}
	if err := yaml.Unmarshal(b, &cfg); err != nil {
		return cfg, fmt.Errorf("parse config %s: %w", path, err)
	}
	return cfg, nil
}

// Save writes cfg to path (creating parent dirs).
func Save(path string, cfg Config) error {
	if path == "" {
		path = DefaultPath()
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	b, err := yaml.Marshal(cfg)
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o600)
}
