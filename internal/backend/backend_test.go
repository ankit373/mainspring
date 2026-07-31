package backend

import "testing"

// TestDegradedIgnoresIntentionalCPUOnly proves the fix: a model correctly
// configured for CPU-only execution (GPULayers < 0, which the runner honors by
// reporting GPUOffload=false with no warning) must not be flagged degraded.
func TestDegradedIgnoresIntentionalCPUOnly(t *testing.T) {
	c := Capabilities{
		Backend: "llamacpp", Model: "m1", Device: "cpu",
		GPUOffload: false, RequestedCtx: 4096, EffectiveCtx: 4096,
		// No Warnings — the runner correctly determined this was intentional.
	}
	if c.Degraded() {
		t.Fatal("an intentional CPU-only model with no warnings must not be degraded")
	}
}

// A silent CPU fallback (GPU was wanted but didn't happen) still carries a
// warning from the backend, so it must still be flagged degraded.
func TestDegradedStillCatchesSilentFallback(t *testing.T) {
	c := Capabilities{
		Backend: "llamacpp", Model: "m1", Device: "cpu",
		GPUOffload: false, RequestedCtx: 4096, EffectiveCtx: 4096,
		Warnings: []string{"GPU offload requested but engine loaded on CPU (silent fallback)"},
	}
	if !c.Degraded() {
		t.Fatal("a silent CPU fallback (has a warning) must still be degraded")
	}
}

func TestDegradedCatchesContextShrink(t *testing.T) {
	c := Capabilities{
		Backend: "llamacpp", Model: "m1", Device: "metal",
		GPUOffload: true, RequestedCtx: 8192, EffectiveCtx: 4096,
	}
	if !c.Degraded() {
		t.Fatal("a shrunk context window must be degraded even with GPU offload and no explicit warning")
	}
}

func TestDegradedHealthyGPUModel(t *testing.T) {
	c := Capabilities{
		Backend: "llamacpp", Model: "m1", Device: "metal",
		GPUOffload: true, RequestedCtx: 4096, EffectiveCtx: 4096,
	}
	if c.Degraded() {
		t.Fatal("a fully healthy GPU model must not be degraded")
	}
}

func TestDegradedAnyWarningDegrades(t *testing.T) {
	c := Capabilities{GPUOffload: true, Warnings: []string{"could not verify device/offload from engine logs"}}
	if !c.Degraded() {
		t.Fatal("any populated warning must mark the model degraded, regardless of GPUOffload")
	}
}
