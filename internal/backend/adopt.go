package backend

import (
	"fmt"
	"slices"
	"strings"
)

// AdoptNames are the detect-and-adopt backends: they hold their own weights and
// run their own process, so Mainspring never installs, pulls or loads for them —
// it adopts what is already there.
//
// The consequence that catches people out is that a model's **id** is the name
// the daemon itself uses, and ModelSpec.Path is unused: there is no file for
// Mainspring to open. This is the single definition of that set; a second copy is
// how the config linter and the startup path would drift apart.
var AdoptNames = []string{"ollama", "lmstudio", "llamafile", "gpt4all"}

// IsAdopt reports whether name is an adopt-only backend.
func IsAdopt(name string) bool { return slices.Contains(AdoptNames, name) }

// ManagedNames are the backends Mainspring runs as a subprocess: it owns the
// binary and opens the model file itself. These are the only backends
// `mainspring install` will accept — installing anything else writes a binary
// nothing ever reads (#233).
var ManagedNames = []string{"llamacpp", "mlx"}

// IsManaged reports whether name is a backend Mainspring runs as a subprocess.
func IsManaged(name string) bool { return slices.Contains(ManagedNames, name) }

// AllNames is every backend name the server accepts, managed first. The CLI's
// --backend help and its unknown-backend error both derive from this, so a
// backend cannot be added to the constructor and left out of the help text.
var AllNames = slices.Concat(ManagedNames, AdoptNames)

// missingModelListCap bounds how much of a daemon's inventory is echoed into an
// error. Enough to recognise the name you meant, not so much that a host with
// hundreds of models buries the message.
const missingModelListCap = 10

// MissingModelError reports that an adopt backend was asked for a model its
// daemon does not have.
//
// It exists because the obvious message is actively misleading. Telling an
// operator to `ollama pull qwen` when they wrote `id: qwen` with
// `path: Qwen2.5-Coder:7b` sends them after a model that does not exist, while
// the field holding the right answer goes unmentioned — it was silently ignored,
// which is the whole mistake. So this names the daemon, lists what it actually
// holds, and calls out `path` when it is set, because a set `path` on an adopt
// backend is near-proof that the id is what is wrong.
//
// remedy is the backend's own advice ("pull it with …", "load it in …"); empty
// omits it.
func MissingModelError(name, host string, spec ModelSpec, have []string, remedy string) error {
	var b strings.Builder
	fmt.Fprintf(&b, "%s: model %q is not available at %s", name, spec.ID, host)

	switch {
	case len(have) == 0:
		b.WriteString(" — it currently has no models")
	case len(have) > missingModelListCap:
		fmt.Fprintf(&b, " — it has: %s and %d more",
			strings.Join(have[:missingModelListCap], ", "), len(have)-missingModelListCap)
	default:
		fmt.Fprintf(&b, " — it has: %s", strings.Join(have, ", "))
	}

	if spec.Path != "" {
		fmt.Fprintf(&b, ". Note that `path` is ignored for %s: the model id must be the name %s "+
			"knows it by, so you likely want `id: %s` (you set path=%q)",
			name, name, spec.Path, spec.Path)
	}
	if remedy != "" {
		b.WriteString(". " + remedy)
	}
	return fmt.Errorf("%s", b.String())
}
