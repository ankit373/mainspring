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

// adminRoutes is every management route: the eight /admin endpoints plus the two
// management endpoints outside that prefix. SECURITY.md promises the admin role
// gates all of them, so every one is asserted, not a representative sample.
var adminRoutes = []struct{ method, path string }{
	{http.MethodGet, "/admin/config"},
	{http.MethodGet, "/admin/usage"},
	{http.MethodPost, "/admin/drain"},
	{http.MethodPost, "/admin/reload"},
	{http.MethodPost, "/admin/models/m1/load"},
	{http.MethodPost, "/admin/models/m1/unload"},
	{http.MethodPost, "/admin/breaker/m1/reset"},
	{http.MethodPost, "/admin/cache/clear"},
	{http.MethodGet, "/capabilities"},
	{http.MethodGet, "/v1/quality"},
}

// gatedServer wires a server with one admin and one inference tenant.
func gatedServer(t *testing.T) http.Handler {
	t.Helper()
	eng := fakeEngine(t)
	sched := scheduler.New(
		map[string]backend.Backend{"fake": &engineBackend{baseURL: eng.URL}},
		[]backend.ModelSpec{{ID: "m1", Backend: "fake"}},
		scheduler.Options{MaxLoaded: 2},
	)
	rec, _ := metrics.New("")
	a := auth.NewTenants([]auth.Tenant{
		{Name: "ops", Key: "adminkey", Role: auth.RoleAdmin},
		{Name: "user", Key: "userkey", Role: auth.RoleInference},
	})
	return server.New(sched, a, rec).Handler()
}

func callAs(h http.Handler, method, path, key string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, nil)
	req.Header.Set("Authorization", "Bearer "+key)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

func TestManagementRoutesRequireAdminRole(t *testing.T) {
	h := gatedServer(t)
	for _, rt := range adminRoutes {
		t.Run(rt.method+" "+rt.path, func(t *testing.T) {
			if w := callAs(h, rt.method, rt.path, "userkey"); w.Code != http.StatusForbidden {
				t.Fatalf("inference role: status = %d, want 403 (body %s)", w.Code, w.Body.String())
			}
			// The gate must be the *role*, not the route being broken for
			// everyone: an admin gets through to the handler.
			if w := callAs(h, rt.method, rt.path, "adminkey"); w.Code == http.StatusForbidden {
				t.Fatalf("admin role was refused: %s", w.Body.String())
			}
		})
	}
}

// TestAccessLogAttributesAdminAction is the audit-trail claim: an admin action
// is only "audited" if the log says who did it.
func TestAccessLogAttributesAdminAction(t *testing.T) {
	eng := fakeEngine(t)
	sched := scheduler.New(
		map[string]backend.Backend{"fake": &engineBackend{baseURL: eng.URL}},
		[]backend.ModelSpec{{ID: "m1", Backend: "fake"}},
		scheduler.Options{MaxLoaded: 2},
	)
	rec, _ := metrics.New("")
	a := auth.NewTenants([]auth.Tenant{{Name: "ops", Key: "adminkey", Role: auth.RoleAdmin}})
	srv := server.New(sched, a, rec)
	var log strings.Builder
	srv.SetAccessLog(&log)
	h := srv.Handler()

	if w := callAs(h, http.MethodPost, "/admin/drain", "adminkey"); w.Code != http.StatusOK {
		t.Fatalf("drain status = %d", w.Code)
	}
	line := log.String()
	if !strings.Contains(line, `"tenant":"ops"`) {
		t.Fatalf("admin action must name its principal in the access log: %s", line)
	}
	if !strings.Contains(line, `"path":"/admin/drain"`) || !strings.Contains(line, `"status":200`) {
		t.Fatalf("access log line malformed: %s", line)
	}

	// A rejected key authenticates nobody, so there is no principal to record.
	log.Reset()
	if w := callAs(h, http.MethodPost, "/admin/drain", "wrongkey"); w.Code != http.StatusUnauthorized {
		t.Fatalf("bad key status = %d, want 401", w.Code)
	}
	if strings.Contains(log.String(), `"tenant"`) {
		t.Fatalf("a rejected request must not be attributed to a tenant: %s", log.String())
	}
}
