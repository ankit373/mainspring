package server

import (
	"net/http"
	"time"

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

// acquireOutcome is why acquireRunner ended. Circuit-open and load-failure are
// distinct: both let the caller try a fallback, but they are different facts
// about the server, and collapsing them is what made a load failure report the
// stable error code `circuit_open` on a server with no breaker configured.
type acquireOutcome int

const (
	acquireOK          acquireOutcome = iota // runner reserved; release must be called
	acquireCircuitOpen                       // the breaker is open for this model → try a fallback
	acquireLoadFailed                        // the model failed to load → try a fallback
	acquireBusy                              // the gate is saturated → backpressure, the caller should stop
	acquireCancelled                         // the caller went away while queued → write nothing
)

// acquireRunner reserves a concurrency slot and loads one candidate model. The
// returned release must be called when the request finishes (it is nil unless
// the outcome is acquireOK). wait is the time spent queued for a gate slot (0
// when gating is disabled or a slot was immediately free), reported regardless
// of the final outcome.
func (s *Server) acquireRunner(r *http.Request, model string) (runner backend.Runner, release func(), wait time.Duration, out acquireOutcome) {
	rel, wait, res := s.gate.acquire(r.Context(), model)
	switch res {
	case gateFull:
		return nil, nil, wait, acquireBusy
	case gateCancelled:
		return nil, nil, wait, acquireCancelled
	}
	if !s.breaker.Allow(model) {
		rel()
		return nil, nil, wait, acquireCircuitOpen
	}
	runner, err := s.sched.EnsureLoaded(r.Context(), model)
	if err != nil {
		s.breaker.OnResult(model, false)
		rel()
		return nil, nil, wait, acquireLoadFailed
	}
	return runner, rel, wait, acquireOK
}
