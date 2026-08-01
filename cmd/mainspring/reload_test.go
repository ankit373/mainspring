package main

import (
	"io"
	"os"
	"strings"
	"testing"

	"github.com/ankit373/mainspring/internal/config"
)

// captureStderr redirects os.Stderr for the duration of fn and returns
// whatever was written to it.
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	orig := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stderr = w
	fn()
	_ = w.Close()
	os.Stderr = orig
	out, _ := io.ReadAll(r)
	return string(out)
}

func TestWarnUnappliedChangesReportsEveryChangedField(t *testing.T) {
	startup := config.Config{Addr: ":11500", Backend: "llamacpp", MaxLoaded: 1, Coalesce: false, RetryMax: 0}
	changed := config.Config{Addr: ":9999", Backend: "ollama", MaxLoaded: 2, Coalesce: true, RetryMax: 3}

	out := captureStderr(t, func() { warnUnappliedChanges(startup, changed) })

	for _, want := range []string{
		"addr changed (:11500 -> :9999)",
		"backend changed (llamacpp -> ollama)",
		"max_loaded changed (1 -> 2)",
		"coalesce changed (false -> true)",
		"retry_max changed (0 -> 3)",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing warning %q in:\n%s", want, out)
		}
	}
	for _, want := range []string{"requires a restart", "ignoring"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q framing in:\n%s", want, out)
		}
	}
}

func TestWarnUnappliedChangesSilentWhenUnchanged(t *testing.T) {
	cfg := config.Config{Addr: ":11500", Backend: "llamacpp", MaxLoaded: 1, CacheMaxEntries: 100}
	out := captureStderr(t, func() { warnUnappliedChanges(cfg, cfg) })
	if out != "" {
		t.Fatalf("identical configs should produce no warnings, got:\n%s", out)
	}
}

func TestWarnUnappliedChangesModelExtras(t *testing.T) {
	on := true
	startup := config.Config{Models: []config.Model{{ID: "m1", Ctx: 4096}}}
	changed := config.Config{Models: []config.Model{{ID: "m1", Ctx: 4096, ClampMaxTokens: &on, InputUSDPerMTok: 3}}}

	out := captureStderr(t, func() { warnUnappliedChanges(startup, changed) })
	if !strings.Contains(out, `model "m1"`) || !strings.Contains(out, "per-model overrides") {
		t.Fatalf("expected a per-model-overrides warning naming m1, got:\n%s", out)
	}
}

func TestWarnUnappliedChangesModelSpecChangeNotDoubleWarned(t *testing.T) {
	// A ModelSpec-level change (path) is scheduler.Reload's job, not this warning's.
	startup := config.Config{Models: []config.Model{{ID: "m1", Path: "/old"}}}
	changed := config.Config{Models: []config.Model{{ID: "m1", Path: "/new"}}}

	out := captureStderr(t, func() { warnUnappliedChanges(startup, changed) })
	if strings.Contains(out, "per-model overrides") {
		t.Fatalf("a plain ModelSpec field change (path) is not a per-model 'extras' change, got:\n%s", out)
	}
}

func TestModelExtrasChanged(t *testing.T) {
	onT, onF := true, false
	base := config.Model{ID: "m1", TimeoutS: 30}
	cases := []struct {
		name    string
		other   config.Model
		changed bool
	}{
		{"identical", config.Model{ID: "m1", TimeoutS: 30}, false},
		{"timeout differs", config.Model{ID: "m1", TimeoutS: 60}, true},
		{"cost differs", config.Model{ID: "m1", TimeoutS: 30, InputUSDPerMTok: 1}, true},
		{"nil vs set bool", config.Model{ID: "m1", TimeoutS: 30, EnforceContext: &onT}, true},
		{"fallbacks differ", config.Model{ID: "m1", TimeoutS: 30, ModelFallbacks: []string{"b"}}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := modelExtrasChanged(base, tc.other); got != tc.changed {
				t.Errorf("modelExtrasChanged = %v, want %v", got, tc.changed)
			}
		})
	}
	// Two entries with the SAME non-nil override must not be flagged.
	a := config.Model{ID: "m1", EnforceContext: &onT}
	b := config.Model{ID: "m1", EnforceContext: &onF}
	bSame := config.Model{ID: "m1", EnforceContext: &onT}
	if !modelExtrasChanged(a, b) {
		t.Fatal("differing override values should be flagged as changed")
	}
	if modelExtrasChanged(a, bSame) {
		t.Fatal("same override value (different pointer) must not be flagged as changed")
	}
}
