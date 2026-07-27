package server_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/ankit373/mainspring/internal/auth"
	"github.com/ankit373/mainspring/internal/backend"
	"github.com/ankit373/mainspring/internal/metrics"
	"github.com/ankit373/mainspring/internal/scheduler"
	"github.com/ankit373/mainspring/internal/server"
)

func TestModelDetailNotResident(t *testing.T) {
	eng := fakeEngine(t)
	sched := scheduler.New(
		map[string]backend.Backend{"fake": &engineBackend{baseURL: eng.URL}},
		[]backend.ModelSpec{{ID: "m1", Backend: "fake"}},
		scheduler.Options{MaxLoaded: 2},
	)
	rec, _ := metrics.New("")
	h := server.New(sched, auth.New(nil), rec).Handler()

	w := getJSON(h, "/v1/models/m1", "")
	if w.Code != 200 {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out["id"] != "m1" || out["object"] != "model" || out["owned_by"] != "mainspring" {
		t.Fatalf("unexpected body: %v", out)
	}
	if out["resident"] != false {
		t.Fatalf("resident = %v, want false (not loaded)", out["resident"])
	}
}

func TestModelDetailResidentEnriched(t *testing.T) {
	eng := fakeEngine(t)
	sched := scheduler.New(
		map[string]backend.Backend{"fake": &engineBackend{baseURL: eng.URL}},
		[]backend.ModelSpec{{ID: "m1", Backend: "fake"}},
		scheduler.Options{MaxLoaded: 2},
	)
	rec, _ := metrics.New("")
	h := server.New(sched, auth.New(nil), rec).Handler()

	if w := post(t, h, "/admin/models/m1/load"); w.Code != 200 {
		t.Fatalf("load status=%d", w.Code)
	}
	w := getJSON(h, "/v1/models/m1", "")
	var out map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out["resident"] != true {
		t.Fatalf("resident = %v, want true", out["resident"])
	}
	if out["backend"] != "fake" || out["device"] != "metal" {
		t.Fatalf("expected fail-loud fields, got: %v", out)
	}
}

func TestModelDetailResolvesAlias(t *testing.T) {
	eng := fakeEngine(t)
	sched := scheduler.New(
		map[string]backend.Backend{"fake": &engineBackend{baseURL: eng.URL}},
		[]backend.ModelSpec{{ID: "m1", Backend: "fake"}},
		scheduler.Options{MaxLoaded: 2, Aliases: map[string]string{"friendly": "m1"}},
	)
	rec, _ := metrics.New("")
	h := server.New(sched, auth.New(nil), rec).Handler()

	w := getJSON(h, "/v1/models/friendly", "")
	var out map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out["id"] != "m1" {
		t.Fatalf("alias should resolve to real id m1, got %v", out["id"])
	}
}

func TestModelDetailUnknown(t *testing.T) {
	eng := fakeEngine(t)
	sched := scheduler.New(
		map[string]backend.Backend{"fake": &engineBackend{baseURL: eng.URL}},
		[]backend.ModelSpec{{ID: "m1", Backend: "fake"}},
		scheduler.Options{MaxLoaded: 2},
	)
	rec, _ := metrics.New("")
	h := server.New(sched, auth.New(nil), rec).Handler()

	if w := getJSON(h, "/v1/models/nope", ""); w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", w.Code)
	}
}
