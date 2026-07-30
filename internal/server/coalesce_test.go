package server_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ankit373/mainspring/internal/auth"
	"github.com/ankit373/mainspring/internal/backend"
	"github.com/ankit373/mainspring/internal/metrics"
	"github.com/ankit373/mainspring/internal/scheduler"
	"github.com/ankit373/mainspring/internal/server"
)

// slowEngine counts hits and delays each response long enough that a burst of
// concurrent requests overlaps in flight.
func slowEngine(t *testing.T, hits *atomic.Int64) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		time.Sleep(120 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"shared"}}],"usage":{"prompt_tokens":3,"completion_tokens":2}}`)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestCoalesceIdenticalRequests(t *testing.T) {
	var hits atomic.Int64
	eng := slowEngine(t, &hits)
	sched := scheduler.New(
		map[string]backend.Backend{"fake": &engineBackend{baseURL: eng.URL}},
		[]backend.ModelSpec{{ID: "m1", Backend: "fake"}},
		scheduler.Options{MaxLoaded: 2},
	)
	rec, _ := metrics.New("")
	srv := server.New(sched, auth.New(nil), rec)
	srv.SetCoalescing(true)
	h := srv.Handler()

	const n = 10
	const body = `{"model":"m1","temperature":0,"messages":[{"role":"user","content":"same"}]}`
	var (
		wg         sync.WaitGroup
		coalesced  atomic.Int64
		okCount    atomic.Int64
		sameBodies sync.Map
	)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			w := postBody(h, body)
			if w.Code == 200 {
				okCount.Add(1)
			}
			if w.Header().Get("X-Mainspring-Coalesced") == "true" {
				coalesced.Add(1)
			}
			sameBodies.Store(w.Body.String(), true)
		}()
	}
	wg.Wait()

	if okCount.Load() != n {
		t.Fatalf("only %d/%d requests succeeded", okCount.Load(), n)
	}
	if hits.Load() != 1 {
		t.Fatalf("backend hits = %d, want 1 (all identical requests coalesced)", hits.Load())
	}
	if coalesced.Load() != n-1 {
		t.Fatalf("coalesced followers = %d, want %d", coalesced.Load(), n-1)
	}
	// Every caller saw the same body.
	count := 0
	sameBodies.Range(func(_, _ any) bool { count++; return true })
	if count != 1 {
		t.Fatalf("callers saw %d distinct bodies, want 1", count)
	}

	// The coalesced count surfaces through /metrics (end-to-end wiring).
	mw := httptest.NewRecorder()
	h.ServeHTTP(mw, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if !strings.Contains(mw.Body.String(), `mainspring_coalesced_total{model="m1"} 9`) {
		t.Fatalf("metrics missing coalesced counter:\n%s", mw.Body.String())
	}
}

func TestNoCoalesceWhenDisabled(t *testing.T) {
	var hits atomic.Int64
	eng := slowEngine(t, &hits)
	sched := scheduler.New(
		map[string]backend.Backend{"fake": &engineBackend{baseURL: eng.URL}},
		[]backend.ModelSpec{{ID: "m1", Backend: "fake"}},
		scheduler.Options{MaxLoaded: 2},
	)
	rec, _ := metrics.New("")
	h := server.New(sched, auth.New(nil), rec).Handler() // coalescing off

	const n = 4
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			postBody(h, `{"model":"m1","temperature":0,"messages":[]}`)
		}()
	}
	wg.Wait()
	if hits.Load() != n {
		t.Fatalf("backend hits = %d, want %d (no coalescing)", hits.Load(), n)
	}
}
