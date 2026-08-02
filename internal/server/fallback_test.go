package server_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ankit373/mainspring/internal/auth"
	"github.com/ankit373/mainspring/internal/backend"
	"github.com/ankit373/mainspring/internal/metrics"
	"github.com/ankit373/mainspring/internal/scheduler"
	"github.com/ankit373/mainspring/internal/server"
)

// unloadableBackend fails to Start (simulates a model that cannot load), forcing
// the router onto a fallback model.
type unloadableBackend struct{}

func (unloadableBackend) Name() string { return "dead" }
func (unloadableBackend) Detect(context.Context) backend.Availability {
	return backend.Availability{Name: "dead", Present: true}
}
func (unloadableBackend) Start(context.Context, backend.ModelSpec) (backend.Runner, error) {
	return nil, io.ErrUnexpectedEOF // any load error
}

// reasonedBackend fails to load with a message worth reading — what a real
// backend produces (which models the daemon actually has, which binary is
// missing) and what acquireRunner used to throw away.
type reasonedBackend struct{ reason string }

func (reasonedBackend) Name() string { return "reasoned" }
func (reasonedBackend) Detect(context.Context) backend.Availability {
	return backend.Availability{Name: "reasoned", Present: true}
}
func (b reasonedBackend) Start(context.Context, backend.ModelSpec) (backend.Runner, error) {
	return nil, errors.New(b.reason)
}

// echoEngine identifies which model the upstream body carried, so a test can
// confirm the fallback model actually served.
func echoEngine(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body) // echo the (model-rewritten) request body back
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestModelFallbackServesWhenPrimaryUnloadable(t *testing.T) {
	eng := echoEngine(t)
	sched := scheduler.New(
		map[string]backend.Backend{
			"dead": unloadableBackend{},
			"live": &engineBackend{baseURL: eng.URL},
		},
		[]backend.ModelSpec{
			{ID: "primary", Backend: "dead"},
			{ID: "backup", Backend: "live"},
		},
		scheduler.Options{MaxLoaded: 2},
	)
	rec, _ := metrics.New("")
	srv := server.New(sched, auth.New(nil), rec)
	srv.SetModelFallbacks(map[string][]string{"primary": {"backup"}})
	h := srv.Handler()

	w := postBody(h, `{"model":"primary","messages":[]}`)
	if w.Code != 200 {
		t.Fatalf("status = %d, want 200 via fallback; body=%s", w.Code, w.Body.String())
	}
	if got := w.Header().Get("X-Mainspring-Served-Model"); got != "backup" {
		t.Fatalf("X-Mainspring-Served-Model = %q, want backup", got)
	}
	// The upstream body must have had its model rewritten to the fallback.
	if !strings.Contains(w.Body.String(), `"model":"backup"`) {
		t.Fatalf("upstream did not receive fallback model id: %s", w.Body.String())
	}
}

// deadServer wires two models that both fail to load, with primary→backup
// fallback, so every candidate is exhausted.
func deadServer(t *testing.T) *server.Server {
	t.Helper()
	sched := scheduler.New(
		map[string]backend.Backend{"dead": unloadableBackend{}},
		[]backend.ModelSpec{
			{ID: "primary", Backend: "dead"},
			{ID: "backup", Backend: "dead"},
		},
		scheduler.Options{MaxLoaded: 2},
	)
	rec, _ := metrics.New("")
	srv := server.New(sched, auth.New(nil), rec)
	srv.SetModelFallbacks(map[string][]string{"primary": {"backup"}})
	return srv
}

// TestModelFallbackExhaustedReportsLoadFailure pins the #166 fix: with no
// breaker configured there is no circuit to be open, so a request that exhausted
// every candidate on load errors must not be handed the stable code
// `circuit_open` — a client branching on it would wait out a cooldown that does
// not exist.
func TestModelFallbackExhaustedReportsLoadFailure(t *testing.T) {
	h := deadServer(t).Handler()

	w := postBody(h, `{"model":"primary","messages":[]}`)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 when all candidates unavailable", w.Code)
	}
	if body := w.Body.String(); !strings.Contains(body, "backend_unavailable") || strings.Contains(body, "circuit_open") {
		t.Fatalf("load failure with no breaker must report backend_unavailable: %s", body)
	}
}

// TestModelFallbackExhaustedReportsCircuitOpen is the other half: once a breaker
// really refuses a candidate, `circuit_open` is the truth and must be reported.
func TestModelFallbackExhaustedReportsCircuitOpen(t *testing.T) {
	srv := deadServer(t)
	srv.SetBreaker(1, time.Minute) // one load failure trips it
	h := srv.Handler()

	// First request: nothing is open yet, both candidates fail to load and trip
	// their breakers.
	if w := postBody(h, `{"model":"primary","messages":[]}`); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("first status = %d, want 503", w.Code)
	}
	// Second request: the breaker now refuses before any load is attempted.
	w := postBody(h, `{"model":"primary","messages":[]}`)
	if !strings.Contains(w.Body.String(), "circuit_open") {
		t.Fatalf("an open circuit must report circuit_open: %s", w.Body.String())
	}
}

func TestNoFallbackHeaderWhenPrimaryServes(t *testing.T) {
	eng := echoEngine(t)
	sched := scheduler.New(
		map[string]backend.Backend{"live": &engineBackend{baseURL: eng.URL}},
		[]backend.ModelSpec{
			{ID: "primary", Backend: "live"},
			{ID: "backup", Backend: "live"},
		},
		scheduler.Options{MaxLoaded: 2},
	)
	rec, _ := metrics.New("")
	srv := server.New(sched, auth.New(nil), rec)
	srv.SetModelFallbacks(map[string][]string{"primary": {"backup"}})
	h := srv.Handler()

	w := postBody(h, `{"model":"primary","messages":[]}`)
	if w.Code != 200 {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if w.Header().Get("X-Mainspring-Served-Model") != "" {
		t.Fatal("primary served the request; no served-model header expected")
	}
}

// A load failure's reason used to be discarded in acquireRunner, leaving the
// operator with a 503 that said "no available backend" and nothing about why —
// while the backend had built a precise explanation (which model the daemon
// actually has, which binary is missing) that nobody ever read.
func TestLoadFailureReasonIsLogged(t *testing.T) {
	var logs bytes.Buffer
	prevOut, prevFlags := log.Writer(), log.Flags()
	log.SetOutput(&logs)
	log.SetFlags(0)
	t.Cleanup(func() { log.SetOutput(prevOut); log.SetFlags(prevFlags) })

	const reason = "daemon has only some-other-model"
	sched := scheduler.New(
		map[string]backend.Backend{"reasoned": reasonedBackend{reason: reason}},
		[]backend.ModelSpec{{ID: "m1", Backend: "reasoned"}},
		scheduler.Options{MaxLoaded: 1},
	)
	rec, _ := metrics.New("")
	h := server.New(sched, auth.New(nil), rec).Handler()

	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"m1","messages":[{"role":"user","content":"hi"}]}`)))

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	got := logs.String()
	if !strings.Contains(got, "LOAD-FAILED") || !strings.Contains(got, "m1") {
		t.Errorf("no load-failure log line naming the model:\n%s", got)
	}
	if !strings.Contains(got, reason) {
		t.Errorf("the log line does not carry the backend's reason:\n%s", got)
	}
	// The reason stays server-side: a caller cannot act on another host's
	// inventory, and it may name paths that are not theirs to see.
	if strings.Contains(w.Body.String(), reason) {
		t.Errorf("the backend's reason leaked into the client response: %s", w.Body.String())
	}
}
