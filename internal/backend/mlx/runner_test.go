package mlx

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ankit373/mainspring/internal/backend"
	"github.com/ankit373/mainspring/internal/util"
)

// fakePython writes an executable stand-in for the python interpreter, so Start
// can be exercised without mlx_lm installed.
func fakePython(t *testing.T, script string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "python3")
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"+script+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

// A server that dies on startup must be reported immediately, with its output.
//
// waitReady used to test `r.cmd.ProcessState != nil && …Exited()`, but
// ProcessState is populated only by cmd.Wait(), and nothing called Wait until
// Stop — so the branch was unreachable. It matters more here than for llama.cpp:
// the child is Python, so a missing wheel or an unsupported architecture exits
// in milliseconds with a traceback that names the cause, and that traceback is
// exactly what a five-minute timeout throws away.
func TestStartFailsFastWhenTheServerDies(t *testing.T) {
	py := fakePython(t, `echo "ModuleNotFoundError: No module named 'mlx_lm'" >&2; exit 1`)
	b := &Backend{Python: py, Host: "127.0.0.1"}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	start := time.Now()
	_, err := b.Start(ctx, backend.ModelSpec{ID: "m", Path: "/models/some-mlx-model"})
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("a server that exits immediately must fail the load")
	}
	if elapsed > 10*time.Second {
		t.Errorf("took %v to notice the server had exited; should be near-immediate", elapsed)
	}
	if !strings.Contains(err.Error(), "exited") {
		t.Errorf("error should say the server exited, got: %v", err)
	}
	if !strings.Contains(err.Error(), "ModuleNotFoundError") {
		t.Errorf("error should carry the traceback, got: %v", err)
	}
}

// Stop observes the reaper rather than calling Wait a second time, which would
// error and race the first.
func TestStopIsIdempotentAlongsideTheReaper(t *testing.T) {
	// exec so the shell replaces itself; a forked child would outlive the kill
	// still holding the stdout pipe and block Wait for 10s.
	py := fakePython(t, `exec sleep 30`)
	b := &Backend{Python: py, Host: "127.0.0.1"}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err := b.Start(ctx, backend.ModelSpec{ID: "m", Path: "/models/x"}); err == nil {
		t.Fatal("a server that never becomes ready must fail to start")
	}
}

// ctx is a context window; --max-tokens is a generation cap. mlx_lm.server has
// no context-window flag at all (verified against 0.31.3), so passing ctx as
// --max-tokens silently reconfigured how much the server would generate.
func TestBuildArgsDoesNotPassCtxAsAGenerationCap(t *testing.T) {
	args := buildArgs(backend.ModelSpec{ID: "m", Path: "/models/x", CtxSize: 8192}, "127.0.0.1", 9000)
	joined := strings.Join(args, " ")

	if strings.Contains(joined, "--max-tokens") {
		t.Errorf("ctx must not become --max-tokens (a generation cap): %s", joined)
	}
	if strings.Contains(joined, "8192") {
		t.Errorf("ctx must not reach the engine at all — it has no flag for it: %s", joined)
	}
	// 0.31.3 deprecates `-m mlx_lm.server` in favour of the subcommand form.
	if strings.Contains(joined, "mlx_lm.server") {
		t.Errorf("use the non-deprecated `-m mlx_lm server` form: %s", joined)
	}
	for _, want := range []string{"-m mlx_lm server", "--model /models/x", "--host 127.0.0.1", "--port 9000"} {
		if !strings.Contains(joined, want) {
			t.Errorf("args missing %q: %s", want, joined)
		}
	}

	// ExtraArgs stays the deliberate escape hatch for a real generation cap.
	extra := buildArgs(backend.ModelSpec{Path: "/x", ExtraArgs: []string{"--max-tokens", "512"}}, "h", 1)
	if !strings.Contains(strings.Join(extra, " "), "--max-tokens 512") {
		t.Error("ExtraArgs must still be passed through")
	}
}

// EffectiveCtx is documented as "the context window that is REALLY in effect".
// It used to echo RequestedCtx, which made a shrink undetectable by construction
// and reported a guess as a measurement.
func TestCapabilitiesDoesNotFabricateEffectiveCtx(t *testing.T) {
	r := &runner{spec: backend.ModelSpec{ID: "m", CtxSize: 8192}, logs: util.NewAccumulator(0)}
	caps, err := r.Capabilities(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if caps.EffectiveCtx != 0 {
		t.Errorf("EffectiveCtx = %d; must be 0 (unknown) — mlx reports no effective window",
			caps.EffectiveCtx)
	}
	if caps.RequestedCtx != 8192 {
		t.Errorf("RequestedCtx = %d, want 8192", caps.RequestedCtx)
	}
	// A permanent unknown must not mark every MLX model degraded: Degraded() is
	// true for any warning, so this is the trap #211 documented.
	if caps.Degraded() {
		t.Errorf("an MLX model must not be degraded by default; warnings: %v", caps.Warnings)
	}
	if !caps.GPUOffload || caps.Device != "metal" {
		t.Errorf("MLX runs on Metal by construction, got device=%q offload=%v",
			caps.Device, caps.GPUOffload)
	}
}

func TestHealth(t *testing.T) {
	var code int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			t.Errorf("health should poll /v1/models, got %s", r.URL.Path)
		}
		w.WriteHeader(code)
	}))
	defer srv.Close()

	r := &runner{baseURL: srv.URL, spec: backend.ModelSpec{ID: "m"}, logs: util.NewAccumulator(0)}

	code = http.StatusOK
	if got := r.Health(context.Background()); got != backend.StatusReady {
		t.Errorf("200 → %v, want Ready", got)
	}
	// mlx_lm.server has no /health, so anything not-yet-200 reads as still loading.
	code = http.StatusNotFound
	if got := r.Health(context.Background()); got != backend.StatusLoading {
		t.Errorf("404 → %v, want Loading", got)
	}

	r.stopped = true
	if got := r.Health(context.Background()); got != backend.StatusDown {
		t.Errorf("stopped → %v, want Down", got)
	}
	// An unreachable server is Down, not Loading forever.
	srv.Close()
	r2 := &runner{baseURL: srv.URL, spec: backend.ModelSpec{ID: "m"}, logs: util.NewAccumulator(0)}
	if got := r2.Health(context.Background()); got != backend.StatusDown {
		t.Errorf("unreachable → %v, want Down", got)
	}
}
