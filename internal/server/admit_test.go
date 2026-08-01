package server_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ankit373/mainspring/internal/breaker"
	"github.com/ankit373/mainspring/internal/server"
)

// countingMessagesEngine answers non-streaming chat completions and records both
// the hit count and the last upstream body, so a test can assert on exactly what
// (if anything) reached the engine.
func countingMessagesEngine(t *testing.T, hits *atomic.Int64) (*httptest.Server, func() []byte) {
	t.Helper()
	var captured atomic.Pointer[[]byte]
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		b, _ := io.ReadAll(r.Body)
		captured.Store(&b)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"Hello"}}],"usage":{"prompt_tokens":3,"completion_tokens":2}}`)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, func() []byte {
		if p := captured.Load(); p != nil {
			return *p
		}
		return nil
	}
}

// TestMessagesContextGuardEnforceRejects proves enforce_context now protects the
// Anthropic API too: an over-context request is rejected before any backend work.
func TestMessagesContextGuardEnforceRejects(t *testing.T) {
	var hits atomic.Int64
	eng, _ := countingMessagesEngine(t, &hits)
	h, _ := messagesLedgerServer(t, eng.URL, func(srv *server.Server) {
		srv.SetContextGuard(map[string]server.ContextPolicy{"m1": {Limit: 4096, Enforce: true}})
	})

	w := postMessages(h, `{"model":"m1","max_tokens":99999,"messages":[{"role":"user","content":"hi"}]}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status=%d, want 400; body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "context_length_exceeded") {
		t.Fatalf("body missing context_length_exceeded: %s", w.Body.String())
	}
	if hits.Load() != 0 {
		t.Fatal("over-context /v1/messages request must be rejected before hitting the backend")
	}
}

// TestMessagesContextGuardWarns proves the warn-only policy reaches the backend
// and reports the overage on a header, same as the OpenAI path.
func TestMessagesContextGuardWarns(t *testing.T) {
	var hits atomic.Int64
	eng, _ := countingMessagesEngine(t, &hits)
	h, _ := messagesLedgerServer(t, eng.URL, func(srv *server.Server) {
		srv.SetContextGuard(map[string]server.ContextPolicy{"m1": {Limit: 4096, Enforce: false}})
	})

	w := postMessages(h, `{"model":"m1","max_tokens":99999,"messages":[{"role":"user","content":"hi"}]}`)
	if w.Code != 200 {
		t.Fatalf("warn mode status=%d, want 200; body=%s", w.Code, w.Body.String())
	}
	if w.Header().Get("X-Mainspring-Context-Warning") == "" {
		t.Fatal("warn mode should set X-Mainspring-Context-Warning")
	}
	if hits.Load() != 1 {
		t.Fatalf("warn mode should still reach the backend, hits=%d", hits.Load())
	}
}

// TestMessagesClampShrinksMaxTokens proves clamp_max_tokens applies to
// /v1/messages: the request is served, and the engine sees the reduced budget.
func TestMessagesClampShrinksMaxTokens(t *testing.T) {
	var hits atomic.Int64
	eng, captured := countingMessagesEngine(t, &hits)
	h, _ := messagesLedgerServer(t, eng.URL, func(srv *server.Server) {
		srv.SetClampLimits(map[string]int{"m1": 4096})
	})

	w := postMessages(h, `{"model":"m1","max_tokens":99999,"messages":[{"role":"user","content":"hi"}]}`)
	if w.Code != 200 {
		t.Fatalf("status=%d, want 200 (clamped, not rejected); body=%s", w.Code, w.Body.String())
	}
	if w.Header().Get("X-Mainspring-Clamped") == "" {
		t.Fatal("expected X-Mainspring-Clamped header")
	}
	var upstream struct {
		MaxTokens int `json:"max_tokens"`
	}
	if err := json.Unmarshal(captured(), &upstream); err != nil {
		t.Fatalf("decode upstream body: %v", err)
	}
	if upstream.MaxTokens <= 0 || upstream.MaxTokens > 4096 {
		t.Fatalf("upstream max_tokens=%d, want clamped into (0,4096]", upstream.MaxTokens)
	}
}

// breakerMessagesServer wires a /v1/messages handler with a tripping breaker and
// an injected clock, so the cooldown can be advanced without sleeping.
func breakerMessagesServer(t *testing.T, baseURL string) (http.Handler, *breaker.Group, *atomic.Int64) {
	t.Helper()
	clock := &atomic.Int64{}
	clock.Store(time.Now().UnixNano())
	var grp *breaker.Group
	h, _ := messagesLedgerServer(t, baseURL, func(srv *server.Server) {
		srv.SetBreaker(1, time.Minute) // one failure trips it
		grp = srv.Breaker()
		grp.SetClock(func() time.Time { return time.Unix(0, clock.Load()) })
	})
	return h, grp, clock
}

// TestMessagesBreakerClosesAfterHalfOpenProbe is the regression test for a breaker
// driven half-open by /v1/messages: because the path never reported a result, the
// probe was consumed and the breaker sat half-open forever, fast-failing every
// later request. A good probe must now close it.
func TestMessagesBreakerClosesAfterHalfOpenProbe(t *testing.T) {
	var hits atomic.Int64
	eng := flakyEngine(t, 1, &hits) // first hit 503, then healthy
	h, grp, clock := breakerMessagesServer(t, eng.URL)

	const body = `{"model":"m1","max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`

	// 1. Upstream 503 → 502 upstream_error → recorded as a backend failure → Open.
	if w := postMessages(h, body); w.Code != http.StatusBadGateway {
		t.Fatalf("first request status=%d, want 502; body=%s", w.Code, w.Body.String())
	}
	if got := grp.State("m1"); got != breaker.Open {
		t.Fatalf("breaker state=%v, want open after an upstream 5xx", got)
	}

	// 2. Still cooling down: fast-fail without touching the engine.
	w2 := postMessages(h, body)
	if w2.Code != http.StatusServiceUnavailable || !strings.Contains(w2.Body.String(), "circuit_open") {
		t.Fatalf("second request status=%d body=%s, want 503 circuit_open", w2.Code, w2.Body.String())
	}

	// 3. Cooldown elapsed: this request is the half-open probe, and it succeeds.
	clock.Add(int64(2 * time.Minute))
	if w := postMessages(h, body); w.Code != 200 {
		t.Fatalf("half-open probe status=%d, want 200; body=%s", w.Code, w.Body.String())
	}
	if got := grp.State("m1"); got != breaker.Closed {
		t.Fatalf("breaker state=%v, want closed after a successful probe", got)
	}

	// 4. And the next request is served rather than fast-failed.
	if w := postMessages(h, body); w.Code != 200 {
		t.Fatalf("post-recovery status=%d, want 200; body=%s", w.Code, w.Body.String())
	}
	if hits.Load() != 3 {
		t.Fatalf("engine hits=%d, want 3 (fail + probe + served)", hits.Load())
	}
}

// TestMessagesBreakerTripsOnUpstreamFailure proves a 5xx on /v1/messages is
// reported to the breaker as a failure.
func TestMessagesBreakerTripsOnUpstreamFailure(t *testing.T) {
	var hits atomic.Int64
	eng := flakyEngine(t, -1, &hits) // always fails
	h, grp, _ := breakerMessagesServer(t, eng.URL)

	const body = `{"model":"m1","max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`
	if w := postMessages(h, body); w.Code != http.StatusBadGateway {
		t.Fatalf("status=%d, want 502; body=%s", w.Code, w.Body.String())
	}
	if got := grp.State("m1"); got != breaker.Open {
		t.Fatalf("breaker state=%v, want open", got)
	}
	if hits.Load() != 1 {
		t.Fatalf("engine hits=%d, want 1", hits.Load())
	}
}

// TestMessagesTruncatedStreamTripsBreaker is the regression test for the one
// failure the HTTP status cannot express: once an SSE stream is open the response
// is pinned at 200, so a generation that dies mid-stream used to be reported to the
// breaker as a success. The backend is broken; the breaker has to hear about it.
func TestMessagesTruncatedStreamTripsBreaker(t *testing.T) {
	eng := truncatedStreamEngine(t)
	h, grp, _ := breakerMessagesServer(t, eng.URL)

	w := postMessages(h, `{"model":"m1","max_tokens":64,"stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	// The stream opened, so the status is necessarily 200 — that is the whole point.
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200 (SSE headers are already sent)", w.Code)
	}
	if !strings.Contains(w.Body.String(), "event: error") {
		t.Fatalf("truncated stream must emit an error event:\n%s", w.Body.String())
	}
	if got := grp.State("m1"); got != breaker.Open {
		t.Fatalf("breaker state=%v, want open: a stream that died mid-flight is a backend failure", got)
	}
}
