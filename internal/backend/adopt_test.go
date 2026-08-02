package backend

import (
	"strings"
	"testing"
)

func TestIsAdopt(t *testing.T) {
	for _, name := range AdoptNames {
		if !IsAdopt(name) {
			t.Errorf("IsAdopt(%q) = false, want true", name)
		}
	}
	for _, name := range []string{"llamacpp", "mlx", "", "OLLAMA"} {
		if IsAdopt(name) {
			t.Errorf("IsAdopt(%q) = true, want false", name)
		}
	}
}

// The failure this replaces told an operator to `ollama pull qwen` when they had
// written `id: qwen` with the real model name in `path` — sending them after a
// model that does not exist while the field holding the answer went unmentioned.
func TestMissingModelErrorNamesWhatIsActuallyThere(t *testing.T) {
	err := MissingModelError("ollama", "http://127.0.0.1:11434",
		ModelSpec{ID: "qwen", Path: "Qwen2.5-Coder:7b"},
		[]string{"Qwen2.5-Coder:7b", "llama3.2:1b"},
		"Pull it with `ollama pull qwen` (Mainspring will not pull automatically).")
	msg := err.Error()

	for _, want := range []string{
		"ollama",                 // which backend
		`model "qwen"`,           // what was asked for
		"http://127.0.0.1:11434", // where it looked
		"Qwen2.5-Coder:7b",       // what is actually there
		"llama3.2:1b",            // …all of it
		"`path` is ignored",      // why their config did not work
		"id: Qwen2.5-Coder:7b",   // what to write instead
		"ollama pull qwen",       // the backend's own remedy
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("message omits %q:\n%s", want, msg)
		}
	}
}

// With no path set there is nothing to explain, so the path advice must not
// appear — an error that speculates is an error people stop reading.
func TestMissingModelErrorStaysQuietAboutAnUnsetPath(t *testing.T) {
	msg := MissingModelError("lmstudio", "http://127.0.0.1:1234",
		ModelSpec{ID: "nope"}, []string{"some-model"},
		"Load it in LM Studio first (Mainspring will not load it).").Error()
	if strings.Contains(msg, "path") {
		t.Errorf("mentions path when none was set:\n%s", msg)
	}
	if !strings.Contains(msg, "some-model") {
		t.Errorf("does not say what the daemon has:\n%s", msg)
	}
}

func TestMissingModelErrorInventory(t *testing.T) {
	tests := []struct {
		name    string
		have    []string
		want    string
		wantNot string
	}{
		{
			name: "an empty daemon says so rather than trailing off",
			have: nil,
			want: "no models",
		},
		{
			name: "a long inventory is clipped with a count, not dumped",
			have: []string{"m0", "m1", "m2", "m3", "m4", "m5", "m6", "m7", "m8", "m9", "m10", "m11"},
			want: "and 2 more",
			// The 11th onward must not be listed, or a host with hundreds of models
			// buries the message.
			wantNot: "m11",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			msg := MissingModelError("ollama", "h", ModelSpec{ID: "x"}, tc.have, "").Error()
			if !strings.Contains(msg, tc.want) {
				t.Errorf("omits %q:\n%s", tc.want, msg)
			}
			if tc.wantNot != "" && strings.Contains(msg, tc.wantNot) {
				t.Errorf("should not list %q:\n%s", tc.wantNot, msg)
			}
		})
	}
}

// An empty remedy must not leave a dangling separator.
func TestMissingModelErrorWithoutRemedy(t *testing.T) {
	msg := MissingModelError("gpt4all", "h", ModelSpec{ID: "x"}, []string{"y"}, "").Error()
	if strings.HasSuffix(msg, ". ") || strings.HasSuffix(msg, ".") && strings.HasSuffix(msg, "..") {
		t.Errorf("trailing punctuation with no remedy: %q", msg)
	}
}
