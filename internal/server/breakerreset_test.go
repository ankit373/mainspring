package server_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ankit373/mainspring/internal/auth"
	"github.com/ankit373/mainspring/internal/backend"
	"github.com/ankit373/mainspring/internal/metrics"
	"github.com/ankit373/mainspring/internal/scheduler"
	"github.com/ankit373/mainspring/internal/server"
)

// TestBreakerResetRecoversImmediately proves the fix: a tripped breaker with a
// long cooldown normally keeps fast-failing every request; POST
// /admin/breaker/{id}/reset must let the next request through immediately.
func TestBreakerResetRecoversImmediately(t *testing.T) {
	// A backend that always fails, so one request trips the breaker (threshold 1).
	var hits atomic.Int64
	eng := flakyEngine(t, -1, &hits)
	sched := scheduler.New(
		map[string]backend.Backend{"fake": &engineBackend{baseURL: eng.URL}},
		[]backend.ModelSpec{{ID: "m1", Backend: "fake"}},
		scheduler.Options{MaxLoaded: 2},
	)
	rec, _ := metrics.New("")
	srv := server.New(sched, auth.New(nil), rec)
	srv.SetBreaker(1, time.Hour) // long cooldown — reset must bypass it
	h := srv.Handler()

	// First request fails at the upstream and trips the breaker.
	postBody(h, `{"model":"m1","messages":[]}`)

	// Second request should now fast-fail with circuit_open, cooldown not elapsed.
	w := postBody(h, `{"model":"m1","messages":[]}`)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 (breaker should be open); body=%s", w.Code, w.Body.String())
	}

	// Reset via the admin endpoint.
	rw := post(t, h, "/admin/breaker/m1/reset")
	if rw.Code != 200 {
		t.Fatalf("reset status = %d, want 200; body=%s", rw.Code, rw.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal(rw.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out["breaker"] != "closed" {
		t.Fatalf("breaker state = %v, want closed", out["breaker"])
	}

	// The next request must reach the upstream again — not fast-fail with the
	// breaker's own circuit_open error. The upstream (flakyEngine) always fails,
	// so it still returns a 503, but that must be the *upstream's* 503 body, not
	// the breaker short-circuit's.
	w2 := postBody(h, `{"model":"m1","messages":[]}`)
	if strings.Contains(w2.Body.String(), "circuit_open") {
		t.Fatalf("breaker should have let the request through after reset, not fast-failed: %s", w2.Body.String())
	}
	if !strings.Contains(w2.Body.String(), "busy") {
		t.Fatalf("expected the request to actually reach the (always-failing) upstream: %s", w2.Body.String())
	}
}

func TestBreakerResetUnknownModel(t *testing.T) {
	eng := fakeEngine(t)
	sched := scheduler.New(
		map[string]backend.Backend{"fake": &engineBackend{baseURL: eng.URL}},
		[]backend.ModelSpec{{ID: "m1", Backend: "fake"}},
		scheduler.Options{MaxLoaded: 2},
	)
	rec, _ := metrics.New("")
	h := server.New(sched, auth.New(nil), rec).Handler()

	if w := post(t, h, "/admin/breaker/nope/reset"); w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", w.Code)
	}
}
