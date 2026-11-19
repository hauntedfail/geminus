// Command fakeacp is the scripted ACP agent of internal/acp/acptest as a
// program. The end-to-end tests install it as the agent, and under the name
// "ssh" as the transport to a remote one.
package main

import (
	"os"
	"strings"

	"github.com/hauntedfail/geminus/internal/acp/acptest"
)

func main() {
	// Record how we were called, so a test can check the ssh command line.
	if path := os.Getenv("FAKEACP_ARGS"); path != "" {
		_ = os.WriteFile(path, []byte(strings.Join(os.Args[1:], "\n")), 0o600)
	}
	os.Exit(acptest.Serve(os.Stdin, os.Stdout, os.Stderr, os.Getenv("FAKEACP_STATE")))
}
