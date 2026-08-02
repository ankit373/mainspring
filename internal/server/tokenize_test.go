package server_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ankit373/mainspring/internal/auth"
	"github.com/ankit373/mainspring/internal/backend"
	"github.com/ankit373/mainspring/internal/metrics"
	"github.com/ankit373/mainspring/internal/scheduler"
	"github.com/ankit373/mainspring/internal/server"
)

// tokenizingBackend starts a runner that implements backend.TokenCounter,
// counting words as a stand-in for a real tokenizer.
type tokenizingBackend struct{ baseURL string }

func (b tokenizingBackend) Name() string { return "tok" }
func (b tokenizingBackend) Detect(context.Context) backend.Availability {
	return backend.Availability{Name: "tok", Present: true}
}
func (b tokenizingBackend) Start(_ context.Context, spec backend.ModelSpec) (backend.Runner, error) {
	return &tokenizingRunner{base: b.baseURL, id: spec.ID}, nil
}

type tokenizingRunner struct {
	base string
	id   string
}

func (r *tokenizingRunner) BaseURL() string                       { return r.base }
func (r *tokenizingRunner) Health(context.Context) backend.Status { return backend.StatusReady }
func (r *tokenizingRunner) MemoryBytes() int64                    { return 1 }
func (r *tokenizingRunner) Stop(context.Context) error            { return nil }
func (r *tokenizingRunner) Capabilities(context.Context) (backend.Capabilities, error) {
	return backend.Capabilities{Backend: "tok", Model: r.id, Device: "metal", EffectiveCtx: 4096}, nil
}
func (r *tokenizingRunner) CountTokens(_ context.Context, text string) (int, error) {
	return len(strings.Fields(text)), nil
}

func tokenizeServer(t *testing.T) http.Handler {
	t.Helper()
	eng := fakeEngine(t)
	sched := scheduler.New(
		map[string]backend.Backend{"tok": tokenizingBackend{baseURL: eng.URL}},
		[]backend.ModelSpec{{ID: "m1", Backend: "tok"}},
		scheduler.Options{MaxLoaded: 2},
	)
	rec, _ := metrics.New("")
	return server.New(sched, auth.New(nil), rec).Handler()
}

func decodeTokenize(t *testing.T, w *httptest.ResponseRecorder) (int, bool) {
	t.Helper()
	var out struct {
		Tokens int  `json:"tokens"`
		Exact  bool `json:"exact"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v (%s)", err, w.Body.String())
	}
	return out.Tokens, out.Exact
}

func TestTokenizeEstimateWhenNotResident(t *testing.T) {
	h := tokenizeServer(t)
	// Model not loaded → falls back to the char estimate (exact=false).
	w := postBody2(h, "/v1/tokenize", `{"model":"m1","input":"aaaa bbbb"}`) // 9 chars → ceil(9/4)=3
	if w.Code != 200 {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	tokens, exact := decodeTokenize(t, w)
	if exact {
		t.Fatal("model not resident should give an estimate, not exact")
	}
	if tokens != 3 {
		t.Fatalf("estimate tokens = %d, want 3", tokens)
	}
}

func TestTokenizeExactWhenResident(t *testing.T) {
	h := tokenizeServer(t)
	// Load the model so its runner (a TokenCounter) is resident.
	if w := post(t, h, "/admin/models/m1/load"); w.Code != 200 {
		t.Fatalf("load status=%d body=%s", w.Code, w.Body.String())
	}
	w := postBody2(h, "/v1/tokenize", `{"model":"m1","input":"one two three four"}`)
	tokens, exact := decodeTokenize(t, w)
	if !exact {
		t.Fatal("resident TokenCounter should give an exact count")
	}
	if tokens != 4 {
		t.Fatalf("exact tokens = %d, want 4 (words)", tokens)
	}
}

func TestTokenizeUnknownModel(t *testing.T) {
	h := tokenizeServer(t)
	if w := postBody2(h, "/v1/tokenize", `{"model":"nope","input":"x"}`); w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", w.Code)
	}
}

// TestTokenizeArrayInput covers the documented embeddings-style array `input`,
// which the endpoint used to reject outright as a JSON type error.
func TestTokenizeArrayInput(t *testing.T) {
	h := tokenizeServer(t)
	if w := post(t, h, "/admin/models/m1/load"); w.Code != 200 {
		t.Fatalf("load status=%d body=%s", w.Code, w.Body.String())
	}
	w := postBody2(h, "/v1/tokenize", `{"model":"m1","input":["one two","three four five"]}`)
	if w.Code != 200 {
		t.Fatalf("array input status = %d, want 200: %s", w.Code, w.Body.String())
	}
	tokens, exact := decodeTokenize(t, w)
	if !exact {
		t.Fatal("resident TokenCounter should count an array exactly")
	}
	if tokens != 5 {
		t.Fatalf("array tokens = %d, want 5 (sum of the elements)", tokens)
	}
}

// TestTokenizeRejectsUncountableInput: shapes that genuinely cannot be counted
// get a precise 400 rather than a silent 0.
func TestTokenizeRejectsUncountableInput(t *testing.T) {
	h := tokenizeServer(t)
	for _, body := range []string{
		`{"model":"m1","input":42}`,
		`{"model":"m1","input":[1,2,3]}`, // pre-tokenized ids
		`{"model":"m1","input":{"text":"x"}}`,
	} {
		w := postBody2(h, "/v1/tokenize", body)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("%s → status %d, want 400 (%s)", body, w.Code, w.Body.String())
		}
		// A precise message, not a leaked Go unmarshal error.
		if !strings.Contains(w.Body.String(), "field input must be a string or an array of strings") {
			t.Fatalf("%s → 400 should say what shape is expected: %s", body, w.Body.String())
		}
	}
}

// TestPreciseGuardrailCountsBlockArraySystem: an Anthropic-shaped block-array
// system must still be counted by the exact tokenizer path, not abandoned.
func TestPreciseGuardrailCountsBlockArraySystem(t *testing.T) {
	h := preciseServer(t, 8, true) // window 8 tokens, word-counting tokenizer
	if w := post(t, h, "/admin/models/m1/load"); w.Code != 200 {
		t.Fatalf("load status=%d", w.Code)
	}
	// system "one two three four five six" = 6 words, + 4 message overhead + 1
	// word of content = 11 exact tokens > 8.
	w := postBody(h, `{"model":"m1","system":[{"type":"text","text":"one two three four five six"}],`+
		`"messages":[{"role":"user","content":"hi"}]}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (over-context): %s", w.Code, w.Body.String())
	}
	if got := w.Header().Get("X-Mainspring-Context-Method"); got != "exact" {
		t.Fatalf("method header = %q, want exact — the block-array system aborted the exact count", got)
	}
	if body := w.Body.String(); !strings.Contains(body, "prompt (11 tokens)") {
		t.Fatalf("expected an exact 11-token reason, got: %s", body)
	}
}

// postBody2 posts a JSON body to an arbitrary path.
func postBody2(h http.Handler, path, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}
