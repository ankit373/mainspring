package server

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/ankit373/mainspring/internal/cache"
)

// cacheBodyCap bounds how large a single response may be to remain cacheable.
// Larger responses stream through untouched but are never stored.
const cacheBodyCap = 1 << 20 // 1 MiB

// SetCache enables the opt-in response cache: identical non-streaming,
// deterministic requests are served from an in-memory LRU for ttl, holding at
// most maxEntries entries. maxEntries<=0 leaves caching disabled.
func (s *Server) SetCache(ttl time.Duration, maxEntries int) {
	if maxEntries <= 0 {
		s.cache = nil
		return
	}
	s.cache = cache.New(ttl, maxEntries)
}

// cacheable reports whether a request body may be cached. Only non-streaming
// requests with a deterministic sampling profile (temperature 0 / unset and no
// nonzero top_p perturbation intent) qualify — a positive temperature makes the
// output non-reproducible, so caching it would pin one random sample.
func cacheable(body []byte) bool {
	var req struct {
		Stream      bool     `json:"stream"`
		Temperature *float64 `json:"temperature"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		return false
	}
	if req.Stream {
		return false
	}
	if req.Temperature != nil && *req.Temperature > 0 {
		return false
	}
	return true
}

// cacheLookup returns a stored response for this request, or ("", false) when
// caching is off or the request is not cacheable. The returned key is used to
// store the response after a miss.
func (s *Server) cacheLookup(model, path string, body []byte) (key string, v cache.Value, hit bool) {
	if s.cache == nil || !cacheable(body) {
		return "", cache.Value{}, false
	}
	key = cache.Key(model, path, body)
	v, hit = s.cache.Get(key)
	return key, v, hit
}

// serveCached replays a cached response verbatim, tagging it as a cache hit.
func serveCached(w http.ResponseWriter, v cache.Value) {
	h := w.Header()
	for k, vs := range v.Header {
		for _, vv := range vs {
			h.Add(k, vv)
		}
	}
	h.Set("X-Mainspring-Cache", "hit")
	w.WriteHeader(v.Status)
	_, _ = w.Write(v.Body)
}
