package main

import (
	"context"
	"errors"
)

// doctorCmd checks the configuration and its dependencies (0003 SPEC
// R296~R304). Stub: a later milestone replaces this file.
func doctorCmd(_ context.Context, e env, args []string) error {
	fs := newFlagSet("doctor", e)
	fs.String("config", defaultConfigPath(), "path to the TOML config file")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	return errors.New("not implemented yet")
}
