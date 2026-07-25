package server

import (
	"bytes"
	"net/http"
	"regexp"
	"strconv"
	"time"
)

// tailCap bounds the trailing bytes kept from a non-streaming response so we can
// sniff usage.completion_tokens without buffering the whole body.
const tailCap = 4096

// captureWriter wraps a ResponseWriter to observe timing, size, and a rough
// output-token estimate for metrics — without altering what the client sees.
type captureWriter struct {
	http.ResponseWriter
	flusher     http.Flusher
	start       time.Time
	firstByte   time.Time
	status      int
	wroteHeader bool
	stream      bool
	bytes       int64
	sseFrames   int64
	tail        []byte

	// Optional full-body recording for the response cache. Enabled by
	// recordBody; body accumulates until it exceeds bodyCap, at which point it is
	// dropped (bodyOver=true) so the response is never cached. snapHeader is the
	// response header snapshot taken when the status line is written.
	recordBody bool
	bodyCap    int
	bodyOver   bool
	body       []byte
	snapHeader http.Header
}

func newCapture(w http.ResponseWriter, start time.Time) *captureWriter {
	fl, _ := w.(http.Flusher)
	return &captureWriter{ResponseWriter: w, flusher: fl, start: start, status: http.StatusOK}
}

// recordFor enables full-body capture up to cap bytes so a successful
// non-streaming response can be stored in the cache.
func (c *captureWriter) recordFor(cap int) {
	c.recordBody = true
	c.bodyCap = cap
}

func (c *captureWriter) WriteHeader(code int) {
	if c.wroteHeader {
		return
	}
	c.status = code
	c.wroteHeader = true
	c.stream = bytes.Contains([]byte(c.Header().Get("Content-Type")), []byte("event-stream"))
	if c.recordBody {
		c.snapHeader = c.Header().Clone()
	}
	c.ResponseWriter.WriteHeader(code)
}

func (c *captureWriter) Write(p []byte) (int, error) {
	if !c.wroteHeader {
		c.WriteHeader(http.StatusOK)
	}
	if c.firstByte.IsZero() && len(p) > 0 {
		c.firstByte = time.Now()
	}
	c.bytes += int64(len(p))
	if c.stream {
		c.sseFrames += int64(bytes.Count(p, []byte("data:")))
	}
	// Keep a rolling window of the last tailCap bytes for both modes. usage is at
	// the end of a non-stream body and in the final include_usage SSE chunk, so a
	// trailing window (not a prefix) is what lets us read it without buffering all.
	c.appendTail(p)
	// Full-body capture for the cache: accumulate until over cap, then drop.
	if c.recordBody && !c.bodyOver {
		if len(c.body)+len(p) > c.bodyCap {
			c.bodyOver = true
			c.body = nil
		} else {
			c.body = append(c.body, p...)
		}
	}
	return c.ResponseWriter.Write(p)
}

// appendTail keeps the last tailCap bytes seen, with bounded backing memory.
func (c *captureWriter) appendTail(p []byte) {
	if len(p) >= tailCap {
		c.tail = append(c.tail[:0], p[len(p)-tailCap:]...)
		return
	}
	c.tail = append(c.tail, p...)
	if len(c.tail) > tailCap {
		c.tail = append(c.tail[:0], c.tail[len(c.tail)-tailCap:]...)
	}
}

// Flush forwards to the underlying flusher so SSE streaming still works.
func (c *captureWriter) Flush() {
	if c.flusher != nil {
		c.flusher.Flush()
	}
}

var (
	completionTokensRe = regexp.MustCompile(`"completion_tokens"\s*:\s*(\d+)`)
	promptTokensRe     = regexp.MustCompile(`"prompt_tokens"\s*:\s*(\d+)`)
)

// usage returns the request's token usage. When the upstream reported a usage
// object — in the non-stream body, or in the streaming final chunk emitted for
// stream_options.include_usage — the real prompt/completion counts are returned
// with exact=true. Otherwise completion falls back to the SSE frame estimate and
// prompt is unknown (0), exact=false.
func (c *captureWriter) usage() (prompt, completion int64, exact bool) {
	if m := completionTokensRe.FindSubmatch(c.tail); m != nil {
		if n, err := strconv.ParseInt(string(m[1]), 10, 64); err == nil {
			completion, exact = n, true
		}
	}
	if m := promptTokensRe.FindSubmatch(c.tail); m != nil {
		if n, err := strconv.ParseInt(string(m[1]), 10, 64); err == nil {
			prompt = n
		}
	}
	if exact {
		return prompt, completion, true
	}
	if c.stream && c.sseFrames > 1 {
		return 0, c.sseFrames - 1, false // subtract the [DONE] frame
	}
	return 0, 0, false
}

// ttftMs is the time-to-first-token in ms (streaming only; 0 otherwise).
func (c *captureWriter) ttftMs() float64 {
	if !c.stream || c.firstByte.IsZero() {
		return 0
	}
	return float64(c.firstByte.Sub(c.start).Microseconds()) / 1000.0
}
