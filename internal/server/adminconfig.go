package server

import "net/http"

// adminConfig implements GET /admin/config — the effective runtime settings the
// server is enforcing. It is assembled entirely from live server state, so it is
// secret-free by construction: no API keys, tenant keys, or TLS material ever
// pass through here. Sensitive-strategy values (cost rates) are reported as
// presence booleans. Admin-gated like the other management endpoints.
func (s *Server) adminConfig(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	if r.Method != http.MethodGet {
		writeErr(w, codeMethodNotAllowed, "method not allowed")
		return
	}

	// Timeouts.
	timeouts := map[string]any{"default_seconds": s.defaultTimeout.Seconds()}
	if len(s.timeouts) > 0 {
		per := make(map[string]float64, len(s.timeouts))
		for m, d := range s.timeouts {
			per[m] = d.Seconds()
		}
		timeouts["per_model_seconds"] = per
	}

	// Retry.
	retry := map[string]any{"enabled": s.retryMax > 0, "max": s.retryMax}
	if s.retryMax > 0 {
		retry["backoff_ms"] = s.retryBackoff.Milliseconds()
	}

	// Circuit breaker.
	bThreshold, bCooldown := s.breaker.Config()
	breaker := map[string]any{
		"enabled":          s.breaker.Enabled(),
		"threshold":        bThreshold,
		"cooldown_seconds": bCooldown.Seconds(),
	}

	// Response cache.
	cacheInfo := map[string]any{"enabled": s.cache != nil}
	if s.cache != nil {
		hits, misses := s.cache.Stats()
		cacheInfo["entries"] = s.cache.Len()
		cacheInfo["hits"] = hits
		cacheInfo["misses"] = misses
	}

	// Concurrency gate.
	concurrency := map[string]any{
		"enabled":      s.gate.enabled(),
		"max_inflight": s.gate.maxInflight,
		"max_queue":    s.gate.maxQueue,
	}

	// Context guardrail (per-model policy).
	guard := make(map[string]any, len(s.ctxPolicies))
	for m, p := range s.ctxPolicies {
		guard[m] = map[string]any{"limit": p.Limit, "enforce": p.Enforce}
	}

	// Models with max_tokens clamping enabled.
	clampModels := make([]string, 0, len(s.clampLimits))
	for m := range s.clampLimits {
		clampModels = append(clampModels, m)
	}

	// Cost-rate presence (per model) — booleans, never the dollar values.
	priced := make(map[string]bool, len(s.costRates))
	for m := range s.costRates {
		priced[m] = true
	}

	// Configured model ids.
	specs := s.sched.Models()
	models := make([]string, 0, len(specs))
	for _, sp := range specs {
		models = append(models, sp.ID)
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"open":            s.auth.Open(),
		"draining":        s.draining.Load(),
		"models":          models,
		"timeouts":        timeouts,
		"retry":           retry,
		"breaker":         breaker,
		"cache":           cacheInfo,
		"coalesce":        map[string]any{"enabled": s.coalesce != nil},
		"concurrency":     concurrency,
		"context_guard":   guard,
		"clamp_models":    clampModels,
		"model_fallbacks": s.modelFallbacks,
		"cost_rates_set":  priced,
	})
}
