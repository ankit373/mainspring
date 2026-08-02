// Package apierr defines Mainspring's error taxonomy — a small set of named
// classes, each with a stable HTTP status and an OpenAI-shaped {type, code} —
// and the single writer that emits it. Emitting all errors through it means
// clients and routers (Hydra) can branch on a stable `code` instead of parsing
// prose. Where one status has several distinct causes (503: busy vs
// circuit-open vs backend-down), the code disambiguates.
//
// It sits below both the HTTP handlers and the auth middleware, because the
// middleware rejects requests (401, 429) before any handler runs: without a
// shared home the wire shape of an error would be maintained in two places.
package apierr

import (
	"encoding/json"
	"net/http"
)

// Code is a stable, machine-branchable error identifier.
type Code string

const (
	InvalidRequest     Code = "invalid_request"
	ContextLength      Code = "context_length_exceeded"
	MethodNotAllowed   Code = "method_not_allowed"
	ModelNotFound      Code = "model_not_found"
	RouteNotFound      Code = "route_not_found" // no such endpoint — not "no such model"
	Unauthorized       Code = "unauthorized"
	Forbidden          Code = "forbidden"
	RateLimited        Code = "rate_limited"
	TokenBudget        Code = "token_budget_exceeded"
	ServerBusy         Code = "server_busy"
	CircuitOpen        Code = "circuit_open"
	BackendUnavailable Code = "backend_unavailable"
	UpstreamError      Code = "upstream_error"
	Timeout            Code = "timeout"
	Internal           Code = "internal_error"
)

// codeMeta maps each code to its HTTP status and OpenAI-compatible error `type`.
var codeMeta = map[Code]struct {
	status int
	typ    string
}{
	InvalidRequest:     {http.StatusBadRequest, "invalid_request_error"},
	ContextLength:      {http.StatusBadRequest, "invalid_request_error"},
	MethodNotAllowed:   {http.StatusMethodNotAllowed, "invalid_request_error"},
	ModelNotFound:      {http.StatusNotFound, "not_found_error"},
	RouteNotFound:      {http.StatusNotFound, "not_found_error"},
	Unauthorized:       {http.StatusUnauthorized, "authentication_error"},
	Forbidden:          {http.StatusForbidden, "permission_error"},
	RateLimited:        {http.StatusTooManyRequests, "rate_limit_error"},
	TokenBudget:        {http.StatusTooManyRequests, "rate_limit_error"},
	ServerBusy:         {http.StatusServiceUnavailable, "overloaded_error"},
	CircuitOpen:        {http.StatusServiceUnavailable, "backend_unavailable_error"},
	BackendUnavailable: {http.StatusServiceUnavailable, "backend_unavailable_error"},
	UpstreamError:      {http.StatusBadGateway, "upstream_error"},
	Timeout:            {http.StatusGatewayTimeout, "timeout_error"},
	Internal:           {http.StatusInternalServerError, "api_error"},
}

// Meta returns a code's canonical HTTP status and OpenAI-compatible error
// `type`, falling back to Internal for a code that is not in the taxonomy.
func Meta(c Code) (status int, typ string) {
	m, ok := codeMeta[c]
	if !ok {
		m = codeMeta[Internal]
	}
	return m.status, m.typ
}

// Body returns the OpenAI-shaped error payload for a code. Callers that must
// embed the error in another transport (an SSE `error` frame, say) use this
// rather than re-assembling the shape.
func Body(c Code, msg string) map[string]any {
	_, typ := Meta(c)
	return map[string]any{
		"error": map[string]string{"message": msg, "type": typ, "code": string(c)},
	}
}

// Write emits an OpenAI-shaped error for a taxonomy code, using that code's
// canonical status and type. This is the preferred path, and the only place an
// HTTP error body is written.
func Write(w http.ResponseWriter, c Code, msg string) {
	status, _ := Meta(c)
	w.Header().Set("content-type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(Body(c, msg))
}

// FromStatus maps a bare HTTP status to the closest taxonomy code, so the
// status-based call sites emit a correct type/code instead of a hardcoded one.
func FromStatus(status int) Code {
	switch status {
	case http.StatusBadRequest:
		return InvalidRequest
	case http.StatusMethodNotAllowed:
		return MethodNotAllowed
	case http.StatusNotFound:
		return ModelNotFound
	case http.StatusUnauthorized:
		return Unauthorized
	case http.StatusForbidden:
		return Forbidden
	case http.StatusTooManyRequests:
		return RateLimited
	case http.StatusBadGateway:
		return UpstreamError
	case http.StatusServiceUnavailable:
		return BackendUnavailable
	case http.StatusGatewayTimeout:
		return Timeout
	default:
		return Internal
	}
}

// WriteStatus is the status-based shim: it classifies the status through the
// taxonomy so every status-only call site emits a correct {type, code}.
func WriteStatus(w http.ResponseWriter, status int, msg string) {
	Write(w, FromStatus(status), msg)
}
