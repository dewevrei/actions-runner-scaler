package service

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/dewevrei/actions-runner-scaler/internal/config"
)

// Launchctl is the part of launchctl(1) the Manager uses. Targets follow
// launchctl(1): a domain target is "gui/<uid>/" and a service target is
// "gui/<uid>/<label>".
type Launchctl interface {
	// Print reports whether the service target is loaded. A launchctl
	// failure that is not a plain "not loaded" answer is an error.
	Print(ctx context.Context, serviceTarget string) (loaded bool, err error)
	// Bootout removes the service from its domain.
	Bootout(ctx context.Context, serviceTarget string) error
	// Bootstrap loads the plist at path into the domain.
	Bootstrap(ctx context.Context, domainTarget, path string) error
}

// Defaults for how the Manager waits for a bootout to take effect.
const (
	defaultPollInterval = time.Second
	// defaultStopMargin is added to ExitTimeOut to bound the wait: launchd
	// sends SIGKILL after ExitTimeOut, so the job should be gone by then.
	defaultStopMargin = 30 * time.Second
	// defaultPrintTimeout bounds each launchctl print, so a hung print
	// cannot outlast the stop deadline.
	defaultPrintTimeout = 10 * time.Second
)

// Manager installs and removes the LaunchAgent in one user's GUI domain.
type Manager struct {
	launchctl Launchctl
	uid       int
	home      string
	logger    *slog.Logger

	pollInterval time.Duration
	printTimeout time.Duration
	// stopLimit bounds the wait for a bootout, given the job's ExitTimeOut.
	stopLimit func(exitTimeOut time.Duration) time.Duration
}

// NewManager returns a Manager for the user with uid whose home directory is
// home. Files go under home/Library; launchctl is only reached through l.
func NewManager(l Launchctl, uid int, home string, logger *slog.Logger) *Manager {
	return &Manager{
		launchctl:    l,
		uid:          uid,
		home:         home,
		logger:       logger,
		pollInterval: defaultPollInterval,
		printTimeout: defaultPrintTimeout,
		stopLimit:    defaultStopLimit,
	}
}

// defaultStopLimit is exitTimeOut plus defaultStopMargin, saturating.
func defaultStopLimit(exitTimeOut time.Duration) time.Duration {
	limit := exitTimeOut + defaultStopMargin
	if limit < exitTimeOut {
		return exitTimeOut
	}
	return limit
}

// LogDir returns home/Library/Logs/actions-runner-scaler, where the
// LaunchAgent's stdout and stderr files go.
func LogDir(home string) string {
	return filepath.Join(home, "Library", "Logs", "actions-runner-scaler")
}

// PlistPath returns home/Library/LaunchAgents/<label>.plist, the per-user
// agent directory of launchd.plist(5) FILES.
func (m *Manager) PlistPath(label string) string {
	return filepath.Join(m.home, "Library", "LaunchAgents", label+".plist")
}

// domainTarget is "gui/<uid>/", the trailing-slash form of launchctl(1).
// Whether bootstrap accepts it is unverified until E-lifecycle (PLAN M7).
func (m *Manager) domainTarget() string {
	return fmt.Sprintf("gui/%d/", m.uid)
}

func (m *Manager) serviceTarget(label string) string {
	return m.domainTarget() + label
}

// Install writes j's plist and loads it. j.Home must be the Manager's home,
// so the plist, its HOME and its log paths agree. If the label is already loaded it
// is booted out first, and the file is written only after launchctl print
// no longer finds it, so the new definition never races the old process.
// The log directory is created with mode 0700 and the plist with 0644.
// ExitTimeOut is fixed at install time from j.DrainTimeout, so a changed
// drain-timeout takes effect only after another Install.
func (m *Manager) Install(ctx context.Context, j Job) error {
	if j.Home != m.home {
		return fmt.Errorf("job home %q differs from the manager's %q", j.Home, m.home)
	}
	data, err := Plist(j)
	if err != nil {
		return err
	}
	// Everything that can fail without launchctl goes before the stop, so
	// a failure leaves the running service alone.
	if err := os.MkdirAll(j.LogDir(), 0o700); err != nil {
		return fmt.Errorf("create log directory: %w", err)
	}
	path := m.PlistPath(j.Label)
	wait := max(m.installedExitTimeOut(path), seconds(j.ExitTimeOut()))
	stopped, err := m.stop(ctx, j.Label, wait)
	if err != nil {
		return err
	}
	if err := writeFile(path, data, 0o644); err != nil {
		if !stopped {
			return fmt.Errorf("write plist: %w", err)
		}
		if _, statErr := os.Stat(path); statErr == nil {
			return fmt.Errorf("service stopped; old plist kept and loads at next login: write plist: %w", err)
		}
		return fmt.Errorf("service stopped and not reinstalled: write plist: %w", err)
	}
	if err := m.launchctl.Bootstrap(ctx, m.domainTarget(), path); err != nil {
		return fmt.Errorf("plist written at %s but not loaded: bootstrap: %w", path, err)
	}
	m.logger.Info("service installed", "label", j.Label, "plist", path, "exitTimeOut", j.ExitTimeOut())
	return nil
}

// Uninstall boots the label out, waits until launchctl print no longer
// finds it, and then removes its plist. It reports whether anything was
// installed: the label loaded or its plist present. A missing plist is not
// an error.
func (m *Manager) Uninstall(ctx context.Context, label string) (installed bool, err error) {
	path := m.PlistPath(label)
	fallback := config.DefaultDrainTimeout + exitTimeOutMargin
	wait := max(m.installedExitTimeOut(path), fallback)
	stopped, err := m.stop(ctx, label, wait)
	if err != nil {
		return true, err
	}
	err = os.Remove(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		if !stopped {
			return false, nil
		}
	case err != nil:
		return true, fmt.Errorf("service stopped; plist remains and loads at next login: remove plist: %w", err)
	}
	m.logger.Info("service uninstalled", "label", label, "plist", path)
	return true, nil
}

// installedExitTimeOut returns the ExitTimeOut of the plist at path, or 0
// if there is none. It is the stop deadline of the job loaded from it.
func (m *Manager) installedExitTimeOut(path string) time.Duration {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	d, _ := exitTimeOutOf(data)
	return d
}

// stop boots the label out if it is loaded and polls launchctl print until
// it is gone, for at most stopLimit(exitTimeOut). It reports whether the
// label was loaded and has now been stopped.
func (m *Manager) stop(ctx context.Context, label string, exitTimeOut time.Duration) (stopped bool, err error) {
	target := m.serviceTarget(label)
	loaded, err := m.print(ctx, target)
	if err != nil {
		return false, fmt.Errorf("print %s: %w", target, err)
	}
	if !loaded {
		return false, nil
	}
	limit := m.stopLimit(exitTimeOut)
	m.logger.Info("stopping the loaded service", "target", target, "maxWait", limit.String())
	// The deadline and the bootout timeout both start before bootout,
	// because bootout itself may wait for the job to exit. launchctl may
	// report an error for a job that is stopping anyway, so a bootout error
	// only counts if the job is still loaded at the deadline.
	deadline := time.NewTimer(limit)
	defer deadline.Stop()
	bootoutCtx, cancel := context.WithTimeout(ctx, limit)
	bootoutErr := m.launchctl.Bootout(bootoutCtx, target)
	cancel()
	tick := time.NewTicker(m.pollInterval)
	defer tick.Stop()
	for {
		loaded, err := m.print(ctx, target)
		if err != nil {
			return false, errors.Join(bootoutErr, fmt.Errorf("print %s: %w", target, err))
		}
		if !loaded {
			return true, nil
		}
		select {
		case <-ctx.Done():
			return false, errors.Join(bootoutErr, ctx.Err())
		case <-deadline.C:
			return false, errors.Join(bootoutErr,
				fmt.Errorf("%s is still loaded %s after bootout", target, limit))
		case <-tick.C:
		}
	}
}

// print runs launchctl print under its own short timeout.
func (m *Manager) print(ctx context.Context, target string) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, m.printTimeout)
	defer cancel()
	return m.launchctl.Print(ctx, target)
}

// writeFile writes data to path with mode perm regardless of the umask,
// through a temporary file in the same directory and a rename, so launchd
// never reads a partial plist. Missing parent directories are created with
// mode 0755.
func writeFile(path string, data []byte, perm fs.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".tmp-"+filepath.Base(path)+"-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp) // fails harmlessly after the rename
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Chmod(perm); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
