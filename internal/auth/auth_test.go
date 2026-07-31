package auth

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestOpenMode(t *testing.T) {
	a := New(nil)
	if !a.Open() {
		t.Fatal("no keys => open")
	}
	if New([]string{"  "}).Open() != true {
		t.Fatal("blank keys are ignored => still open")
	}
	if New([]string{"k"}).Open() {
		t.Fatal("a real key => not open")
	}
}

func TestWrapEnforcesKey(t *testing.T) {
	a := New([]string{"secret"})
	ok := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) })
	h := a.Wrap(ok)

	cases := []struct {
		name   string
		path   string
		header map[string]string
		want   int
	}{
		{"no key", "/v1/models", nil, http.StatusUnauthorized},
		{"wrong key", "/v1/models", map[string]string{"Authorization": "Bearer nope"}, http.StatusUnauthorized},
		{"right bearer", "/v1/models", map[string]string{"Authorization": "Bearer secret"}, http.StatusOK},
		{"right x-api-key", "/v1/models", map[string]string{"x-api-key": "secret"}, http.StatusOK},
		{"healthz exempt", "/healthz", nil, http.StatusOK},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, tc.path, nil)
			for k, v := range tc.header {
				r.Header.Set(k, v)
			}
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != tc.want {
				t.Fatalf("got %d, want %d", w.Code, tc.want)
			}
		})
	}
}

func TestReloadSwapsTenants(t *testing.T) {
	a := New([]string{"old"})
	if a.lookup("old") == nil {
		t.Fatal("initial key should authenticate")
	}
	// Reload with a new key set: old revoked, new admitted.
	a.Reload([]Tenant{{Name: "svc", Key: "new", Role: RoleAdmin}})
	if a.lookup("old") != nil {
		t.Fatal("revoked key must no longer authenticate after reload")
	}
	tn := a.lookup("new")
	if tn == nil || tn.Name != "svc" || tn.Role != RoleAdmin {
		t.Fatalf("new tenant not admitted after reload: %+v", tn)
	}
}

func TestReloadToOpen(t *testing.T) {
	a := New([]string{"k"})
	if a.Open() {
		t.Fatal("keyed auth should not be open")
	}
	a.Reload(nil)
	if !a.Open() {
		t.Fatal("reloading to no tenants should make the server open")
	}
}

func TestTenantRateLimit(t *testing.T) {
	a := NewTenants([]Tenant{{Name: "t", Key: "k", Role: RoleInference, RateRPM: 2}})
	h := a.Wrap(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) }))
	do := func() *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
		r.Header.Set("Authorization", "Bearer k")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	if do().Code != 200 || do().Code != 200 {
		t.Fatal("first two requests should pass")
	}
	third := do()
	if third.Code != http.StatusTooManyRequests {
		t.Fatalf("third request should be 429, got %d", third.Code)
	}
	if third.Header().Get("Retry-After") == "" {
		t.Fatal("429 must set Retry-After")
	}
}

func TestTokenBudget(t *testing.T) {
	a := NewTenants([]Tenant{{Name: "t", Key: "k", TokenBudget: 100, WindowSec: 60}})
	tn := &Tenant{Name: "t", Key: "k", TokenBudget: 100, WindowSec: 60}
	if !a.AllowTokens(tn) {
		t.Fatal("fresh budget should allow")
	}
	a.AddTokens(tn, 100)
	if a.AllowTokens(tn) {
		t.Fatal("budget exhausted should deny")
	}
}

func TestUnlimitedTenantHasNoQuota(t *testing.T) {
	a := NewTenants([]Tenant{{Name: "t", Key: "k"}}) // no RateRPM, no budget
	tn := &Tenant{Name: "t", Key: "k"}
	a.AddTokens(tn, 1_000_000)
	if !a.AllowTokens(tn) {
		t.Fatal("zero budget means unlimited")
	}
}

// TestQuotaHeadersOnSuccessfulResponse proves the fix: a tenant with a
// configured rate/token quota sees headroom on a normal, successful response
// -- not just after finally being rejected.
func TestQuotaHeadersOnSuccessfulResponse(t *testing.T) {
	a := NewTenants([]Tenant{{Name: "t", Key: "k", RateRPM: 5, TokenBudget: 1000, WindowSec: 60}})
	h := a.Wrap(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) }))
	r := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	r.Header.Set("Authorization", "Bearer k")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if got := w.Header().Get("X-Mainspring-RateLimit-Limit"); got != "5" {
		t.Fatalf("RateLimit-Limit = %q, want 5", got)
	}
	if got := w.Header().Get("X-Mainspring-RateLimit-Remaining"); got != "4" {
		t.Fatalf("RateLimit-Remaining = %q, want 4 (5 - this request)", got)
	}
	if got := w.Header().Get("X-Mainspring-RateLimit-Reset"); got == "" {
		t.Fatal("RateLimit-Reset should be set")
	}
	if got := w.Header().Get("X-Mainspring-TokenBudget-Limit"); got != "1000" {
		t.Fatalf("TokenBudget-Limit = %q, want 1000", got)
	}
	if got := w.Header().Get("X-Mainspring-TokenBudget-Remaining"); got != "1000" {
		t.Fatalf("TokenBudget-Remaining = %q, want 1000 (no tokens spent yet)", got)
	}
}

// Headers must be omitted entirely for a tenant with no configured quota, and
// in open mode (no tenant at all).
func TestQuotaHeadersOmittedWhenUnlimited(t *testing.T) {
	a := NewTenants([]Tenant{{Name: "t", Key: "k"}}) // no RateRPM, no TokenBudget
	h := a.Wrap(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) }))
	r := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	r.Header.Set("Authorization", "Bearer k")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)

	for _, h := range []string{
		"X-Mainspring-RateLimit-Limit", "X-Mainspring-RateLimit-Remaining", "X-Mainspring-RateLimit-Reset",
		"X-Mainspring-TokenBudget-Limit", "X-Mainspring-TokenBudget-Remaining", "X-Mainspring-TokenBudget-Reset",
	} {
		if got := w.Header().Get(h); got != "" {
			t.Fatalf("%s = %q, want empty (no quota configured)", h, got)
		}
	}
}

func TestOpenModeAllowsAll(t *testing.T) {
	a := New(nil)
	h := a.Wrap(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) }))
	r := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("open mode should allow all, got %d", w.Code)
	}
}
