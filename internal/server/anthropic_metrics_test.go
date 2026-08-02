package server_test

import (
	"bufio"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ankit373/mainspring/internal/auth"
	"github.com/ankit373/mainspring/internal/backend"
	"github.com/ankit373/mainspring/internal/metrics"
	"github.com/ankit373/mainspring/internal/scheduler"
	"github.com/ankit373/mainspring/internal/server"
)

// msgEvent is the subset of a usage-ledger event the /v1/messages metrics tests
// assert on — the ledger is the only place the recorded Event is observable.
type msgEvent struct {
	Model        string  `json:"model"`
	Status       int     `json:"status"`
	Stream       bool    `json:"stream"`
	TTFTMs       float64 `json:"ttft_ms"`
	Bytes        int64   `json:"bytes"`
	PromptTokens int64   `json:"prompt_tokens"`
	TokensEst    int64   `json:"tokens_est"`
	CostUSD      float64 `json:"cost_usd"`
	Retries      int     `json:"retries"`
	Fallback     bool    `json:"fallback"`
}

// messagesLedgerServer builds a /v1/messages handler over baseURL with a JSONL
// usage ledger, plus a reader for the events recorded so far.
func messagesLedgerServer(t *testing.T, baseURL string, configure func(*server.Server)) (http.Handler, func() []msgEvent) {
	t.Helper()
	sched := scheduler.New(
		map[string]backend.Backend{"fake": &engineBackend{baseURL: baseURL}},
		[]backend.ModelSpec{{ID: "m1", Backend: "fake"}},
		scheduler.Options{MaxLoaded: 2},
	)
	path := filepath.Join(t.TempDir(), "usage.jsonl")
	rec, err := metrics.New(path)
	if err != nil {
		t.Fatalf("metrics ledger: %v", err)
	}
	t.Cleanup(func() { _ = rec.Close() })
	srv := server.New(sched, auth.New(nil), rec)
	if configure != nil {
		configure(srv)
	}
	return srv.Handler(), func() []msgEvent {
		f, err := os.Open(path)
		if err != nil {
			t.Fatalf("open ledger: %v", err)
		}
		defer f.Close()
		var out []msgEvent
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			line := strings.TrimSpace(sc.Text())
			if line == "" {
				continue
			}
			var ev msgEvent
			if err := json.Unmarshal([]byte(line), &ev); err != nil {
				t.Fatalf("decode ledger line %q: %v", line, err)
			}
			out = append(out, ev)
		}
		return out
	}
}

func postMessages(h http.Handler, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

// onlyEvent returns the single recorded event, failing if there isn't exactly one.
func onlyEvent(t *testing.T, events []msgEvent) msgEvent {
	t.Helper()
	if len(events) != 1 {
		t.Fatalf("recorded %d events, want 1: %+v", len(events), events)
	}
	return events[0]
}

// usageEngine reports a real usage object on both paths: 12/4 for the
// non-streaming body and 15/3 in the streaming include_usage final chunk.
func usageEngine(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if strings.Contains(string(body), `"stream":true`) {
			w.Header().Set("Content-Type", "text/event-stream")
			fl := w.(http.Flusher)
			_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"Hello\"}}]}\n\n")
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
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// priced installs a cost rate for m1 (USD per million tokens).
func priced(srv *server.Server) {
	srv.SetCostRates(map[string]server.CostRate{"m1": server.NewCostRate(3.0, 15.0)})
}

// TestMessagesRecordsBytesAndCostNonStream proves the recorded event for a
// non-streaming /v1/messages request carries the real status, byte count, token
// usage and charged cost — none of which used to be reported.
func TestMessagesRecordsBytesAndCostNonStream(t *testing.T) {
	eng := usageEngine(t)
	h, events := messagesLedgerServer(t, eng.URL, priced)

	if w := postMessages(h, `{"model":"m1","max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`); w.Code != 200 {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	ev := onlyEvent(t, events())
	if ev.Status != 200 {
		t.Fatalf("recorded status=%d, want 200", ev.Status)
	}
	if ev.Bytes <= 0 {
		t.Fatalf("recorded bytes=%d, want >0", ev.Bytes)
	}
	if ev.PromptTokens != 12 || ev.TokensEst != 4 {
		t.Fatalf("recorded tokens prompt=%d output=%d, want 12/4", ev.PromptTokens, ev.TokensEst)
	}
	want := 12.0/1e6*3.0 + 4.0/1e6*15.0
	if ev.CostUSD != want {
		t.Fatalf("recorded cost=%v, want %v", ev.CostUSD, want)
	}
}

// TestMessagesRecordsTTFTAndCostStream proves TTFT — the flagship metric, blank
// for the whole Anthropic API before — is recorded for a streaming request, along
// with bytes and cost.
func TestMessagesRecordsTTFTAndCostStream(t *testing.T) {
	eng := usageEngine(t)
	h, events := messagesLedgerServer(t, eng.URL, priced)

	if w := postMessages(h, `{"model":"m1","max_tokens":64,"stream":true,"messages":[{"role":"user","content":"hi"}]}`); w.Code != 200 {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	ev := onlyEvent(t, events())
	if ev.Status != 200 || !ev.Stream {
		t.Fatalf("recorded status=%d stream=%v, want 200/true", ev.Status, ev.Stream)
	}
	if ev.TTFTMs <= 0 {
		t.Fatalf("recorded ttft_ms=%v, want >0", ev.TTFTMs)
	}
	if ev.Bytes <= 0 {
		t.Fatalf("recorded bytes=%d, want >0", ev.Bytes)
	}
	want := 15.0/1e6*3.0 + 3.0/1e6*15.0
	if ev.CostUSD != want {
		t.Fatalf("recorded cost=%v, want %v", ev.CostUSD, want)
	}
}

// TestMessagesRecordsUpstreamFailureStatus proves an upstream 5xx is recorded with
// the status actually written (502), not the hardcoded 200 it used to report.
func TestMessagesRecordsUpstreamFailureStatus(t *testing.T) {
	eng := erroringEngine(t, http.StatusInternalServerError, `{"error":"engine exploded"}`)
	h, events := messagesLedgerServer(t, eng.URL, nil)

	w := postMessages(h, `{"model":"m1","max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`)
	if w.Code != http.StatusBadGateway {
		t.Fatalf("status=%d, want 502; body=%s", w.Code, w.Body.String())
	}
	if ev := onlyEvent(t, events()); ev.Status != http.StatusBadGateway {
		t.Fatalf("recorded status=%d, want 502", ev.Status)
	}
}

// TestMessagesRecordsTimeoutStatus proves a per-request timeout is recorded as the
// 504 it returns.
func TestMessagesRecordsTimeoutStatus(t *testing.T) {
	eng := stallingEngine(t, 500*time.Millisecond)
	h, events := messagesLedgerServer(t, eng.URL, func(srv *server.Server) {
		srv.SetTimeouts(30*time.Millisecond, nil)
	})

	w := postMessages(h, `{"model":"m1","max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`)
	if w.Code != http.StatusGatewayTimeout {
		t.Fatalf("status=%d, want 504; body=%s", w.Code, w.Body.String())
	}
	if ev := onlyEvent(t, events()); ev.Status != http.StatusGatewayTimeout {
		t.Fatalf("recorded status=%d, want 504", ev.Status)
	}
}
