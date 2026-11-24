package main

import "context"

func statusCmd(_ context.Context, e env, args []string) error {
	fs := newFlagSet("status", e)
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	return errNotImplemented
}
