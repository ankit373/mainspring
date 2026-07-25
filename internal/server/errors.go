package server

import "net/http"

// This file defines Mainspring's error taxonomy: a small set of named classes,
// each with a stable HTTP status and an OpenAI-shaped {type, code}. Emitting all
// errors through it means clients and routers (Hydra) can branch on a stable
// `code` instead of parsing prose. Where one status has several distinct causes
// (503: busy vs circuit-open vs backend-down), the code disambiguates.

// errorCode is a stable, machine-branchable error identifier.
type errorCode string

const (
	codeInvalidRequest     errorCode = "invalid_request"
	codeMethodNotAllowed   errorCode = "method_not_allowed"
	codeModelNotFound      errorCode = "model_not_found"
	codeUnauthorized       errorCode = "unauthorized"
	codeForbidden          errorCode = "forbidden"
	codeRateLimited        errorCode = "rate_limited"
	codeTokenBudget        errorCode = "token_budget_exceeded"
	codeServerBusy         errorCode = "server_busy"
	codeCircuitOpen        errorCode = "circuit_open"
	codeBackendUnavailable errorCode = "backend_unavailable"
	codeUpstreamError      errorCode = "upstream_error"
	codeTimeout            errorCode = "timeout"
	codeInternal           errorCode = "internal_error"
)

// codeMeta maps each code to its HTTP status and OpenAI-compatible error `type`.
var codeMeta = map[errorCode]struct {
	status int
	typ    string
}{
	codeInvalidRequest:     {http.StatusBadRequest, "invalid_request_error"},
	codeMethodNotAllowed:   {http.StatusMethodNotAllowed, "invalid_request_error"},
	codeModelNotFound:      {http.StatusNotFound, "not_found_error"},
	codeUnauthorized:       {http.StatusUnauthorized, "authentication_error"},
	codeForbidden:          {http.StatusForbidden, "permission_error"},
	codeRateLimited:        {http.StatusTooManyRequests, "rate_limit_error"},
	codeTokenBudget:        {http.StatusTooManyRequests, "rate_limit_error"},
	codeServerBusy:         {http.StatusServiceUnavailable, "overloaded_error"},
	codeCircuitOpen:        {http.StatusServiceUnavailable, "backend_unavailable_error"},
	codeBackendUnavailable: {http.StatusServiceUnavailable, "backend_unavailable_error"},
	codeUpstreamError:      {http.StatusBadGateway, "upstream_error"},
	codeTimeout:            {http.StatusGatewayTimeout, "timeout_error"},
	codeInternal:           {http.StatusInternalServerError, "api_error"},
}

// writeErr emits an OpenAI-shaped error for a taxonomy code, using that code's
// canonical status and type. This is the preferred path.
func writeErr(w http.ResponseWriter, code errorCode, msg string) {
	m, ok := codeMeta[code]
	if !ok {
		m = codeMeta[codeInternal]
	}
	writeJSON(w, m.status, map[string]any{
		"error": map[string]string{"message": msg, "type": m.typ, "code": string(code)},
	})
}

// statusToCode maps a bare HTTP status to the closest taxonomy code, so the
// legacy writeError(status, msg) call sites emit a correct type/code instead of
// a hardcoded one.
func statusToCode(status int) errorCode {
	switch status {
	case http.StatusBadRequest:
		return codeInvalidRequest
	case http.StatusMethodNotAllowed:
		return codeMethodNotAllowed
	case http.StatusNotFound:
		return codeModelNotFound
	case http.StatusUnauthorized:
		return codeUnauthorized
	case http.StatusForbidden:
		return codeForbidden
	case http.StatusTooManyRequests:
		return codeRateLimited
	case http.StatusBadGateway:
		return codeUpstreamError
	case http.StatusServiceUnavailable:
		return codeBackendUnavailable
	case http.StatusGatewayTimeout:
		return codeTimeout
	default:
		return codeInternal
	}
}

// writeError is the status-based shim: it classifies the status through the
// taxonomy so every existing call site emits a correct {type, code}.
func writeError(w http.ResponseWriter, status int, msg string) {
	writeErr(w, statusToCode(status), msg)
}
