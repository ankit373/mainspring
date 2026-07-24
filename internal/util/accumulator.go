// Package util holds small shared helpers.
package util

import "sync"

// DefaultCap bounds captured subprocess output at 4 MiB. Runner logs are for
// diagnostics, not payloads — an engine that floods stderr must never OOM us.
const DefaultCap = 4 << 20

// Accumulator is a bounded, concurrency-safe io.Writer for capturing subprocess
// stdout/stderr. Writes past the cap are counted but discarded, and Truncated
// reports whether that happened. This is the ONLY writer permitted for
// unbounded subprocess output — never a raw bytes.Buffer.
type Accumulator struct {
	mu        sync.Mutex
	buf       []byte
	cap       int
	total     int
	truncated bool
}

// NewAccumulator returns an Accumulator capped at capBytes (<=0 uses DefaultCap).
func NewAccumulator(capBytes int) *Accumulator {
	if capBytes <= 0 {
		capBytes = DefaultCap
	}
	return &Accumulator{cap: capBytes}
}

// Write appends up to the remaining capacity, dropping the overflow.
func (a *Accumulator) Write(p []byte) (int, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.total += len(p)
	if room := a.cap - len(a.buf); room > 0 {
		if len(p) > room {
			a.buf = append(a.buf, p[:room]...)
			a.truncated = true
		} else {
			a.buf = append(a.buf, p...)
		}
	} else if len(p) > 0 {
		a.truncated = true
	}
	// Always report the full length written so callers (io.Copy) don't error.
	return len(p), nil
}

// String returns the captured (possibly truncated) output.
func (a *Accumulator) String() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return string(a.buf)
}

// Truncated reports whether any output was dropped at the cap.
func (a *Accumulator) Truncated() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.truncated
}
