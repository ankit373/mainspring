package server_test

import (
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

func anthropicServer(t *testing.T) http.Handler {
	t.Helper()
	eng := fakeEngine(t)
	sched := scheduler.New(
		map[string]backend.Backend{"fake": &engineBackend{baseURL: eng.URL}},
		[]backend.ModelSpec{{ID: "m1", Backend: "fake"}},
		scheduler.Options{MaxLoaded: 2},
	)
	rec, _ := metrics.New("")
	return server.New(sched, auth.New(nil), rec).Handler()
}

func TestMessagesNonStreaming(t *testing.T) {
	h := anthropicServer(t)
	req := httptest.NewRequest(http.MethodPost, "/v1/messages",
		strings.NewReader(`{"model":"m1","max_tokens":64,"system":"be brief","messages":[{"role":"user","content":"hi"}]}`))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != 200 {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	var resp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if resp["type"] != "message" || resp["role"] != "assistant" {
		t.Fatalf("not an Anthropic message: %v", resp)
	}
	content := resp["content"].([]any)
	block := content[0].(map[string]any)
	if block["type"] != "text" || !strings.Contains(block["text"].(string), "Hello") {
		t.Fatalf("unexpected content block: %v", block)
	}
	if resp["stop_reason"] != "end_turn" {
		t.Fatalf("stop_reason=%v, want end_turn", resp["stop_reason"])
	}
	if _, ok := resp["usage"].(map[string]any); !ok {
		t.Fatal("missing usage")
	}
}

func TestMessagesStreaming(t *testing.T) {
	h := anthropicServer(t)
	req := httptest.NewRequest(http.MethodPost, "/v1/messages",
		strings.NewReader(`{"model":"m1","max_tokens":64,"stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	body := w.Body.String()
	for _, want := range []string{
		"event: message_start", "event: content_block_start",
		"event: content_block_delta", "text_delta",
		"event: content_block_stop", "event: message_delta", "event: message_stop",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("stream missing %q:\n%s", want, body)
		}
	}
	if ct := w.Header().Get("Content-Type"); !strings.Contains(ct, "event-stream") {
		t.Fatalf("expected SSE content-type, got %q", ct)
	}
}

func TestMessagesUnknownModel(t *testing.T) {
	h := anthropicServer(t)
	req := httptest.NewRequest(http.MethodPost, "/v1/messages",
		strings.NewReader(`{"model":"nope","messages":[{"role":"user","content":"hi"}]}`))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != 404 {
		t.Fatalf("unknown model: want 404 got %d", w.Code)
	}
}
