package cache

import (
	"testing"
	"time"
)

func val(body string) Value { return Value{Status: 200, Body: []byte(body)} }

func TestGetPutHitMiss(t *testing.T) {
	c := New(time.Minute, 4)
	if _, ok := c.Get("k"); ok {
		t.Fatal("empty cache should miss")
	}
	c.Put("k", val("hello"))
	got, ok := c.Get("k")
	if !ok || string(got.Body) != "hello" {
		t.Fatalf("hit = %q,%v; want hello,true", got.Body, ok)
	}
	h, m := c.Stats()
	if h != 1 || m != 1 {
		t.Fatalf("stats = %d,%d; want 1,1", h, m)
	}
}

func TestTTLExpiry(t *testing.T) {
	now := time.Unix(1000, 0)
	c := New(30*time.Second, 4)
	c.SetClock(func() time.Time { return now })
	c.Put("k", val("v"))
	now = now.Add(29 * time.Second)
	if _, ok := c.Get("k"); !ok {
		t.Fatal("entry should still be live at 29s")
	}
	now = now.Add(2 * time.Second) // 31s total
	if _, ok := c.Get("k"); ok {
		t.Fatal("entry should have expired at 31s")
	}
	if c.Len() != 0 {
		t.Fatalf("expired entry should be reaped; len=%d", c.Len())
	}
}

func TestLRUEviction(t *testing.T) {
	c := New(time.Minute, 2)
	c.Put("a", val("a"))
	c.Put("b", val("b"))
	_, _ = c.Get("a")    // touch a → b is now LRU
	c.Put("c", val("c")) // evicts b
	if _, ok := c.Get("b"); ok {
		t.Fatal("b should have been evicted as LRU")
	}
	if _, ok := c.Get("a"); !ok {
		t.Fatal("a was recently used; should survive")
	}
	if _, ok := c.Get("c"); !ok {
		t.Fatal("c was just inserted; should be present")
	}
}

func TestDisabledCache(t *testing.T) {
	c := New(time.Minute, 0)
	c.Put("k", val("v"))
	if _, ok := c.Get("k"); ok {
		t.Fatal("max<=0 cache must store nothing")
	}
	var nilC *LRU
	nilC.Put("k", val("v")) // must not panic
	if _, ok := nilC.Get("k"); ok {
		t.Fatal("nil cache must miss")
	}
}

func TestKeyStability(t *testing.T) {
	// Same semantic body, different key order + whitespace → same key.
	a := Key("m", "/v1/chat/completions", []byte(`{"model":"m","temperature":0,"messages":[]}`))
	b := Key("m", "/v1/chat/completions", []byte(`{ "temperature":0, "messages":[], "model":"m" }`))
	if a != b {
		t.Fatalf("canonicalization failed: %s != %s", a, b)
	}
	// Different model → different key.
	if Key("m2", "/v1/chat/completions", []byte(`{}`)) == Key("m", "/v1/chat/completions", []byte(`{}`)) {
		t.Fatal("model must be part of the key")
	}
	// Non-JSON body falls back to raw hashing (still deterministic).
	if Key("m", "/p", []byte("not json")) != Key("m", "/p", []byte("not json")) {
		t.Fatal("raw fallback should be deterministic")
	}
}
