package config

import (
	"fmt"
	"os"

	"github.com/ankit373/mainspring/internal/backend"
)

// LintWarning is one config combination that is valid YAML but has no effect
// (or a broken effect) — a mismatch between what an operator asked for and
// what will actually happen. Model is empty for a server-wide warning.
type LintWarning struct {
	Model   string
	Message string
}

// String renders a warning for a human (CLI output or a startup log line).
func (w LintWarning) String() string {
	if w.Model == "" {
		return w.Message
	}
	return fmt.Sprintf("model %q: %s", w.Model, w.Message)
}

// Lint reports config combinations that are technically valid but silently do
// nothing or misbehave — the project's "fail loud" principle applied to
// configuration itself. It never blocks startup; each finding is worth a
// human's attention, not a fatal error.
func (c Config) Lint() []LintWarning {
	var warnings []LintWarning

	// Identifiers a model_fallbacks entry may legitimately target: any
	// configured model id or any alias name (both resolve at request time).
	known := make(map[string]bool, len(c.Models)+len(c.Aliases))
	for _, m := range c.Models {
		known[m.ID] = true
	}
	for alias := range c.Aliases {
		known[alias] = true
	}

	for _, m := range c.Models {
		// An adopt-only backend runs its own process and holds its own weights, so
		// there is no file for Mainspring to open: the model id must be the name the
		// daemon knows it by, and path is read by nothing. Setting it is usually a
		// sign the id is wrong — the real name was put in path — which otherwise
		// surfaces much later as a request-time failure naming a model that does not
		// exist.
		if b := c.modelBackend(m); backend.IsAdopt(b) && m.Path != "" {
			warnings = append(warnings, LintWarning{m.ID, fmt.Sprintf(
				"path is ignored for the adopt-only backend %q — the model id must be the name %s "+
					"knows it by, so you likely want `id: %s`", b, b, m.Path)})
		}
		if m.Ctx <= 0 {
			if c.EnforceContext || boolOverride(m.EnforceContext) {
				warnings = append(warnings, LintWarning{m.ID,
					"enforce_context is on but ctx is not set — the context guardrail has no effect for this model"})
			}
			if c.PreciseContext || boolOverride(m.PreciseContext) {
				warnings = append(warnings, LintWarning{m.ID,
					"precise_context is on but ctx is not set — has no effect for this model"})
			}
			if c.ClampMaxTokens || boolOverride(m.ClampMaxTokens) {
				warnings = append(warnings, LintWarning{m.ID,
					"clamp_max_tokens is on but ctx is not set — has no effect for this model"})
			}
		} else if effective(c.PreciseContext, m.PreciseContext) && !effective(c.EnforceContext, m.EnforceContext) {
			// precise_context only refines the guardrail's prompt count. With no
			// guardrail to refine, exact tokenization is computed for nothing.
			warnings = append(warnings, LintWarning{m.ID,
				"precise_context has no effect without enforce_context — the guardrail never runs for this model"})
		}
		for _, fb := range m.ModelFallbacks {
			if !known[fb] {
				warnings = append(warnings, LintWarning{m.ID,
					fmt.Sprintf("model_fallbacks references %q, which is not a configured model or alias", fb)})
			}
		}
		if m.InputUSDPerMTok < 0 || m.OutputUSDPerMTok < 0 {
			warnings = append(warnings, LintWarning{m.ID, "a cost rate (input/output USD per Mtok) is negative"})
		}
	}

	// Auth block. authTenants prefers tenants and ignores api_keys entirely when
	// any tenant exists, and an unrecognised role silently becomes "inference" —
	// so a typo'd "admin" quietly strips a tenant's privileges.
	if len(c.Tenants) > 0 && len(c.APIKeys) > 0 {
		warnings = append(warnings, LintWarning{Message: fmt.Sprintf(
			"api_keys is ignored because tenants is set (%d key(s) will not authenticate anything)", len(c.APIKeys))})
	}
	for _, t := range c.Tenants {
		switch t.Role {
		case "", "admin", "inference":
		default:
			warnings = append(warnings, LintWarning{Message: fmt.Sprintf(
				"tenant %q has unknown role %q — it will be treated as \"inference\"; valid roles are \"admin\" and \"inference\"", t.Name, t.Role)})
		}
		if t.Key == "" {
			warnings = append(warnings, LintWarning{Message: fmt.Sprintf("tenant %q has an empty key and can never authenticate", t.Name)})
		}
	}

	if c.TLSCert != "" && !fileExists(c.TLSCert) {
		warnings = append(warnings, LintWarning{Message: fmt.Sprintf("tls_cert %q does not exist", c.TLSCert)})
	}
	if c.TLSKey != "" && !fileExists(c.TLSKey) {
		warnings = append(warnings, LintWarning{Message: fmt.Sprintf("tls_key %q does not exist", c.TLSKey)})
	}

	return warnings
}

// boolOverride reports whether a *bool per-model override is explicitly true;
// nil (inherit global) or false are both "not on" for this check.
func boolOverride(b *bool) bool { return b != nil && *b }

// effective resolves a per-model override against the server-wide default the
// same way the server wires it: set wins, nil inherits.
func effective(global bool, override *bool) bool {
	if override != nil {
		return *override
	}
	return global
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// modelBackend is the backend a model will actually run on: its own if set, else
// the server-wide default.
func (c Config) modelBackend(m Model) string {
	if m.Backend != "" {
		return m.Backend
	}
	return c.Backend
}
