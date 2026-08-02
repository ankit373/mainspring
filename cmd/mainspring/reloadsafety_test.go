package main

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/ankit373/mainspring/internal/auth"
	"github.com/ankit373/mainspring/internal/backend"
	"github.com/ankit373/mainspring/internal/config"
	"github.com/ankit373/mainspring/internal/scheduler"
	"github.com/ankit373/mainspring/internal/server"
)

// reloadRig stands up the same objects runServe wires together, so a test can
// drive the real reloadConfig against a real config file on disk.
type reloadRig struct {
	path  string
	cfg   config.Config
	sched *scheduler.Scheduler
	authn *auth.Authenticator
	srv   *server.Server
	src   reloadSources
}

// newReloadRig starts a server from `startup` (the effective config, i.e. after
// flag overrides) with `flags` recording what came from the command line.
func newReloadRig(t *testing.T, startup config.Config, flags flagSources) *reloadRig {
	t.Helper()
	backends := map[string]backend.Backend{
		"llamacpp": &plainBackend{name: "llamacpp"},
		"ollama":   &listerBackend{name: "ollama", ids: []string{"discovered-1"}},
	}
	sched := scheduler.New(backends, modelSpecs(startup), scheduler.Options{})
	authn := buildAuth(startup)
	srv := server.New(sched, authn, nil)
	return &reloadRig{
		path:  filepath.Join(t.TempDir(), "config.yaml"),
		cfg:   startup,
		sched: sched,
		authn: authn,
		srv:   srv,
		src:   reloadSources{flags: flags, backends: backends, discover: startup.DiscoverModels},
	}
}

// writeConfig puts YAML at the rig's config path — the file a reload will read.
func (r *reloadRig) writeConfig(t *testing.T, yaml string) {
	t.Helper()
	if err := os.WriteFile(r.path, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
}

func (r *reloadRig) reload() error {
	return reloadConfig(r.path, r.cfg, r.src, r.sched, r.authn, r.srv)
}

func (r *reloadRig) modelIDs() []string {
	ids := make([]string, 0)
	for _, m := range r.sched.Models() {
		ids = append(ids, m.ID)
	}
	slices.Sort(ids)
	return ids
}

// TestReloadKeepsFlagSuppliedAuth is the security regression test. A server
// started with --api-key has credentials the config file has never heard of, so
// a reload that rebuilt auth from the file alone emptied the tenant set and put
// the server in open mode — where requireAdmin admits everyone and drain, reload,
// model load/unload, breaker reset and cache clear are all reachable anonymously.
func TestReloadKeepsFlagSuppliedAuth(t *testing.T) {
	startup := config.Config{
		APIKeys: []string{"secret-from-flag"},
		Models:  []config.Model{{ID: "m1", Path: "/w.gguf"}},
	}
	rig := newReloadRig(t, startup, flagSources{apiKeys: []string{"secret-from-flag"}})
	if rig.authn.Open() {
		t.Fatal("precondition: server should start authenticated")
	}

	// A perfectly ordinary config file that simply has no auth block.
	rig.writeConfig(t, "models:\n  - id: m1\n    path: /w.gguf\n")
	if err := rig.reload(); err != nil {
		t.Fatalf("reload should succeed, carrying the flag key across: %v", err)
	}
	if rig.authn.Open() {
		t.Fatal("reload switched the server to OPEN mode — every endpoint, /admin included, is now unauthenticated")
	}
}

// TestReloadRefusesToOpenAnAuthenticatedServer covers the case the flag carry-over
// cannot: keys were in the file, and the new file drops them. Reload must refuse
// rather than silently de-authenticate a running server.
func TestReloadRefusesToOpenAnAuthenticatedServer(t *testing.T) {
	startup := config.Config{
		APIKeys: []string{"secret-from-file"},
		Models:  []config.Model{{ID: "m1", Path: "/w.gguf"}},
	}
	rig := newReloadRig(t, startup, flagSources{}) // nothing came from a flag

	rig.writeConfig(t, "models:\n  - id: m2\n    path: /w2.gguf\n")
	err := rig.reload()
	if err == nil {
		t.Fatal("reload must be refused when it would open the server")
	}
	if !strings.Contains(err.Error(), "OPEN mode") {
		t.Errorf("error should name the consequence plainly, got: %v", err)
	}
	if rig.authn.Open() {
		t.Fatal("server was opened despite the refusal")
	}
	// A refused reload must be atomic: the model set must not have moved either.
	if got := rig.modelIDs(); !slices.Equal(got, []string{"m1"}) {
		t.Errorf("refused reload still mutated the scheduler: models=%v, want [m1]", got)
	}
}

// TestReloadIntoOpenServerStillAllowed — a server that was already open has no
// posture to protect, so reload behaves normally rather than refusing forever.
func TestReloadIntoOpenServerStillAllowed(t *testing.T) {
	startup := config.Config{Models: []config.Model{{ID: "m1", Path: "/w.gguf"}}}
	rig := newReloadRig(t, startup, flagSources{})
	if !rig.authn.Open() {
		t.Fatal("precondition: server should start open")
	}
	rig.writeConfig(t, "models:\n  - id: m2\n    path: /w2.gguf\n")
	if err := rig.reload(); err != nil {
		t.Fatalf("reload of an already-open server should succeed: %v", err)
	}
}

// TestReloadKeepsFlagSuppliedModels — --model entries live nowhere in the file,
// so a file-only reload dropped them and the server stopped serving a model it
// had been serving a moment earlier.
func TestReloadKeepsFlagSuppliedModels(t *testing.T) {
	flagModel := config.Model{ID: "from-flag", Path: "/flag.gguf"}
	startup := config.Config{Models: []config.Model{
		{ID: "from-file", Path: "/file.gguf"},
		flagModel,
	}}
	rig := newReloadRig(t, startup, flagSources{models: []config.Model{flagModel}})

	rig.writeConfig(t, "models:\n  - id: from-file\n    path: /file.gguf\n")
	if err := rig.reload(); err != nil {
		t.Fatalf("reload: %v", err)
	}
	if got := rig.modelIDs(); !slices.Equal(got, []string{"from-file", "from-flag"}) {
		t.Fatalf("models after reload = %v, want both [from-file from-flag]", got)
	}
}

// TestReloadFileWinsOverFlagModelOnIDClash — carrying flag models across must not
// override an operator who has since defined that id in the file.
func TestReloadFileWinsOverFlagModelOnIDClash(t *testing.T) {
	flagModel := config.Model{ID: "m1", Path: "/flag.gguf"}
	startup := config.Config{Models: []config.Model{flagModel}}
	rig := newReloadRig(t, startup, flagSources{models: []config.Model{flagModel}})

	rig.writeConfig(t, "models:\n  - id: m1\n    path: /file-wins.gguf\n")
	if err := rig.reload(); err != nil {
		t.Fatalf("reload: %v", err)
	}
	specs := rig.sched.Models()
	if len(specs) != 1 {
		t.Fatalf("want exactly one m1, got %d specs", len(specs))
	}
	if specs[0].Path != "/file-wins.gguf" {
		t.Errorf("path = %q, want the file's value to win", specs[0].Path)
	}
}

// TestReloadRerunsDiscovery — discovered models are appended to the scheduler's
// specs at startup and never written back to cfg.Models, so a file-only reload
// dropped every adopted model from a discovery-enabled server.
func TestReloadRerunsDiscovery(t *testing.T) {
	startup := config.Config{
		DiscoverModels: true,
		Models:         []config.Model{{ID: "m1", Path: "/w.gguf"}},
	}
	rig := newReloadRig(t, startup, flagSources{})
	rig.writeConfig(t, "discover_models: true\nmodels:\n  - id: m1\n    path: /w.gguf\n")
	if err := rig.reload(); err != nil {
		t.Fatalf("reload: %v", err)
	}
	if got := rig.modelIDs(); !slices.Contains(got, "discovered-1") {
		t.Fatalf("models after reload = %v, want the discovered model to survive", got)
	}
}

func TestResolveBool(t *testing.T) {
	tr, fa := true, false
	cases := []struct {
		global   bool
		override *bool
		want     bool
	}{
		{true, nil, true},   // inherit on
		{false, nil, false}, // inherit off
		{true, &fa, false},  // per-model false must win — the bug
		{false, &tr, true},  // per-model true must win
		{true, &tr, true},
		{false, &fa, false},
	}
	for _, c := range cases {
		if got := resolveBool(c.global, c.override); got != c.want {
			t.Errorf("resolveBool(%v, %v) = %v, want %v", c.global, c.override, got, c.want)
		}
	}
}

// A reload is the worse version of the silent-typo bug: the operator is watching
// for a change to take effect, and a dropped key means it quietly did not. The
// reload must be refused with the running config left exactly as it was — the
// same contract as a reload that would leave the server unauthenticated.
func TestReloadRefusesAConfigWithAnUnknownKey(t *testing.T) {
	rig := newReloadRig(t, config.Config{
		Models: []config.Model{{ID: "m1", Path: "/m1.gguf", Backend: "llamacpp"}},
	}, flagSources{})

	rig.writeConfig(t, `enfore_context: true
models:
  - id: m2
    path: /m2.gguf
    backend: llamacpp
`)
	err := rig.reload()
	if err == nil {
		t.Fatal("reload accepted a config with a misspelled key")
	}
	if !strings.Contains(err.Error(), "enfore_context") {
		t.Errorf("error does not name the offending key: %v", err)
	}
	if !strings.Contains(err.Error(), "keeping current config") {
		t.Errorf("error does not say the running config was kept: %v", err)
	}
	// The refusal must have changed nothing.
	if got := rig.modelIDs(); !slices.Equal(got, []string{"m1"}) {
		t.Errorf("models = %v after a refused reload, want [m1]", got)
	}
}
