package server_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/ankit373/mainspring/internal/auth"
	"github.com/ankit373/mainspring/internal/backend"
	"github.com/ankit373/mainspring/internal/metrics"
	"github.com/ankit373/mainspring/internal/scheduler"
	"github.com/ankit373/mainspring/internal/server"
)

// TestGuardrailFiresOnOversizedEmbeddings proves the fix: before counting the
// `input` field, the guardrail treated every embeddings request as 0 prompt
// tokens and could never reject it. A huge input must now be rejected.
func TestGuardrailFiresOnOversizedEmbeddings(t *testing.T) {
	eng := fakeEngine(t)
	sched := scheduler.New(
		map[string]backend.Backend{"fake": &engineBackend{baseURL: eng.URL}},
		[]backend.ModelSpec{{ID: "m1", Backend: "fake"}},
		scheduler.Options{MaxLoaded: 2},
	)
	rec, _ := metrics.New("")
	srv := server.New(sched, auth.New(nil), rec)
	srv.SetContextGuard(map[string]server.ContextPolicy{"m1": {Limit: 16, Enforce: true}})
	h := srv.Handler()

	big := strings.Repeat("x", 1000) // ~250 estimated tokens, way over the 16-token window
	w := postBody2(h, "/v1/embeddings", `{"model":"m1","input":"`+big+`"}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (embeddings input should trip the guardrail); body=%s", w.Code, w.Body.String())
	}
}

func TestGuardrailPassesSmallEmbeddings(t *testing.T) {
	eng := fakeEngine(t)
	sched := scheduler.New(
		map[string]backend.Backend{"fake": &engineBackend{baseURL: eng.URL}},
		[]backend.ModelSpec{{ID: "m1", Backend: "fake"}},
		scheduler.Options{MaxLoaded: 2},
	)
	rec, _ := metrics.New("")
	srv := server.New(sched, auth.New(nil), rec)
	srv.SetContextGuard(map[string]server.ContextPolicy{"m1": {Limit: 4096, Enforce: true}})
	h := srv.Handler()

	w := postBody2(h, "/v1/embeddings", `{"model":"m1","input":"hello world"}`)
	if w.Code != 200 {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
}
