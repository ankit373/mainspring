// Package auth provides API-key admission control for the server. Mainspring is
// standalone-first, so it must be able to require keys itself — the incumbents'
// "no authentication whatsoever" is the single most-exploited local-inference
// failure. When no keys are configured the server runs open but says so loudly
// (see server startup); it never becomes insecure silently.
package auth

import (
	"crypto/subtle"
	"net/http"
	"strings"
)

// Authenticator checks API keys.
type Authenticator struct {
	keys []string
}

// New builds an Authenticator from the configured keys (may be empty).
func New(keys []string) *Authenticator {
	cleaned := make([]string, 0, len(keys))
	for _, k := range keys {
		if k = strings.TrimSpace(k); k != "" {
			cleaned = append(cleaned, k)
		}
	}
	return &Authenticator{keys: cleaned}
}

// Open reports whether the server is running without authentication.
func (a *Authenticator) Open() bool { return len(a.keys) == 0 }

// valid reports whether presented matches a configured key (constant-time).
func (a *Authenticator) valid(presented string) bool {
	ok := false
	for _, k := range a.keys {
		if subtle.ConstantTimeCompare([]byte(k), []byte(presented)) == 1 {
			ok = true
		}
	}
	return ok
}

// Wrap enforces auth on next. /healthz is always exempt (liveness probes).
func (a *Authenticator) Wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if a.Open() || r.URL.Path == "/healthz" {
			next.ServeHTTP(w, r)
			return
		}
		if !a.valid(bearer(r)) {
			w.Header().Set("content-type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":{"message":"invalid or missing API key","type":"authentication_error"}}`))
			return
		}
		next.ServeHTTP(w, r)
	})
}

// bearer extracts the key from Authorization: Bearer <key> or the x-api-key header.
func bearer(r *http.Request) string {
	if h := r.Header.Get("Authorization"); h != "" {
		if after, found := strings.CutPrefix(h, "Bearer "); found {
			return strings.TrimSpace(after)
		}
	}
	return strings.TrimSpace(r.Header.Get("x-api-key"))
}
