package server_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ankit373/mainspring/internal/auth"
	"github.com/ankit373/mainspring/internal/backend"
	"github.com/ankit373/mainspring/internal/metrics"
	"github.com/ankit373/mainspring/internal/scheduler"
	"github.com/ankit373/mainspring/internal/server"
)

func configTestServer(t *testing.T, a *auth.Authenticator) *server.Server {
	t.Helper()
	eng := fakeEngine(t)
	sched := scheduler.New(
		map[string]backend.Backend{"fake": &engineBackend{baseURL: eng.URL}},
		[]backend.ModelSpec{{ID: "m1", Backend: "fake"}},
		scheduler.Options{MaxLoaded: 2},
	)
	rec, _ := metrics.New("")
	return server.New(sched, a, rec)
}

func getJSON(h http.Handler, path, key string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

func TestAdminConfigReportsEffectiveSettings(t *testing.T) {
	srv := configTestServer(t, auth.New(nil)) // open mode
	srv.SetRetry(3, 100*time.Millisecond)
	srv.SetCoalescing(true)
	srv.SetContextGuard(map[string]server.ContextPolicy{"m1": {Limit: 4096, Enforce: true}})
	srv.SetModelFallbacks(map[string][]string{"m1": {"backup"}})
	srv.SetCostRates(map[string]server.CostRate{"m1": server.NewCostRate(3, 15)})
	h := srv.Handler()

	w := getJSON(h, "/admin/config", "")
	if w.Code != 200 {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var cfg map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &cfg); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if retry := cfg["retry"].(map[string]any); retry["enabled"] != true || retry["max"].(float64) != 3 {
		t.Fatalf("retry not reported: %v", retry)
	}
	if co := cfg["coalesce"].(map[string]any); co["enabled"] != true {
		t.Fatal("coalesce not reported enabled")
	}
	if g := cfg["context_guard"].(map[string]any); g["m1"] == nil {
		t.Fatal("context guard for m1 missing")
	}
	if fb := cfg["model_fallbacks"].(map[string]any); fb["m1"] == nil {
		t.Fatal("model fallbacks for m1 missing")
	}
	if priced := cfg["cost_rates_set"].(map[string]any); priced["m1"] != true {
		t.Fatal("cost rate presence for m1 missing")
	}
}

// The config endpoint must never leak secret material: not the API key, and not
// the raw cost-rate dollar values (only presence booleans).
func TestAdminConfigIsSecretFree(t *testing.T) {
	srv := configTestServer(t, auth.New([]string{"super-secret-key"})) // key => admin role
	srv.SetCostRates(map[string]server.CostRate{"m1": server.NewCostRate(3, 15)})
	h := srv.Handler()

	w := getJSON(h, "/admin/config", "super-secret-key")
	if w.Code != 200 {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	body := w.Body.String()
	if strings.Contains(body, "super-secret-key") {
		t.Fatal("config leaked an API key")
	}
	if strings.Contains(body, "in_per_mtok") || strings.Contains(body, "out_per_mtok") {
		t.Fatal("config leaked raw cost-rate values (should be presence booleans only)")
	}
}

// TestAdminConfigExposesLintWarnings proves the fix: config lint results are
// now queryable live over HTTP, not just visible in startup stderr.
func TestAdminConfigExposesLintWarnings(t *testing.T) {
	srv := configTestServer(t, auth.New(nil))
	srv.SetLintWarnings([]string{`model "m1": enforce_context is on but ctx is not set — the context guardrail has no effect for this model`})
	h := srv.Handler()

	w := getJSON(h, "/admin/config", "")
	var cfg map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &cfg); err != nil {
		t.Fatalf("decode: %v", err)
	}
	warnings, ok := cfg["lint_warnings"].([]any)
	if !ok || len(warnings) != 1 {
		t.Fatalf("expected 1 lint warning, got %v", cfg["lint_warnings"])
	}
	if !strings.Contains(warnings[0].(string), "enforce_context") {
		t.Fatalf("unexpected warning content: %v", warnings[0])
	}
}

// A clean config (or one where SetLintWarnings was never called) reports an
// empty array, not null — friendlier for API consumers.
func TestAdminConfigLintWarningsEmptyWhenClean(t *testing.T) {
	srv := configTestServer(t, auth.New(nil))
	h := srv.Handler()

	w := getJSON(h, "/admin/config", "")
	var cfg map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &cfg); err != nil {
		t.Fatalf("decode: %v", err)
	}
	warnings, ok := cfg["lint_warnings"].([]any)
	if !ok {
		t.Fatalf("lint_warnings should be an array (even if empty), got %v (%T)", cfg["lint_warnings"], cfg["lint_warnings"])
	}
	if len(warnings) != 0 {
		t.Fatalf("expected no warnings, got %v", warnings)
	}
}

// SetLintWarnings can be called again (e.g. after a reload) and the response
// must reflect the latest call, not accumulate or ignore updates.
func TestAdminConfigLintWarningsUpdateAfterReload(t *testing.T) {
	srv := configTestServer(t, auth.New(nil))
	srv.SetLintWarnings([]string{"first warning"})
	h := srv.Handler()

	srv.SetLintWarnings([]string{"second warning", "third warning"})
	w := getJSON(h, "/admin/config", "")
	var cfg map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &cfg); err != nil {
		t.Fatalf("decode: %v", err)
	}
	warnings, _ := cfg["lint_warnings"].([]any)
	if len(warnings) != 2 {
		t.Fatalf("expected the latest 2 warnings (not accumulated), got %v", warnings)
	}
}

func TestAdminConfigForbiddenForNonAdmin(t *testing.T) {
	a := auth.NewTenants([]auth.Tenant{{Name: "user", Key: "userkey", Role: auth.RoleInference}})
	h := configTestServer(t, a).Handler()

	if w := getJSON(h, "/admin/config", "userkey"); w.Code != http.StatusForbidden {
		t.Fatalf("non-admin status = %d, want 403", w.Code)
	}
}
