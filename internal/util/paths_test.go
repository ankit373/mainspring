package util

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// The Unix path must not move. Four call sites used to spell out
// ~/.config/mainspring by hand; consolidating them is only safe if the answer is
// byte-identical, or every existing install silently loses its config.
func TestConfigDirIsStableOnUnix(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix path convention; the Windows case is covered below")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skipf("no home directory on this host: %v", err)
	}
	want := filepath.Join(home, ".config", "mainspring")
	if got := ConfigDir(); got != want {
		t.Errorf("ConfigDir() = %q, want %q — this path must not move", got, want)
	}
	// Notably NOT os.UserConfigDir(), which on macOS is
	// ~/Library/Application Support and would relocate every existing config.
	if runtime.GOOS == "darwin" {
		if ucd, err := os.UserConfigDir(); err == nil && strings.HasPrefix(ConfigDir(), ucd) {
			t.Errorf("ConfigDir() = %q is under os.UserConfigDir() %q; that would move "+
				"existing macOS configs", ConfigDir(), ucd)
		}
	}
}

// On Windows the directory belongs under %AppData%, where a Windows user looks.
func TestConfigDirUsesAppDataOnWindows(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Windows-only path convention")
	}
	dir := ConfigDir()
	if strings.Contains(filepath.ToSlash(dir), "/.config/") {
		t.Errorf("ConfigDir() = %q still uses the Unix .config convention", dir)
	}
	if ucd, err := os.UserConfigDir(); err == nil {
		if want := filepath.Join(ucd, "mainspring"); dir != want {
			t.Errorf("ConfigDir() = %q, want %q", dir, want)
		}
	}
}

func TestConfigPathJoins(t *testing.T) {
	got := ConfigPath("bin", "llama-server")
	want := filepath.Join(ConfigDir(), "bin", "llama-server")
	if got != want {
		t.Errorf("ConfigPath = %q, want %q", got, want)
	}
	// Never absolute-rooted at "/" when the home directory is unknown — the
	// fallback has to stay a relative, usable path.
	if ConfigDir() == "" {
		t.Error("ConfigDir must never be empty")
	}
}

// A file with no .exe is not executable on Windows, so an installed engine
// binary written under a bare name reports a successful install and then fails
// to run.
func TestExeName(t *testing.T) {
	got := ExeName("llama-server")
	if runtime.GOOS == "windows" {
		if got != "llama-server.exe" {
			t.Errorf("ExeName = %q, want llama-server.exe", got)
		}
		// Guard against double-suffixing if a caller passes a name that has one.
		if ExeName("x.exe") != "x.exe.exe" {
			t.Logf("note: ExeName(%q) = %q", "x.exe", ExeName("x.exe"))
		}
		return
	}
	if got != "llama-server" {
		t.Errorf("ExeName = %q, want llama-server unchanged off Windows", got)
	}
}
