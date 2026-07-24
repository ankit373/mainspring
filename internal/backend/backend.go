// Package backend defines the pluggable inference-engine contract. A Backend
// detects an engine on the host and starts a Runner: a live process serving one
// model over a localhost OpenAI-compatible endpoint. Concrete adapters
// (llamacpp, mlx, ollama, …) live in subpackages.
//
// Design rule: Mainspring never implements an inference kernel. A Backend is a
// thin supervisor around a permissively-licensed engine subprocess.
package backend

import "context"

// Status is a Runner's readiness, following the fail-loud health protocol
// copied from Ollama's runner: ready → serve; loading → 503 retry; no_slot →
// 503 backpressure; down → the process is gone.
type Status string

const (
	StatusReady   Status = "ready"
	StatusLoading Status = "loading"
	StatusNoSlot  Status = "no_slot"
	StatusDown    Status = "down"
)

// ModelSpec describes a model to serve.
type ModelSpec struct {
	ID      string   // logical id exposed via the API, e.g. "qwen2.5-coder"
	Path    string   // absolute path to the weights (e.g. a .gguf file)
	CtxSize   int      // requested context window; 0 = engine default
	GPULayers int      // 0 = offload all (default), N>0 = that many, N<0 = CPU-only
	ExtraArgs []string // engine-specific passthrough flags
}

// Capabilities is the fail-loud truth about a running model — the anti-Ollama
// signal. It states which backend actually served the model, whether it fell
// back to CPU, and the context window that is REALLY in effect. EffectiveCtx <
// RequestedCtx means the engine silently shrank the window; callers must warn,
// never treat it as success.
type Capabilities struct {
	Backend      string `json:"backend"`               // "llamacpp"
	Model        string `json:"model"`                 // logical model id
	Device       string `json:"device"`                // "metal" | "cuda" | "rocm" | "cpu"
	GPUOffload   bool   `json:"gpu_offload"`           // false ⇒ running on CPU
	RequestedCtx int    `json:"requested_ctx"`
	EffectiveCtx int    `json:"effective_ctx"`
	Quantization string `json:"quantization,omitempty"`
	Warnings     []string `json:"warnings,omitempty"`  // populated when degraded
}

// Degraded reports whether the running model differs from what was asked for —
// CPU fallback or a shrunk context window. The server surfaces this loudly.
func (c Capabilities) Degraded() bool {
	if !c.GPUOffload {
		return true
	}
	if c.RequestedCtx > 0 && c.EffectiveCtx > 0 && c.EffectiveCtx < c.RequestedCtx {
		return true
	}
	return len(c.Warnings) > 0
}

// Availability reports whether a backend can run on this host (detect-first UX).
type Availability struct {
	Name    string `json:"name"`
	Present bool   `json:"present"`           // binary/runtime found
	Path    string `json:"path,omitempty"`    // resolved binary path
	Version string `json:"version,omitempty"`
	Reason  string `json:"reason,omitempty"`  // when absent: why / how to install
}

// Runner is a live inference process serving one model.
type Runner interface {
	// BaseURL is the localhost OpenAI-compatible endpoint to proxy to,
	// e.g. "http://127.0.0.1:8181".
	BaseURL() string
	// Health polls readiness. It must be cheap and non-blocking.
	Health(ctx context.Context) Status
	// Capabilities reports the fail-loud truth about what is actually running.
	Capabilities(ctx context.Context) (Capabilities, error)
	// MemoryBytes estimates the resident footprint, for the VRAM scheduler.
	MemoryBytes() int64
	// Stop terminates the process and reclaims its memory. Must be idempotent.
	Stop(ctx context.Context) error
}

// Backend is a pluggable inference-engine adapter.
type Backend interface {
	// Name is the adapter id: "llamacpp", "mlx", "ollama".
	Name() string
	// Detect reports whether this engine is usable on the current host.
	Detect(ctx context.Context) Availability
	// Start launches a Runner serving spec. It blocks until the runner is
	// Ready or the context is cancelled.
	Start(ctx context.Context, spec ModelSpec) (Runner, error)
}
