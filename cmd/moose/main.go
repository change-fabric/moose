// Command moose is the CLI client for a moose hub.
package main

import (
	"context"
	"fmt"
	"os"

	"github.com/change-fabric/moose/internal/cli"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	root := cli.NewRoot(cli.Env{Getenv: os.Getenv, Stdin: os.Stdin, Stdout: os.Stdout, Stderr: os.Stderr, Version: version})
	if err := root.ExecuteContext(context.Background()); err != nil {
		fmt.Fprintln(os.Stderr, "moose:", err)
		os.Exit(cli.ExitCode(err))
	}
}
