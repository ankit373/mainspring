package server

import (
	"context"
	"net/http"
	"time"

	"github.com/ankit373/mainspring/internal/auth"
)

// This file implements the admin API: operational actions (drain, config
// reload, per-model load/unload) that previously needed a restart or a signal.
// Every endpoint is admin-gated and, when the access log is enabled, each
// action is audited there with its principal (tenant, method, path, status,
// request_id) — in open mode there is no principal to record, which is one more
// reason not to run open on a network you do not control.

// SetReloadFunc wires the config-reload action (used by POST /admin/reload).
// When nil, the endpoint reports 501 Not Implemented.
func (s *Server) SetReloadFunc(fn func() error) { s.reloadFn = fn }

// requireAdmin is the single admin gate for every management endpoint — the
// /admin/* actions plus /capabilities and /v1/quality. It returns true if the
// caller may proceed: an authenticated caller must be admin; open mode (no
// auth, so no tenant to check) allows. One authorization decision, one place:
// a second copy is how an authorization bug ships.
func (s *Server) requireAdmin(w http.ResponseWriter, r *http.Request) bool {
	if t, ok := auth.FromContext(r.Context()); ok && t.Role != auth.RoleAdmin {
		writeErr(w, codeForbidden, "admin role required")
		return false
	}
	return true
}

// adminDrain sets (or clears) draining. POST /admin/drain[?on=false].
func (s *Server) adminDrain(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	on := r.URL.Query().Get("on") != "false" // default: begin draining
	s.SetDraining(on)
	writeJSON(w, http.StatusOK, map[string]any{"draining": on})
}

// adminReload triggers a config reload. POST /admin/reload.
func (s *Server) adminReload(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	if s.reloadFn == nil {
		writeErr(w, codeInternal, "reload not supported")
		return
	}
	if err := s.reloadFn(); err != nil {
		writeErr(w, codeInvalidRequest, "reload failed: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"reloaded": true})
}

// adminLoad preloads a model. POST /admin/models/{id}/load.
func (s *Server) adminLoad(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	id := r.PathValue("id")
	if !s.sched.Known(id) {
		writeErr(w, codeModelNotFound, "model not found: "+id)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	if _, err := s.sched.EnsureLoaded(ctx, id); err != nil {
		writeErr(w, codeBackendUnavailable, "load "+id+": "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"loaded": id})
}

// adminUnload evicts a model. POST /admin/models/{id}/unload.
func (s *Server) adminUnload(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	id := r.PathValue("id")
	if !s.sched.Known(id) {
		writeErr(w, codeModelNotFound, "model not found: "+id)
		return
	}
	unloaded, interrupted := s.sched.Unload(id)
	writeJSON(w, http.StatusOK, map[string]any{"id": id, "unloaded": unloaded, "interrupted_requests": interrupted})
}

// adminBreakerReset forces a model's circuit breaker back to Closed, letting an
// operator recover immediately (e.g. after manually confirming the backend is
// healthy) rather than waiting out the cooldown. POST /admin/breaker/{id}/reset.
func (s *Server) adminBreakerReset(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	id, ok := s.sched.Resolve(r.PathValue("id"))
	if !ok {
		writeErr(w, codeModelNotFound, "model not found: "+r.PathValue("id"))
		return
	}
	s.breaker.Reset(id)
	writeJSON(w, http.StatusOK, map[string]any{"id": id, "breaker": s.breaker.State(id).String()})
}

// adminCacheClear purges every entry from the response cache. A no-op (still
// 200) when caching is disabled. POST /admin/cache/clear.
func (s *Server) adminCacheClear(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	s.cache.Clear()
	writeJSON(w, http.StatusOK, map[string]any{"cleared": true})
}
