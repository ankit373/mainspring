// Package auth provides admission control: API keys mapped to named tenants,
// roles (admin/inference), and per-tenant quotas (request rate + token budget).
// Mainspring is standalone-first, so it must govern its own endpoint — the
// incumbents' "no authentication whatsoever" is the most-exploited local
// failure. With no tenants configured the server runs open but says so loudly.
package auth

import (
	"context"
	"crypto/subtle"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

type ctxKey int

const tenantCtxKey ctxKey = 0

// Role gates which endpoints a tenant may call.
type Role string

const (
	RoleAdmin     Role = "admin"     // everything, incl. management endpoints
	RoleInference Role = "inference" // inference endpoints only
)

// Tenant is a named principal identified by an API key.
type Tenant struct {
	Name        string
	Key         string
	Role        Role
	RateRPM     int   // requests/min; 0 = unlimited
	TokenBudget int64 // tokens per window; 0 = unlimited
	WindowSec   int   // token-budget window; <=0 => 60s
}

func (t *Tenant) tokenWindow() int {
	if t.WindowSec <= 0 {
		return 60
	}
	return t.WindowSec
}

// Authenticator enforces keys, roles, and quotas.
type Authenticator struct {
	byKey map[string]*Tenant
	lim   *Limiter
}

// New builds an authenticator from a flat key list; each key becomes an admin
// tenant with no quotas. An empty list => open mode.
func New(keys []string) *Authenticator {
	ts := make([]Tenant, 0, len(keys))
	for i, k := range keys {
		if k = strings.TrimSpace(k); k != "" {
			ts = append(ts, Tenant{Name: fmt.Sprintf("key-%d", i+1), Key: k, Role: RoleAdmin})
		}
	}
	return NewTenants(ts)
}

// NewTenants builds an authenticator from explicit tenants.
func NewTenants(ts []Tenant) *Authenticator {
	a := &Authenticator{byKey: make(map[string]*Tenant), lim: NewLimiter()}
	for i := range ts {
		t := ts[i]
		if t.Key = strings.TrimSpace(t.Key); t.Key == "" {
			continue
		}
		if t.Role == "" {
			t.Role = RoleInference
		}
		if t.Name == "" {
			t.Name = fmt.Sprintf("tenant-%d", i+1)
		}
		tc := t
		a.byKey[t.Key] = &tc
	}
	return a
}

// Open reports whether the server runs without authentication.
func (a *Authenticator) Open() bool { return len(a.byKey) == 0 }

func (a *Authenticator) lookup(presented string) *Tenant {
	var found *Tenant
	for k, t := range a.byKey {
		if subtle.ConstantTimeCompare([]byte(k), []byte(presented)) == 1 {
			found = t
		}
	}
	return found
}

// exempt paths bypass auth entirely: liveness and metrics scraping.
func exempt(path string) bool { return path == "/healthz" || path == "/metrics" }

// Wrap authenticates the request, enforces the per-tenant request rate, and
// stashes the tenant in the context for downstream token-budget checks.
func (a *Authenticator) Wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if exempt(r.URL.Path) || a.Open() {
			next.ServeHTTP(w, r)
			return
		}
		t := a.lookup(bearer(r))
		if t == nil {
			unauthorized(w)
			return
		}
		if t.RateRPM > 0 {
			if ok, retry := a.lim.AllowRequest(t.Key, t.RateRPM); !ok {
				tooManyRequests(w, retry)
				return
			}
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), tenantCtxKey, t)))
	})
}

// AllowTokens reports whether tenant t is within its token budget.
func (a *Authenticator) AllowTokens(t *Tenant) bool {
	if t == nil || t.TokenBudget <= 0 {
		return true
	}
	return a.lim.WithinTokenBudget(t.Key, t.TokenBudget, t.tokenWindow())
}

// AddTokens accrues n output tokens against tenant t's budget window.
func (a *Authenticator) AddTokens(t *Tenant, n int64) {
	if t == nil || t.TokenBudget <= 0 || n <= 0 {
		return
	}
	a.lim.AddTokens(t.Key, n, t.tokenWindow())
}

// FromContext returns the authenticated tenant, if any (absent in open mode).
func FromContext(ctx context.Context) (*Tenant, bool) {
	t, ok := ctx.Value(tenantCtxKey).(*Tenant)
	return t, ok
}

// TenantOf returns the tenant name, or "" when unauthenticated.
func TenantOf(ctx context.Context) string {
	if t, ok := FromContext(ctx); ok {
		return t.Name
	}
	return ""
}

// bearer extracts the key from Authorization: Bearer <key> or x-api-key.
func bearer(r *http.Request) string {
	if h := r.Header.Get("Authorization"); h != "" {
		if after, found := strings.CutPrefix(h, "Bearer "); found {
			return strings.TrimSpace(after)
		}
	}
	return strings.TrimSpace(r.Header.Get("x-api-key"))
}

func unauthorized(w http.ResponseWriter) {
	w.Header().Set("content-type", "application/json")
	w.WriteHeader(http.StatusUnauthorized)
	_, _ = w.Write([]byte(`{"error":{"message":"invalid or missing API key","type":"authentication_error"}}`))
}

func tooManyRequests(w http.ResponseWriter, retry time.Duration) {
	secs := int(retry.Seconds())
	if secs < 1 {
		secs = 1
	}
	w.Header().Set("Retry-After", strconv.Itoa(secs))
	w.Header().Set("content-type", "application/json")
	w.WriteHeader(http.StatusTooManyRequests)
	_, _ = w.Write([]byte(`{"error":{"message":"rate limit exceeded","type":"rate_limit_error"}}`))
}
