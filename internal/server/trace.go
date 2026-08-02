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

// sampledFlags is the trace-flags byte for a trace Mainspring mints itself:
// there is no upstream decision to respect, so record it.
const sampledFlags = "01"

// withTrace derives the trace state for a request: it reuses the incoming
// traceparent's trace id and trace flags when valid (continuing the distributed
// trace, and honouring the caller's sampling decision — forcing 01 would sample
// downstream a trace the caller explicitly opted out of), else starts a new
// sampled trace. Either way it mints a fresh span id for this hop and builds the
// child traceparent to send upstream.
func withTrace(r *http.Request) *http.Request {
	traceID, flags, ok := parseTraceparent(r.Header.Get("traceparent"))
	if !ok {
		traceID, flags = randomHex(16), sampledFlags
	}
	spanID := randomHex(8)
	ts := traceState{
		traceID:    traceID,
		outgoingTP: "00-" + traceID + "-" + spanID + "-" + flags,
	}
	return r.WithContext(context.WithValue(r.Context(), traceKey, ts))
}

// parseTraceparent validates a W3C traceparent header and returns its trace id
// and trace flags. Format: version(2) "-" trace-id(32) "-" parent-id(16) "-"
// flags(2), all hex. An all-zero trace id is invalid per the spec.
func parseTraceparent(h string) (traceID, flags string, ok bool) {
	// The spec defines the field as lowercase hex; normalising here keeps the
	// forwarded child traceparent canonical whatever case the caller sent.
	parts := strings.Split(strings.ToLower(strings.TrimSpace(h)), "-")
	if len(parts) != 4 {
		return "", "", false
	}
	version, tid, pid, fl := parts[0], parts[1], parts[2], parts[3]
	if len(version) != 2 || len(tid) != 32 || len(pid) != 16 || len(fl) != 2 {
		return "", "", false
	}
	if version == "ff" { // invalid/forbidden version
		return "", "", false
	}
	for _, p := range []string{version, tid, pid, fl} {
		if !isHex(p) {
			return "", "", false
		}
	}
	if tid == "00000000000000000000000000000000" || pid == "0000000000000000" {
		return "", "", false
	}
	return tid, fl, true
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
