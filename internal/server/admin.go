package server

import (
	"context"
	"net/http"
	"time"

	"github.com/ankit373/mainspring/internal/auth"
)

// This file implements the admin API: operational actions (drain, config
// reload, per-model load/unload) that previously needed a restart or a signal.
// Every endpoint is admin-gated and, because the request-ID middleware logs all
// requests, each action is audited in the access log (method, path, status,
// request_id).

// SetReloadFunc wires the config-reload action (used by POST /admin/reload).
// When nil, the endpoint reports 501 Not Implemented.
func (s *Server) SetReloadFunc(fn func() error) { s.reloadFn = fn }

// requireAdmin returns true if the caller may use management endpoints: an
// authenticated caller must be admin; open mode (no auth) allows.
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
	writeJSON(w, http.StatusOK, map[string]any{"id": id, "unloaded": s.sched.Unload(id)})
}
