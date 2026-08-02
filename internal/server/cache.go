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

// replayHeaderAllow lists the only response headers a cached or coalesced replay
// may carry: each one describes the stored payload itself, so it stays true no
// matter who receives it later.
//
// Everything else on a response is request- or tenant-scoped and must come from
// the caller's own request instead — above all X-Request-ID, which otherwise
// reports one id for two different requests, and the RateLimit/TokenBudget
// headroom, which would disclose one tenant's remaining quota to another.
//
// This is a whitelist deliberately. A blacklist leaks the next request-scoped
// header someone adds, silently, and that is exactly how this bug arrived: the
// snapshot was taken with Header().Clone() after the middleware had already
// written the leader's id and headroom into the map.
var replayHeaderAllow = []string{
	"Content-Type",
	"X-Mainspring-Backend",
	"X-Mainspring-Device",
	"X-Mainspring-Warning",
}

// replayHeader copies just the payload-describing headers out of a response, for
// storing alongside a cached or coalesced body.
func replayHeader(h http.Header) http.Header {
	out := make(http.Header, len(replayHeaderAllow))
	for _, k := range replayHeaderAllow {
		if vs := h.Values(k); len(vs) > 0 {
			out[k] = append([]string(nil), vs...)
		}
	}
	return out
}

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
// requests that explicitly ask for determinism — `"temperature": 0` — qualify.
// An *omitted* temperature is not deterministic: the OpenAI default is 1, i.e.
// "sample normally", so storing that response would pin one random sample and
// replay it to every later caller — the cache changing observable behaviour
// instead of being transparent. top_p needs no separate check: at temperature 0
// decoding is greedy, so a nucleus cutoff cannot change the token chosen.
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
	return req.Temperature != nil && *req.Temperature == 0
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
