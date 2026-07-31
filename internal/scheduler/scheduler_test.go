package scheduler

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ankit373/mainspring/internal/backend"
)

// fakeRunner is a no-op backend.Runner that records Stop calls.
type fakeRunner struct {
	id      string
	mem     int64
	stopped atomic.Bool
}

func (r *fakeRunner) BaseURL() string { return "http://fake/" + r.id }
func (r *fakeRunner) Health(context.Context) backend.Status { return backend.StatusReady }
func (r *fakeRunner) MemoryBytes() int64 {
	if r.mem == 0 {
		return 1
	}
	return r.mem
}
func (r *fakeRunner) Stop(context.Context) error { r.stopped.Store(true); return nil }
func (r *fakeRunner) Capabilities(context.Context) (backend.Capabilities, error) {
	return backend.Capabilities{Backend: "fake", Model: r.id, GPUOffload: true}, nil
}

// fakeBackend counts Start calls per model. mem, when set, is reported both as
// the pre-load estimate (MemoryEstimator) and the runner's resident bytes.
type fakeBackend struct {
	starts sync.Map // id -> *atomic.Int64
	delay  time.Duration
	mem    int64
}

func (b *fakeBackend) Name() string { return "fake" }
func (b *fakeBackend) Detect(context.Context) backend.Availability {
	return backend.Availability{Name: "fake", Present: true}
}
func (b *fakeBackend) Start(_ context.Context, spec backend.ModelSpec) (backend.Runner, error) {
	c, _ := b.starts.LoadOrStore(spec.ID, &atomic.Int64{})
	c.(*atomic.Int64).Add(1)
	if b.delay > 0 {
		time.Sleep(b.delay)
	}
	return &fakeRunner{id: spec.ID, mem: b.mem}, nil
}
func (b *fakeBackend) EstimateMemory(backend.ModelSpec) int64 { return b.mem }
func (b *fakeBackend) startCount(id string) int64 {
	c, ok := b.starts.Load(id)
	if !ok {
		return 0
	}
	return c.(*atomic.Int64).Load()
}

func specs(ids ...string) []backend.ModelSpec {
	out := make([]backend.ModelSpec, len(ids))
	for i, id := range ids {
		out[i] = backend.ModelSpec{ID: id, Backend: "fake"}
	}
	return out
}

// bmap wraps a single backend as the named-backend map the scheduler expects.
func bmap(be backend.Backend) map[string]backend.Backend {
	return map[string]backend.Backend{"fake": be}
}

func TestEnsureLoadedSingleFlight(t *testing.T) {
	be := &fakeBackend{delay: 20 * time.Millisecond}
	s := New(bmap(be), specs("a"), Options{MaxLoaded: 4})

	var wg sync.WaitGroup
	runners := make([]backend.Runner, 20)
	for i := range runners {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			r, err := s.EnsureLoaded(context.Background(), "a")
			if err != nil {
				t.Errorf("EnsureLoaded: %v", err)
				return
			}
			runners[i] = r
		}(i)
	}
	wg.Wait()

	if got := be.startCount("a"); got != 1 {
		t.Fatalf("expected exactly 1 Start under concurrency, got %d", got)
	}
	for i, r := range runners {
		if r != runners[0] {
			t.Fatalf("runner %d differs — single-flight returned distinct runners", i)
		}
	}
}

func TestEnsureLoadedEvictsLRU(t *testing.T) {
	be := &fakeBackend{}
	s := New(bmap(be), specs("a", "b"), Options{MaxLoaded: 1}) // capacity 1 → loading b evicts a

	ra, err := s.EnsureLoaded(context.Background(), "a")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.EnsureLoaded(context.Background(), "b"); err != nil {
		t.Fatal(err)
	}

	if !ra.(*fakeRunner).stopped.Load() {
		t.Fatal("expected model a to be stopped (evicted) when b loaded at capacity 1")
	}
	loaded := s.Loaded()
	if len(loaded) != 1 || loaded[0].ID != "b" {
		t.Fatalf("expected only b loaded, got %+v", loaded)
	}
}

func TestByteBudgetEvicts(t *testing.T) {
	// Each model is 6 units; budget is 10 and count cap is high — so a second
	// model cannot co-reside and must evict the first on the byte budget alone.
	be := &fakeBackend{mem: 6}
	s := New(bmap(be), specs("a", "b"), Options{MaxLoaded: 10, MaxBytes: 10})

	ra, err := s.EnsureLoaded(context.Background(), "a")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.EnsureLoaded(context.Background(), "b"); err != nil {
		t.Fatal(err)
	}
	if !ra.(*fakeRunner).stopped.Load() {
		t.Fatal("expected a evicted on byte budget (6+6 > 10)")
	}
	if loaded := s.Loaded(); len(loaded) != 1 || loaded[0].ID != "b" {
		t.Fatalf("expected only b resident, got %+v", loaded)
	}
}

func TestByteBudgetAllowsCoresidence(t *testing.T) {
	// Two 4-unit models fit within a 10-unit budget — both stay resident.
	be := &fakeBackend{mem: 4}
	s := New(bmap(be), specs("a", "b"), Options{MaxLoaded: 10, MaxBytes: 10})
	ra, _ := s.EnsureLoaded(context.Background(), "a")
	_, _ = s.EnsureLoaded(context.Background(), "b")
	if ra.(*fakeRunner).stopped.Load() {
		t.Fatal("a should not be evicted (4+4 <= 10)")
	}
	if len(s.Loaded()) != 2 {
		t.Fatalf("expected both resident, got %d", len(s.Loaded()))
	}
}

func TestPerModelBackendRouting(t *testing.T) {
	ba := &fakeBackend{}
	bb := &fakeBackend{}
	s := New(
		map[string]backend.Backend{"A": ba, "B": bb},
		[]backend.ModelSpec{{ID: "ma", Backend: "A"}, {ID: "mb", Backend: "B"}},
		Options{MaxLoaded: 5},
	)
	if _, err := s.EnsureLoaded(context.Background(), "ma"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.EnsureLoaded(context.Background(), "mb"); err != nil {
		t.Fatal(err)
	}
	if ba.startCount("ma") != 1 || bb.startCount("mb") != 1 {
		t.Fatalf("each model should start on its own backend: A[ma]=%d B[mb]=%d", ba.startCount("ma"), bb.startCount("mb"))
	}
	if ba.startCount("mb") != 0 || bb.startCount("ma") != 0 {
		t.Fatal("a model must not be started on the wrong backend")
	}
}

func TestUnavailableBackendErrors(t *testing.T) {
	s := New(
		map[string]backend.Backend{"A": &fakeBackend{}},
		[]backend.ModelSpec{{ID: "m", Backend: "missing"}},
		Options{},
	)
	if _, err := s.EnsureLoaded(context.Background(), "m"); err == nil {
		t.Fatal("model referencing an unavailable backend must error")
	}
}

func TestUnknownModel(t *testing.T) {
	s := New(bmap(&fakeBackend{}), specs("a"), Options{MaxLoaded: 1})
	if _, err := s.EnsureLoaded(context.Background(), "nope"); err == nil {
		t.Fatal("expected error for unknown model")
	}
}

func TestAliasResolution(t *testing.T) {
	s := New(bmap(&fakeBackend{}), specs("real-a"),
		Options{MaxLoaded: 1, Aliases: map[string]string{"gpt-4o": "real-a", "fast": "gpt-4o"}})

	if got, ok := s.Resolve("gpt-4o"); !ok || got != "real-a" {
		t.Fatalf("direct alias => %q,%v; want real-a,true", got, ok)
	}
	if got, ok := s.Resolve("fast"); !ok || got != "real-a" {
		t.Fatalf("chained alias => %q,%v; want real-a,true", got, ok)
	}
	if got, ok := s.Resolve("real-a"); !ok || got != "real-a" {
		t.Fatalf("real id resolve should be idempotent, got %q,%v", got, ok)
	}
	if _, ok := s.Resolve("missing"); ok {
		t.Fatal("unknown name must not resolve")
	}
	if !s.Known("gpt-4o") || s.Known("missing") {
		t.Fatal("Known must follow alias resolution")
	}

	// An alias loads the underlying real model.
	r, err := s.EnsureLoaded(context.Background(), "fast")
	if err != nil {
		t.Fatal(err)
	}
	if r.(*fakeRunner).id != "real-a" {
		t.Fatalf("alias loaded %q, want real-a", r.(*fakeRunner).id)
	}
}

func TestAliasValidation(t *testing.T) {
	// Unknown target.
	bad := New(bmap(&fakeBackend{}), specs("a"), Options{Aliases: map[string]string{"x": "nope"}})
	if err := bad.Validate(); err == nil {
		t.Fatal("alias to unknown target must fail Validate")
	}
	// Cycle.
	cyc := New(bmap(&fakeBackend{}), specs("a"),
		Options{Aliases: map[string]string{"x": "y", "y": "x"}})
	if err := cyc.Validate(); err == nil {
		t.Fatal("alias cycle must fail Validate")
	}
	// Valid.
	ok := New(bmap(&fakeBackend{}), specs("a"), Options{Aliases: map[string]string{"x": "a"}})
	if err := ok.Validate(); err != nil {
		t.Fatalf("valid alias failed Validate: %v", err)
	}
}

func TestReloadSwapsSpecsAndAliases(t *testing.T) {
	be := &fakeBackend{}
	s := New(bmap(be), specs("a", "b"),
		Options{MaxLoaded: 3, Aliases: map[string]string{"x": "a"}})

	// New config: drop b, add c, repoint alias x -> c.
	err := s.Reload(specs("a", "c"), map[string]string{"x": "c"})
	if err != nil {
		t.Fatal(err)
	}
	if !s.Known("c") || s.Known("b") {
		t.Fatal("Reload must add c and drop b")
	}
	if got, _ := s.Resolve("x"); got != "c" {
		t.Fatalf("alias not repointed: got %q, want c", got)
	}
}

func TestReloadEvictsChangedModel(t *testing.T) {
	be := &fakeBackend{}
	s := New(bmap(be), specs("a"), Options{MaxLoaded: 2})
	r, _ := s.EnsureLoaded(context.Background(), "a")

	// Same id but a different path => must be evicted (stale runner stopped).
	changed := []backend.ModelSpec{{ID: "a", Backend: "fake", Path: "/new/path"}}
	if err := s.Reload(changed, nil); err != nil {
		t.Fatal(err)
	}
	if !r.(*fakeRunner).stopped.Load() {
		t.Fatal("changed spec should evict (Stop) the running model")
	}
	if len(s.Loaded()) != 0 {
		t.Fatal("evicted model should not remain loaded")
	}
}

// TestReloadDefersEvictionWhileBusy proves the fix: a config reload that
// changes a busy model's spec must not kill it mid-request. It survives until
// MarkIdle, then evicts promptly (self-healing, no restart needed).
func TestReloadDefersEvictionWhileBusy(t *testing.T) {
	be := &fakeBackend{}
	s := New(bmap(be), specs("a"), Options{MaxLoaded: 2})
	r, _ := s.EnsureLoaded(context.Background(), "a")
	s.MarkBusy("a")

	// Same id but a different path — would normally be evicted immediately.
	changed := []backend.ModelSpec{{ID: "a", Backend: "fake", Path: "/new/path"}}
	if err := s.Reload(changed, nil); err != nil {
		t.Fatal(err)
	}
	if r.(*fakeRunner).stopped.Load() {
		t.Fatal("busy model must not be evicted by a reload mid-request")
	}
	if len(s.Loaded()) != 1 {
		t.Fatal("busy model should still be resident immediately after reload")
	}

	// Once idle, the deferred eviction fires.
	s.MarkIdle("a")
	if !r.(*fakeRunner).stopped.Load() {
		t.Fatal("stale model should be evicted as soon as it goes idle")
	}
	if len(s.Loaded()) != 0 {
		t.Fatal("stale model should no longer be resident after MarkIdle")
	}
}

// A busy model whose spec did NOT change must survive reload regardless, and
// must not be evicted later by an unrelated MarkIdle (it was never stale).
func TestReloadUnchangedBusyModelNeverMarkedStale(t *testing.T) {
	be := &fakeBackend{}
	s := New(bmap(be), specs("a"), Options{MaxLoaded: 2})
	r, _ := s.EnsureLoaded(context.Background(), "a")
	s.MarkBusy("a")

	if err := s.Reload(specs("a"), nil); err != nil {
		t.Fatal(err)
	}
	s.MarkIdle("a")
	if r.(*fakeRunner).stopped.Load() {
		t.Fatal("unchanged model must never be evicted, even after going idle post-reload")
	}
}

func TestReloadKeepsUnchangedModelResident(t *testing.T) {
	be := &fakeBackend{}
	s := New(bmap(be), specs("a", "b"), Options{MaxLoaded: 3})
	ra, _ := s.EnsureLoaded(context.Background(), "a")

	// Reload with identical spec for a (plus b): a stays resident, not restarted.
	if err := s.Reload(specs("a", "b"), nil); err != nil {
		t.Fatal(err)
	}
	if ra.(*fakeRunner).stopped.Load() {
		t.Fatal("unchanged model must stay resident across reload")
	}
	if be.startCount("a") != 1 {
		t.Fatalf("unchanged model should not reload, starts=%d", be.startCount("a"))
	}
}

func TestReloadRejectsBadAliases(t *testing.T) {
	s := New(bmap(&fakeBackend{}), specs("a"), Options{Aliases: map[string]string{"x": "a"}})
	// Alias points at a model that won't exist after reload => reject, keep old.
	if err := s.Reload(specs("a"), map[string]string{"x": "gone"}); err == nil {
		t.Fatal("Reload must reject unresolvable aliases")
	}
	if got, _ := s.Resolve("x"); got != "a" {
		t.Fatalf("rejected reload must leave old aliases intact, got %q", got)
	}
}

func TestReloadConcurrentWithResolve(t *testing.T) {
	be := &fakeBackend{}
	s := New(bmap(be), specs("a"), Options{MaxLoaded: 4, Aliases: map[string]string{"x": "a"}})

	var wg sync.WaitGroup
	// Readers hammer Resolve/EnsureLoaded/Models while writers reload — the race
	// detector must find no data race on specs/aliases.
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				s.Resolve("x")
				s.Known("a")
				_ = s.Models()
				_, _ = s.EnsureLoaded(context.Background(), "a")
			}
		}()
	}
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				_ = s.Reload(specs("a", "b"), map[string]string{"x": "a"})
			}
		}(i)
	}
	wg.Wait()
}

// downBackend fails every Start (simulates a dead engine).
type downBackend struct{ name string }

func (b *downBackend) Name() string { return b.name }
func (b *downBackend) Detect(context.Context) backend.Availability {
	return backend.Availability{Name: b.name, Present: false, Reason: "down"}
}
func (b *downBackend) Start(context.Context, backend.ModelSpec) (backend.Runner, error) {
	return nil, context.DeadlineExceeded
}

func TestFallbackToSecondBackend(t *testing.T) {
	up := &fakeBackend{}
	backends := map[string]backend.Backend{"a": &downBackend{name: "a"}, "b": up}
	spec := backend.ModelSpec{ID: "m", Backend: "a", Fallbacks: []string{"b"}}
	s := New(backends, []backend.ModelSpec{spec}, Options{MaxLoaded: 2})

	r, err := s.EnsureLoaded(context.Background(), "m")
	if err != nil {
		t.Fatalf("fallback should have loaded via b: %v", err)
	}
	if r.(*fakeRunner).id != "m" {
		t.Fatalf("unexpected runner id %q", r.(*fakeRunner).id)
	}
	if up.startCount("m") != 1 {
		t.Fatalf("fallback backend b should have started the model, starts=%d", up.startCount("m"))
	}
}

func TestFallbackAllDownErrors(t *testing.T) {
	backends := map[string]backend.Backend{"a": &downBackend{name: "a"}, "b": &downBackend{name: "b"}}
	spec := backend.ModelSpec{ID: "m", Backend: "a", Fallbacks: []string{"b"}}
	s := New(backends, []backend.ModelSpec{spec}, Options{MaxLoaded: 2})

	if _, err := s.EnsureLoaded(context.Background(), "m"); err == nil {
		t.Fatal("all candidates down should error")
	} else if !strings.Contains(err.Error(), "all backends failed") {
		t.Fatalf("error should mention all backends failed: %v", err)
	}
}

func TestCandidatesOrderAndDedup(t *testing.T) {
	m := backend.ModelSpec{Backend: "a", Fallbacks: []string{"a", "b", "", "c", "b"}}
	got := m.Candidates()
	want := []string{"a", "b", "c"}
	if len(got) != len(want) {
		t.Fatalf("candidates=%v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("candidates=%v, want %v", got, want)
		}
	}
}

func TestUnloadEvictsSpecificModel(t *testing.T) {
	be := &fakeBackend{}
	s := New(bmap(be), specs("a", "b"), Options{MaxLoaded: 3})
	ra, _ := s.EnsureLoaded(context.Background(), "a")
	_, _ = s.EnsureLoaded(context.Background(), "b")

	if !s.Unload("a") {
		t.Fatal("Unload should report true for a resident model")
	}
	if !ra.(*fakeRunner).stopped.Load() {
		t.Fatal("Unload must Stop the runner")
	}
	if len(s.Loaded()) != 1 {
		t.Fatalf("only b should remain, got %d loaded", len(s.Loaded()))
	}
	if s.Unload("a") {
		t.Fatal("Unloading a non-resident model should return false")
	}
	// A reload path still works after unload.
	if _, err := s.EnsureLoaded(context.Background(), "a"); err != nil {
		t.Fatalf("model should reload after unload: %v", err)
	}
}

func TestUnloadResolvesAlias(t *testing.T) {
	be := &fakeBackend{}
	s := New(bmap(be), specs("real"), Options{MaxLoaded: 2, Aliases: map[string]string{"friendly": "real"}})
	_, _ = s.EnsureLoaded(context.Background(), "friendly")
	if !s.Unload("friendly") {
		t.Fatal("Unload should resolve the alias and evict the real model")
	}
}

// TestIdleEvictStopsGenuinelyUnusedModel is the baseline: with no MarkBusy in
// play, a model that goes untouched past KeepAlive is evicted as before.
func TestIdleEvictStopsGenuinelyUnusedModel(t *testing.T) {
	be := &fakeBackend{}
	s := New(bmap(be), specs("a"), Options{MaxLoaded: 2, KeepAlive: 30 * time.Millisecond})
	ra, err := s.EnsureLoaded(context.Background(), "a")
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	if !ra.(*fakeRunner).stopped.Load() {
		t.Fatal("genuinely idle model should have been evicted past KeepAlive")
	}
}

// TestMarkBusyPreventsIdleEviction proves the fix: a request whose duration
// outlasts KeepAlive must not have its runner killed mid-flight.
func TestMarkBusyPreventsIdleEviction(t *testing.T) {
	be := &fakeBackend{}
	s := New(bmap(be), specs("a"), Options{MaxLoaded: 2, KeepAlive: 30 * time.Millisecond})
	ra, err := s.EnsureLoaded(context.Background(), "a")
	if err != nil {
		t.Fatal(err)
	}
	s.MarkBusy("a")

	// Sleep well past KeepAlive while "busy" — the runner must survive.
	time.Sleep(100 * time.Millisecond)
	if ra.(*fakeRunner).stopped.Load() {
		t.Fatal("runner was evicted while marked busy — a request would have broken mid-flight")
	}
	if loaded := s.Loaded(); len(loaded) != 1 || loaded[0].ID != "a" {
		t.Fatalf("model should still be resident while busy, got %+v", loaded)
	}

	// Once idle, the clock restarts from MarkIdle (not from the original load),
	// so it takes another full KeepAlive window to actually evict.
	s.MarkIdle("a")
	time.Sleep(100 * time.Millisecond)
	if !ra.(*fakeRunner).stopped.Load() {
		t.Fatal("runner should be evicted a full KeepAlive window after MarkIdle")
	}
}

// TestMarkBusyPreventsLRUEviction proves the fix applies to capacity-pressure
// eviction too, not just the idle timer: a busy model must not be picked as the
// LRU victim when a new model needs room.
func TestMarkBusyPreventsLRUEviction(t *testing.T) {
	be := &fakeBackend{}
	s := New(bmap(be), specs("a", "b"), Options{MaxLoaded: 1}) // capacity 1
	ra, err := s.EnsureLoaded(context.Background(), "a")
	if err != nil {
		t.Fatal(err)
	}
	s.MarkBusy("a")

	if _, err := s.EnsureLoaded(context.Background(), "b"); err != nil {
		t.Fatal(err)
	}
	if ra.(*fakeRunner).stopped.Load() {
		t.Fatal("busy model 'a' must not be evicted to make room for 'b'")
	}
	// Over capacity temporarily, but nothing broken — both remain resident.
	ids := map[string]bool{}
	for _, l := range s.Loaded() {
		ids[l.ID] = true
	}
	if !ids["a"] || !ids["b"] {
		t.Fatalf("expected both a and b resident (capacity exceeded rather than breaking a's request), got %+v", ids)
	}
	s.MarkIdle("a")
}

func TestShutdownStopsAll(t *testing.T) {
	be := &fakeBackend{}
	s := New(bmap(be), specs("a", "b"), Options{MaxLoaded: 2})
	ra, _ := s.EnsureLoaded(context.Background(), "a")
	rb, _ := s.EnsureLoaded(context.Background(), "b")
	s.Shutdown(context.Background())
	if !ra.(*fakeRunner).stopped.Load() || !rb.(*fakeRunner).stopped.Load() {
		t.Fatal("Shutdown must stop every loaded runner")
	}
	if len(s.Loaded()) != 0 {
		t.Fatal("Shutdown must clear the loaded set")
	}
}
