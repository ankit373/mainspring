// Package mlx is the Apple-Silicon companion backend: it supervises Apple MLX's
// `mlx_lm.server` (run via `python -m mlx_lm.server`) as a subprocess and proxies
// its OpenAI-compatible endpoints. Like the llamacpp backend it is a thin
// supervisor, never a kernel — MLX does the inference on the unified-memory GPU.
package mlx

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"time"

	"github.com/ankit373/mainspring/internal/backend"
	"github.com/ankit373/mainspring/internal/util"
)

// Backend supervises mlx_lm.server processes.
type Backend struct {
	Python string // python interpreter; "" => "python3"
	Host   string // loopback bind address
}

// New returns an MLX backend. python "" resolves to python3 on PATH.
func New(python string) *Backend {
	if python == "" {
		python = "python3"
	}
	return &Backend{Python: python, Host: "127.0.0.1"}
}

func (b *Backend) Name() string { return "mlx" }

func (b *Backend) python() (string, error) { return exec.LookPath(b.Python) }

// Detect reports whether MLX is usable: Apple Silicon + an importable mlx_lm.
func (b *Backend) Detect(ctx context.Context) backend.Availability {
	av := backend.Availability{Name: "mlx"}
	if runtime.GOOS != "darwin" || runtime.GOARCH != "arm64" {
		av.Reason = "MLX requires Apple Silicon (darwin/arm64); this host is " + runtime.GOOS + "/" + runtime.GOARCH
		return av
	}
	py, err := b.python()
	if err != nil {
		av.Reason = "python interpreter " + b.Python + " not found on PATH"
		return av
	}
	av.Path = py
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	// Import check + version in one shot.
	out, err := exec.CommandContext(ctx, py, "-c",
		"import mlx_lm,sys; sys.stdout.write(getattr(mlx_lm,'__version__',''))").CombinedOutput()
	if err != nil {
		av.Reason = "mlx_lm not importable — install with `pip install mlx-lm` (Apple Silicon only)"
		return av
	}
	av.Present = true
	av.Version = util.FirstLine(string(out))
	return av
}

// Start launches `python -m mlx_lm.server --model <path> --port <p>` and waits
// for readiness. Process lifetime is detached from ctx (use Runner.Stop).
func (b *Backend) Start(ctx context.Context, spec backend.ModelSpec) (backend.Runner, error) {
	py, err := b.python()
	if err != nil {
		return nil, err
	}
	if spec.Path == "" {
		return nil, fmt.Errorf("mlx: model %q has no path (local dir or HF repo id)", spec.ID)
	}
	port, err := util.FreePort()
	if err != nil {
		return nil, fmt.Errorf("mlx: allocate port: %w", err)
	}
	host := b.Host
	if host == "" {
		host = "127.0.0.1"
	}

	args := buildArgs(spec, host, port)
	procCtx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(procCtx, py, args...)
	logs := util.NewAccumulator(0)
	cmd.Stdout = logs
	cmd.Stderr = logs
	if err := cmd.Start(); err != nil {
		cancel()
		return nil, fmt.Errorf("mlx: start: %w", err)
	}

	r := &runner{
		baseURL: fmt.Sprintf("http://%s:%d", host, port),
		cmd:     cmd,
		cancel:  cancel,
		logs:    logs,
		spec:    spec,
		mem:     dirOrFileSize(spec.Path),
		exited:  make(chan struct{}),
	}
	// One reaper, so a server that dies is observable rather than something we
	// only infer from a socket that stopped answering. Python backends make this
	// especially worth having: a missing wheel or an unsupported model exits in
	// milliseconds with a traceback that says exactly what is wrong.
	go func() {
		r.waitErr = r.cmd.Wait()
		close(r.exited)
	}()

	if err := r.waitReady(ctx); err != nil {
		_ = r.Stop(context.Background())
		return nil, err
	}
	return r, nil
}

// buildArgs assembles the `python -m mlx_lm server` argument vector.
//
// CtxSize is deliberately not passed. mlx_lm.server has no context-window flag
// — verified against 0.31.3, whose only related option is:
//
//	--max-tokens MAX_TOKENS   Default maximum number of tokens to generate
//
// which is the generation cap, a different setting. Mapping ctx onto it meant
// `ctx: 8192` quietly reconfigured how much the server would generate rather
// than how much context it would keep. Config.Lint warns when an mlx model sets
// ctx, so the setting is reported as unsupported instead of silently reinterpreted;
// ExtraArgs remains the way to set a real generation cap on purpose.
//
// The subcommand form is `-m mlx_lm server`, not `-m mlx_lm.server`: 0.31.3
// prints a deprecation notice for the latter.
func buildArgs(spec backend.ModelSpec, host string, port int) []string {
	args := []string{"-m", "mlx_lm", "server", "--model", spec.Path, "--host", host, "--port", fmt.Sprint(port)}
	return append(args, spec.ExtraArgs...)
}

// EstimateMemory implements backend.MemoryEstimator (weights size proxy).
func (b *Backend) EstimateMemory(spec backend.ModelSpec) int64 { return dirOrFileSize(spec.Path) }

// runner is a live mlx_lm.server process.
type runner struct {
	baseURL string
	cmd     *exec.Cmd
	cancel  context.CancelFunc
	logs    *util.Accumulator
	spec    backend.ModelSpec
	mem     int64

	mu      sync.Mutex
	stopped bool

	// exited is closed by the reaper goroutine once the process has been waited
	// on; waitErr is set before the close, so receiving from exited establishes
	// happens-before and makes waitErr safe to read. See the same construction in
	// internal/backend/llamacpp: cmd.ProcessState cannot serve this purpose,
	// because only Wait populates it.
	exited  chan struct{}
	waitErr error
}

var healthClient = &http.Client{Timeout: 2 * time.Second}

func (r *runner) BaseURL() string    { return r.baseURL }
func (r *runner) MemoryBytes() int64 { return r.mem }

// waitReady polls /v1/models (mlx_lm.server has no /health) until ready.
func (r *runner) waitReady(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	tick := time.NewTicker(300 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-r.exited:
			return r.exitErr()
		default:
		}
		if r.Health(ctx) == backend.StatusReady {
			return nil
		}
		select {
		case <-r.exited:
			return r.exitErr()
		case <-ctx.Done():
			return fmt.Errorf("mlx: server not ready before timeout:\n%s", util.Tail(r.logs.String(), 40))
		case <-tick.C:
		}
	}
}

// exitErr describes a process that died during load. Only safe to call once
// r.exited is closed. The traceback is the whole point: reporting a timeout
// instead discards it and points at the wrong cause.
func (r *runner) exitErr() error {
	if r.waitErr != nil {
		return fmt.Errorf("mlx: server exited during load (%v):\n%s",
			r.waitErr, util.Tail(r.logs.String(), 40))
	}
	return fmt.Errorf("mlx: server exited during load:\n%s", util.Tail(r.logs.String(), 40))
}

func (r *runner) Health(ctx context.Context) backend.Status {
	r.mu.Lock()
	stopped := r.stopped
	r.mu.Unlock()
	if stopped {
		return backend.StatusDown
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, r.baseURL+"/v1/models", nil)
	if err != nil {
		return backend.StatusDown
	}
	resp, err := healthClient.Do(req)
	if err != nil {
		return backend.StatusDown
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		return backend.StatusReady
	}
	return backend.StatusLoading
}

// Capabilities: MLX runs on the Apple Silicon unified-memory GPU (Metal) by
// construction, so offload is always true — no silent CPU fallback to detect.
//
// EffectiveCtx is left at 0, meaning unknown, the same convention llamacpp uses
// when /props cannot be read. mlx_lm.server exposes no context-window setting
// and reports no effective window, so there is nothing to measure. It used to
// echo RequestedCtx, which made EffectiveCtx < RequestedCtx impossible and the
// shrink warning dead — a guess presented as a measurement, in the one field
// documented as "the context window that is REALLY in effect".
//
// No warning is attached: this is a permanent property of the engine, and
// Degraded() is true for any warning, so one here would mark every MLX model
// degraded forever.
func (r *runner) Capabilities(context.Context) (backend.Capabilities, error) {
	return backend.Capabilities{
		Backend:      "mlx",
		Model:        r.spec.ID,
		Device:       "metal",
		GPUOffload:   true,
		RequestedCtx: r.spec.CtxSize,
	}, nil
}

func (r *runner) Stop(ctx context.Context) error {
	r.mu.Lock()
	if r.stopped {
		r.mu.Unlock()
		return nil
	}
	r.stopped = true
	r.mu.Unlock()

	r.cancel()
	// Observe the reaper started in Start rather than calling Wait again: a
	// second Wait on the same Cmd returns an error and races the first.
	select {
	case <-r.exited:
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

// dirOrFileSize returns the total size of a model path (dir tree or single file);
// 0 for a bare HF repo id that isn't a local path.
func dirOrFileSize(path string) int64 {
	fi, err := os.Stat(path)
	if err != nil {
		return 0
	}
	if !fi.IsDir() {
		return fi.Size()
	}
	var total int64
	_ = filepath.Walk(path, func(_ string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() {
			total += info.Size()
		}
		return nil
	})
	return total
}
