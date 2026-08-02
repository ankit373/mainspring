package server

import (
	"encoding/json"
	"net/http/httptest"
	"testing"
)

// The taxonomy itself is tested in internal/apierr. What matters here is that
// this package's helpers still route through it and emit the same shape.

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

// legacy writeError must emit a correct type/code, not a hardcoded one.
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
