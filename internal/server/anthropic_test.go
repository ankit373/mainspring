package server_test

import (
	"encoding/json"
	"io"
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

// toolEngineServer builds a /v1/messages handler backed by a fake OpenAI engine
// that returns a tool call (non-stream) or streams one (SSE) when it sees tools.
func toolEngineServer(t *testing.T) http.Handler {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		stream := strings.Contains(string(body), `"stream":true`)
		if stream {
			w.Header().Set("Content-Type", "text/event-stream")
			fl := w.(http.Flusher)
			// Tool call streamed in fragments across two chunks.
			frames := []string{
				`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"get_weather","arguments":"{\"city\":"}}]}}]}`,
				`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"paris\"}"}}]},"finish_reason":"tool_calls"}]}`,
			}
			for _, f := range frames {
				_, _ = io.WriteString(w, "data: "+f+"\n\n")
				fl.Flush()
			}
			_, _ = io.WriteString(w, "data: [DONE]\n\n")
			fl.Flush()
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"","tool_calls":[{"id":"call_1","type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"paris\"}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":10,"completion_tokens":5}}`)
	})
	eng := httptest.NewServer(mux)
	t.Cleanup(eng.Close)
	sched := scheduler.New(
		map[string]backend.Backend{"fake": &engineBackend{baseURL: eng.URL}},
		[]backend.ModelSpec{{ID: "m1", Backend: "fake"}},
		scheduler.Options{MaxLoaded: 2},
	)
	rec, _ := metrics.New("")
	return server.New(sched, auth.New(nil), rec).Handler()
}

func TestMessagesToolUseNonStreaming(t *testing.T) {
	h := toolEngineServer(t)
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(
		`{"model":"m1","max_tokens":64,"tools":[{"name":"get_weather","input_schema":{"type":"object"}}],"messages":[{"role":"user","content":"weather in paris?"}]}`))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	var resp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp["stop_reason"] != "tool_use" {
		t.Fatalf("stop_reason=%v, want tool_use", resp["stop_reason"])
	}
	content := resp["content"].([]any)
	var found map[string]any
	for _, c := range content {
		if b := c.(map[string]any); b["type"] == "tool_use" {
			found = b
		}
	}
	if found == nil {
		t.Fatalf("no tool_use block: %v", content)
	}
	if found["name"] != "get_weather" {
		t.Fatalf("tool name=%v", found["name"])
	}
	input := found["input"].(map[string]any)
	if input["city"] != "paris" {
		t.Fatalf("tool input=%v", input)
	}
}

func TestMessagesToolUseStreaming(t *testing.T) {
	h := toolEngineServer(t)
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(
		`{"model":"m1","max_tokens":64,"stream":true,"tools":[{"name":"get_weather"}],"messages":[{"role":"user","content":"weather?"}]}`))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	body := w.Body.String()
	for _, want := range []string{
		`"type":"tool_use"`, `"name":"get_weather"`, "input_json_delta",
		`"partial_json"`, "event: content_block_stop", `"stop_reason":"tool_use"`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("tool stream missing %q:\n%s", want, body)
		}
	}
}

// usageEngineServer returns a fixed usage object (non-stream) and an
// include_usage final chunk (stream).
func usageEngineServer(t *testing.T) http.Handler {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if strings.Contains(string(body), `"stream":true`) {
			w.Header().Set("Content-Type", "text/event-stream")
			fl := w.(http.Flusher)
			_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n")
			fl.Flush()
			_, _ = io.WriteString(w, "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":15,\"completion_tokens\":3}}\n\n")
			fl.Flush()
			_, _ = io.WriteString(w, "data: [DONE]\n\n")
			fl.Flush()
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"Hello"}}],"usage":{"prompt_tokens":12,"completion_tokens":4}}`)
	})
	eng := httptest.NewServer(mux)
	t.Cleanup(eng.Close)
	sched := scheduler.New(
		map[string]backend.Backend{"fake": &engineBackend{baseURL: eng.URL}},
		[]backend.ModelSpec{{ID: "m1", Backend: "fake"}},
		scheduler.Options{MaxLoaded: 2},
	)
	rec, _ := metrics.New("")
	return server.New(sched, auth.New(nil), rec).Handler()
}

func TestMessagesUsageHeaderNonStream(t *testing.T) {
	h := usageEngineServer(t)
	req := httptest.NewRequest(http.MethodPost, "/v1/messages",
		strings.NewReader(`{"model":"m1","max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if got := w.Header().Get("X-Mainspring-Tokens-Input"); got != "12" {
		t.Fatalf("input token header=%q, want 12", got)
	}
	if got := w.Header().Get("X-Mainspring-Tokens-Output"); got != "4" {
		t.Fatalf("output token header=%q, want 4", got)
	}
}

func TestMessagesUsageStream(t *testing.T) {
	h := usageEngineServer(t)
	req := httptest.NewRequest(http.MethodPost, "/v1/messages",
		strings.NewReader(`{"model":"m1","max_tokens":64,"stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	// The real upstream completion count (3) must appear in message_delta usage,
	// not the raw frame count.
	if !strings.Contains(w.Body.String(), `"output_tokens":3`) {
		t.Fatalf("stream message_delta should carry real output_tokens=3:\n%s", w.Body.String())
	}
}

// capturingEngineServer records the raw upstream request body it receives, so a
// test can assert on exactly what Mainspring asked the backend for.
func capturingEngineServer(t *testing.T) (http.Handler, func() []byte) {
	t.Helper()
	var captured []byte
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		captured, _ = io.ReadAll(r.Body)
		if strings.Contains(string(captured), `"stream":true`) {
			w.Header().Set("Content-Type", "text/event-stream")
			fl := w.(http.Flusher)
			_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n")
			fl.Flush()
			_, _ = io.WriteString(w, "data: [DONE]\n\n")
			fl.Flush()
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"Hello"}}]}`)
	})
	eng := httptest.NewServer(mux)
	t.Cleanup(eng.Close)
	sched := scheduler.New(
		map[string]backend.Backend{"fake": &engineBackend{baseURL: eng.URL}},
		[]backend.ModelSpec{{ID: "m1", Backend: "fake"}},
		scheduler.Options{MaxLoaded: 2},
	)
	rec, _ := metrics.New("")
	return server.New(sched, auth.New(nil), rec).Handler(), func() []byte { return captured }
}

// TestStreamingRequestsIncludeUsage proves the fix: a streaming /v1/messages
// call must ask the upstream for stream_options.include_usage so
// messagesStream's exact-usage path is actually reachable.
func TestStreamingRequestsIncludeUsage(t *testing.T) {
	h, captured := capturingEngineServer(t)
	req := httptest.NewRequest(http.MethodPost, "/v1/messages",
		strings.NewReader(`{"model":"m1","max_tokens":64,"stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	var upstream map[string]any
	if err := json.Unmarshal(captured(), &upstream); err != nil {
		t.Fatalf("decode captured upstream body: %v", err)
	}
	opts, ok := upstream["stream_options"].(map[string]any)
	if !ok {
		t.Fatalf("upstream body missing stream_options: %v", upstream)
	}
	if opts["include_usage"] != true {
		t.Fatalf("stream_options.include_usage = %v, want true", opts["include_usage"])
	}
}

// A non-streaming request must not carry stream_options at all.
func TestNonStreamingOmitsStreamOptions(t *testing.T) {
	h, captured := capturingEngineServer(t)
	req := httptest.NewRequest(http.MethodPost, "/v1/messages",
		strings.NewReader(`{"model":"m1","max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	var upstream map[string]any
	if err := json.Unmarshal(captured(), &upstream); err != nil {
		t.Fatalf("decode captured upstream body: %v", err)
	}
	if _, ok := upstream["stream_options"]; ok {
		t.Fatalf("non-streaming request should not carry stream_options: %v", upstream)
	}
}

// TestMessagesForwardsSamplingParams: every sampling parameter the caller sent
// reaches the engine. top_k had no field on anthropicRequest at all, so it was
// dropped on the floor with no error — the one thing a fail-loud server must
// not do with a parameter it was given.
func TestMessagesForwardsSamplingParams(t *testing.T) {
	h, captured := capturingEngineServer(t)
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(
		`{"model":"m1","max_tokens":64,"temperature":0.3,"top_p":0.9,"top_k":40,`+
			`"messages":[{"role":"user","content":"hi"}]}`))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	var upstream map[string]any
	if err := json.Unmarshal(captured(), &upstream); err != nil {
		t.Fatalf("decode captured upstream body: %v", err)
	}
	if upstream["temperature"] != 0.3 || upstream["top_p"] != 0.9 {
		t.Fatalf("temperature/top_p not forwarded: %v", upstream)
	}
	if upstream["top_k"] != float64(40) {
		t.Fatalf("top_k = %v, want 40 (silently discarded)", upstream["top_k"])
	}

	// Unset sampling params stay absent so the engine's own default applies.
	h2, captured2 := capturingEngineServer(t)
	req2 := httptest.NewRequest(http.MethodPost, "/v1/messages",
		strings.NewReader(`{"model":"m1","max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`))
	h2.ServeHTTP(httptest.NewRecorder(), req2)
	var plain map[string]any
	if err := json.Unmarshal(captured2(), &plain); err != nil {
		t.Fatalf("decode captured upstream body: %v", err)
	}
	if _, ok := plain["top_k"]; ok {
		t.Fatalf("unset top_k must not be sent: %v", plain)
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
