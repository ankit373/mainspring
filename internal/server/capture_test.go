package server

import (
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// writeAll pushes p through the capture writer as the proxy would.
func feed(c *captureWriter, chunks ...string) {
	for _, s := range chunks {
		_, _ = c.Write([]byte(s))
	}
}

func TestUsageNonStreamExact(t *testing.T) {
	rr := httptest.NewRecorder()
	c := newCapture(rr, time.Now())
	c.Header().Set("Content-Type", "application/json")
	feed(c, `{"choices":[{"message":{"content":"hi"}}],"usage":{"prompt_tokens":11,"completion_tokens":7}}`)
	p, cmp, exact := c.usage()
	if !exact || p != 11 || cmp != 7 {
		t.Fatalf("usage=(%d,%d,%v), want (11,7,true)", p, cmp, exact)
	}
}

func TestUsageStreamIncludeUsage(t *testing.T) {
	rr := httptest.NewRecorder()
	c := newCapture(rr, time.Now())
	c.Header().Set("Content-Type", "text/event-stream")
	feed(c,
		"data: {\"choices\":[{\"delta\":{\"content\":\"Hel\"}}]}\n\n",
		"data: {\"choices\":[{\"delta\":{\"content\":\"lo\"}}]}\n\n",
		"data: {\"choices\":[],\"usage\":{\"prompt_tokens\":20,\"completion_tokens\":2}}\n\n",
		"data: [DONE]\n\n",
	)
	p, cmp, exact := c.usage()
	if !exact || p != 20 || cmp != 2 {
		t.Fatalf("stream usage=(%d,%d,%v), want (20,2,true)", p, cmp, exact)
	}
}

func TestUsageStreamEstimateFallback(t *testing.T) {
	rr := httptest.NewRecorder()
	c := newCapture(rr, time.Now())
	c.Header().Set("Content-Type", "text/event-stream")
	feed(c,
		"data: {\"choices\":[{\"delta\":{\"content\":\"a\"}}]}\n\n",
		"data: {\"choices\":[{\"delta\":{\"content\":\"b\"}}]}\n\n",
		"data: [DONE]\n\n",
	)
	p, cmp, exact := c.usage()
	if exact || p != 0 || cmp != 2 { // 3 data frames minus [DONE]
		t.Fatalf("estimate usage=(%d,%d,%v), want (0,2,false)", p, cmp, exact)
	}
}

// TestUsageTailBeyond4KB guards the rolling-tail fix: a usage object at the end
// of a large body must still be read (a prefix window would have missed it).
func TestUsageTailBeyond4KB(t *testing.T) {
	rr := httptest.NewRecorder()
	c := newCapture(rr, time.Now())
	c.Header().Set("Content-Type", "application/json")
	big := strings.Repeat("x", 10000)
	feed(c, `{"content":"`+big+`","usage":{"prompt_tokens":3,"completion_tokens":9}}`)
	p, cmp, exact := c.usage()
	if !exact || p != 3 || cmp != 9 {
		t.Fatalf("large-body usage=(%d,%d,%v), want (3,9,true)", p, cmp, exact)
	}
}
