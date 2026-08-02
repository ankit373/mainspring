// Package breaker implements a per-key circuit breaker: after a backend fails
// repeatedly, the breaker opens and requests fast-fail (no upstream call, no
// timeout paid) until a cooldown elapses, then a single half-open probe decides
// whether to close it again. Mainspring keys breakers by model so one flapping
// backend can't drag down the others. The clock is injectable for testing.
package breaker

import (
	"io"
	"sort"
	"sync"
	"time"

	"github.com/ankit373/mainspring/internal/util"
)

// State is a breaker's lifecycle state.
type State int

const (
	Closed   State = iota // healthy: requests flow
	Open                  // failing: requests fast-fail until cooldown elapses
	HalfOpen              // cooldown elapsed: one probe request is allowed
)

func (s State) String() string {
	switch s {
	case Open:
		return "open"
	case HalfOpen:
		return "half_open"
	default:
		return "closed"
	}
}

// breaker is one key's state.
type breaker struct {
	state    State
	failures int
	openedAt time.Time
}

// Group manages one breaker per key.
type Group struct {
	threshold int           // consecutive failures that open the breaker
	cooldown  time.Duration // how long to stay open before a half-open probe
	now       func() time.Time

	mu       sync.Mutex
	breakers map[string]*breaker
}

// NewGroup builds a breaker group. threshold<=0 or cooldown<=0 disables it
// (Allow always permits, results are ignored).
func NewGroup(threshold int, cooldown time.Duration) *Group {
	return &Group{
		threshold: threshold,
		cooldown:  cooldown,
		now:       time.Now,
		breakers:  make(map[string]*breaker),
	}
}

// SetClock overrides the time source (tests only).
func (g *Group) SetClock(now func() time.Time) { g.now = now }

// Enabled reports whether breaking is active.
func (g *Group) Enabled() bool { return g != nil && g.threshold > 0 && g.cooldown > 0 }

// Config returns the configured failure threshold and cooldown (for reporting).
func (g *Group) Config() (threshold int, cooldown time.Duration) {
	if g == nil {
		return 0, 0
	}
	return g.threshold, g.cooldown
}

// Allow reports whether a request for key may proceed. When it returns false the
// caller must fast-fail (the breaker is open and still cooling down, or a
// half-open probe is already in flight). It transitions Open→HalfOpen once the
// cooldown has elapsed and lets exactly one probe through.
func (g *Group) Allow(key string) bool {
	if !g.Enabled() {
		return true
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	b := g.breakers[key]
	if b == nil || b.state == Closed {
		return true
	}
	if b.state == HalfOpen {
		return false // a probe is already in flight; don't pile on
	}
	// Open: allow a single probe once the cooldown has elapsed.
	if g.now().Sub(b.openedAt) >= g.cooldown {
		b.state = HalfOpen
		return true
	}
	return false
}

// OnAbandoned releases a request that produced no verdict about the backend —
// a caller that cancelled before the answer arrived. Such a request says nothing
// about backend health, so the failure count must not move in either direction:
// recording a failure would let client disconnects open the circuit for every
// tenant, and recording a success would clear the count, letting a client with a
// flaky connection hold a genuinely broken backend's circuit closed indefinitely.
//
// The one thing that must not leak is a consumed probe. Allow() moves
// Open -> HalfOpen and then refuses every later caller until OnResult resolves it,
// so an abandoned probe would wedge the breaker half-open forever. Put it back to
// Open, leaving openedAt alone: the cooldown has already elapsed, so the next
// caller takes the probe immediately rather than serving another full cooldown for
// someone else's cancellation.
func (g *Group) OnAbandoned(key string) {
	if !g.Enabled() {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if b := g.breakers[key]; b != nil && b.state == HalfOpen {
		b.state = Open
	}
}

// OnResult records the outcome of a request (or health probe) for key.
func (g *Group) OnResult(key string, success bool) {
	if !g.Enabled() {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	b := g.breakers[key]
	if b == nil {
		b = &breaker{}
		g.breakers[key] = b
	}
	if success {
		// Any success closes the breaker and clears the failure count.
		b.state = Closed
		b.failures = 0
		return
	}
	// Failure.
	if b.state == HalfOpen {
		// Probe failed: re-open and restart the cooldown.
		b.state = Open
		b.openedAt = g.now()
		return
	}
	b.failures++
	if b.failures >= g.threshold {
		b.state = Open
		b.openedAt = g.now()
	}
}

// Reset forces key's breaker back to Closed and clears its failure count,
// letting an operator recover immediately (e.g. after manually confirming the
// backend is healthy again) rather than waiting out the cooldown. A no-op when
// breaking is disabled or key has never tripped.
func (g *Group) Reset(key string) {
	if !g.Enabled() {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if b := g.breakers[key]; b != nil {
		b.state = Closed
		b.failures = 0
	}
}

// State returns the current state for key (Closed if unseen).
func (g *Group) State(key string) State {
	if !g.Enabled() {
		return Closed
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if b := g.breakers[key]; b != nil {
		return b.state
	}
	return Closed
}

// WritePrometheus emits a per-key open-state gauge (1 when open or half-open,
// preserving existing dashboard semantics), plus a separate half-open gauge so
// "still failing, cooldown running" is distinguishable from "cooldown elapsed,
// actively probing recovery" without switching to /v1/quality, which already
// reports the granular state string per model.
func (g *Group) WritePrometheus(w io.Writer) {
	if !g.Enabled() {
		return
	}
	g.mu.Lock()
	states := make(map[string]State, len(g.breakers))
	keys := make([]string, 0, len(g.breakers))
	for k, b := range g.breakers {
		states[k] = b.state
		keys = append(keys, k)
	}
	g.mu.Unlock()
	sort.Strings(keys)

	io.WriteString(w, "# HELP mainspring_breaker_open Circuit breaker tripped (1=open/half-open) by model.\n")
	io.WriteString(w, "# TYPE mainspring_breaker_open gauge\n")
	for _, k := range keys {
		v := 0
		if states[k] != Closed {
			v = 1
		}
		writeGauge(w, "mainspring_breaker_open", k, v)
	}

	io.WriteString(w, "# HELP mainspring_breaker_half_open Circuit breaker actively probing recovery (1=half-open) by model.\n")
	io.WriteString(w, "# TYPE mainspring_breaker_half_open gauge\n")
	for _, k := range keys {
		v := 0
		if states[k] == HalfOpen {
			v = 1
		}
		writeGauge(w, "mainspring_breaker_half_open", k, v)
	}
}

func writeGauge(w io.Writer, name, model string, v int) {
	io.WriteString(w, name)
	io.WriteString(w, "{model=\"")
	io.WriteString(w, util.PromLabelValue(model))
	io.WriteString(w, "\"} ")
	if v == 1 {
		io.WriteString(w, "1\n")
	} else {
		io.WriteString(w, "0\n")
	}
}
