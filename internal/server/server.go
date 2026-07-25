// Package server exposes the stable OpenAI-compatible HTTP API. It does not
// generate tokens — it authenticates, resolves the model to a loaded runner via
// the scheduler, and streams a reverse-proxied response from that runner's
// engine. It adds the one thing the engines omit: fail-loud signals about what
// actually ran (backend, device, effective context) on every response.
package server

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ankit373/mainspring/internal/auth"
	"github.com/ankit373/mainspring/internal/backend"
	"github.com/ankit373/mainspring/internal/breaker"
	"github.com/ankit373/mainspring/internal/metrics"
	"github.com/ankit373/mainspring/internal/scheduler"
)

// maxRequestBody caps a single inference request payload (prompts included).
const maxRequestBody = 64 << 20

// Server serves the OpenAI-compatible API.
type Server struct {
	sched    *scheduler.Scheduler
	auth     *auth.Authenticator
	metrics  *metrics.Recorder
	gate     *gate
	breaker  *breaker.Group
	draining atomic.Bool
	accessMu sync.RWMutex
	access   *accessLogger
	reloadFn func() error // wired by main for POST /admin/reload

	defaultTimeout time.Duration            // per-request timeout (0 = unbounded)
	timeouts       map[string]time.Duration // per-model overrides (real ids)
}

// SetTimeouts configures the per-request generation timeout: a default applied
// to every model, with optional per-model (real id) overrides. A duration of 0
// means unbounded. Safe to call at startup.
func (s *Server) SetTimeouts(def time.Duration, perModel map[string]time.Duration) {
	s.defaultTimeout = def
	s.timeouts = perModel
}

// timeoutFor returns the request timeout for a resolved model id (per-model
// override if set, else the default; 0 = unbounded).
func (s *Server) timeoutFor(model string) time.Duration {
	if d, ok := s.timeouts[model]; ok {
		return d
	}
	return s.defaultTimeout
}

// SetBreaker enables the per-model circuit breaker: after `threshold`
// consecutive backend failures a model's requests fast-fail with 503 until
// `cooldown` elapses and a half-open probe succeeds. threshold<=0 disables it.
func (s *Server) SetBreaker(threshold int, cooldown time.Duration) {
	s.breaker = breaker.NewGroup(threshold, cooldown)
}

// Breaker exposes the breaker group (may be a disabled group) for the health
// prober to feed probe outcomes into.
func (s *Server) Breaker() *breaker.Group { return s.breaker }

// SetAccessLog enables the structured JSONL access log, writing one line per
// request to w. Passing nil disables it. Safe to call at startup.
func (s *Server) SetAccessLog(w io.Writer) {
	s.accessMu.Lock()
	defer s.accessMu.Unlock()
	if w == nil {
		s.access = nil
		return
	}
	s.access = &accessLogger{w: w}
}

func (s *Server) accessLog() *accessLogger {
	s.accessMu.RLock()
	defer s.accessMu.RUnlock()
	return s.access
}

// SetDraining marks the server as draining: readiness (/readyz) starts failing so
// load balancers stop routing new traffic while in-flight requests finish.
func (s *Server) SetDraining(v bool) { s.draining.Store(v) }

// New builds a Server. rec may be nil (metrics disabled). Concurrency gating is
// off by default; enable it with SetConcurrency.
func New(sched *scheduler.Scheduler, a *auth.Authenticator, rec *metrics.Recorder) *Server {
	return &Server{
		sched:   sched,
		auth:    a,
		metrics: rec,
		gate:    newGate(0, 0),
		breaker: breaker.NewGroup(0, 0), // disabled until SetBreaker
	}
}

// SetConcurrency enables per-model backpressure: at most maxInflight concurrent
// requests per model with up to maxQueue waiters; excess is rejected with 503.
// maxInflight <= 0 disables gating.
func (s *Server) SetConcurrency(maxInflight, maxQueue int) {
	s.gate = newGate(maxInflight, maxQueue)
}

// Handler returns the fully-wired http.Handler (auth-wrapped).
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", s.healthz)
	mux.HandleFunc("/readyz", s.readyz)
	mux.HandleFunc("/metrics", s.metricsHandler)
	mux.HandleFunc("/capabilities", s.capabilities)
	mux.HandleFunc("/v1/quality", s.quality) // optional Hydra routing signal
	mux.HandleFunc("/v1/models", s.models)
	mux.HandleFunc("/v1/chat/completions", s.inference)
	mux.HandleFunc("/v1/completions", s.inference)
	mux.HandleFunc("/v1/embeddings", s.inference)
	mux.HandleFunc("/v1/messages", s.messages) // Anthropic Messages API
	// Admin API (management actions; admin-gated, audited via the access log).
	mux.HandleFunc("POST /admin/drain", s.adminDrain)
	mux.HandleFunc("POST /admin/reload", s.adminReload)
	mux.HandleFunc("POST /admin/models/{id}/load", s.adminLoad)
	mux.HandleFunc("POST /admin/models/{id}/unload", s.adminUnload)
	// requestID is outermost so every request — including auth rejections and
	// health checks — gets a correlation id and an access-log line.
	return s.requestID(s.auth.Wrap(mux))
}

// metricsHandler renders the Prometheus exposition, merging live residency.
func (s *Server) metricsHandler(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	if s.metrics == nil {
		return
	}
	used, budget, count := s.sched.Residency()
	s.metrics.WritePrometheus(w, metrics.Gauges{
		LoadedModels:  count,
		ResidentBytes: used,
		BudgetBytes:   budget,
	})
	s.gate.writePrometheus(w)
	s.breaker.WritePrometheus(w)
}

func (s *Server) healthz(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// readyz is the readiness probe: 200 normally, 503 while draining.
func (s *Server) readyz(w http.ResponseWriter, _ *http.Request) {
	if s.draining.Load() {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "draining"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
}

// models implements GET /v1/models.
func (s *Server) models(w http.ResponseWriter, r *http.Request) {
	type model struct {
		ID      string `json:"id"`
		Object  string `json:"object"`
		OwnedBy string `json:"owned_by"`
	}
	specs := s.sched.Models()
	aliases := s.sched.Aliases()
	data := make([]model, 0, len(specs)+len(aliases))
	for _, sp := range specs {
		data = append(data, model{ID: sp.ID, Object: "model", OwnedBy: "mainspring"})
	}
	for name := range aliases {
		data = append(data, model{ID: name, Object: "model", OwnedBy: "mainspring"})
	}
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": data})
}

// capabilities implements GET /capabilities — the fail-loud endpoint. It reports
// the real backend/device/effective-ctx for every loaded model and whether any
// is running degraded (CPU fallback or shrunk context).
func (s *Server) capabilities(w http.ResponseWriter, r *http.Request) {
	// Management endpoint: admin role required (open mode has no tenant → allow).
	if t, ok := auth.FromContext(r.Context()); ok && t.Role != auth.RoleAdmin {
		writeError(w, http.StatusForbidden, "admin role required")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()

	loaded := s.sched.Loaded()
	caps := make([]backend.Capabilities, 0, len(loaded))
	degraded := false
	for _, ri := range loaded {
		c, err := ri.Runner.Capabilities(ctx)
		if err != nil {
			continue
		}
		if c.Degraded() {
			degraded = true
		}
		caps = append(caps, c)
	}
	used, budget, count := s.sched.Residency()
	residency := map[string]any{
		"resident_bytes": used,
		"budget_bytes":   budget,
		"loaded_count":   count,
	}
	if budget > 0 {
		residency["headroom_bytes"] = budget - used
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"open":      s.auth.Open(),
		"degraded":  degraded,
		"residency": residency,
		"loaded":    caps,
	})
}

// inference handles the chat/completions/embeddings endpoints by proxying to the
// runner that serves the requested model.
func (s *Server) inference(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxRequestBody))
	if err != nil {
		writeError(w, http.StatusBadRequest, "read request body: "+err.Error())
		return
	}

	var peek struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(body, &peek); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return
	}
	if peek.Model == "" {
		writeError(w, http.StatusBadRequest, "missing required field: model")
		return
	}
	// Resolve aliases to the real model id; the gate, loader, metrics, and the
	// upstream request body all key on the resolved id.
	model, ok := s.sched.Resolve(peek.Model)
	if !ok {
		writeError(w, http.StatusNotFound, "model not found: "+peek.Model)
		return
	}
	if model != peek.Model {
		body = rewriteModelField(body, model)
	}

	// Per-tenant token budget (enforced pre-request; accrued after).
	tenant, _ := auth.FromContext(r.Context())
	if !s.auth.AllowTokens(tenant) {
		writeErr(w, codeTokenBudget, "token budget exceeded")
		return
	}

	// Concurrency gate: bound in-flight requests per model, backpressure over it.
	release, ok := s.gate.acquire(r.Context(), model)
	if !ok {
		w.Header().Set("Retry-After", "1")
		writeErr(w, codeServerBusy, "server busy: too many concurrent requests for "+model)
		return
	}
	defer release()

	// Circuit breaker: fast-fail while this model's backend is tripped open.
	if !s.breaker.Allow(model) {
		w.Header().Set("Retry-After", "5")
		writeErr(w, codeCircuitOpen, "circuit open: backend for "+model+" is unavailable")
		return
	}

	// Per-request timeout: bound total generation time (0 = unbounded).
	if d := s.timeoutFor(model); d > 0 {
		tctx, cancel := context.WithTimeout(r.Context(), d)
		defer cancel()
		r = r.WithContext(tctx)
	}

	runner, err := s.sched.EnsureLoaded(r.Context(), model)
	if err != nil {
		s.breaker.OnResult(model, false)
		writeErr(w, codeBackendUnavailable, "load model "+model+": "+err.Error())
		return
	}

	extra := s.failLoudHeaders(r.Context(), runner)

	start := time.Now()
	cap := newCapture(w, start)
	s.proxyTo(cap, r, runner.BaseURL(), body, extra)
	// A 5xx from the upstream counts as a backend failure; 2xx/4xx are healthy
	// (4xx is a client error, not the backend's fault).
	s.breaker.OnResult(model, cap.status < 500)

	prompt, completion, exact := cap.usage()
	// Token budgets charge total consumption when we have exact usage; otherwise
	// only the (estimated) output count is known.
	charge := completion
	if exact {
		charge = prompt + completion
	}
	s.auth.AddTokens(tenant, charge)
	if s.metrics != nil {
		s.metrics.Record(metrics.Event{
			Time:         start,
			RequestID:    RequestID(r.Context()),
			TraceID:      TraceID(r.Context()),
			Model:        model,
			Tenant:       auth.TenantOf(r.Context()),
			Status:       cap.status,
			Stream:       cap.stream,
			DurationMs:   float64(time.Since(start).Microseconds()) / 1000.0,
			TTFTMs:       cap.ttftMs(),
			Bytes:        cap.bytes,
			PromptTokens: prompt,
			TokensEst:    completion,
			Exact:        exact,
		})
	}
}

// failLoudHeaders builds the X-Mainspring-* signal headers and logs loudly when
// a model is running degraded — the direct countermeasure to silent CPU
// fallback / silent context truncation.
func (s *Server) failLoudHeaders(ctx context.Context, runner backend.Runner) map[string]string {
	capCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	c, err := runner.Capabilities(capCtx)
	if err != nil {
		return nil
	}
	h := map[string]string{
		"X-Mainspring-Backend": c.Backend,
		"X-Mainspring-Device":  c.Device,
	}
	if c.Degraded() {
		msg := joinWarnings(c.Warnings)
		h["X-Mainspring-Warning"] = msg
		log.Printf("DEGRADED model=%s device=%s: %s", c.Model, c.Device, msg)
	}
	return h
}

func joinWarnings(ws []string) string {
	if len(ws) == 0 {
		return "running degraded"
	}
	out := ws[0]
	for _, w := range ws[1:] {
		out += "; " + w
	}
	return out
}

// ── response helpers ─────────────────────────────────────────────────────────

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("content-type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// rewriteModelField rewrites the top-level "model" field of a JSON request body
// to realID, used when the client referenced an alias. On any parse failure it
// returns the body unchanged (the alias reaches the backend, which for the
// single-model managed backends is harmless).
func rewriteModelField(body []byte, realID string) []byte {
	var m map[string]json.RawMessage
	if json.Unmarshal(body, &m) != nil {
		return body
	}
	rid, err := json.Marshal(realID)
	if err != nil {
		return body
	}
	m["model"] = rid
	out, err := json.Marshal(m)
	if err != nil {
		return body
	}
	return out
}
