package server

import (
	"net/http"

	"github.com/ankit373/mainspring/internal/backend"
)

// Model-level fallback: when a requested model cannot serve — its circuit is
// open, or it fails to load — Mainspring transparently tries the next model in a
// configured chain rather than failing the request. This is distinct from
// backend-level fallback (a model failing over to another *engine*): here a whole
// different *model* answers. It is only ever attempted before any byte reaches
// the client; a served 4xx/5xx or a mid-stream failure is never retried on
// another model.

// SetModelFallbacks installs per-model fallback chains (keyed by resolved model
// id; values are model ids or aliases). Safe to call at startup.
func (s *Server) SetModelFallbacks(chains map[string][]string) {
	s.modelFallbacks = chains
}

// candidatesFor returns the ordered, de-duplicated list of resolved model ids to
// try for a request: the primary first, then each resolvable fallback. Unknown
// fallbacks are skipped.
func (s *Server) candidatesFor(primary string) []string {
	out := []string{primary}
	seen := map[string]bool{primary: true}
	for _, fb := range s.modelFallbacks[primary] {
		id, ok := s.sched.Resolve(fb)
		if !ok || seen[id] {
			continue
		}
		out = append(out, id)
		seen[id] = true
	}
	return out
}

// acquireRunner reserves a concurrency slot and loads one candidate model. The
// returned release must be called when the request finishes. busy=true means the
// gate is saturated for this model (backpressure — not a fallback trigger, the
// caller should stop). ok=false with busy=false means the model is unavailable
// (circuit open or load failure) and the caller may try a fallback.
func (s *Server) acquireRunner(r *http.Request, model string) (runner backend.Runner, release func(), busy, ok bool) {
	rel, acquired := s.gate.acquire(r.Context(), model)
	if !acquired {
		return nil, nil, true, false
	}
	if !s.breaker.Allow(model) {
		rel()
		return nil, nil, false, false
	}
	runner, err := s.sched.EnsureLoaded(r.Context(), model)
	if err != nil {
		s.breaker.OnResult(model, false)
		rel()
		return nil, nil, false, false
	}
	return runner, rel, false, true
}
