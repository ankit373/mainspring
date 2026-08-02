// Command fakeserver is a deterministic OpenAI/Ollama upstream used only by the
// conformance suite. It lets us run Mainspring exactly as shipped (the Ollama
// adopt path) against a scripted engine, so the real OpenAI and Anthropic SDKs
// exercise Mainspring's proxy/translation/streaming/error surface without a
// model download.
//
// It intentionally emits the CORRECT-but-easy-to-get-wrong OpenAI shapes:
// tool_call function.arguments as a JSON *string*, SSE framed as
// `data: {..}\n\n` ending with `data: [DONE]`, and a usage block.
//
// # Steering the upstream from a test
//
// A conformance script cannot reach this process directly — it only talks to
// Mainspring. So the behaviour of a single request is selected by a marker
// embedded in the prompt text, which both dialects carry through untouched into
// `messages[].content`:
//
//	[[echo]]        reply with the exact OpenAI body this server received, as the
//	                assistant's text. Lets a test assert what the Anthropic →
//	                OpenAI translation actually put on the wire (system prompt,
//	                top_k, stop, image parts, role:tool messages …), which the
//	                client-visible response alone cannot show.
//	[[status:NNN]]  reply with HTTP NNN and an error body, before any SSE byte.
//	[[truncate]]    streaming only: emit a few deltas, then abort the connection
//	                mid-body with no terminating chunk and no [DONE].
//	[[honour-n]]    honour the request's `n`: answer with that many independent
//	                choices (distinct `index` values, one per candidate). Without
//	                it this server *ignores* `n` and answers with one choice — what
//	                a real engine that never implemented `n` does, and what
//	                Mainspring has to detect by counting rather than by consulting
//	                a per-backend table.
//
// No marker means the default scripted behaviour, so the OpenAI scripts are
// unaffected.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// maxBody bounds how much of a request we buffer (echo returns it verbatim).
const maxBody = 8 << 20

const (
	dirEcho     = "[[echo]]"
	dirTruncate = "[[truncate]]"
	dirStatus   = "[[status:" // [[status:503]]
	dirHonourN  = "[[honour-n]]"
)

func main() {
	addr := flag.String("addr", ":18080", "listen address")
	flag.Parse()

	mux := http.NewServeMux()

	// ── Ollama discovery surface (so Mainspring's ollama backend adopts us) ──
	mux.HandleFunc("/api/version", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, map[string]string{"version": "9.9.9-fake"})
	})
	mux.HandleFunc("/api/tags", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, map[string]any{"models": []map[string]string{{"name": "mock-model:latest"}}})
	})
	mux.HandleFunc("/api/show", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, map[string]any{"model_info": map[string]any{"fake.context_length": 8192}})
	})
	mux.HandleFunc("/api/ps", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, map[string]any{"models": []any{}})
	})

	// ── OpenAI-compatible surface ──
	mux.HandleFunc("/v1/models", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, map[string]any{
			"object": "list",
			"data":   []map[string]any{{"id": "mock-model", "object": "model", "owned_by": "fake"}},
		})
	})
	mux.HandleFunc("/v1/chat/completions", chatCompletions)
	mux.HandleFunc("/v1/embeddings", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, map[string]any{
			"object": "list",
			"data":   []map[string]any{{"object": "embedding", "index": 0, "embedding": []float64{0.1, 0.2, 0.3}}},
			"model":  "mock-model",
			"usage":  map[string]int{"prompt_tokens": 3, "total_tokens": 3},
		})
	})

	log.Printf("fakeserver listening on %s", *addr)
	log.Fatal(http.ListenAndServe(*addr, mux))
}

// oaiRequest is the slice of the incoming OpenAI body this server steers on.
type oaiRequest struct {
	Stream        bool `json:"stream"`
	N             int  `json:"n"`
	StreamOptions *struct {
		IncludeUsage bool `json:"include_usage"`
	} `json:"stream_options"`
	Tools    []json.RawMessage `json:"tools"`
	Messages []struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	} `json:"messages"`
}

func chatCompletions(w http.ResponseWriter, r *http.Request) {
	raw, err := io.ReadAll(io.LimitReader(r.Body, maxBody))
	if err != nil {
		writeStatus(w, http.StatusBadRequest, "read body: "+err.Error())
		return
	}
	var req oaiRequest
	_ = json.Unmarshal(raw, &req)
	hasTools := len(req.Tools) > 0
	prompt := promptText(req)

	if code, ok := statusDirective(prompt); ok {
		writeStatus(w, code, fmt.Sprintf("fakeserver was asked for status %d", code))
		return
	}
	// Echo wins over the tool-call script: a test that sends tools *and* asks to
	// echo wants to inspect the translated request, not a canned tool call.
	echo := strings.Contains(prompt, dirEcho)

	if req.Stream {
		streamChat(w, script{
			tools:    hasTools && !echo,
			echo:     echo,
			truncate: strings.Contains(prompt, dirTruncate),
			usage:    req.StreamOptions != nil && req.StreamOptions.IncludeUsage,
			choices:  choicesFor(req, prompt),
			raw:      raw,
		})
		return
	}

	if n := choicesFor(req, prompt); n > 1 {
		writeJSON(w, map[string]any{
			"id": "chatcmpl-fake", "object": "chat.completion", "created": 0, "model": "mock-model",
			"choices": candidates(n),
			"usage":   map[string]int{"prompt_tokens": 5, "completion_tokens": 4 * n, "total_tokens": 5 + 4*n},
		})
		return
	}

	msg := map[string]any{"role": "assistant", "content": "Hello from fakeserver."}
	finish := "stop"
	switch {
	case echo:
		msg["content"] = string(raw)
	case hasTools:
		// OpenAI shape: function.arguments is a JSON-encoded STRING, not an object.
		msg["content"] = nil
		msg["tool_calls"] = []map[string]any{{
			"id":   "call_1",
			"type": "function",
			"function": map[string]any{
				"name":      "get_weather",
				"arguments": `{"location":"San Francisco"}`,
			},
		}}
		finish = "tool_calls"
	}
	writeJSON(w, map[string]any{
		"id":      "chatcmpl-fake",
		"object":  "chat.completion",
		"created": 0,
		"model":   "mock-model",
		"choices": []map[string]any{{"index": 0, "message": msg, "finish_reason": finish}},
		"usage":   map[string]int{"prompt_tokens": 5, "completion_tokens": 4, "total_tokens": 9},
	})
}

// promptText concatenates every text fragment in the request's messages, so a
// directive is found whether the client sent a plain string or content parts.
func promptText(req oaiRequest) string {
	var b strings.Builder
	for _, m := range req.Messages {
		var s string
		if json.Unmarshal(m.Content, &s) == nil {
			b.WriteString(s)
			continue
		}
		var parts []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		if json.Unmarshal(m.Content, &parts) == nil {
			for _, p := range parts {
				if p.Type == "text" || p.Type == "" {
					b.WriteString(p.Text)
				}
			}
		}
	}
	return b.String()
}

// choicesFor returns how many choices this request is answered with: the
// requested `n` when [[honour-n]] asked us to implement it, and 1 otherwise —
// including when the caller sent n>1, which is exactly the silent denial
// Mainspring has to notice.
func choicesFor(req oaiRequest, prompt string) int {
	if req.N > 1 && strings.Contains(prompt, dirHonourN) {
		return req.N
	}
	return 1
}

// candidates builds n independent non-streaming choices, each with its own index.
func candidates(n int) []map[string]any {
	out := make([]map[string]any, n)
	for i := range out {
		out[i] = map[string]any{
			"index":         i,
			"message":       map[string]any{"role": "assistant", "content": fmt.Sprintf("Candidate %d from fakeserver.", i)},
			"finish_reason": "stop",
		}
	}
	return out
}

// statusDirective returns the HTTP status a [[status:NNN]] marker asks for.
func statusDirective(prompt string) (int, bool) {
	_, rest, ok := strings.Cut(prompt, dirStatus)
	if !ok {
		return 0, false
	}
	num, _, ok := strings.Cut(rest, "]]")
	if !ok {
		return 0, false
	}
	code, err := strconv.Atoi(strings.TrimSpace(num))
	if err != nil || code < 100 || code > 599 {
		return 0, false
	}
	return code, true
}

// script is what one request asked this upstream to do while streaming.
type script struct {
	tools    bool // emit the canned tool call
	echo     bool // reply with the body we received
	truncate bool // abort the connection mid-stream
	usage    bool // append a stream_options.include_usage chunk
	choices  int  // distinct choice indices to emit (1 unless [[honour-n]])
	raw      []byte
}

func streamChat(w http.ResponseWriter, sc script) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	fl, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	send := func(v any) {
		b, _ := json.Marshal(v)
		fmt.Fprintf(w, "data: %s\n\n", b)
		fl.Flush()
		time.Sleep(2 * time.Millisecond)
	}
	// at emits a chunk for choice index idx; base is the index-0 shorthand every
	// script but the multi-candidate one needs.
	at := func(idx int, delta map[string]any, finish any) map[string]any {
		return map[string]any{
			"id": "chatcmpl-fake", "object": "chat.completion.chunk", "model": "mock-model",
			"choices": []map[string]any{{"index": idx, "delta": delta, "finish_reason": finish}},
		}
	}
	base := func(delta map[string]any, finish any) map[string]any { return at(0, delta, finish) }
	send(base(map[string]any{"role": "assistant"}, nil))

	if sc.truncate {
		// Emit real content, then die mid-body: no finish_reason, no [DONE] and no
		// terminating chunk, so the reader sees an unexpected EOF. This is the
		// abnormal end a translating proxy must not paper over with a stop event.
		send(base(map[string]any{"content": "partial "}, nil))
		send(base(map[string]any{"content": "answer "}, nil))
		panic(http.ErrAbortHandler)
	}

	switch {
	case sc.echo:
		send(base(map[string]any{"content": string(sc.raw)}, nil))
		send(base(map[string]any{}, "stop"))
	case sc.tools:
		// Text first, then a tool call: a real engine narrates before it calls, and
		// the translation layer has to close the text block and allocate a second
		// content-block index for the tool_use rather than reusing index 0.
		send(base(map[string]any{"content": "Checking. "}, nil))
		// Arguments arrive split across frames, as a real engine emits them: the
		// translation layer must stitch them into one tool_use block.
		send(base(map[string]any{"tool_calls": []map[string]any{{
			"index": 0, "id": "call_1", "type": "function",
			"function": map[string]any{"name": "get_weather", "arguments": `{"location":`},
		}}}, nil))
		send(base(map[string]any{"tool_calls": []map[string]any{{
			"index": 0, "function": map[string]any{"arguments": `"SF"}`},
		}}}, nil))
		send(base(map[string]any{}, "tool_calls"))
	default:
		// One candidate per requested choice index. sc.choices is 1 unless
		// [[honour-n]] asked for `n`, so an engine that ignores `n` emits index 0
		// only — which is what the caller must be told about.
		for idx := 0; idx < sc.choices; idx++ {
			for _, tok := range strings.Fields("Hello from fakeserver .") {
				send(at(idx, map[string]any{"content": tok + " "}, nil))
			}
			send(at(idx, map[string]any{}, "stop"))
		}
	}

	// stream_options.include_usage: a final choices-less chunk carrying real token
	// counts. Mainspring always asks for it on the Anthropic path, and it is what
	// lets the terminal message_delta report exact usage instead of a frame count.
	if sc.usage {
		send(map[string]any{
			"id": "chatcmpl-fake", "object": "chat.completion.chunk", "model": "mock-model",
			"choices": []any{},
			"usage":   map[string]int{"prompt_tokens": 7, "completion_tokens": 13, "total_tokens": 20},
		})
	}
	fmt.Fprint(w, "data: [DONE]\n\n")
	fl.Flush()
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// writeStatus emits an OpenAI-shaped error body with an explicit status.
func writeStatus(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]any{"message": msg, "type": "fakeserver_error", "code": code},
	})
}
