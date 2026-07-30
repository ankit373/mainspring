package config

import (
	"os"
	"strings"
	"testing"
)

func TestLintCleanConfigHasNoWarnings(t *testing.T) {
	cfg := Config{
		Models: []Model{{ID: "m1", Ctx: 4096}},
	}
	if got := cfg.Lint(); len(got) != 0 {
		t.Fatalf("clean config should have no warnings, got %v", got)
	}
}

func TestLintInertGuardrailWithoutCtx(t *testing.T) {
	cfg := Config{
		EnforceContext: true,
		Models:         []Model{{ID: "m1"}}, // Ctx unset (0)
	}
	warnings := cfg.Lint()
	if len(warnings) != 1 || warnings[0].Model != "m1" {
		t.Fatalf("expected one warning for m1, got %+v", warnings)
	}
	if !strings.Contains(warnings[0].Message, "enforce_context") {
		t.Fatalf("warning should mention enforce_context: %+v", warnings[0])
	}
}

func TestLintPerModelOverrideInertWithoutCtx(t *testing.T) {
	on := true
	cfg := Config{
		// Global settings off; per-model override on, but ctx unset.
		Models: []Model{{ID: "m1", PreciseContext: &on, ClampMaxTokens: &on}},
	}
	warnings := cfg.Lint()
	if len(warnings) != 2 {
		t.Fatalf("expected 2 warnings (precise + clamp), got %+v", warnings)
	}
}

func TestLintGuardrailFineWithCtx(t *testing.T) {
	cfg := Config{
		EnforceContext: true,
		ClampMaxTokens: true,
		PreciseContext: true,
		Models:         []Model{{ID: "m1", Ctx: 4096}},
	}
	if got := cfg.Lint(); len(got) != 0 {
		t.Fatalf("ctx is set — guardrail/clamp/precise are NOT inert, want no warnings, got %v", got)
	}
}

// A per-model EnforceContext=false only downgrades enforce->warn (per its
// documented semantics); it does not disable the guardrail outright when the
// global flag is on. So the guardrail is still "active" (in warn mode) here,
// and still inert without ctx — this combination must still warn.
func TestLintPerModelFalseStillActiveUnderGlobalTrue(t *testing.T) {
	off := false
	cfg := Config{
		EnforceContext: true,                                      // global on
		Models:         []Model{{ID: "m1", EnforceContext: &off}}, // downgrades to warn-mode, ctx unset
	}
	if got := cfg.Lint(); len(got) != 1 {
		t.Fatalf("guardrail still active in warn mode without ctx — expected 1 warning, got %v", got)
	}
}

// When the global flag is off and a model has no per-model override at all,
// the guardrail never activates for it — nothing to warn about.
func TestLintGloballyOffNoOverrideNoWarning(t *testing.T) {
	cfg := Config{Models: []Model{{ID: "m1"}}} // ctx unset, everything off
	if got := cfg.Lint(); len(got) != 0 {
		t.Fatalf("guardrail never activated — should not warn, got %v", got)
	}
}

func TestLintUnresolvableModelFallback(t *testing.T) {
	cfg := Config{
		Models: []Model{
			{ID: "primary", Ctx: 4096, ModelFallbacks: []string{"backup", "ghost"}},
			{ID: "backup", Ctx: 4096},
		},
	}
	warnings := cfg.Lint()
	if len(warnings) != 1 || !strings.Contains(warnings[0].Message, `"ghost"`) {
		t.Fatalf("expected one warning about unresolvable %q, got %+v", "ghost", warnings)
	}
}

func TestLintModelFallbackResolvesViaAlias(t *testing.T) {
	cfg := Config{
		Aliases: map[string]string{"friendly": "backup"},
		Models: []Model{
			{ID: "primary", Ctx: 4096, ModelFallbacks: []string{"friendly"}},
			{ID: "backup", Ctx: 4096},
		},
	}
	if got := cfg.Lint(); len(got) != 0 {
		t.Fatalf("fallback targeting a known alias should not warn, got %v", got)
	}
}

func TestLintNegativeCostRate(t *testing.T) {
	cfg := Config{
		Models: []Model{{ID: "m1", Ctx: 4096, InputUSDPerMTok: -1}},
	}
	warnings := cfg.Lint()
	if len(warnings) != 1 || !strings.Contains(warnings[0].Message, "negative") {
		t.Fatalf("expected a negative-cost-rate warning, got %+v", warnings)
	}
}

func TestLintMissingTLSFiles(t *testing.T) {
	cfg := Config{TLSCert: "/nonexistent/cert.pem", TLSKey: "/nonexistent/key.pem"}
	warnings := cfg.Lint()
	if len(warnings) != 2 {
		t.Fatalf("expected 2 warnings (cert + key missing), got %+v", warnings)
	}
	for _, w := range warnings {
		if w.Model != "" {
			t.Fatalf("TLS warnings are server-wide, not per-model: %+v", w)
		}
	}
}

func TestLintExistingTLSFilesNoWarning(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "cert*.pem")
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
	cfg := Config{TLSCert: f.Name(), TLSKey: f.Name()}
	if got := cfg.Lint(); len(got) != 0 {
		t.Fatalf("existing files should not warn, got %v", got)
	}
}

func TestLintWarningString(t *testing.T) {
	if got := (LintWarning{Message: "server-wide"}).String(); got != "server-wide" {
		t.Fatalf("server-wide String() = %q", got)
	}
	if got := (LintWarning{Model: "m1", Message: "oops"}).String(); got != `model "m1": oops` {
		t.Fatalf("per-model String() = %q", got)
	}
}
