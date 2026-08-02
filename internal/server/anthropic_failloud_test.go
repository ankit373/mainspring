package server_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// erroringEngine always answers /v1/chat/completions with the given status/body.
func erroringEngine(t *testing.T, status int, body string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// stallingEngine answers only after delay (or when the client gives up), so a
// per-request timeout can be observed.
func stallingEngine(t *testing.T, delay time.Duration) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(delay):
		case <-r.Context().Done():
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"late"}}]}`)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// truncatedStreamEngine opens an SSE response, emits one content frame, and then
// stops short of the Content-Length it promised — the wire-level equivalent of a
// generation cut mid-stream (the client's body read fails with an unexpected EOF).
func truncatedStreamEngine(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Content-Length", "4096") // far more than we will write
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"Hel\"}}]}\n\n")
		w.(http.Flusher).Flush()
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// TestMessagesUpstream500IsNotEmptySuccess proves an upstream 5xx no longer decodes
// into a well-formed empty 200 Anthropic message.
func TestMessagesUpstream500IsNotEmptySuccess(t *testing.T) {
	eng := erroringEngine(t, http.StatusInternalServerError, `{"error":"engine exploded"}`)
	h, _ := messagesLedgerServer(t, eng.URL, nil)

	w := postMessages(h, `{"model":"m1","max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`)
	if w.Code != http.StatusBadGateway {
		t.Fatalf("status=%d, want 502; body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, "upstream_error") {
		t.Fatalf("body should carry the upstream_error code: %s", body)
	}
	if !strings.Contains(body, "engine exploded") {
		t.Fatalf("body should carry the upstream message: %s", body)
	}
	if strings.Contains(body, `"type":"message"`) {
		t.Fatalf("an upstream failure must not look like an Anthropic message: %s", body)
	}
}

// TestMessagesUpstream400PassesThrough proves an upstream client error keeps its
// own status instead of being reported as a success.
func TestMessagesUpstream400PassesThrough(t *testing.T) {
	eng := erroringEngine(t, http.StatusBadRequest, `{"error":"unknown sampler"}`)
	h, _ := messagesLedgerServer(t, eng.URL, nil)

	w := postMessages(h, `{"model":"m1","max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status=%d, want 400; body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "unknown sampler") {
		t.Fatalf("body should carry the upstream message: %s", w.Body.String())
	}
}

// TestMessagesStreamUpstreamErrorBeforeSSE proves a streaming request whose
// upstream fails gets a real error status — the stream is never opened, because
// once it is the status can no longer be corrected.
func TestMessagesStreamUpstreamErrorBeforeSSE(t *testing.T) {
	eng := erroringEngine(t, http.StatusInternalServerError, `{"error":"engine exploded"}`)
	h, _ := messagesLedgerServer(t, eng.URL, nil)

	w := postMessages(h, `{"model":"m1","max_tokens":64,"stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	if w.Code != http.StatusBadGateway {
		t.Fatalf("status=%d, want 502; body=%s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "event: message_start") {
		t.Fatalf("no SSE should be emitted for a failed upstream: %s", w.Body.String())
	}
}

// TestMessagesStreamTruncatedEmitsError proves a stream cut mid-generation ends
// with an Anthropic `error` event and never a fabricated message_stop.
func TestMessagesStreamTruncatedEmitsError(t *testing.T) {
	eng := truncatedStreamEngine(t)
	h, _ := messagesLedgerServer(t, eng.URL, nil)

	w := postMessages(h, `{"model":"m1","max_tokens":64,"stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	body := w.Body.String()
	if !strings.Contains(body, "event: error") {
		t.Fatalf("truncated stream must emit an error event:\n%s", body)
	}
	if !strings.Contains(body, "upstream_error") {
		t.Fatalf("error event should carry the upstream_error code:\n%s", body)
	}
	if strings.Contains(body, "event: message_stop") {
		t.Fatalf("a truncated stream must not be closed with message_stop:\n%s", body)
	}
	if strings.Contains(body, "event: message_delta") {
		t.Fatalf("a truncated stream must not report a stop_reason:\n%s", body)
	}
}

// TestMessagesStreamDeltaReportsInputTokens proves the final message_delta carries
// the prompt size, so a streaming client can see it at all.
func TestMessagesStreamDeltaReportsInputTokens(t *testing.T) {
	eng := usageEngine(t)
	h, _ := messagesLedgerServer(t, eng.URL, nil)

	w := postMessages(h, `{"model":"m1","max_tokens":64,"stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	body := w.Body.String()
	_, delta, ok := strings.Cut(body, "event: message_delta")
	if !ok {
		t.Fatalf("stream missing message_delta:\n%s", body)
	}
	if !strings.Contains(delta, `"input_tokens":15`) {
		t.Fatalf("message_delta usage should report input_tokens=15:\n%s", delta)
	}
}
