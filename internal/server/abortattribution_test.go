package server_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ankit373/mainspring/internal/auth"
	"github.com/ankit373/mainspring/internal/backend"
	"github.com/ankit373/mainspring/internal/breaker"
	"github.com/ankit373/mainspring/internal/metrics"
	"github.com/ankit373/mainspring/internal/scheduler"
	"github.com/ankit373/mainspring/internal/server"
)

// slowLoadBackend blocks in Start until released or the context is cancelled, so
// a test can disconnect a caller *while its model is loading*. `started` closes
// once Start is actually executing, so the test synchronises on real state
// instead of a sleep.
type slowLoadBackend struct {
	release <-chan struct{}
	started chan struct{}
	once    sync.Once
	baseURL string
}

func (b *slowLoadBackend) Name() string { return "slowload" }
func (b *slowLoadBackend) Detect(context.Context) backend.Availability {
	return backend.Availability{Name: "slowload", Present: true}
}

func (b *slowLoadBackend) Start(ctx context.Context, spec backend.ModelSpec) (backend.Runner, error) {
	b.once.Do(func() { close(b.started) })
	select {
	case <-b.release:
		return &engineRunner{base: b.baseURL, id: spec.ID}, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// abortServer wires a server whose single model takes as long to load as the
// test wants, with a one-failure breaker so any misattribution is immediately
// visible in /metrics.
func abortServer(t *testing.T, release <-chan struct{}) (http.Handler, *slowLoadBackend) {
	t.Helper()
	eng := fakeEngine(t)
	be := &slowLoadBackend{release: release, started: make(chan struct{}), baseURL: eng.URL}
	sched := scheduler.New(
		map[string]backend.Backend{"slowload": be},
		[]backend.ModelSpec{{ID: "m1", Backend: "slowload"}},
		scheduler.Options{MaxLoaded: 2},
	)
	rec, _ := metrics.New("")
	srv := server.New(sched, auth.New(nil), rec)
	srv.SetBreaker(1, time.Hour) // a single failure would open it
	return srv.Handler(), be
}

// abortDuring fires a request at path, waits for it to reach the server, then
// cancels it — returning what (if anything) was written back.
func abortDuring(t *testing.T, h http.Handler, be *slowLoadBackend, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body)).WithContext(ctx)
	w := httptest.NewRecorder()
	done := make(chan struct{})
	go func() { defer close(done); h.ServeHTTP(w, req) }()

	// Wait until the handler is genuinely parked inside EnsureLoaded before
	// disconnecting, so the test exercises the mid-load path every run.
	select {
	case <-be.started:
	case <-time.After(5 * time.Second):
		t.Fatal("load never started")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("handler did not return after the client disconnected")
	}
	return w
}

// TestClientAbortDuringLoadIsNotALoadFailure — acquireRunner reported any
// EnsureLoaded error to the breaker as a backend failure, including one caused
// by the caller's own disconnect. A client that gives up while a large model is
// loading is not evidence that the model cannot load.
func TestClientAbortDuringLoadIsNotALoadFailure(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	h, be := abortServer(t, release)

	abortDuring(t, h, be, "/v1/chat/completions", `{"model":"m1","messages":[]}`)

	if out := scrape(t, h); strings.Contains(out, `mainspring_breaker_open{model="m1"} 1`) {
		t.Fatalf("a client that disconnected mid-load must not open the circuit:\n%s", out)
	}
}

// TestClientAbortDuringLoadWritesNothing — the connection is already gone, so a
// 503 backend_unavailable is both wrong and unreadable.
func TestClientAbortDuringLoadWritesNothing(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	h, be := abortServer(t, release)

	w := abortDuring(t, h, be, "/v1/chat/completions", `{"model":"m1","messages":[]}`)
	if w.Body.Len() != 0 {
		t.Fatalf("nothing should be written to an abandoned connection, got %d: %s", w.Code, w.Body.String())
	}
}

// TestMessagesClientAbortIsNotA502 — messagesJSON checked only for a deadline
// before falling through to writeError(502), so a cancelled non-streaming
// /v1/messages request had a 502 written to a dead connection and recorded as a
// backend failure.
func TestMessagesClientAbortIsNotA502(t *testing.T) {
	// An engine that never answers, so the caller can cancel mid-request.
	stall := make(chan struct{})
	defer close(stall)
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-stall:
		case <-r.Context().Done():
		}
	})
	eng := httptest.NewServer(mux)
	t.Cleanup(eng.Close)

	h, grp, _ := breakerMessagesServer(t, eng.URL)

	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodPost, "/v1/messages",
		strings.NewReader(`{"model":"m1","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`)).WithContext(ctx)
	w := httptest.NewRecorder()
	done := make(chan struct{})
	go func() { defer close(done); h.ServeHTTP(w, req) }()
	time.Sleep(50 * time.Millisecond) // let the request reach the stalled engine
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("handler did not return after the client disconnected")
	}

	if w.Body.Len() != 0 {
		t.Fatalf("nothing should be written to an abandoned connection, got %d: %s", w.Code, w.Body.String())
	}
	if got := grp.State("m1"); got == breaker.Open {
		t.Fatal("a client abort must not open the circuit on /v1/messages")
	}
}

// TestLoadFailureIsStillABackendFailure guards the exclusion: only a caller's own
// cancellation is exempt. A model that genuinely cannot load must still trip.
func TestLoadFailureIsStillABackendFailure(t *testing.T) {
	sched := scheduler.New(
		map[string]backend.Backend{"broken": &failingLoadBackend{}},
		[]backend.ModelSpec{{ID: "m1", Backend: "broken"}},
		scheduler.Options{MaxLoaded: 2},
	)
	rec, _ := metrics.New("")
	srv := server.New(sched, auth.New(nil), rec)
	srv.SetBreaker(1, time.Hour)
	h := srv.Handler()

	if w := postBody(h, `{"model":"m1","messages":[]}`); w.Code < 500 {
		t.Fatalf("a real load failure should be a 5xx, got %d: %s", w.Code, w.Body.String())
	}
	if out := scrape(t, h); !strings.Contains(out, `mainspring_breaker_open{model="m1"} 1`) {
		t.Fatalf("a real load failure must still open the circuit:\n%s", out)
	}
}

// failingLoadBackend always fails to start — a genuinely broken model.
type failingLoadBackend struct{}

func (b *failingLoadBackend) Name() string { return "broken" }
func (b *failingLoadBackend) Detect(context.Context) backend.Availability {
	return backend.Availability{Name: "broken", Present: true}
}

func (b *failingLoadBackend) Start(context.Context, backend.ModelSpec) (backend.Runner, error) {
	return nil, errors.New("engine refused to start")
}
