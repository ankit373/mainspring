package server_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ankit373/mainspring/internal/auth"
	"github.com/ankit373/mainspring/internal/backend"
	"github.com/ankit373/mainspring/internal/metrics"
	"github.com/ankit373/mainspring/internal/scheduler"
	"github.com/ankit373/mainspring/internal/server"
)

// flakyEngine returns `failFirst` retryable 503s (counting every hit) before
// serving a 200. A negative failFirst fails forever.
func flakyEngine(t *testing.T, failFirst int, hits *atomic.Int64) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		n := hits.Add(1)
		if failFirst < 0 || n <= int64(failFirst) {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = io.WriteString(w, `{"error":"busy"}`)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"ok"}}]}`)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func retryServer(t *testing.T, baseURL string, maxRetries int) http.Handler {
	t.Helper()
	sched := scheduler.New(
		map[string]backend.Backend{"fake": &engineBackend{baseURL: baseURL}},
		[]backend.ModelSpec{{ID: "m1", Backend: "fake"}},
		scheduler.Options{MaxLoaded: 2},
	)
	rec, _ := metrics.New("")
	srv := server.New(sched, auth.New(nil), rec)
	srv.SetRetry(maxRetries, time.Millisecond)
	return srv.Handler()
}

func TestRetrySucceedsAfterTransient(t *testing.T) {
	var hits atomic.Int64
	eng := flakyEngine(t, 1, &hits) // fail once, then succeed
	h := retryServer(t, eng.URL, 2)

	w := postBody(h, `{"model":"m1","messages":[]}`)
	if w.Code != 200 {
		t.Fatalf("status = %d, want 200 after retry; body=%s", w.Code, w.Body.String())
	}
	if w.Header().Get("X-Mainspring-Retries") != "1" {
		t.Fatalf("X-Mainspring-Retries = %q, want 1", w.Header().Get("X-Mainspring-Retries"))
	}
	if hits.Load() != 2 {
		t.Fatalf("backend hits = %d, want 2 (1 fail + 1 success)", hits.Load())
	}
}

func TestRetryExhaustedReturnsUpstreamStatus(t *testing.T) {
	var hits atomic.Int64
	eng := flakyEngine(t, -1, &hits) // always fails
	h := retryServer(t, eng.URL, 1)

	w := postBody(h, `{"model":"m1","messages":[]}`)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 after exhausting retries", w.Code)
	}
	if hits.Load() != 2 {
		t.Fatalf("backend hits = %d, want 2 (1 + 1 retry)", hits.Load())
	}
}

func TestRetryDisabledSingleAttempt(t *testing.T) {
	var hits atomic.Int64
	eng := flakyEngine(t, -1, &hits)
	h := retryServer(t, eng.URL, 0) // retry disabled

	w := postBody(h, `{"model":"m1","messages":[]}`)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", w.Code)
	}
	if hits.Load() != 1 {
		t.Fatalf("backend hits = %d, want 1 (no retry)", hits.Load())
	}
	if w.Header().Get("X-Mainspring-Retries") != "" {
		t.Fatal("no retry occurred; X-Mainspring-Retries should be absent")
	}
}

// A non-retryable status (400) must pass straight through without retrying.
func TestRetrySkipsNonRetryable(t *testing.T) {
	var hits atomic.Int64
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusBadRequest)
	})
	eng := httptest.NewServer(mux)
	t.Cleanup(eng.Close)
	h := retryServer(t, eng.URL, 3)

	w := postBody(h, `{"model":"m1","messages":[]}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 passed through", w.Code)
	}
	if hits.Load() != 1 {
		t.Fatalf("backend hits = %d, want 1 (400 is not retryable)", hits.Load())
	}
}
