package server_test

import (
	"bufio"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ankit373/mainspring/internal/auth"
	"github.com/ankit373/mainspring/internal/backend"
	"github.com/ankit373/mainspring/internal/metrics"
	"github.com/ankit373/mainspring/internal/scheduler"
	"github.com/ankit373/mainspring/internal/server"
)

// The usage ledger is the only place a recorded metrics.Event is observable in
// full, so these tests read it back rather than scraping /metrics.

type acctEvent struct {
	Model        string  `json:"model"`
	Status       int     `json:"status"`
	ErrorCode    string  `json:"error_code"`
	Cached       bool    `json:"cached"`
	Coalesced    bool    `json:"coalesced"`
	DurationMs   float64 `json:"duration_ms"`
	PromptTokens int64   `json:"prompt_tokens"`
	TokensEst    int64   `json:"tokens_est"`
	Exact        bool    `json:"exact_usage"`
}

// acctServer wires a handler over sched with a JSONL usage ledger, returning the
// handler and a reader for the events recorded so far.
func acctServer(t *testing.T, sched *scheduler.Scheduler, a *auth.Authenticator, configure func(*server.Server)) (http.Handler, func() []acctEvent) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "usage.jsonl")
	rec, err := metrics.New(path)
	if err != nil {
		t.Fatalf("metrics ledger: %v", err)
	}
	t.Cleanup(func() { _ = rec.Close() })
	srv := server.New(sched, a, rec)
	if configure != nil {
		configure(srv)
	}
	return srv.Handler(), func() []acctEvent {
		f, err := os.Open(path)
		if err != nil {
			t.Fatalf("open ledger: %v", err)
		}
		defer f.Close()
		var out []acctEvent
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			line := strings.TrimSpace(sc.Text())
			if line == "" {
				continue
			}
			var ev acctEvent
			if err := json.Unmarshal([]byte(line), &ev); err != nil {
				t.Fatalf("decode ledger line %q: %v", line, err)
			}
			out = append(out, ev)
		}
		return out
	}
}

// fakeSched is the one-model scheduler the accounting tests share.
func fakeSched(baseURL string) *scheduler.Scheduler {
	return scheduler.New(
		map[string]backend.Backend{"fake": &engineBackend{baseURL: baseURL}},
		[]backend.ModelSpec{{ID: "m1", Backend: "fake"}},
		scheduler.Options{MaxLoaded: 2},
	)
}

// lastEvent returns the most recent recorded event.
func lastEvent(t *testing.T, events []acctEvent) acctEvent {
	t.Helper()
	if len(events) == 0 {
		t.Fatal("no events recorded")
	}
	return events[len(events)-1]
}

// ── #164 token accounting ────────────────────────────────────────────────────

// TestEmbeddingsBillPromptTokens proves the real /v1/embeddings shape — a usage
// object carrying prompt_tokens and no completion_tokens — is billed exactly
// instead of reported as zero.
func TestEmbeddingsBillPromptTokens(t *testing.T) {
	eng := fakeEngine(t)
	h, events := acctServer(t, fakeSched(eng.URL), auth.New(nil), nil)

	if w := postBody2(h, "/v1/embeddings", `{"model":"m1","input":"hello world"}`); w.Code != 200 {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	ev := lastEvent(t, events())
	if !ev.Exact {
		t.Fatal("an embeddings response reporting prompt_tokens must be billed as exact usage")
	}
	if ev.PromptTokens != 2 {
		t.Fatalf("recorded prompt_tokens=%d, want 2 (the count the engine reported)", ev.PromptTokens)
	}
}

// preciseClampServer wires the word-counting tokenizer with both max_tokens
// clamping and an enforcing precise guardrail on the same window.
func preciseClampServer(t *testing.T, window int) http.Handler {
	t.Helper()
	eng := fakeEngine(t)
	sched := scheduler.New(
		map[string]backend.Backend{"tok": tokenizingBackend{baseURL: eng.URL}},
		[]backend.ModelSpec{{ID: "m1", Backend: "tok"}},
		scheduler.Options{MaxLoaded: 2},
	)
	rec, _ := metrics.New("")
	srv := server.New(sched, auth.New(nil), rec)
	srv.SetClampLimits(map[string]int{"m1": window})
	srv.SetContextGuard(map[string]server.ContextPolicy{"m1": {Limit: window, Enforce: true}})
	srv.SetPreciseContext(map[string]bool{"m1": true})
	return srv.Handler()
}

// TestClampAndGuardrailUseTheSamePromptCount proves the #164 disagreement is
// gone. The prompt is 40 one-character words: the char heuristic sees ~24 tokens
// and the word tokenizer sees 44. Clamping against the heuristic leaves
// 100-24=76 of output room, which the guardrail then re-counts as 44+76=120 and
// rejects — a request the clamp had just shrunk to fit. Counting once gives
// 100-44=56, and 44+56 fits exactly.
func TestClampAndGuardrailUseTheSamePromptCount(t *testing.T) {
	h := preciseClampServer(t, 100)
	if w := post(t, h, "/admin/models/m1/load"); w.Code != 200 {
		t.Fatalf("load status=%d", w.Code)
	}
	prompt := strings.TrimSpace(strings.Repeat("a ", 40)) // 40 words, 79 chars

	w := postBody(h, `{"model":"m1","max_tokens":1000,"messages":[{"role":"user","content":"`+prompt+`"}]}`)
	if w.Code != 200 {
		t.Fatalf("status = %d, want 200 — a clamped request must not then be rejected; body=%s", w.Code, w.Body.String())
	}
	if got := w.Header().Get("X-Mainspring-Clamped"); got != "1000->56" {
		t.Fatalf("X-Mainspring-Clamped = %q, want 1000->56 (clamped against the exact count)", got)
	}
}

// ── #165 rejections, cache-hit duration ──────────────────────────────────────

// TestGuardrailRejectionIsRecorded covers the 400 path, which lives in the
// admission step shared by both dialects.
func TestGuardrailRejectionIsRecorded(t *testing.T) {
	eng := fakeEngine(t)
	h, events := acctServer(t, fakeSched(eng.URL), auth.New(nil), func(srv *server.Server) {
		srv.SetContextGuard(map[string]server.ContextPolicy{"m1": {Limit: 32, Enforce: true}})
	})

	if w := postBody(h, `{"model":"m1","max_tokens":9000,"messages":[]}`); w.Code != http.StatusBadRequest {
		t.Fatalf("status=%d, want 400", w.Code)
	}
	ev := lastEvent(t, events())
	if ev.Status != http.StatusBadRequest || ev.ErrorCode != "context_length_exceeded" || ev.Model != "m1" {
		t.Fatalf("recorded %+v, want a 400 context_length_exceeded for m1", ev)
	}
}

// TestTokenBudgetRejectionIsRecorded covers the 429 path.
func TestTokenBudgetRejectionIsRecorded(t *testing.T) {
	eng := usageEngine(t)
	a := auth.NewTenants([]auth.Tenant{{Name: "t", Key: "k", TokenBudget: 1, WindowSec: 60}})
	h, events := acctServer(t, fakeSched(eng.URL), a, nil)

	// First request is admitted and spends the budget (12+4 tokens > 1).
	if w := postKeyed(h, "k", `{"model":"m1","messages":[]}`); w.Code != 200 {
		t.Fatalf("first status=%d body=%s", w.Code, w.Body.String())
	}
	w := postKeyed(h, "k", `{"model":"m1","messages":[]}`)
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("second status=%d, want 429", w.Code)
	}
	ev := lastEvent(t, events())
	if ev.Status != http.StatusTooManyRequests || ev.ErrorCode != "token_budget_exceeded" {
		t.Fatalf("recorded %+v, want a 429 token_budget_exceeded", ev)
	}
}

// TestBusyRejectionIsRecorded covers the 503 backpressure path: one slot, no
// queue, and a second request while the first is still upstream.
func TestBusyRejectionIsRecorded(t *testing.T) {
	eng := stallingEngine(t, 300*time.Millisecond)
	h, events := acctServer(t, fakeSched(eng.URL), auth.New(nil), func(srv *server.Server) {
		srv.SetConcurrency(1, 0)
	})

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		postBody(h, `{"model":"m1","messages":[{"role":"user","content":"slow"}]}`)
	}()
	// Give the first request time to occupy the only slot.
	var busy *httptest.ResponseRecorder
	for i := 0; i < 100; i++ {
		time.Sleep(5 * time.Millisecond)
		if w := postBody(h, `{"model":"m1","messages":[]}`); w.Code == http.StatusServiceUnavailable {
			busy = w
			break
		}
	}
	wg.Wait()
	if busy == nil {
		t.Fatal("never observed the 503 backpressure rejection")
	}
	for _, ev := range events() {
		if ev.Status == http.StatusServiceUnavailable && ev.ErrorCode == "server_busy" {
			return
		}
	}
	t.Fatalf("no server_busy rejection recorded: %+v", events())
}

// TestBackendUnavailableRejectionIsRecorded covers the exhausted-candidates 503.
func TestBackendUnavailableRejectionIsRecorded(t *testing.T) {
	sched := scheduler.New(
		map[string]backend.Backend{"dead": unloadableBackend{}},
		[]backend.ModelSpec{{ID: "m1", Backend: "dead"}},
		scheduler.Options{MaxLoaded: 2},
	)
	h, events := acctServer(t, sched, auth.New(nil), nil)

	if w := postBody(h, `{"model":"m1","messages":[]}`); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d, want 503", w.Code)
	}
	ev := lastEvent(t, events())
	if ev.Status != http.StatusServiceUnavailable || ev.ErrorCode != "backend_unavailable" {
		t.Fatalf("recorded %+v, want a 503 backend_unavailable", ev)
	}
}

// TestCacheHitRecordsRealDuration proves a cache hit is timed rather than
// reported as 0ms — a zero in every hit drags the latency mean and percentiles
// below anything the server ever actually did.
func TestCacheHitRecordsRealDuration(t *testing.T) {
	var hits atomic.Int64
	eng := countingEngine(t, &hits)
	h, events := acctServer(t, fakeSched(eng.URL), auth.New(nil), func(srv *server.Server) {
		srv.SetCache(time.Minute, 16)
	})

	const body = `{"model":"m1","temperature":0,"messages":[{"role":"user","content":"hi"}]}`
	postBody(h, body)
	if w := postBody(h, body); w.Header().Get("X-Mainspring-Cache") != "hit" {
		t.Fatalf("second request was not a cache hit: %v", w.Header())
	}
	ev := lastEvent(t, events())
	if !ev.Cached {
		t.Fatalf("last event is not the cache hit: %+v", ev)
	}
	if ev.DurationMs <= 0 {
		t.Fatalf("cache hit recorded duration_ms=%v, want the real (small) latency", ev.DurationMs)
	}
}

// TestCoalescedFollowerRecordsRealDuration proves the same for a follower that
// waited on an in-flight leader — for it the wait *is* the latency.
func TestCoalescedFollowerRecordsRealDuration(t *testing.T) {
	var hits atomic.Int64
	eng := slowEngine(t, &hits) // ~120ms per upstream call
	h, events := acctServer(t, fakeSched(eng.URL), auth.New(nil), func(srv *server.Server) {
		srv.SetCoalescing(true)
	})

	const body = `{"model":"m1","temperature":0,"messages":[{"role":"user","content":"same"}]}`
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			postBody(h, body)
		}()
	}
	wg.Wait()

	var followers int
	for _, ev := range events() {
		if !ev.Coalesced {
			continue
		}
		followers++
		if ev.DurationMs <= 0 {
			t.Fatalf("coalesced follower recorded duration_ms=%v, want the time it actually waited", ev.DurationMs)
		}
	}
	if followers == 0 {
		t.Fatalf("no coalesced followers recorded: %+v", events())
	}
}

// TestQualityReportsInflightWithGatingDisabled proves the default configuration
// (no concurrency gate) reports real occupancy instead of a permanent zero.
func TestQualityReportsInflightWithGatingDisabled(t *testing.T) {
	eng := stallingEngine(t, 300*time.Millisecond)
	h, _ := acctServer(t, fakeSched(eng.URL), auth.New(nil), nil) // gating off (default)

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		postBody(h, `{"model":"m1","messages":[{"role":"user","content":"slow"}]}`)
	}()

	var sawInflight bool
	for i := 0; i < 100 && !sawInflight; i++ {
		time.Sleep(5 * time.Millisecond)
		qw := httptest.NewRecorder()
		h.ServeHTTP(qw, httptest.NewRequest(http.MethodGet, "/v1/quality", nil))
		sawInflight = strings.Contains(qw.Body.String(), `"inflight":1`)
	}
	wg.Wait()
	if !sawInflight {
		t.Fatal("/v1/quality never reported inflight=1 while a request was in flight")
	}
}

// ── #166 fail-loud headers on an upstream error ──────────────────────────────

// TestFailLoudHeadersOnUpstreamError proves the X-Mainspring-* signals reach the
// client on an upstream *failure* too. They used to be applied only on the
// commit path, so every early error return — the case an operator most needs to
// attribute — went out anonymous.
func TestFailLoudHeadersOnUpstreamError(t *testing.T) {
	dead := httptest.NewServer(http.NewServeMux())
	dead.Close() // nothing is listening → the upstream call fails outright
	h, _ := acctServer(t, fakeSched(dead.URL), auth.New(nil), nil)

	w := postBody(h, `{"model":"m1","messages":[]}`)
	if w.Code != http.StatusBadGateway {
		t.Fatalf("status=%d, want 502; body=%s", w.Code, w.Body.String())
	}
	if got := w.Header().Get("X-Mainspring-Backend"); got != "fake" {
		t.Fatalf("X-Mainspring-Backend = %q, want fake on an upstream-error response", got)
	}
	if got := w.Header().Get("X-Mainspring-Device"); got != "metal" {
		t.Fatalf("X-Mainspring-Device = %q, want metal", got)
	}
}

// TestServedModelHeaderOnUpstreamError is the same guarantee for the fallback
// signal: a client told "this failed" must still be told which model failed.
func TestServedModelHeaderOnUpstreamError(t *testing.T) {
	dead := httptest.NewServer(http.NewServeMux())
	dead.Close()
	sched := scheduler.New(
		map[string]backend.Backend{
			"dead": unloadableBackend{},
			"live": &engineBackend{baseURL: dead.URL},
		},
		[]backend.ModelSpec{
			{ID: "primary", Backend: "dead"},
			{ID: "backup", Backend: "live"},
		},
		scheduler.Options{MaxLoaded: 2},
	)
	h, _ := acctServer(t, sched, auth.New(nil), func(srv *server.Server) {
		srv.SetModelFallbacks(map[string][]string{"primary": {"backup"}})
	})

	w := postBody(h, `{"model":"primary","messages":[]}`)
	if w.Code != http.StatusBadGateway {
		t.Fatalf("status=%d, want 502; body=%s", w.Code, w.Body.String())
	}
	if got := w.Header().Get("X-Mainspring-Served-Model"); got != "backup" {
		t.Fatalf("X-Mainspring-Served-Model = %q, want backup on the error response", got)
	}
}

// postKeyed posts an authenticated inference request.
func postKeyed(h http.Handler, key, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+key)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}
