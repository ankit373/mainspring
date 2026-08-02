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

// TestLintPreciseWithoutEnforceIsInert — precise_context only refines the
// guardrail's prompt count, so with ctx set but no guardrail to refine, exact
// tokenization is computed for nothing.
func TestLintPreciseWithoutEnforceIsInert(t *testing.T) {
	cfg := Config{
		PreciseContext: true,
		Models:         []Model{{ID: "m1", Ctx: 4096}},
	}
	got := cfg.Lint()
	if len(got) != 1 || !strings.Contains(got[0].Message, "without enforce_context") {
		t.Fatalf("want one precise-without-enforce warning, got %v", got)
	}
	// With the guardrail on, it is doing its job — no warning.
	cfg.EnforceContext = true
	if got := cfg.Lint(); len(got) != 0 {
		t.Fatalf("precise_context refines an active guardrail — want no warnings, got %v", got)
	}
}

// TestLintAPIKeysIgnoredWhenTenantsSet — authTenants returns early on tenants, so
// every api_keys entry silently authenticates nothing.
func TestLintAPIKeysIgnoredWhenTenantsSet(t *testing.T) {
	cfg := Config{
		APIKeys: []string{"k1", "k2"},
		Tenants: []Tenant{{Name: "t1", Key: "tk", Role: "admin"}},
	}
	got := cfg.Lint()
	if len(got) != 1 || !strings.Contains(got[0].Message, "api_keys is ignored") {
		t.Fatalf("want the ignored-api_keys warning, got %v", got)
	}
}

// TestLintUnknownTenantRole — an unrecognised role silently becomes "inference",
// so a typo'd "admin" quietly strips a tenant's privileges.
func TestLintUnknownTenantRole(t *testing.T) {
	cfg := Config{Tenants: []Tenant{{Name: "ops", Key: "k", Role: "Admin"}}}
	got := cfg.Lint()
	if len(got) != 1 || !strings.Contains(got[0].Message, "unknown role") {
		t.Fatalf("want an unknown-role warning, got %v", got)
	}
	if !strings.Contains(got[0].Message, "ops") || !strings.Contains(got[0].Message, "Admin") {
		t.Errorf("warning should name the tenant and the bad value: %v", got[0].Message)
	}
	for _, ok := range []string{"", "admin", "inference"} {
		cfg := Config{Tenants: []Tenant{{Name: "t", Key: "k", Role: ok}}}
		if got := cfg.Lint(); len(got) != 0 {
			t.Errorf("role %q is valid, got %v", ok, got)
		}
	}
}

func TestLintTenantWithEmptyKey(t *testing.T) {
	cfg := Config{Tenants: []Tenant{{Name: "ghost", Role: "admin"}}}
	got := cfg.Lint()
	if len(got) != 1 || !strings.Contains(got[0].Message, "empty key") {
		t.Fatalf("want an empty-key warning, got %v", got)
	}
}

// path on an adopt-only backend is read by nothing: the daemon holds its own
// weights and the model id must be the name it knows. Setting path is usually a
// sign the id is wrong, which otherwise surfaces much later as a request-time
// failure naming a model that does not exist.
func TestLintFlagsPathOnAnAdoptBackend(t *testing.T) {
	tests := []struct {
		name  string
		cfg   Config
		warns bool
	}{
		{
			name: "adopt backend with a path set",
			cfg: Config{Backend: "ollama", Models: []Model{
				{ID: "qwen", Path: "Qwen2.5-Coder:7b"}}},
			warns: true,
		},
		{
			name: "adopt backend chosen per-model overrides a non-adopt default",
			cfg: Config{Backend: "llamacpp", Models: []Model{
				{ID: "qwen", Backend: "lmstudio", Path: "something"}}},
			warns: true,
		},
		{
			name: "adopt backend with no path is the correct shape",
			cfg: Config{Backend: "ollama", Models: []Model{
				{ID: "Qwen2.5-Coder:7b"}}},
			warns: false,
		},
		{
			name: "a real backend needs its path and must not be warned about",
			cfg: Config{Backend: "llamacpp", Models: []Model{
				{ID: "m", Path: "/models/m.gguf"}}},
			warns: false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var got string
			for _, w := range tc.cfg.Lint() {
				if strings.Contains(w.Message, "path is ignored") {
					got = w.Message
				}
			}
			if tc.warns && got == "" {
				t.Fatalf("expected a path-ignored warning, got %+v", tc.cfg.Lint())
			}
			if !tc.warns && got != "" {
				t.Fatalf("unexpected warning: %s", got)
			}
			// The warning has to be actionable: name the id to use.
			if tc.warns && !strings.Contains(got, "id: ") {
				t.Errorf("warning does not say what to write instead: %s", got)
			}
		})
	}
}

// mlx_lm.server has no context-window flag (verified against 0.31.3, whose only
// related option is --max-tokens, the generation cap). ctx used to be passed as
// --max-tokens, silently reconfiguring generation instead. It is no longer sent
// at all, so it must be reported rather than quietly doing nothing at the engine
// — the failure mode #197 was about.
func TestLintMLXCtxIsNotSilentlyDropped(t *testing.T) {
	cfg := Config{
		Backend: "mlx",
		Models:  []Model{{ID: "m1", Ctx: 8192}},
	}
	warnings := cfg.Lint()
	if len(warnings) != 1 || warnings[0].Model != "m1" {
		t.Fatalf("expected one warning for m1, got %+v", warnings)
	}
	if !strings.Contains(warnings[0].Message, "ctx cannot be applied to the mlx backend") {
		t.Errorf("warning should name the cause, got: %s", warnings[0].Message)
	}
	// It is a caveat, not "ignored": Mainspring's own guardrail still uses ctx.
	if !strings.Contains(warnings[0].Message, "guardrail") {
		t.Errorf("warning should say Mainspring still enforces it, got: %s", warnings[0].Message)
	}

	// Per-model backend selection is honoured too.
	perModel := Config{
		Backend: "llamacpp",
		Models:  []Model{{ID: "a", Ctx: 4096}, {ID: "b", Backend: "mlx", Ctx: 4096}},
	}
	w := perModel.Lint()
	if len(w) != 1 || w[0].Model != "b" {
		t.Fatalf("only the mlx model should warn, got %+v", w)
	}

	// No ctx set, nothing to warn about.
	if w := (Config{Backend: "mlx", Models: []Model{{ID: "m1"}}}).Lint(); len(w) != 0 {
		t.Errorf("mlx without ctx should not warn, got %+v", w)
	}
}
