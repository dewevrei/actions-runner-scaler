package main

import (
	"context"
	"errors"
)

// addRepoCmd adds a [[targets]] entry to the configuration file (0003 SPEC
// R290). Stub: a later milestone replaces this file.
func addRepoCmd(_ context.Context, e env, args []string) error {
	return repoEditStub("add-repo", e, args)
}

// removeRepoCmd removes a [[targets]] entry from the configuration file
// (0003 SPEC R291). Stub: a later milestone replaces this file.
func removeRepoCmd(_ context.Context, e env, args []string) error {
	return repoEditStub("remove-repo", e, args)
}

func repoEditStub(name string, e env, args []string) error {
	fs := newFlagSet(name, e)
	fs.String("config", defaultConfigPath(), "path to the TOML config file")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	return errors.New("not implemented yet")
}
