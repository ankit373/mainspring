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

// countingEngine records how many upstream requests it actually served, so a
// cache hit can be proven by the count staying flat.
func countingEngine(t *testing.T, hits *atomic.Int64) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"Hello"}}],"usage":{"prompt_tokens":3,"completion_tokens":2}}`)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func cachedServer(t *testing.T, baseURL string) http.Handler {
	t.Helper()
	sched := scheduler.New(
		map[string]backend.Backend{"fake": &engineBackend{baseURL: baseURL}},
		[]backend.ModelSpec{{ID: "m1", Backend: "fake"}},
		scheduler.Options{MaxLoaded: 2},
	)
	rec, _ := metrics.New("")
	srv := server.New(sched, auth.New(nil), rec)
	srv.SetCache(time.Minute, 16)
	return srv.Handler()
}

func postBody(h http.Handler, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

func TestCacheHitSkipsBackend(t *testing.T) {
	var hits atomic.Int64
	eng := countingEngine(t, &hits)
	h := cachedServer(t, eng.URL)

	const body = `{"model":"m1","temperature":0,"messages":[{"role":"user","content":"hi"}]}`

	w1 := postBody(h, body)
	if w1.Code != 200 || w1.Header().Get("X-Mainspring-Cache") == "hit" {
		t.Fatalf("first request should be a miss: code=%d cache=%q", w1.Code, w1.Header().Get("X-Mainspring-Cache"))
	}
	// Reordered keys + whitespace: must still hit.
	w2 := postBody(h, `{ "messages":[{"role":"user","content":"hi"}], "temperature":0, "model":"m1" }`)
	if w2.Code != 200 || w2.Header().Get("X-Mainspring-Cache") != "hit" {
		t.Fatalf("second request should be a cache hit: code=%d cache=%q", w2.Code, w2.Header().Get("X-Mainspring-Cache"))
	}
	if w2.Body.String() != w1.Body.String() {
		t.Fatalf("cached body differs: %q vs %q", w2.Body.String(), w1.Body.String())
	}
	if w2.Header().Get("X-Mainspring-Backend") != "fake" {
		t.Fatal("cache hit should replay fail-loud headers")
	}
	if got := hits.Load(); got != 1 {
		t.Fatalf("backend hit %d times; cache should have served the second request", got)
	}
}

func TestCacheBypassStreamAndTemp(t *testing.T) {
	var hits atomic.Int64
	eng := countingEngine(t, &hits)
	h := cachedServer(t, eng.URL)

	// Positive temperature is non-deterministic → never cached.
	postBody(h, `{"model":"m1","temperature":0.7,"messages":[]}`)
	postBody(h, `{"model":"m1","temperature":0.7,"messages":[]}`)
	if got := hits.Load(); got != 2 {
		t.Fatalf("temp>0 must bypass cache; backend hits=%d want 2", got)
	}
}
