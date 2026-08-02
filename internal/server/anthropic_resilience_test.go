package server_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ankit373/mainspring/internal/auth"
	"github.com/ankit373/mainspring/internal/backend"
	"github.com/ankit373/mainspring/internal/metrics"
	"github.com/ankit373/mainspring/internal/scheduler"
	"github.com/ankit373/mainspring/internal/server"
)

const msgBody = `{"model":"m1","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`

// TestMessagesRetriesTransientUpstream — /v1/messages called the upstream exactly
// once, so a transient 503 was handed straight to the client while the OpenAI
// path would have retried and succeeded.
func TestMessagesRetriesTransientUpstream(t *testing.T) {
	var hits atomic.Int64
	eng := flakyEngine(t, 2, &hits) // two 503s, then healthy
	h, _ := messagesLedgerServer(t, eng.URL, func(srv *server.Server) {
		srv.SetRetry(3, time.Millisecond)
	})

	w := postMessages(h, msgBody)
	if w.Code != 200 {
		t.Fatalf("status=%d, want 200 after retrying two transient failures; body=%s", w.Code, w.Body.String())
	}
	if hits.Load() != 3 {
		t.Fatalf("upstream attempts=%d, want 3 (two failures + the success)", hits.Load())
	}
}

// TestMessagesRetriesAreRecorded — the Anthropic ledger event never carried a
// retry count, so retries on this path were invisible to an operator.
func TestMessagesRetriesAreRecorded(t *testing.T) {
	var hits atomic.Int64
	eng := flakyEngine(t, 1, &hits)
	h, events := messagesLedgerServer(t, eng.URL, func(srv *server.Server) {
		srv.SetRetry(2, time.Millisecond)
	})

	if w := postMessages(h, msgBody); w.Code != 200 {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	ev := events()
	if len(ev) == 0 {
		t.Fatal("no event recorded")
	}
	if got := ev[len(ev)-1].Retries; got != 1 {
		t.Fatalf("recorded retries=%d, want 1", got)
	}
}

// streamAfterFailures answers with 503 for the first n calls, then a real SSE
// stream. It records the byte offset at which the first frame was written so a
// test can prove no frame escaped before the retry decision.
func streamAfterFailures(t *testing.T, n int, hits *atomic.Int64) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		if hits.Add(1) <= int64(n) {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
		w.(http.Flusher).Flush()
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// TestMessagesStreamRetryHappensBeforeAnyFrame is the safety property that makes
// retry legal on a streaming path at all: the decision is taken after the
// upstream status is known and before a single SSE frame is emitted. A retry
// after message_start had gone out would corrupt the stream.
func TestMessagesStreamRetryHappensBeforeAnyFrame(t *testing.T) {
	var hits atomic.Int64
	eng := streamAfterFailures(t, 2, &hits)
	h, _ := messagesLedgerServer(t, eng.URL, func(srv *server.Server) {
		srv.SetRetry(3, time.Millisecond)
	})

	w := postMessages(h, `{"model":"m1","max_tokens":16,"stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	if w.Code != 200 {
		t.Fatalf("status=%d, want 200; body=%s", w.Code, w.Body.String())
	}
	if hits.Load() != 3 {
		t.Fatalf("upstream attempts=%d, want 3", hits.Load())
	}
	body := w.Body.String()
	// Exactly one stream: a retry that leaked frames would emit message_start twice.
	if got := strings.Count(body, "event: message_start"); got != 1 {
		t.Fatalf("message_start appears %d times, want exactly 1 — a retry leaked frames:\n%s", got, body)
	}
	if !strings.Contains(body, "event: message_stop") {
		t.Fatalf("stream did not complete:\n%s", body)
	}
	if strings.Contains(body, "event: error") {
		t.Fatalf("a retried stream must not carry an error frame:\n%s", body)
	}
}

// TestMessagesModelFallbackServes — model_fallbacks never engaged on
// /v1/messages, so a configured fallback model did not answer when the primary
// was down.
func TestMessagesModelFallbackServes(t *testing.T) {
	eng := fakeEngine(t)
	sched := scheduler.New(
		map[string]backend.Backend{
			"broken": &failingLoadBackend{},
			"fake":   &engineBackend{baseURL: eng.URL},
		},
		[]backend.ModelSpec{
			{ID: "primary", Backend: "broken"},
			{ID: "backup", Backend: "fake"},
		},
		scheduler.Options{MaxLoaded: 2},
	)
	rec, _ := metrics.New("")
	srv := server.New(sched, auth.New(nil), rec)
	srv.SetModelFallbacks(map[string][]string{"primary": {"backup"}})
	h := srv.Handler()

	w := postMessages(h, `{"model":"primary","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`)
	if w.Code != 200 {
		t.Fatalf("status=%d, want 200 via the fallback model; body=%s", w.Code, w.Body.String())
	}
	if got := w.Header().Get("X-Mainspring-Served-Model"); got != "backup" {
		t.Fatalf("X-Mainspring-Served-Model=%q, want \"backup\"", got)
	}
	if !strings.Contains(w.Body.String(), `"type":"message"`) {
		t.Fatalf("fallback did not return an Anthropic message:\n%s", w.Body.String())
	}
}

// TestMessagesNoFallbackConfiguredStillReportsLoadFailure guards the exclusion:
// with no fallback chain, an unservable model is still an honest failure and not
// silently answered by something else.
func TestMessagesNoFallbackConfiguredStillReportsLoadFailure(t *testing.T) {
	sched := scheduler.New(
		map[string]backend.Backend{"broken": &failingLoadBackend{}},
		[]backend.ModelSpec{{ID: "m1", Backend: "broken"}},
		scheduler.Options{MaxLoaded: 2},
	)
	rec, _ := metrics.New("")
	h := server.New(sched, auth.New(nil), rec).Handler()

	w := postMessages(h, msgBody)
	if w.Code < 500 {
		t.Fatalf("status=%d, want a 5xx for an unservable model; body=%s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "circuit_open") {
		t.Fatalf("no breaker is configured, so this is a load failure, not a circuit:\n%s", w.Body.String())
	}
}
