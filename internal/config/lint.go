package config

import (
	"fmt"
	"os"
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

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}
