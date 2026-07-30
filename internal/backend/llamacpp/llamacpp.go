// Package llamacpp is the v0 Mainspring backend: it supervises llama.cpp's
// `llama-server` as a subprocess and proxies its OpenAI-compatible endpoints.
//
// It is a thin supervisor, never a kernel. All inference happens inside
// llama-server; this package only launches it, polls health, reclaims memory on
// stop, and — critically — reports the fail-loud truth about what actually ran
// (device, GPU offload, effective context) by reading the engine's own logs.
package llamacpp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/ankit373/mainspring/internal/backend"
	"github.com/ankit373/mainspring/internal/util"
)

// binName is the llama.cpp server executable.
const binName = "llama-server"

// Backend supervises llama-server processes.
type Backend struct {
	// BinPath overrides the executable location; empty means look it up on PATH.
	BinPath string
	// Host is the loopback address runners bind to.
	Host string
}

// New returns a llama.cpp backend. binPath may be "" to resolve from PATH.
func New(binPath string) *Backend {
	return &Backend{BinPath: binPath, Host: "127.0.0.1"}
}

// Name implements backend.Backend.
func (b *Backend) Name() string { return "llamacpp" }

func (b *Backend) resolveBin() (string, error) {
	if b.BinPath != "" {
		if fi, err := os.Stat(b.BinPath); err == nil && !fi.IsDir() {
			return b.BinPath, nil
		}
		return "", fmt.Errorf("configured llama-server not found at %s", b.BinPath)
	}
	return exec.LookPath(binName)
}

// Detect implements backend.Backend (detect-first UX).
func (b *Backend) Detect(ctx context.Context) backend.Availability {
	av := backend.Availability{Name: b.Name()}
	path, err := b.resolveBin()
	if err != nil {
		av.Reason = "llama-server not found on PATH — install llama.cpp (macOS: `brew install llama.cpp`)"
		return av
	}
	av.Present = true
	av.Path = path
	if v := b.version(ctx, path); v != "" {
		av.Version = v
	}
	return av
}

func (b *Backend) version(ctx context.Context, path string) string {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, path, "--version").CombinedOutput()
	if err != nil {
		return ""
	}
	return util.FirstLine(string(out))
}

// Start implements backend.Backend. It launches llama-server on a free port,
// waits for it to become ready, and returns a Runner. The process lifetime is
// detached from ctx (which only bounds the readiness wait) — use Runner.Stop to
// terminate it.
func (b *Backend) Start(ctx context.Context, spec backend.ModelSpec) (backend.Runner, error) {
	bin, err := b.resolveBin()
	if err != nil {
		return nil, err
	}
	if spec.Path == "" {
		return nil, fmt.Errorf("llamacpp: model %q has no weights path", spec.ID)
	}
	if fi, err := os.Stat(spec.Path); err != nil || fi.IsDir() {
		return nil, fmt.Errorf("llamacpp: weights not found: %s", spec.Path)
	}

	port, err := util.FreePort()
	if err != nil {
		return nil, fmt.Errorf("llamacpp: allocate port: %w", err)
	}

	host := b.Host
	if host == "" {
		host = "127.0.0.1"
	}

	args := []string{"-m", spec.Path, "--host", host, "--port", fmt.Sprint(port)}
	if spec.CtxSize > 0 {
		args = append(args, "-c", fmt.Sprint(spec.CtxSize))
	}
	args = append(args, "-ngl", fmt.Sprint(gpuLayersArg(spec.GPULayers)))
	args = append(args, spec.ExtraArgs...)

	// The process must outlive the (possibly request-scoped) startup ctx.
	procCtx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(procCtx, bin, args...)
	logs := util.NewAccumulator(0)
	cmd.Stdout = logs
	cmd.Stderr = logs

	if err := cmd.Start(); err != nil {
		cancel()
		return nil, fmt.Errorf("llamacpp: start %s: %w", bin, err)
	}

	r := &runner{
		baseURL: fmt.Sprintf("http://%s:%d", host, port),
		cmd:     cmd,
		cancel:  cancel,
		logs:    logs,
		spec:    spec,
		mem:     estimateMemory(spec.Path, spec.CtxSize),
	}

	if err := r.waitReady(ctx); err != nil {
		_ = r.Stop(context.Background())
		return nil, err
	}
	return r, nil
}

// kvBytesPerToken is a coarse per-token KV-cache footprint. Real usage depends
// on n_layers/n_kv_heads/head_dim, unknown before load; this is deliberately
// conservative and refined once the engine reports model metadata (Phase 1).
const kvBytesPerToken = 128 << 10

// defaultCtxAssumption is the context window assumed for estimation when a model
// leaves CtxSize unset (engine default is typically ~4k).
const defaultCtxAssumption = 4096

// estimateMemory approximates a model's resident footprint: weights on disk plus
// a KV-cache estimate scaled by context window.
func estimateMemory(path string, ctx int) int64 {
	var weights int64
	if fi, err := os.Stat(path); err == nil {
		weights = fi.Size()
	}
	if ctx <= 0 {
		ctx = defaultCtxAssumption
	}
	return weights + int64(ctx)*kvBytesPerToken
}

// EstimateMemory implements backend.MemoryEstimator for scheduler admission.
func (b *Backend) EstimateMemory(spec backend.ModelSpec) int64 {
	return estimateMemory(spec.Path, spec.CtxSize)
}

// gpuLayersArg maps our convention to llama.cpp's -ngl:
// 0 → offload all (999), N>0 → N, N<0 → CPU only (0).
func gpuLayersArg(n int) int {
	switch {
	case n == 0:
		return 999
	case n < 0:
		return 0
	default:
		return n
	}
}

// runner is a live llama-server process.
type runner struct {
	baseURL string
	cmd     *exec.Cmd
	cancel  context.CancelFunc
	logs    *util.Accumulator
	spec    backend.ModelSpec
	mem     int64

	mu      sync.Mutex
	stopped bool

	capsMu sync.Mutex
	caps   *backend.Capabilities // cached once device+ctx are resolved
}

var healthClient = &http.Client{Timeout: 2 * time.Second}

func (r *runner) BaseURL() string    { return r.baseURL }
func (r *runner) MemoryBytes() int64 { return r.mem }

// waitReady polls /health until the server is ready, the process dies, or ctx
// (or a hard 5-minute ceiling) expires.
func (r *runner) waitReady(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	tick := time.NewTicker(250 * time.Millisecond)
	defer tick.Stop()
	for {
		if r.cmd.ProcessState != nil && r.cmd.ProcessState.Exited() {
			return fmt.Errorf("llamacpp: server exited during load:\n%s", util.Tail(r.logs.String(), 40))
		}
		if r.Health(ctx) == backend.StatusReady {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("llamacpp: server not ready before timeout:\n%s", util.Tail(r.logs.String(), 40))
		case <-tick.C:
		}
	}
}

// Health implements backend.Runner.
func (r *runner) Health(ctx context.Context) backend.Status {
	r.mu.Lock()
	stopped := r.stopped
	r.mu.Unlock()
	if stopped {
		return backend.StatusDown
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, r.baseURL+"/health", nil)
	if err != nil {
		return backend.StatusDown
	}
	resp, err := healthClient.Do(req)
	if err != nil {
		return backend.StatusDown
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
		return backend.StatusReady
	case http.StatusServiceUnavailable:
		return backend.StatusLoading
	default:
		return backend.StatusDown
	}
}

// Capabilities implements backend.Runner: the fail-loud truth. Effective context
// comes from the engine's /props; device and offload are read from the engine's
// own startup logs (the only reliable source), never assumed from the request.
func (r *runner) Capabilities(ctx context.Context) (backend.Capabilities, error) {
	r.capsMu.Lock()
	if r.caps != nil {
		c := *r.caps
		r.capsMu.Unlock()
		return c, nil
	}
	r.capsMu.Unlock()

	caps := backend.Capabilities{
		Backend:      "llamacpp",
		Model:        r.spec.ID,
		RequestedCtx: r.spec.CtxSize,
	}

	effCtx := r.propsCtx(ctx)
	if effCtx > 0 {
		caps.EffectiveCtx = effCtx
	}

	device, offloaded, total, parsed := parseRuntime(r.logs.String())
	if parsed {
		caps.Device = device
		caps.GPUOffload = offloaded > 0
		if offloaded == 0 && r.spec.GPULayers >= 0 {
			caps.Warnings = append(caps.Warnings,
				"GPU offload requested but engine loaded on CPU (silent fallback)")
		} else if total > 0 && offloaded < total {
			caps.Warnings = append(caps.Warnings,
				fmt.Sprintf("partial GPU offload: %d/%d layers", offloaded, total))
		}
	} else {
		caps.Device = "unknown"
		caps.Warnings = append(caps.Warnings, "could not verify device/offload from engine logs")
	}

	if caps.RequestedCtx > 0 && caps.EffectiveCtx > 0 && caps.EffectiveCtx < caps.RequestedCtx {
		caps.Warnings = append(caps.Warnings,
			fmt.Sprintf("context shrunk: requested %d, effective %d", caps.RequestedCtx, caps.EffectiveCtx))
	}

	// Cache once resolved: device and effective ctx are fixed for a runner's
	// lifetime, so subsequent per-request calls avoid the props round-trip and
	// the log scan. If either is still unknown, leave it uncached to retry.
	if parsed && effCtx > 0 {
		c := caps
		r.capsMu.Lock()
		r.caps = &c
		r.capsMu.Unlock()
	}
	return caps, nil
}

// propsCtx fetches the effective n_ctx from llama-server's /props.
func (r *runner) propsCtx(ctx context.Context) int {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, r.baseURL+"/props", nil)
	if err != nil {
		return 0
	}
	resp, err := healthClient.Do(req)
	if err != nil {
		return 0
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0
	}
	var p struct {
		NCtx                      int `json:"n_ctx"`
		DefaultGenerationSettings struct {
			NCtx int `json:"n_ctx"`
		} `json:"default_generation_settings"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&p); err != nil {
		return 0
	}
	if p.NCtx > 0 {
		return p.NCtx
	}
	return p.DefaultGenerationSettings.NCtx
}

// CountTokens implements backend.TokenCounter using llama-server's POST
// /tokenize endpoint, which returns the model's real token ids for the text.
func (r *runner) CountTokens(ctx context.Context, text string) (int, error) {
	body, err := json.Marshal(map[string]string{"content": text})
	if err != nil {
		return 0, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, r.baseURL+"/tokenize", bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := healthClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("tokenize: upstream status %d", resp.StatusCode)
	}
	var out struct {
		Tokens []json.RawMessage `json:"tokens"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(&out); err != nil {
		return 0, err
	}
	return len(out.Tokens), nil
}

// Stop implements backend.Runner. Idempotent; reclaims the process (and its VRAM).
func (r *runner) Stop(ctx context.Context) error {
	r.mu.Lock()
	if r.stopped {
		r.mu.Unlock()
		return nil
	}
	r.stopped = true
	r.mu.Unlock()

	r.cancel() // sends kill via exec.CommandContext
	done := make(chan error, 1)
	go func() { done <- r.cmd.Wait() }()
	select {
	case <-done:
		return nil
	case <-time.After(10 * time.Second):
		if r.cmd.Process != nil {
			_ = r.cmd.Process.Kill()
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// ── log parsing ────────────────────────────────────────────────────────────────

var offloadRe = regexp.MustCompile(`offloaded (\d+)/(\d+) layers to GPU`)

// parseRuntime reads llama.cpp's startup logs for the device and GPU-offload
// counts. parsed is false when the logs don't yet contain the markers.
func parseRuntime(logs string) (device string, offloaded, total int, parsed bool) {
	lower := strings.ToLower(logs)
	switch {
	case strings.Contains(lower, "metal"):
		device = "metal"
	case strings.Contains(lower, "cuda"):
		device = "cuda"
	case strings.Contains(lower, "rocm"), strings.Contains(lower, "hipblas"):
		device = "rocm"
	case strings.Contains(lower, "vulkan"):
		device = "vulkan"
	default:
		device = "cpu"
	}
	if m := offloadRe.FindStringSubmatch(logs); m != nil {
		offloaded = atoi(m[1])
		total = atoi(m[2])
		return device, offloaded, total, true
	}
	return device, 0, 0, false
}

func atoi(s string) int {
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			return n
		}
		n = n*10 + int(c-'0')
	}
	return n
}
