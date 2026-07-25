package server_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/ankit373/mainspring/internal/auth"
	"github.com/ankit373/mainspring/internal/backend"
	"github.com/ankit373/mainspring/internal/metrics"
	"github.com/ankit373/mainspring/internal/scheduler"
	"github.com/ankit373/mainspring/internal/server"
)

// fakeEngine is an httptest server that speaks the minimal OpenAI surface we
// proxy: non-streaming JSON and SSE streaming for /v1/chat/completions.
func fakeEngine(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if strings.Contains(string(body), `"stream":true`) {
			w.Header().Set("Content-Type", "text/event-stream")
			fl := w.(http.Flusher)
			for _, tok := range []string{"Hel", "lo"} {
				_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\""+tok+"\"}}]}\n\n")
				fl.Flush()
			}
			_, _ = io.WriteString(w, "data: [DONE]\n\n")
			fl.Flush()
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"Hello"}}]}`)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// engineBackend is a backend.Backend whose runner proxies to a fixed base URL.
type engineBackend struct {
	baseURL  string
	degraded bool
	starts   atomic.Int64
}

func (b *engineBackend) Name() string { return "fake" }
func (b *engineBackend) Detect(context.Context) backend.Availability {
	return backend.Availability{Name: "fake", Present: true}
}
func (b *engineBackend) Start(_ context.Context, spec backend.ModelSpec) (backend.Runner, error) {
	b.starts.Add(1)
	return &engineRunner{base: b.baseURL, id: spec.ID, degraded: b.degraded}, nil
}

type engineRunner struct {
	base     string
	id       string
	degraded bool
}

func (r *engineRunner) BaseURL() string                       { return r.base }
func (r *engineRunner) Health(context.Context) backend.Status { return backend.StatusReady }
func (r *engineRunner) MemoryBytes() int64                    { return 1 }
func (r *engineRunner) Stop(context.Context) error            { return nil }
func (r *engineRunner) Capabilities(context.Context) (backend.Capabilities, error) {
	c := backend.Capabilities{Backend: "fake", Model: r.id, Device: "metal", GPUOffload: true, RequestedCtx: 4096, EffectiveCtx: 4096}
	if r.degraded {
		c.Device = "cpu"
		c.GPUOffload = false
		c.Warnings = []string{"GPU offload requested but engine loaded on CPU (silent fallback)"}
	}
	return c, nil
}

func newTestServer(t *testing.T, be backend.Backend, keys []string) http.Handler {
	t.Helper()
	sched := scheduler.New(
		map[string]backend.Backend{"fake": be},
		[]backend.ModelSpec{{ID: "m1", Backend: "fake"}},
		scheduler.Options{MaxLoaded: 2},
	)
	rec, _ := metrics.New("") // in-memory only
	return server.New(sched, auth.New(keys), rec).Handler()
}

func TestLoopNonStreaming(t *testing.T) {
	eng := fakeEngine(t)
	h := newTestServer(t, &engineBackend{baseURL: eng.URL}, nil)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"m1","messages":[]}`))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != 200 {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "Hello") {
		t.Fatalf("proxied body missing content: %s", w.Body.String())
	}
	if w.Header().Get("X-Mainspring-Backend") != "fake" {
		t.Fatalf("missing fail-loud backend header: %v", w.Header())
	}
	if w.Header().Get("X-Mainspring-Device") != "metal" {
		t.Fatalf("expected device=metal, got %q", w.Header().Get("X-Mainspring-Device"))
	}
	if w.Header().Get("X-Mainspring-Warning") != "" {
		t.Fatalf("healthy model should carry no warning: %q", w.Header().Get("X-Mainspring-Warning"))
	}
}

func TestLoopStreaming(t *testing.T) {
	eng := fakeEngine(t)
	h := newTestServer(t, &engineBackend{baseURL: eng.URL}, nil)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"m1","stream":true,"messages":[]}`))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	body := w.Body.String()
	if !strings.Contains(body, `"delta"`) || !strings.Contains(body, "[DONE]") {
		t.Fatalf("SSE stream not proxied intact: %q", body)
	}
	if ct := w.Header().Get("Content-Type"); !strings.Contains(ct, "event-stream") {
		t.Fatalf("expected SSE content-type, got %q", ct)
	}
}

func TestFailLoudWarningPropagates(t *testing.T) {
	eng := fakeEngine(t)
	h := newTestServer(t, &engineBackend{baseURL: eng.URL, degraded: true}, nil)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"m1","messages":[]}`))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Header().Get("X-Mainspring-Device") != "cpu" {
		t.Fatalf("expected device=cpu on degraded model")
	}
	if !strings.Contains(w.Header().Get("X-Mainspring-Warning"), "CPU") {
		t.Fatalf("expected CPU-fallback warning header, got %q", w.Header().Get("X-Mainspring-Warning"))
	}
}

func TestModelAliasResolvesAndRewritesBody(t *testing.T) {
	// Engine records the "model" it actually received so we can assert the alias
	// was rewritten to the real id before proxying.
	var gotModel string
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var p struct {
			Model string `json:"model"`
		}
		_ = json.Unmarshal(body, &p)
		gotModel = p.Model
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"ok"}}]}`)
	})
	eng := httptest.NewServer(mux)
	t.Cleanup(eng.Close)

	sched := scheduler.New(
		map[string]backend.Backend{"fake": &engineBackend{baseURL: eng.URL}},
		[]backend.ModelSpec{{ID: "m1", Backend: "fake"}},
		scheduler.Options{MaxLoaded: 2, Aliases: map[string]string{"gpt-4o": "m1"}},
	)
	rec, _ := metrics.New("")
	h := server.New(sched, auth.New(nil), rec).Handler()

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"gpt-4o","messages":[]}`))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	if gotModel != "m1" {
		t.Fatalf("upstream received model=%q, want the resolved id m1", gotModel)
	}

	// /v1/models lists the alias alongside the real model.
	mreq := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	mw := httptest.NewRecorder()
	h.ServeHTTP(mw, mreq)
	if !strings.Contains(mw.Body.String(), "gpt-4o") || !strings.Contains(mw.Body.String(), "m1") {
		t.Fatalf("/v1/models should list alias and real id: %s", mw.Body.String())
	}
}

func TestUnknownAndMissingModel(t *testing.T) {
	eng := fakeEngine(t)
	h := newTestServer(t, &engineBackend{baseURL: eng.URL}, nil)

	// unknown model → 404
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"nope","messages":[]}`))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != 404 {
		t.Fatalf("unknown model: want 404 got %d", w.Code)
	}

	// missing model → 400
	req = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"messages":[]}`))
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != 400 {
		t.Fatalf("missing model: want 400 got %d", w.Code)
	}
}

func TestAuthEnforcedOnInference(t *testing.T) {
	eng := fakeEngine(t)
	h := newTestServer(t, &engineBackend{baseURL: eng.URL}, []string{"secret"})

	// no key → 401
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"m1","messages":[]}`))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != 401 {
		t.Fatalf("want 401 without key, got %d", w.Code)
	}

	// with key → 200
	req = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"m1","messages":[]}`))
	req.Header.Set("Authorization", "Bearer secret")
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("want 200 with key, got %d", w.Code)
	}
}

func TestReadyzReflectsDraining(t *testing.T) {
	eng := fakeEngine(t)
	sched := scheduler.New(
		map[string]backend.Backend{"fake": &engineBackend{baseURL: eng.URL}},
		[]backend.ModelSpec{{ID: "m1", Backend: "fake"}},
		scheduler.Options{MaxLoaded: 2},
	)
	rec, _ := metrics.New("")
	srv := server.New(sched, auth.New(nil), rec)
	h := srv.Handler()

	get := func(path string) int {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
		return w.Code
	}
	if get("/readyz") != http.StatusOK {
		t.Fatal("readyz should be 200 before draining")
	}
	srv.SetDraining(true)
	if get("/readyz") != http.StatusServiceUnavailable {
		t.Fatal("readyz should be 503 while draining")
	}
	if get("/healthz") != http.StatusOK {
		t.Fatal("healthz should stay 200 while draining")
	}
}

func TestMetricsEndpoint(t *testing.T) {
	eng := fakeEngine(t)
	h := newTestServer(t, &engineBackend{baseURL: eng.URL}, nil)

	// One inference request to generate a metric.
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"m1","messages":[]}`))
	h.ServeHTTP(httptest.NewRecorder(), req)

	m := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, m)
	if w.Code != 200 {
		t.Fatalf("metrics status=%d", w.Code)
	}
	if !strings.Contains(w.Body.String(), `mainspring_requests_total{model="m1",status="200"}`) {
		t.Fatalf("metrics missing request counter:\n%s", w.Body.String())
	}
}

func TestCapabilitiesRequiresAdmin(t *testing.T) {
	eng := fakeEngine(t)
	sched := scheduler.New(
		map[string]backend.Backend{"fake": &engineBackend{baseURL: eng.URL}},
		[]backend.ModelSpec{{ID: "m1", Backend: "fake"}},
		scheduler.Options{MaxLoaded: 2},
	)
	rec, _ := metrics.New("")
	authn := auth.NewTenants([]auth.Tenant{
		{Name: "inf", Key: "infkey", Role: auth.RoleInference},
		{Name: "adm", Key: "admkey", Role: auth.RoleAdmin},
	})
	h := server.New(sched, authn, rec).Handler()

	get := func(key string) int {
		r := httptest.NewRequest(http.MethodGet, "/capabilities", nil)
		r.Header.Set("Authorization", "Bearer "+key)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w.Code
	}
	if code := get("infkey"); code != http.StatusForbidden {
		t.Fatalf("inference role on /capabilities: want 403, got %d", code)
	}
	if code := get("admkey"); code != http.StatusOK {
		t.Fatalf("admin role on /capabilities: want 200, got %d", code)
	}
}

func TestCapabilitiesEndpoint(t *testing.T) {
	eng := fakeEngine(t)
	h := newTestServer(t, &engineBackend{baseURL: eng.URL, degraded: true}, nil)

	// Load the model first so it appears in /capabilities.
	load := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"m1","messages":[]}`))
	h.ServeHTTP(httptest.NewRecorder(), load)

	req := httptest.NewRequest(http.MethodGet, "/capabilities", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("capabilities status=%d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, `"degraded":true`) {
		t.Fatalf("expected degraded=true in capabilities: %s", body)
	}
}
