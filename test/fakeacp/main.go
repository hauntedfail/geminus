// Command fakeacp is the scripted ACP agent of internal/acp/acptest as a
// program. The end-to-end tests install it as an agent, and under the name
// "ssh" as the way to a remote computer.
package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/hauntedfail/geminus/internal/acp/acptest"
)

func main() {
	args := strings.Join(os.Args[1:], "\n")
	// As ssh, the first call geminus makes asks what the computer has.
	if strings.Contains(args, "geminus-probe") {
		record("FAKEACP_PROBE_ARGS", args)
		fmt.Print(os.Getenv("FAKEACP_PROBE"))
		return
	}
	record("FAKEACP_ARGS", args)
	os.Exit(acptest.Serve(os.Stdin, os.Stdout, os.Stderr, os.Getenv("FAKEACP_STATE")))
}

// record saves how the program was called, so a test can check the ssh
// command line.
func record(env, args string) {
	if path := os.Getenv(env); path != "" {
		_ = os.WriteFile(path, []byte(args), 0o600)
	}
}
