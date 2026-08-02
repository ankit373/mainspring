package server

import (
	"net/http"
	"time"

	"github.com/ankit373/mainspring/internal/auth"
	"github.com/ankit373/mainspring/internal/cache"
	"github.com/ankit373/mainspring/internal/metrics"
)

// This file holds the response-sharing lifecycle: answering a caller from the
// response cache, or from a concurrent leader's in-flight computation, and
// storing a finished response for whoever comes next.
//
// Both dialects use it. Each caches its own wire shape — an Anthropic caller
// gets an Anthropic response back — which is safe because cache.Key includes the
// request path, so /v1/messages and /v1/chat/completions can never collide even
// when a request translates to a byte-identical upstream body.

// sharedOrigin says where an already-computed response came from. It changes
// only the replay header and which ledger flag is set; the metering is identical
// either way, because from the tenant's point of view the tokens were spent.
type sharedOrigin int

const (
	fromCache sharedOrigin = iota
	fromLeader
)

// serveShared replays a response this caller did not compute — a cache hit or a
// coalescing leader's result — and meters and records it exactly as a generated
// response would be, so accounting does not depend on whether the model actually
// ran. Timed from recvd (handler entry): there is no generation phase to time,
// and reporting 0ms is not the same as reporting how long the caller waited.
func (s *Server) serveShared(w http.ResponseWriter, r *http.Request, model string, tenant *auth.Tenant, v cache.Value, origin sharedOrigin, recvd time.Time) {
	if origin == fromCache {
		serveCached(w, v)
	} else {
		serveCoalesced(w, v)
	}
	charge := v.Completion
	if v.Exact {
		charge = v.Prompt + v.Completion
	}
	s.auth.AddTokens(tenant, charge)
	if s.metrics == nil {
		return
	}
	s.metrics.Record(metrics.Event{
		Time:         recvd,
		RequestID:    RequestID(r.Context()),
		TraceID:      TraceID(r.Context()),
		Model:        model,
		Tenant:       auth.TenantOf(r.Context()),
		Status:       v.Status,
		Cached:       origin == fromCache,
		Coalesced:    origin == fromLeader,
		DurationMs:   ms(time.Since(recvd)),
		Bytes:        int64(len(v.Body)),
		PromptTokens: v.Prompt,
		TokensEst:    v.Completion,
		Exact:        v.Exact,
		CostUSD:      s.costFor(model, v.Prompt, v.Completion, v.Exact),
	})
}

// share tracks one request's participation in caching and coalescing between
// the admission checks and the response being stored.
type share struct {
	s        *Server
	cacheKey string // "" when this response will not be cached
	coKey    string // "" when this request is not a coalescing leader
	val      cache.Value
	ok       bool
}

// wanted reports whether the response body must be captured — it is only worth
// buffering when something will actually be stored or published.
func (sh *share) wanted() bool { return sh.cacheKey != "" || sh.coKey != "" }

// store records a finished, shareable response. Callers must gate this on the
// response actually being complete and successful; an incomplete body must never
// be stored, or one severed answer is replayed to every later caller.
func (sh *share) store(v cache.Value) {
	if sh.cacheKey != "" {
		sh.s.cache.Put(sh.cacheKey, v)
	}
	if sh.coKey != "" {
		sh.val, sh.ok = v, true
	}
}

// beginShare answers the caller from the cache or from a concurrent leader when
// it can, and otherwise enrolls this request as the leader others will wait on.
//
// served=true means the response is already written and the handler must return
// immediately. Otherwise done MUST be deferred: it releases any followers with
// whatever store() recorded, or with a failure if the handler returned without
// storing anything. Without that a follower waits on a leader that already gave
// up — which is why the publish is a defer here rather than a call at the end.
func (s *Server) beginShare(w http.ResponseWriter, r *http.Request, model string, tenant *auth.Tenant, body []byte, recvd time.Time) (sh *share, done func(), served bool) {
	sh = &share{s: s}
	noop := func() {}

	// Response cache: a hit skips the gate, breaker, loader and backend entirely.
	cacheKey, cached, hit := s.cacheLookup(model, r.URL.Path, body)
	if hit {
		s.serveShared(w, r, model, tenant, cached, fromCache, recvd)
		return sh, noop, true
	}
	sh.cacheKey = cacheKey

	if s.coalesce == nil || !cacheable(body) {
		return sh, noop, false
	}
	key := cacheKey
	if key == "" {
		key = cache.Key(model, r.URL.Path, body)
	}
	leader, f := s.coalesce.join(key)
	if leader {
		sh.coKey = key
		return sh, func() { s.coalesce.publish(key, sh.val, sh.ok) }, false
	}

	// Follower: wait for the leader. If the leader failed to produce a shareable
	// result, fall through and compute independently rather than inheriting its
	// failure — the caller asked for an answer, not for the leader's luck.
	select {
	case <-f.done:
		if f.ok {
			s.serveShared(w, r, model, tenant, f.val, fromLeader, recvd)
			return sh, noop, true
		}
		return sh, noop, false
	case <-r.Context().Done():
		if !clientGone(r.Context()) {
			writeErr(w, codeTimeout, "request cancelled while waiting for coalesced result")
		}
		return sh, noop, true
	}
}
