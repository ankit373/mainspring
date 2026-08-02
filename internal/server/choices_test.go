package server_test

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// choiceEngine is an OpenAI-shaped upstream that either honours `n` or ignores it
// and answers with a single choice — the two engines the check has to tell apart.
// Whether `n` works is a property of the engine and its version, which is exactly
// why Mainspring counts the answer instead of consulting a table.
func choiceEngine(t *testing.T, honoursN bool) *httptest.Server {
	t.Helper()
	handler := func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var req struct {
			N      int  `json:"n"`
			Stream bool `json:"stream"`
		}
		_ = json.Unmarshal(raw, &req)
		got := 1
		if honoursN && req.N > 1 {
			got = req.N
		}
		if req.Stream {
			w.Header().Set("Content-Type", "text/event-stream")
			fl := w.(http.Flusher)
			for i := 0; i < got; i++ {
				fmt.Fprintf(w, "data: {\"choices\":[{\"index\":%d,\"delta\":{\"content\":\"c%d\"}}]}\n\n", i, i)
				fl.Flush()
			}
			_, _ = io.WriteString(w, "data: [DONE]\n\n")
			fl.Flush()
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, nonStreamBody(got))
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/chat/completions", handler)
	mux.HandleFunc("/v1/completions", handler)
	mux.HandleFunc("/v1/embeddings", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"object":"list","data":[{"object":"embedding","embedding":[0.1],"index":0}],"usage":{"prompt_tokens":2,"total_tokens":2}}`)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// nonStreamBody is the exact upstream body for n choices, so tests can assert the
// proxied body is byte-for-byte the engine's answer.
func nonStreamBody(n int) string {
	out := make([]string, n)
	for i := range out {
		out[i] = fmt.Sprintf(`{"index":%d,"message":{"role":"assistant","content":"c%d"}}`, i, i)
	}
	return `{"choices":[` + strings.Join(out, ",") + `]}`
}

func postJSON(h http.Handler, path, body string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodPost, path, strings.NewReader(body)))
	return w
}

func TestShortChoicesReportedNonStreaming(t *testing.T) {
	eng := choiceEngine(t, false)
	h := newTestServer(t, &engineBackend{baseURL: eng.URL}, nil)

	w := postJSON(h, "/v1/chat/completions", `{"model":"m1","n":3,"messages":[]}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	want := "n=3 requested but the engine returned 1 choice"
	if got := w.Header().Get("X-Mainspring-Warning"); got != want {
		t.Fatalf("shortfall must be reported:\n got %q\nwant %q", got, want)
	}
	// The body is the engine's answer, untouched — the warning is beside it, not in it.
	if got := w.Body.String(); got != nonStreamBody(1) {
		t.Fatalf("body must be byte-for-byte the engine's:\n got %q\nwant %q", got, nonStreamBody(1))
	}
}

// TestHonouredChoicesNotReported is the half that makes this observational rather
// than a table: the same request against an engine that *does* honour n carries no
// warning at all.
func TestHonouredChoicesNotReported(t *testing.T) {
	eng := choiceEngine(t, true)
	h := newTestServer(t, &engineBackend{baseURL: eng.URL}, nil)

	w := postJSON(h, "/v1/chat/completions", `{"model":"m1","n":3,"messages":[]}`)
	if got := w.Header().Get("X-Mainspring-Warning"); got != "" {
		t.Fatalf("a satisfied n must not be warned about, got %q", got)
	}
	if got := w.Body.String(); got != nonStreamBody(3) {
		t.Fatalf("all three choices must reach the client:\n got %q", got)
	}
}

func TestShortChoicesReportedInStream(t *testing.T) {
	eng := choiceEngine(t, false)
	h := newTestServer(t, &engineBackend{baseURL: eng.URL}, nil)

	w := postJSON(h, "/v1/chat/completions", `{"model":"m1","n":3,"stream":true,"messages":[]}`)
	body := w.Body.String()
	// The engine's frames are forwarded intact...
	if !strings.Contains(body, `"index":0`) || !strings.Contains(body, "data: [DONE]") {
		t.Fatalf("SSE stream not proxied intact: %q", body)
	}
	// ...and the shortfall arrives in-band, as an SSE comment (inert per the
	// grammar, because a stream's headers are long gone by the last frame).
	want := ": X-Mainspring-Warning: n=3 requested but the engine returned 1 choice\n\n"
	if !strings.Contains(body, want) {
		t.Fatalf("stream shortfall must be reported in-band:\nwant %q\n got %q", want, body)
	}
}

func TestHonouredChoicesNotReportedInStream(t *testing.T) {
	eng := choiceEngine(t, true)
	h := newTestServer(t, &engineBackend{baseURL: eng.URL}, nil)

	w := postJSON(h, "/v1/chat/completions", `{"model":"m1","n":3,"stream":true,"messages":[]}`)
	body := w.Body.String()
	for _, idx := range []string{`"index":0`, `"index":1`, `"index":2`} {
		if !strings.Contains(body, idx) {
			t.Fatalf("engine honoured n=3 but %s is missing: %q", idx, body)
		}
	}
	if strings.Contains(body, "X-Mainspring-Warning") {
		t.Fatalf("a satisfied n must not be warned about in-band: %q", body)
	}
}

// TestSingleChoiceRequestsUnchanged covers the acceptance criterion that an
// n-less or n:1 request is byte-for-byte what it was: no warning header and no
// injected frame, on either path. (That nothing *reads* the body on that path is
// proved separately, in TestCommitResponseBuffersOnlyForN.)
func TestSingleChoiceRequestsUnchanged(t *testing.T) {
	eng := choiceEngine(t, false)
	h := newTestServer(t, &engineBackend{baseURL: eng.URL}, nil)

	for _, body := range []string{
		`{"model":"m1","messages":[]}`,
		`{"model":"m1","n":1,"messages":[]}`,
	} {
		w := postJSON(h, "/v1/chat/completions", body)
		if got := w.Header().Get("X-Mainspring-Warning"); got != "" {
			t.Fatalf("%s: no warning expected, got %q", body, got)
		}
		if got := w.Body.String(); got != nonStreamBody(1) {
			t.Fatalf("%s: body altered: %q", body, got)
		}

		sw := postJSON(h, "/v1/chat/completions", strings.Replace(body, `"model"`, `"stream":true,"model"`, 1))
		if strings.Contains(sw.Body.String(), "X-Mainspring-Warning") {
			t.Fatalf("%s (stream): nothing may be injected: %q", body, sw.Body.String())
		}
	}
}

// TestShortChoicesComposesWithDegradedWarning: a CPU fallback and a short `n` can
// both be true of one response, and the header has to say both.
func TestShortChoicesComposesWithDegradedWarning(t *testing.T) {
	eng := choiceEngine(t, false)
	h := newTestServer(t, &engineBackend{baseURL: eng.URL, degraded: true}, nil)

	w := postJSON(h, "/v1/chat/completions", `{"model":"m1","n":2,"messages":[]}`)
	got := w.Header().Get("X-Mainspring-Warning")
	if !strings.Contains(got, "CPU") {
		t.Fatalf("the degraded-device warning must survive: %q", got)
	}
	if !strings.Contains(got, "n=2 requested but the engine returned 1 choice") {
		t.Fatalf("the shortfall must be appended: %q", got)
	}
}

func TestLegacyCompletionsAndEmbeddingsWithN(t *testing.T) {
	eng := choiceEngine(t, false)
	h := newTestServer(t, &engineBackend{baseURL: eng.URL}, nil)

	// Legacy /v1/completions has `n` and a choices array: same check.
	w := postJSON(h, "/v1/completions", `{"model":"m1","n":2,"prompt":"hi"}`)
	if got := w.Header().Get("X-Mainspring-Warning"); !strings.Contains(got, "n=2 requested") {
		t.Fatalf("/v1/completions should report the shortfall, got %q", got)
	}

	// /v1/embeddings has no `n` and no choices — there is nothing to observe, so
	// a stray field must not manufacture a warning out of `data`.
	ew := postJSON(h, "/v1/embeddings", `{"model":"m1","n":3,"input":"hi"}`)
	if got := ew.Header().Get("X-Mainspring-Warning"); got != "" {
		t.Fatalf("embeddings must not be warned about, got %q", got)
	}
}

// TestShortChoicesOnUpstreamErrorNotReported: a non-2xx body is an error object,
// not an answer with choices, so it must not be counted.
func TestShortChoicesOnUpstreamErrorNotReported(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"error":{"message":"bad n"}}`)
	})
	eng := httptest.NewServer(mux)
	t.Cleanup(eng.Close)
	h := newTestServer(t, &engineBackend{baseURL: eng.URL}, nil)

	w := postJSON(h, "/v1/chat/completions", `{"model":"m1","n":3,"messages":[]}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status=%d, want the upstream 400", w.Code)
	}
	if got := w.Header().Get("X-Mainspring-Warning"); got != "" {
		t.Fatalf("an error response has no choices to be short of, got %q", got)
	}
}
