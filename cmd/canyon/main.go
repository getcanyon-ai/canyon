// Command canyon is the CLI: one binary for the platform engineer who types it and the agent that
// runs it (design/specs/cli.md). The only difference is the format, and the builder never picks it.
package main

import (
	"os"

	"github.com/getcanyon-ai/canyon/cli"
)

// version is stamped at build time, the same shape canyon-server is stamped with.
var version = "dev"

func main() {
	cli.Version = version
	self, err := cli.Locate(os.Args[0])
	if err != nil { // argv[0] the shell did not resolve: the file the kernel ran, symlinks resolved on Linux
		self, err = os.Executable()
	}
	if err == nil {
		cli.Self = self
	}
	os.Exit(cli.Main(os.Args[1:], os.Stdout, os.Stderr, os.Stdin, os.Getenv, cli.IsTTY(os.Stdout)))
}
