package server

import (
	"encoding/json"
	"net/http/httptest"
	"testing"
)

func TestEveryCodeHasMeta(t *testing.T) {
	all := []errorCode{
		codeInvalidRequest, codeMethodNotAllowed, codeModelNotFound, codeUnauthorized,
		codeForbidden, codeRateLimited, codeTokenBudget, codeServerBusy, codeCircuitOpen,
		codeBackendUnavailable, codeUpstreamError, codeTimeout, codeInternal,
	}
	for _, c := range all {
		m, ok := codeMeta[c]
		if !ok || m.status == 0 || m.typ == "" {
			t.Fatalf("code %q missing status/type in codeMeta", c)
		}
	}
}

func TestWriteErrShape(t *testing.T) {
	w := httptest.NewRecorder()
	writeErr(w, codeCircuitOpen, "boom")
	if w.Code != 503 {
		t.Fatalf("circuit_open status = %d, want 503", w.Code)
	}
	var body struct {
		Error struct {
			Message, Type, Code string
		}
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Error.Code != "circuit_open" || body.Error.Type != "backend_unavailable_error" || body.Error.Message != "boom" {
		t.Fatalf("unexpected error body: %+v", body.Error)
	}
}

func TestStatusToCodeMapping(t *testing.T) {
	cases := map[int]errorCode{
		400: codeInvalidRequest,
		404: codeModelNotFound,
		401: codeUnauthorized,
		403: codeForbidden,
		429: codeRateLimited,
		502: codeUpstreamError,
		503: codeBackendUnavailable,
		504: codeTimeout,
		418: codeInternal, // unknown => internal
	}
	for status, want := range cases {
		if got := statusToCode(status); got != want {
			t.Fatalf("statusToCode(%d) = %q, want %q", status, got, want)
		}
	}
}

// legacy writeError must now emit a correct type/code, not a hardcoded one.
func TestLegacyWriteErrorClassifies(t *testing.T) {
	w := httptest.NewRecorder()
	writeError(w, 404, "nope")
	var body struct {
		Error struct{ Type, Code string }
	}
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	if body.Error.Type != "not_found_error" || body.Error.Code != "model_not_found" {
		t.Fatalf("legacy writeError(404) should classify as not_found: %+v", body.Error)
	}
}
