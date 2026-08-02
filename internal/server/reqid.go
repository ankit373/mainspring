package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/ankit373/mainspring/internal/auth"
)

// This file implements request correlation: every request carries an
// X-Request-ID (accepted from the client or generated), which is echoed on the
// response, stored in the context (so handlers can attach it to the usage
// ledger), and — when an access log is configured — written as one JSONL line
// naming the authenticated tenant, so an admin action can be attributed.

type ctxKey int

const requestIDKey ctxKey = 0

// maxRequestIDLen bounds a client-supplied id so it cannot bloat logs/headers.
const maxRequestIDLen = 200

// RequestID returns the correlation id for a request context ("" if none).
func RequestID(ctx context.Context) string {
	id, _ := ctx.Value(requestIDKey).(string)
	return id
}

// newRequestID returns a random correlation id. crypto/rand never fails on the
// platforms we target; on the impossible error path we still return a usable id.
func newRequestID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return "req_" + hex.EncodeToString(b[:])
}

// sanitizeRequestID accepts a client id only if it is short and free of control
// characters (prevents header/log injection). Otherwise it returns "".
func sanitizeRequestID(s string) string {
	if s == "" || len(s) > maxRequestIDLen {
		return ""
	}
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return ""
		}
	}
	return s
}

// requestID is the outermost middleware: it assigns/propagates the correlation
// id, echoes it, and (if enabled) emits a structured access-log line covering
// every request — including health checks and auth rejections.
func (s *Server) requestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := sanitizeRequestID(r.Header.Get("X-Request-ID"))
		if id == "" {
			id = newRequestID()
		}
		w.Header().Set("X-Request-ID", id)
		r = r.WithContext(context.WithValue(r.Context(), requestIDKey, id))
		// Establish/continue the W3C trace context for this request.
		r = withTrace(r)

		al := s.accessLog()
		if al == nil {
			next.ServeHTTP(w, r)
			return
		}
		// An audit trail needs a principal. This middleware wraps authentication
		// (so rejections and health checks are logged too) and therefore cannot
		// see the resolved tenant on the way in — it installs an empty slot that
		// auth.Wrap fills as the request passes through, and reads it back below.
		r = r.WithContext(auth.NewPrincipalSlot(r.Context()))
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		al.log(accessEntry{
			Time:       start,
			RequestID:  id,
			TraceID:    TraceID(r.Context()),
			Tenant:     auth.Principal(r.Context()),
			Method:     r.Method,
			Path:       r.URL.Path,
			Status:     rec.status,
			DurationMs: float64(time.Since(start).Microseconds()) / 1000.0,
			Bytes:      rec.bytes,
		})
	})
}

// statusRecorder captures the status code and byte count while forwarding Flush
// so SSE streaming still works through the middleware.
type statusRecorder struct {
	http.ResponseWriter
	status      int
	bytes       int64
	wroteHeader bool
}

func (r *statusRecorder) WriteHeader(code int) {
	if r.wroteHeader {
		return
	}
	r.status = code
	r.wroteHeader = true
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Write(p []byte) (int, error) {
	if !r.wroteHeader {
		r.WriteHeader(http.StatusOK)
	}
	n, err := r.ResponseWriter.Write(p)
	r.bytes += int64(n)
	return n, err
}

func (r *statusRecorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// accessEntry is one structured access-log record. Tenant names the
// authenticated principal (omitted for open mode, exempt paths, and rejected
// keys), which is what makes the log an audit trail rather than a traffic log.
type accessEntry struct {
	Time       time.Time `json:"time"`
	RequestID  string    `json:"request_id"`
	TraceID    string    `json:"trace_id,omitempty"`
	Tenant     string    `json:"tenant,omitempty"`
	Method     string    `json:"method"`
	Path       string    `json:"path"`
	Status     int       `json:"status"`
	DurationMs float64   `json:"duration_ms"`
	Bytes      int64     `json:"bytes"`
}

// accessLogger writes access entries as JSONL, serializing writes.
type accessLogger struct {
	mu sync.Mutex
	w  io.Writer
}

func (a *accessLogger) log(e accessEntry) {
	b, err := json.Marshal(e)
	if err != nil {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	_, _ = a.w.Write(append(b, '\n'))
}
