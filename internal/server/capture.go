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
}

func newCapture(w http.ResponseWriter, start time.Time) *captureWriter {
	fl, _ := w.(http.Flusher)
	return &captureWriter{ResponseWriter: w, flusher: fl, start: start, status: http.StatusOK}
}

func (c *captureWriter) WriteHeader(code int) {
	if c.wroteHeader {
		return
	}
	c.status = code
	c.wroteHeader = true
	c.stream = bytes.Contains([]byte(c.Header().Get("Content-Type")), []byte("event-stream"))
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
	} else if len(c.tail) < tailCap {
		c.tail = append(c.tail, p...)
		if len(c.tail) > tailCap {
			c.tail = c.tail[:tailCap]
		}
	}
	return c.ResponseWriter.Write(p)
}

// Flush forwards to the underlying flusher so SSE streaming still works.
func (c *captureWriter) Flush() {
	if c.flusher != nil {
		c.flusher.Flush()
	}
}

var completionTokensRe = regexp.MustCompile(`"completion_tokens"\s*:\s*(\d+)`)

// tokensEstimate returns a rough output-token count: parsed usage for
// non-streaming responses, else SSE data-frame count minus the [DONE] frame.
func (c *captureWriter) tokensEstimate() int64 {
	if !c.stream {
		if m := completionTokensRe.FindSubmatch(c.tail); m != nil {
			if n, err := strconv.ParseInt(string(m[1]), 10, 64); err == nil {
				return n
			}
		}
		return 0
	}
	if c.sseFrames > 1 {
		return c.sseFrames - 1 // subtract the [DONE] frame
	}
	return 0
}

// ttftMs is the time-to-first-token in ms (streaming only; 0 otherwise).
func (c *captureWriter) ttftMs() float64 {
	if !c.stream || c.firstByte.IsZero() {
		return 0
	}
	return float64(c.firstByte.Sub(c.start).Microseconds()) / 1000.0
}
