package main

import "context"

func deleteScaleSetCmd(_ context.Context, e env, args []string) error {
	fs := newFlagSet("delete-scaleset", e)
	fs.String("config", defaultConfigPath(), "path to the TOML config file")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return badUsage("usage: delete-scaleset [-config path] <pool name>")
	}
	return errNotImplemented
}
