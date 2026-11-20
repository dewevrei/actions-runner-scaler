// Command actions-runner-scaler runs ephemeral GitHub Actions self-hosted
// runners as local Docker containers and scales them with the number of
// assigned jobs, using github.com/actions/scaleset.
package main

import (
	"flag"
	"fmt"
	"os"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	showVersion := flag.Bool("version", false, "print the version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Println(version)
		return
	}

	fmt.Fprintln(os.Stderr, "actions-runner-scaler: not implemented yet")
	os.Exit(1)
}
