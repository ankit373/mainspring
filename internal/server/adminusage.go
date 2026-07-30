package server

import "net/http"

// adminUsage implements GET /admin/usage — per-tenant consumption actuals
// (requests, prompt/output tokens, USD cost). It complements the per-tenant
// token *budget* enforced in the auth layer with what each principal actually
// consumed. Unauthenticated (open-mode) traffic is bucketed under "(anonymous)".
// Admin-gated like the other management endpoints.
func (s *Server) adminUsage(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	var tenants any = []any{}
	if s.metrics != nil {
		tenants = s.metrics.TenantUsage()
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"object":  "usage",
		"tenants": tenants,
	})
}
