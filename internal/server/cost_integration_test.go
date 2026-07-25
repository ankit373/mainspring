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

// TestCostAccountingFlows drives a priced model and asserts the request's cost
// (from the engine's real usage object: 3 prompt + 2 completion tokens) shows up
// in /v1/quality and /metrics.
func TestCostAccountingFlows(t *testing.T) {
	var hits atomic.Int64
	eng := countingEngine(t, &hits)

	sched := scheduler.New(
		map[string]backend.Backend{"fake": &engineBackend{baseURL: eng.URL}},
		[]backend.ModelSpec{{ID: "m1", Backend: "fake"}},
		scheduler.Options{MaxLoaded: 2},
	)
	rec, _ := metrics.New("")
	srv := server.New(sched, auth.New(nil), rec)
	srv.SetCostRates(map[string]server.CostRate{
		"m1": server.NewCostRate(3.0, 15.0),
	})
	h := srv.Handler()

	if w := postBody(h, `{"model":"m1","messages":[]}`); w.Code != 200 {
		t.Fatalf("inference status=%d", w.Code)
	}
	want := 3.0/1e6*3.0 + 2.0/1e6*15.0 // prompt*in + completion*out

	// /v1/quality: per-model + server total cost.
	qw := httptest.NewRecorder()
	h.ServeHTTP(qw, httptest.NewRequest(http.MethodGet, "/v1/quality", nil))
	var q struct {
		Server struct {
			CostTotal float64 `json:"cost_usd_total"`
		} `json:"server"`
		Models []struct {
			ID   string  `json:"id"`
			Cost float64 `json:"cost_usd"`
		} `json:"models"`
	}
	if err := json.Unmarshal(qw.Body.Bytes(), &q); err != nil {
		t.Fatalf("decode quality: %v", err)
	}
	if q.Server.CostTotal <= 0 {
		t.Fatalf("server cost_usd_total = %v, want ~%v", q.Server.CostTotal, want)
	}
	var m1cost float64
	for _, m := range q.Models {
		if m.ID == "m1" {
			m1cost = m.Cost
		}
	}
	if m1cost <= 0 {
		t.Fatalf("m1 cost_usd = %v, want ~%v", m1cost, want)
	}

	// /metrics: cost counter present and non-zero for m1.
	mw := httptest.NewRecorder()
	h.ServeHTTP(mw, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if !strings.Contains(mw.Body.String(), `mainspring_cost_usd_total{model="m1"}`) {
		t.Fatalf("metrics missing cost counter:\n%s", mw.Body.String())
	}
}
