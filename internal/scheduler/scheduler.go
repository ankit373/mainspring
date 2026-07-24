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

// Scheduler manages loaded runners for a single backend (v0).
type Scheduler struct {
	be        backend.Backend
	specs     map[string]backend.ModelSpec
	keepAlive time.Duration
	maxLoaded int

	mu      sync.Mutex
	running map[string]*loaded

	loadingMu sync.Mutex
	loading   map[string]*loadCall
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

// New builds a scheduler. keepAlive <= 0 disables idle unloading; maxLoaded <= 0
// means one model at a time (swap on switch).
func New(be backend.Backend, specs []backend.ModelSpec, keepAlive time.Duration, maxLoaded int) *Scheduler {
	if maxLoaded <= 0 {
		maxLoaded = 1
	}
	m := make(map[string]backend.ModelSpec, len(specs))
	for _, s := range specs {
		m[s.ID] = s
	}
	return &Scheduler{
		be:        be,
		specs:     m,
		keepAlive: keepAlive,
		maxLoaded: maxLoaded,
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

	// Evict LRU victims (outside the lock) until there is room.
	for _, v := range s.evictionVictims() {
		_ = v.Stop(context.Background())
	}

	r, err := s.be.Start(ctx, spec)
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

// evictionVictims removes enough LRU entries from the map to make room for one
// more and returns their runners to be stopped by the caller (outside the lock).
func (s *Scheduler) evictionVictims() []backend.Runner {
	s.mu.Lock()
	defer s.mu.Unlock()
	var victims []backend.Runner
	for len(s.running) >= s.maxLoaded {
		var oldestID string
		var oldest time.Time
		first := true
		for id, l := range s.running {
			if first || l.lastUsed.Before(oldest) {
				oldestID, oldest, first = id, l.lastUsed, false
			}
		}
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
