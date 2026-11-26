package main

import (
	"context"
	"errors"
)

// appLinkCmd prints the link that creates the GitHub App (0003 SPEC R305,
// R306). It reads no configuration file. Stub: a later milestone replaces
// this file.
func appLinkCmd(_ context.Context, e env, args []string) error {
	fs := newFlagSet("app-link", e)
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	return errors.New("not implemented yet")
}
