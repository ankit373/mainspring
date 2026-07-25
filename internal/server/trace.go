package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"strings"
)

// This file implements W3C Trace Context propagation (traceparent) without an
// OpenTelemetry SDK dependency — keeping Mainspring stdlib-only. It accepts an
// incoming traceparent (or generates one), records the trace id alongside the
// request id in the ledger/access log, and forwards a fresh child traceparent to
// the upstream engine so a distributed trace stitches together end to end.

const traceKey ctxKey = 1

// traceState is the propagated trace identity for a request.
type traceState struct {
	traceID    string // 32 hex chars (16 bytes)
	outgoingTP string // traceparent to forward upstream (our span as parent)
}

// TraceID returns the W3C trace id for the request ("" if none).
func TraceID(ctx context.Context) string {
	if ts, ok := ctx.Value(traceKey).(traceState); ok {
		return ts.traceID
	}
	return ""
}

// outgoingTraceparent returns the traceparent header to forward upstream.
func outgoingTraceparent(ctx context.Context) string {
	if ts, ok := ctx.Value(traceKey).(traceState); ok {
		return ts.outgoingTP
	}
	return ""
}

// withTrace derives the trace state for a request: it reuses the incoming
// traceparent's trace id when valid (continuing the distributed trace), else
// starts a new trace. Either way it mints a fresh span id for this hop and
// builds the child traceparent to send upstream.
func withTrace(r *http.Request) *http.Request {
	traceID, ok := parseTraceparent(r.Header.Get("traceparent"))
	if !ok {
		traceID = randomHex(16)
	}
	spanID := randomHex(8)
	ts := traceState{
		traceID:    traceID,
		outgoingTP: "00-" + traceID + "-" + spanID + "-01",
	}
	return r.WithContext(context.WithValue(r.Context(), traceKey, ts))
}

// parseTraceparent validates a W3C traceparent header and returns its trace id.
// Format: version(2) "-" trace-id(32) "-" parent-id(16) "-" flags(2), all hex.
// An all-zero trace id is invalid per the spec.
func parseTraceparent(h string) (traceID string, ok bool) {
	parts := strings.Split(strings.TrimSpace(h), "-")
	if len(parts) != 4 {
		return "", false
	}
	version, tid, pid, flags := parts[0], parts[1], parts[2], parts[3]
	if len(version) != 2 || len(tid) != 32 || len(pid) != 16 || len(flags) != 2 {
		return "", false
	}
	if version == "ff" { // invalid/forbidden version
		return "", false
	}
	for _, p := range []string{version, tid, pid, flags} {
		if !isHex(p) {
			return "", false
		}
	}
	if tid == "00000000000000000000000000000000" || pid == "0000000000000000" {
		return "", false
	}
	return tid, true
}

func isHex(s string) bool {
	_, err := hex.DecodeString(s)
	return err == nil
}

// randomHex returns n random bytes as a lowercase hex string. crypto/rand does
// not fail on the platforms we target.
func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
