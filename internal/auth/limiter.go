package auth

import (
	"sync"
	"time"
)

// Limiter provides per-key fixed-window request-rate and token-budget accounting.
// Fixed windows are simple and predictable; a token bucket can replace this later
// without changing the auth surface.
type Limiter struct {
	mu     sync.Mutex
	reqWin map[string]*counter // 60s request window
	tokWin map[string]*counter // token-budget window (per-tenant length)
}

type counter struct {
	start time.Time
	n     int64
}

// NewLimiter builds an empty Limiter.
func NewLimiter() *Limiter {
	return &Limiter{reqWin: make(map[string]*counter), tokWin: make(map[string]*counter)}
}

// AllowRequest reports whether a request is within the per-minute rate for key,
// and if not, how long until the window resets.
func (l *Limiter) AllowRequest(key string, rpm int) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	c := l.reqWin[key]
	if c == nil || now.Sub(c.start) >= time.Minute {
		l.reqWin[key] = &counter{start: now, n: 1}
		return true, 0
	}
	if c.n >= int64(rpm) {
		return false, time.Minute - now.Sub(c.start)
	}
	c.n++
	return true, 0
}

// WithinTokenBudget reports whether the current window's token usage for key is
// below budget. An expired/empty window is always within budget.
func (l *Limiter) WithinTokenBudget(key string, budget int64, windowSec int) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	c := l.tokWin[key]
	if c == nil || time.Since(c.start) >= time.Duration(windowSec)*time.Second {
		return true
	}
	return c.n < budget
}

// AddTokens accrues n tokens against key's current window (starting a fresh one
// if the previous expired).
func (l *Limiter) AddTokens(key string, n int64, windowSec int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	c := l.tokWin[key]
	if c == nil || time.Since(c.start) >= time.Duration(windowSec)*time.Second {
		l.tokWin[key] = &counter{start: time.Now(), n: n}
		return
	}
	c.n += n
}
