package server

import (
	"context"
	"net/http"
	"time"

	"github.com/ankit373/mainspring/internal/auth"
	"github.com/ankit373/mainspring/internal/backend"
)

// This file implements GET /v1/quality — the routing signal a higher-level
// router (Hydra) can read to route to Mainspring *best*. It is an OPTIONAL,
// provider-neutral contract: Hydra never depends on it, and Mainspring works
// fine without a router in front. It reports, per configured model, whether it
// is resident, the real device/effective-ctx (fail-loud), whether it is running
// degraded, current in-flight/queue depth, and a recent TTFT p50.

// modelQuality is the per-model routing signal.
type modelQuality struct {
	ID            string   `json:"id"`
	Resident      bool     `json:"resident"`
	Backend       string   `json:"backend,omitempty"`
	Device        string   `json:"device,omitempty"`
	GPUOffload    bool     `json:"gpu_offload"`
	Degraded      bool     `json:"degraded"`
	RequestedCtx  int      `json:"requested_ctx,omitempty"`
	EffectiveCtx  int      `json:"effective_ctx,omitempty"`
	Warnings      []string `json:"warnings,omitempty"`
	Inflight      int      `json:"inflight"`
	QueueDepth    int      `json:"queue_depth"`
	TTFTp50Ms     float64  `json:"ttft_ms_p50"`
	TTFTp90Ms     float64  `json:"ttft_ms_p90"`
	TTFTp99Ms     float64  `json:"ttft_ms_p99"`
	DurationP50Ms float64  `json:"duration_ms_p50"`
	DurationP90Ms float64  `json:"duration_ms_p90"`
	DurationP99Ms float64  `json:"duration_ms_p99"`
	Breaker       string   `json:"breaker"`  // "closed" | "open" | "half_open"
	CostUSD       float64  `json:"cost_usd"` // accumulated spend for this model
}

// quality implements GET /v1/quality. Gated like /capabilities: an authenticated
// caller must be admin; open mode allows it.
func (s *Server) quality(w http.ResponseWriter, r *http.Request) {
	if t, ok := auth.FromContext(r.Context()); ok && t.Role != auth.RoleAdmin {
		writeError(w, http.StatusForbidden, "admin role required")
		return
	}

	// Capabilities of currently-resident models, keyed by model id.
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	capsByID := make(map[string]backend.Capabilities)
	for _, ri := range s.sched.Loaded() {
		if c, err := ri.Runner.Capabilities(ctx); err == nil {
			capsByID[ri.ID] = c
		}
	}

	var costByModel map[string]float64
	var costTotal float64
	if s.metrics != nil {
		costByModel, costTotal = s.metrics.Costs()
	}

	specs := s.sched.Models()
	models := make([]modelQuality, 0, len(specs))
	anyDegraded := false
	for _, sp := range specs {
		inflight, queued := s.gate.stat(sp.ID)
		mq := modelQuality{
			ID:           sp.ID,
			Backend:      sp.Backend,
			RequestedCtx: sp.CtxSize,
			Inflight:     inflight,
			QueueDepth:   queued,
			Breaker:      s.breaker.State(sp.ID).String(),
		}
		if s.metrics != nil {
			lat := s.metrics.LatencyPercentiles(sp.ID)
			mq.TTFTp50Ms, mq.TTFTp90Ms, mq.TTFTp99Ms = lat.TTFTP50, lat.TTFTP90, lat.TTFTP99
			mq.DurationP50Ms, mq.DurationP90Ms, mq.DurationP99Ms = lat.DurationP50, lat.DurationP90, lat.DurationP99
			mq.CostUSD = costByModel[sp.ID]
		}
		if c, ok := capsByID[sp.ID]; ok {
			mq.Resident = true
			mq.Backend = c.Backend
			mq.Device = c.Device
			mq.GPUOffload = c.GPUOffload
			mq.Degraded = c.Degraded()
			mq.RequestedCtx = c.RequestedCtx
			mq.EffectiveCtx = c.EffectiveCtx
			mq.Warnings = c.Warnings
			if mq.Degraded {
				anyDegraded = true
			}
		}
		models = append(models, mq)
	}

	used, budget, count := s.sched.Residency()
	server := map[string]any{
		"open":           s.auth.Open(),
		"degraded":       anyDegraded,
		"resident_bytes": used,
		"budget_bytes":   budget,
		"loaded_count":   count,
		"cost_usd_total": costTotal,
	}
	if budget > 0 {
		server["headroom_bytes"] = budget - used
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"object": "quality",
		"server": server,
		"models": models,
	})
}
