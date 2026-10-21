// Command moosed is the long-lived moose server.
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/change-fabric/moose/internal/config"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Parse()
	if *showVersion {
		fmt.Println("moosed", version)
		return
	}
	cfg, err := config.Load(os.Getenv)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	// The server is wired up in a later phase; for now validate config only.
	fmt.Fprintf(os.Stderr, "moosed %s: config ok (env=%s); server not implemented yet\n", version, cfg.Env)
	os.Exit(1)
}
