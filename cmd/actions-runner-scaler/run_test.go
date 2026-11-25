package main

import (
	"bytes"
	"context"
	"crypto/rsa"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dewevrei/actions-runner-scaler/internal/config"
	"github.com/dewevrei/actions-runner-scaler/internal/daemon"
	"github.com/dewevrei/actions-runner-scaler/internal/docker"
	"github.com/dewevrei/actions-runner-scaler/internal/gate"
	"github.com/dewevrei/actions-runner-scaler/internal/scaler"
	"github.com/dewevrei/actions-runner-scaler/internal/status"
)

// pingDocker answers Ping only; the tests below never get further.
type pingDocker struct{ daemon.Docker }

func (pingDocker) Ping(ctx context.Context) error { return ctx.Err() }

type nopServer struct{ stop chan struct{} }

func (s nopServer) Serve() error                   { <-s.stop; return nil }
func (s nopServer) Shutdown(context.Context) error { close(s.stop); return nil }

// fakeDaemonConfig is a daemon.Config on fakes that never reach GitHub or
// Docker.
func fakeDaemonConfig(e env, lockErr error) daemon.Config {
	return daemon.Config{
		Clock:   scaler.SystemClock(),
		Signals: make(chan os.Signal),
		Logger:  e.logger,
		Secrets: e.secrets,
		Lock: func() (func(), error) {
			if lockErr != nil {
				return nil, lockErr
			}
			return func() {}, nil
		},
		ListenStatus: func(status.Reporter) (daemon.StatusServer, error) {
			return nopServer{stop: make(chan struct{})}, nil
		},
		LoadConfig: func() (*config.Config, *rsa.PrivateKey, error) {
			return nil, nil, errors.New("no config")
		},
		Docker:        pingDocker{},
		CheckExposure: func(context.Context) docker.Exposure { return docker.NotExposed },
		NewGitHub: func(*config.Config, *rsa.PrivateKey) (daemon.GitHub, error) {
			return nil, errors.New("not used")
		},
		NewJobServer: func(string, gate.Authenticator) (daemon.JobServer, error) {
			return nil, errors.New("not used")
		},
		Owner: "host",
	}
}

// A stop by signal (here the ended context, which the daemon treats as
// one) exits 0 (R84, R57).
func TestRunDaemonSignalStopExits0(t *testing.T) {
	var stdout, stderr bytes.Buffer
	e := newEnv(&stdout, &stderr)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := runDaemon(ctx, fakeDaemonConfig(e, nil))
	if code := exitCode(err); code != 0 {
		t.Fatalf("exit = %d (%v), want 0", code, err)
	}
}

// A held lock exits 1 (R78, R85) and is logged once, by the daemon.
func TestRunDaemonLockHeldExits1(t *testing.T) {
	var stdout, stderr bytes.Buffer
	e := newEnv(&stdout, &stderr)
	cmds := map[string]command{"run": {run: func(ctx context.Context, e env, _ []string) error {
		return runDaemon(ctx, fakeDaemonConfig(e, daemon.ErrLocked))
	}}}
	if code := dispatch(context.Background(), e, []string{"run"}, cmds); code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	out := stderr.String()
	if !strings.Contains(out, "R78") {
		t.Errorf("lock failure not logged: %s", out)
	}
	if strings.Contains(out, `"msg":"command failed"`) {
		t.Errorf("logged twice: %s", out)
	}
}

// The real wiring has every dependency the daemon needs. Nothing runs.
func TestDaemonConfigComplete(t *testing.T) {
	var stdout, stderr bytes.Buffer
	cfg, err := daemonConfig(newEnv(&stdout, &stderr), "/nonexistent/config.toml", make(chan os.Signal))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := daemon.New(cfg); err != nil {
		t.Fatalf("daemon.New = %v", err)
	}
	if _, _, err := cfg.LoadConfig(); err == nil {
		t.Fatal("LoadConfig of a missing file succeeded")
	}
}

func TestLoadConfigInvalid(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte("bogus = 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, _, err := loadConfig(path)
	if err == nil || !strings.Contains(err.Error(), "bogus") {
		t.Fatalf("loadConfig = %v, want the unknown key reported", err)
	}
}

func TestRunRejectsArguments(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := dispatch(context.Background(), newEnv(&stdout, &stderr), []string{"run", "extra"}, commands()); code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "usage: run") {
		t.Errorf("stderr = %q", stderr.String())
	}
}
