package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dewevrei/actions-runner-scaler/internal/daemon"
	"github.com/dewevrei/actions-runner-scaler/internal/service"
	"github.com/dewevrei/actions-runner-scaler/internal/status"
)

// fakeLaunchctl records calls; nothing is ever loaded. The real launchctl
// is never run by these tests.
type fakeLaunchctl struct {
	mu    sync.Mutex
	calls []string
}

func (f *fakeLaunchctl) Print(_ context.Context, target string) (bool, error) {
	f.add("print " + target)
	return false, nil
}

func (f *fakeLaunchctl) Bootout(_ context.Context, target string) error {
	f.add("bootout " + target)
	return nil
}

func (f *fakeLaunchctl) Bootstrap(_ context.Context, domain, path string) error {
	f.add("bootstrap " + domain + " " + path)
	return nil
}

func (f *fakeLaunchctl) add(c string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, c)
}

// fakeDaemon stands in for the scaler behind the status socket; no real
// process is signaled. A running one exits when terminated.
type fakeDaemon struct {
	running bool
	pid     int

	mu         sync.Mutex
	exited     chan struct{}
	terminated []int
}

func newFakeDaemon(running bool, pid int) *fakeDaemon {
	return &fakeDaemon{running: running, pid: pid, exited: make(chan struct{})}
}

func (d *fakeDaemon) Find(context.Context) (status.Report, int, bool, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	select {
	case <-d.exited:
		return status.Report{}, 0, false, nil
	default:
	}
	if !d.running {
		return status.Report{}, 0, false, nil
	}
	return status.Report{State: status.Running}, d.pid, true, nil
}

func (d *fakeDaemon) Watch(int) (service.ExitWatch, error) { return fakeWatch{d.exited}, nil }

func (d *fakeDaemon) Terminate(pid int) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.terminated = append(d.terminated, pid)
	select {
	case <-d.exited:
	default:
		close(d.exited)
	}
	return nil
}

func (d *fakeDaemon) terminatedPIDs() []int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]int(nil), d.terminated...)
}

type fakeWatch struct{ ch chan struct{} }

func (w fakeWatch) Wait(ctx context.Context) error {
	select {
	case <-w.ch:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (w fakeWatch) Exited() bool {
	select {
	case <-w.ch:
		return true
	default:
		return false
	}
}

func (fakeWatch) Close() error { return nil }

// testHost is a serviceHost with a fake launchctl and an idle fake daemon.
// Its default config path does not exist and its lock is always free.
func testHost(home string, f service.Launchctl) serviceHost {
	return serviceHost{
		uid:           501,
		home:          home,
		executable:    "/opt/scaler/bin/actions-runner-scaler",
		launchctl:     f,
		daemon:        newFakeDaemon(false, 0),
		defaultConfig: filepath.Join(home, "no-such-config.toml"),
		lock:          func() (func(), error) { return func() {}, nil },
	}
}

// writeTestConfig writes a valid configuration with the given drain-timeout
// and a throwaway RSA key (0600) into dir, and returns the config path.
func writeTestConfig(t *testing.T, dir, drain string) string {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	keyPath := filepath.Join(dir, "app.pem")
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	if err := os.WriteFile(keyPath, keyPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := `[github]
app-client-id = "fake-client-id"
app-private-key-file = "` + keyPath + `"

[runner]
image = "sha256:` + strings.Repeat("a", 64) + `"

[buildkit]
image = "moby/buildkit:v0.0.0-rootless@sha256:` + strings.Repeat("b", 64) + `"

[shutdown]
drain-timeout = "` + drain + `"

[[targets]]
url = "https://github.com/acme/widget"
`
	path := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(path, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestServiceInstallAndUninstall(t *testing.T) {
	home := t.TempDir()
	configPath := writeTestConfig(t, t.TempDir(), "25m")
	f := &fakeLaunchctl{}
	host := testHost(home, f)
	var stdout, stderr bytes.Buffer
	e := newEnv(&stdout, &stderr)

	if err := runService(context.Background(), e, host, "install", configPath); err != nil {
		t.Fatalf("install: %v\n%s", err, stderr.String())
	}
	plistPath := filepath.Join(home, "Library", "LaunchAgents", service.Label+".plist")
	data, err := os.ReadFile(plistPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"<string>/opt/scaler/bin/actions-runner-scaler</string>",
		"<string>run</string>\n\t\t<string>-config</string>\n\t\t<string>" + configPath + "</string>",
		"<key>ExitTimeOut</key>\n\t<integer>60</integer>", // the macOS 26.4+ cap, whatever drain-timeout is
		"<key>EnvironmentVariables</key>\n\t<dict>\n\t\t<key>HOME</key>\n\t\t<string>" + home + "</string>\n\t</dict>",
		"<string>" + filepath.Join(home, "Library", "Logs", "actions-runner-scaler", "stderr.log") + "</string>",
	} {
		if !strings.Contains(string(data), want) {
			t.Errorf("plist lacks %q:\n%s", want, data)
		}
	}
	if !strings.Contains(stdout.String(), "installed "+service.Label) {
		t.Errorf("stdout = %q", stdout.String())
	}

	if err := runService(context.Background(), e, host, "uninstall", configPath); err != nil {
		t.Fatalf("uninstall: %v", err)
	}
	if _, err := os.Stat(plistPath); !os.IsNotExist(err) {
		t.Errorf("plist still there after uninstall: %v", err)
	}
	if !strings.Contains(stdout.String(), "uninstalled "+service.Label) {
		t.Errorf("stdout = %q, want uninstalled", stdout.String())
	}
	want := []string{
		"print gui/501/" + service.Label,
		"bootstrap gui/501/ " + plistPath,
		"print gui/501/" + service.Label,
	}
	if strings.Join(f.calls, "\n") != strings.Join(want, "\n") {
		t.Errorf("calls = %q, want %q", f.calls, want)
	}
}

// A relative -config is written to the plist as an absolute path.
func TestServiceInstallRelativeConfig(t *testing.T) {
	dir := t.TempDir()
	writeTestConfig(t, dir, "10m")
	t.Chdir(dir)
	home := t.TempDir()
	host := testHost(home, &fakeLaunchctl{})
	var stdout, stderr bytes.Buffer
	if err := runService(context.Background(), newEnv(&stdout, &stderr), host, "install", "config.toml"); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(home, "Library", "LaunchAgents", service.Label+".plist"))
	if err != nil {
		t.Fatal(err)
	}
	want, err := filepath.Abs("config.toml")
	if err != nil {
		t.Fatal(err)
	}
	if !filepath.IsAbs(want) || !strings.Contains(string(data), "<string>"+want+"</string>") {
		t.Errorf("plist lacks absolute config path %q:\n%s", want, data)
	}
}

func TestServiceInstallRefusesBadInput(t *testing.T) {
	tests := []struct {
		name       string
		executable string
		config     func(t *testing.T) string
		wantErr    string
	}{
		{
			name:       "go run binary",
			executable: "/var/folders/xx/T/go-build123/b001/exe/actions-runner-scaler",
			config:     func(t *testing.T) string { return writeTestConfig(t, t.TempDir(), "10m") },
			wantErr:    "go run",
		},
		{
			name:       "invalid config",
			executable: "/opt/scaler/bin/actions-runner-scaler",
			config: func(t *testing.T) string {
				p := filepath.Join(t.TempDir(), "config.toml")
				if err := os.WriteFile(p, []byte("[shutdown]\ndrain-timeout = \"0s\"\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				return p
			},
			wantErr: "drain-timeout",
		},
		{
			name:       "missing config",
			executable: "/opt/scaler/bin/actions-runner-scaler",
			config:     func(t *testing.T) string { return filepath.Join(t.TempDir(), "none.toml") },
			wantErr:    "config",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			home := t.TempDir()
			f := &fakeLaunchctl{}
			host := testHost(home, f)
			host.executable = tt.executable
			var stdout, stderr bytes.Buffer
			err := runService(context.Background(), newEnv(&stdout, &stderr), host, "install", tt.config(t))
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("err = %v, want it to mention %q", err, tt.wantErr)
			}
			if len(f.calls) != 0 {
				t.Errorf("launchctl calls = %q, want none", f.calls)
			}
			if _, err := os.Stat(filepath.Join(home, "Library")); !os.IsNotExist(err) {
				t.Errorf("wrote under home/Library: %v", err)
			}
		})
	}
}

func TestServiceUninstallNothingInstalled(t *testing.T) {
	host := testHost(t.TempDir(), &fakeLaunchctl{})
	var stdout, stderr bytes.Buffer
	if err := runService(context.Background(), newEnv(&stdout, &stderr), host, "uninstall", "unused.toml"); err != nil {
		t.Fatal(err)
	}
	if out := stdout.String(); !strings.Contains(out, "is not installed") || strings.Contains(out, "uninstalled") {
		t.Errorf("stdout = %q, want a not installed message", out)
	}
}

// service stop signals the scaler serving the status socket and waits for
// it, leaving the agent loaded: no launchctl call.
func TestServiceStop(t *testing.T) {
	f := &fakeLaunchctl{}
	host := testHost(t.TempDir(), f)
	d := newFakeDaemon(true, 4242)
	host.daemon = d
	configPath := writeTestConfig(t, t.TempDir(), "25m")
	var stdout, stderr bytes.Buffer
	if err := runService(context.Background(), newEnv(&stdout, &stderr), host, "stop", configPath); err != nil {
		t.Fatalf("stop: %v\n%s", err, stderr.String())
	}
	if got := d.terminatedPIDs(); len(got) != 1 || got[0] != 4242 {
		t.Errorf("terminated %v, want [4242]", got)
	}
	out := stdout.String()
	for _, want := range []string{
		"sent SIGTERM to the scaler (pid 4242",
		"waiting up to 26m30s", // 25m drain-timeout + 60s + 30s
		"stays loaded",
		"launchctl kickstart gui/501/" + service.Label,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("stdout lacks %q:\n%s", want, out)
		}
	}
	if len(f.calls) != 0 {
		t.Errorf("launchctl calls = %q, want none", f.calls)
	}
}

func TestServiceStopNotRunning(t *testing.T) {
	host := testHost(t.TempDir(), &fakeLaunchctl{})
	var stdout, stderr bytes.Buffer
	if err := runService(context.Background(), newEnv(&stdout, &stderr), host, "stop", writeTestConfig(t, t.TempDir(), "10m")); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout.String(), "not running; nothing to stop") {
		t.Errorf("stdout = %q", stdout.String())
	}
}

// stop must work while the config is broken, for example for a scaler in
// error-wait over a config error: it logs a warning and waits for the
// default drain-timeout.
func TestServiceStopBrokenConfig(t *testing.T) {
	host := testHost(t.TempDir(), &fakeLaunchctl{})
	d := newFakeDaemon(true, 4242)
	host.daemon = d
	var stdout, stderr bytes.Buffer
	if err := runService(context.Background(), newEnv(&stdout, &stderr), host, "stop", filepath.Join(t.TempDir(), "none.toml")); err != nil {
		t.Fatal(err)
	}
	if len(d.terminatedPIDs()) != 1 {
		t.Error("the scaler was not signaled")
	}
	if want := "waiting up to " + service.StopLimit(10*time.Minute).String(); !strings.Contains(stdout.String(), want) {
		t.Errorf("stdout lacks %q:\n%s", want, stdout.String())
	}
	if !strings.Contains(stderr.String(), "default drain-timeout") {
		t.Errorf("stderr lacks the config warning:\n%s", stderr.String())
	}
}

// Without -config, stop waits for the drain-timeout of the config in the
// installed plist.
func TestServiceStopUsesInstalledConfig(t *testing.T) {
	host := testHost(t.TempDir(), &fakeLaunchctl{})
	var stdout, stderr bytes.Buffer
	e := newEnv(&stdout, &stderr)
	if err := runService(context.Background(), e, host, "install", writeTestConfig(t, t.TempDir(), "25m")); err != nil {
		t.Fatal(err)
	}
	host.daemon = newFakeDaemon(true, 4242)
	stdout.Reset()
	if err := runService(context.Background(), e, host, "stop", ""); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout.String(), "waiting up to 26m30s") {
		t.Errorf("stdout lacks the installed config's wait:\n%s", stdout.String())
	}
}

// Only one service command runs at a time: with the lock held by another,
// the command says so, exits 1 without a failure log, and touches nothing.
func TestServiceLockHeld(t *testing.T) {
	lockPath := filepath.Join(t.TempDir(), "stop.lock")
	unlock, err := daemon.LockFile(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	for _, action := range []string{"install", "stop", "uninstall"} {
		f := &fakeLaunchctl{}
		host := testHost(t.TempDir(), f)
		d := newFakeDaemon(true, 4242)
		host.daemon = d
		host.lock = func() (func(), error) { return daemon.LockFile(lockPath) }
		var stdout, stderr bytes.Buffer
		err := runService(context.Background(), newEnv(&stdout, &stderr), host, action, writeTestConfig(t, t.TempDir(), "10m"))
		if _, ok := err.(reportedError); !ok {
			t.Errorf("%s: err = %v, want a reportedError", action, err)
		}
		if !strings.Contains(stdout.String(), "another service command is running") {
			t.Errorf("%s: stdout = %q", action, stdout.String())
		}
		if len(f.calls) != 0 || len(d.terminatedPIDs()) != 0 {
			t.Errorf("%s: launchctl calls %q, terminated %v, want none", action, f.calls, d.terminatedPIDs())
		}
	}
}

func TestProgramPath(t *testing.T) {
	tests := []struct {
		exe     string
		wantErr bool
	}{
		{"/opt/scaler/bin/actions-runner-scaler", false},
		{"/Users/x/go-builder/bin/app", false},
		{"/Users/x/go-build/bin/app", false},
		{"/Users/x/my-go-build1/bin/app", false},
		{"/var/folders/xx/T/go-build123/b001/exe/actions-runner-scaler", true},
		{"/tmp/go-build9/exe/app", true},
	}
	for _, tt := range tests {
		got, err := programPath(tt.exe)
		if (err != nil) != tt.wantErr {
			t.Errorf("programPath(%q) = %q, %v, wantErr %v", tt.exe, got, err, tt.wantErr)
		}
	}
}

func TestServiceUsageText(t *testing.T) {
	for _, args := range [][]string{{"-h"}, {"install", "-h"}, {"-bogus", "install"}, {"install", "-bogus"}} {
		var stdout, stderr bytes.Buffer
		fs := newFlagSet("service", newEnv(&stdout, &stderr))
		fs.String("config", "default.toml", "")
		if _, err := parseServiceArgs(fs, args); err == nil {
			t.Errorf("%q: no error", args)
		}
		if n := strings.Count(stderr.String(), serviceUsage); n != 1 {
			t.Errorf("%q: stderr has the usage %d times, want once:\n%s", args, n, stderr.String())
		}
	}
}

func TestParseServiceArgs(t *testing.T) {
	tests := []struct {
		args       []string
		wantAction string
		wantConfig string
		wantErr    bool
	}{
		{[]string{"install"}, "install", "default.toml", false},
		{[]string{"install", "-config", "x.toml"}, "install", "x.toml", false},
		{[]string{"-config", "x.toml", "install"}, "install", "x.toml", false},
		{[]string{"uninstall"}, "uninstall", "default.toml", false},
		{[]string{"stop"}, "stop", "default.toml", false},
		{[]string{"stop", "-config", "x.toml"}, "stop", "x.toml", false},
		{[]string{}, "", "", true},
		{[]string{"restart"}, "", "", true},
		{[]string{"install", "extra"}, "", "", true},
		{[]string{"install", "-bogus"}, "", "", true},
	}
	for _, tt := range tests {
		t.Run(strings.Join(tt.args, " "), func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			fs := newFlagSet("service", newEnv(&stdout, &stderr))
			config := fs.String("config", "default.toml", "")
			action, err := parseServiceArgs(fs, tt.args)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if err == nil && (action != tt.wantAction || *config != tt.wantConfig) {
				t.Errorf("action, config = %q, %q, want %q, %q", action, *config, tt.wantAction, tt.wantConfig)
			}
			if err != nil && !strings.Contains(strings.ToLower(stderr.String()+err.Error()), "usage") {
				t.Errorf("err = %v, stderr = %q, want usage", err, stderr.String())
			}
		})
	}
}
