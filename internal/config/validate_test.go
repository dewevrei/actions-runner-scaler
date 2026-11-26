package config

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"io"
	"math"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/dewevrei/actions-runner-scaler/internal/github"
)

func validConfig(t *testing.T) Config {
	t.Helper()
	cfg := Default()
	cfg.GitHub = GitHub{
		AppClientID:       "test-client-id",
		AppPrivateKeyFile: writeKey(t, pkcs1PEM(t), 0o600),
	}
	cfg.Runner.Image = testRunnerImage
	cfg.BuildKit.Image = testBuildKitImage
	target := DefaultTarget()
	target.URL = "https://github.com/test-org"
	cfg.Targets = []Target{target}
	return cfg
}

func TestValidateAcceptsValidConfig(t *testing.T) {
	cfg := validConfig(t)
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
}

// Each case changes one value; an invalid value must produce exactly one
// error naming its key, and a valid boundary value none.
func TestValidateRanges(t *testing.T) {
	hex64 := strings.Repeat("c", 64)
	target := func(f func(*Target)) func(*Config) {
		return func(c *Config) { f(&c.Targets[0]) }
	}
	tests := []struct {
		name   string
		key    string // empty when the value is valid
		modify func(*Config)
	}{
		{"client id empty", "github.app-client-id", func(c *Config) { c.GitHub.AppClientID = "" }},
		{"client id blank", "github.app-client-id", func(c *Config) { c.GitHub.AppClientID = "  " }},
		{"key file empty", "github.app-private-key-file", func(c *Config) { c.GitHub.AppPrivateKeyFile = "" }},
		{"key file relative", "github.app-private-key-file", func(c *Config) { c.GitHub.AppPrivateKeyFile = "app.pem" }},
		{"key file dot relative", "github.app-private-key-file", func(c *Config) { c.GitHub.AppPrivateKeyFile = "./keys/app.pem" }},
		{"key file home relative", "github.app-private-key-file", func(c *Config) { c.GitHub.AppPrivateKeyFile = "~/app.pem" }},

		{"runner image ID", "", func(c *Config) { c.Runner.Image = "sha256:" + hex64 }},
		{"runner image empty", "runner.image", func(c *Config) { c.Runner.Image = "" }},
		{"runner image 63 hex", "runner.image", func(c *Config) { c.Runner.Image = "sha256:" + hex64[1:] }},
		{"runner image upper hex", "runner.image", func(c *Config) { c.Runner.Image = "sha256:" + strings.ToUpper(hex64) }},
		{"runner image tag", "runner.image", func(c *Config) { c.Runner.Image = "runner:latest" }},
		{"buildkit image", "", func(c *Config) { c.BuildKit.Image = "moby/buildkit:v0.34.2-rootless@sha256:" + hex64 }},
		{"buildkit image empty", "buildkit.image", func(c *Config) { c.BuildKit.Image = "" }},
		{"buildkit no digest", "buildkit.image", func(c *Config) { c.BuildKit.Image = "moby/buildkit:v0.34.2-rootless" }},
		{"buildkit not rootless", "buildkit.image", func(c *Config) { c.BuildKit.Image = "moby/buildkit:v0.34.2@sha256:" + hex64 }},
		{"buildkit no version", "buildkit.image", func(c *Config) { c.BuildKit.Image = "moby/buildkit:rootless@sha256:" + hex64 }},
		{"buildkit other repo", "buildkit.image", func(c *Config) { c.BuildKit.Image = "other/buildkit:v1-rootless@sha256:" + hex64 }},

		{"listen IPv4 loopback", "", func(c *Config) { c.JobCheck.Listen = "127.0.0.1:8765" }},
		{"listen IPv6 loopback", "", func(c *Config) { c.JobCheck.Listen = "[::1]:8765" }},
		{"listen 127/8", "", func(c *Config) { c.JobCheck.Listen = "127.0.0.2:8765" }},
		{"listen port 1", "", func(c *Config) { c.JobCheck.Listen = "127.0.0.1:1" }},
		{"listen port 65535", "", func(c *Config) { c.JobCheck.Listen = "127.0.0.1:65535" }},
		{"listen port 0", "jobcheck.listen", func(c *Config) { c.JobCheck.Listen = "127.0.0.1:0" }},
		{"listen port 65536", "jobcheck.listen", func(c *Config) { c.JobCheck.Listen = "127.0.0.1:65536" }},
		{"listen all interfaces", "jobcheck.listen", func(c *Config) { c.JobCheck.Listen = "0.0.0.0:8765" }},
		{"listen empty host", "jobcheck.listen", func(c *Config) { c.JobCheck.Listen = ":8765" }},
		{"listen hostname", "jobcheck.listen", func(c *Config) { c.JobCheck.Listen = "localhost:8765" }},
		{"listen no port", "jobcheck.listen", func(c *Config) { c.JobCheck.Listen = "127.0.0.1" }},

		{"drain timeout 1ns", "", func(c *Config) { c.Shutdown.DrainTimeout = 1 }},
		{"drain timeout 0", "shutdown.drain-timeout", func(c *Config) { c.Shutdown.DrainTimeout = 0 }},
		{"drain timeout negative", "shutdown.drain-timeout", func(c *Config) { c.Shutdown.DrainTimeout = -1 }},

		{"label 1 char", "", func(c *Config) { c.Scaling.Label = "a" }},
		{"label 40 chars", "", func(c *Config) { c.Scaling.Label = strings.Repeat("a", 40) }},
		{"label digits and dash", "", func(c *Config) { c.Scaling.Label = "pool-2" }},
		{"label 41 chars", "scaling.label", func(c *Config) { c.Scaling.Label = strings.Repeat("a", 41) }},
		{"label empty", "scaling.label", func(c *Config) { c.Scaling.Label = "" }},
		{"label upper case", "scaling.label", func(c *Config) { c.Scaling.Label = "Pool" }},
		{"label underscore", "scaling.label", func(c *Config) { c.Scaling.Label = "a_b" }},
		{"label leading dash", "scaling.label", func(c *Config) { c.Scaling.Label = "-pool" }},
		{"label only dash", "scaling.label", func(c *Config) { c.Scaling.Label = "-" }},
		{"label leading digit", "", func(c *Config) { c.Scaling.Label = "2pool" }},

		{"max-runners 1", "", target(func(p *Target) { p.MaxRunners = 1 })},
		{"max-runners 0", "targets[0].max-runners", target(func(p *Target) { p.MaxRunners, p.Idle = 0, 0 })},
		{"max-runners large", "", target(func(p *Target) { p.MaxRunners = 1000 })},
		{"idle 0", "", target(func(p *Target) { p.Idle = 0 })},
		{"idle equals max-runners", "", target(func(p *Target) { p.Idle = p.MaxRunners })},
		{"idle above max-runners", "targets[0].idle", target(func(p *Target) { p.Idle = p.MaxRunners + 1 })},
		{"idle negative", "targets[0].idle", target(func(p *Target) { p.Idle = -1 })},

		{"runner group empty", "", target(func(p *Target) { p.RunnerGroup = "" })},
		{"runner group 1 char", "", target(func(p *Target) { p.RunnerGroup = "g" })},
		{"runner group 255 chars", "", target(func(p *Target) { p.RunnerGroup = strings.Repeat("g", 255) })},
		{"runner group 255 multibyte chars", "", target(func(p *Target) { p.RunnerGroup = strings.Repeat("그", 255) })},
		{"runner group 256 chars", "targets[0].runner-group", target(func(p *Target) { p.RunnerGroup = strings.Repeat("g", 256) })},

		{"runner cpus small", "", func(c *Config) { c.Runner.CPUs = 0.01 }},
		{"runner cpus below 0.01", "runner.cpus", func(c *Config) { c.Runner.CPUs = 0.009 }},
		{"runner cpus 9e9", "", func(c *Config) { c.Runner.CPUs = 9e9 }},
		{"runner cpus largest", "", func(c *Config) { c.Runner.CPUs = math.Nextafter(maxCPUs, 0) }},
		{"runner cpus MaxInt64/1e9", "runner.cpus", func(c *Config) { c.Runner.CPUs = maxCPUs }},
		{"runner cpus overflow nanocpus", "runner.cpus", func(c *Config) { c.Runner.CPUs = 1e10 }},
		{"runner cpus 0", "runner.cpus", func(c *Config) { c.Runner.CPUs = 0 }},
		{"runner cpus negative", "runner.cpus", func(c *Config) { c.Runner.CPUs = -1 }},
		{"runner cpus inf", "runner.cpus", func(c *Config) { c.Runner.CPUs = math.Inf(1) }},
		{"runner cpus nan", "runner.cpus", func(c *Config) { c.Runner.CPUs = math.NaN() }},
		{"runner memory 256m", "", func(c *Config) { c.Runner.Memory = 256 * Mebibyte }},
		{"runner memory below 256m", "runner.memory", func(c *Config) { c.Runner.Memory = 256*Mebibyte - 1 }},
		{"runner pids 64", "", func(c *Config) { c.Runner.Pids = 64 }},
		{"runner pids 63", "runner.pids", func(c *Config) { c.Runner.Pids = 63 }},

		{"buildkit cpus small", "", func(c *Config) { c.BuildKit.CPUs = 0.01 }},
		{"buildkit cpus below 0.01", "buildkit.cpus", func(c *Config) { c.BuildKit.CPUs = 0.009 }},
		{"buildkit cpus 9e9", "", func(c *Config) { c.BuildKit.CPUs = 9e9 }},
		{"buildkit cpus largest", "", func(c *Config) { c.BuildKit.CPUs = math.Nextafter(maxCPUs, 0) }},
		{"buildkit cpus MaxInt64/1e9", "buildkit.cpus", func(c *Config) { c.BuildKit.CPUs = maxCPUs }},
		{"buildkit cpus overflow nanocpus", "buildkit.cpus", func(c *Config) { c.BuildKit.CPUs = 1e10 }},
		{"buildkit cpus 0", "buildkit.cpus", func(c *Config) { c.BuildKit.CPUs = 0 }},
		{"buildkit cpus negative", "buildkit.cpus", func(c *Config) { c.BuildKit.CPUs = -1 }},
		{"buildkit cpus inf", "buildkit.cpus", func(c *Config) { c.BuildKit.CPUs = math.Inf(1) }},
		{"buildkit cpus nan", "buildkit.cpus", func(c *Config) { c.BuildKit.CPUs = math.NaN() }},
		{"buildkit memory 256m", "", func(c *Config) { c.BuildKit.Memory = 256 * Mebibyte }},
		{"buildkit memory below 256m", "buildkit.memory", func(c *Config) { c.BuildKit.Memory = 256*Mebibyte - 1 }},
		{"buildkit pids 64", "", func(c *Config) { c.BuildKit.Pids = 64 }},
		{"buildkit pids 63", "buildkit.pids", func(c *Config) { c.BuildKit.Pids = 63 }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := validConfig(t)
			tt.modify(&cfg)
			err := cfg.Validate()
			if tt.key == "" {
				if err != nil {
					t.Fatalf("Validate: %v", err)
				}
				return
			}
			errs := errorList(err)
			if len(errs) != 1 || !strings.HasPrefix(errs[0].Error(), tt.key+":") {
				t.Fatalf("Validate error = %v, want one error for %s", err, tt.key)
			}
		})
	}
}

// TestValidateTargetURL proves 0002 SPEC R106.
func TestValidateTargetURL(t *testing.T) {
	for url, ok := range targetURLCases {
		cfg := validConfig(t)
		cfg.Targets[0].URL = url
		err := cfg.Validate()
		if ok != (err == nil) {
			t.Errorf("url %q: Validate = %v, want accepted = %v", url, err, ok)
			continue
		}
		if !ok && !strings.HasPrefix(err.Error(), "targets[0].url:") {
			t.Errorf("url %q: error = %v, want one for targets[0].url", url, err)
		}
	}
}

// TestTargetURLRuleMatchesGitHub proves 0002 SPEC R106: this package's rule
// and github.ParseTargetURL give the same answer.
func TestTargetURLRuleMatchesGitHub(t *testing.T) {
	for url := range targetURLCases {
		_, err := github.ParseTargetURL(url)
		if got := targetURLPattern.MatchString(url); got != (err == nil) {
			t.Errorf("url %q: config accepts %v, github.ParseTargetURL error = %v", url, got, err)
		}
	}
}

var targetURLCases = map[string]bool{
	"https://github.com/acme-1":        true,
	"https://github.com/Acme":          true,
	"https://github.com/a":             true,
	"https://github.com/acme/":         false,
	"https://github.com/acme.git":      false,
	"https://github.com/acme/repo":     false,
	"https://github.com/acme/repo.git": false,
	"http://github.com/acme":           false,
	"https://github.com/":              false,
	"https://github.com":               false,
	"https://github.com/acme_corp":     false,
	"https://github.com/acme?x=1":      false,
	"https://github.com/acme#x":        false,
	"https://github.com/acme ":         false,
	"https://example.com/acme":         false,
	"https://github.com/acme\n":        false,
	"":                                 false,
}

func TestValidateChecksEveryTarget(t *testing.T) {
	cfg := validConfig(t)
	second := cfg.Targets[0]
	second.URL = "https://github.com/other-org"
	second.MaxRunners = 0
	cfg.Targets = append(cfg.Targets, second)
	err := cfg.Validate()
	if errs := errorList(err); len(errs) != 1 || !strings.Contains(err.Error(), "targets[1].max-runners") {
		t.Fatalf("Validate error = %v, want only targets[1].max-runners", err)
	}
}

// TestValidateDuplicateOrganization proves 0002 SPEC R166: only the
// overlapping values are listed, in one error, whatever the case.
func TestValidateDuplicateOrganization(t *testing.T) {
	cfg := validConfig(t)
	cfg.Targets[0].URL = "https://github.com/Acme"
	for _, url := range []string{"https://github.com/solo", "https://github.com/ACME", "https://github.com/acme"} {
		extra := cfg.Targets[0]
		extra.URL = url
		cfg.Targets = append(cfg.Targets, extra)
	}
	errs := errorList(cfg.Validate())
	const want = "targets: organization listed more than once (names compare without case): https://github.com/Acme, https://github.com/ACME, https://github.com/acme"
	if len(errs) != 1 || errs[0].Error() != want {
		t.Fatalf("Validate errors = %v, want only %q", errs, want)
	}
}

func TestReadAppPrivateKey(t *testing.T) {
	ecDER, err := func() ([]byte, error) {
		k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			return nil, err
		}
		return x509.MarshalPKCS8PrivateKey(k)
	}()
	if err != nil {
		t.Fatalf("generate EC key: %v", err)
	}
	pkcs1 := pkcs1PEM(t)
	truncated := append(bytes.Clone(pkcs1[:len(pkcs1)/2]), []byte("\n-----END RSA PRIVATE KEY-----\n")...)
	corrupt := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: []byte("not a key")})

	tests := []struct {
		name    string
		data    []byte
		mode    os.FileMode
		wantErr int // number of errors expected
	}{
		{name: "PKCS#1 0600", data: pkcs1, mode: 0o600},
		{name: "PKCS#1 0400", data: pkcs1, mode: 0o400},
		{name: "PKCS#8 0600", data: pkcs8PEM(t), mode: 0o600},
		{name: "PKCS#8 0400", data: pkcs8PEM(t), mode: 0o400},
		{name: "0644", data: pkcs1, mode: 0o644, wantErr: 1},
		{name: "0640", data: pkcs1, mode: 0o640, wantErr: 1},
		{name: "0604", data: pkcs1, mode: 0o604, wantErr: 1},
		{name: "0610", data: pkcs1, mode: 0o610, wantErr: 1},
		{name: "0700", data: pkcs1, mode: 0o700, wantErr: 1},
		{name: "0500", data: pkcs1, mode: 0o500, wantErr: 1},
		{name: "setuid 0600", data: pkcs1, mode: 0o600 | os.ModeSetuid, wantErr: 1},
		{name: "not PEM", data: []byte("not a key at all"), mode: 0o600, wantErr: 1},
		{name: "empty", data: nil, mode: 0o600, wantErr: 1},
		{name: "truncated PEM", data: truncated, mode: 0o600, wantErr: 1},
		{name: "corrupt PKCS#1 body", data: corrupt, mode: 0o600, wantErr: 1},
		{name: "EC key in PKCS#8", data: pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: ecDER}), mode: 0o600, wantErr: 1},
		{name: "EC PEM type", data: pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: ecDER}), mode: 0o600, wantErr: 1},
		{name: "0644 and not PEM", data: []byte("garbage"), mode: 0o644, wantErr: 2},
		{name: "too large", data: bytes.Repeat([]byte("a"), maxKeyFileSize+1), mode: 0o600, wantErr: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := writeKey(t, tt.data, tt.mode)
			key, err := ReadAppPrivateKey(path)
			if tt.wantErr == 0 {
				if err != nil || key == nil {
					t.Fatalf("ReadAppPrivateKey = %v, %v; want a key", key, err)
				}
				if !key.Equal(rsaKey(t)) {
					t.Fatal("ReadAppPrivateKey returned a different key")
				}
				return
			}
			if key != nil {
				t.Error("ReadAppPrivateKey returned a key together with an error")
			}
			if errs := errorList(err); len(errs) != tt.wantErr {
				t.Fatalf("ReadAppPrivateKey errors = %v, want %d", errs, tt.wantErr)
			}
			assertNoKeyMaterial(t, err.Error(), tt.data)
			if strings.Contains(err.Error(), path) {
				t.Errorf("error contains the key file path: %v", err)
			}
		})
	}
}

// A key pasted in place of the path must be rejected without echoing it.
func TestReadAppPrivateKeyRejectsKeyContentAsPath(t *testing.T) {
	pemText := string(pkcs1PEM(t))
	for name, value := range map[string]string{
		"full PEM":       pemText,
		"one line":       strings.ReplaceAll(pemText, "\n", ""),
		"body only":      strings.Join(strings.Split(pemText, "\n")[1:4], "\n"),
		"header only":    "-----BEGIN RSA PRIVATE KEY-----",
		"trailing break": "/tmp/app.pem\n",
	} {
		t.Run(name, func(t *testing.T) {
			_, err := ReadAppPrivateKey(value)
			if errs := errorList(err); len(errs) != 1 || !strings.Contains(err.Error(), "looks like key content") {
				t.Fatalf("ReadAppPrivateKey error = %v, want the key-content rejection", err)
			}
			assertNoKeyMaterial(t, err.Error(), []byte(pemText))
		})
	}
}

func TestLoadKeyContentAsPathIsNotEchoed(t *testing.T) {
	pemText := string(pkcs1PEM(t))
	content := strings.Replace(requiredTOML("PLACEHOLDER"), `"PLACEHOLDER"`, `"""`+"\n"+pemText+`"""`, 1)
	_, err := loadString(t, content)
	errs := errorList(err)
	if len(errs) != 1 || !strings.HasPrefix(errs[0].Error(), "github.app-private-key-file: ") {
		t.Fatalf("Load error = %v, want one github.app-private-key-file error", err)
	}
	assertNoKeyMaterial(t, err.Error(), []byte(pemText))
}

func TestReadAppPrivateKeyFileErrors(t *testing.T) {
	dir := t.TempDir()
	// Opening a FIFO without a writer would block Load forever.
	fifo := dir + "/fifo.pem"
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatalf("mkfifo: %v", err)
	}
	for name, path := range map[string]string{
		"missing":   dir + "/missing.pem",
		"directory": dir,
		"fifo":      fifo,
	} {
		_, err := ReadAppPrivateKey(path)
		if err == nil {
			t.Errorf("%s: ReadAppPrivateKey succeeded", name)
			continue
		}
		if strings.Contains(err.Error(), dir) {
			t.Errorf("%s: error contains the path: %v", name, err)
		}
	}
}

// TestOpenRegularFIFO checks the open itself, past the Stat in
// ReadAppPrivateKey: a path that became a FIFO with no writer after that
// Stat is rejected at once instead of blocking.
func TestOpenRegularFIFO(t *testing.T) {
	dir := t.TempDir()
	fifo := dir + "/fifo.pem"
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatalf("mkfifo: %v", err)
	}
	done := make(chan error, 1)
	go func() {
		f, _, err := openRegular(fifo)
		if f != nil {
			f.Close()
		}
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil || err.Error() != "not a regular file" {
			t.Errorf("openRegular(fifo) = %v, want not a regular file", err)
		}
	case <-time.After(5 * time.Second):
		// The goroutine stays blocked; opening the FIFO for writing
		// would release it, but the test has already failed.
		t.Fatal("openRegular blocked on a FIFO")
	}
}

func TestOpenRegularFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "key.pem")
	want := bytes.Repeat([]byte("x"), 3*1024)
	if err := os.WriteFile(path, want, 0o600); err != nil {
		t.Fatal(err)
	}
	f, info, err := openRegular(path)
	if err != nil {
		t.Fatalf("openRegular: %v", err)
	}
	defer f.Close()
	if !info.Mode().IsRegular() || info.Size() != int64(len(want)) {
		t.Errorf("info = %v, %d bytes", info.Mode(), info.Size())
	}
	got, err := io.ReadAll(f)
	if err != nil || !bytes.Equal(got, want) {
		t.Errorf("read %d bytes, err %v; want %d bytes", len(got), err, len(want))
	}
	if _, _, err := openRegular(filepath.Join(t.TempDir(), "missing.pem")); err == nil || strings.Contains(err.Error(), "missing.pem") {
		t.Errorf("openRegular(missing) = %v, want an error without the path", err)
	}
}

// A relative key path is rejected without echoing it, since it may be a
// pasted secret (R31).
func TestValidateRelativeKeyPathIsNotEchoed(t *testing.T) {
	cfg := validConfig(t)
	const value = "ghs_fakeRelativeValue"
	cfg.GitHub.AppPrivateKeyFile = value
	errs := errorList(cfg.Validate())
	if len(errs) != 1 || errs[0].Error() != "github.app-private-key-file: must be an absolute path" {
		t.Fatalf("Validate errors = %v, want one absolute-path error", errs)
	}
	if strings.Contains(errs[0].Error(), value) {
		t.Errorf("error echoes the value: %v", errs[0])
	}
}

func TestValidateReportsKeyModeAndParseErrors(t *testing.T) {
	cfg := validConfig(t)
	cfg.GitHub.AppPrivateKeyFile = writeKey(t, []byte("garbage"), 0o644)
	errs := errorList(cfg.Validate())
	if len(errs) != 2 {
		t.Fatalf("Validate errors = %v, want 2", errs)
	}
	for _, e := range errs {
		if !strings.HasPrefix(e.Error(), "github.app-private-key-file: ") {
			t.Errorf("error %q is not reported under github.app-private-key-file", e)
		}
	}
}

// assertNoKeyMaterial fails if msg contains any base64 line of the PEM body.
func assertNoKeyMaterial(t *testing.T, msg string, data []byte) {
	t.Helper()
	for _, line := range strings.Split(string(data), "\n") {
		if len(line) >= 16 && !strings.HasPrefix(line, "-----") && strings.Contains(msg, line) {
			t.Fatalf("error message contains key material: %q", line)
		}
	}
}

// A symbolic link to a valid key file is read as its target.
func TestReadAppPrivateKeySymlink(t *testing.T) {
	target := writeKey(t, pkcs1PEM(t), 0o600)
	link := filepath.Join(t.TempDir(), "link.pem")
	if err := os.Symlink(target, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	if key, err := ReadAppPrivateKey(link); err != nil || key == nil {
		t.Fatalf("ReadAppPrivateKey(link) = %v, %v; want the target's key", key, err)
	}
}

// writeKeySpecial writes a valid key with mode and skips the test when the
// OS does not let this user set the special bit in mode. Unprivileged
// chmod may refuse setgid (EPERM when the file's group is not one of the
// user's, as on macOS for a file in a wheel-owned directory) or drop it
// silently (Linux clears setgid then), and some systems refuse sticky on
// a regular file (EFTYPE on BSD); the check needs the bit really set.
func writeKeySpecial(t *testing.T, mode os.FileMode) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "app.pem")
	if err := os.WriteFile(path, pkcs1PEM(t), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Skipf("chmod %v refused for this user: %v", mode, err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode() != mode {
		t.Skipf("chmod %v left mode %v; the OS dropped the special bit", mode, info.Mode())
	}
	return path
}

// R30: setgid and sticky are rejected like setuid, on 0600 and 0400.
func TestReadAppPrivateKeySpecialBits(t *testing.T) {
	for _, mode := range []os.FileMode{
		0o600 | os.ModeSetgid,
		0o400 | os.ModeSetgid,
		0o600 | os.ModeSticky,
		0o400 | os.ModeSticky,
	} {
		t.Run(mode.String(), func(t *testing.T) {
			path := writeKeySpecial(t, mode)
			key, err := ReadAppPrivateKey(path)
			if key != nil || err == nil {
				t.Fatalf("ReadAppPrivateKey = %v, %v; want the mode error", key, err)
			}
			if errs := errorList(err); len(errs) != 1 || !strings.Contains(err.Error(), "special bits") {
				t.Errorf("errors = %v, want one mode error", errs)
			}
		})
	}
}
