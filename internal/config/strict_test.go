package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// write puts contents in a temp file and returns its path.
func write(t *testing.T, name, contents string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// The bug this exists for: a key the struct does not know used to be dropped in
// silence, so a misspelled `enforce_context` started a server with the guardrail
// off and nothing ever said so.
func TestLoadRefusesUnknownKeys(t *testing.T) {
	tests := []struct {
		name        string
		yaml        string
		wantKey     string
		wantSuggest string // "" = no suggestion should be offered
	}{
		{
			name:        "misspelled guardrail — the one that silently disabled a safety check",
			yaml:        "enfore_context: true\n",
			wantKey:     "enfore_context",
			wantSuggest: "enforce_context",
		},
		{
			name:        "misspelled backend host",
			yaml:        "olama_host: \"http://127.0.0.1:11434\"\n",
			wantKey:     "olama_host",
			wantSuggest: "ollama_host",
		},
		{
			name:        "unknown key nested in a model entry",
			yaml:        "models:\n  - id: m1\n    ctx_size: 8192\n",
			wantKey:     "ctx_size",
			wantSuggest: "ctx",
		},
		{
			name:        "unknown key nested in a tenant entry",
			yaml:        "tenants:\n  - name: t1\n    key: k\n    rate_rpmm: 60\n",
			wantKey:     "rate_rpmm",
			wantSuggest: "rate_rpm",
		},
		{
			name: "a key with no near miss is still refused, just without a guess",
			yaml: "completely_made_up_setting: 1\n",
			// Nothing in the schema is within the distance bound, and inventing a
			// suggestion would be worse than offering none.
			wantKey:     "completely_made_up_setting",
			wantSuggest: "",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Load(write(t, "c.yaml", tc.yaml))
			if err == nil {
				t.Fatal("config loaded silently; the key was dropped")
			}
			msg := err.Error()
			if !strings.Contains(msg, tc.wantKey) {
				t.Errorf("error does not name the offending key %q: %s", tc.wantKey, msg)
			}
			if !strings.Contains(msg, "unknown field") {
				t.Errorf("error does not say the field is unknown: %s", msg)
			}
			if tc.wantSuggest == "" {
				if strings.Contains(msg, "did you mean") {
					t.Errorf("suggested something for a key with no near miss: %s", msg)
				}
				return
			}
			if !strings.Contains(msg, "did you mean \""+tc.wantSuggest+"\"") {
				t.Errorf("error does not suggest %q: %s", tc.wantSuggest, msg)
			}
		})
	}
}

// Every unknown key is reported, not just the first: an operator fixing a config
// should not have to restart the server once per typo.
func TestLoadReportsEveryUnknownKey(t *testing.T) {
	_, err := Load(write(t, "c.yaml",
		"olama_host: h\nenfore_context: true\nmax_loadedd: 2\n"))
	if err == nil {
		t.Fatal("expected an error")
	}
	for _, key := range []string{"olama_host", "enfore_context", "max_loadedd"} {
		if !strings.Contains(err.Error(), key) {
			t.Errorf("error omits %q: %s", key, err)
		}
	}
}

// The error must point at the line, because a long config with one bad key is
// exactly when that matters.
func TestUnknownKeyErrorCarriesTheLine(t *testing.T) {
	_, err := Load(write(t, "c.yaml", "addr: \":1\"\nmax_loaded: 1\nenfore_context: true\n"))
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "line 3") {
		t.Errorf("error does not name line 3: %s", err)
	}
}

// Strictness must not have made valid configs fail. These are the shapes the
// repo actually ships and documents.
func TestLoadAcceptsValidConfigs(t *testing.T) {
	valid := map[string]string{
		"conformance shape": `addr: ":11500"
keep_alive_seconds: 0
backend: ollama
ollama_host: "http://127.0.0.1:18080"
usage_ledger: "off"
models:
  - id: mock-model
    path: mock-model
`,
		"helm values shape": `addr: ":11500"
keep_alive_seconds: 300
max_loaded: 1
max_resident_mb: 20000
max_inflight: 4
max_queue: 16
backend: llamacpp
models: []
`,
		"every model field": `models:
  - id: m1
    backend: llamacpp
    fallbacks: [ollama]
    path: /m.gguf
    ctx: 8192
    gpu_layers: -1
    preload: true
    timeout_seconds: 30
    args: ["--foo"]
    input_usd_per_mtok: 1.5
    output_usd_per_mtok: 2.0
    enforce_context: false
    model_fallbacks: [m2]
`,
		"tenants": `tenants:
  - name: team-a
    key: change-me
    role: inference
    rate_rpm: 120
    token_budget: 2000000
    window_sec: 3600
`,
		"empty file is just defaults":    "",
		"comments only is just defaults": "# nothing here\n",
	}
	for name, y := range valid {
		t.Run(name, func(t *testing.T) {
			if _, err := Load(write(t, "c.yaml", y)); err != nil {
				t.Fatalf("valid config rejected: %v", err)
			}
		})
	}
}

// The repo's own config must load, so a change to the schema cannot quietly
// invalidate the file CI runs conformance against.
func TestLoadAcceptsTheConformanceConfig(t *testing.T) {
	p := filepath.Join("..", "..", "test", "conformance", "config.yaml")
	if _, err := os.Stat(p); err != nil {
		t.Skip("conformance config not present")
	}
	if _, err := Load(p); err != nil {
		t.Fatalf("test/conformance/config.yaml no longer loads: %v", err)
	}
}

// A genuine type mismatch already says something true and must not be rewritten
// into an unknown-field complaint.
func TestTypeErrorsAreLeftAlone(t *testing.T) {
	_, err := Load(write(t, "c.yaml", "max_loaded: \"not a number\"\n"))
	if err == nil {
		t.Fatal("expected an error")
	}
	if strings.Contains(err.Error(), "unknown field") {
		t.Errorf("a type error was reported as an unknown field: %s", err)
	}
}

func TestNearest(t *testing.T) {
	known := configFields()
	tests := []struct{ key, want string }{
		{"enfore_context", "enforce_context"},
		{"ollama_hosts", "ollama_host"},
		{"max_loadedd", "max_loaded"},
		// Wrong-but-related name rather than a misspelling: too far by edit
		// distance, obvious by shared prefix.
		{"ctx_size", "ctx"},
		{"ctx_sze", "ctx"},
		// Too far from anything: a wrong guess is worse than none.
		{"zzzzzzzzzzzzzzzz", ""},
		// Short keys must not match half the schema on a 1-character bound.
		{"xy", ""},
	}
	for _, tc := range tests {
		if got := nearest(tc.key, known); got != tc.want {
			t.Errorf("nearest(%q) = %q, want %q", tc.key, got, tc.want)
		}
	}
}

func TestEditDistance(t *testing.T) {
	tests := []struct {
		a, b string
		want int
	}{
		{"", "", 0},
		{"abc", "abc", 0},
		{"abc", "", 3},
		{"", "abc", 3},
		{"enfore_context", "enforce_context", 1}, // one insertion
		{"ctx_sze", "ctx_size", 1},
		{"kitten", "sitting", 3},
	}
	for _, tc := range tests {
		if got := editDistance(tc.a, tc.b); got != tc.want {
			t.Errorf("editDistance(%q, %q) = %d, want %d", tc.a, tc.b, got, tc.want)
		}
	}
}
