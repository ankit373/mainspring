package scheduler

import (
	"context"
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
