package main

import (
	"context"
	"os"
	"path/filepath"
)

func runCmd(_ context.Context, e env, args []string) error {
	fs := newFlagSet("run", e)
	fs.String("config", defaultConfigPath(), "path to the TOML config file")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	return errNotImplemented
}

// defaultConfigPath is ~/.config/actions-runner-scaler/config.toml, next to
// the App key the user places there.
func defaultConfigPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return "config.toml"
	}
	return filepath.Join(home, ".config", "actions-runner-scaler", "config.toml")
}
