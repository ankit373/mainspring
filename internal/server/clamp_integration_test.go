package server_test

import (
	"strings"
	"testing"

	"github.com/ankit373/mainspring/internal/auth"
	"github.com/ankit373/mainspring/internal/backend"
	"github.com/ankit373/mainspring/internal/metrics"
	"github.com/ankit373/mainspring/internal/scheduler"
	"github.com/ankit373/mainspring/internal/server"
)

func TestClampProceedsWithReducedMaxTokens(t *testing.T) {
	eng := echoEngine(t) // echoes the upstream body back
	sched := scheduler.New(
		map[string]backend.Backend{"fake": &engineBackend{baseURL: eng.URL}},
		[]backend.ModelSpec{{ID: "m1", Backend: "fake"}},
		scheduler.Options{MaxLoaded: 2},
	)
	rec, _ := metrics.New("")
	srv := server.New(sched, auth.New(nil), rec)
	srv.SetClampLimits(map[string]int{"m1": 4096})
	h := srv.Handler()

	w := postBody(h, `{"model":"m1","max_tokens":99999,"messages":[{"role":"user","content":"hi"}]}`)
	if w.Code != 200 {
		t.Fatalf("status = %d, want 200 (clamped, not rejected); body=%s", w.Code, w.Body.String())
	}
	if w.Header().Get("X-Mainspring-Clamped") == "" {
		t.Fatal("expected X-Mainspring-Clamped header")
	}
	// The upstream (echoed) body must carry the reduced max_tokens, not 99999.
	if strings.Contains(w.Body.String(), "99999") {
		t.Fatalf("upstream still received the original max_tokens: %s", w.Body.String())
	}
}

func TestClampNoHeaderWhenFits(t *testing.T) {
	eng := echoEngine(t)
	sched := scheduler.New(
		map[string]backend.Backend{"fake": &engineBackend{baseURL: eng.URL}},
		[]backend.ModelSpec{{ID: "m1", Backend: "fake"}},
		scheduler.Options{MaxLoaded: 2},
	)
	rec, _ := metrics.New("")
	srv := server.New(sched, auth.New(nil), rec)
	srv.SetClampLimits(map[string]int{"m1": 4096})
	h := srv.Handler()

	w := postBody(h, `{"model":"m1","max_tokens":128,"messages":[{"role":"user","content":"hi"}]}`)
	if w.Code != 200 {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if w.Header().Get("X-Mainspring-Clamped") != "" {
		t.Fatal("a request that fits must not be clamped")
	}
}
