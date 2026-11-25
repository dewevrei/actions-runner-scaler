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

	"github.com/dewevrei/actions-runner-scaler/internal/service"
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
app-installation-id = 1
app-private-key-file = "` + keyPath + `"

[runner]
image = "sha256:` + strings.Repeat("a", 64) + `"

[buildkit]
image = "moby/buildkit:v0.0.0-rootless@sha256:` + strings.Repeat("b", 64) + `"

[shutdown]
drain-timeout = "` + drain + `"

[[pools]]
repo-url = "https://github.com/example-org/example-repo"
allowed-workflows = [".github/workflows/deploy.yml"]
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
	host := serviceHost{uid: 501, home: home, executable: "/opt/scaler/bin/actions-runner-scaler", launchctl: f}
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
		"<key>ExitTimeOut</key>\n\t<integer>1560</integer>", // 25m + 60s
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
	host := serviceHost{uid: 501, home: home, executable: "/opt/scaler/bin/actions-runner-scaler", launchctl: &fakeLaunchctl{}}
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
			host := serviceHost{uid: 501, home: home, executable: tt.executable, launchctl: f}
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
	host := serviceHost{uid: 501, home: t.TempDir(), executable: "/opt/scaler/bin/actions-runner-scaler", launchctl: &fakeLaunchctl{}}
	var stdout, stderr bytes.Buffer
	if err := runService(context.Background(), newEnv(&stdout, &stderr), host, "uninstall", "unused.toml"); err != nil {
		t.Fatal(err)
	}
	if out := stdout.String(); !strings.Contains(out, "is not installed") || strings.Contains(out, "uninstalled") {
		t.Errorf("stdout = %q, want a not installed message", out)
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
