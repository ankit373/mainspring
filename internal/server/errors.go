package server

import (
	"net/http"

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
