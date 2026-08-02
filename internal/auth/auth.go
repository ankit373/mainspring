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
	"sync"
	"time"
)

type ctxKey int

const (
	tenantCtxKey    ctxKey = 0
	principalCtxKey ctxKey = 1
)

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
	mu    sync.RWMutex // guards byKey (SIGHUP reload swaps it)
	byKey map[string]*Tenant
	lim   *Limiter
}

// buildByKey indexes tenants by key, applying role/name defaults and skipping
// blank keys.
func buildByKey(ts []Tenant) map[string]*Tenant {
	byKey := make(map[string]*Tenant, len(ts))
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
		byKey[t.Key] = &tc
	}
	return byKey
}

// Reload atomically swaps the tenant set (e.g. on SIGHUP). The rate/token
// limiter state is preserved, so surviving keys keep their current windows.
func (a *Authenticator) Reload(ts []Tenant) {
	byKey := buildByKey(ts)
	a.mu.Lock()
	a.byKey = byKey
	a.mu.Unlock()
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
	return &Authenticator{byKey: buildByKey(ts), lim: NewLimiter()}
}

// Open reports whether the server runs without authentication.
func (a *Authenticator) Open() bool {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return len(a.byKey) == 0
}

func (a *Authenticator) lookup(presented string) *Tenant {
	a.mu.RLock()
	defer a.mu.RUnlock()
	var found *Tenant
	for k, t := range a.byKey {
		if subtle.ConstantTimeCompare([]byte(k), []byte(presented)) == 1 {
			found = t
		}
	}
	return found
}

// exempt paths bypass auth entirely: liveness, readiness, and metrics scraping.
func exempt(path string) bool {
	return path == "/healthz" || path == "/readyz" || path == "/metrics"
}

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
		// Name the principal before the quota checks, so a request rejected for
		// exceeding its rate limit is still attributable in the access log.
		setPrincipal(r.Context(), t.Name)
		if t.RateRPM > 0 {
			if ok, retry := a.lim.AllowRequest(t.Key, t.RateRPM); !ok {
				tooManyRequests(w, retry)
				return
			}
		}
		// Set after AllowRequest consumes this request's slot, so Remaining
		// reflects the count including the request currently being served (the
		// standard rate-limit-header convention).
		a.setQuotaHeaders(w, t)
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), tenantCtxKey, t)))
	})
}

// setQuotaHeaders reports a tenant's current rate-limit and token-budget
// headroom on every authenticated response — not just the one that finally
// gets rejected — so a well-behaved client can back off proactively instead of
// only reactively after a 429/token_budget error. Headers are pure reads (no
// side effects) and are omitted entirely for a quota that isn't configured.
func (a *Authenticator) setQuotaHeaders(w http.ResponseWriter, t *Tenant) {
	if t.RateRPM > 0 {
		remaining, reset := a.lim.RemainingRequests(t.Key, t.RateRPM)
		h := w.Header()
		h.Set("X-Mainspring-RateLimit-Limit", strconv.Itoa(t.RateRPM))
		h.Set("X-Mainspring-RateLimit-Remaining", strconv.FormatInt(remaining, 10))
		h.Set("X-Mainspring-RateLimit-Reset", strconv.Itoa(int(reset.Seconds())))
	}
	if t.TokenBudget > 0 {
		remaining, reset := a.lim.RemainingTokens(t.Key, t.TokenBudget, t.tokenWindow())
		h := w.Header()
		h.Set("X-Mainspring-TokenBudget-Limit", strconv.FormatInt(t.TokenBudget, 10))
		h.Set("X-Mainspring-TokenBudget-Remaining", strconv.FormatInt(remaining, 10))
		h.Set("X-Mainspring-TokenBudget-Reset", strconv.Itoa(int(reset.Seconds())))
	}
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

// principalSlot is a mutable, per-request holder for the authenticated tenant
// name. An audit trail needs the principal, but the middleware that writes it
// (the access log) has to sit *outside* authentication so that rejected and
// exempt requests are logged too — which means it never sees the resolved
// tenant on the way in. It instead installs an empty slot on the way in and
// reads it back on the way out, after Wrap has filled it. Both the write and
// the read happen on the single goroutine net/http runs the handler chain on,
// so the slot needs no synchronization.
type principalSlot struct{ name string }

// NewPrincipalSlot returns a context carrying an empty principal slot for Wrap
// to fill. Install it before authentication runs.
func NewPrincipalSlot(ctx context.Context) context.Context {
	return context.WithValue(ctx, principalCtxKey, &principalSlot{})
}

// Principal returns the tenant name authenticated for this request, or "" when
// the request was rejected, exempt, served in open mode, or carries no slot.
func Principal(ctx context.Context) string {
	if p, ok := ctx.Value(principalCtxKey).(*principalSlot); ok {
		return p.name
	}
	return ""
}

// setPrincipal records the authenticated tenant in the request's slot, if one
// was installed.
func setPrincipal(ctx context.Context, name string) {
	if p, ok := ctx.Value(principalCtxKey).(*principalSlot); ok {
		p.name = name
	}
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
	_, _ = w.Write([]byte(`{"error":{"message":"invalid or missing API key","type":"authentication_error","code":"unauthorized"}}`))
}

func tooManyRequests(w http.ResponseWriter, retry time.Duration) {
	secs := int(retry.Seconds())
	if secs < 1 {
		secs = 1
	}
	w.Header().Set("Retry-After", strconv.Itoa(secs))
	w.Header().Set("content-type", "application/json")
	w.WriteHeader(http.StatusTooManyRequests)
	_, _ = w.Write([]byte(`{"error":{"message":"rate limit exceeded","type":"rate_limit_error","code":"rate_limited"}}`))
}
