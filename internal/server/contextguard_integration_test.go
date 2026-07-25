package server_test

import (
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/ankit373/mainspring/internal/auth"
	"github.com/ankit373/mainspring/internal/backend"
	"github.com/ankit373/mainspring/internal/metrics"
	"github.com/ankit373/mainspring/internal/scheduler"
	"github.com/ankit373/mainspring/internal/server"
)

func guardedServer(t *testing.T, baseURL string, enforce bool) http.Handler {
	t.Helper()
	sched := scheduler.New(
		map[string]backend.Backend{"fake": &engineBackend{baseURL: baseURL}},
		[]backend.ModelSpec{{ID: "m1", Backend: "fake"}},
		scheduler.Options{MaxLoaded: 2},
	)
	rec, _ := metrics.New("")
	srv := server.New(sched, auth.New(nil), rec)
	srv.SetContextGuard(map[string]server.ContextPolicy{
		"m1": {Limit: 4096, Enforce: enforce},
	})
	return srv.Handler()
}

func TestContextGuardEnforceRejects(t *testing.T) {
	var hits atomic.Int64
	eng := countingEngine(t, &hits)
	h := guardedServer(t, eng.URL, true)

	w := postBody(h, `{"model":"m1","max_tokens":99999,"messages":[]}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
	if !strings.Contains(w.Body.String(), "context_length_exceeded") {
		t.Fatalf("body missing context_length_exceeded code: %s", w.Body.String())
	}
	if hits.Load() != 0 {
		t.Fatal("over-context request must be rejected before hitting the backend")
	}
}

func TestContextGuardWarnPasses(t *testing.T) {
	var hits atomic.Int64
	eng := countingEngine(t, &hits)
	h := guardedServer(t, eng.URL, false) // enforce=false → warn only

	w := postBody(h, `{"model":"m1","max_tokens":99999,"messages":[]}`)
	if w.Code != 200 {
		t.Fatalf("warn mode status = %d, want 200", w.Code)
	}
	if w.Header().Get("X-Mainspring-Context-Warning") == "" {
		t.Fatal("warn mode should set X-Mainspring-Context-Warning header")
	}
	if hits.Load() != 1 {
		t.Fatal("warn mode should still reach the backend")
	}
}

func TestContextGuardWithinLimitNoWarning(t *testing.T) {
	var hits atomic.Int64
	eng := countingEngine(t, &hits)
	h := guardedServer(t, eng.URL, true)

	w := postBody(h, `{"model":"m1","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`)
	if w.Code != 200 {
		t.Fatalf("in-limit status = %d, want 200", w.Code)
	}
	if w.Header().Get("X-Mainspring-Context-Warning") != "" {
		t.Fatal("in-limit request should carry no context warning")
	}
}
