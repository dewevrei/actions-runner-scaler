package main

import (
	"context"
	"fmt"
	"testing"

	"github.com/dewevrei/actions-runner-scaler/internal/status"
)

// statusCmd prints the running scaler's state from its status socket. It
// exits 0 while starting, running or draining and 1 in error-wait or when
// the socket does not answer (R26, R27, R45, R56, R58).
func statusCmd(h hosts) func(ctx context.Context, e env, args []string) error {
	return func(ctx context.Context, e env, args []string) error {
		return runStatusCmd(ctx, e, args, h.statusSocket)
	}
}

// realStatusSocket is the default status socket of this machine. A test
// binary gets errRealHostInTests instead.
func realStatusSocket() (string, error) {
	if testing.Testing() {
		return "", errRealHostInTests
	}
	return status.DefaultSocketPath()
}

func runStatusCmd(ctx context.Context, e env, args []string, socket func() (string, error)) error {
	fs := newFlagSet("status", e)
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return badUsage("usage: status")
	}
	path, err := socket()
	if err != nil {
		fmt.Fprintln(e.stdout, status.NotRunning)
		return err
	}
	return showStatus(ctx, e, path)
}

// showStatus prints the state served at path. When Show's output is the
// whole answer (not running, error-wait, unknown state) the error only sets
// exit code 1; any other failure, such as EACCES, a timeout or a malformed
// report, is returned as is for dispatch to log.
func showStatus(ctx context.Context, e env, path string) error {
	err := status.Show(ctx, e.stdout, path)
	if err != nil && status.Reported(err) {
		return reportedError{err: err}
	}
	return err
}
