package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// The production mux registers no trailing-slash patterns, so the redirect
// ServeMux does for /tree → /tree/ can only be exercised against a mux built
// here. It matters because that redirect is the one case where a *miss* still
// reports a non-empty pattern, and turning it into a 404 would break subtree
// routing for anything registered later.
func TestRouteErrorsKeepsTrailingSlashRedirect(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/tree/", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	})
	h := routeErrors(mux)

	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/tree", nil))
	// net/http answered this with 301 before Go 1.26 and 307 after; either way it
	// must be a redirect to the slashed path, not a 404.
	if w.Code < 300 || w.Code >= 400 {
		t.Fatalf("GET /tree → %d, want a redirect to /tree/ (%s)", w.Code, w.Body.String())
	}
	if got := w.Header().Get("Location"); got != "/tree/" {
		t.Fatalf("Location = %q, want /tree/", got)
	}
	// And the subtree itself still routes.
	w = httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/tree/leaf", nil))
	if w.Code != http.StatusTeapot {
		t.Fatalf("GET /tree/leaf → %d, want 418 from the subtree handler", w.Code)
	}
}

// allowedMethods must report exactly the methods that route the path — the same
// set ServeMux would have put on its own Allow header — and nothing for a path
// no method routes.
func TestAllowedMethods(t *testing.T) {
	mux := http.NewServeMux()
	nop := func(http.ResponseWriter, *http.Request) {}
	mux.HandleFunc("GET /read", nop)
	mux.HandleFunc("POST /write", nop)
	mux.HandleFunc("GET /both", nop)
	mux.HandleFunc("DELETE /both", nop)
	mux.HandleFunc("/any", nop) // no method: every method routes it

	for _, tc := range []struct{ method, path, want string }{
		{http.MethodPost, "/read", "GET, HEAD"},
		{http.MethodGet, "/write", "POST"},
		{http.MethodPatch, "/both", "GET, HEAD, DELETE"},
		{http.MethodGet, "/nothing", ""},
		{http.MethodHead, "/write", "POST"},
	} {
		r := httptest.NewRequest(tc.method, tc.path, nil)
		if got := allowedMethods(mux, r); got != tc.want {
			t.Fatalf("allowedMethods(%s %s) = %q, want %q", tc.method, tc.path, got, tc.want)
		}
	}
	// The request's own method is never listed as an alternative to itself: it is
	// the one method already known not to route.
	if got := allowedMethods(mux, httptest.NewRequest(http.MethodPost, "/write", nil)); got != "" {
		t.Fatalf("allowedMethods listed the request's own method: %q", got)
	}
}

// The wrapper must not swallow what a routed handler writes.
func TestRouteErrorsPassesRoutedRequestsThrough(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/thing/{id}", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"id": r.PathValue("id")})
	})
	w := httptest.NewRecorder()
	routeErrors(mux).ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/v1/thing/abc", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", w.Code, w.Body.String())
	}
	var out map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out["id"] != "abc" {
		t.Fatalf("PathValue(id) = %q, want abc", out["id"])
	}
}
