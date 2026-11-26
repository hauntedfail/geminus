// Command release-build compiles an existing tag using the OTA build recipe.
package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/hauntedfail/geminus/internal/updates"
	"github.com/hauntedfail/geminus/internal/updates/build"
)

func main() {
	if len(os.Args) != 3 || !updates.IsRelease(os.Args[1]) {
		fmt.Fprintln(os.Stderr, "usage: release-build X.Y.Z OUTPUT-DIRECTORY")
		os.Exit(2)
	}
	repo, err := filepath.Abs(".")
	if err == nil {
		source := build.Source{
			Repo: repo, Git: "git", Go: "go",
			Targets: strings.Split(build.Targets, ","),
			Key:     updates.PublicKeyText(updates.ReleaseKey()),
		}
		err = source.Build(context.Background(), os.Args[1], os.Args[2])
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
