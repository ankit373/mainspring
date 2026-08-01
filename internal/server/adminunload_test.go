package server_test

import (
	"encoding/json"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/ankit373/mainspring/internal/auth"
	"github.com/ankit373/mainspring/internal/backend"
	"github.com/ankit373/mainspring/internal/metrics"
	"github.com/ankit373/mainspring/internal/scheduler"
	"github.com/ankit373/mainspring/internal/server"
)

// TestAdminUnloadReportsInterruptedRequests proves the fix end-to-end: a
// forced unload while a request is actively in flight reports it as
// interrupted (rather than silently pretending the model was idle).
func TestAdminUnloadReportsInterruptedRequests(t *testing.T) {
	const delay = 150 * time.Millisecond
	eng := slowFixedEngine(t, delay)
	sched := scheduler.New(
		map[string]backend.Backend{"fake": &engineBackend{baseURL: eng.URL}},
		[]backend.ModelSpec{{ID: "m1", Backend: "fake"}},
		scheduler.Options{MaxLoaded: 2},
	)
	rec, _ := metrics.New("")
	h := server.New(sched, auth.New(nil), rec).Handler()

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		postBody(h, `{"model":"m1","messages":[]}`)
	}()
	time.Sleep(30 * time.Millisecond) // let the request actually start (mark busy)

	w := post(t, h, "/admin/models/m1/unload")
	if w.Code != 200 {
		t.Fatalf("unload status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var out struct {
		Unloaded            bool `json:"unloaded"`
		InterruptedRequests int  `json:"interrupted_requests"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !out.Unloaded {
		t.Fatal("unloaded should be true")
	}
	if out.InterruptedRequests != 1 {
		t.Fatalf("interrupted_requests = %d, want 1 (the in-flight request)", out.InterruptedRequests)
	}
	wg.Wait()
}

func TestAdminUnloadIdleModelReportsZeroInterrupted(t *testing.T) {
	eng := fakeEngine(t)
	sched := scheduler.New(
		map[string]backend.Backend{"fake": &engineBackend{baseURL: eng.URL}},
		[]backend.ModelSpec{{ID: "m1", Backend: "fake"}},
		scheduler.Options{MaxLoaded: 2},
	)
	rec, _ := metrics.New("")
	h := server.New(sched, auth.New(nil), rec).Handler()

	postBody(h, `{"model":"m1","messages":[]}`) // completes fully before unload

	w := post(t, h, "/admin/models/m1/unload")
	var out struct {
		Unloaded            bool `json:"unloaded"`
		InterruptedRequests int  `json:"interrupted_requests"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !out.Unloaded || out.InterruptedRequests != 0 {
		t.Fatalf("idle unload = (unloaded=%v, interrupted=%d), want (true, 0)", out.Unloaded, out.InterruptedRequests)
	}
}

func TestAdminUnloadUnknownModel(t *testing.T) {
	eng := fakeEngine(t)
	sched := scheduler.New(
		map[string]backend.Backend{"fake": &engineBackend{baseURL: eng.URL}},
		[]backend.ModelSpec{{ID: "m1", Backend: "fake"}},
		scheduler.Options{MaxLoaded: 2},
	)
	rec, _ := metrics.New("")
	h := server.New(sched, auth.New(nil), rec).Handler()

	if w := post(t, h, "/admin/models/nope/unload"); w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", w.Code)
	}
}
