package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/actions/scaleset"

	"github.com/dewevrei/actions-runner-scaler/internal/config"
	"github.com/dewevrei/actions-runner-scaler/internal/daemon"
	"github.com/dewevrei/actions-runner-scaler/internal/docker"
	"github.com/dewevrei/actions-runner-scaler/internal/gate"
	"github.com/dewevrei/actions-runner-scaler/internal/logging"
	"github.com/dewevrei/actions-runner-scaler/internal/scaler"
	"github.com/dewevrei/actions-runner-scaler/internal/status"
)

// systemName is the System of the scale set's system information.
const systemName = "actions-runner-scaler"

func runCmd(ctx context.Context, e env, args []string) error {
	fs := newFlagSet("run", e)
	configPath := fs.String("config", defaultConfigPath(), "path to the TOML config file")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return badUsage("usage: run [-config path]")
	}

	sigs := make(chan os.Signal, 2)
	signal.Notify(sigs, syscall.SIGTERM, syscall.SIGINT)
	defer signal.Stop(sigs)

	cfg, err := daemonConfig(e, *configPath, sigs)
	if err != nil {
		return err
	}
	return runDaemon(ctx, cfg)
}

// runDaemon runs the daemon and maps its end to the command's result: nil
// for every signal stop, which exits 0 (R84), and an error otherwise, which
// exits 1 (R85). The daemon logs its own failures, so they are returned as
// reportedError and not logged again.
func runDaemon(ctx context.Context, cfg daemon.Config) error {
	d, err := daemon.New(cfg)
	if err != nil {
		return err
	}
	if err := d.Run(ctx); err != nil {
		return reportedError{err: err}
	}
	return nil
}

// daemonConfig wires the real dependencies: one logging.Secrets (the env's,
// which its logger redacts with), the system clock, the lock and status
// socket under ~/.local/state/actions-runner-scaler, the default Docker
// socket, and the GitHub clients of each target's credential.
func daemonConfig(e env, configPath string, sigs <-chan os.Signal) (daemon.Config, error) {
	clock := scaler.SystemClock()
	lockPath, err := daemon.DefaultLockPath()
	if err != nil {
		return daemon.Config{}, err
	}
	socketPath, err := status.DefaultSocketPath()
	if err != nil {
		return daemon.Config{}, err
	}
	engine, err := docker.NewEngine(docker.DefaultHost)
	if err != nil {
		return daemon.Config{}, fmt.Errorf("docker client: %w", err)
	}
	owner, err := os.Hostname()
	if err != nil || owner == "" {
		owner = systemName
	}
	info := scaleset.SystemInfo{System: systemName, Version: version, Subsystem: "listener"}
	log := e.logger
	return daemon.Config{
		Clock:   clock,
		Signals: sigs,
		Logger:  log,
		Secrets: e.secrets,
		Lock:    func() (func(), error) { return daemon.LockFile(lockPath) },
		ListenStatus: func(rep status.Reporter) (daemon.StatusServer, error) {
			return status.Listen(socketPath, rep, log)
		},
		LoadConfig: func() (*config.Config, config.Credentials, error) { return loadConfig(configPath, e.secrets) },
		Docker:     docker.New(engine),
		CheckExposure: func(ctx context.Context) docker.Exposure {
			return docker.CheckExposure(ctx, nil)
		},
		NewGitHub: func(cfg *config.Config, creds config.Credentials) (daemon.GitHub, error) {
			return daemon.NewGitHub(daemon.GitHubConfig{
				Config:      cfg,
				Credentials: creds,
				Clock:       clock,
				Secrets:     e.secrets,
				Logger:      log,
				SystemInfo:  info,
			})
		},
		NewJobServer: func(addr string, auth gate.Authenticator) (daemon.JobServer, error) {
			return gate.NewServer(addr, auth, clock, log.With("component", "jobcheck"))
		},
		Owner:      owner,
		SystemInfo: info,
	}, nil
}

// loadConfig reads and validates the configuration (R28, R30, 0002 R105~R107,
// R233, 0003 SPEC R271~R276) and reads the App private key and the tokens
// that validation checked. It registers every token in secrets before it
// returns, so the logger redacts them from the first line that could hold
// one (0003 SPEC R277).
func loadConfig(path string, secrets *logging.Secrets) (*config.Config, config.Credentials, error) {
	cfg, err := config.Load(path)
	if err != nil {
		return nil, config.Credentials{}, err
	}
	creds, err := config.LoadCredentials(cfg)
	if err != nil {
		return nil, config.Credentials{}, err
	}
	secrets.Register(creds.DefaultToken)
	for _, token := range creds.TargetTokens {
		secrets.Register(token)
	}
	return cfg, creds, nil
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
