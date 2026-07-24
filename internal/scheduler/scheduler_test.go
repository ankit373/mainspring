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
	stopped atomic.Bool
}

func (r *fakeRunner) BaseURL() string                        { return "http://fake/" + r.id }
func (r *fakeRunner) Health(context.Context) backend.Status  { return backend.StatusReady }
func (r *fakeRunner) MemoryBytes() int64                     { return 1 }
func (r *fakeRunner) Stop(context.Context) error             { r.stopped.Store(true); return nil }
func (r *fakeRunner) Capabilities(context.Context) (backend.Capabilities, error) {
	return backend.Capabilities{Backend: "fake", Model: r.id, GPUOffload: true}, nil
}

// fakeBackend counts Start calls per model.
type fakeBackend struct {
	starts sync.Map // id -> *atomic.Int64
	delay  time.Duration
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
	return &fakeRunner{id: spec.ID}, nil
}
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
		out[i] = backend.ModelSpec{ID: id}
	}
	return out
}

func TestEnsureLoadedSingleFlight(t *testing.T) {
	be := &fakeBackend{delay: 20 * time.Millisecond}
	s := New(be, specs("a"), 0, 4)

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
	s := New(be, specs("a", "b"), 0, 1) // capacity 1 → loading b evicts a

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

func TestUnknownModel(t *testing.T) {
	s := New(&fakeBackend{}, specs("a"), 0, 1)
	if _, err := s.EnsureLoaded(context.Background(), "nope"); err == nil {
		t.Fatal("expected error for unknown model")
	}
}

func TestShutdownStopsAll(t *testing.T) {
	be := &fakeBackend{}
	s := New(be, specs("a", "b"), 0, 2)
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
