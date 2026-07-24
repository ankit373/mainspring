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
	"time"

	"github.com/ankit373/mainspring/internal/auth"
	"github.com/ankit373/mainspring/internal/backend"
	"github.com/ankit373/mainspring/internal/metrics"
	"github.com/ankit373/mainspring/internal/scheduler"
)

// maxRequestBody caps a single inference request payload (prompts included).
const maxRequestBody = 64 << 20

// Server serves the OpenAI-compatible API.
type Server struct {
	sched   *scheduler.Scheduler
	auth    *auth.Authenticator
	metrics *metrics.Recorder
}

// New builds a Server. rec may be nil (metrics disabled).
func New(sched *scheduler.Scheduler, a *auth.Authenticator, rec *metrics.Recorder) *Server {
	return &Server{sched: sched, auth: a, metrics: rec}
}

// Handler returns the fully-wired http.Handler (auth-wrapped).
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", s.healthz)
	mux.HandleFunc("/metrics", s.metricsHandler)
	mux.HandleFunc("/capabilities", s.capabilities)
	mux.HandleFunc("/v1/models", s.models)
	mux.HandleFunc("/v1/chat/completions", s.inference)
	mux.HandleFunc("/v1/completions", s.inference)
	mux.HandleFunc("/v1/embeddings", s.inference)
	return s.auth.Wrap(mux)
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
}

func (s *Server) healthz(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// models implements GET /v1/models.
func (s *Server) models(w http.ResponseWriter, r *http.Request) {
	type model struct {
		ID      string `json:"id"`
		Object  string `json:"object"`
		OwnedBy string `json:"owned_by"`
	}
	specs := s.sched.Models()
	data := make([]model, 0, len(specs))
	for _, sp := range specs {
		data = append(data, model{ID: sp.ID, Object: "model", OwnedBy: "mainspring"})
	}
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": data})
}

// capabilities implements GET /capabilities — the fail-loud endpoint. It reports
// the real backend/device/effective-ctx for every loaded model and whether any
// is running degraded (CPU fallback or shrunk context).
func (s *Server) capabilities(w http.ResponseWriter, r *http.Request) {
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
	if !s.sched.Known(peek.Model) {
		writeError(w, http.StatusNotFound, "model not found: "+peek.Model)
		return
	}

	runner, err := s.sched.EnsureLoaded(r.Context(), peek.Model)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "load model "+peek.Model+": "+err.Error())
		return
	}

	extra := s.failLoudHeaders(r.Context(), runner)

	start := time.Now()
	cap := newCapture(w, start)
	s.proxyTo(cap, r, runner.BaseURL(), body, extra)

	if s.metrics != nil {
		s.metrics.Record(metrics.Event{
			Time:       start,
			Model:      peek.Model,
			Status:     cap.status,
			Stream:     cap.stream,
			DurationMs: float64(time.Since(start).Microseconds()) / 1000.0,
			TTFTMs:     cap.ttftMs(),
			Bytes:      cap.bytes,
			TokensEst:  cap.tokensEstimate(),
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

// writeError emits an OpenAI-shaped error object.
func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]any{
		"error": map[string]string{"message": msg, "type": "invalid_request_error"},
	})
}
