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

func TestAdminUsagePerTenant(t *testing.T) {
	eng := fakeEngine(t)
	sched := scheduler.New(
		map[string]backend.Backend{"fake": &engineBackend{baseURL: eng.URL}},
		[]backend.ModelSpec{{ID: "m1", Backend: "fake"}},
		scheduler.Options{MaxLoaded: 2},
	)
	rec, _ := metrics.New("")
	a := auth.NewTenants([]auth.Tenant{
		{Name: "boss", Key: "adminkey", Role: auth.RoleAdmin},
		{Name: "user", Key: "userkey", Role: auth.RoleInference},
	})
	h := server.New(sched, a, rec).Handler()

	// The inference user makes two requests.
	for i := 0; i < 2; i++ {
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"m1","messages":[]}`))
		req.Header.Set("Authorization", "Bearer userkey")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != 200 {
			t.Fatalf("inference %d status=%d", i, w.Code)
		}
	}

	// Admin reads the per-tenant usage rollup.
	w := getJSON(h, "/admin/usage", "adminkey")
	if w.Code != 200 {
		t.Fatalf("usage status = %d, want 200", w.Code)
	}
	var out struct {
		Tenants []metrics.TenantUsage `json:"tenants"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	var found bool
	for _, tu := range out.Tenants {
		if tu.Tenant == "user" {
			found = true
			if tu.Requests != 2 {
				t.Fatalf("user requests = %d, want 2", tu.Requests)
			}
		}
	}
	if !found {
		t.Fatalf("user not in usage rollup: %+v", out.Tenants)
	}
}

func TestAdminUsageForbiddenForNonAdmin(t *testing.T) {
	a := auth.NewTenants([]auth.Tenant{{Name: "user", Key: "userkey", Role: auth.RoleInference}})
	h := configTestServer(t, a).Handler()
	if w := getJSON(h, "/admin/usage", "userkey"); w.Code != http.StatusForbidden {
		t.Fatalf("non-admin status = %d, want 403", w.Code)
	}
}
