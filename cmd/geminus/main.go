// Command geminus は Claude Code / Hermes Agent の入出力を PTY 越しに中継する。
package main

import (
	"os"

	"github.com/hauntedfail/geminus/internal/cli"
)

func main() {
	os.Exit(cli.Run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}
