package server

import (
	"net/http"
	"sync"

	"github.com/ankit373/mainspring/internal/cache"
)

// Request coalescing (single-flight): a burst of identical deterministic
// requests should cost one backend computation, not N. The first request for a
// key is the leader and runs normally; concurrent identical requests are
// followers that wait for the leader's result and replay it. Only cacheable
// requests (non-stream, temperature 0) are ever coalesced — a positive
// temperature must yield an independent sample per caller. This is a tiny
// in-house single-flight (no new module dependency).

// flight is one in-progress leader computation others may wait on.
type flight struct {
	done chan struct{}
	val  cache.Value
	ok   bool // true when the leader produced a shareable result
}

// flightGroup deduplicates concurrent work by key.
type flightGroup struct {
	mu    sync.Mutex
	calls map[string]*flight
}

func newFlightGroup() *flightGroup { return &flightGroup{calls: make(map[string]*flight)} }

// SetCoalescing enables or disables single-flight request coalescing. Safe to
// call at startup.
func (s *Server) SetCoalescing(enabled bool) {
	if enabled {
		s.coalesce = newFlightGroup()
	} else {
		s.coalesce = nil
	}
}

// join registers the caller against key. The first caller is the leader
// (leader=true, f=nil) and must eventually publish(key, …). A concurrent caller
// is a follower (leader=false) and receives the in-flight f to wait on.
func (g *flightGroup) join(key string) (leader bool, f *flight) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if existing, ok := g.calls[key]; ok {
		return false, existing
	}
	nf := &flight{done: make(chan struct{})}
	g.calls[key] = nf
	return true, nil
}

// publish records the leader's result under key, wakes any followers, and clears
// the key so the next burst starts fresh. Must be called exactly once by the
// leader (defer it so early returns still release followers).
func (g *flightGroup) publish(key string, val cache.Value, ok bool) {
	g.mu.Lock()
	f := g.calls[key]
	delete(g.calls, key)
	g.mu.Unlock()
	if f == nil {
		return
	}
	f.val, f.ok = val, ok
	close(f.done)
}

// serveCoalesced replays a leader's shared response to a follower.
func serveCoalesced(w http.ResponseWriter, v cache.Value) {
	h := w.Header()
	for k, vs := range v.Header {
		for _, vv := range vs {
			h.Add(k, vv)
		}
	}
	h.Set("X-Mainspring-Coalesced", "true")
	w.WriteHeader(v.Status)
	_, _ = w.Write(v.Body)
}
