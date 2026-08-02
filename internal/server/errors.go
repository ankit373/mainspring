package server

import (
	"net/http"
	"strings"

	"github.com/ankit373/mainspring/internal/apierr"
)

// The error taxonomy itself lives in internal/apierr, because the auth
// middleware rejects requests (401, 429) before any handler here runs and must
// emit the identical wire shape. What follows are names, not a second copy:
// aliases so the call sites in this package keep their short spelling.

type errorCode = apierr.Code

const (
	codeInvalidRequest     = apierr.InvalidRequest
	codeContextLength      = apierr.ContextLength
	codeMethodNotAllowed   = apierr.MethodNotAllowed
	codeModelNotFound      = apierr.ModelNotFound
	codeRouteNotFound      = apierr.RouteNotFound
	codeUnauthorized       = apierr.Unauthorized
	codeForbidden          = apierr.Forbidden
	codeRateLimited        = apierr.RateLimited
	codeTokenBudget        = apierr.TokenBudget
	codeServerBusy         = apierr.ServerBusy
	codeCircuitOpen        = apierr.CircuitOpen
	codeBackendUnavailable = apierr.BackendUnavailable
	codeUpstreamError      = apierr.UpstreamError
	codeTimeout            = apierr.Timeout
	codeInternal           = apierr.Internal
)

// writeErr emits an OpenAI-shaped error for a taxonomy code, using that code's
// canonical status and type. This is the preferred path.
func writeErr(w http.ResponseWriter, code errorCode, msg string) {
	apierr.Write(w, code, msg)
}

// writeError is the status-based shim: it classifies the status through the
// taxonomy so every existing call site emits a correct {type, code}.
func writeError(w http.ResponseWriter, status int, msg string) {
	apierr.WriteStatus(w, status, msg)
}

// routeErrors wraps the routing mux so the paths it does *not* route are
// answered inside the taxonomy too, instead of by net/http's plain-text
// `404 page not found` and `405 Method Not Allowed` — the two responses a client
// branching on a stable `code` would otherwise get nothing from.
//
// Registering a catch-all at "/" is the shorter fix and the wrong one: a
// catch-all matches every method, so ServeMux would never again see a request
// whose path matched but whose method did not, and the automatic 405 would
// quietly become a 404. Instead the mux stays free of catch-alls and this
// wrapper decides. ServeMux reports 404 and 405 identically — an empty pattern —
// so the two are told apart by asking again for the other methods, which happens
// only on the error path. A routed request costs one extra tree lookup and no
// allocation.
func routeErrors(mux *http.ServeMux) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, pattern := mux.Handler(r); pattern != "" {
			// Routed — a trailing-slash redirect included, whose pattern names the
			// target it would redirect to. Dispatch through the mux itself rather
			// than the handler returned above: only ServeHTTP records the matched
			// pattern on the request, and without it r.PathValue("id") is empty.
			mux.ServeHTTP(w, r)
			return
		}
		if allow := allowedMethods(mux, r); allow != "" {
			w.Header().Set("Allow", allow)
			writeErr(w, codeMethodNotAllowed, "method not allowed: "+r.Method+" "+r.URL.Path)
			return
		}
		writeErr(w, codeRouteNotFound, "no route for "+r.Method+" "+r.URL.Path)
	})
}

// probeMethods are the methods an unmatched request is re-tried with to decide
// 404 vs 405. Everything Mainspring registers is one of these; a path routed
// only under some other method would be reported as a 404, which is what
// ServeMux itself would have answered before this wrapper existed.
var probeMethods = [...]string{
	http.MethodGet, http.MethodHead, http.MethodPost,
	http.MethodPut, http.MethodPatch, http.MethodDelete,
}

// allowedMethods returns the Allow header value for a path that some other
// method routes, or "" when nothing routes it at all — a genuine 404.
func allowedMethods(mux *http.ServeMux, r *http.Request) string {
	var allow []string
	for _, m := range probeMethods {
		if m == r.Method {
			continue // already known not to route: it is why we are here
		}
		probe := *r // shallow copy; only the method differs
		probe.Method = m
		if _, pattern := mux.Handler(&probe); pattern != "" {
			allow = append(allow, m)
		}
	}
	return strings.Join(allow, ", ")
}
