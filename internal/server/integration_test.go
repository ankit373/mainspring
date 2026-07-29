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
	"time"

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
	mux.HandleFunc("/v1/embeddings", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"data":[{"embedding":[0.1,0.2,0.3],"index":0}],"usage":{"prompt_tokens":2,"completion_tokens":0}}`)
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

func TestRequestIDEchoedAndGenerated(t *testing.T) {
	eng := fakeEngine(t)
	h := newTestServer(t, &engineBackend{baseURL: eng.URL}, nil)

	// Client-supplied id is echoed.
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	req.Header.Set("X-Request-ID", "client-42")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if got := w.Header().Get("X-Request-ID"); got != "client-42" {
		t.Fatalf("client id not echoed: %q", got)
	}

	// Absent id is generated.
	w2 := httptest.NewRecorder()
	h.ServeHTTP(w2, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if got := w2.Header().Get("X-Request-ID"); len(got) < 4 {
		t.Fatalf("id not generated: %q", got)
	}

	// A malicious id is dropped in favor of a generated one.
	req3 := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	req3.Header.Set("X-Request-ID", "evil\nInjected: 1")
	w3 := httptest.NewRecorder()
	h.ServeHTTP(w3, req3)
	if strings.Contains(w3.Header().Get("X-Request-ID"), "\n") {
		t.Fatal("newline id must not be echoed")
	}
}

func TestAccessLogEmitsJSONL(t *testing.T) {
	eng := fakeEngine(t)
	sched := scheduler.New(
		map[string]backend.Backend{"fake": &engineBackend{baseURL: eng.URL}},
		[]backend.ModelSpec{{ID: "m1", Backend: "fake"}},
		scheduler.Options{MaxLoaded: 2},
	)
	rec, _ := metrics.New("")
	srv := server.New(sched, auth.New(nil), rec)
	var buf strings.Builder
	srv.SetAccessLog(&buf)
	h := srv.Handler()

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	req.Header.Set("X-Request-ID", "logtest-1")
	h.ServeHTTP(httptest.NewRecorder(), req)

	line := buf.String()
	if !strings.Contains(line, `"request_id":"logtest-1"`) ||
		!strings.Contains(line, `"path":"/healthz"`) ||
		!strings.Contains(line, `"status":200`) {
		t.Fatalf("access log line malformed: %s", line)
	}
}

func TestQualityEndpoint(t *testing.T) {
	eng := fakeEngine(t)
	h := newTestServer(t, &engineBackend{baseURL: eng.URL}, nil)

	// Before any request, the model is configured but not resident.
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/v1/quality", nil))
	if w.Code != 200 {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	var q struct {
		Object string `json:"object"`
		Models []struct {
			ID       string `json:"id"`
			Resident bool   `json:"resident"`
			Backend  string `json:"backend"`
		} `json:"models"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &q); err != nil {
		t.Fatal(err)
	}
	if q.Object != "quality" || len(q.Models) != 1 || q.Models[0].ID != "m1" {
		t.Fatalf("unexpected quality body: %s", w.Body.String())
	}
	if q.Models[0].Resident {
		t.Fatal("model should not be resident before any request")
	}

	// After an inference call, the model becomes resident with a real device.
	ireq := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"m1","messages":[]}`))
	h.ServeHTTP(httptest.NewRecorder(), ireq)

	w2 := httptest.NewRecorder()
	h.ServeHTTP(w2, httptest.NewRequest(http.MethodGet, "/v1/quality", nil))
	if !strings.Contains(w2.Body.String(), `"resident":true`) ||
		!strings.Contains(w2.Body.String(), `"device":"metal"`) {
		t.Fatalf("resident model quality missing device/resident: %s", w2.Body.String())
	}
}

func TestQualityRequiresAdmin(t *testing.T) {
	eng := fakeEngine(t)
	// Inference-role tenant must be forbidden from the routing-signal endpoint.
	sched := scheduler.New(
		map[string]backend.Backend{"fake": &engineBackend{baseURL: eng.URL}},
		[]backend.ModelSpec{{ID: "m1", Backend: "fake"}},
		scheduler.Options{MaxLoaded: 2},
	)
	rec, _ := metrics.New("")
	a := auth.NewTenants([]auth.Tenant{{Name: "user", Key: "k", Role: auth.RoleInference}})
	h := server.New(sched, a, rec).Handler()

	req := httptest.NewRequest(http.MethodGet, "/v1/quality", nil)
	req.Header.Set("Authorization", "Bearer k")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("inference role should be forbidden, got %d", w.Code)
	}
}

// downBackend fails every Start (a dead engine) for failover tests.
type downBackend struct{}

func (b *downBackend) Name() string { return "down" }
func (b *downBackend) Detect(context.Context) backend.Availability {
	return backend.Availability{Name: "down", Present: false}
}
func (b *downBackend) Start(context.Context, backend.ModelSpec) (backend.Runner, error) {
	return nil, context.DeadlineExceeded
}

func TestFailoverServesFromFallbackBackend(t *testing.T) {
	eng := fakeEngine(t)
	sched := scheduler.New(
		map[string]backend.Backend{"primary": &downBackend{}, "backup": &engineBackend{baseURL: eng.URL}},
		[]backend.ModelSpec{{ID: "m1", Backend: "primary", Fallbacks: []string{"backup"}}},
		scheduler.Options{MaxLoaded: 2},
	)
	rec, _ := metrics.New("")
	h := server.New(sched, auth.New(nil), rec).Handler()

	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"m1","messages":[]}`)))
	if w.Code != 200 {
		t.Fatalf("failover should serve 200, got %d body=%s", w.Code, w.Body.String())
	}
	// The surviving backend is surfaced fail-loud.
	if w.Header().Get("X-Mainspring-Backend") != "fake" {
		t.Fatalf("X-Mainspring-Backend should reflect the serving backend, got %q", w.Header().Get("X-Mainspring-Backend"))
	}
}

func TestTraceparentForwardedAndLogged(t *testing.T) {
	// Engine records the traceparent it received.
	var gotTP string
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		gotTP = r.Header.Get("traceparent")
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"ok"}}]}`)
	})
	eng := httptest.NewServer(mux)
	t.Cleanup(eng.Close)

	sched := scheduler.New(
		map[string]backend.Backend{"fake": &engineBackend{baseURL: eng.URL}},
		[]backend.ModelSpec{{ID: "m1", Backend: "fake"}},
		scheduler.Options{MaxLoaded: 2},
	)
	rec, _ := metrics.New("")
	srv := server.New(sched, auth.New(nil), rec)
	var alog strings.Builder
	srv.SetAccessLog(&alog)
	h := srv.Handler()

	const traceID = "4bf92f3577b34da6a3ce929d0e0e4736"
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"m1","messages":[]}`))
	req.Header.Set("traceparent", "00-"+traceID+"-00f067aa0ba902b7-01")
	h.ServeHTTP(httptest.NewRecorder(), req)

	// The upstream got a child traceparent that continues the same trace.
	if !strings.HasPrefix(gotTP, "00-"+traceID+"-") {
		t.Fatalf("upstream traceparent should continue the trace, got %q", gotTP)
	}
	if strings.Contains(gotTP, "00f067aa0ba902b7") {
		t.Fatal("upstream traceparent must carry our span id, not the client's")
	}
	// The access log carries the trace id for correlation.
	if !strings.Contains(alog.String(), `"trace_id":"`+traceID+`"`) {
		t.Fatalf("access log should carry trace_id: %s", alog.String())
	}
}

func TestRequestTimeoutReturns504(t *testing.T) {
	// Engine sleeps longer than the per-model timeout, honoring cancellation.
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(3 * time.Second):
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"late"}}]}`)
		case <-r.Context().Done(): // client (mainspring) cancelled on deadline
			return
		}
	})
	eng := httptest.NewServer(mux)
	t.Cleanup(eng.Close)

	sched := scheduler.New(
		map[string]backend.Backend{"fake": &engineBackend{baseURL: eng.URL}},
		[]backend.ModelSpec{{ID: "m1", Backend: "fake"}},
		scheduler.Options{MaxLoaded: 2},
	)
	rec, _ := metrics.New("")
	srv := server.New(sched, auth.New(nil), rec)
	srv.SetTimeouts(0, map[string]time.Duration{"m1": 150 * time.Millisecond})
	h := srv.Handler()

	start := time.Now()
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"m1","messages":[]}`)))
	if w.Code != http.StatusGatewayTimeout {
		t.Fatalf("expected 504 on timeout, got %d body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"code":"timeout"`) {
		t.Fatalf("timeout error should carry code timeout: %s", w.Body.String())
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("timeout should fire fast (~150ms), took %v", elapsed)
	}
}

func TestCircuitBreakerTripsAndReports(t *testing.T) {
	// Engine that always 500s so the breaker trips.
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, `{"error":"boom"}`)
	})
	eng := httptest.NewServer(mux)
	t.Cleanup(eng.Close)

	sched := scheduler.New(
		map[string]backend.Backend{"fake": &engineBackend{baseURL: eng.URL}},
		[]backend.ModelSpec{{ID: "m1", Backend: "fake"}},
		scheduler.Options{MaxLoaded: 2},
	)
	rec, _ := metrics.New("")
	srv := server.New(sched, auth.New(nil), rec)
	srv.SetBreaker(2, time.Minute) // opens after 2 consecutive failures
	h := srv.Handler()

	post := func() int {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
			strings.NewReader(`{"model":"m1","messages":[]}`)))
		return w.Code
	}
	// Two upstream 500s trip the breaker.
	if c := post(); c != 500 {
		t.Fatalf("first call should proxy the 500, got %d", c)
	}
	post() // second failure => opens
	// Now the breaker fast-fails with 503 (no upstream call) and the taxonomy
	// code distinguishes it from a busy/backend-down 503.
	fw := httptest.NewRecorder()
	h.ServeHTTP(fw, httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"m1","messages":[]}`)))
	if fw.Code != http.StatusServiceUnavailable {
		t.Fatalf("open breaker should fast-fail 503, got %d", fw.Code)
	}
	if !strings.Contains(fw.Body.String(), `"code":"circuit_open"`) {
		t.Fatalf("open-breaker error should carry code circuit_open: %s", fw.Body.String())
	}

	// /v1/quality reports the open breaker.
	qw := httptest.NewRecorder()
	h.ServeHTTP(qw, httptest.NewRequest(http.MethodGet, "/v1/quality", nil))
	if !strings.Contains(qw.Body.String(), `"breaker":"open"`) {
		t.Fatalf("quality should report open breaker: %s", qw.Body.String())
	}
	// /metrics exposes the gauge.
	mw := httptest.NewRecorder()
	h.ServeHTTP(mw, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if !strings.Contains(mw.Body.String(), `mainspring_breaker_open{model="m1"} 1`) {
		t.Fatalf("metrics should expose breaker gauge: %s", mw.Body.String())
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
