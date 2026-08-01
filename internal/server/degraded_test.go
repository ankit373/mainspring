package server_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ankit373/mainspring/internal/auth"
	"github.com/ankit373/mainspring/internal/backend"
	"github.com/ankit373/mainspring/internal/metrics"
	"github.com/ankit373/mainspring/internal/scheduler"
	"github.com/ankit373/mainspring/internal/server"
)

// cpuOnlyBackend reports a model intentionally running CPU-only (as if
// configured with gpu_layers < 0): GPUOffload is false, but — because this was
// requested, not a silent fallback — there is no warning. This is the exact
// shape that used to trip the Degraded() false positive.
type cpuOnlyBackend struct{ baseURL string }

func (b cpuOnlyBackend) Name() string { return "fake" }
func (b cpuOnlyBackend) Detect(context.Context) backend.Availability {
	return backend.Availability{Name: "fake", Present: true}
}
func (b cpuOnlyBackend) Start(_ context.Context, spec backend.ModelSpec) (backend.Runner, error) {
	return &cpuOnlyRunner{base: b.baseURL, id: spec.ID}, nil
}

type cpuOnlyRunner struct {
	base string
	id   string
}

func (r *cpuOnlyRunner) BaseURL() string                       { return r.base }
func (r *cpuOnlyRunner) Health(context.Context) backend.Status { return backend.StatusReady }
func (r *cpuOnlyRunner) MemoryBytes() int64                    { return 1 }
func (r *cpuOnlyRunner) Stop(context.Context) error            { return nil }
func (r *cpuOnlyRunner) Capabilities(context.Context) (backend.Capabilities, error) {
	return backend.Capabilities{
		Backend: "fake", Model: r.id, Device: "cpu",
		GPUOffload: false, RequestedCtx: 4096, EffectiveCtx: 4096,
		// No Warnings: this is intentional CPU-only, not a silent fallback.
	}, nil
}

// TestIntentionalCPUOnlyModelNotFlaggedDegraded proves the fix end-to-end: a
// model deliberately configured CPU-only must not carry X-Mainspring-Warning
// or report degraded via /v1/quality.
func TestIntentionalCPUOnlyModelNotFlaggedDegraded(t *testing.T) {
	eng := fakeEngine(t)
	sched := scheduler.New(
		map[string]backend.Backend{"fake": cpuOnlyBackend{baseURL: eng.URL}},
		[]backend.ModelSpec{{ID: "m1", Backend: "fake", GPULayers: -1}},
		scheduler.Options{MaxLoaded: 2},
	)
	rec, _ := metrics.New("")
	h := server.New(sched, auth.New(nil), rec).Handler()

	w := postBody(h, `{"model":"m1","messages":[]}`)
	if w.Code != 200 {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	if got := w.Header().Get("X-Mainspring-Warning"); got != "" {
		t.Fatalf("intentional CPU-only model should carry no warning, got %q", got)
	}
	if w.Header().Get("X-Mainspring-Device") != "cpu" {
		t.Fatalf("device header should still honestly report cpu, got %q", w.Header().Get("X-Mainspring-Device"))
	}

	qw := httptest.NewRecorder()
	h.ServeHTTP(qw, httptest.NewRequest(http.MethodGet, "/v1/quality", nil))
	if strings.Contains(qw.Body.String(), `"degraded":true`) {
		t.Fatalf("quality should not report degraded for intentional CPU-only: %s", qw.Body.String())
	}
}
