// Package version provides the geminus version string.
package version

import (
	"fmt"
	"runtime"
)

// Version is overridden at release time with -ldflags "-X
// github.com/hauntedfail/geminus/internal/version.Version=...".
var Version = "0.4.0-dev"

// String returns the line printed by `geminus version`.
func String() string {
	return fmt.Sprintf("geminus %s (%s %s/%s)", Version, runtime.Version(), runtime.GOOS, runtime.GOARCH)
}
