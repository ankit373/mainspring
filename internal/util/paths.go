package util

import (
	"os"
	"path/filepath"
	"runtime"
)

// ConfigDir is the per-user directory Mainspring keeps its config, ledger and
// managed engine binaries in. It is the single definition of that convention;
// four separate copies of the same filepath.Join is how a per-platform fix gets
// applied to three of them.
//
// On Windows this resolves under %AppData%, where a Windows user would actually
// look. Everywhere else it stays ~/.config/mainspring.
//
// This is deliberately NOT os.UserConfigDir() across the board. That returns
// ~/Library/Application Support on macOS, so adopting it wholesale would
// relocate every existing macOS user's config to a path they never chose and
// make their current one invisible — a silent break, for tidiness.
//
// Returns a relative "mainspring" if the home directory cannot be determined, so
// callers still get a usable path rather than something rooted at "/".
func ConfigDir() string {
	if runtime.GOOS == "windows" {
		// os.UserConfigDir is %AppData% here, which is the right answer; fall back
		// to the home directory if the environment does not define it.
		if dir, err := os.UserConfigDir(); err == nil {
			return filepath.Join(dir, "mainspring")
		}
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, "mainspring")
		}
		return "mainspring"
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "mainspring"
	}
	return filepath.Join(home, ".config", "mainspring")
}

// ConfigPath joins elements onto ConfigDir.
func ConfigPath(elem ...string) string {
	return filepath.Join(append([]string{ConfigDir()}, elem...)...)
}

// ExeName appends the platform's executable suffix to name. On Windows a file
// without .exe is not executable, so an installed engine binary written under a
// bare name would report a successful install and then fail to run.
func ExeName(name string) string {
	if runtime.GOOS == "windows" {
		return name + ".exe"
	}
	return name
}
