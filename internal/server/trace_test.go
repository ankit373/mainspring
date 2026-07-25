package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestParseTraceparent(t *testing.T) {
	valid := "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"
	tid, ok := parseTraceparent(valid)
	if !ok || tid != "4bf92f3577b34da6a3ce929d0e0e4736" {
		t.Fatalf("valid traceparent => %q,%v", tid, ok)
	}
	bad := []string{
		"",
		"garbage",
		"00-tooshort-00f067aa0ba902b7-01",
		"00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7",    // 3 fields
		"00-00000000000000000000000000000000-00f067aa0ba902b7-01", // all-zero trace id
		"00-4bf92f3577b34da6a3ce929d0e0e4736-0000000000000000-01", // all-zero span id
		"ff-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01", // forbidden version
		"00-4bf92f3577b34da6a3ce929d0e0e473g-00f067aa0ba902b7-01", // non-hex
	}
	for _, h := range bad {
		if _, ok := parseTraceparent(h); ok {
			t.Fatalf("should have rejected %q", h)
		}
	}
}

func TestWithTraceContinuesTrace(t *testing.T) {
	// An incoming valid traceparent's trace id is continued; a fresh span id is
	// minted, so the outgoing traceparent shares the trace id but not the parent.
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	r.Header.Set("traceparent", "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01")
	r = withTrace(r)
	if TraceID(r.Context()) != "4bf92f3577b34da6a3ce929d0e0e4736" {
		t.Fatalf("trace id not continued: %q", TraceID(r.Context()))
	}
	out := outgoingTraceparent(r.Context())
	if !strings.HasPrefix(out, "00-4bf92f3577b34da6a3ce929d0e0e4736-") {
		t.Fatalf("outgoing tp should keep trace id: %q", out)
	}
	if strings.Contains(out, "00f067aa0ba902b7") {
		t.Fatal("outgoing tp must use a fresh span id, not the incoming parent id")
	}
}

func TestWithTraceGeneratesWhenAbsent(t *testing.T) {
	r := withTrace(httptest.NewRequest(http.MethodGet, "/healthz", nil))
	tid := TraceID(r.Context())
	if len(tid) != 32 {
		t.Fatalf("generated trace id should be 32 hex chars, got %q", tid)
	}
	if !strings.HasPrefix(outgoingTraceparent(r.Context()), "00-"+tid+"-") {
		t.Fatalf("outgoing tp malformed: %q", outgoingTraceparent(r.Context()))
	}
}
