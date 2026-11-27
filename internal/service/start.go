package service

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"time"
)

// Start starts the scaler of the installed LaunchAgent label, the way the
// agent's own launch would: "launchctl kickstart gui/<uid>/<label>" when the
// label is loaded, which is the state "service stop" leaves it in. A label
// whose plist exists but that is not loaded (after a manual bootout) is
// bootstrapped instead, and RunAtLoad starts it. It reports whether it
// started anything; a scaler already running on the status socket is left
// alone. Without the plist it fails and says to run "service install".
//
// After the kickstart or bootstrap, Start waits up to startLimit for the
// scaler to answer on its status socket, and if it does not, fails with an
// error that names the launchctl print command to look at.
func (m *Manager) Start(ctx context.Context, label string) (started bool, err error) {
	path := m.PlistPath(label)
	if _, err := os.Stat(path); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return false, fmt.Errorf("%s is not installed (no plist at %s); run service install first", label, path)
		}
		return false, fmt.Errorf("check the plist: %w", err)
	}
	_, _, running, err := m.daemon.Find(ctx)
	if err != nil {
		return false, fmt.Errorf("query the scaler's status socket: %w", err)
	}
	if running {
		return false, nil
	}
	target := m.serviceTarget(label)
	loaded, err := m.print(ctx, target)
	if err != nil {
		return false, fmt.Errorf("print %s: %w", target, err)
	}
	if !loaded {
		if err := m.launchctl.Bootstrap(ctx, m.domainTarget(), path); err != nil {
			return false, fmt.Errorf("bootstrap %s: %w", path, err)
		}
		m.logger.Info("service loaded", "label", label, "plist", path)
		return true, m.awaitRunning(ctx, target)
	}
	if err := m.launchctl.Kickstart(ctx, target); err != nil {
		return false, fmt.Errorf("kickstart %s: %w", target, err)
	}
	m.logger.Info("service kickstarted", "label", label)
	return true, m.awaitRunning(ctx, target)
}

// awaitRunning polls the status socket every pollInterval until a scaler
// answers, for at most startLimit.
func (m *Manager) awaitRunning(ctx context.Context, target string) error {
	deadline := time.NewTimer(m.startLimit)
	defer deadline.Stop()
	tick := time.NewTicker(m.pollInterval)
	defer tick.Stop()
	for {
		_, _, running, err := m.daemon.Find(ctx)
		if err != nil {
			return fmt.Errorf("query the scaler's status socket: %w", err)
		}
		if running {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return fmt.Errorf("started %s, but the scaler did not answer on its status socket within %s; check launchctl print %s and the logs",
				target, m.startLimit, target)
		case <-tick.C:
		}
	}
}
