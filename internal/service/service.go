package service

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"time"
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

// Defaults for how the Manager waits.
const (
	defaultPollInterval = time.Second
	// defaultBootoutLimit bounds the wait for a bootout to take effect:
	// launchd sends SIGKILL ExitTimeOut after SIGTERM, so the job should be
	// gone well before ExitTimeOut plus 30 seconds. The Manager stops the
	// scaler itself first, so a bootout normally meets a job with no
	// process and returns at once.
	defaultBootoutLimit = ExitTimeOut + 30*time.Second
	// defaultPrintTimeout bounds each launchctl print, so a hung print
	// cannot outlast the bootout limit.
	defaultPrintTimeout = 10 * time.Second
	// defaultProgressInterval is how often Stop prints that it is still
	// waiting.
	defaultProgressInterval = 10 * time.Second
	// finalizeAllowance covers the scaler's finalization after a drain,
	// which takes at most 55 seconds (SPEC Design 운영).
	finalizeAllowance = 60 * time.Second
	// stopMargin is added to drain-timeout + finalizeAllowance for the
	// stop deadline.
	stopMargin = 30 * time.Second
)

// Manager installs, stops and removes the LaunchAgent in one user's GUI
// domain.
type Manager struct {
	launchctl Launchctl
	daemon    Daemon
	uid       int
	home      string
	out       io.Writer
	logger    *slog.Logger

	pollInterval     time.Duration
	printTimeout     time.Duration
	progressInterval time.Duration
	bootoutLimit     time.Duration
	// stopLimit bounds the wait for the scaler to exit after SIGTERM,
	// given its drain-timeout.
	stopLimit func(drainTimeout time.Duration) time.Duration
}

// NewManager returns a Manager for the user with uid whose home directory is
// home. Files go under home/Library; launchctl is only reached through l,
// and the running scaler only through d. Progress lines for the user go to
// out.
func NewManager(l Launchctl, d Daemon, uid int, home string, out io.Writer, logger *slog.Logger) *Manager {
	return &Manager{
		launchctl:        l,
		daemon:           d,
		uid:              uid,
		home:             home,
		out:              out,
		logger:           logger,
		pollInterval:     defaultPollInterval,
		printTimeout:     defaultPrintTimeout,
		progressInterval: defaultProgressInterval,
		bootoutLimit:     defaultBootoutLimit,
		stopLimit:        StopLimit,
	}
}

// StopLimit is how long Stop waits for the scaler to exit after SIGTERM:
// drainTimeout, plus 60 seconds for the finalization after the drain, plus
// a 30-second margin. It saturates instead of overflowing.
func StopLimit(drainTimeout time.Duration) time.Duration {
	limit := drainTimeout + finalizeAllowance + stopMargin
	if limit < drainTimeout {
		return time.Duration(1<<63 - 1)
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
// bootstrap accepts it (E-lifecycle, 2026-11-25).
func (m *Manager) domainTarget() string {
	return fmt.Sprintf("gui/%d/", m.uid)
}

func (m *Manager) serviceTarget(label string) string {
	return m.domainTarget() + label
}

// Install writes j's plist and loads it. j.Home must be the Manager's home,
// so the plist, its HOME and its log paths agree. If the label is already
// loaded, the running scaler is first stopped gracefully (Stop, waiting up
// to StopLimit(drainTimeout)) and the label booted out, and the file is
// written only after launchctl print no longer finds it, so the new
// definition never races the old process. The log directory is created
// with mode 0700 and the plist with 0644.
func (m *Manager) Install(ctx context.Context, j Job, drainTimeout time.Duration) error {
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
	stopped, err := m.unload(ctx, j.Label, drainTimeout)
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
	m.logger.Info("service installed", "label", j.Label, "plist", path)
	return nil
}

// Uninstall stops the running scaler gracefully if the label is loaded
// (Stop), boots the label out, waits until launchctl print no longer finds
// it, and then removes its plist. It reports whether anything was
// installed: the label loaded or its plist present. A missing plist is not
// an error. If the scaler does not exit within StopLimit(drainTimeout),
// nothing is booted out or removed.
func (m *Manager) Uninstall(ctx context.Context, label string, drainTimeout time.Duration) (installed bool, err error) {
	path := m.PlistPath(label)
	stopped, err := m.unload(ctx, label, drainTimeout)
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

// InstalledConfigPath returns the config path in the installed plist of
// label, the argument after "-config" in its ProgramArguments, which is
// the config the agent's scaler runs with.
func (m *Manager) InstalledConfigPath(label string) (string, error) {
	path := m.PlistPath(label)
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	p, ok := configPathOf(data)
	if !ok {
		return "", fmt.Errorf("%s has no -config argument", path)
	}
	return p, nil
}

// unload stops the running scaler and boots the label out if the label is
// loaded. It reports whether the label was loaded and is now gone.
//
// The scaler is signaled directly and waited for before the bootout,
// because launchd kills a job ExitTimeOut (at most 60 seconds) after the
// bootout's SIGTERM, which cuts a drain short. Once the scaler has exited
// 0, launchd does not restart it (KeepAlive SuccessfulExit false) and the
// bootout has no process to wait for.
func (m *Manager) unload(ctx context.Context, label string, drainTimeout time.Duration) (bool, error) {
	target := m.serviceTarget(label)
	loaded, err := m.print(ctx, target)
	if err != nil {
		return false, fmt.Errorf("print %s: %w", target, err)
	}
	if !loaded {
		return false, nil
	}
	ran, err := m.Stop(ctx, drainTimeout)
	if err != nil {
		err = fmt.Errorf("%w; %s is still loaded and its plist was not changed", err, target)
		if sr := (*StillRunningError)(nil); errors.As(err, &sr) && sr.Signaled {
			err = fmt.Errorf("%w; after the drain the scaler stays stopped until the next login, launchctl kickstart %s, or running this command again", err, target)
		}
		return false, err
	}
	if err := m.bootout(ctx, target); err != nil {
		if ran {
			return false, fmt.Errorf("the scaler is stopped, but %s is still loaded with the old plist and starts it again at the next login or with launchctl kickstart: %w", target, err)
		}
		return false, err
	}
	return true, nil
}

// bootout boots target out and polls launchctl print until it is gone, for
// at most bootoutLimit.
func (m *Manager) bootout(ctx context.Context, target string) error {
	limit := m.bootoutLimit
	m.logger.Info("booting out the loaded service", "target", target, "maxWait", limit.String())
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
			return errors.Join(bootoutErr, fmt.Errorf("print %s: %w", target, err))
		}
		if !loaded {
			return nil
		}
		select {
		case <-ctx.Done():
			return errors.Join(bootoutErr, ctx.Err())
		case <-deadline.C:
			return errors.Join(bootoutErr,
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
