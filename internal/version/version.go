// Package version reports the release a mutant build was cut from. The
// committed VERSION file is maintained by the release workflow — written
// and committed as part of the version bump — so every build path knows
// the true version, including `go install …@<tag>` builds where no
// ldflags stamping runs (D17).
package version

import (
	"strings"

	_ "embed"
)

//go:embed VERSION
var data string

// String returns the release version, or "dev" before the first release.
func String() string {
	if v := strings.TrimSpace(data); v != "" {
		return v
	}
	return "dev"
}
