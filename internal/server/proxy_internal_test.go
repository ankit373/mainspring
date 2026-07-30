package server

import (
	"context"
	"net/http"
	"testing"
	"time"
)

func TestRetryableStatus(t *testing.T) {
	for _, c := range []struct {
		code int
		want bool
	}{
		{http.StatusBadGateway, true},
		{http.StatusServiceUnavailable, true},
		{http.StatusGatewayTimeout, true},
		{http.StatusInternalServerError, false}, // may be deterministic
		{http.StatusOK, false},
		{http.StatusBadRequest, false},
	} {
		if got := retryableStatus(c.code); got != c.want {
			t.Errorf("retryableStatus(%d) = %v, want %v", c.code, got, c.want)
		}
	}
}

func TestSleepBackoff(t *testing.T) {
	// Zero base returns immediately (true) while ctx is live.
	if !sleepBackoff(context.Background(), 0, 1) {
		t.Fatal("zero-base backoff should return true")
	}
	// A cancelled context returns false without waiting the full delay.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if sleepBackoff(ctx, time.Hour, 3) {
		t.Fatal("cancelled context should abort backoff (false)")
	}
	// A real short wait completes.
	if !sleepBackoff(context.Background(), time.Millisecond, 1) {
		t.Fatal("short backoff should complete true")
	}
}
