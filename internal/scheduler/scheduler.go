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
	"log"
	"strings"
	"sync"
	"time"

	"github.com/ankit373/mainspring/internal/backend"
)

// Scheduler manages loaded runners across one or more backends, dispatching
// each model to the backend named in its ModelSpec.
type Scheduler struct {
	backends  map[string]backend.Backend
	keepAlive time.Duration
	maxLoaded int
	maxBytes  int64

	// specsMu guards specs+aliases, which SIGHUP reload swaps at runtime.
	specsMu sync.RWMutex
	specs   map[string]backend.ModelSpec
	aliases map[string]string // friendly name -> target (spec id or another alias)

	mu      sync.Mutex
	running map[string]*loaded

	loadingMu sync.Mutex
	loading   map[string]*loadCall
}

// Options configures a Scheduler.
type Options struct {
	KeepAlive time.Duration     // idle-unload delay; <=0 disables idle unloading
	MaxLoaded int               // max models resident by count; <=0 => 1
	MaxBytes  int64             // max resident bytes across all models; <=0 => no byte cap
	Aliases   map[string]string // friendly name -> target model id (or another alias)
}

// maxAliasDepth bounds transitive alias resolution, which also detects cycles.
const maxAliasDepth = 16

type loaded struct {
	runner   backend.Runner
	spec     backend.ModelSpec
	lastUsed time.Time
	timer    *time.Timer
	inflight int  // requests actively using this runner right now; never evict while > 0
	stale    bool // spec changed (or model removed) during Reload while busy; evict once idle
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
	al := make(map[string]string, len(opts.Aliases))
	for name, target := range opts.Aliases {
		al[name] = target
	}
	return &Scheduler{
		backends:  backends,
		specs:     m,
		aliases:   al,
		keepAlive: opts.KeepAlive,
		maxLoaded: opts.MaxLoaded,
		maxBytes:  opts.MaxBytes,
		running:   make(map[string]*loaded),
		loading:   make(map[string]*loadCall),
	}
}

// resolveIn maps name to a real model id within the given spec/alias tables,
// following alias chains with a depth (cycle) guard. It is a pure function so
// both live resolution and pre-commit validation share one implementation.
func resolveIn(specs map[string]backend.ModelSpec, aliases map[string]string, name string) (string, bool) {
	cur := name
	for i := 0; i < maxAliasDepth; i++ {
		if _, ok := specs[cur]; ok {
			return cur, true
		}
		next, ok := aliases[cur]
		if !ok {
			return "", false
		}
		cur = next
	}
	return "", false // exceeded depth => cycle or too-long chain
}

// validateAliases checks every alias resolves to a real model within the tables.
func validateAliases(specs map[string]backend.ModelSpec, aliases map[string]string) error {
	for name := range aliases {
		if _, ok := resolveIn(specs, aliases, name); !ok {
			return fmt.Errorf("alias %q does not resolve to a known model (unknown target or cycle)", name)
		}
	}
	return nil
}

// Resolve maps a requested model name (which may be an alias, possibly chained)
// to a real, configured model id. It returns false if the name is neither a
// configured model nor an alias that resolves to one within maxAliasDepth (the
// cycle guard). Resolving a real id is idempotent.
func (s *Scheduler) Resolve(name string) (string, bool) {
	s.specsMu.RLock()
	defer s.specsMu.RUnlock()
	return resolveIn(s.specs, s.aliases, name)
}

// Aliases returns a copy of the configured alias table (friendly name -> target).
func (s *Scheduler) Aliases() map[string]string {
	s.specsMu.RLock()
	defer s.specsMu.RUnlock()
	out := make(map[string]string, len(s.aliases))
	for k, v := range s.aliases {
		out[k] = v
	}
	return out
}

// Validate checks that every configured alias resolves to a real model within
// the cycle guard. It is meant to be called once at startup so misconfiguration
// fails loud rather than surfacing as a 404 at request time.
func (s *Scheduler) Validate() error {
	s.specsMu.RLock()
	defer s.specsMu.RUnlock()
	return validateAliases(s.specs, s.aliases)
}

// Reload atomically swaps the model specs and aliases (e.g. on SIGHUP). It
// rejects the new set if any alias fails to resolve, leaving the old set intact.
// Running models whose spec changed or was removed are evicted so the next
// request reloads them under the new spec; unchanged models stay resident.
// Note: reload does not construct new backends, so a model referencing a
// backend not already built will only load after a restart.
func (s *Scheduler) Reload(specList []backend.ModelSpec, aliases map[string]string) error {
	newSpecs := make(map[string]backend.ModelSpec, len(specList))
	for _, sp := range specList {
		newSpecs[sp.ID] = sp
	}
	newAliases := make(map[string]string, len(aliases))
	for k, v := range aliases {
		newAliases[k] = v
	}
	if err := validateAliases(newSpecs, newAliases); err != nil {
		return err
	}

	s.specsMu.Lock()
	s.specs = newSpecs
	s.aliases = newAliases
	s.specsMu.Unlock()

	// Evict runners whose spec changed or disappeared — but never one that is
	// actively serving a request (the same in-flight protection idleEvict and
	// the LRU picker already apply). A busy, changed model is marked stale
	// instead: MarkIdle evicts it as soon as it actually finishes, so the fix
	// is self-healing without a second background sweep.
	var victims []backend.Runner
	s.mu.Lock()
	for id, l := range s.running {
		ns, ok := newSpecs[id]
		if ok && specEqual(ns, l.spec) {
			continue
		}
		if l.inflight > 0 {
			l.stale = true
			continue
		}
		if l.timer != nil {
			l.timer.Stop()
		}
		delete(s.running, id)
		victims = append(victims, l.runner)
	}
	s.mu.Unlock()
	for _, r := range victims {
		_ = r.Stop(context.Background())
	}
	return nil
}

// specEqual reports whether two specs describe the same runtime (so an unchanged
// model need not be reloaded on config reload).
func specEqual(a, b backend.ModelSpec) bool {
	if a.ID != b.ID || a.Backend != b.Backend || a.Path != b.Path ||
		a.CtxSize != b.CtxSize || a.GPULayers != b.GPULayers ||
		len(a.ExtraArgs) != len(b.ExtraArgs) || len(a.Fallbacks) != len(b.Fallbacks) {
		return false
	}
	for i := range a.ExtraArgs {
		if a.ExtraArgs[i] != b.ExtraArgs[i] {
			return false
		}
	}
	for i := range a.Fallbacks {
		if a.Fallbacks[i] != b.Fallbacks[i] {
			return false
		}
	}
	return true
}

// Known reports whether a model id is configured or is an alias that resolves
// to a configured model.
func (s *Scheduler) Known(id string) bool {
	_, ok := s.Resolve(id)
	return ok
}

// Models returns the configured model ids.
func (s *Scheduler) Models() []backend.ModelSpec {
	s.specsMu.RLock()
	defer s.specsMu.RUnlock()
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
	// Resolve aliases so residency keys on the real id (two aliases pointing at
	// one model share a single load) and direct callers need not pre-resolve.
	if real, ok := s.Resolve(modelID); ok {
		modelID = real
	}
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

// MarkBusy records that a request is actively using modelID's runner right
// now, deferring both idle-timeout and capacity eviction until the matching
// MarkIdle. Without this, a request whose total duration outlasts KeepAlive (or
// that loses a capacity race to a new model) would have its runner killed out
// from under it — idleEvict and the LRU picker have no other way to know a
// "stale-looking" runner is still in active use. No-op if the model isn't
// currently resident (e.g. a race with eviction) — nothing left to protect.
func (s *Scheduler) MarkBusy(modelID string) {
	if real, ok := s.Resolve(modelID); ok {
		modelID = real
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if l, ok := s.running[modelID]; ok {
		l.inflight++
	}
}

// MarkIdle is the matching release for MarkBusy. It also touches the model, so
// the idle clock starts counting from actual last-activity (when the request
// truly finished) rather than from when it started. If a config Reload marked
// this model stale (its spec changed while it was busy) and this was the last
// in-flight request, it is evicted now — self-healing, without a second
// background sweep — so the next request picks up the new spec.
func (s *Scheduler) MarkIdle(modelID string) {
	if real, ok := s.Resolve(modelID); ok {
		modelID = real
	}
	s.mu.Lock()
	l, ok := s.running[modelID]
	if !ok {
		s.mu.Unlock()
		return
	}
	if l.inflight > 0 {
		l.inflight--
	}
	s.touch(l)
	var victim backend.Runner
	if l.stale && l.inflight == 0 {
		if l.timer != nil {
			l.timer.Stop()
		}
		delete(s.running, modelID)
		victim = l.runner
	}
	s.mu.Unlock()
	if victim != nil {
		_ = victim.Stop(context.Background())
	}
}

func (s *Scheduler) load(ctx context.Context, modelID string) (backend.Runner, error) {
	s.specsMu.RLock()
	spec, ok := s.specs[modelID]
	s.specsMu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("unknown model %q", modelID)
	}

	// Try each candidate backend in order (primary, then fallbacks) until one
	// starts. This is the failover path: a down or misconfigured backend hands
	// off to the next. The runner reports which backend actually served via its
	// Capabilities (surfaced as X-Mainspring-Backend).
	candidates := spec.Candidates()
	var errs []string
	for _, bname := range candidates {
		be, ok := s.backends[bname]
		if !ok {
			errs = append(errs, fmt.Sprintf("%s: backend not configured", bname))
			continue
		}

		// Estimate the incoming footprint (if the backend can) so byte-budget
		// admission can make room before starting the process.
		var incoming int64
		if est, ok := be.(backend.MemoryEstimator); ok {
			incoming = est.EstimateMemory(spec)
		}
		for _, v := range s.makeRoom(incoming) {
			_ = v.Stop(context.Background())
		}

		r, err := be.Start(ctx, spec)
		if err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", bname, err))
			continue
		}

		s.mu.Lock()
		l := &loaded{runner: r, spec: spec, lastUsed: time.Now()}
		if s.keepAlive > 0 {
			l.timer = time.AfterFunc(s.keepAlive, func() { s.idleEvict(modelID) })
		}
		s.running[modelID] = l
		s.mu.Unlock()
		if bname != spec.Backend {
			log.Printf("model %q failed over to backend %q (primary %q unavailable)", modelID, bname, spec.Backend)
		}
		return r, nil
	}
	return nil, fmt.Errorf("model %q: all backends failed [%s]", modelID, strings.Join(errs, "; "))
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
// lruLocked returns the least-recently-used *idle* model, skipping any with an
// in-flight request — an active request must never be evicted to make room for
// another. Returns "" if every resident model is currently busy (the caller
// then admits the new one without evicting anything, over capacity but not
// broken — the same best-effort trade-off already made when a single model is
// larger than the whole budget).
func (s *Scheduler) lruLocked() string {
	var oldestID string
	var oldest time.Time
	first := true
	for id, l := range s.running {
		if l.inflight > 0 {
			continue
		}
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
	if l.inflight > 0 {
		// Never unload a runner mid-request; recheck after a full fresh window
		// rather than racing the in-flight request's own completion.
		l.timer.Reset(s.keepAlive)
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

// Unload evicts a specific model if resident, stopping its runner and reclaiming
// its memory. It resolves aliases first. It returns unloaded=true if a model
// was unloaded, false if it was not resident. Unlike the automatic idle/LRU/
// reload eviction paths, this is an explicit operator action and always takes
// priority over any in-flight request — but interrupted reports how many
// requests were actually using the runner at the moment it was force-stopped,
// so the operator knows whether they just interrupted live traffic (0 = the
// model was idle). The model can be reloaded on the next request.
func (s *Scheduler) Unload(modelID string) (unloaded bool, interrupted int) {
	if real, ok := s.Resolve(modelID); ok {
		modelID = real
	}
	s.mu.Lock()
	l, ok := s.running[modelID]
	if !ok {
		s.mu.Unlock()
		return false, 0
	}
	if l.timer != nil {
		l.timer.Stop()
	}
	delete(s.running, modelID)
	r, n := l.runner, l.inflight
	s.mu.Unlock()
	_ = r.Stop(context.Background())
	return true, n
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
