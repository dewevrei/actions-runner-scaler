package main

import "context"

func serviceCmd(_ context.Context, e env, args []string) error {
	fs := newFlagSet("service", e)
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if fs.NArg() != 1 || (fs.Arg(0) != "install" && fs.Arg(0) != "uninstall") {
		return badUsage("usage: service install|uninstall")
	}
	return errNotImplemented
}
