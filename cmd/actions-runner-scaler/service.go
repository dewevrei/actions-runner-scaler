package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/dewevrei/actions-runner-scaler/internal/config"
	"github.com/dewevrei/actions-runner-scaler/internal/daemon"
	"github.com/dewevrei/actions-runner-scaler/internal/service"
	"github.com/dewevrei/actions-runner-scaler/internal/status"
)

// serviceHost is what "service" needs from the machine, injected so tests
// use a temporary home, a fake launchctl, a fake daemon and their own lock.
type serviceHost struct {
	uid        int
	home       string
	executable string
	launchctl  service.Launchctl
	daemon     service.Daemon
	// defaultConfig is the config path when -config is not given.
	defaultConfig string
	// lock takes the lock that lets one service command run at a time. It
	// returns daemon.ErrLocked when another holds it.
	lock func() (unlock func(), err error)
}

// stopLockName is the file next to the status socket that service commands
// flock, so two of them never signal the scaler twice (a second SIGTERM cuts
// a drain, R21).
const stopLockName = "stop.lock"

// errRealHostInTests is what the real host factories return in a test
// binary: a test must never reach the user's launchd, status socket, lock or
// scaler. Tests pass their own hosts to commands.
var errRealHostInTests = errors.New("real host refused in tests")

// serviceCmd returns the service command over the host h.service builds.
func serviceCmd(h hosts) func(ctx context.Context, e env, args []string) error {
	return func(ctx context.Context, e env, args []string) error {
		return runServiceCmd(ctx, e, args, h.service)
	}
}

func runServiceCmd(ctx context.Context, e env, args []string, newHost func() (serviceHost, error)) error {
	fs := newFlagSet("service", e)
	configPath := fs.String("config", "", "path to the TOML config file: the plist's config (install) and the drain-timeout to wait for (all). Default: for install "+defaultConfigPath()+"; for stop, restart and uninstall the installed plist's config, else that path")
	action, err := parseServiceArgs(fs, args)
	if err != nil {
		return err
	}
	host, err := newHost()
	if err != nil {
		return err
	}
	return runService(ctx, e, host, action, *configPath)
}

// realServiceHost is the service host of this machine: the user's home, the
// system launchctl, the default status socket and its lock. A test binary
// gets errRealHostInTests instead.
func realServiceHost() (serviceHost, error) {
	if testing.Testing() {
		return serviceHost{}, errRealHostInTests
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return serviceHost{}, err
	}
	exe, err := os.Executable()
	if err != nil {
		return serviceHost{}, fmt.Errorf("find the scaler binary: %w", err)
	}
	socket, err := status.DefaultSocketPath()
	if err != nil {
		return serviceHost{}, err
	}
	lockPath := filepath.Join(filepath.Dir(socket), stopLockName)
	return serviceHost{
		uid:           os.Getuid(),
		home:          home,
		executable:    exe,
		launchctl:     service.NewExecLaunchctl(service.LaunchctlPath),
		daemon:        service.NewSocketDaemon(socket),
		defaultConfig: defaultConfigPath(),
		lock:          func() (func(), error) { return daemon.LockFile(lockPath) },
	}, nil
}

const serviceUsage = "usage: service install|start|stop|restart|uninstall [-config path]"

// parseServiceArgs parses flags before and after the action, so both
// "service install -config x" and "service -config x install" work, and
// returns the action.
func parseServiceArgs(fs *flag.FlagSet, args []string) (string, error) {
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), serviceUsage)
		fs.PrintDefaults()
	}
	if err := parseFlags(fs, args); err != nil {
		return "", err
	}
	if fs.NArg() == 0 {
		return "", badUsage(serviceUsage)
	}
	action := fs.Arg(0)
	if err := parseFlags(fs, fs.Args()[1:]); err != nil {
		return "", err
	}
	switch action {
	case "install", "start", "stop", "restart", "uninstall":
	default:
		return "", badUsage(serviceUsage)
	}
	if fs.NArg() != 0 {
		return "", badUsage(serviceUsage)
	}
	return action, nil
}

// runService installs, starts, stops, restarts or uninstalls the
// LaunchAgent. install loads and validates the configuration first, so a
// broken file fails here instead of in the service. install, stop, restart
// and uninstall wait for a running scaler's drain for its drain-timeout plus
// a margin (service.StopLimit), and only one command runs at a time. start
// needs no config and fails when the agent is not installed. restart is
// stop, then start. An empty configPath means -config was not given.
func runService(ctx context.Context, e env, h serviceHost, action, configPath string) error {
	unlock, err := h.lock()
	if errors.Is(err, daemon.ErrLocked) {
		fmt.Fprintln(e.stdout, "another service command is running; run this one after it ends")
		return reportedError{err: err}
	}
	if err != nil {
		return fmt.Errorf("service lock: %w", err)
	}
	defer unlock()

	m := service.NewManager(h.launchctl, h.daemon, h.uid, h.home, e.stdout, e.logger)
	if action != "install" && action != "start" && configPath == "" {
		configPath = h.defaultConfig
		if p, err := m.InstalledConfigPath(service.Label); err == nil {
			configPath = p
		} else {
			e.logger.Info("no config in the installed plist; using the default config path", "config", configPath, "err", err)
		}
	}
	switch action {
	case "start":
		return startService(ctx, e, m)
	case "restart":
		if _, err := m.Stop(ctx, drainTimeout(e, configPath)); err != nil {
			return err
		}
		return startService(ctx, e, m)
	case "stop":
		running, err := m.Stop(ctx, drainTimeout(e, configPath))
		if err != nil {
			return err
		}
		if !running {
			fmt.Fprintln(e.stdout, "the scaler is not running; nothing to stop")
			return nil
		}
		fmt.Fprintf(e.stdout, "stopped; if installed, %s stays loaded and starts the scaler again at the next login or with: service start\n",
			service.Label)
		return nil
	case "uninstall":
		installed, err := m.Uninstall(ctx, service.Label, drainTimeout(e, configPath))
		if err != nil {
			return err
		}
		if !installed {
			fmt.Fprintf(e.stdout, "%s is not installed; nothing to uninstall\n", service.Label)
			return nil
		}
		fmt.Fprintf(e.stdout, "uninstalled %s\n", service.Label)
		return nil
	}

	program, err := programPath(h.executable)
	if err != nil {
		return err
	}
	if configPath == "" {
		configPath = h.defaultConfig
	}
	absConfig, err := filepath.Abs(configPath)
	if err != nil {
		return err
	}
	cfg, err := config.Load(absConfig)
	if err != nil {
		return fmt.Errorf("config: %w", err)
	}
	job := service.Job{
		Label:      service.Label,
		Program:    program,
		ConfigPath: absConfig,
		Home:       h.home,
	}
	if err := m.Install(ctx, job, cfg.Shutdown.DrainTimeout); err != nil {
		return err
	}
	fmt.Fprintf(e.stdout, "installed %s (%s)\nlogs: %s\n", service.Label, m.PlistPath(service.Label), job.LogDir())
	return nil
}

// startService starts the installed agent's scaler and reports what it did.
func startService(ctx context.Context, e env, m *service.Manager) error {
	started, err := m.Start(ctx, service.Label)
	if err != nil {
		return err
	}
	if !started {
		fmt.Fprintln(e.stdout, "the scaler is already running; nothing to start")
		return nil
	}
	fmt.Fprintf(e.stdout, "started %s; see its state with the status command\n", service.Label)
	return nil
}

// drainTimeout returns shutdown.drain-timeout from the config at path, for
// stop and uninstall to wait for. They must work even when the config is
// broken (a scaler in error-wait over a config error), so a config that
// does not load is logged and the default is used.
func drainTimeout(e env, path string) time.Duration {
	cfg, err := config.Load(path)
	if err != nil {
		e.logger.Warn("config not loaded; waiting for the default drain-timeout",
			"config", path, "drainTimeout", config.DefaultDrainTimeout.String(), "err", err)
		return config.DefaultDrainTimeout
	}
	return cfg.Shutdown.DrainTimeout
}

// goBuildDir matches the temporary work directory of the go command,
// go-build<digits>.
var goBuildDir = regexp.MustCompile(`^go-build[0-9]+$`)

// programPath returns the absolute path of the running binary for
// ProgramArguments. A binary from "go run" lives in a temporary go-build
// directory that is deleted on exit, so it is refused.
func programPath(exe string) (string, error) {
	abs, err := filepath.Abs(exe)
	if err != nil {
		return "", err
	}
	for _, elem := range strings.Split(abs, string(filepath.Separator)) {
		if goBuildDir.MatchString(elem) {
			return "", errors.New("the binary is a temporary go run build; build it with go build and run service install from that binary")
		}
	}
	return abs, nil
}
