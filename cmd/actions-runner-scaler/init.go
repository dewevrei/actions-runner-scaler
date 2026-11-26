package main

import (
	"context"
	"errors"
)

// initCmd writes a new configuration file (0003 SPEC R282~R289). Stub:
// 0003 PLAN M34~M38 replace this file.
func initCmd(_ context.Context, e env, args []string) error {
	fs := newFlagSet("init", e)
	fs.String("config", defaultConfigPath(), "path to the TOML config file")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	return errors.New("not implemented yet")
}
