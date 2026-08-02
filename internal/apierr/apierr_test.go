package apierr

import (
	"encoding/json"
	"net/http/httptest"
	"testing"
)

func TestEveryCodeHasMeta(t *testing.T) {
	all := []Code{
		InvalidRequest, ContextLength, MethodNotAllowed, ModelNotFound, Unauthorized,
		Forbidden, RateLimited, TokenBudget, ServerBusy, CircuitOpen,
		BackendUnavailable, UpstreamError, Timeout, Internal,
	}
	for _, c := range all {
		if _, ok := codeMeta[c]; !ok {
			t.Fatalf("code %q missing from codeMeta", c)
		}
		if status, typ := Meta(c); status == 0 || typ == "" {
			t.Fatalf("code %q has status=%d type=%q", c, status, typ)
		}
	}
}

func TestMetaFallsBackToInternal(t *testing.T) {
	status, typ := Meta(Code("not_a_real_code"))
	wantStatus, wantTyp := Meta(Internal)
	if status != wantStatus || typ != wantTyp {
		t.Fatalf("unknown code = (%d, %q), want internal (%d, %q)", status, typ, wantStatus, wantTyp)
	}
}

func TestWriteShape(t *testing.T) {
	w := httptest.NewRecorder()
	Write(w, CircuitOpen, "boom")
	if w.Code != 503 {
		t.Fatalf("circuit_open status = %d, want 503", w.Code)
	}
	if ct := w.Header().Get("content-type"); ct != "application/json" {
		t.Fatalf("content-type = %q, want application/json", ct)
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

func TestFromStatusMapping(t *testing.T) {
	cases := map[int]Code{
		400: InvalidRequest,
		404: ModelNotFound,
		401: Unauthorized,
		403: Forbidden,
		405: MethodNotAllowed,
		429: RateLimited,
		502: UpstreamError,
		503: BackendUnavailable,
		504: Timeout,
		418: Internal, // unknown => internal
	}
	for status, want := range cases {
		if got := FromStatus(status); got != want {
			t.Fatalf("FromStatus(%d) = %q, want %q", status, got, want)
		}
	}
}

func TestWriteStatusClassifies(t *testing.T) {
	w := httptest.NewRecorder()
	WriteStatus(w, 404, "nope")
	var body struct {
		Error struct{ Type, Code string }
	}
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	if body.Error.Type != "not_found_error" || body.Error.Code != "model_not_found" {
		t.Fatalf("WriteStatus(404) should classify as not_found: %+v", body.Error)
	}
}

// Body is what the SSE/stream paths embed; it must match what Write emits.
func TestBodyMatchesWrite(t *testing.T) {
	want, err := json.Marshal(Body(Timeout, "too slow"))
	if err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	Write(w, Timeout, "too slow")
	if got := w.Body.String(); got != string(want)+"\n" {
		t.Fatalf("Write = %q, Body = %q", got, want)
	}
}
