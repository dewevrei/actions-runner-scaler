package service

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
)

// Start starts the scaler of the installed LaunchAgent label, the way the
// agent's own launch would: "launchctl kickstart gui/<uid>/<label>" when the
// label is loaded, which is the state "service stop" leaves it in. A label
// whose plist exists but that is not loaded (after a manual bootout) is
// bootstrapped instead, and RunAtLoad starts it. It reports whether it
// started anything; a scaler already running on the status socket is left
// alone. Without the plist it fails and says to run "service install".
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
		m.logger.Info("service loaded and started", "label", label, "plist", path)
		return true, nil
	}
	if err := m.launchctl.Kickstart(ctx, target); err != nil {
		return false, fmt.Errorf("kickstart %s: %w", target, err)
	}
	m.logger.Info("service started", "label", label)
	return true, nil
}
