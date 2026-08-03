package install

import (
	"fmt"
	"slices"
	"strings"

	"github.com/ankit373/mainspring/internal/backend"
)

// installableNames are the backends `install` can fetch a binary for.
//
// It is llamacpp alone, and that is not an oversight: being *managed* (Mainspring
// runs it as a subprocess) is not the same as being a *downloadable binary*.
// `newBackendByName` only ever consults `install.ManagedPath("llamacpp")`, so a
// binary installed under any other name is never read by anything.
var installableNames = []string{"llamacpp"}

// notDistributedAsBinary maps a managed backend Mainspring cannot download to the
// reason and the real remedy. Keeping the reason next to the name is what stops
// this from degrading into "unknown backend", which would be a lie.
var notDistributedAsBinary = map[string]string{
	"mlx": "it runs as a Python module (`python -m mlx_lm.server`), so there is no binary to " +
		"fetch — install it with `pip install mlx-lm` and point --mlx-python at that interpreter",
}

// checkInstallable rejects a backend Mainspring can never manage a binary for,
// before any network work happens.
//
// Without this, `install ollama --url … --sha256 …` downloaded, verified and wrote
// a binary that the ollama backend never reads, printing "installed →" while the
// same CLI reported "adopt-only; Mainspring will not install it" in the very next
// command (#233). An adopt backend runs its own process and holds its own weights,
// so there is nothing here for Mainspring to own; installing one is a no-op that
// looks like a success.
func checkInstallable(name string) error {
	if slices.Contains(installableNames, name) {
		return nil
	}
	if backend.IsAdopt(name) {
		return fmt.Errorf("%s is adopt-only: it runs its own process and holds its own weights, so "+
			"Mainspring never installs it — start it yourself and point Mainspring at it "+
			"(installable: %s)", name, strings.Join(installableNames, ", "))
	}
	if why, ok := notDistributedAsBinary[name]; ok {
		return fmt.Errorf("%s is not installed as a binary: %s (installable: %s)",
			name, why, strings.Join(installableNames, ", "))
	}
	return fmt.Errorf("unknown backend %q — installable backends are %s",
		name, strings.Join(installableNames, ", "))
}
