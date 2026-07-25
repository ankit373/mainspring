// Command fakeserver is a deterministic OpenAI/Ollama upstream used only by the
// conformance suite. It lets us run Mainspring exactly as shipped (the Ollama
// adopt path) against a scripted engine, so the real OpenAI SDKs exercise
// Mainspring's proxy/streaming/error surface without a model download.
//
// It intentionally emits the CORRECT-but-easy-to-get-wrong OpenAI shapes:
// tool_call function.arguments as a JSON *string*, SSE framed as
// `data: {..}\n\n` ending with `data: [DONE]`, and a usage block.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"
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

func chatCompletions(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Stream bool              `json:"stream"`
		Tools  []json.RawMessage `json:"tools"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	hasTools := len(req.Tools) > 0

	if req.Stream {
		streamChat(w, hasTools)
		return
	}

	msg := map[string]any{"role": "assistant", "content": "Hello from fakeserver."}
	finish := "stop"
	if hasTools {
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

func streamChat(w http.ResponseWriter, hasTools bool) {
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
	base := func(delta map[string]any, finish any) map[string]any {
		return map[string]any{
			"id": "chatcmpl-fake", "object": "chat.completion.chunk", "model": "mock-model",
			"choices": []map[string]any{{"index": 0, "delta": delta, "finish_reason": finish}},
		}
	}
	send(base(map[string]any{"role": "assistant"}, nil))
	if hasTools {
		send(base(map[string]any{"tool_calls": []map[string]any{{
			"index": 0, "id": "call_1", "type": "function",
			"function": map[string]any{"name": "get_weather", "arguments": `{"location":"SF"}`},
		}}}, nil))
		send(base(map[string]any{}, "tool_calls"))
	} else {
		for _, tok := range strings.Fields("Hello from fakeserver .") {
			send(base(map[string]any{"content": tok + " "}, nil))
		}
		send(base(map[string]any{}, "stop"))
	}
	fmt.Fprint(w, "data: [DONE]\n\n")
	fl.Flush()
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
