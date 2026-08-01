// Package ollama is an adopt-if-present backend: it uses an EXISTING Ollama
// daemon (its OpenAI-compatible endpoint) as a runner. Mainspring never installs
// or supervises Ollama — the daemon owns its own lifecycle; we adopt it and add
// the fail-loud signals Ollama omits (notably the per-request num_ctx trap).
package ollama

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/ankit373/mainspring/internal/backend"
)

const defaultHost = "http://127.0.0.1:11434"

// Backend adopts a running Ollama daemon.
type Backend struct {
	Host string // e.g. http://127.0.0.1:11434
}

// New returns an Ollama backend; host "" uses the default local daemon.
func New(host string) *Backend {
	if host == "" {
		host = defaultHost
	}
	return &Backend{Host: strings.TrimRight(host, "/")}
}

func (b *Backend) Name() string { return "ollama" }

var client = &http.Client{Timeout: 3 * time.Second}

// Detect reports whether a reachable Ollama daemon is present.
func (b *Backend) Detect(ctx context.Context) backend.Availability {
	av := backend.Availability{Name: "ollama", Path: b.Host}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, b.Host+"/api/tags", nil)
	resp, err := client.Do(req)
	if err != nil {
		av.Reason = "no Ollama daemon at " + b.Host + " — start it (`ollama serve`) or install from https://ollama.com (adopt-only; Mainspring will not install it)"
		return av
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		av.Reason = fmt.Sprintf("Ollama at %s returned status %d", b.Host, resp.StatusCode)
		return av
	}
	av.Present = true
	av.Version = b.version(ctx)
	return av
}

func (b *Backend) version(ctx context.Context) string {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, b.Host+"/api/version", nil)
	resp, err := client.Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	var v struct {
		Version string `json:"version"`
	}
	_ = json.NewDecoder(io.LimitReader(resp.Body, 1<<16)).Decode(&v)
	return v.Version
}

// Start verifies the model exists in the daemon and returns a runner pointing at
// Ollama's OpenAI-compatible endpoint. It does NOT pull models (adopt-only).
func (b *Backend) Start(ctx context.Context, spec backend.ModelSpec) (backend.Runner, error) {
	ok, err := b.hasModel(ctx, spec.ID)
	if err != nil {
		return nil, fmt.Errorf("ollama: %w", err)
	}
	if !ok {
		return nil, fmt.Errorf("ollama: model %q not present — pull it first with `ollama pull %s` (Mainspring will not pull automatically)", spec.ID, spec.ID)
	}
	return &runner{host: b.Host, spec: spec}, nil
}

func (b *Backend) hasModel(ctx context.Context, id string) (bool, error) {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, b.Host+"/api/tags", nil)
	resp, err := client.Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	var payload struct {
		Models []struct {
			Name string `json:"name"`
		} `json:"models"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&payload); err != nil {
		return false, err
	}
	for _, m := range payload.Models {
		// Ollama tags include the ":latest" suffix; match with or without it.
		if m.Name == id || strings.TrimSuffix(m.Name, ":latest") == id {
			return true, nil
		}
	}
	return false, nil
}

// ListModels enumerates the models the daemon has pulled (adopt-only discovery).
// The ":latest" suffix is trimmed so ids match how they're normally referenced.
func (b *Backend) ListModels(ctx context.Context) ([]string, error) {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, b.Host+"/api/tags", nil)
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("ollama /api/tags returned status %d", resp.StatusCode)
	}
	var payload struct {
		Models []struct {
			Name string `json:"name"`
		} `json:"models"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&payload); err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(payload.Models))
	for _, m := range payload.Models {
		ids = append(ids, strings.TrimSuffix(m.Name, ":latest"))
	}
	return ids, nil
}

// EstimateMemory implements backend.MemoryEstimator: it looks up the model's
// on-disk size from /api/tags — a reasonable pre-admission resident-footprint
// proxy, available even for a model Ollama has not loaded yet. This is what
// lets the scheduler's byte-budget eviction make room for an adopted Ollama
// model BEFORE starting it, the same way it already does for llamacpp/mlx;
// runner.MemoryBytes (via /api/ps) only knows the real size after the fact.
// Returns 0 (no estimate, same as not implementing this at all) on any lookup
// or parse failure, or if the model isn't in the tag list — never fabricated.
func (b *Backend) EstimateMemory(spec backend.ModelSpec) int64 {
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, b.Host+"/api/tags", nil)
	if err != nil {
		return 0
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0
	}
	defer resp.Body.Close()
	var payload struct {
		Models []struct {
			Name string `json:"name"`
			Size int64  `json:"size"`
		} `json:"models"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&payload); err != nil {
		return 0
	}
	for _, m := range payload.Models {
		if m.Name == spec.ID || strings.TrimSuffix(m.Name, ":latest") == spec.ID {
			return m.Size
		}
	}
	return 0
}

// runner adopts the daemon for one logical model.
type runner struct {
	host string
	spec backend.ModelSpec
}

func (r *runner) BaseURL() string { return r.host }

func (r *runner) Health(ctx context.Context) backend.Status {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, r.host+"/api/tags", nil)
	resp, err := client.Do(req)
	if err != nil {
		return backend.StatusDown
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		return backend.StatusReady
	}
	return backend.StatusDown
}

// MemoryBytes reports the VRAM/RAM Ollama has resident for this model (0 if not
// currently loaded). We don't manage this memory — Ollama does.
func (r *runner) MemoryBytes() int64 {
	if _, _, size := r.ps(context.Background()); size > 0 {
		return size
	}
	return 0
}

// Stop is a no-op: we adopt the daemon, we don't own its process or KeepAlive.
func (r *runner) Stop(context.Context) error { return nil }

// Capabilities surfaces the fail-loud truth. Ollama's effective num_ctx is set
// PER REQUEST (defaulting low) and is not exposed here, so we report the model's
// max context and warn that the runtime window may be smaller — directly naming
// the silent-truncation trap rather than pretending the max is in effect.
func (r *runner) Capabilities(ctx context.Context) (backend.Capabilities, error) {
	caps := backend.Capabilities{
		Backend:      "ollama",
		Model:        r.spec.ID,
		RequestedCtx: r.spec.CtxSize,
	}
	maxCtx := r.modelMaxContext(ctx)

	loaded, vram, _ := r.ps(ctx)
	switch {
	case !loaded:
		caps.Device = "unknown"
		caps.Warnings = append(caps.Warnings, "model not currently loaded in Ollama; device/offload unknown until first request")
	case vram > 0:
		caps.Device = "gpu"
		caps.GPUOffload = true
	default:
		caps.Device = "cpu"
	}

	if maxCtx > 0 {
		caps.Warnings = append(caps.Warnings, fmt.Sprintf(
			"Ollama effective context is per-request (defaults may truncate silently); model max_ctx=%d — set num_ctx explicitly", maxCtx))
	}
	return caps, nil
}

// modelMaxContext reads the model's max context length from /api/show.
func (r *runner) modelMaxContext(ctx context.Context) int {
	body, _ := json.Marshal(map[string]string{"name": r.spec.ID})
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, r.host+"/api/show", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return 0
	}
	defer resp.Body.Close()
	var payload struct {
		ModelInfo map[string]json.RawMessage `json:"model_info"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&payload); err != nil {
		return 0
	}
	for k, v := range payload.ModelInfo {
		if strings.HasSuffix(k, ".context_length") {
			var n int
			if json.Unmarshal(v, &n) == nil {
				return n
			}
		}
	}
	return 0
}

// ps returns whether the model is loaded, and its total/vram sizes, via /api/ps.
func (r *runner) ps(ctx context.Context) (loaded bool, vram, size int64) {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, r.host+"/api/ps", nil)
	resp, err := client.Do(req)
	if err != nil {
		return false, 0, 0
	}
	defer resp.Body.Close()
	var payload struct {
		Models []struct {
			Name     string `json:"name"`
			Size     int64  `json:"size"`
			SizeVRAM int64  `json:"size_vram"`
		} `json:"models"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&payload); err != nil {
		return false, 0, 0
	}
	for _, m := range payload.Models {
		if m.Name == r.spec.ID || strings.TrimSuffix(m.Name, ":latest") == r.spec.ID {
			return true, m.SizeVRAM, m.Size
		}
	}
	return false, 0, 0
}
