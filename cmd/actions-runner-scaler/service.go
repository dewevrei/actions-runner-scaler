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
	"time"

	"github.com/dewevrei/actions-runner-scaler/internal/config"
	"github.com/dewevrei/actions-runner-scaler/internal/service"
	"github.com/dewevrei/actions-runner-scaler/internal/status"
)

// serviceHost is what "service" needs from the machine, injected so tests
// use a temporary home, a fake launchctl and a fake daemon.
type serviceHost struct {
	uid        int
	home       string
	executable string
	launchctl  service.Launchctl
	daemon     service.Daemon
}

func serviceCmd(ctx context.Context, e env, args []string) error {
	fs := newFlagSet("service", e)
	configPath := fs.String("config", defaultConfigPath(), "path to the TOML config file: the plist's config (install) and the drain-timeout to wait for (install, stop, uninstall)")
	action, err := parseServiceArgs(fs, args)
	if err != nil {
		return err
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("find the scaler binary: %w", err)
	}
	socket, err := status.DefaultSocketPath()
	if err != nil {
		return err
	}
	host := serviceHost{
		uid:        os.Getuid(),
		home:       home,
		executable: exe,
		launchctl:  service.NewExecLaunchctl(service.LaunchctlPath),
		daemon:     service.NewSocketDaemon(socket),
	}
	return runService(ctx, e, host, action, *configPath)
}

const serviceUsage = "usage: service install|stop|uninstall [-config path]"

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
	if fs.NArg() != 0 || (action != "install" && action != "uninstall" && action != "stop") {
		return "", badUsage(serviceUsage)
	}
	return action, nil
}

// runService installs, stops or uninstalls the LaunchAgent. install loads
// and validates the configuration first, so a broken file fails here
// instead of in the service. All three wait for a running scaler's drain
// for its drain-timeout plus a margin (service.StopLimit).
func runService(ctx context.Context, e env, h serviceHost, action, configPath string) error {
	m := service.NewManager(h.launchctl, h.daemon, h.uid, h.home, e.stdout, e.logger)
	switch action {
	case "stop":
		running, err := m.Stop(ctx, drainTimeout(e, configPath))
		if err != nil {
			return err
		}
		if !running {
			fmt.Fprintln(e.stdout, "the scaler is not running; nothing to stop")
			return nil
		}
		fmt.Fprintf(e.stdout, "stopped; if installed, %s stays loaded and starts the scaler again at the next login or with: launchctl kickstart gui/%d/%s\n",
			service.Label, h.uid, service.Label)
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
