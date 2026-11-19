// Package version は geminus のバージョン文字列を持つ。
package version

import (
	"fmt"
	"runtime"
)

// Version はリリース時に
// -ldflags "-X github.com/hauntedfail/geminus/internal/version.Version=..." で上書きする。
var Version = "0.1.0-dev"

// String は `geminus version` が出す1行を返す。
func String() string {
	return fmt.Sprintf("geminus %s (%s %s/%s)", Version, runtime.Version(), runtime.GOOS, runtime.GOARCH)
}
