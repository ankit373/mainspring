package server

import (
	"log"
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
		rel()
		if clientGone(r.Context()) {
			// The caller went away while the model was loading, so the backend
			// never got the chance to fail — this is not a load failure. Release
			// the half-open probe Allow may have just consumed (see OnAbandoned)
			// without moving the failure count, and write nothing back.
			s.breaker.OnAbandoned(model)
			return nil, nil, wait, acquireCancelled
		}
		// The reason is the only thing that makes this actionable, and it used to be
		// dropped here: the caller's 503 says "no available backend", which is true
		// and useless. A backend knows exactly why — a model the daemon does not
		// have, a binary that is missing, a subprocess that would not start — so say
		// it where an operator will see it. It stays out of the response: the caller
		// cannot act on another host's inventory, and the operator can.
		log.Printf("LOAD-FAILED model=%s: %v", model, err)
		s.breaker.OnResult(model, false)
		return nil, nil, wait, acquireLoadFailed
	}
	return runner, rel, wait, acquireOK
}

// selectRunner picks a model that can actually serve this request: the requested
// one, then each configured model_fallback in turn. A candidate is skipped only
// for a pre-serve failure (circuit open or load error) — gate saturation is
// backpressure, not a reason to answer as a different model.
//
// When nothing is servable, or the caller went away, it writes and records the
// outcome itself and returns ok=false, meaning the handler must return
// immediately. Otherwise release must be deferred by the caller.
//
// Shared by both dialects: they differ in what they send upstream, not in how
// they choose who sends it.
func (s *Server) selectRunner(w http.ResponseWriter, r *http.Request, model string, recvd time.Time) (runner backend.Runner, release func(), served string, queueWait time.Duration, ok bool) {
	sawBreaker := false // a candidate was refused by an open circuit, not a load failure
candidates:
	for _, cand := range s.candidatesFor(model) {
		rr, rel, wait, out := s.acquireRunner(r, cand)
		queueWait += wait
		switch out {
		case acquireBusy:
			w.Header().Set("Retry-After", "1")
			writeErr(w, codeServerBusy, "server busy: too many concurrent requests for "+cand)
			s.recordRejected(r.Context(), cand, codeServerBusy, recvd)
			return nil, nil, "", queueWait, false
		case acquireCancelled:
			// The caller disconnected while queued; a 503 would be written to a
			// dead connection and would misreport why the request ended.
			return nil, nil, "", queueWait, false
		case acquireCircuitOpen:
			sawBreaker = true
		case acquireOK:
			runner, release, served = rr, rel, cand
			break candidates
		}
	}
	if runner == nil {
		// `circuit_open` is only true when a breaker actually refused a candidate.
		// Every other way to get here — including every way to get here with the
		// breaker disabled — is a load failure, and a client branching on the
		// stable code must not be told a circuit tripped when none exists.
		code := codeBackendUnavailable
		if sawBreaker {
			code = codeCircuitOpen
		}
		w.Header().Set("Retry-After", "5")
		writeErr(w, code, "no available backend for "+model+" or its fallbacks")
		s.recordRejected(r.Context(), model, code, recvd)
		return nil, nil, "", queueWait, false
	}
	return runner, release, served, queueWait, true
}
