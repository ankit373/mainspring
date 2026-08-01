package server_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
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
	eng := fakeEngine(t)
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
	if len(q.Models) != 1 || q.Models[0].DurationP50Ms <= 0 {
		t.Fatalf("expected a positive duration_ms_p50 after 5 requests: %+v", q.Models)
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
