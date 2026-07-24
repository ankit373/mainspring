// Package scheduler owns model residency: which models are loaded, when to
// swap them, and when to unload idle ones. It is the VRAM brain that the
// incumbents get wrong (thrashing, leaked VRAM, evict-and-reload every call).
//
// v0 keeps a capacity cap (max concurrently-loaded models) with LRU eviction
// and a KeepAlive idle timer. A single-flight guard ensures a model is loaded
// exactly once even under concurrent first-hits. Phase 1 will make the cap
// VRAM-byte-aware rather than a simple count.
package scheduler

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/ankit373/mainspring/internal/backend"
)

// Scheduler manages loaded runners across one or more backends, dispatching
// each model to the backend named in its ModelSpec.
type Scheduler struct {
	backends  map[string]backend.Backend
	specs     map[string]backend.ModelSpec
	keepAlive time.Duration
	maxLoaded int
	maxBytes  int64

	mu      sync.Mutex
	running map[string]*loaded

	loadingMu sync.Mutex
	loading   map[string]*loadCall
}

// Options configures a Scheduler.
type Options struct {
	KeepAlive time.Duration // idle-unload delay; <=0 disables idle unloading
	MaxLoaded int           // max models resident by count; <=0 => 1
	MaxBytes  int64         // max resident bytes across all models; <=0 => no byte cap
}

type loaded struct {
	runner   backend.Runner
	spec     backend.ModelSpec
	lastUsed time.Time
	timer    *time.Timer
}

type loadCall struct {
	done   chan struct{}
	runner backend.Runner
	err    error
}

// New builds a scheduler from a set of named backends and Options. Each spec's
// Backend field selects which backend serves it.
func New(backends map[string]backend.Backend, specs []backend.ModelSpec, opts Options) *Scheduler {
	if opts.MaxLoaded <= 0 {
		opts.MaxLoaded = 1
	}
	m := make(map[string]backend.ModelSpec, len(specs))
	for _, s := range specs {
		m[s.ID] = s
	}
	return &Scheduler{
		backends:  backends,
		specs:     m,
		keepAlive: opts.KeepAlive,
		maxLoaded: opts.MaxLoaded,
		maxBytes:  opts.MaxBytes,
		running:   make(map[string]*loaded),
		loading:   make(map[string]*loadCall),
	}
}

// Known reports whether a model id is configured.
func (s *Scheduler) Known(id string) bool {
	_, ok := s.specs[id]
	return ok
}

// Models returns the configured model ids.
func (s *Scheduler) Models() []backend.ModelSpec {
	out := make([]backend.ModelSpec, 0, len(s.specs))
	for _, sp := range s.specs {
		out = append(out, sp)
	}
	return out
}

// EnsureLoaded returns a ready runner for modelID, loading it (and evicting the
// LRU model if at capacity) if necessary. Concurrent callers for the same model
// share one load.
func (s *Scheduler) EnsureLoaded(ctx context.Context, modelID string) (backend.Runner, error) {
	// Fast path: already loaded.
	s.mu.Lock()
	if l, ok := s.running[modelID]; ok {
		s.touch(l)
		r := l.runner
		s.mu.Unlock()
		return r, nil
	}
	s.mu.Unlock()

	// Single-flight the load.
	s.loadingMu.Lock()
	if c, ok := s.loading[modelID]; ok {
		s.loadingMu.Unlock()
		select {
		case <-c.done:
			return c.runner, c.err
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	c := &loadCall{done: make(chan struct{})}
	s.loading[modelID] = c
	s.loadingMu.Unlock()

	c.runner, c.err = s.load(ctx, modelID)
	close(c.done)

	s.loadingMu.Lock()
	delete(s.loading, modelID)
	s.loadingMu.Unlock()
	return c.runner, c.err
}

func (s *Scheduler) load(ctx context.Context, modelID string) (backend.Runner, error) {
	spec, ok := s.specs[modelID]
	if !ok {
		return nil, fmt.Errorf("unknown model %q", modelID)
	}
	be, ok := s.backends[spec.Backend]
	if !ok {
		return nil, fmt.Errorf("model %q references unavailable backend %q", modelID, spec.Backend)
	}

	// Estimate the incoming footprint (if the backend can) so byte-budget
	// admission can make room before starting the process.
	var incoming int64
	if est, ok := be.(backend.MemoryEstimator); ok {
		incoming = est.EstimateMemory(spec)
	}

	// Evict LRU victims (outside the lock) until there is room.
	for _, v := range s.makeRoom(incoming) {
		_ = v.Stop(context.Background())
	}

	r, err := be.Start(ctx, spec)
	if err != nil {
		return nil, err
	}

	s.mu.Lock()
	l := &loaded{runner: r, spec: spec, lastUsed: time.Now()}
	if s.keepAlive > 0 {
		l.timer = time.AfterFunc(s.keepAlive, func() { s.idleEvict(modelID) })
	}
	s.running[modelID] = l
	s.mu.Unlock()
	return r, nil
}

// makeRoom evicts LRU entries until there is room for a model of `incoming`
// bytes — under both the count cap and (when set) the byte budget. It returns
// the evicted runners for the caller to Stop outside the lock. If a single
// model is larger than the whole budget, everything else is evicted and it is
// still admitted (best effort — surfaced via /capabilities residency).
func (s *Scheduler) makeRoom(incoming int64) []backend.Runner {
	s.mu.Lock()
	defer s.mu.Unlock()
	var victims []backend.Runner
	for len(s.running) > 0 {
		overCount := len(s.running) >= s.maxLoaded
		overBytes := s.maxBytes > 0 && s.usedBytesLocked()+incoming > s.maxBytes
		if !overCount && !overBytes {
			break
		}
		oldestID := s.lruLocked()
		if oldestID == "" {
			break
		}
		l := s.running[oldestID]
		if l.timer != nil {
			l.timer.Stop()
		}
		delete(s.running, oldestID)
		victims = append(victims, l.runner)
	}
	return victims
}

// usedBytesLocked sums the resident footprint of loaded runners (caller holds mu).
func (s *Scheduler) usedBytesLocked() int64 {
	var total int64
	for _, l := range s.running {
		total += l.runner.MemoryBytes()
	}
	return total
}

// lruLocked returns the id of the least-recently-used model (caller holds mu).
func (s *Scheduler) lruLocked() string {
	var oldestID string
	var oldest time.Time
	first := true
	for id, l := range s.running {
		if first || l.lastUsed.Before(oldest) {
			oldestID, oldest, first = id, l.lastUsed, false
		}
	}
	return oldestID
}

func (s *Scheduler) idleEvict(modelID string) {
	s.mu.Lock()
	l, ok := s.running[modelID]
	if !ok {
		s.mu.Unlock()
		return
	}
	if idle := time.Since(l.lastUsed); idle < s.keepAlive {
		l.timer.Reset(s.keepAlive - idle) // touched since arm; reschedule
		s.mu.Unlock()
		return
	}
	delete(s.running, modelID)
	r := l.runner
	s.mu.Unlock()
	_ = r.Stop(context.Background())
}

// touch marks a loaded model as recently used (caller holds s.mu).
func (s *Scheduler) touch(l *loaded) {
	l.lastUsed = time.Now()
	if l.timer != nil {
		l.timer.Reset(s.keepAlive)
	}
}

// RunnerInfo pairs a model id with its live runner.
type RunnerInfo struct {
	ID     string
	Runner backend.Runner
}

// Loaded returns a snapshot of currently-loaded runners.
func (s *Scheduler) Loaded() []RunnerInfo {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]RunnerInfo, 0, len(s.running))
	for id, l := range s.running {
		out = append(out, RunnerInfo{ID: id, Runner: l.runner})
	}
	return out
}

// Residency reports current resident bytes, the configured byte budget (0 =
// unbounded), and the number of loaded models — the signal Hydra's router can
// use to factor swap cost into routing.
func (s *Scheduler) Residency() (used, budget int64, count int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.usedBytesLocked(), s.maxBytes, len(s.running)
}

// Shutdown stops every loaded runner, reclaiming all memory.
func (s *Scheduler) Shutdown(ctx context.Context) {
	s.mu.Lock()
	runners := make([]backend.Runner, 0, len(s.running))
	for id, l := range s.running {
		if l.timer != nil {
			l.timer.Stop()
		}
		runners = append(runners, l.runner)
		delete(s.running, id)
	}
	s.mu.Unlock()
	for _, r := range runners {
		_ = r.Stop(ctx)
	}
}
