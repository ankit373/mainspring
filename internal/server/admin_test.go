package server_test

import (
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

func adminServer(t *testing.T) (*server.Server, http.Handler) {
	t.Helper()
	eng := fakeEngine(t)
	sched := scheduler.New(
		map[string]backend.Backend{"fake": &engineBackend{baseURL: eng.URL}},
		[]backend.ModelSpec{{ID: "m1", Backend: "fake"}},
		scheduler.Options{MaxLoaded: 2},
	)
	rec, _ := metrics.New("")
	srv := server.New(sched, auth.New(nil), rec)
	return srv, srv.Handler()
}

func post(t *testing.T, h http.Handler, path string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodPost, path, nil))
	return w
}

func TestAdminDrainFlipsReadiness(t *testing.T) {
	_, h := adminServer(t)
	if w := post(t, h, "/admin/drain"); w.Code != 200 {
		t.Fatalf("drain status=%d", w.Code)
	}
	// readyz should now be 503.
	rw := httptest.NewRecorder()
	h.ServeHTTP(rw, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if rw.Code != http.StatusServiceUnavailable {
		t.Fatalf("after drain, readyz=%d want 503", rw.Code)
	}
	// Clearing drain restores readiness.
	post(t, h, "/admin/drain?on=false")
	rw2 := httptest.NewRecorder()
	h.ServeHTTP(rw2, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if rw2.Code != 200 {
		t.Fatalf("after drain off, readyz=%d want 200", rw2.Code)
	}
}

func TestAdminLoadThenUnload(t *testing.T) {
	_, h := adminServer(t)
	if w := post(t, h, "/admin/models/m1/load"); w.Code != 200 || !strings.Contains(w.Body.String(), `"loaded":"m1"`) {
		t.Fatalf("load status=%d body=%s", w.Code, w.Body.String())
	}
	if w := post(t, h, "/admin/models/m1/unload"); w.Code != 200 || !strings.Contains(w.Body.String(), `"unloaded":true`) {
		t.Fatalf("unload status=%d body=%s", w.Code, w.Body.String())
	}
	// Unknown model => 404 model_not_found.
	if w := post(t, h, "/admin/models/nope/load"); w.Code != 404 || !strings.Contains(w.Body.String(), "model_not_found") {
		t.Fatalf("unknown model load status=%d body=%s", w.Code, w.Body.String())
	}
}

func TestAdminReloadHook(t *testing.T) {
	srv, h := adminServer(t)
	// No hook => 500.
	if w := post(t, h, "/admin/reload"); w.Code != 500 {
		t.Fatalf("reload without hook status=%d want 500", w.Code)
	}
	called := false
	srv.SetReloadFunc(func() error { called = true; return nil })
	if w := post(t, h, "/admin/reload"); w.Code != 200 || !called {
		t.Fatalf("reload hook not invoked: status=%d called=%v", w.Code, called)
	}
}

func TestAdminRequiresAdminRole(t *testing.T) {
	eng := fakeEngine(t)
	sched := scheduler.New(
		map[string]backend.Backend{"fake": &engineBackend{baseURL: eng.URL}},
		[]backend.ModelSpec{{ID: "m1", Backend: "fake"}},
		scheduler.Options{MaxLoaded: 2},
	)
	rec, _ := metrics.New("")
	a := auth.NewTenants([]auth.Tenant{{Name: "user", Key: "k", Role: auth.RoleInference}})
	h := server.New(sched, a, rec).Handler()

	req := httptest.NewRequest(http.MethodPost, "/admin/drain", nil)
	req.Header.Set("Authorization", "Bearer k")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("inference role should be forbidden from admin, got %d", w.Code)
	}
}
