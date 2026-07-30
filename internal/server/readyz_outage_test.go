package server_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ankit373/mainspring/internal/auth"
	"github.com/ankit373/mainspring/internal/backend"
	"github.com/ankit373/mainspring/internal/metrics"
	"github.com/ankit373/mainspring/internal/scheduler"
	"github.com/ankit373/mainspring/internal/server"
)

func getReadyz(h http.Handler) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	return w
}

// TestReadyzReflectsTotalOutage proves the fix: when every configured model's
// breaker is Open, /readyz must report 503 rather than 200 — a load balancer
// should stop routing to a pod that cannot serve a single request.
func TestReadyzReflectsTotalOutage(t *testing.T) {
	var hits atomic.Int64
	eng := flakyEngine(t, -1, &hits) // always fails
	sched := scheduler.New(
		map[string]backend.Backend{"fake": &engineBackend{baseURL: eng.URL}},
		[]backend.ModelSpec{{ID: "m1", Backend: "fake"}},
		scheduler.Options{MaxLoaded: 2},
	)
	rec, _ := metrics.New("")
	srv := server.New(sched, auth.New(nil), rec)
	srv.SetBreaker(1, time.Hour) // trips after 1 failure, long cooldown
	h := srv.Handler()

	if w := getReadyz(h); w.Code != http.StatusOK {
		t.Fatalf("readyz before any failure = %d, want 200", w.Code)
	}

	// Trip the only model's breaker.
	postBody(h, `{"model":"m1","messages":[]}`)

	w := getReadyz(h)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("readyz with all breakers open = %d, want 503; body=%s", w.Code, w.Body.String())
	}
}

// A single healthy model must keep readyz at 200 even if others are down.
func TestReadyzOKWhenOneModelHealthy(t *testing.T) {
	var hits atomic.Int64
	dead := flakyEngine(t, -1, &hits)
	healthy := fakeEngine(t)
	sched := scheduler.New(
		map[string]backend.Backend{
			"dead":    &engineBackend{baseURL: dead.URL},
			"healthy": &engineBackend{baseURL: healthy.URL},
		},
		[]backend.ModelSpec{
			{ID: "m1", Backend: "dead"},
			{ID: "m2", Backend: "healthy"},
		},
		scheduler.Options{MaxLoaded: 2},
	)
	rec, _ := metrics.New("")
	srv := server.New(sched, auth.New(nil), rec)
	srv.SetBreaker(1, time.Hour)
	h := srv.Handler()

	postBody(h, `{"model":"m1","messages":[]}`) // trip m1 only

	if w := getReadyz(h); w.Code != http.StatusOK {
		t.Fatalf("readyz = %d, want 200 (m2 is still healthy)", w.Code)
	}
}

// Breaker disabled entirely -> readyz is unaffected by this check.
func TestReadyzUnaffectedWhenBreakerDisabled(t *testing.T) {
	eng := fakeEngine(t)
	sched := scheduler.New(
		map[string]backend.Backend{"fake": &engineBackend{baseURL: eng.URL}},
		[]backend.ModelSpec{{ID: "m1", Backend: "fake"}},
		scheduler.Options{MaxLoaded: 2},
	)
	rec, _ := metrics.New("")
	h := server.New(sched, auth.New(nil), rec).Handler() // breaker never enabled

	if w := getReadyz(h); w.Code != http.StatusOK {
		t.Fatalf("readyz = %d, want 200 (breaker disabled)", w.Code)
	}
}

// Draining still takes priority over the total-outage check.
func TestReadyzDrainingTakesPriorityOverOutage(t *testing.T) {
	var hits atomic.Int64
	eng := flakyEngine(t, -1, &hits)
	sched := scheduler.New(
		map[string]backend.Backend{"fake": &engineBackend{baseURL: eng.URL}},
		[]backend.ModelSpec{{ID: "m1", Backend: "fake"}},
		scheduler.Options{MaxLoaded: 2},
	)
	rec, _ := metrics.New("")
	srv := server.New(sched, auth.New(nil), rec)
	srv.SetBreaker(1, time.Hour)
	h := srv.Handler()

	srv.SetDraining(true)
	w := getReadyz(h)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("readyz while draining = %d, want 503", w.Code)
	}
	var body struct {
		Status string `json:"status"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	if body.Status != "draining" {
		t.Fatalf("status = %q, want draining (not degraded) to take priority", body.Status)
	}
}
