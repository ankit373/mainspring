// Package install provides opt-in, checksum-verified engine installation.
//
// Design rules (from the locked decision): detect-first, never silent. Nothing
// here runs unless the user explicitly invokes `mainspring install`. Every
// download is SHA-256 verified against a pinned digest — from an explicit
// --url/--sha256 pair or a curated manifest — and a receipt records what was
// installed so the backends prefer the managed binary over PATH.
package install

import (
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"time"
)

//go:embed manifest.json
var embeddedManifest []byte

// Artifact is one pinned, checksum-verified download for a platform.
type Artifact struct {
	URL    string `json:"url"`
	SHA256 string `json:"sha256"`
}

// Entry pins a backend's version and its per-platform artifacts (keyed "os/arch").
type Entry struct {
	Version   string              `json:"version"`
	Artifacts map[string]Artifact `json:"artifacts"`
}

// Manifest maps backend name -> pinned entry.
type Manifest struct {
	Backends map[string]Entry `json:"backends"`
}

// Receipt records a completed install.
type Receipt struct {
	Backend     string `json:"backend"`
	Version     string `json:"version"`
	Path        string `json:"path"`
	SHA256      string `json:"sha256"`
	InstalledAt string `json:"installed_at"`
}

// Spec directs an install: an explicit URL+SHA256 pins directly; otherwise the
// manifest is consulted for Version (or the manifest's default version).
type Spec struct {
	Version string
	URL     string
	SHA256  string
	PubKey  string // base64 ed25519 public key; when set, the manifest must be signed
	SigPath string // detached signature path (defaults to <manifest>.sig)
}

// PlatformKey is "os/arch", e.g. "darwin/arm64".
func PlatformKey() string { return runtime.GOOS + "/" + runtime.GOARCH }

// binaryName maps a backend to the executable it installs.
func binaryName(backend string) string {
	switch backend {
	case "llamacpp":
		return "llama-server"
	default:
		return backend
	}
}

// ManagedDir is ~/.config/mainspring/bin.
func ManagedDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return "bin"
	}
	return filepath.Join(home, ".config", "mainspring", "bin")
}

func receiptPath(backend string) string {
	return filepath.Join(ManagedDir(), backend, "receipt.json")
}

// ManagedPath returns the installed binary path for backend if a valid receipt
// exists and the file is present.
func ManagedPath(backend string) (string, bool) {
	b, err := os.ReadFile(receiptPath(backend))
	if err != nil {
		return "", false
	}
	var r Receipt
	if json.Unmarshal(b, &r) != nil || r.Path == "" {
		return "", false
	}
	if fi, err := os.Stat(r.Path); err != nil || fi.IsDir() {
		return "", false
	}
	return r.Path, true
}

// LoadManifest returns the manifest: a user override at path (or the default
// ~/.config/mainspring/install.json) layered over the embedded one.
func LoadManifest(path string) (Manifest, error) {
	var m Manifest
	if err := json.Unmarshal(embeddedManifest, &m); err != nil {
		return m, fmt.Errorf("embedded manifest: %w", err)
	}
	if path == "" {
		home, err := os.UserHomeDir()
		if err == nil {
			path = filepath.Join(home, ".config", "mainspring", "install.json")
		}
	}
	if path != "" {
		if b, err := os.ReadFile(path); err == nil {
			var override Manifest
			if err := json.Unmarshal(b, &override); err != nil {
				return m, fmt.Errorf("manifest %s: %w", path, err)
			}
			if m.Backends == nil {
				m.Backends = map[string]Entry{}
			}
			for k, v := range override.Backends {
				m.Backends[k] = v
			}
		}
	}
	return m, nil
}

// resolve returns the (url, sha256, version) to install for backend.
func resolve(backend string, spec Spec, manifestPath string) (url, sha, version string, err error) {
	if spec.URL != "" {
		if spec.SHA256 == "" {
			return "", "", "", fmt.Errorf("--sha256 is required with --url (installs are always checksum-verified)")
		}
		v := spec.Version
		if v == "" {
			v = "pinned"
		}
		return spec.URL, spec.SHA256, v, nil
	}
	m, err := LoadManifest(manifestPath)
	if err != nil {
		return "", "", "", err
	}
	entry, ok := m.Backends[backend]
	if !ok {
		return "", "", "", fmt.Errorf("no manifest entry for %q on this system — supply --url and --sha256 to pin a specific artifact", backend)
	}
	key := PlatformKey()
	art, ok := entry.Artifacts[key]
	if !ok {
		return "", "", "", fmt.Errorf("manifest has no %q artifact for platform %s", backend, key)
	}
	return art.URL, art.SHA256, entry.Version, nil
}

// Install downloads, checksum-verifies, and installs backend's engine binary,
// writing a receipt. It is the ONLY function that performs a download, and only
// when explicitly invoked.
func Install(ctx context.Context, backend string, spec Spec, manifestPath string, out io.Writer) (Receipt, error) {
	// When installing from a manifest (not a direct --url pin) and a public key
	// is configured, the manifest must carry a valid detached signature.
	if spec.URL == "" {
		if err := verifyManifestFile(manifestPath, spec.SigPath, spec.PubKey); err != nil {
			return Receipt{}, err
		}
	}
	url, wantSHA, version, err := resolve(backend, spec, manifestPath)
	if err != nil {
		return Receipt{}, err
	}
	fmt.Fprintf(out, "installing %s %s for %s\n  from %s\n", backend, version, PlatformKey(), url)

	destDir := filepath.Join(ManagedDir(), backend, version)
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return Receipt{}, err
	}
	dest := filepath.Join(destDir, binaryName(backend))

	// Stage, verify, then publish. The digest is computed over the staged file and
	// only a match earns the rename, so nothing unverified is ever reachable at
	// dest — not even transiently, and not if the process dies mid-install.
	staged, gotSHA, err := stage(ctx, url, destDir)
	if err != nil {
		return Receipt{}, err
	}
	// Harmless once the rename below has consumed it.
	defer func() { _ = os.Remove(staged) }()

	if !equalHex(gotSHA, wantSHA) {
		return Receipt{}, fmt.Errorf("checksum mismatch: got %s, want %s (refusing to install)", gotSHA, wantSHA)
	}
	// Chmod before the rename so the binary is executable the instant it appears,
	// rather than existing briefly in a state a concurrent exec would fail on.
	if err := os.Chmod(staged, 0o755); err != nil {
		return Receipt{}, err
	}
	if err := os.Rename(staged, dest); err != nil {
		return Receipt{}, err
	}

	rec := Receipt{Backend: backend, Version: version, Path: dest, SHA256: gotSHA, InstalledAt: time.Now().UTC().Format(time.RFC3339)}
	b, _ := json.MarshalIndent(rec, "", "  ")
	if err := os.WriteFile(receiptPath(backend), b, 0o600); err != nil {
		return Receipt{}, err
	}
	fmt.Fprintf(out, "  verified sha256=%s\n  installed → %s\n", gotSHA, dest)
	return rec, nil
}

// maxDownloadBytes caps an install. Engine builds are large — a CUDA-enabled
// llama-server with its shared libraries runs to hundreds of megabytes — so the
// ceiling is generous. It exists so a hostile or compromised URL cannot fill the
// disk, which is the same reason util.Accumulator bounds subprocess output.
//
// A var rather than a const only so the test can shrink it; pushing 2 GiB
// through a loopback socket to prove a limit works is a cost with no benefit.
var maxDownloadBytes int64 = 2 << 30 // 2 GiB

const (
	// Time to first response byte. Catches a server that accepts the connection
	// and then says nothing.
	downloadHeaderTimeout = 30 * time.Second
	// Absolute ceiling on the whole transfer. Slack enough for a large binary on
	// a slow link, but an install can never hang forever.
	downloadTotalTimeout = 30 * time.Minute
)

// newDownloadClient returns the client installs use. http.DefaultClient has no
// timeout at all, and `mainspring install` runs on a context with no deadline
// (signal.NotifyContext is only wired into serve), so the bound has to live here.
func newDownloadClient() *http.Client {
	return &http.Client{
		Timeout: downloadTotalTimeout,
		Transport: &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			ResponseHeaderTimeout: downloadHeaderTimeout,
		},
	}
}

// stage streams url to a temp file inside destDir and returns that path with the
// content's hex SHA-256. It deliberately does NOT publish the file: the caller
// verifies the digest first and renames only on a match, so unverified bytes are
// never reachable at the install path.
func stage(ctx context.Context, url, destDir string) (string, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", "", err
	}
	resp, err := newDownloadClient().Do(req)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", "", fmt.Errorf("download %s: status %d", url, resp.StatusCode)
	}

	tmp, err := os.CreateTemp(destDir, ".dl-*")
	if err != nil {
		return "", "", err
	}
	tmpName := tmp.Name()
	// Every path out of here except a clean return removes the staging file; a
	// failed install must not leave the payload lying around under another name.
	clean := func(err error) (string, string, error) {
		tmp.Close()
		_ = os.Remove(tmpName)
		return "", "", err
	}

	// One byte past the cap, so a file landing exactly on the limit is accepted
	// and anything beyond it is detectable rather than silently truncated —
	// truncation would surface as a checksum mismatch and send the operator
	// hunting for a corrupt artifact that is really an oversized one.
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(tmp, h), io.LimitReader(resp.Body, maxDownloadBytes+1))
	if err != nil {
		return clean(err)
	}
	if n > maxDownloadBytes {
		return clean(fmt.Errorf("download %s: too large (exceeds %d bytes)", url, maxDownloadBytes))
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return "", "", err
	}
	return tmpName, hex.EncodeToString(h.Sum(nil)), nil
}

func equalHex(a, b string) bool {
	return len(a) == len(b) && a != "" && equalFold(a, b)
}

func equalFold(a, b string) bool {
	for i := 0; i < len(a); i++ {
		ca, cb := a[i], b[i]
		if 'A' <= ca && ca <= 'Z' {
			ca += 'a' - 'A'
		}
		if 'A' <= cb && cb <= 'Z' {
			cb += 'a' - 'A'
		}
		if ca != cb {
			return false
		}
	}
	return true
}
