package server

import (
	"context"
	"fmt"
	"io"
	"sort"
	"sync"
	"time"
)

// gate bounds concurrent inference per model: at most maxInflight requests run
// at once, with up to maxQueue additional waiters; beyond that it rejects with
// backpressure (503) instead of letting latency collapse. maxInflight <= 0
// disables gating entirely (unbounded, the default).
type gate struct {
	maxInflight int
	maxQueue    int

	mu       sync.Mutex
	sems     map[string]chan struct{}
	inflight map[string]int
	queued   map[string]int
}

func newGate(maxInflight, maxQueue int) *gate {
	return &gate{
		maxInflight: maxInflight,
		maxQueue:    maxQueue,
		sems:        map[string]chan struct{}{},
		inflight:    map[string]int{},
		queued:      map[string]int{},
	}
}

func (g *gate) enabled() bool { return g.maxInflight > 0 }

// gateResult is how an acquire ended. Queue-full and caller-cancelled are
// distinct outcomes: the first is backpressure the client must be told about,
// the second means the client is already gone and a 503 would be written to a
// dead connection.
type gateResult int

const (
	gateAcquired  gateResult = iota // slot reserved; release is non-nil
	gateFull                        // no slot and the wait queue is full → reject
	gateCancelled                   // the caller's context ended while queued
)

// acquire reserves a slot for model, blocking (as a queued waiter) until one is
// free. It returns a release func, the time spent waiting for a slot (0 when
// gating is disabled or a slot was immediately free), and which of the three
// outcomes occurred. release is nil unless the result is gateAcquired.
func (g *gate) acquire(ctx context.Context, model string) (release func(), waitTime time.Duration, res gateResult) {
	if !g.enabled() {
		return func() {}, 0, gateAcquired
	}
	start := time.Now()
	g.mu.Lock()
	sem := g.sems[model]
	if sem == nil {
		sem = make(chan struct{}, g.maxInflight)
		g.sems[model] = sem
	}
	free := g.maxInflight - g.inflight[model]
	if free <= 0 && g.queued[model] >= g.maxQueue {
		g.mu.Unlock()
		return nil, 0, gateFull // no free slot and the wait queue is full → backpressure
	}
	g.queued[model]++
	g.mu.Unlock()

	select {
	case sem <- struct{}{}:
		g.mu.Lock()
		g.queued[model]--
		g.inflight[model]++
		g.mu.Unlock()
		return func() {
			<-sem
			g.mu.Lock()
			g.inflight[model]--
			g.mu.Unlock()
		}, time.Since(start), gateAcquired
	case <-ctx.Done():
		g.mu.Lock()
		g.queued[model]--
		g.mu.Unlock()
		return nil, time.Since(start), gateCancelled
	}
}

// stat returns the current in-flight and queued counts for one model. Both are 0
// when gating is disabled (the counters are only maintained when enabled).
func (g *gate) stat(model string) (inflight, queued int) {
	if !g.enabled() {
		return 0, 0
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.inflight[model], g.queued[model]
}

// writePrometheus appends inflight/queued gauges (no-op when disabled).
func (g *gate) writePrometheus(w io.Writer) {
	if !g.enabled() {
		return
	}
	g.mu.Lock()
	models := make([]string, 0, len(g.sems))
	for m := range g.sems {
		models = append(models, m)
	}
	sort.Strings(models)
	inflight := make(map[string]int, len(models))
	queued := make(map[string]int, len(models))
	for _, m := range models {
		inflight[m] = g.inflight[m]
		queued[m] = g.queued[m]
	}
	g.mu.Unlock()

	fmt.Fprint(w, "# HELP mainspring_inflight_requests In-flight inference requests by model.\n# TYPE mainspring_inflight_requests gauge\n")
	for _, m := range models {
		fmt.Fprintf(w, "mainspring_inflight_requests{model=%q} %d\n", esc(m), inflight[m])
	}
	fmt.Fprint(w, "# HELP mainspring_queued_requests Queued (waiting) inference requests by model.\n# TYPE mainspring_queued_requests gauge\n")
	for _, m := range models {
		fmt.Fprintf(w, "mainspring_queued_requests{model=%q} %d\n", esc(m), queued[m])
	}
}

// esc escapes a Prometheus label value.
func esc(s string) string {
	out := make([]rune, 0, len(s))
	for _, r := range s {
		switch r {
		case '\\', '"':
			out = append(out, '\\', r)
		case '\n':
			out = append(out, '\\', 'n')
		default:
			out = append(out, r)
		}
	}
	return string(out)
}
