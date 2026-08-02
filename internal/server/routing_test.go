package server_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ankit373/mainspring/internal/auth"
	"github.com/ankit373/mainspring/internal/backend"
	"github.com/ankit373/mainspring/internal/metrics"
	"github.com/ankit373/mainspring/internal/scheduler"
	"github.com/ankit373/mainspring/internal/server"
)

// Every error the server emits goes through the taxonomy — including the two
// net/http used to answer on its own, in plain text, for paths the mux does not
// route: `404 page not found` and `405 Method Not Allowed`. A client branching on
// the stable `code` field got nothing to branch on.

// apiError is the wire shape every error must have.
type apiError struct {
	Error struct {
		Message string `json:"message"`
		Type    string `json:"type"`
		Code    string `json:"code"`
	} `json:"error"`
}

func decodeAPIError(t *testing.T, w *httptest.ResponseRecorder) apiError {
	t.Helper()
	if ct := w.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("content-type = %q, want application/json (body: %s)", ct, w.Body.String())
	}
	var e apiError
	if err := json.Unmarshal(w.Body.Bytes(), &e); err != nil {
		t.Fatalf("decode error body: %v (%s)", err, w.Body.String())
	}
	return e
}

// do issues a request with an arbitrary method and no body.
func do(h http.Handler, method, path string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(method, path, nil))
	return w
}

func routingServer(t *testing.T) http.Handler {
	t.Helper()
	eng := fakeEngine(t)
	return newTestServer(t, &engineBackend{baseURL: eng.URL}, nil)
}

func TestUnroutedPathIsInTheTaxonomy(t *testing.T) {
	h := routingServer(t)
	// Not just /v1/*: nothing the mux fails to route may escape the taxonomy.
	for _, tc := range []struct{ method, path string }{
		{http.MethodPost, "/v1/messages/count_tokenz"},
		{http.MethodGet, "/v1/nope"},
		{http.MethodPost, "/v2/chat/completions"},
		{http.MethodGet, "/"},
		{http.MethodGet, "/admin"},
		{http.MethodDelete, "/totally/unknown"},
	} {
		w := do(h, tc.method, tc.path)
		if w.Code != http.StatusNotFound {
			t.Fatalf("%s %s → status %d, want 404 (%s)", tc.method, tc.path, w.Code, w.Body.String())
		}
		e := decodeAPIError(t, w)
		if e.Error.Code != "route_not_found" {
			t.Fatalf("%s %s → code %q, want route_not_found", tc.method, tc.path, e.Error.Code)
		}
		if e.Error.Type != "not_found_error" {
			t.Fatalf("%s %s → type %q, want not_found_error", tc.method, tc.path, e.Error.Type)
		}
		if e.Error.Message == "" {
			t.Fatalf("%s %s → empty error message", tc.method, tc.path)
		}
	}
}

// An unrouted path must not be reported as a missing *model*: the two 404s have
// different causes and a router branching on `code` must be able to tell them
// apart.
func TestUnroutedPathIsNotModelNotFound(t *testing.T) {
	h := routingServer(t)
	if got := decodeAPIError(t, do(h, http.MethodGet, "/v1/nope")).Error.Code; got == "model_not_found" {
		t.Fatal("an unrouted path reported model_not_found — the model was never the problem")
	}
	// The real model 404 still says model_not_found.
	if got := decodeAPIError(t, do(h, http.MethodGet, "/v1/models/ghost")).Error.Code; got != "model_not_found" {
		t.Fatalf("unknown model → code %q, want model_not_found", got)
	}
}

// The automatic 405 must survive the fix. A catch-all registered at "/" would
// have made ServeMux answer 404 here, because a catch-all matches every method
// and there is then no "path matched, method did not" case left.
func TestMethodMismatchIsInTheTaxonomy(t *testing.T) {
	h := routingServer(t)
	for _, tc := range []struct{ method, path, allow string }{
		{http.MethodPost, "/v1/models/m1", "GET, HEAD"},   // registered GET /v1/models/{id}
		{http.MethodGet, "/admin/drain", "POST"},          // registered POST /admin/drain
		{http.MethodDelete, "/admin/config", "GET, HEAD"}, // registered GET /admin/config
		{http.MethodGet, "/v1/messages/count_tokens", "POST"},
	} {
		w := do(h, tc.method, tc.path)
		if w.Code != http.StatusMethodNotAllowed {
			t.Fatalf("%s %s → status %d, want 405 (%s)", tc.method, tc.path, w.Code, w.Body.String())
		}
		if got := w.Header().Get("Allow"); got != tc.allow {
			t.Fatalf("%s %s → Allow %q, want %q", tc.method, tc.path, got, tc.allow)
		}
		e := decodeAPIError(t, w)
		if e.Error.Code != "method_not_allowed" {
			t.Fatalf("%s %s → code %q, want method_not_allowed", tc.method, tc.path, e.Error.Code)
		}
	}
}

// The wrapper decides on mux.Handler but must dispatch through mux.ServeHTTP:
// only ServeHTTP records the matched pattern on the request, and every {id}
// route reads it back with r.PathValue.
func TestWildcardRoutesStillSeePathValues(t *testing.T) {
	h := routingServer(t)
	w := getJSON(h, "/v1/models/m1", "")
	if w.Code != http.StatusOK {
		t.Fatalf("GET /v1/models/m1 → %d, want 200 (%s)", w.Code, w.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out["id"] != "m1" {
		t.Fatalf("id = %v, want m1 — the path wildcard did not reach the handler", out["id"])
	}
	if w := post(t, h, "/admin/models/m1/load"); w.Code != http.StatusOK {
		t.Fatalf("POST /admin/models/m1/load → %d, want 200 (%s)", w.Code, w.Body.String())
	}
}

// A path ServeMux would redirect to must still redirect rather than 404: the
// redirect reports a non-empty pattern (the target it points at), which is what
// the wrapper keys on.
func TestRedirectingPathStillRedirects(t *testing.T) {
	h := routingServer(t)
	w := do(h, http.MethodGet, "/v1//models") // cleans to /v1/models
	if w.Code < 300 || w.Code >= 400 {
		t.Fatalf("GET /v1//models → %d, want a redirect (%s)", w.Code, w.Body.String())
	}
	if got := w.Header().Get("Location"); got != "/v1/models" {
		t.Fatalf("Location = %q, want /v1/models", got)
	}
}

// Ordering with auth is load-bearing: an unauthenticated caller must be rejected
// before routing, so the 401 never doubles as a map of which paths exist.
func TestAuthStillPrecedesRouting(t *testing.T) {
	eng := fakeEngine(t)
	h := newTestServer(t, &engineBackend{baseURL: eng.URL}, []string{"secret"})
	for _, path := range []string{"/v1/nope", "/v1/models/m1"} {
		if w := do(h, http.MethodDelete, path); w.Code != http.StatusUnauthorized {
			t.Fatalf("unauthenticated %s → %d, want 401 (%s)", path, w.Code, w.Body.String())
		}
	}
	// Exempt paths are unaffected.
	if w := do(h, http.MethodGet, "/healthz"); w.Code != http.StatusOK {
		t.Fatalf("healthz → %d, want 200", w.Code)
	}
}

// An alias-only server proves the 404 for a path is decided by the mux, not by
// anything model-shaped: no model needs to exist for routing to answer.
func TestUnroutedPathNeedsNoModels(t *testing.T) {
	sched := scheduler.New(map[string]backend.Backend{}, nil, scheduler.Options{})
	rec, _ := metrics.New("")
	h := server.New(sched, auth.New(nil), rec).Handler()
	w := do(h, http.MethodPost, "/v1/anything")
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", w.Code)
	}
	if got := decodeAPIError(t, w).Error.Code; got != "route_not_found" {
		t.Fatalf("code = %q, want route_not_found", got)
	}
}
