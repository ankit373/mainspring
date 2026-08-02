package llamacpp

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

// fakeEngine writes an executable stand-in for llama-server and a dummy weights
// file, and returns the two paths. script is a /bin/sh body.
func fakeEngine(t *testing.T, script string) (bin, weights string) {
	t.Helper()
	dir := t.TempDir()
	bin = filepath.Join(dir, "llama-server")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n"+script+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	weights = filepath.Join(dir, "model.gguf")
	if err := os.WriteFile(weights, []byte("not really a model"), 0o644); err != nil {
		t.Fatal(err)
	}
	return bin, weights
}

// A server that dies on startup must be reported immediately, with the engine's
// own output attached.
//
// waitReady used to test `r.cmd.ProcessState != nil && …Exited()`, but
// ProcessState is populated only by cmd.Wait(), and nothing called Wait until
// Stop. The branch could therefore never be taken: a bad weights file or an
// engine built without the right backend polled a dead process for the full
// five-minute ceiling and then blamed a timeout, discarding the one thing that
// explained it — the engine's stderr.
func TestStartFailsFastWhenTheEngineDies(t *testing.T) {
	bin, weights := fakeEngine(t, `echo "error: unable to load model" >&2; exit 1`)
	b := New(bin)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	start := time.Now()
	_, err := b.Start(ctx, backend.ModelSpec{ID: "m", Path: weights})
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("starting an engine that exits immediately must fail")
	}
	// The point of the check: don't wait out the ceiling for a process that is
	// already gone. Generous bound so a loaded CI box doesn't flake.
	if elapsed > 10*time.Second {
		t.Errorf("took %v to notice the engine had exited; it should be near-immediate", elapsed)
	}
	if !strings.Contains(err.Error(), "exited") {
		t.Errorf("error should say the engine exited, got: %v", err)
	}
	// Without the engine's own stderr the operator has nothing to act on.
	if !strings.Contains(err.Error(), "unable to load model") {
		t.Errorf("error should carry the engine's output, got: %v", err)
	}
}

// Stop must stay correct now that a goroutine reaps the process: calling
// cmd.Wait twice returns an error and races, so Stop has to observe the reaper
// rather than call Wait itself. It must also stay idempotent.
func TestStopIsIdempotentAlongsideTheReaper(t *testing.T) {
	// exec, so the shell replaces itself: a forked child would outlive the kill
	// still holding the stdout pipe, and Wait would block on it for 10s.
	bin, weights := fakeEngine(t, `exec sleep 30`)
	b := New(bin)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	// Never becomes ready, so Start fails — but it must fail by timeout, having
	// already stopped the process it spawned.
	if _, err := b.Start(ctx, backend.ModelSpec{ID: "m", Path: weights}); err == nil {
		t.Fatal("a server that never becomes ready must fail to start")
	}
}

// newTestRunner points a runner at an httptest server instead of a real engine.
func newTestRunner(url string, spec backend.ModelSpec, logs string) *runner {
	acc := util.NewAccumulator(0)
	_, _ = acc.Write([]byte(logs))
	return &runner{baseURL: url, spec: spec, logs: acc}
}

func TestHealthMapsUpstreamStatus(t *testing.T) {
	var code int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(code)
	}))
	defer srv.Close()

	for _, tc := range []struct {
		status int
		want   backend.Status
	}{
		{http.StatusOK, backend.StatusReady},
		{http.StatusServiceUnavailable, backend.StatusLoading}, // still loading the model
		{http.StatusInternalServerError, backend.StatusDown},
	} {
		code = tc.status
		r := newTestRunner(srv.URL, backend.ModelSpec{ID: "m"}, "")
		if got := r.Health(context.Background()); got != tc.want {
			t.Errorf("status %d → %v, want %v", tc.status, got, tc.want)
		}
	}

	// A stopped runner is Down without touching the network.
	r := newTestRunner(srv.URL, backend.ModelSpec{ID: "m"}, "")
	r.stopped = true
	if got := r.Health(context.Background()); got != backend.StatusDown {
		t.Errorf("stopped runner → %v, want Down", got)
	}
}

func TestPropsCtx(t *testing.T) {
	tests := []struct {
		name, body string
		status     int
		want       int
	}{
		{"top-level n_ctx", `{"n_ctx": 8192}`, 200, 8192},
		// Newer llama-server reports it only under default_generation_settings.
		{"nested n_ctx", `{"default_generation_settings":{"n_ctx":4096}}`, 200, 4096},
		{"top level wins", `{"n_ctx":8192,"default_generation_settings":{"n_ctx":4096}}`, 200, 8192},
		{"non-200 is unknown", `{"n_ctx": 8192}`, 500, 0},
		{"garbage is unknown", `not json`, 200, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()
			r := newTestRunner(srv.URL, backend.ModelSpec{ID: "m"}, "")
			if got := r.propsCtx(context.Background()); got != tc.want {
				t.Errorf("propsCtx = %d, want %d", got, tc.want)
			}
		})
	}
}

// Capabilities is the fail-loud surface: what it reports is what the operator
// and Hydra believe actually happened.
func TestCapabilities(t *testing.T) {
	const cudaLogs = "ggml_cuda_init: found 1 CUDA device\noffloaded 32/32 layers to GPU\n"

	tests := []struct {
		name       string
		logs       string
		spec       backend.ModelSpec
		nCtx       int
		wantDevice string
		wantGPU    bool
		wantWarn   string
	}{
		{
			name: "full GPU offload, context honoured", logs: cudaLogs,
			spec: backend.ModelSpec{ID: "m", CtxSize: 8192}, nCtx: 8192,
			wantDevice: "cuda", wantGPU: true,
		},
		{
			// The silent failure this project exists to surface.
			name: "GPU requested but loaded on CPU",
			logs: "offloaded 0/32 layers to GPU\n",
			spec: backend.ModelSpec{ID: "m", CtxSize: 4096}, nCtx: 4096,
			wantDevice: "cpu", wantGPU: false,
			wantWarn: "silent fallback",
		},
		{
			name: "partial offload is called out", logs: "ggml_cuda_init\noffloaded 10/32 layers to GPU\n",
			spec: backend.ModelSpec{ID: "m", CtxSize: 4096}, nCtx: 4096,
			wantDevice: "cuda", wantGPU: true,
			wantWarn: "partial GPU offload: 10/32",
		},
		{
			// -1 means "CPU only, deliberately", so no fallback warning is due.
			name: "explicit CPU-only is not a fallback",
			logs: "offloaded 0/32 layers to GPU\n",
			spec: backend.ModelSpec{ID: "m", CtxSize: 4096, GPULayers: -1}, nCtx: 4096,
			wantDevice: "cpu", wantGPU: false,
		},
		{
			name: "context silently shrunk by the engine", logs: cudaLogs,
			spec: backend.ModelSpec{ID: "m", CtxSize: 32768}, nCtx: 4096,
			wantDevice: "cuda", wantGPU: true,
			wantWarn: "context shrunk: requested 32768, effective 4096",
		},
		{
			name: "unparseable logs are admitted, not guessed at", logs: "starting up\n",
			spec: backend.ModelSpec{ID: "m", CtxSize: 4096}, nCtx: 4096,
			wantDevice: "unknown",
			wantWarn:   "could not verify device/offload",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(`{"n_ctx":` + itoa(tc.nCtx) + `}`))
			}))
			defer srv.Close()

			r := newTestRunner(srv.URL, tc.spec, tc.logs)
			caps, err := r.Capabilities(context.Background())
			if err != nil {
				t.Fatalf("Capabilities: %v", err)
			}
			if caps.Device != tc.wantDevice {
				t.Errorf("Device = %q, want %q", caps.Device, tc.wantDevice)
			}
			if caps.GPUOffload != tc.wantGPU {
				t.Errorf("GPUOffload = %v, want %v", caps.GPUOffload, tc.wantGPU)
			}
			if caps.EffectiveCtx != tc.nCtx {
				t.Errorf("EffectiveCtx = %d, want %d", caps.EffectiveCtx, tc.nCtx)
			}
			joined := strings.Join(caps.Warnings, " | ")
			if tc.wantWarn == "" {
				if len(caps.Warnings) > 0 {
					t.Errorf("unexpected warnings: %s", joined)
				}
			} else if !strings.Contains(joined, tc.wantWarn) {
				t.Errorf("warnings %q missing %q", joined, tc.wantWarn)
			}
		})
	}
}

// #135 removed a standalone !GPUOffload check that flagged every CPU-only model
// as degraded. Degraded() now defers entirely to the backend's warnings, on the
// documented contract that a backend reports an honoured CPU-only request with
// GPUOffload=false *and no warning*. This backend broke that contract from the
// other side: with GPULayers < 0 and logs reading "offloaded 0/32", the partial
// branch fired and produced "partial GPU offload: 0/32 layers" — resurrecting
// the same false positive through the warning path. Zero offload is never
// partial.
func TestDeliberateCPUOnlyIsNotDegraded(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"n_ctx":4096}`))
	}))
	defer srv.Close()

	r := newTestRunner(srv.URL,
		backend.ModelSpec{ID: "m", CtxSize: 4096, GPULayers: -1}, // CPU-only, on purpose
		"offloaded 0/32 layers to GPU\n")
	caps, err := r.Capabilities(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if caps.Degraded() {
		t.Errorf("a deliberately CPU-only model must not be degraded; warnings: %v", caps.Warnings)
	}
	if caps.GPUOffload {
		t.Error("GPUOffload should be false for a CPU-only run")
	}
}

// Device and effective context are fixed for a runner's lifetime, so they are
// cached — but only once both are actually known, or a transient /props failure
// would be frozen in as the truth.
func TestCapabilitiesCachesOnlyOnceResolved(t *testing.T) {
	var hits int
	var body string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits++
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()

	// /props is failing, so effective ctx is unknown: nothing may be cached.
	body = `{}`
	r := newTestRunner(srv.URL, backend.ModelSpec{ID: "m", CtxSize: 4096}, "offloaded 32/32 layers to GPU\n")
	if _, err := r.Capabilities(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Capabilities(context.Background()); err != nil {
		t.Fatal(err)
	}
	if hits != 2 {
		t.Fatalf("unresolved capabilities must be retried, got %d /props calls", hits)
	}

	// Now it answers; the result is resolved and may be cached.
	body = `{"n_ctx":4096}`
	if _, err := r.Capabilities(context.Background()); err != nil {
		t.Fatal(err)
	}
	before := hits
	caps, err := r.Capabilities(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if hits != before {
		t.Errorf("resolved capabilities should be cached, got another /props call")
	}
	if caps.EffectiveCtx != 4096 {
		t.Errorf("EffectiveCtx = %d, want 4096", caps.EffectiveCtx)
	}
}

func TestCountTokens(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/tokenize" {
			t.Errorf("tokenize should POST /tokenize, got %s", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"tokens":[1,2,3,4,5]}`))
	}))
	defer srv.Close()

	r := newTestRunner(srv.URL, backend.ModelSpec{ID: "m"}, "")
	n, err := r.CountTokens(context.Background(), "hello there")
	if err != nil {
		t.Fatalf("CountTokens: %v", err)
	}
	if n != 5 {
		t.Errorf("CountTokens = %d, want 5", n)
	}

	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer bad.Close()
	// An error must surface: the caller downgrades to an estimate and labels it,
	// which only works if the failure is reported rather than counted as zero.
	if _, err := newTestRunner(bad.URL, backend.ModelSpec{ID: "m"}, "").
		CountTokens(context.Background(), "x"); err == nil {
		t.Error("a non-200 from /tokenize must be an error, not 0 tokens")
	}
}

func TestEstimateMemory(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "w.gguf")
	if err := os.WriteFile(p, make([]byte, 1024), 0o644); err != nil {
		t.Fatal(err)
	}
	// An unset context window must not estimate as weights-only, or the byte
	// budget would admit far more than actually fits.
	if got, want := estimateMemory(p, 0), int64(1024)+defaultCtxAssumption*kvBytesPerToken; got != want {
		t.Errorf("estimateMemory(unset ctx) = %d, want %d", got, want)
	}
	if got, want := estimateMemory(p, 8192), int64(1024)+8192*kvBytesPerToken; got != want {
		t.Errorf("estimateMemory(8192) = %d, want %d", got, want)
	}
	// A missing file is an estimate of the KV cache alone, not a crash.
	if got := estimateMemory(filepath.Join(dir, "nope.gguf"), 4096); got != 4096*kvBytesPerToken {
		t.Errorf("estimateMemory(missing) = %d", got)
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}
