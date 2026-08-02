package server_test

import (
	"context"
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

// truncatedJSONEngine promises far more body than it delivers on a
// *non-streaming* response — a generation cut mid-answer. It lingers before
// returning so concurrent identical requests coalesce behind it.
func truncatedJSONEngine(t *testing.T, hits *atomic.Int64, linger time.Duration) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Length", "4096") // far more than we will write
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"Hel`)
		w.(http.Flusher).Flush()
		time.Sleep(linger)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// heldEngine blocks every response until release is closed, so a test can pin a
// gate slot occupied for exactly as long as it needs.
func heldEngine(t *testing.T, release <-chan struct{}) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"ok"}}]}`)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// integrityServer builds an OpenAI-path handler over baseURL, letting the test
// configure breaker/cache/coalescing/concurrency.
func integrityServer(t *testing.T, baseURL string, configure func(*server.Server)) http.Handler {
	t.Helper()
	sched := scheduler.New(
		map[string]backend.Backend{"fake": &engineBackend{baseURL: baseURL}},
		[]backend.ModelSpec{{ID: "m1", Backend: "fake"}},
		scheduler.Options{MaxLoaded: 2},
	)
	rec, _ := metrics.New("")
	srv := server.New(sched, auth.New(nil), rec)
	if configure != nil {
		configure(srv)
	}
	return srv.Handler()
}

// scrape returns the current /metrics exposition.
func scrape(t *testing.T, h http.Handler) string {
	t.Helper()
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	return w.Body.String()
}

// waitForMetric polls /metrics until it contains want, so a concurrency test can
// synchronise on real server state instead of a sleep.
func waitForMetric(t *testing.T, h http.Handler, want string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(scrape(t, h), want) {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %q in /metrics:\n%s", want, scrape(t, h))
}

// ── #160 stream integrity ────────────────────────────────────────────────────

// TestStreamTruncatedEmitsErrorFrame proves a stream cut mid-generation ends with
// a terminal SSE error frame instead of a clean stop. The status is pinned at 200
// by then, so the frame is the only thing that can tell the client its answer was
// severed rather than finished.
func TestStreamTruncatedEmitsErrorFrame(t *testing.T) {
	eng := truncatedStreamEngine(t)
	h := integrityServer(t, eng.URL, nil)

	w := postBody(h, `{"model":"m1","stream":true,"messages":[]}`)
	body := w.Body.String()
	if !strings.Contains(body, "event: error") {
		t.Fatalf("a truncated stream must emit a terminal error frame:\n%s", body)
	}
	if !strings.Contains(body, `"code":"upstream_error"`) {
		t.Fatalf("the error frame should carry the upstream_error code:\n%s", body)
	}
	if !strings.Contains(body, "Hel") {
		t.Fatalf("the tokens that did arrive should still be delivered:\n%s", body)
	}
}

// TestTruncatedStreamCountsAsBackendFailure proves the breaker learns about a
// mid-stream failure. Status alone cannot express it — the stream was already
// committed at 200 — so before the explicit signal a severed stream was recorded
// as a healthy request.
func TestTruncatedStreamCountsAsBackendFailure(t *testing.T) {
	eng := truncatedStreamEngine(t)
	h := integrityServer(t, eng.URL, func(srv *server.Server) {
		srv.SetBreaker(1, time.Hour) // one failure trips it
	})

	postBody(h, `{"model":"m1","stream":true,"messages":[]}`)

	if out := scrape(t, h); !strings.Contains(out, `mainspring_breaker_open{model="m1"} 1`) {
		t.Fatalf("a truncated stream must count as a backend failure:\n%s", out)
	}
}

// TestTruncatedResponseIsNotCached proves a non-streaming answer that stopped
// early is never stored — otherwise one severed read is replayed to every later
// caller for the life of the entry.
func TestTruncatedResponseIsNotCached(t *testing.T) {
	var hits atomic.Int64
	eng := truncatedJSONEngine(t, &hits, 0)
	h := integrityServer(t, eng.URL, func(srv *server.Server) { srv.SetCache(time.Minute, 16) })

	const body = `{"model":"m1","temperature":0,"messages":[{"role":"user","content":"hi"}]}`
	postBody(h, body)
	w2 := postBody(h, body)

	if w2.Header().Get("X-Mainspring-Cache") == "hit" {
		t.Fatal("a truncated response must never be served from the cache")
	}
	if got := hits.Load(); got != 2 {
		t.Fatalf("backend hits = %d, want 2 — the truncated answer was cached and replayed", got)
	}
}

// TestTruncatedResponseIsNotPublishedToFollowers proves the coalescing leader
// does not hand a severed body to the callers waiting on it. They fall through
// and run their own request instead.
func TestTruncatedResponseIsNotPublishedToFollowers(t *testing.T) {
	var hits atomic.Int64
	eng := truncatedJSONEngine(t, &hits, 120*time.Millisecond)
	h := integrityServer(t, eng.URL, func(srv *server.Server) { srv.SetCoalescing(true) })

	const n = 5
	const body = `{"model":"m1","temperature":0,"messages":[{"role":"user","content":"same"}]}`
	var (
		wg        sync.WaitGroup
		coalesced atomic.Int64
	)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if postBody(h, body).Header().Get("X-Mainspring-Coalesced") == "true" {
				coalesced.Add(1)
			}
		}()
	}
	wg.Wait()

	if got := coalesced.Load(); got != 0 {
		t.Fatalf("%d follower(s) were served the leader's truncated body; want 0", got)
	}
}

// ── #161 failure attribution ─────────────────────────────────────────────────

// TestClientAbortIsNotABackendFailure proves a caller that hangs up mid-request
// is not charged to the circuit breaker. It used to surface as a 502, so enough
// client disconnects opened the circuit for every tenant.
func TestClientAbortIsNotABackendFailure(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	eng := heldEngine(t, release)
	h := integrityServer(t, eng.URL, func(srv *server.Server) {
		srv.SetBreaker(1, time.Hour) // one failure would trip it
		srv.SetConcurrency(1, 4)     // so /metrics reports when the request is in flight
	})

	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"m1","messages":[]}`)).WithContext(ctx)
	w := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		defer close(done)
		h.ServeHTTP(w, req)
	}()

	waitForMetric(t, h, `mainspring_inflight_requests{model="m1"} 1`)
	cancel() // the client goes away mid-generation
	<-done

	if out := scrape(t, h); strings.Contains(out, `mainspring_breaker_open{model="m1"} 1`) {
		t.Fatalf("a client abort must not open the circuit:\n%s", out)
	}
	if w.Body.Len() != 0 {
		t.Fatalf("nothing should be written to an abandoned connection, got: %s", w.Body.String())
	}
}

// TestRequestTimeoutIsStillABackendFailure guards the deliberate other half of
// the rule: a per-request timeout is the backend's fault and must keep counting.
func TestRequestTimeoutIsStillABackendFailure(t *testing.T) {
	eng := stallingEngine(t, 500*time.Millisecond)
	h := integrityServer(t, eng.URL, func(srv *server.Server) {
		srv.SetBreaker(1, time.Hour)
		srv.SetTimeouts(30*time.Millisecond, nil)
	})

	if w := postBody(h, `{"model":"m1","messages":[]}`); w.Code != http.StatusGatewayTimeout {
		t.Fatalf("status = %d, want 504; body=%s", w.Code, w.Body.String())
	}
	if out := scrape(t, h); !strings.Contains(out, `mainspring_breaker_open{model="m1"} 1`) {
		t.Fatalf("a timeout must still count as a backend failure:\n%s", out)
	}
}

// TestClientCancelledWhileQueuedGetsNoBusyError proves a caller who disconnects
// while waiting for a gate slot is not answered `503 server_busy` — the queue was
// never full, and the connection is already gone.
func TestClientCancelledWhileQueuedGetsNoBusyError(t *testing.T) {
	release := make(chan struct{})
	eng := heldEngine(t, release)
	h := integrityServer(t, eng.URL, func(srv *server.Server) { srv.SetConcurrency(1, 4) })

	// Occupy the only slot. Freeing it is deferred so a failed assertion still
	// unblocks the engine before httptest waits on its connections.
	holder := make(chan struct{})
	go func() {
		defer close(holder)
		postBody(h, `{"model":"m1","messages":[]}`)
	}()
	defer func() { close(release); <-holder }()
	waitForMetric(t, h, `mainspring_inflight_requests{model="m1"} 1`)

	// A second caller queues behind it, then hangs up.
	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"m1","messages":[]}`)).WithContext(ctx)
	w := httptest.NewRecorder()
	queued := make(chan struct{})
	go func() {
		defer close(queued)
		h.ServeHTTP(w, req)
	}()
	waitForMetric(t, h, `mainspring_queued_requests{model="m1"} 1`)
	cancel()
	<-queued

	if strings.Contains(w.Body.String(), "server_busy") {
		t.Fatalf("a cancelled waiter must not be told the server is busy: %s", w.Body.String())
	}
	if w.Body.Len() != 0 {
		t.Fatalf("nothing should be written to an abandoned connection, got: %s", w.Body.String())
	}
}
