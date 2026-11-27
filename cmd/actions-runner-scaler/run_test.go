package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dewevrei/actions-runner-scaler/internal/config"
	"github.com/dewevrei/actions-runner-scaler/internal/daemon"
	"github.com/dewevrei/actions-runner-scaler/internal/docker"
	"github.com/dewevrei/actions-runner-scaler/internal/gate"
	"github.com/dewevrei/actions-runner-scaler/internal/github"
	"github.com/dewevrei/actions-runner-scaler/internal/logging"
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
		LoadConfig: func() (*config.Config, config.Credentials, error) {
			return nil, config.Credentials{}, errors.New("no config")
		},
		NewDocker:     func(string) (daemon.Docker, error) { return pingDocker{}, nil },
		CheckExposure: func(context.Context) docker.Exposure { return docker.NotExposed },
		NewGitHub: func(*config.Config, config.Credentials) (daemon.GitHub, error) {
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
	_, _, err := loadConfig(path, logging.NewSecrets())
	if err == nil || !strings.Contains(err.Error(), "bogus") {
		t.Fatalf("loadConfig = %v, want the unknown key reported", err)
	}
}

func TestRunRejectsArguments(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := dispatch(context.Background(), newEnv(&stdout, &stderr), []string{"run", "extra"}, commands(refusingHosts())); code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "usage: run") {
		t.Errorf("stderr = %q", stderr.String())
	}
}

// TestRunWiresTargets proves 0002 SPEC R105: the real wiring loads a
// configuration with several [[targets]] and makes the GitHub side from it
// without an installation ID, which is looked up for each target later.
func TestRunWiresTargets(t *testing.T) {
	dir := t.TempDir()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	keyPath := filepath.Join(dir, "app.pem")
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	if err := os.WriteFile(keyPath, keyPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(dir, "config.toml")
	content := `[github]
app-client-id = "fake-client-id"
app-private-key-file = "` + keyPath + `"

[runner]
image = "sha256:` + strings.Repeat("a", 64) + `"

[buildkit]
image = "moby/buildkit:v0.0.0-rootless@sha256:` + strings.Repeat("b", 64) + `"

[[targets]]
url = "https://github.com/acme/widget"

[[targets]]
url = "https://github.com/other/gadget"
max-runners = 3
`
	if err := os.WriteFile(cfgPath, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	e := newEnv(&stdout, &stderr)
	cfg, err := daemonConfig(e, cfgPath, make(chan os.Signal))
	if err != nil {
		t.Fatal(err)
	}
	loaded, creds, err := cfg.LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if len(loaded.Targets) != 2 || loaded.Targets[1].MaxRunners != 3 {
		t.Fatalf("targets = %+v, want both with the second's max-runners 3", loaded.Targets)
	}
	if _, err := cfg.NewGitHub(loaded, creds); err != nil {
		t.Fatalf("NewGitHub = %v", err)
	}
}

// TestLoadConfigRegistersTokens proves 0003 SPEC R277: once the run
// wiring's LoadConfig returned, a log line holding the default token or a
// target's own token prints <redacted> in their place. The tokens have no
// GitHub prefix, so only their registration hides them.
func TestLoadConfigRegistersTokens(t *testing.T) {
	defaultToken, ownToken := fixtureToken(t, ""), fixtureToken(t, "own")
	cfgPath := filepath.Join(t.TempDir(), "config.toml")
	content := `[github]
token-file = "` + writeTokenFile(t, defaultToken) + `"

[runner]
image = "sha256:` + strings.Repeat("a", 64) + `"

[buildkit]
image = "moby/buildkit:v0.0.0-rootless@sha256:` + strings.Repeat("b", 64) + `"

[[targets]]
url = "https://github.com/acme/widget"

[[targets]]
url = "https://github.com/acme/gadget"
token-file = "` + writeTokenFile(t, ownToken) + `"
`
	if err := os.WriteFile(cfgPath, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	e := newEnv(&stdout, &stderr)
	cfg, err := daemonConfig(e, cfgPath, make(chan os.Signal))
	if err != nil {
		t.Fatal(err)
	}
	_, creds, err := cfg.LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if creds.DefaultToken != defaultToken || creds.TargetTokens[1] != ownToken || creds.AppKey != nil {
		t.Fatal("LoadConfig did not return the configured credentials")
	}
	e.logger.Info("probe", "default", defaultToken, "own", ownToken, "creds", creds)
	out := stderr.String()
	for name, token := range map[string]string{"default": defaultToken, "own": ownToken} {
		if strings.Contains(out, token) {
			t.Errorf("the %s token is in the log", name)
		}
	}
	if n := strings.Count(out, logging.Placeholder); n != 3 {
		t.Errorf("log has %d %s, want 3: %s", n, logging.Placeholder, out)
	}
}

// TestDaemonConfigApp0002 proves 0003 SPEC R271: a 0002 configuration (two
// App keys, no token key) loads the App key and no token, and the run
// wiring makes every target's credential from the App.
func TestDaemonConfigApp0002(t *testing.T) {
	cfgPath := writeTestConfig(t, t.TempDir(), "25m")
	var stdout, stderr bytes.Buffer
	cfg, err := daemonConfig(newEnv(&stdout, &stderr), cfgPath, make(chan os.Signal))
	if err != nil {
		t.Fatal(err)
	}
	loaded, creds, err := cfg.LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if creds.AppKey == nil || creds.DefaultToken != "" || len(creds.TargetTokens) != 1 || creds.TargetTokens[0] != "" {
		t.Fatal("a 0002 configuration did not give the App key alone")
	}
	gh, err := cfg.NewGitHub(loaded, creds)
	if err != nil {
		t.Fatalf("NewGitHub = %v", err)
	}
	target, err := github.ParseTargetURL(loaded.Targets[0].URL)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := gh.Auth(target, 0); err != nil {
		t.Fatalf("Auth = %v, want the App credential", err)
	}
}

// TestRunLogLevel proves log.level: once the run wiring's LoadConfig loaded
// the configuration, the env's logger and the loggers derived from it, the
// scaleset library's among them, write debug lines only at level debug.
// Before the configuration loads, and when it is invalid, the level is info.
func TestRunLogLevel(t *testing.T) {
	tests := []struct {
		level       string
		debug, info bool
	}{
		{"debug", true, true},
		{"info", false, true},
		{"warn", false, false},
	}
	for _, tt := range tests {
		t.Run(tt.level, func(t *testing.T) {
			path := writeTestConfig(t, t.TempDir(), "10m")
			f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := f.WriteString("\n[log]\nlevel = \"" + tt.level + "\"\n"); err != nil {
				t.Fatal(err)
			}
			if err := f.Close(); err != nil {
				t.Fatal(err)
			}
			var stdout, stderr bytes.Buffer
			e := newEnv(&stdout, &stderr)
			cfg, err := daemonConfig(e, path, make(chan os.Signal))
			if err != nil {
				t.Fatal(err)
			}
			e.logger.Debug("debug before load")
			e.logger.Info("info before load")
			if _, _, err := cfg.LoadConfig(); err != nil {
				t.Fatalf("LoadConfig: %v", err)
			}
			lib := e.logger.With("component", "scaleset")
			lib.Debug("library debug")
			e.logger.Debug("debug after load")
			e.logger.Info("info after load")
			out := stderr.String()
			for msg, want := range map[string]bool{
				"debug before load": false,
				"info before load":  true,
				"library debug":     tt.debug,
				"debug after load":  tt.debug,
				"info after load":   tt.info,
			} {
				if got := strings.Contains(out, `"msg":"`+msg+`"`); got != want {
					t.Errorf("%q logged = %v, want %v:\n%s", msg, got, want, out)
				}
			}
		})
	}

	var stdout, stderr bytes.Buffer
	e := newEnv(&stdout, &stderr)
	cfg, err := daemonConfig(e, "/nonexistent/config.toml", make(chan os.Signal))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := cfg.LoadConfig(); err == nil {
		t.Fatal("LoadConfig of a missing file succeeded")
	}
	e.logger.Debug("debug after a failed load")
	e.logger.Info("info after a failed load")
	if out := stderr.String(); strings.Contains(out, "debug after a failed load") || !strings.Contains(out, "info after a failed load") {
		t.Errorf("level after a failed load is not info:\n%s", out)
	}
}

// TestDaemonConfigDockerHost proves that the run wiring makes the Docker
// client for the host the daemon passes, docker.host, without connecting.
func TestDaemonConfigDockerHost(t *testing.T) {
	var stdout, stderr bytes.Buffer
	cfg, err := daemonConfig(newEnv(&stdout, &stderr), "/nonexistent/config.toml", make(chan os.Signal))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cfg.NewDocker("tcp://127.0.0.1:2376"); err != nil {
		t.Errorf("NewDocker(tcp) = %v", err)
	}
	if _, err := cfg.NewDocker(config.DefaultDockerHost); err != nil {
		t.Errorf("NewDocker(default) = %v", err)
	}
}
