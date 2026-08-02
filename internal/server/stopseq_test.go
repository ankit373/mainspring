package server_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ankit373/mainspring/internal/auth"
	"github.com/ankit373/mainspring/internal/backend"
	"github.com/ankit373/mainspring/internal/metrics"
	"github.com/ankit373/mainspring/internal/scheduler"
	"github.com/ankit373/mainspring/internal/server"
)

// recordingEngine is a fake OpenAI upstream that remembers every request body it
// was sent — the only way to assert what the engine was, and was not, told.
// It deliberately keeps emitting frames past a stop sequence: Mainspring must not
// depend on the engine stopping, because a real engine that stops also erases the
// match on its way out.
type recordingEngine struct {
	mu     sync.Mutex
	bodies []string
	frames []string // SSE chunk payloads, sent in order
	json   string   // response for a non-streaming request
}

func (e *recordingEngine) serve(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	e.mu.Lock()
	e.bodies = append(e.bodies, string(body))
	e.mu.Unlock()

	if strings.Contains(string(body), `"stream":true`) {
		w.Header().Set("Content-Type", "text/event-stream")
		fl := w.(http.Flusher)
		for _, f := range e.frames {
			_, _ = io.WriteString(w, "data: "+f+"\n\n")
			fl.Flush()
		}
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
		fl.Flush()
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = io.WriteString(w, e.json)
}

func (e *recordingEngine) requests() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.bodies...)
}

func stopSeqServer(t *testing.T, eng *recordingEngine, tune func(*server.Server)) http.Handler {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/chat/completions", eng.serve)
	up := httptest.NewServer(mux)
	t.Cleanup(up.Close)

	sched := scheduler.New(
		map[string]backend.Backend{"fake": &engineBackend{baseURL: up.URL}},
		[]backend.ModelSpec{{ID: "m1", Backend: "fake"}},
		scheduler.Options{MaxLoaded: 2},
	)
	rec, _ := metrics.New("")
	srv := server.New(sched, auth.New(nil), rec)
	if tune != nil {
		tune(srv)
	}
	return srv.Handler()
}

func contentFrame(text string) string {
	b, _ := json.Marshal(text)
	return `{"choices":[{"delta":{"content":` + string(b) + `}}]}`
}

// messageText concatenates the text blocks of a non-streaming Anthropic message.
func messageText(t *testing.T, w *httptest.ResponseRecorder) (string, map[string]any) {
	t.Helper()
	if w.Code != 200 {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	var resp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, w.Body.String())
	}
	var text strings.Builder
	for _, raw := range resp["content"].([]any) {
		if b, _ := raw.(map[string]any); b["type"] == "text" {
			text.WriteString(b["text"].(string))
		}
	}
	return text.String(), resp
}

// sseText concatenates the text_delta fragments of an Anthropic SSE stream and
// returns the message_delta payload.
func sseText(t *testing.T, body string) (string, map[string]any) {
	t.Helper()
	var text strings.Builder
	var final map[string]any
	for _, line := range strings.Split(body, "\n") {
		data, ok := strings.CutPrefix(line, "data: ")
		if !ok {
			continue
		}
		var ev map[string]any
		if json.Unmarshal([]byte(data), &ev) != nil {
			continue
		}
		switch ev["type"] {
		case "content_block_delta":
			if d, _ := ev["delta"].(map[string]any); d["type"] == "text_delta" {
				text.WriteString(d["text"].(string))
			}
		case "message_delta":
			final, _ = ev["delta"].(map[string]any)
		}
	}
	if final == nil {
		t.Fatalf("stream carried no message_delta:\n%s", body)
	}
	return text.String(), final
}

// The sequence is split across two upstream fragments on purpose: neither one
// contains it, so a naive per-fragment match would miss it entirely.
var splitStopFrames = []string{
	contentFrame("Hello "),
	contentFrame("world[[ST"),
	contentFrame("OP]] and more that must never be seen"),
	`{"choices":[{"delta":{},"finish_reason":"stop"}]}`,
	`{"choices":[],"usage":{"prompt_tokens":7,"completion_tokens":9}}`,
}

func TestMessagesStopSequenceNonStreaming(t *testing.T) {
	eng := &recordingEngine{frames: splitStopFrames}
	h := stopSeqServer(t, eng, nil)

	w := postMessages(h, `{"model":"m1","max_tokens":64,"stop_sequences":["[[STOP]]"],
		"messages":[{"role":"user","content":"hi"}]}`)
	text, resp := messageText(t, w)

	if text != "Hello world" {
		t.Errorf("content = %q, want %q", text, "Hello world")
	}
	if resp["stop_reason"] != "stop_sequence" {
		t.Errorf("stop_reason = %v, want stop_sequence", resp["stop_reason"])
	}
	if resp["stop_sequence"] != "[[STOP]]" {
		t.Errorf("stop_sequence = %v, want [[STOP]]", resp["stop_sequence"])
	}

	// Our own stop cuts the stream before the upstream's usage chunk arrives, so
	// input_tokens has to be filled in rather than reported as a bogus zero.
	usage := resp["usage"].(map[string]any)
	if n, _ := usage["input_tokens"].(float64); n <= 0 {
		t.Errorf("input_tokens = %v, want a non-zero estimate", usage["input_tokens"])
	}

	// The engine must not have been told to stop — that is what makes the match
	// visible — and a non-streaming request must be promoted to a stream so the
	// generation can be cut off rather than run to max_tokens.
	reqs := eng.requests()
	if len(reqs) != 1 {
		t.Fatalf("upstream calls = %d, want 1", len(reqs))
	}
	if strings.Contains(reqs[0], `"stop"`) {
		t.Errorf("stop set was forwarded to the engine: %s", reqs[0])
	}
	if !strings.Contains(reqs[0], `"stream":true`) {
		t.Errorf("upstream call was not promoted to a stream: %s", reqs[0])
	}
}

func TestMessagesStopSequenceStreaming(t *testing.T) {
	eng := &recordingEngine{frames: splitStopFrames}
	h := stopSeqServer(t, eng, nil)

	w := postMessages(h, `{"model":"m1","max_tokens":64,"stream":true,"stop_sequences":["[[STOP]]"],
		"messages":[{"role":"user","content":"hi"}]}`)
	text, final := sseText(t, w.Body.String())

	if text != "Hello world" {
		t.Errorf("streamed text = %q, want %q", text, "Hello world")
	}
	if final["stop_reason"] != "stop_sequence" {
		t.Errorf("stop_reason = %v, want stop_sequence", final["stop_reason"])
	}
	if final["stop_sequence"] != "[[STOP]]" {
		t.Errorf("stop_sequence = %v, want [[STOP]]", final["stop_sequence"])
	}
	if !strings.Contains(w.Body.String(), "event: message_stop") {
		t.Error("a stopped stream must still be closed with message_stop")
	}
	if strings.Contains(w.Body.String(), "event: error") {
		t.Error("our own stop must not be reported as a broken stream")
	}
	if reqs := eng.requests(); strings.Contains(reqs[0], `"stop"`) {
		t.Errorf("stop set was forwarded to the engine: %s", reqs[0])
	}
}

// Text held back as a possible partial match is real output. Dropping it would
// silently truncate the answer — the exact failure mode this server exists to
// prevent.
func TestMessagesStopSequencePartialTailIsNotLost(t *testing.T) {
	frames := []string{contentFrame("answer: "), contentFrame("42[[ST")}

	t.Run("non-streaming", func(t *testing.T) {
		h := stopSeqServer(t, &recordingEngine{frames: frames}, nil)
		w := postMessages(h, `{"model":"m1","max_tokens":64,"stop_sequences":["[[STOP]]"],
			"messages":[{"role":"user","content":"hi"}]}`)
		text, resp := messageText(t, w)
		if text != "answer: 42[[ST" {
			t.Errorf("content = %q, want %q", text, "answer: 42[[ST")
		}
		if resp["stop_reason"] != "end_turn" || resp["stop_sequence"] != nil {
			t.Errorf("no sequence matched, got stop_reason=%v stop_sequence=%v",
				resp["stop_reason"], resp["stop_sequence"])
		}
	})

	t.Run("streaming", func(t *testing.T) {
		h := stopSeqServer(t, &recordingEngine{frames: frames}, nil)
		w := postMessages(h, `{"model":"m1","max_tokens":64,"stream":true,"stop_sequences":["[[STOP]]"],
			"messages":[{"role":"user","content":"hi"}]}`)
		text, final := sseText(t, w.Body.String())
		if text != "answer: 42[[ST" {
			t.Errorf("streamed text = %q, want %q", text, "answer: 42[[ST")
		}
		if final["stop_reason"] != "end_turn" {
			t.Errorf("stop_reason = %v, want end_turn", final["stop_reason"])
		}
	})
}

// A request with no stop sequences must be untouched: same non-streaming upstream
// call it always made, same exact usage from the JSON body.
func TestMessagesWithoutStopSequencesIsUnchanged(t *testing.T) {
	eng := &recordingEngine{
		json: `{"choices":[{"message":{"role":"assistant","content":"Hello"},"finish_reason":"stop"}],
		        "usage":{"prompt_tokens":7,"completion_tokens":2}}`,
	}
	h := stopSeqServer(t, eng, nil)

	w := postMessages(h, `{"model":"m1","max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`)
	text, resp := messageText(t, w)

	if text != "Hello" {
		t.Errorf("content = %q, want Hello", text)
	}
	if resp["stop_reason"] != "end_turn" || resp["stop_sequence"] != nil {
		t.Errorf("stop_reason=%v stop_sequence=%v", resp["stop_reason"], resp["stop_sequence"])
	}
	if got := w.Header().Get("X-Mainspring-Tokens-Input"); got != "7" {
		t.Errorf("exact input tokens header = %q, want 7", got)
	}
	if reqs := eng.requests(); !strings.Contains(reqs[0], `"stream":false`) {
		t.Errorf("upstream call should not have been promoted to a stream: %s", reqs[0])
	}
}

// A stop set that never matches must not swallow the upstream's own reason.
func TestMessagesStopSequenceKeepsUpstreamFinishReason(t *testing.T) {
	eng := &recordingEngine{frames: []string{
		contentFrame("truncated"),
		`{"choices":[{"delta":{},"finish_reason":"length"}]}`,
	}}
	h := stopSeqServer(t, eng, nil)

	w := postMessages(h, `{"model":"m1","max_tokens":2,"stop_sequences":["[[STOP]]"],
		"messages":[{"role":"user","content":"hi"}]}`)
	_, resp := messageText(t, w)

	if resp["stop_reason"] != "max_tokens" {
		t.Errorf("stop_reason = %v, want max_tokens", resp["stop_reason"])
	}
	if resp["stop_sequence"] != nil {
		t.Errorf("stop_sequence = %v, want null", resp["stop_sequence"])
	}
}

// Tool calls arrive as indexed fragments, so the internally-streamed path has to
// reassemble them into the whole-message shape a non-streaming reply needs.
func TestMessagesStopSequenceReassemblesToolCalls(t *testing.T) {
	eng := &recordingEngine{frames: []string{
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"get_weather","arguments":"{\"city\":"}}]}}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"paris\"}"}}]},"finish_reason":"tool_calls"}]}`,
	}}
	h := stopSeqServer(t, eng, nil)

	w := postMessages(h, `{"model":"m1","max_tokens":64,"stop_sequences":["[[STOP]]"],
		"tools":[{"name":"get_weather","input_schema":{"type":"object"}}],
		"messages":[{"role":"user","content":"weather in paris?"}]}`)
	_, resp := messageText(t, w)

	if resp["stop_reason"] != "tool_use" {
		t.Errorf("stop_reason = %v, want tool_use", resp["stop_reason"])
	}
	blocks := resp["content"].([]any)
	if len(blocks) != 1 {
		t.Fatalf("content blocks = %d, want 1: %v", len(blocks), blocks)
	}
	b := blocks[0].(map[string]any)
	if b["type"] != "tool_use" || b["id"] != "call_1" || b["name"] != "get_weather" {
		t.Fatalf("unexpected tool block: %v", b)
	}
	input, _ := b["input"].(map[string]any)
	if input["city"] != "paris" {
		t.Errorf("tool input = %v, want city=paris (arguments were not reassembled)", input)
	}
}

// The stop set stays on the body the cache key is computed over, so two requests
// that differ only in their stop sequences are different requests — while an
// identical repeat is still served from the cache.
func TestMessagesStopSequenceIsPartOfTheCacheKey(t *testing.T) {
	eng := &recordingEngine{frames: splitStopFrames}
	h := stopSeqServer(t, eng, func(s *server.Server) { s.SetCache(time.Minute, 16) })

	body := func(seq string) string {
		return `{"model":"m1","max_tokens":64,"temperature":0,"stop_sequences":["` + seq + `"],
			"messages":[{"role":"user","content":"hi"}]}`
	}
	if w := postMessages(h, body("[[STOP]]")); w.Code != 200 {
		t.Fatalf("first: status=%d %s", w.Code, w.Body.String())
	}
	if w := postMessages(h, body("[[OTHER]]")); w.Code != 200 {
		t.Fatalf("second: status=%d %s", w.Code, w.Body.String())
	}
	if n := len(eng.requests()); n != 2 {
		t.Fatalf("upstream calls = %d, want 2 — different stop sequences collided in the cache", n)
	}

	w := postMessages(h, body("[[STOP]]"))
	if got := w.Header().Get("X-Mainspring-Cache"); got != "hit" {
		t.Errorf("repeat of the first request: X-Mainspring-Cache = %q, want hit", got)
	}
	if n := len(eng.requests()); n != 2 {
		t.Errorf("upstream calls = %d after a repeat, want 2", n)
	}
}
