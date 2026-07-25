// Package cache is a small, self-contained TTL+LRU response cache. It stores
// completed non-streaming HTTP responses keyed by a stable hash of the request,
// so identical deterministic requests return instantly without re-running the
// model. It is opt-in and bounded in both entry count and age; nothing here
// touches the wire format — the server decides what is cacheable and replays a
// hit verbatim.
package cache

import (
	"container/list"
	"io"
	"sync"
	"sync/atomic"
	"time"
)

// Value is a cached response payload. Header is a snapshot of the response
// headers at write time (hop-by-hop already stripped by the proxy). Prompt and
// Completion carry the token usage observed when the entry was stored, so a hit
// can be metered without re-parsing the body.
type Value struct {
	Status     int
	Header     map[string][]string
	Body       []byte
	Prompt     int64
	Completion int64
	Exact      bool
}

type entry struct {
	key     string
	val     Value
	expires time.Time // zero => never expires
}

// LRU is a concurrency-safe TTL + max-entries response cache. The zero value is
// not usable; construct with New.
type LRU struct {
	mu    sync.Mutex
	ttl   time.Duration
	max   int
	ll    *list.List // front = most recently used
	items map[string]*list.Element
	now   func() time.Time

	hits   atomic.Uint64
	misses atomic.Uint64
}

// New builds an LRU that holds at most max entries, each living ttl before it
// expires (ttl<=0 => entries never expire on age, only on eviction). max<=0
// yields a cache that stores nothing.
func New(ttl time.Duration, max int) *LRU {
	return &LRU{
		ttl:   ttl,
		max:   max,
		ll:    list.New(),
		items: make(map[string]*list.Element),
		now:   time.Now,
	}
}

// SetClock overrides the time source (tests). Not safe to call concurrently
// with Get/Put.
func (c *LRU) SetClock(fn func() time.Time) { c.now = fn }

// Get returns the cached value for key and records a hit/miss. An expired entry
// is treated as a miss and evicted.
func (c *LRU) Get(key string) (Value, bool) {
	if c == nil || c.max <= 0 {
		return Value{}, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.items[key]
	if !ok {
		c.misses.Add(1)
		return Value{}, false
	}
	e := el.Value.(*entry)
	if !e.expires.IsZero() && !c.now().Before(e.expires) {
		c.removeElement(el)
		c.misses.Add(1)
		return Value{}, false
	}
	c.ll.MoveToFront(el)
	c.hits.Add(1)
	return e.val, true
}

// Put stores val under key, evicting the least-recently-used entry if at
// capacity. Storing an existing key refreshes its value and TTL.
func (c *LRU) Put(key string, val Value) {
	if c == nil || c.max <= 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	var exp time.Time
	if c.ttl > 0 {
		exp = c.now().Add(c.ttl)
	}
	if el, ok := c.items[key]; ok {
		e := el.Value.(*entry)
		e.val, e.expires = val, exp
		c.ll.MoveToFront(el)
		return
	}
	el := c.ll.PushFront(&entry{key: key, val: val, expires: exp})
	c.items[key] = el
	for c.ll.Len() > c.max {
		c.removeElement(c.ll.Back())
	}
}

func (c *LRU) removeElement(el *list.Element) {
	if el == nil {
		return
	}
	c.ll.Remove(el)
	delete(c.items, el.Value.(*entry).key)
}

// Len returns the current number of live elements (including not-yet-reaped
// expired ones).
func (c *LRU) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.ll.Len()
}

// Stats returns cumulative hits and misses.
func (c *LRU) Stats() (hits, misses uint64) {
	return c.hits.Load(), c.misses.Load()
}

// WritePrometheus emits cache counters in the Prometheus text format.
func (c *LRU) WritePrometheus(w io.Writer) {
	if c == nil || c.max <= 0 {
		return
	}
	h, m := c.Stats()
	io.WriteString(w, "# HELP mainspring_cache_hits_total Response cache hits.\n")
	io.WriteString(w, "# TYPE mainspring_cache_hits_total counter\n")
	writeCounter(w, "mainspring_cache_hits_total", h)
	io.WriteString(w, "# HELP mainspring_cache_misses_total Response cache misses.\n")
	io.WriteString(w, "# TYPE mainspring_cache_misses_total counter\n")
	writeCounter(w, "mainspring_cache_misses_total", m)
	io.WriteString(w, "# HELP mainspring_cache_entries Live response cache entries.\n")
	io.WriteString(w, "# TYPE mainspring_cache_entries gauge\n")
	writeCounter(w, "mainspring_cache_entries", uint64(c.Len()))
}

func writeCounter(w io.Writer, name string, v uint64) {
	io.WriteString(w, name)
	io.WriteString(w, " ")
	io.WriteString(w, strconvU(v))
	io.WriteString(w, "\n")
}

func strconvU(v uint64) string {
	if v == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for v > 0 {
		i--
		b[i] = byte('0' + v%10)
		v /= 10
	}
	return string(b[i:])
}
