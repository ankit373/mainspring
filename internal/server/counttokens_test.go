package server_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ankit373/mainspring/internal/auth"
	"github.com/ankit373/mainspring/internal/backend"
	"github.com/ankit373/mainspring/internal/metrics"
	"github.com/ankit373/mainspring/internal/scheduler"
	"github.com/ankit373/mainspring/internal/server"
)

// POST /v1/messages/count_tokens is the Anthropic SDK's pre-flight budget check.
// Its body is exactly {"input_tokens": N}, and whether N was counted or guessed
// is reported on X-Mainspring-Tokens-Method — the difference between a budget a
// caller can trust and one it cannot.

func countTokens(h http.Handler, body string) *httptest.ResponseRecorder {
	return postBody2(h, "/v1/messages/count_tokens", body)
}

func decodeCount(t *testing.T, w *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v (%s)", err, w.Body.String())
	}
	return out
}

func inputTokens(t *testing.T, w *httptest.ResponseRecorder) int {
	t.Helper()
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", w.Code, w.Body.String())
	}
	out := decodeCount(t, w)
	n, ok := out["input_tokens"].(float64)
	if !ok {
		t.Fatalf("input_tokens missing or not a number: %s", w.Body.String())
	}
	return int(n)
}

// The response body is the Anthropic contract and nothing else: adding fields to
// it is how an SDK that validates the shape starts failing.
func TestCountTokensBodyIsExactlyInputTokens(t *testing.T) {
	h := tokenizeServer(t)
	w := countTokens(h, `{"model":"m1","messages":[{"role":"user","content":"hi"}]}`)
	out := decodeCount(t, w)
	if len(out) != 1 {
		t.Fatalf("body has %d fields, want exactly input_tokens: %s", len(out), w.Body.String())
	}
	if _, ok := out["input_tokens"]; !ok {
		t.Fatalf("body missing input_tokens: %s", w.Body.String())
	}
}

// The count must come from the same tokenizer /v1/tokenize uses, not a second
// implementation free to drift from it: for the same text the two must agree,
// apart from the chat template's fixed 4-token per-message overhead that only a
// message-shaped request carries.
func TestCountTokensMatchesTokenizeExactly(t *testing.T) {
	h := tokenizeServer(t)
	if w := post(t, h, "/admin/models/m1/load"); w.Code != 200 {
		t.Fatalf("load status=%d body=%s", w.Code, w.Body.String())
	}
	for _, text := range []string{"one two three four", "a", "the quick brown fox jumps"} {
		enc, err := json.Marshal(text)
		if err != nil {
			t.Fatal(err)
		}
		want, exact := decodeTokenize(t, postBody2(h, "/v1/tokenize",
			`{"model":"m1","input":`+string(enc)+`}`))
		if !exact {
			t.Fatal("resident TokenCounter should make /v1/tokenize exact")
		}
		w := countTokens(h, `{"model":"m1","messages":[{"role":"user","content":`+string(enc)+`}]}`)
		if got := inputTokens(t, w); got != want+4 {
			t.Fatalf("count_tokens(%q) = %d, want %d (/v1/tokenize's %d + one message's overhead)",
				text, got, want+4, want)
		}
	}
}

// Every turn costs the chat template's fixed per-message overhead (4 tokens) —
// the system prompt included, because the Anthropic → OpenAI translation makes it
// a leading system *message*. The context guardrail counts the same way.
func TestCountTokensCountsMessagesWithOverhead(t *testing.T) {
	h := tokenizeServer(t)
	if w := post(t, h, "/admin/models/m1/load"); w.Code != 200 {
		t.Fatalf("load status=%d", w.Code)
	}
	for _, tc := range []struct {
		name string
		body string
		want int
	}{
		// tokenizingRunner counts words; +4 per message.
		{"one message", `{"model":"m1","messages":[{"role":"user","content":"one two three"}]}`, 3 + 4},
		{"two messages", `{"model":"m1","messages":[` +
			`{"role":"user","content":"one two"},{"role":"assistant","content":"three"}]}`, 3 + 8},
		{"system plus message", `{"model":"m1","system":"be terse",` +
			`"messages":[{"role":"user","content":"hello there"}]}`, (2 + 4) + (2 + 4)},
		{"text blocks", `{"model":"m1","messages":[{"role":"user","content":` +
			`[{"type":"text","text":"one two"}]}]}`, 2 + 4},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := inputTokens(t, countTokens(h, tc.body)); got != tc.want {
				t.Fatalf("input_tokens = %d, want %d", got, tc.want)
			}
		})
	}
}

// Exactness is reported, not assumed: exact when the model is resident on a
// tokenizing engine, estimated when it is not (counting must never force a load
// — the endpoint exists to be cheap).
func TestCountTokensReportsExactness(t *testing.T) {
	h := tokenizeServer(t)
	body := `{"model":"m1","messages":[{"role":"user","content":"one two three four"}]}`

	w := countTokens(h, body)
	if got := w.Header().Get("X-Mainspring-Tokens-Method"); got != "estimated" {
		t.Fatalf("method = %q, want estimated (model not resident)", got)
	}
	// 18 chars → ceil(18/4)=5, +4 per-message overhead.
	if got := inputTokens(t, w); got != 9 {
		t.Fatalf("estimated input_tokens = %d, want 9", got)
	}

	if w := post(t, h, "/admin/models/m1/load"); w.Code != 200 {
		t.Fatalf("load status=%d", w.Code)
	}
	w = countTokens(h, body)
	if got := w.Header().Get("X-Mainspring-Tokens-Method"); got != "exact" {
		t.Fatalf("method = %q, want exact (model resident on a tokenizing engine)", got)
	}
	if got := inputTokens(t, w); got != 8 { // 4 words + 4 overhead
		t.Fatalf("exact input_tokens = %d, want 8", got)
	}
}

// aliasTokenizeServer is tokenizeServer with an alias pointing at the model, so
// the alias resolution count_tokens shares with /v1/messages can be asserted.
func aliasTokenizeServer(t *testing.T) http.Handler {
	t.Helper()
	eng := fakeEngine(t)
	sched := scheduler.New(
		map[string]backend.Backend{"tok": tokenizingBackend{baseURL: eng.URL}},
		[]backend.ModelSpec{{ID: "m1", Backend: "tok"}},
		scheduler.Options{MaxLoaded: 2, Aliases: map[string]string{"friendly": "m1"}},
	)
	rec, _ := metrics.New("")
	return server.New(sched, auth.New(nil), rec).Handler()
}

func TestCountTokensResolvesAliases(t *testing.T) {
	h := aliasTokenizeServer(t)
	if w := post(t, h, "/admin/models/m1/load"); w.Code != 200 {
		t.Fatalf("load status=%d", w.Code)
	}
	w := countTokens(h, `{"model":"friendly","messages":[{"role":"user","content":"one two"}]}`)
	if got := inputTokens(t, w); got != 6 { // 2 words + 4 overhead, counted exactly
		t.Fatalf("input_tokens = %d, want 6", got)
	}
	if got := w.Header().Get("X-Mainspring-Tokens-Method"); got != "exact" {
		t.Fatalf("method = %q, want exact — the alias did not resolve to the resident model", got)
	}
}

func TestCountTokensRejects(t *testing.T) {
	h := tokenizeServer(t)
	for _, tc := range []struct {
		name, body, code string
		status           int
	}{
		{"unknown model", `{"model":"ghost","messages":[]}`, "model_not_found", http.StatusNotFound},
		{"missing model", `{"messages":[]}`, "invalid_request", http.StatusBadRequest},
		{"malformed json", `{`, "invalid_request", http.StatusBadRequest},
		{"untranslatable content", `{"model":"m1","messages":[{"role":"user","content":7}]}`,
			"invalid_request", http.StatusBadRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := countTokens(h, tc.body)
			if w.Code != tc.status {
				t.Fatalf("status = %d, want %d (%s)", w.Code, tc.status, w.Body.String())
			}
			if got := decodeAPIError(t, w).Error.Code; got != tc.code {
				t.Fatalf("code = %q, want %q", got, tc.code)
			}
		})
	}
}

// The route is method-scoped, so a GET is the taxonomy's 405 — not a 404, and
// not net/http's plain text.
func TestCountTokensGetIsMethodNotAllowed(t *testing.T) {
	h := tokenizeServer(t)
	w := do(h, http.MethodGet, "/v1/messages/count_tokens")
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405 (%s)", w.Code, w.Body.String())
	}
	if got := w.Header().Get("Allow"); got != "POST" {
		t.Fatalf("Allow = %q, want POST", got)
	}
	if got := decodeAPIError(t, w).Error.Code; got != "method_not_allowed" {
		t.Fatalf("code = %q, want method_not_allowed", got)
	}
}
