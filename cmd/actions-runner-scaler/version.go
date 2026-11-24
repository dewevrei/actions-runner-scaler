package main

import (
	"context"
	"fmt"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

func versionCmd(_ context.Context, e env, args []string) error {
	fs := newFlagSet("version", e)
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	_, err := fmt.Fprintln(e.stdout, version)
	return err
}
