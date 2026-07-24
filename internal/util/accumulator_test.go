package util

import (
	"strings"
	"sync"
	"testing"
)

func TestAccumulatorCapsAndTruncates(t *testing.T) {
	a := NewAccumulator(10)
	n, err := a.Write([]byte("hello"))
	if err != nil || n != 5 {
		t.Fatalf("write 1: n=%d err=%v", n, err)
	}
	if a.Truncated() {
		t.Fatal("should not be truncated yet")
	}
	// This write overflows the 10-byte cap.
	if _, err := a.Write([]byte("world!!!")); err != nil {
		t.Fatalf("write 2: %v", err)
	}
	if !a.Truncated() {
		t.Fatal("expected truncated")
	}
	if got := a.String(); len(got) != 10 {
		t.Fatalf("buffer should be capped at 10, got %d (%q)", len(got), got)
	}
	if !strings.HasPrefix(a.String(), "hello") {
		t.Fatalf("prefix lost: %q", a.String())
	}
}

func TestAccumulatorConcurrent(t *testing.T) {
	a := NewAccumulator(1 << 16)
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				_, _ = a.Write([]byte("xyz"))
			}
		}()
	}
	wg.Wait()
	if got := len(a.String()); got > (1 << 16) {
		t.Fatalf("exceeded cap: %d", got)
	}
}
