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

// slowFixedEngine responds after a fixed delay, long enough that a concurrent
// second request is forced to queue behind a single-slot gate.
func slowFixedEngine(t *testing.T, delay time.Duration) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(delay)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"ok"}}]}`)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// TestQueueWaitEndToEnd proves the fix end-to-end: two concurrent requests
// against a single-slot gate force the second to actually queue, and that wait
// time surfaces as a positive queue_wait_ms_p50 in both /v1/quality and
// /metrics — previously this was invisible, folded silently into duration.
func TestQueueWaitEndToEnd(t *testing.T) {
	const delay = 100 * time.Millisecond
	eng := slowFixedEngine(t, delay)
	sched := scheduler.New(
		map[string]backend.Backend{"fake": &engineBackend{baseURL: eng.URL}},
		[]backend.ModelSpec{{ID: "m1", Backend: "fake"}},
		scheduler.Options{MaxLoaded: 2},
	)
	rec, _ := metrics.New("")
	srv := server.New(sched, auth.New(nil), rec)
	srv.SetConcurrency(1, 4) // 1 slot, room to queue
	h := srv.Handler()

	var wg sync.WaitGroup
	wg.Add(2)
	for i := 0; i < 2; i++ {
		go func() {
			defer wg.Done()
			postBody(h, `{"model":"m1","messages":[]}`)
		}()
	}
	wg.Wait()

	qw := httptest.NewRecorder()
	h.ServeHTTP(qw, httptest.NewRequest(http.MethodGet, "/v1/quality", nil))
	var q struct {
		Models []struct {
			ID             string  `json:"id"`
			QueueWaitP50Ms float64 `json:"queue_wait_ms_p50"`
		} `json:"models"`
	}
	if err := json.Unmarshal(qw.Body.Bytes(), &q); err != nil {
		t.Fatalf("decode quality: %v", err)
	}
	if len(q.Models) != 1 || q.Models[0].QueueWaitP50Ms <= 0 {
		t.Fatalf("expected a positive queue_wait_ms_p50 (one request queued behind the other): %+v", q.Models)
	}

	mw := httptest.NewRecorder()
	h.ServeHTTP(mw, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if !strings.Contains(mw.Body.String(), `mainspring_queue_wait_ms_p50{model="m1"}`) {
		t.Fatalf("metrics missing queue wait gauge:\n%s", mw.Body.String())
	}
}

// TestQueueWaitZeroWhenUncontended proves the flip side: with no concurrency
// limit, requests never queue and the percentile stays at (or very near) 0.
func TestQueueWaitZeroWhenUncontended(t *testing.T) {
	eng := fakeEngine(t)
	sched := scheduler.New(
		map[string]backend.Backend{"fake": &engineBackend{baseURL: eng.URL}},
		[]backend.ModelSpec{{ID: "m1", Backend: "fake"}},
		scheduler.Options{MaxLoaded: 2},
	)
	rec, _ := metrics.New("")
	h := server.New(sched, auth.New(nil), rec).Handler() // no SetConcurrency => unbounded

	postBody(h, `{"model":"m1","messages":[]}`)

	qw := httptest.NewRecorder()
	h.ServeHTTP(qw, httptest.NewRequest(http.MethodGet, "/v1/quality", nil))
	var q struct {
		Models []struct {
			QueueWaitP99Ms float64 `json:"queue_wait_ms_p99"`
		} `json:"models"`
	}
	if err := json.Unmarshal(qw.Body.Bytes(), &q); err != nil {
		t.Fatalf("decode quality: %v", err)
	}
	if len(q.Models) != 1 || q.Models[0].QueueWaitP99Ms > 5 {
		t.Fatalf("expected ~0 queue wait when gating is disabled, got %+v", q.Models)
	}
}
