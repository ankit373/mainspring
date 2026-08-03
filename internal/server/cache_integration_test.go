package server_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
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

// countingEngine records how many upstream requests it actually served, so a
// cache hit can be proven by the count staying flat.
func countingEngine(t *testing.T, hits *atomic.Int64) *httptest.Server {
	return slowCountingEngine(t, hits, 0)
}

// slowCountingEngine is countingEngine with a delay before it answers, for tests
// that assert on a measured latency. The delay stays opt-in so the other seventeen
// users of countingEngine are not slowed for it.
func slowCountingEngine(t *testing.T, hits *atomic.Int64, delay time.Duration) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if delay > 0 {
			time.Sleep(delay)
		}
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

// TestCacheOmittedTemperatureNotCached guards the #158 fix: an omitted
// temperature is not a deterministic profile — the OpenAI default is 1, i.e.
// "sample normally" — so each caller must get its own sample.
func TestCacheOmittedTemperatureNotCached(t *testing.T) {
	var hits atomic.Int64
	eng := countingEngine(t, &hits)
	h := cachedServer(t, eng.URL)

	const body = `{"model":"m1","messages":[{"role":"user","content":"hi"}]}` // no temperature
	postBody(h, body)
	w := postBody(h, body)
	if w.Header().Get("X-Mainspring-Cache") == "hit" {
		t.Fatal("a request with no temperature must not be served from the cache")
	}
	if got := hits.Load(); got != 2 {
		t.Fatalf("backend hits = %d, want 2 (an unset temperature samples per call)", got)
	}
}

// TestCoalesceOmittedTemperatureNotShared is the coalescing half of #158: an
// unset temperature must not have one random sample fanned out to a whole burst.
func TestCoalesceOmittedTemperatureNotShared(t *testing.T) {
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

	const n = 4
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			w := postBody(h, `{"model":"m1","messages":[]}`) // no temperature
			if w.Header().Get("X-Mainspring-Coalesced") == "true" {
				t.Error("a request with no temperature must never be coalesced")
			}
		}()
	}
	wg.Wait()
	if hits.Load() != n {
		t.Fatalf("backend hits = %d, want %d (each caller samples independently)", hits.Load(), n)
	}
}

// ── replay header integrity (#157) ───────────────────────────────────────────

// twoTenantServer builds a server with two tenants whose quotas differ, so a
// leaked headroom header is unambiguous about which tenant it came from.
func twoTenantServer(t *testing.T, baseURL string) *server.Server {
	t.Helper()
	sched := scheduler.New(
		map[string]backend.Backend{"fake": &engineBackend{baseURL: baseURL}},
		[]backend.ModelSpec{{ID: "m1", Backend: "fake"}},
		scheduler.Options{MaxLoaded: 2},
	)
	rec, _ := metrics.New("")
	return server.New(sched, auth.NewTenants([]auth.Tenant{
		{Name: "alpha", Key: "key-alpha", Role: auth.RoleInference, RateRPM: 90, TokenBudget: 9000},
		{Name: "beta", Key: "key-beta", Role: auth.RoleInference, RateRPM: 7, TokenBudget: 700},
	}), rec)
}

// postAs sends a cacheable request as one tenant with an explicit correlation
// id, so a replay can be checked for the caller's *own* id.
func postAs(h http.Handler, key, reqID, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("X-Request-ID", reqID)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

// assertOwnHeaders asserts a replayed response reports the current caller's own
// correlation id and quota headroom — exactly one value each, so the leader's
// value is neither substituted nor appended alongside.
func assertOwnHeaders(t *testing.T, what string, h http.Header, reqID, rateLimit, tokenLimit string) {
	t.Helper()
	for _, c := range []struct{ key, want string }{
		{"X-Request-ID", reqID},
		{"X-Mainspring-RateLimit-Limit", rateLimit},
		{"X-Mainspring-TokenBudget-Limit", tokenLimit},
	} {
		if got := h.Values(c.key); len(got) != 1 || got[0] != c.want {
			t.Errorf("%s: %s = %q, want exactly [%q]", what, c.key, got, c.want)
		}
	}
	for _, k := range []string{
		"X-Mainspring-RateLimit-Remaining", "X-Mainspring-RateLimit-Reset",
		"X-Mainspring-TokenBudget-Remaining", "X-Mainspring-TokenBudget-Reset",
	} {
		if got := h.Values(k); len(got) != 1 {
			t.Errorf("%s: %s = %q, want exactly one value (this caller's own)", what, k, got)
		}
	}
	// Payload-describing headers must still replay.
	if got := h.Get("X-Mainspring-Backend"); got != "fake" {
		t.Errorf("%s: X-Mainspring-Backend = %q, want %q", what, got, "fake")
	}
	if got := h.Get("X-Mainspring-Device"); got != "metal" {
		t.Errorf("%s: X-Mainspring-Device = %q, want %q", what, got, "metal")
	}
	if got := h.Get("Content-Type"); !strings.Contains(got, "application/json") {
		t.Errorf("%s: Content-Type = %q, want application/json", what, got)
	}
}

// TestCacheReplayDoesNotLeakTenantHeaders drives two distinct tenants through
// the same cacheable request. The cache hit must carry tenant beta's own id and
// headroom, never alpha's — replaying alpha's quota headers would disclose one
// tenant's remaining budget to another.
func TestCacheReplayDoesNotLeakTenantHeaders(t *testing.T) {
	var hits atomic.Int64
	eng := countingEngine(t, &hits)
	srv := twoTenantServer(t, eng.URL)
	srv.SetCache(time.Minute, 16)
	h := srv.Handler()

	const body = `{"model":"m1","temperature":0,"messages":[{"role":"user","content":"hi"}]}`

	if w := postAs(h, "key-alpha", "req-alpha", body); w.Code != 200 {
		t.Fatalf("leader status = %d; body=%s", w.Code, w.Body.String())
	}
	w2 := postAs(h, "key-beta", "req-beta", body)
	if w2.Code != 200 || w2.Header().Get("X-Mainspring-Cache") != "hit" {
		t.Fatalf("second tenant should be served from cache: code=%d cache=%q", w2.Code, w2.Header().Get("X-Mainspring-Cache"))
	}
	if hits.Load() != 1 {
		t.Fatalf("backend hits = %d, want 1", hits.Load())
	}
	assertOwnHeaders(t, "cache hit", w2.Header(), "req-beta", "7", "700")
}

// gatedEngine signals when a request reaches it and blocks until release is
// closed, so a second caller is guaranteed to still be waiting on the leader's
// in-flight computation rather than starting its own.
func gatedEngine(t *testing.T, hits *atomic.Int64, entered chan<- struct{}, release <-chan struct{}) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		entered <- struct{}{}
		<-release
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"shared"}}],"usage":{"prompt_tokens":3,"completion_tokens":2}}`)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// TestCoalescedReplayDoesNotLeakTenantHeaders is the coalescing half of #157:
// the follower replays the leader's *body*, but must report its own id and its
// own quota headroom.
func TestCoalescedReplayDoesNotLeakTenantHeaders(t *testing.T) {
	var hits atomic.Int64
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	eng := gatedEngine(t, &hits, entered, release)
	srv := twoTenantServer(t, eng.URL)
	srv.SetCoalescing(true)
	h := srv.Handler()

	const body = `{"model":"m1","temperature":0,"messages":[{"role":"user","content":"same"}]}`

	leader := make(chan *httptest.ResponseRecorder, 1)
	go func() { leader <- postAs(h, "key-alpha", "req-alpha", body) }()
	<-entered // alpha owns the flight and is parked in the engine

	// Beta joins alpha's flight (a handful of in-memory steps), then alpha is let go.
	go func() { time.Sleep(100 * time.Millisecond); close(release) }()
	w2 := postAs(h, "key-beta", "req-beta", body)
	w1 := <-leader

	if w1.Code != 200 || w2.Code != 200 {
		t.Fatalf("status leader=%d follower=%d, want 200/200", w1.Code, w2.Code)
	}
	if w2.Header().Get("X-Mainspring-Coalesced") != "true" {
		t.Fatalf("second tenant should have been coalesced; headers=%v", w2.Header())
	}
	if hits.Load() != 1 {
		t.Fatalf("backend hits = %d, want 1 (one shared computation)", hits.Load())
	}
	if w2.Body.String() != w1.Body.String() {
		t.Fatalf("follower body differs from the leader's: %q vs %q", w2.Body.String(), w1.Body.String())
	}
	assertOwnHeaders(t, "coalesced follower", w2.Header(), "req-beta", "7", "700")
	assertOwnHeaders(t, "leader", w1.Header(), "req-alpha", "90", "9000")
}

// TestCacheHitReplaysNoLeaderDate pins the header whitelist end to end: an
// upstream Date (or any other header the engine sends) must not be replayed as
// if it described this response.
func TestCacheHitReplaysNoLeaderDate(t *testing.T) {
	var hits atomic.Int64
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Date", "Mon, 01 Jan 2035 00:00:00 GMT")
		w.Header().Set("X-Engine-Session", "leader-session")
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"Hello"}}],"usage":{"prompt_tokens":3,"completion_tokens":2}}`)
	})
	eng := httptest.NewServer(mux)
	t.Cleanup(eng.Close)
	h := cachedServer(t, eng.URL)

	const body = `{"model":"m1","temperature":0,"messages":[{"role":"user","content":"hi"}]}`
	postBody(h, body)
	w := postBody(h, body)
	if w.Header().Get("X-Mainspring-Cache") != "hit" {
		t.Fatalf("second request should be a cache hit; headers=%v", w.Header())
	}
	if got := w.Header().Get("Date"); got == "Mon, 01 Jan 2035 00:00:00 GMT" {
		t.Error("cache hit replayed the leader's Date")
	}
	if got := w.Header().Get("X-Engine-Session"); got != "" {
		t.Errorf("cache hit replayed an unlisted upstream header: X-Engine-Session=%q", got)
	}
	if got := w.Header().Get("X-Mainspring-Backend"); got != "fake" {
		t.Errorf("X-Mainspring-Backend = %q, want %q", got, "fake")
	}
}

// TestReplayableHeaderSetIsClosed documents the whitelist itself, so widening it
// is a deliberate act with a test to update rather than a silent regression.
func TestReplayableHeaderSetIsClosed(t *testing.T) {
	var hits atomic.Int64
	eng := countingEngine(t, &hits)
	h := cachedServer(t, eng.URL)

	const body = `{"model":"m1","temperature":0,"messages":[{"role":"user","content":"hi"}]}`
	postBody(h, body)
	w := postBody(h, body)

	got := map[string]bool{}
	for k := range w.Header() {
		got[k] = true
	}
	want := map[string]bool{
		// Replayed from the stored value.
		"Content-Type": true, "X-Mainspring-Backend": true, "X-Mainspring-Device": true,
		// Set by this replay, not the leader.
		"X-Mainspring-Cache": true, "X-Request-Id": true,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("cache-hit headers = %v, want %v", got, want)
	}
}

// ── /v1/messages sharing (the Anthropic dialect) ─────────────────────────────

// postMessagesAs sends a cacheable Anthropic request as one tenant.
func postMessagesAs(h http.Handler, key, reqID, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("X-Request-ID", reqID)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

const anthropicCacheable = `{"model":"m1","max_tokens":16,"temperature":0,"messages":[{"role":"user","content":"hi"}]}`

// TestMessagesCacheHitSkipsBackend — /v1/messages used to call the backend for
// every request no matter what, so N identical deterministic requests cost N
// generations. It now shares the same cache the OpenAI path uses.
func TestMessagesCacheHitSkipsBackend(t *testing.T) {
	var hits atomic.Int64
	eng := countingEngine(t, &hits)
	srv := twoTenantServer(t, eng.URL)
	srv.SetCache(time.Minute, 16)
	h := srv.Handler()

	first := postMessagesAs(h, "key-alpha", "req-alpha", anthropicCacheable)
	if first.Code != 200 {
		t.Fatalf("first status=%d body=%s", first.Code, first.Body.String())
	}
	second := postMessagesAs(h, "key-alpha", "req-2", anthropicCacheable)
	if second.Code != 200 || second.Header().Get("X-Mainspring-Cache") != "hit" {
		t.Fatalf("second should be a cache hit: code=%d cache=%q", second.Code, second.Header().Get("X-Mainspring-Cache"))
	}
	if hits.Load() != 1 {
		t.Fatalf("backend hits=%d, want 1", hits.Load())
	}
	// The replay must still be an Anthropic-shaped message, not the OpenAI body
	// the upstream returned — each dialect caches its own wire shape.
	if !strings.Contains(second.Body.String(), `"type":"message"`) {
		t.Fatalf("replay is not an Anthropic message:\n%s", second.Body.String())
	}
	if strings.Contains(second.Body.String(), `"choices"`) {
		t.Fatalf("replay leaked the OpenAI upstream shape:\n%s", second.Body.String())
	}
}

// TestMessagesCacheDoesNotCollideWithOpenAI — cache.Key includes the request
// path, so the same logical request through the two dialects must not serve one
// dialect's body to the other.
func TestMessagesCacheDoesNotCollideWithOpenAI(t *testing.T) {
	var hits atomic.Int64
	eng := countingEngine(t, &hits)
	srv := twoTenantServer(t, eng.URL)
	srv.SetCache(time.Minute, 16)
	h := srv.Handler()

	postMessagesAs(h, "key-alpha", "a1", anthropicCacheable)
	w := postAs(h, "key-alpha", "a2", `{"model":"m1","temperature":0,"messages":[{"role":"user","content":"hi"}]}`)
	if w.Header().Get("X-Mainspring-Cache") == "hit" {
		t.Fatal("an OpenAI request was served from the Anthropic cache entry")
	}
	if !strings.Contains(w.Body.String(), `"choices"`) {
		t.Fatalf("OpenAI caller did not get an OpenAI body:\n%s", w.Body.String())
	}
}

// TestMessagesCacheReplayDoesNotLeakTenantHeaders — the whitelist from #157 has
// to hold on this dialect too, now that it replays responses at all.
func TestMessagesCacheReplayDoesNotLeakTenantHeaders(t *testing.T) {
	var hits atomic.Int64
	eng := countingEngine(t, &hits)
	srv := twoTenantServer(t, eng.URL)
	srv.SetCache(time.Minute, 16)
	h := srv.Handler()

	if w := postMessagesAs(h, "key-alpha", "req-alpha", anthropicCacheable); w.Code != 200 {
		t.Fatalf("leader status=%d body=%s", w.Code, w.Body.String())
	}
	w2 := postMessagesAs(h, "key-beta", "req-beta", anthropicCacheable)
	if w2.Header().Get("X-Mainspring-Cache") != "hit" {
		t.Fatalf("second tenant should be served from cache: %q", w2.Header().Get("X-Mainspring-Cache"))
	}
	assertOwnHeaders(t, "messages cache hit", w2.Header(), "req-beta", "7", "700")
}

// TestMessagesStreamingBypassesCache — a stream can never be replayed, and
// cacheable() already refuses it; assert that holds through the new wiring.
func TestMessagesStreamingBypassesCache(t *testing.T) {
	var hits atomic.Int64
	eng := countingEngine(t, &hits)
	srv := twoTenantServer(t, eng.URL)
	srv.SetCache(time.Minute, 16)
	h := srv.Handler()

	const body = `{"model":"m1","max_tokens":16,"temperature":0,"stream":true,"messages":[{"role":"user","content":"hi"}]}`
	postMessagesAs(h, "key-alpha", "s1", body)
	w := postMessagesAs(h, "key-alpha", "s2", body)
	if w.Header().Get("X-Mainspring-Cache") == "hit" {
		t.Fatal("a streaming response must never be served from cache")
	}
	if hits.Load() != 2 {
		t.Fatalf("backend hits=%d, want 2 (both streams ran)", hits.Load())
	}
}
