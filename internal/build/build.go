// Package build holds version metadata injected at compile time via -ldflags.
package build

import "fmt"

// These are overridden by the release build with:
//
//	-ldflags "-X github.com/ankit373/mainspring/internal/build.Version=v1.2.3 ..."
var (
	Version = "dev"
	Commit  = "none"
	Date    = "unknown"
)

// String renders a one-line version banner.
func String() string {
	return fmt.Sprintf("mainspring %s (commit %s, built %s)", Version, Commit, Date)
}
