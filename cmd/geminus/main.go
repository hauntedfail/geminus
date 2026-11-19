// Command geminus talks to agents on other computers.
package main

import (
	"context"
	"os"

	"github.com/hauntedfail/geminus/internal/chat"
)

func main() {
	os.Exit(chat.Run(context.Background(), os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}
