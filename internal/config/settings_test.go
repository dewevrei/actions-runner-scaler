package config

import (
	"bytes"
	"crypto/rsa"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/moby/moby/client"
)

// TestScalingServerLimit proves the convention fix that gives the server
// limit a setting: absent or 0 leaves the value computed from Docker, a
// positive number replaces it, a negative number is refused.
func TestScalingServerLimit(t *testing.T) {
	key := writeKey(t, pkcs1PEM(t), 0o600)
	tests := []struct {
		name    string
		table   string
		want    int
		wantErr string
	}{
		{name: "absent", want: 0},
		{name: "zero", table: "[scaling]\nserver-limit = 0\n", want: 0},
		{name: "positive", table: "[scaling]\nserver-limit = 4\n", want: 4},
		{name: "negative", table: "[scaling]\nserver-limit = -1\n", wantErr: "scaling.server-limit: must be 0"},
		{name: "float", table: "[scaling]\nserver-limit = 1.5\n", wantErr: "scaling.server-limit"},
		{name: "string", table: "[scaling]\nserver-limit = \"4\"\n", wantErr: "scaling.server-limit"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, err := loadString(t, requiredTOML(key)+tt.table)
			if tt.wantErr != "" {
				if errs := errorList(err); len(errs) != 1 || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("Load error = %v, want one error containing %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if cfg.Scaling.ServerLimit != tt.want {
				t.Errorf("scaling.server-limit = %d, want %d", cfg.Scaling.ServerLimit, tt.want)
			}
		})
	}
}

// TestBuildKitEnabled proves buildkit.enabled: false when absent, the
// value written when it is a boolean, and a type error otherwise. The other
// [buildkit] keys decode beside it as before.
func TestBuildKitEnabled(t *testing.T) {
	key := writeKey(t, pkcs1PEM(t), 0o600)
	if Default().BuildKit.Enabled {
		t.Error("Default().BuildKit.Enabled = true, want false")
	}
	tests := []struct {
		name    string
		lines   string
		want    bool
		wantErr string
	}{
		{name: "absent", want: false},
		{name: "false", lines: "enabled = false\n", want: false},
		{name: "true", lines: "enabled = true\ncpus = 3\n", want: true},
		{name: "string", lines: "enabled = \"true\"\n", wantErr: "buildkit.enabled"},
		{name: "number", lines: "enabled = 1\n", wantErr: "buildkit.enabled"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, err := loadString(t, withBuildKit(requiredTOML(key), tt.lines))
			if tt.wantErr != "" {
				if errs := errorList(err); len(errs) != 1 || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("Load error = %v, want one error containing %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if cfg.BuildKit.Enabled != tt.want {
				t.Errorf("buildkit.enabled = %v, want %v", cfg.BuildKit.Enabled, tt.want)
			}
			if tt.want && cfg.BuildKit.CPUs != 3 {
				t.Errorf("buildkit.cpus = %v beside enabled, want 3", cfg.BuildKit.CPUs)
			}
		})
	}
}

func TestServerLimitOr(t *testing.T) {
	tests := []struct {
		limit, computed, want int
	}{
		{0, 3, 3},
		{0, 0, 0},
		{2, 3, 2},
		{8, 3, 8},
		{-1, 3, 3},
	}
	for _, tt := range tests {
		if got := (Scaling{ServerLimit: tt.limit}).ServerLimitOr(tt.computed); got != tt.want {
			t.Errorf("Scaling{ServerLimit: %d}.ServerLimitOr(%d) = %d, want %d", tt.limit, tt.computed, got, tt.want)
		}
	}
}

// TestDockerHost proves docker.host: the default is the Docker client's own,
// unix:// and tcp:// URLs load, anything else is refused, and no error quotes
// the value, which a tcp URL could use to carry a password.
func TestDockerHost(t *testing.T) {
	key := writeKey(t, pkcs1PEM(t), 0o600)

	if DefaultDockerHost != client.DefaultDockerHost {
		t.Errorf("DefaultDockerHost = %q, want the Docker client's %q", DefaultDockerHost, client.DefaultDockerHost)
	}
	cfg, err := loadString(t, requiredTOML(key))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Docker.Host != "unix:///var/run/docker.sock" {
		t.Errorf("docker.host default = %q, want unix:///var/run/docker.sock", cfg.Docker.Host)
	}

	for _, host := range []string{
		"unix:///var/run/docker.sock",
		"unix:///Users/someone/.docker/run/docker.sock",
		"tcp://127.0.0.1:2375",
		"tcp://[::1]:2376",
		"tcp://docker.internal:2375",
		"tcp://docker.internal:2375/base",
	} {
		t.Run("valid "+host, func(t *testing.T) {
			cfg, err := loadString(t, requiredTOML(key)+"[docker]\nhost = \""+host+"\"\n")
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if cfg.Docker.Host != host {
				t.Errorf("docker.host = %q, want %q", cfg.Docker.Host, host)
			}
		})
	}

	for _, host := range []string{
		"",
		"/var/run/docker.sock",
		"unix://",
		"unix:///",
		"unix://docker.sock",
		"tcp://",
		"tcp://docker.internal",
		"tcp://:2375",
		"tcp://docker.internal:0",
		"tcp://docker.internal:65536",
		"tcp://docker.internal:port",
		"tcp://user:hunter2secret@docker.internal:2375",
		"http://docker.internal:2375",
		"ssh://docker.internal",
		"npipe:////./pipe/docker_engine",
		"docker.internal:2375",
		"tcp://::1:2375",
		"tcp://h:1:2",
		"tcp://[::1]",
		"tcp://[::1]:",
		"tcp://docker.internal:2375?x=1",
		"tcp://docker.internal:2375?",
		"tcp://docker.internal:2375#frag",
		"tcp://docker.internal:2375#",
		// TOML escapes of a tab, a NUL and a DEL.
		`unix:///var/run/dock\ter.sock`,
		`unix:///var/run/docker.sock\u0000`,
		`tcp://docker.internal:2375\u007F`,
	} {
		t.Run("invalid "+host, func(t *testing.T) {
			_, err := loadString(t, requiredTOML(key)+"[docker]\nhost = \""+host+"\"\n")
			if errs := errorList(err); len(errs) != 1 || !strings.HasPrefix(err.Error(), "docker.host: must be unix://") {
				t.Fatalf("Load error = %v, want one docker.host error", err)
			}
			if rest := strings.Replace(err.Error(), msgDockerHost, "", 1); host != "" && strings.Contains(rest, host) {
				t.Errorf("error quotes the value: %v", err)
			}
		})
	}
}

// TestValidDockerHost checks validDockerHost with real control characters,
// which a TOML basic string cannot hold raw, and with bracketed IPv6.
func TestValidDockerHost(t *testing.T) {
	for host, want := range map[string]bool{
		"unix:///var/run/docker.sock":     true,
		"tcp://[::1]:2375":                true,
		"tcp://127.0.0.1:2375":            true,
		"tcp://::1:2375":                  false,
		"tcp://h:1:2":                     false,
		"unix:///var/run/docker.sock\n":   false,
		"unix:///var/run/docker.sock\r":   false,
		"unix:///var/run/dock\ter.sock":   false,
		"unix:///var/run/docker.sock\x00": false,
		"unix:///var/run/docker.sock\x1b": false,
		"unix:///var/run/docker.sock\x7f": false,
		"tcp://docker.internal:2375\x01":  false,
		"tcp://docker.internal:2375?a=b":  false,
		"tcp://docker.internal:2375#a":    false,
	} {
		if got := validDockerHost(host); got != want {
			t.Errorf("validDockerHost(%q) = %v, want %v", host, got, want)
		}
	}
}

// TestLogLevel proves log.level: info by default, debug, info, warn and
// error load and map to the slog levels, anything else is refused.
func TestLogLevel(t *testing.T) {
	key := writeKey(t, pkcs1PEM(t), 0o600)

	cfg, err := loadString(t, requiredTOML(key))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Log.Level != "info" || cfg.Log.SlogLevel() != slog.LevelInfo {
		t.Errorf("log.level default = %q (%v), want info", cfg.Log.Level, cfg.Log.SlogLevel())
	}

	for level, want := range map[string]slog.Level{
		"debug": slog.LevelDebug,
		"info":  slog.LevelInfo,
		"warn":  slog.LevelWarn,
		"error": slog.LevelError,
	} {
		t.Run(level, func(t *testing.T) {
			cfg, err := loadString(t, requiredTOML(key)+"[log]\nlevel = \""+level+"\"\n")
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if got := cfg.Log.SlogLevel(); got != want {
				t.Errorf("SlogLevel() = %v, want %v", got, want)
			}
		})
	}

	for _, level := range []string{"", "INFO", "Warn", "trace", "warning", "fatal", "info+2", " info"} {
		t.Run("invalid "+level, func(t *testing.T) {
			_, err := loadString(t, requiredTOML(key)+"[log]\nlevel = \""+level+"\"\n")
			if errs := errorList(err); len(errs) != 1 || !strings.HasPrefix(err.Error(), "log.level: must be debug, info, warn or error") {
				t.Fatalf("Load error = %v, want one log.level error", err)
			}
		})
	}
	if _, err := loadString(t, requiredTOML(key)+"[log]\nlevel = 1\n"); err == nil || !strings.Contains(err.Error(), "log.level") {
		t.Errorf("Load error = %v, want a log.level type error", err)
	}
	if got := (Log{Level: "bogus"}).SlogLevel(); got != slog.LevelInfo {
		t.Errorf("SlogLevel() of an invalid level = %v, want info", got)
	}
}

// TestDefaultsOfNewKeys proves the defaults of the keys added by the
// convention fixes, as literals.
func TestDefaultsOfNewKeys(t *testing.T) {
	c := Default()
	if c.Scaling.ServerLimit != 0 || c.Docker.Host != "unix:///var/run/docker.sock" || c.Log.Level != "info" {
		t.Errorf("defaults = server-limit %d, docker.host %q, log.level %q; want 0, unix:///var/run/docker.sock, info",
			c.Scaling.ServerLimit, c.Docker.Host, c.Log.Level)
	}
	if MinMemory != 6*Mebibyte || MinPids != 1 {
		t.Errorf("lower bounds = memory %s, pids %d; want 6m and 1 (Docker's own)", MinMemory, MinPids)
	}
}

// appTarget returns a [[targets]] entry that names its own App.
func appTarget(url, clientID, keyPath string) string {
	return targetTOML(url, `app-client-id = "`+clientID+`"`+"\n"+`app-private-key-file = "`+keyPath+`"`+"\n")
}

// TestModeRule proves the mode rule of 0001 R30 and 0003 R275 as OpenSSH
// and runpool apply it: only group and other permission bits refuse a key
// or token file; owner-only modes and special bits do not.
func TestModeRule(t *testing.T) {
	tests := []struct {
		mode    os.FileMode
		refused bool
	}{
		{0o600, false},
		{0o400, false},
		{0o700, false},
		{0o500, false},
		{0o200, false},
		{0o600 | os.ModeSetuid, false},
		{0o640, true},
		{0o604, true},
		{0o660, true},
		{0o606, true},
		{0o610, true},
		{0o601, true},
		{0o644, true},
		{0o755, true},
		{0o777, true},
		{0o644 | os.ModeSetuid, true},
	}
	for _, tt := range tests {
		t.Run(tt.mode.String(), func(t *testing.T) {
			err := checkSecretMode(tt.mode)
			if (err != nil) != tt.refused {
				t.Fatalf("checkSecretMode(%v) = %v, want refused %v", tt.mode, err, tt.refused)
			}
			if err != nil && !strings.Contains(err.Error(), "wider than 0600") {
				t.Errorf("error %q does not say the mode is wider than 0600", err)
			}
		})
	}
}

// inspectConfig returns the Config of content as Inspect decodes it, with
// every problem validation found left aside, so the loading code can be
// tried on a configuration validation refuses.
func inspectConfig(t *testing.T, content string) *Config {
	t.Helper()
	in, err := Inspect(writeFile(t, "config.toml", []byte(content), 0o600))
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	return in.Config
}

// TestTargetAppCredential proves the rules of a target's own App: a pair or
// a token file, never both and never half a pair, with the default's checks
// of the key file. A target with App keys of its own needs no default
// credential.
func TestTargetAppCredential(t *testing.T) {
	key := func() string { return writeKey(t, pkcs1PEM(t), 0o600) }
	token := func() string { return writeToken(t, fixtureToken("github_pat_"), 0o600) }
	wantNoDefault := "github: no default credential (app-client-id and app-private-key-file, or token-file) for targets without their own token-file or App: "

	tests := []struct {
		name   string
		github bool // whether the default App is written
		extra  func() string
		want   []string
	}{
		{name: "App target on the default App", github: true, extra: func() string {
			return appTarget("https://github.com/acme/second", "other-id", key())
		}, want: nil},
		{name: "no [github], a bare target and an App target", extra: func() string {
			return appTarget("https://github.com/acme/second", "other-id", key())
		}, want: []string{wantNoDefault + "targets[0] (https://github.com/acme/widget)"}},
		{name: "both", github: true, extra: func() string {
			return targetTOML("https://github.com/acme/second",
				`app-client-id = "other-id"`+"\n"+`app-private-key-file = "`+key()+`"`+"\n"+tokenLine(token()))
		}, want: []string{"targets[1]: " + msgCredentialConflict}},
		{name: "client ID and token", github: true, extra: func() string {
			return targetTOML("https://github.com/acme/second", `app-client-id = "other-id"`+"\n"+tokenLine(token()))
		}, want: []string{"targets[1]: " + msgCredentialConflict}},
		{name: "key file and token", github: true, extra: func() string {
			return targetTOML("https://github.com/acme/second", `app-private-key-file = "`+key()+`"`+"\n"+tokenLine(token()))
		}, want: []string{"targets[1]: " + msgCredentialConflict}},
		{name: "client ID only", github: true, extra: func() string {
			return targetTOML("https://github.com/acme/second", `app-client-id = "other-id"`+"\n")
		}, want: []string{"targets[1].app-private-key-file: is required"}},
		{name: "key file only", github: true, extra: func() string {
			return targetTOML("https://github.com/acme/second", `app-private-key-file = "`+key()+`"`+"\n")
		}, want: []string{"targets[1].app-client-id: is required"}},
		{name: "blank client ID", github: true, extra: func() string {
			return appTarget("https://github.com/acme/second", "  ", key())
		}, want: []string{"targets[1].app-client-id: is required"}},
		{name: "relative key file", github: true, extra: func() string {
			return appTarget("https://github.com/acme/second", "other-id", "app.pem")
		}, want: []string{"targets[1].app-private-key-file: must be an absolute path"}},
		{name: "wide key file", github: true, extra: func() string {
			return appTarget("https://github.com/acme/second", "other-id", writeKey(t, pkcs1PEM(t), 0o644))
		}, want: []string{"targets[1].app-private-key-file: file mode -rw-r--r-- is wider than 0600 (group or other bits are set); use 0600 or 0400"}},
		{name: "owner-only key file", github: true, extra: func() string {
			return appTarget("https://github.com/acme/second", "other-id", writeKey(t, pkcs1PEM(t), 0o700))
		}, want: nil},
		{name: "key file that is not a key", github: true, extra: func() string {
			return appTarget("https://github.com/acme/second", "other-id", writeKey(t, []byte("garbage"), 0o600))
		}, want: []string{"targets[1].app-private-key-file: no PEM block found"}},
		{name: "key content as the path", github: true, extra: func() string {
			return targetTOML("https://github.com/acme/second",
				`app-client-id = "other-id"`+"\n"+`app-private-key-file = "-----BEGIN RSA PRIVATE KEY-----\nQUJD\n-----END RSA PRIVATE KEY-----\n"`+"\n")
		}, want: []string{"targets[1].app-private-key-file: value looks like key content, not a file path; set the path of the key file"}},
		{name: "App target beside a token target", github: true, extra: func() string {
			return appTarget("https://github.com/acme/second", "other-id", key()) +
				targetTOML("https://github.com/acme/third", tokenLine(token()))
		}, want: nil},
		{name: "no default, an App target, a token target and a bare one", extra: func() string {
			return appTarget("https://github.com/acme/second", "other-id", key()) +
				targetTOML("https://github.com/acme/third", tokenLine(token())) +
				targetTOML("https://github.com/acme/fourth", "")
		}, want: []string{
			wantNoDefault + "targets[0] (https://github.com/acme/widget), targets[3] (https://github.com/acme/fourth)",
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			content := githubTOML("")
			if tt.github {
				content = requiredTOML(writeKey(t, pkcs1PEM(t), 0o600))
			}
			_, err := loadString(t, content+tt.extra())
			if got := errorTexts(err); !slices.Equal(got, tt.want) {
				t.Errorf("Load errors:\n got %q\nwant %q", got, tt.want)
			}
		})
	}

	t.Run("no [github] and every target has a token-file loads", func(t *testing.T) {
		content := strings.Replace(githubTOML(""), `url = "https://github.com/acme/widget"`+"\n",
			`url = "https://github.com/acme/widget"`+"\n"+tokenLine(token()), 1)
		content += targetTOML("https://github.com/acme/second", tokenLine(token()))
		if _, err := loadString(t, content); err != nil {
			t.Fatalf("Load: %v", err)
		}
	})

	t.Run("no [github] and every target has its own credential loads", func(t *testing.T) {
		content := strings.Replace(githubTOML(""), `url = "https://github.com/acme/widget"`+"\n",
			`url = "https://github.com/acme/widget"`+"\n"+`app-client-id = "own-id"`+"\n"+`app-private-key-file = "`+key()+`"`+"\n", 1)
		content += targetTOML("https://github.com/acme/second", tokenLine(token()))
		cfg := inspectConfig(t, content)
		if cfg.GitHub != (GitHub{}) {
			t.Errorf("github = %+v, want empty", cfg.GitHub)
		}
		if cfg.Targets[0].AppClientID != "own-id" || cfg.Targets[0].AppPrivateKeyFile == "" || cfg.Targets[1].AppClientID != "" {
			t.Errorf("targets = %+v, want the App keys on target 0 only", cfg.Targets)
		}
		if _, err := loadString(t, content); err != nil {
			t.Fatalf("Load: %v", err)
		}
	})

	t.Run("no [github] and only App targets loads", func(t *testing.T) {
		content := strings.Replace(githubTOML(""), `url = "https://github.com/acme/widget"`+"\n",
			`url = "https://github.com/acme/widget"`+"\n"+`app-client-id = "own-id"`+"\n"+`app-private-key-file = "`+key()+`"`+"\n", 1)
		content += appTarget("https://github.com/acme/second", "other-id", key())
		if _, err := loadString(t, content); err != nil {
			t.Fatalf("Load: %v", err)
		}
	})

	t.Run("default half pair is still refused", func(t *testing.T) {
		content := githubTOML(`app-client-id = "id"`) + appTarget("https://github.com/acme/second", "other-id", key())
		_, err := loadString(t, content)
		want := []string{"github.app-private-key-file: is required"}
		if got := errorTexts(err); !slices.Equal(got, want) {
			t.Errorf("Load errors:\n got %q\nwant %q", got, want)
		}
	})

	t.Run("a key that failed to decode counts as written", func(t *testing.T) {
		content := requiredTOML(key()) + "\n[[targets]]\nurl = \"https://github.com/acme/second\"\napp-client-id = 5\napp-private-key-file = \"" + key() + "\"\n"
		_, err := loadString(t, content)
		if err == nil || !strings.Contains(err.Error(), "targets[1].app-client-id") {
			t.Fatalf("Load error = %v, want the type error of app-client-id", err)
		}
		if strings.Contains(err.Error(), "is required") {
			t.Errorf("Load error = %v, want no \"is required\" for a key that failed to decode", err)
		}
	})
}

// TestInspectTargetAppKeyFile proves that a target's App key file result
// is in Files under its own key and that no file result is among the other
// problems.
func TestInspectTargetAppKeyFile(t *testing.T) {
	content := requiredTOML(writeKey(t, pkcs1PEM(t), 0o600)) +
		appTarget("https://github.com/acme/second", "other-id", writeKey(t, []byte("garbage"), 0o600)) +
		appTarget("https://github.com/acme/third", "third-id", writeKey(t, pkcs1PEM(t), 0o600))
	in, err := Inspect(writeFile(t, "config.toml", []byte(content), 0o600))
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	var problems []string
	for _, e := range in.Problems {
		problems = append(problems, e.Error())
	}
	if len(problems) != 0 {
		t.Errorf("Problems = %q, want none", problems)
	}
	var got []string
	for _, f := range in.Files {
		got = append(got, fmt.Sprintf("%s %v", f.Key, f.Err))
	}
	want := []string{
		"github.app-private-key-file <nil>",
		"targets[1].app-private-key-file no PEM block found",
		"targets[2].app-private-key-file <nil>",
	}
	if !slices.Equal(got, want) {
		t.Errorf("Files:\n got %q\nwant %q", got, want)
	}
}

// TestLoadCredentialsTargetApp proves that LoadCredentials reads each
// target's App key, exposes it by target index, keeps it apart from the
// default App key and the tokens, and names a broken file by its key.
func TestLoadCredentialsTargetApp(t *testing.T) {
	token := fixtureToken("ghp_")
	tokenPath := writeToken(t, token+"\n", 0o600)

	t.Run("keys by target index", func(t *testing.T) {
		content := githubTOML("") // no default credential
		content = strings.Replace(content, `url = "https://github.com/acme/widget"`+"\n",
			`url = "https://github.com/acme/widget"`+"\n"+`app-client-id = "own-id"`+"\n"+`app-private-key-file = "`+writeKey(t, pkcs8PEM(t), 0o400)+`"`+"\n", 1)
		content += targetTOML("https://github.com/acme/second", tokenLine(tokenPath))
		content += appTarget("https://github.com/acme/third", "third-id", writeKey(t, pkcs1PEM(t), 0o700))
		creds, err := LoadCredentials(inspectConfig(t, content))
		if err != nil {
			t.Fatalf("LoadCredentials: %v", err)
		}
		if creds.AppKey != nil || creds.DefaultToken != "" {
			t.Error("credentials hold a default credential the file does not have")
		}
		if len(creds.TargetAppKeys) != 3 || creds.TargetAppKeys[0] == nil || creds.TargetAppKeys[1] != nil || creds.TargetAppKeys[2] == nil {
			t.Fatalf("TargetAppKeys = %v, want keys at index 0 and 2 only", creds.TargetAppKeys)
		}
		if !creds.TargetAppKeys[0].Equal(rsaKey(t)) || !creds.TargetAppKeys[2].Equal(rsaKey(t)) {
			t.Error("TargetAppKeys are not the files' keys")
		}
		if !slices.Equal(creds.TargetTokens, []string{"", token, ""}) {
			t.Error("TargetTokens are not the token files' tokens in target order")
		}
	})

	t.Run("default App and a target App", func(t *testing.T) {
		cfg := inspectConfig(t, requiredTOML(writeKey(t, pkcs1PEM(t), 0o600))+
			appTarget("https://github.com/acme/second", "other-id", writeKey(t, pkcs1PEM(t), 0o600)))
		creds, err := LoadCredentials(cfg)
		if err != nil {
			t.Fatalf("LoadCredentials: %v", err)
		}
		if creds.AppKey == nil || len(creds.TargetAppKeys) != 2 || creds.TargetAppKeys[0] != nil || creds.TargetAppKeys[1] == nil {
			t.Errorf("credentials = default key %v, target keys %v", creds.AppKey != nil, creds.TargetAppKeys)
		}
	})

	t.Run("file errors by key", func(t *testing.T) {
		wide := writeKey(t, pkcs1PEM(t), 0o644)
		cfg := Config{
			GitHub: GitHub{TokenFile: tokenPath},
			Targets: []Target{
				{},
				{AppClientID: "id", AppPrivateKeyFile: wide},
				{AppClientID: "id", AppPrivateKeyFile: "app.pem", TokenFile: writeToken(t, token, 0o644)},
			},
		}
		creds, err := LoadCredentials(&cfg)
		want := []string{
			"targets[1].app-private-key-file: file mode -rw-r--r-- is wider than 0600 (group or other bits are set); use 0600 or 0400",
			"targets[2].app-private-key-file: stat file: no such file or directory",
			"targets[2].token-file: file mode -rw-r--r-- is wider than 0600 (group or other bits are set); use 0600 or 0400",
		}
		if got := errorTexts(err); !slices.Equal(got, want) {
			t.Errorf("LoadCredentials errors:\n got %q\nwant %q", got, want)
		}
		if creds.AppKey != nil || creds.TargetAppKeys != nil || creds.TargetTokens != nil {
			t.Error("LoadCredentials returned credentials with an error")
		}
		if strings.Contains(err.Error(), wide) {
			t.Error("an error holds the key file path")
		}
	})
}

// TestCredentialsRedactTargetAppKeys proves that no way of printing a
// Credentials shows an App private key, the default one or a target's.
func TestCredentialsRedactTargetAppKeys(t *testing.T) {
	key := rsaKey(t)
	creds := Credentials{AppKey: key, TargetAppKeys: []*rsa.PrivateKey{nil, key}, TargetTokens: []string{"", ""}}

	var out bytes.Buffer
	fmt.Fprintf(&out, "%v %+v %#v %s %q %d %x\n", creds, creds, creds, creds, creds, creds, creds)
	fmt.Fprintf(&out, "%v %+v %#v\n", &creds, struct{ C Credentials }{creds}, []Credentials{creds})
	slog.New(slog.NewJSONHandler(&out, nil)).Info("loaded", "credentials", creds, "pointer", &creds, "nested", struct{ C Credentials }{creds})
	slog.New(slog.NewTextHandler(&out, nil)).Info("loaded", "credentials", creds, "nested", struct{ C Credentials }{creds})

	text := out.String()
	for _, secret := range []string{key.D.String(), key.N.String(), key.Primes[0].String()} {
		if strings.Contains(text, secret) {
			t.Fatalf("printed credentials hold key material:\n%.200s", text)
		}
	}
	if strings.Contains(text, "TargetAppKeys") || !strings.Contains(text, "<redacted>") {
		t.Fatalf("printed credentials show their fields or lack <redacted>:\n%s", text)
	}
}

// TestExampleDocumentsEveryNewKey keeps config.example.toml in step with
// the keys the convention fixes added: each is written there with its
// default and a comment.
func TestExampleDocumentsEveryNewKey(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "config.example.toml"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	for _, line := range []string{
		`server-limit = 0 # default;`,
		`host = "unix:///var/run/docker.sock" # default;`,
		`level = "info" # default;`,
	} {
		if !strings.Contains(text, line) {
			t.Errorf("example lacks %q", line)
		}
	}
}

// TestValidateClientID proves that validation applies CheckAppClientID to
// github.app-client-id and every targets[i].app-client-id, without quoting
// the value, while a blank default client ID keeps the 0002 "is required".
func TestValidateClientID(t *testing.T) {
	key := writeKey(t, pkcs1PEM(t), 0o600)
	token := fixtureToken("ghp_")
	withDefault := func(id string) string {
		return strings.Replace(requiredTOML(key), `app-client-id = "test-client-id"`, `app-client-id = "`+id+`"`, 1)
	}
	tests := []struct {
		name    string
		content string
		want    []string
		hidden  string
	}{
		{"default with spaces around", withDefault(" Iv1.abc "),
			[]string{"github.app-client-id: must not contain spaces or control characters"}, ""},
		{"default is a token", withDefault(token),
			[]string{"github.app-client-id: looks like a token, not a GitHub App client ID"}, token},
		{"default blank", withDefault("  "), []string{"github.app-client-id: is required"}, ""},
		{"target with a tab", requiredTOML(key) + appTarget("https://github.com/acme/second", `Iv1\tx`, key),
			[]string{"targets[1].app-client-id: must not contain spaces or control characters"}, ""},
		{"target is a token", requiredTOML(key) + appTarget("https://github.com/acme/second", token, key),
			[]string{"targets[1].app-client-id: looks like a token, not a GitHub App client ID"}, token},
		{"target blank", requiredTOML(key) + appTarget("https://github.com/acme/second", " ", key),
			[]string{"targets[1].app-client-id: is required"}, ""},
		{"target fine", requiredTOML(key) + appTarget("https://github.com/acme/second", "Iv1.own", key), nil, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := loadString(t, tt.content)
			if got := errorTexts(err); !slices.Equal(got, tt.want) {
				t.Errorf("Load errors:\n got %q\nwant %q", got, tt.want)
			}
			if tt.hidden != "" && err != nil && strings.Contains(err.Error(), tt.hidden) {
				t.Error("an error quotes the client ID")
			}
		})
	}
}

func TestCheckAppClientID(t *testing.T) {
	for _, tt := range []struct {
		id   string
		want string
	}{
		{"Iv1.0123456789abcdef", ""},
		{"12345", ""},
		{"", "must not be empty"},
		{"\xff", "valid UTF-8"},
		{" Iv1.x", "spaces"},
		{"Iv1.x ", "spaces"},
		{"Iv1\nx", "spaces or control"},
		{fixtureToken("github_pat_"), "looks like a token"},
		{"x@y", "looks like a token"},
	} {
		err := CheckAppClientID(tt.id)
		switch {
		case tt.want == "" && err != nil:
			t.Errorf("CheckAppClientID(%q) = %v, want nil", tt.id, err)
		case tt.want != "" && (err == nil || !strings.Contains(err.Error(), tt.want)):
			t.Errorf("CheckAppClientID(%q) = %v, want %q", tt.id, err, tt.want)
		}
	}
}

// TestCredentialSource proves the order a target picks its credential in,
// and that a key failed reports counts as written.
func TestCredentialSource(t *testing.T) {
	app := GitHub{AppClientID: "id", AppPrivateKeyFile: "/k"}
	token := GitHub{TokenFile: "/t"}
	tests := []struct {
		name   string
		github GitHub
		target Target
		failed string
		want   CredentialSource
	}{
		{"own token over everything", app, Target{TokenFile: "/own", AppClientID: "own"}, "", OwnToken},
		{"own App over the default token", token, Target{AppClientID: "own"}, "", OwnApp},
		{"own key file alone is an own App", token, Target{AppPrivateKeyFile: "/own.pem"}, "", OwnApp},
		{"default token over the default App", GitHub{TokenFile: "/t", AppClientID: "id"}, Target{}, "", DefaultToken},
		{"default App", app, Target{}, "", DefaultApp},
		{"none", GitHub{}, Target{}, "", NoCredential},
		{"own token that failed to decode", app, Target{}, "targets[0].token-file", OwnToken},
		{"own client ID that failed to decode", token, Target{}, "targets[0].app-client-id", OwnApp},
		{"default token that failed to decode", app, Target{}, "github.token-file", DefaultToken},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := Config{GitHub: tt.github, Targets: []Target{tt.target}}
			failed := func(key string) bool { return key == tt.failed }
			if got := c.CredentialSource(0, failed); got != tt.want {
				t.Errorf("CredentialSource = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestCredentialsTarget proves that Credentials.Target returns the secret
// of the source CredentialSource picks, and an error, never another
// credential, when that secret is missing or the lists do not match.
func TestCredentialsTarget(t *testing.T) {
	defaultKey, ownKey := rsaKey(t), &rsa.PrivateKey{}
	def, own := fixtureToken("ghp_"), fixtureToken("github_pat_")
	cfg := &Config{
		GitHub: GitHub{AppClientID: "default-id", AppPrivateKeyFile: "/k"},
		Targets: []Target{
			{TokenFile: "/own"},
			{AppClientID: "own-id", AppPrivateKeyFile: "/own.pem"},
			{},
		},
	}
	creds := Credentials{AppKey: defaultKey, TargetTokens: []string{own, "", ""}, TargetAppKeys: []*rsa.PrivateKey{nil, ownKey, nil}}
	want := []TargetCredential{
		{Source: OwnToken, Token: own},
		{Source: OwnApp, AppClientID: "own-id", AppKey: ownKey},
		{Source: DefaultApp, AppClientID: "default-id", AppKey: defaultKey},
	}
	for i := range cfg.Targets {
		got, err := creds.Target(cfg, i)
		if err != nil || got != want[i] {
			t.Errorf("Target(%d) = %v source %v, %v; want source %v", i, got.Source == want[i].Source, got.Source, err, want[i].Source)
		}
	}

	tokenCfg := *cfg
	tokenCfg.GitHub = GitHub{TokenFile: "/t"}
	if got, err := (Credentials{DefaultToken: def}).Target(&tokenCfg, 2); err != nil || got.Token != def || got.Source != DefaultToken {
		t.Errorf("Target with a default token = source %v, %v", got.Source, err)
	}

	for name, tt := range map[string]struct {
		creds Credentials
		index int
		want  string
	}{
		"own App key missing": {Credentials{AppKey: defaultKey}, 1, "targets[1]: the secret of targets.app-client-id and targets.app-private-key-file is not loaded"},
		"own token missing":   {Credentials{DefaultToken: def, AppKey: defaultKey}, 0, "targets[0]: the secret of targets.token-file is not loaded"},
		"default App missing": {Credentials{}, 2, "targets[2]: the secret of github.app-client-id and github.app-private-key-file is not loaded"},
		"misaligned tokens":   {Credentials{AppKey: defaultKey, TargetTokens: []string{own}}, 2, "credentials do not match the configured targets"},
		"misaligned keys":     {Credentials{AppKey: defaultKey, TargetAppKeys: []*rsa.PrivateKey{nil}}, 2, "credentials do not match the configured targets"},
		"no such target":      {creds, 3, "no configured target 3"},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := tt.creds.Target(cfg, tt.index)
			if err == nil || err.Error() != tt.want {
				t.Errorf("Target = %v, want %q", err, tt.want)
			}
			if got != (TargetCredential{}) {
				t.Error("Target returned a credential with its error")
			}
			if err != nil && (strings.Contains(err.Error(), def) || strings.Contains(err.Error(), own)) {
				t.Error("the error holds a token")
			}
		})
	}
	if _, err := (Credentials{}).Target(&Config{Targets: []Target{{}}}, 0); err == nil || err.Error() != "targets[0] has no credential" {
		t.Errorf("Target without any credential = %v", err)
	}

	got, _ := creds.Target(cfg, 0)
	var out bytes.Buffer
	fmt.Fprintf(&out, "%v %+v %#v %s %q", got, got, got, got, got)
	slog.New(slog.NewJSONHandler(&out, nil)).Info("x", "cred", got, "nested", struct{ C TargetCredential }{got})
	if strings.Contains(out.String(), own) {
		t.Errorf("a printed TargetCredential holds the token:\n%s", out.String())
	}
}
