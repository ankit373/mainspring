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
	Backend   string   `yaml:"backend,omitempty"`   // primary backend; empty => server default
	Fallbacks []string `yaml:"fallbacks,omitempty"` // ordered backends to fail over to
	Path      string   `yaml:"path"`
	Ctx       int      `yaml:"ctx,omitempty"`
	GPULayers int      `yaml:"gpu_layers,omitempty"`
	Preload   bool     `yaml:"preload,omitempty"` // load at startup instead of on first request
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
	Addr             string            `yaml:"addr"`
	APIKeys          []string          `yaml:"api_keys,omitempty"`
	Tenants          []Tenant          `yaml:"tenants,omitempty"`
	KeepAliveSeconds int               `yaml:"keep_alive_seconds"`
	MaxLoaded        int               `yaml:"max_loaded"`
	MaxResidentMB    int               `yaml:"max_resident_mb,omitempty"`
	MaxInflight      int               `yaml:"max_inflight,omitempty"`             // concurrent requests per model; 0 = unbounded
	MaxQueue         int               `yaml:"max_queue,omitempty"`                // extra waiters per model before 503
	BreakerThreshold int               `yaml:"breaker_threshold,omitempty"`        // consecutive failures before the circuit opens; 0 = disabled
	BreakerCooldownS int               `yaml:"breaker_cooldown_seconds,omitempty"` // seconds a tripped breaker stays open; <=0 => 30
	HealthProbeS     int               `yaml:"health_probe_seconds,omitempty"`     // background health-probe interval; <=0 => off
	DrainSeconds     int               `yaml:"drain_seconds,omitempty"`            // shutdown drain timeout; <=0 => 30s
	UsageLedger      string            `yaml:"usage_ledger,omitempty"`             // JSONL path; empty => default location
	AccessLog        string            `yaml:"access_log,omitempty"`               // JSONL access log path; empty => off, "stderr"/"stdout" accepted
	TLSCert          string            `yaml:"tls_cert,omitempty"`                 // PEM cert path; with tls_key => serve HTTPS
	TLSKey           string            `yaml:"tls_key,omitempty"`                  // PEM key path
	Backend          string            `yaml:"backend,omitempty"`                  // "llamacpp" (default) | "ollama" | "mlx" | "lmstudio"
	LlamaServerPath  string            `yaml:"llama_server_path,omitempty"`
	OllamaHost       string            `yaml:"ollama_host,omitempty"`    // e.g. http://127.0.0.1:11434
	MLXPython        string            `yaml:"mlx_python,omitempty"`     // python interpreter for mlx_lm.server
	LMStudioHost     string            `yaml:"lmstudio_host,omitempty"`  // e.g. http://127.0.0.1:1234
	LlamafileHost    string            `yaml:"llamafile_host,omitempty"` // e.g. http://127.0.0.1:8080
	GPT4AllHost      string            `yaml:"gpt4all_host,omitempty"`   // e.g. http://127.0.0.1:4891
	Models           []Model           `yaml:"models,omitempty"`
	Aliases          map[string]string `yaml:"aliases,omitempty"`         // friendly name -> model id (or another alias)
	DiscoverModels   bool              `yaml:"discover_models,omitempty"` // auto-expose models from present adopt backends
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

// DrainTimeout returns how long to wait for in-flight requests on shutdown.
func (c Config) DrainTimeout() time.Duration {
	if c.DrainSeconds <= 0 {
		return 30 * time.Second
	}
	return time.Duration(c.DrainSeconds) * time.Second
}

// BreakerCooldown returns how long a tripped circuit breaker stays open.
func (c Config) BreakerCooldown() time.Duration {
	if c.BreakerCooldownS <= 0 {
		return 30 * time.Second
	}
	return time.Duration(c.BreakerCooldownS) * time.Second
}

// HealthProbeInterval returns the background health-probe period (0 = disabled).
func (c Config) HealthProbeInterval() time.Duration {
	if c.HealthProbeS <= 0 {
		return 0
	}
	return time.Duration(c.HealthProbeS) * time.Second
}

// TLSEnabled reports whether both a cert and key are configured (=> serve HTTPS).
func (c Config) TLSEnabled() bool { return c.TLSCert != "" && c.TLSKey != "" }

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
