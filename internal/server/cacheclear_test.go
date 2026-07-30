package server_test

import (
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ankit373/mainspring/internal/auth"
	"github.com/ankit373/mainspring/internal/backend"
	"github.com/ankit373/mainspring/internal/metrics"
	"github.com/ankit373/mainspring/internal/scheduler"
	"github.com/ankit373/mainspring/internal/server"
)

// TestAdminCacheClearEvictsHits proves the fix: a cached response that would
// normally survive until TTL expiry can be purged on demand.
func TestAdminCacheClearEvictsHits(t *testing.T) {
	var hits atomic.Int64
	eng := countingEngine(t, &hits)
	sched := scheduler.New(
		map[string]backend.Backend{"fake": &engineBackend{baseURL: eng.URL}},
		[]backend.ModelSpec{{ID: "m1", Backend: "fake"}},
		scheduler.Options{MaxLoaded: 2},
	)
	rec, _ := metrics.New("")
	srv := server.New(sched, auth.New(nil), rec)
	srv.SetCache(time.Hour, 16) // long TTL — only Clear should evict it
	h := srv.Handler()

	const body = `{"model":"m1","temperature":0,"messages":[{"role":"user","content":"hi"}]}`
	postBody(h, body) // miss, populates the cache
	w := postBody(h, body)
	if w.Header().Get("X-Mainspring-Cache") != "hit" {
		t.Fatalf("second identical request should be a cache hit before clear")
	}
	if hits.Load() != 1 {
		t.Fatalf("backend hits = %d, want 1 before clear", hits.Load())
	}

	cw := post(t, h, "/admin/cache/clear")
	if cw.Code != 200 {
		t.Fatalf("clear status = %d, want 200; body=%s", cw.Code, cw.Body.String())
	}

	// The same request must now miss again and hit the backend a second time.
	w2 := postBody(h, body)
	if w2.Header().Get("X-Mainspring-Cache") == "hit" {
		t.Fatal("request should be a miss immediately after /admin/cache/clear")
	}
	if hits.Load() != 2 {
		t.Fatalf("backend hits = %d, want 2 after clear forced a re-fetch", hits.Load())
	}
}

func TestAdminCacheClearNoopWhenDisabled(t *testing.T) {
	eng := fakeEngine(t)
	sched := scheduler.New(
		map[string]backend.Backend{"fake": &engineBackend{baseURL: eng.URL}},
		[]backend.ModelSpec{{ID: "m1", Backend: "fake"}},
		scheduler.Options{MaxLoaded: 2},
	)
	rec, _ := metrics.New("")
	h := server.New(sched, auth.New(nil), rec).Handler() // caching never enabled

	if w := post(t, h, "/admin/cache/clear"); w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 even with caching disabled", w.Code)
	}
}
