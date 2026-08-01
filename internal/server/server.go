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
	"github.com/ankit373/mainspring/internal/cache"
	"github.com/ankit373/mainspring/internal/metrics"
	"github.com/ankit373/mainspring/internal/scheduler"
)

// maxRequestBody caps a single inference request payload (prompts included).
const maxRequestBody = 64 << 20

// Server serves the OpenAI-compatible API.
type Server struct {
	sched          *scheduler.Scheduler
	auth           *auth.Authenticator
	metrics        *metrics.Recorder
	gate           *gate
	breaker        *breaker.Group
	draining       atomic.Bool
	accessMu       sync.RWMutex
	access         *accessLogger
	lintMu         sync.RWMutex
	lint           []string                 // current config-lint warnings (rendered strings), for GET /admin/config
	reloadFn       func() error             // wired by main for POST /admin/reload
	cache          *cache.LRU               // opt-in response cache (nil = disabled)
	costRates      map[string]CostRate      // per-model USD pricing (nil = all free)
	ctxPolicies    map[string]ContextPolicy // per-model context guardrail (nil = off)
	modelFallbacks map[string][]string      // per-model fallback chains (nil = none)
	clampLimits    map[string]int           // per-model ctx window for max_tokens clamping (nil = off)
	preciseCtx     map[string]bool          // per-model exact-tokenization guardrail (nil = estimate only)
	coalesce       *flightGroup             // single-flight de-dup of identical in-flight requests (nil = off)

	retryMax     int           // additional upstream attempts after the first (0 = no retry)
	retryBackoff time.Duration // base of the exponential retry backoff

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

// SetLintWarnings records the current config-lint warnings (rendered as
// strings, so this package stays independent of internal/config), surfaced via
// GET /admin/config. Safe to call at startup and again after every successful
// reload, so the report reflects the config as of the last load, not just the
// one the process booted with.
func (s *Server) SetLintWarnings(warnings []string) {
	s.lintMu.Lock()
	defer s.lintMu.Unlock()
	s.lint = warnings
}

// lintWarnings never returns nil (json.Marshal renders a nil slice as `null`,
// not `[]`) — a clean config reports an explicit empty array.
func (s *Server) lintWarnings() []string {
	s.lintMu.RLock()
	defer s.lintMu.RUnlock()
	if s.lint == nil {
		return []string{}
	}
	return s.lint
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
	mux.HandleFunc("GET /v1/models/{id}", s.modelDetail)
	mux.HandleFunc("/v1/chat/completions", s.inference)
	mux.HandleFunc("/v1/completions", s.inference)
	mux.HandleFunc("/v1/embeddings", s.inference)
	mux.HandleFunc("/v1/tokenize", s.tokenize) // token-counting utility
	mux.HandleFunc("/v1/messages", s.messages) // Anthropic Messages API
	// Admin API (management actions; admin-gated, audited via the access log).
	mux.HandleFunc("GET /admin/config", s.adminConfig)
	mux.HandleFunc("GET /admin/usage", s.adminUsage)
	mux.HandleFunc("POST /admin/drain", s.adminDrain)
	mux.HandleFunc("POST /admin/reload", s.adminReload)
	mux.HandleFunc("POST /admin/models/{id}/load", s.adminLoad)
	mux.HandleFunc("POST /admin/models/{id}/unload", s.adminUnload)
	mux.HandleFunc("POST /admin/breaker/{id}/reset", s.adminBreakerReset)
	mux.HandleFunc("POST /admin/cache/clear", s.adminCacheClear)
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
	s.cache.WritePrometheus(w)
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
	if s.totalOutage() {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "degraded"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
}

// totalOutage reports whether every configured model's circuit breaker is
// Open — a full-outage signal a load balancer or Kubernetes readinessProbe
// should act on by routing traffic elsewhere, not something drain alone
// covers. False whenever breaking is disabled, no models are configured, or at
// least one model is Closed/HalfOpen (HalfOpen means recovery is actively
// being probed, not that the service is down).
func (s *Server) totalOutage() bool {
	if !s.breaker.Enabled() {
		return false
	}
	specs := s.sched.Models()
	if len(specs) == 0 {
		return false
	}
	for _, sp := range specs {
		if s.breaker.State(sp.ID) != breaker.Open {
			return false
		}
	}
	return true
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

// modelDetail implements GET /v1/models/{id} — the OpenAI "retrieve a model"
// endpoint. It resolves aliases and, when the model is currently resident,
// enriches the standard id/object/owned_by shape with the fail-loud
// backend/device fields (non-standard, but useful at a glance — the same data
// /v1/quality reports at scale).
func (s *Server) modelDetail(w http.ResponseWriter, r *http.Request) {
	id, ok := s.sched.Resolve(r.PathValue("id"))
	if !ok {
		writeError(w, http.StatusNotFound, "model not found: "+r.PathValue("id"))
		return
	}
	out := map[string]any{"id": id, "object": "model", "owned_by": "mainspring"}
	for _, ri := range s.sched.Loaded() {
		if ri.ID != id {
			continue
		}
		ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
		c, err := ri.Runner.Capabilities(ctx)
		cancel()
		if err == nil {
			out["resident"] = true
			out["backend"] = c.Backend
			out["device"] = c.Device
			out["effective_ctx"] = c.EffectiveCtx
			out["degraded"] = c.Degraded()
		}
		break
	}
	if _, ok := out["resident"]; !ok {
		out["resident"] = false
	}
	writeJSON(w, http.StatusOK, out)
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

	// Graceful clamp: shrink an over-budget max_tokens to fit the window before
	// the guardrail gets a chance to reject the request.
	body = s.applyClamp(w, model, body)

	// Context guardrail: reject (or warn) an over-context request before doing any
	// work, rather than letting the engine silently truncate it. Prompt tokens are
	// counted exactly when precise_context is on and the model is resident on a
	// tokenizing engine, otherwise estimated; the method is reported on a header.
	if pol, ok := s.ctxPolicies[model]; ok && pol.Limit > 0 {
		promptTok, exact := s.guardPromptTokens(r.Context(), model, body)
		method := "estimated"
		if exact {
			method = "exact"
		}
		w.Header().Set("X-Mainspring-Context-Method", method)
		if over, reason := contextOverageDetail(body, pol.Limit, promptTok, exact); over {
			if pol.Enforce {
				writeErr(w, codeContextLength, reason)
				return
			}
			w.Header().Set("X-Mainspring-Context-Warning", reason)
		}
	}

	// Per-tenant token budget (enforced pre-request; accrued after).
	tenant, _ := auth.FromContext(r.Context())
	if !s.auth.AllowTokens(tenant) {
		writeErr(w, codeTokenBudget, "token budget exceeded")
		return
	}

	// Response cache: serve identical deterministic non-stream requests without
	// touching the gate, breaker, loader, or backend. A hit still meters the
	// tenant's token budget so accounting is consistent whether or not the model
	// actually ran.
	cacheKey, cached, hit := s.cacheLookup(model, r.URL.Path, body)
	if hit {
		serveCached(w, cached)
		charge := cached.Completion
		if cached.Exact {
			charge = cached.Prompt + cached.Completion
		}
		s.auth.AddTokens(tenant, charge)
		if s.metrics != nil {
			s.metrics.Record(metrics.Event{
				Time:         time.Now(),
				RequestID:    RequestID(r.Context()),
				TraceID:      TraceID(r.Context()),
				Model:        model,
				Tenant:       auth.TenantOf(r.Context()),
				Status:       cached.Status,
				Cached:       true,
				Bytes:        int64(len(cached.Body)),
				PromptTokens: cached.Prompt,
				TokensEst:    cached.Completion,
				Exact:        cached.Exact,
				CostUSD:      s.costFor(model, cached.Prompt, cached.Completion, cached.Exact),
			})
		}
		return
	}

	// Request coalescing: identical deterministic requests already in flight share
	// one backend computation. Followers wait for the leader and replay its result.
	var (
		coKey    string
		coLeader bool
		coVal    cache.Value
		coOK     bool
	)
	if s.coalesce != nil && cacheable(body) {
		coKey = cacheKey
		if coKey == "" {
			coKey = cache.Key(model, r.URL.Path, body)
		}
		leader, f := s.coalesce.join(coKey)
		if !leader {
			select {
			case <-f.done:
				if f.ok {
					serveCoalesced(w, f.val)
					charge := f.val.Completion
					if f.val.Exact {
						charge = f.val.Prompt + f.val.Completion
					}
					s.auth.AddTokens(tenant, charge)
					if s.metrics != nil {
						s.metrics.Record(metrics.Event{
							Time:         time.Now(),
							RequestID:    RequestID(r.Context()),
							TraceID:      TraceID(r.Context()),
							Model:        model,
							Tenant:       auth.TenantOf(r.Context()),
							Status:       f.val.Status,
							Coalesced:    true,
							Bytes:        int64(len(f.val.Body)),
							PromptTokens: f.val.Prompt,
							TokensEst:    f.val.Completion,
							Exact:        f.val.Exact,
							CostUSD:      s.costFor(model, f.val.Prompt, f.val.Completion, f.val.Exact),
						})
					}
					return
				}
				// Leader produced no shareable result → fall through and run normally.
			case <-r.Context().Done():
				writeErr(w, codeTimeout, "request cancelled while waiting for coalesced result")
				return
			}
		} else {
			coLeader = true
			defer func() { s.coalesce.publish(coKey, coVal, coOK) }()
		}
	}

	// Select a servable model: try the requested one, then its fallback chain.
	// A candidate is skipped only for a pre-serve failure (circuit open or load
	// error); gate saturation is backpressure, not a fallback trigger.
	var (
		runner    backend.Runner
		release   func()
		served    string
		queueWait time.Duration
	)
	for _, cand := range s.candidatesFor(model) {
		rr, rel, wait, busy, okc := s.acquireRunner(r, cand)
		queueWait += wait
		if busy {
			w.Header().Set("Retry-After", "1")
			writeErr(w, codeServerBusy, "server busy: too many concurrent requests for "+cand)
			return
		}
		if okc {
			runner, release, served = rr, rel, cand
			break
		}
	}
	if runner == nil {
		w.Header().Set("Retry-After", "5")
		writeErr(w, codeCircuitOpen, "no available backend for "+model+" or its fallbacks")
		return
	}
	defer release()

	// Per-request timeout for the model that will actually serve (0 = unbounded).
	if d := s.timeoutFor(served); d > 0 {
		tctx, cancel := context.WithTimeout(r.Context(), d)
		defer cancel()
		r = r.WithContext(tctx)
	}

	// If a fallback answered, point the upstream body at it and tell the client.
	upBody := body
	extra := s.failLoudHeaders(r.Context(), runner)
	if served != model {
		upBody = rewriteModelField(body, served)
		if extra == nil {
			extra = map[string]string{}
		}
		extra["X-Mainspring-Served-Model"] = served
	}

	start := time.Now()
	cap := newCapture(w, start)
	// Record the full body when the response may be shared — cached and/or
	// coalesced — and only when the primary model served (a fallback's output is
	// never stored under the primary key).
	if ((cacheKey != "") || coLeader) && served == model {
		cap.recordFor(cacheBodyCap)
	}
	// Mark the runner busy for the actual generation call so the scheduler's
	// idle timer and LRU eviction never pull it out from under a long-running
	// request (e.g. one that streams longer than KeepAlive).
	s.sched.MarkBusy(served)
	retries := s.proxyTo(cap, r, runner.BaseURL(), upBody, extra)
	s.sched.MarkIdle(served)
	// A 5xx from the upstream counts as a backend failure; 2xx/4xx are healthy
	// (4xx is a client error, not the backend's fault).
	s.breaker.OnResult(served, cap.status < 500)

	prompt, completion, exact := cap.usage()
	// Build the shareable value once for a successful, non-streaming, within-cap
	// primary-model response, then feed it to the cache and/or coalesce followers.
	if served == model && !cap.stream && !cap.bodyOver && cap.status >= 200 && cap.status < 300 && cap.body != nil {
		shared := cache.Value{
			Status:     cap.status,
			Header:     cap.snapHeader,
			Body:       cap.body,
			Prompt:     prompt,
			Completion: completion,
			Exact:      exact,
		}
		if cacheKey != "" {
			s.cache.Put(cacheKey, shared)
		}
		if coLeader {
			coVal, coOK = shared, true
		}
	}
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
			Model:        served,
			Tenant:       auth.TenantOf(r.Context()),
			Status:       cap.status,
			Stream:       cap.stream,
			DurationMs:   float64(time.Since(start).Microseconds()) / 1000.0,
			TTFTMs:       cap.ttftMs(),
			Bytes:        cap.bytes,
			PromptTokens: prompt,
			TokensEst:    completion,
			Exact:        exact,
			CostUSD:      s.costFor(served, prompt, completion, exact),
			Retries:      retries,
			Fallback:     served != model,
			QueueWaitMs:  float64(queueWait.Microseconds()) / 1000.0,
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
