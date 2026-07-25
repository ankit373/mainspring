package server

import (
	"context"
	"testing"
)

func TestSanitizeRequestID(t *testing.T) {
	if got := sanitizeRequestID("abc-123_XYZ"); got != "abc-123_XYZ" {
		t.Fatalf("clean id rejected: %q", got)
	}
	if sanitizeRequestID("bad\nid") != "" {
		t.Fatal("newline must be rejected (log/header injection)")
	}
	if sanitizeRequestID("bad\x00id") != "" {
		t.Fatal("control char must be rejected")
	}
	long := make([]byte, maxRequestIDLen+1)
	for i := range long {
		long[i] = 'a'
	}
	if sanitizeRequestID(string(long)) != "" {
		t.Fatal("over-length id must be rejected")
	}
}

func TestNewRequestIDUnique(t *testing.T) {
	a, b := newRequestID(), newRequestID()
	if a == b || a == "req_" || len(a) <= 4 {
		t.Fatalf("ids not unique/formed: %q %q", a, b)
	}
}

func TestRequestIDContextRoundTrip(t *testing.T) {
	ctx := context.WithValue(context.Background(), requestIDKey, "req_test")
	if RequestID(ctx) != "req_test" {
		t.Fatal("RequestID must read back the stored id")
	}
	if RequestID(context.Background()) != "" {
		t.Fatal("missing id should be empty string")
	}
}
