// Command whatchanged answers the first question of every incident:
// what changed?
package main

import (
	"os"

	"github.com/simplyzee/whatchanged/internal/cli"
)

// Set with -ldflags "-X main.version=… -X main.commit=…".
var (
	version = "dev"
	commit  = "none"
)

func main() {
	cli.Version, cli.Commit = version, commit
	os.Exit(cli.Main(os.Args[1:], os.Stdout, os.Stderr))
}
