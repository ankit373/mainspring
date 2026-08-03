package server_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/ankit373/mainspring/internal/auth"
	"github.com/ankit373/mainspring/internal/backend"
	"github.com/ankit373/mainspring/internal/metrics"
	"github.com/ankit373/mainspring/internal/scheduler"
	"github.com/ankit373/mainspring/internal/server"
)

// TestLatencyPercentilesEndToEnd drives several requests through a resident
// model and asserts the resulting duration percentiles surface through both
// /v1/quality and /metrics — proving the metrics-package percentile machinery
// is actually wired into the HTTP surface, not just unit-tested in isolation.
func TestLatencyPercentilesEndToEnd(t *testing.T) {
	// A deliberately slow engine, not an instant one. Percentiles taken off a
	// sub-millisecond in-process loopback call floor to exactly 0 on Windows, whose
	// timer granularity is coarser than the call itself — and 0 is indistinguishable
	// from "never recorded", which is the thing this test exists to catch (#230).
	var hits atomic.Int64
	eng := slowCountingEngine(t, &hits, firstByteDelay)
	sched := scheduler.New(
		map[string]backend.Backend{"fake": &engineBackend{baseURL: eng.URL}},
		[]backend.ModelSpec{{ID: "m1", Backend: "fake"}},
		scheduler.Options{MaxLoaded: 2},
	)
	rec, _ := metrics.New("")
	h := server.New(sched, auth.New(nil), rec).Handler()

	for i := 0; i < 5; i++ {
		postBody(h, `{"model":"m1","messages":[]}`)
	}

	qw := httptest.NewRecorder()
	h.ServeHTTP(qw, httptest.NewRequest(http.MethodGet, "/v1/quality", nil))
	var q struct {
		Models []struct {
			ID            string  `json:"id"`
			DurationP50Ms float64 `json:"duration_ms_p50"`
			DurationP99Ms float64 `json:"duration_ms_p99"`
		} `json:"models"`
	}
	if err := json.Unmarshal(qw.Body.Bytes(), &q); err != nil {
		t.Fatalf("decode quality: %v", err)
	}
	if len(q.Models) != 1 {
		t.Fatalf("expected exactly one model in /v1/quality: %+v", q.Models)
	}
	// Every request waited at least firstByteDelay upstream, so require the reported
	// median to reflect that. This is strictly stronger than "nonzero": it checks the
	// figure tracks real elapsed time instead of merely being present.
	if want := float64(firstByteDelay.Milliseconds()); q.Models[0].DurationP50Ms < want {
		t.Fatalf("duration_ms_p50 = %v, want at least the upstream delay %v: %+v",
			q.Models[0].DurationP50Ms, want, q.Models)
	}
	// And that the percentiles were computed over the requests actually served,
	// rather than over an empty window that happened to render plausibly.
	if got := hits.Load(); got != 5 {
		t.Fatalf("engine served %d requests, want 5", got)
	}
	if q.Models[0].DurationP99Ms < q.Models[0].DurationP50Ms {
		t.Fatalf("p99 (%v) should be >= p50 (%v)", q.Models[0].DurationP99Ms, q.Models[0].DurationP50Ms)
	}

	mw := httptest.NewRecorder()
	h.ServeHTTP(mw, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	body := mw.Body.String()
	for _, want := range []string{
		`mainspring_duration_ms_p50{model="m1"}`,
		`mainspring_duration_ms_p90{model="m1"}`,
		`mainspring_duration_ms_p99{model="m1"}`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("metrics missing %q:\n%s", want, body)
		}
	}
}
