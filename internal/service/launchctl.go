package service

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// LaunchctlPath is the macOS launchctl binary.
const LaunchctlPath = "/bin/launchctl"

// ExecLaunchctl runs launchctl(1) subcommands as child processes.
type ExecLaunchctl struct {
	path string
}

// NewExecLaunchctl returns a Launchctl that runs the binary at path,
// normally LaunchctlPath.
func NewExecLaunchctl(path string) *ExecLaunchctl {
	return &ExecLaunchctl{path: path}
}

// errRealLaunchctlInTests is returned by an ExecLaunchctl that would run the
// real launchctl inside a test binary: a test must never load, boot out or
// start the user's LaunchAgent.
var errRealLaunchctlInTests = errors.New("real launchctl refused in tests")

// refused reports errRealLaunchctlInTests when l runs the system launchctl
// and the process is a test binary: the system path in any spelling that
// filepath.Clean reduces to it, or the bare name "launchctl", which exec
// resolves through PATH.
func (l *ExecLaunchctl) refused() error {
	if !testing.Testing() {
		return nil
	}
	if filepath.Clean(l.path) == LaunchctlPath || l.path == "launchctl" {
		return errRealLaunchctlInTests
	}
	return nil
}

// exitServiceNotFound is the launchctl exit status for "service not found"
// (113, Could not find service). It comes from observed launchctl behavior,
// not from launchctl(1): print exited 113 after an uninstall in the
// E-lifecycle test (2026-11-25, macOS 26.5).
const exitServiceNotFound = 113

// Print runs "launchctl print <service-target>". launchctl(1) says the
// output "is NOT API", so only the exit status decides: 0 means loaded and
// exitServiceNotFound means not loaded (launchctl(1) EXIT STATUS: 0 on
// success, an error code otherwise). Any other exit status, a canceled
// context, or failing to start launchctl is an error with the output.
func (l *ExecLaunchctl) Print(ctx context.Context, serviceTarget string) (bool, error) {
	if err := l.refused(); err != nil {
		return false, err
	}
	var out bytes.Buffer
	cmd := exec.CommandContext(ctx, l.path, "print", serviceTarget)
	cmd.Stdout = &out
	cmd.Stderr = &out
	err := cmd.Run()
	if err == nil {
		return true, nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() == exitServiceNotFound && ctx.Err() == nil {
		return false, nil
	}
	return false, launchctlError("print", err, &out)
}

// Bootout runs "launchctl bootout <service-target>".
func (l *ExecLaunchctl) Bootout(ctx context.Context, serviceTarget string) error {
	return l.run(ctx, "bootout", serviceTarget)
}

// Bootstrap runs "launchctl bootstrap <domain-target> <path>". The Manager
// passes the domain target as "gui/<uid>/" with the trailing slash, the form
// launchctl(1) gives ("domain-target is gui/501/"). bootstrap accepted this
// form in the E-lifecycle test (2026-11-25, macOS 26.5).
func (l *ExecLaunchctl) Bootstrap(ctx context.Context, domainTarget, path string) error {
	return l.run(ctx, "bootstrap", domainTarget, path)
}

// Kickstart runs "launchctl kickstart <service-target>". Without -k it
// starts a service that is loaded but not running and leaves a running one
// alone (launchctl(1) kickstart).
func (l *ExecLaunchctl) Kickstart(ctx context.Context, serviceTarget string) error {
	return l.run(ctx, "kickstart", serviceTarget)
}

// run runs launchctl with args and puts its trimmed output in the error.
func (l *ExecLaunchctl) run(ctx context.Context, args ...string) error {
	if err := l.refused(); err != nil {
		return err
	}
	var out bytes.Buffer
	cmd := exec.CommandContext(ctx, l.path, args...)
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Run(); err != nil {
		return launchctlError(args[0], err, &out)
	}
	return nil
}

// launchctlError wraps err from the named subcommand with its trimmed
// output, if any.
func launchctlError(sub string, err error, out *bytes.Buffer) error {
	if msg := strings.TrimSpace(out.String()); msg != "" {
		return fmt.Errorf("launchctl %s: %w: %s", sub, err, msg)
	}
	return fmt.Errorf("launchctl %s: %w", sub, err)
}
