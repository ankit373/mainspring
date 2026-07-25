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

// preciseServer wires a word-counting tokenizer runner with an enforcing,
// precise context guardrail limited to `limit` tokens.
func preciseServer(t *testing.T, limit int, enforce bool) http.Handler {
	t.Helper()
	eng := fakeEngine(t)
	sched := scheduler.New(
		map[string]backend.Backend{"tok": tokenizingBackend{baseURL: eng.URL}},
		[]backend.ModelSpec{{ID: "m1", Backend: "tok"}},
		scheduler.Options{MaxLoaded: 2},
	)
	rec, _ := metrics.New("")
	srv := server.New(sched, auth.New(nil), rec)
	srv.SetContextGuard(map[string]server.ContextPolicy{"m1": {Limit: limit, Enforce: enforce}})
	srv.SetPreciseContext(map[string]bool{"m1": true})
	return srv.Handler()
}

func TestPreciseGuardrailExactWhenResident(t *testing.T) {
	h := preciseServer(t, 8, true) // window 8 tokens

	// Load the model so its word-counting tokenizer is resident.
	if w := post(t, h, "/admin/models/m1/load"); w.Code != 200 {
		t.Fatalf("load status=%d", w.Code)
	}
	// "one two three four five six" = 6 words + 4 overhead = 10 exact tokens > 8.
	w := postBody(h, `{"model":"m1","messages":[{"role":"user","content":"one two three four five six"}]}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (exact over-context)", w.Code)
	}
	if w.Header().Get("X-Mainspring-Context-Method") != "exact" {
		t.Fatalf("method header = %q, want exact", w.Header().Get("X-Mainspring-Context-Method"))
	}
	// Exact reason has no "~" estimate marker and shows the precise count.
	if body := w.Body.String(); !strings.Contains(body, "prompt (10 tokens)") || strings.Contains(body, "~") {
		t.Fatalf("expected an exact reason with 10 tokens, got: %s", body)
	}
}

func TestPreciseFallsBackToEstimateWhenNotResident(t *testing.T) {
	h := preciseServer(t, 4, false) // warn mode, tiny window; model NOT loaded

	w := postBody(h, `{"model":"m1","messages":[{"role":"user","content":"some longish content here"}]}`)
	if w.Code != 200 {
		t.Fatalf("warn-mode status = %d, want 200", w.Code)
	}
	if w.Header().Get("X-Mainspring-Context-Method") != "estimated" {
		t.Fatalf("method header = %q, want estimated (model not resident)", w.Header().Get("X-Mainspring-Context-Method"))
	}
	if warn := w.Header().Get("X-Mainspring-Context-Warning"); !strings.Contains(warn, "estimated") {
		t.Fatalf("warning should be estimate-based: %q", warn)
	}
}
