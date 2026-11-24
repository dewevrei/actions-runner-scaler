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
)

func validConfig(t *testing.T) Config {
	t.Helper()
	cfg := Default()
	cfg.GitHub = GitHub{
		AppClientID:       "test-client-id",
		AppInstallationID: 1,
		AppPrivateKeyFile: writeKey(t, pkcs1PEM(t), 0o600),
	}
	cfg.Runner.Image = testRunnerImage
	cfg.BuildKit.Image = testBuildKitImage
	pool := DefaultPool()
	pool.RepoURL = "https://github.com/test-org/test-repo"
	pool.AllowedWorkflows = []string{".github/workflows/deploy.yml"}
	cfg.Pools = []Pool{pool}
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
	pool := func(f func(*Pool)) func(*Config) {
		return func(c *Config) { f(&c.Pools[0]) }
	}
	tests := []struct {
		name   string
		key    string // empty when the value is valid
		modify func(*Config)
	}{
		{"client id empty", "github.app-client-id", func(c *Config) { c.GitHub.AppClientID = "" }},
		{"client id blank", "github.app-client-id", func(c *Config) { c.GitHub.AppClientID = "  " }},
		{"installation id 0", "github.app-installation-id", func(c *Config) { c.GitHub.AppInstallationID = 0 }},
		{"installation id -1", "github.app-installation-id", func(c *Config) { c.GitHub.AppInstallationID = -1 }},
		{"installation id 1", "", func(c *Config) { c.GitHub.AppInstallationID = 1 }},
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

		{"name 1 char", "", pool(func(p *Pool) { p.Name = "a" })},
		{"name 40 chars", "", pool(func(p *Pool) { p.Name = strings.Repeat("a", 40) })},
		{"name digits and dash", "", pool(func(p *Pool) { p.Name = "pool-2" })},
		{"name 41 chars", "pools[0].name", pool(func(p *Pool) { p.Name = strings.Repeat("a", 41) })},
		{"name empty", "pools[0].name", pool(func(p *Pool) { p.Name = "" })},
		{"name upper case", "pools[0].name", pool(func(p *Pool) { p.Name = "Pool" })},
		{"name underscore", "pools[0].name", pool(func(p *Pool) { p.Name = "a_b" })},
		{"name leading dash", "pools[0].name", pool(func(p *Pool) { p.Name = "-pool" })},
		{"name only dash", "pools[0].name", pool(func(p *Pool) { p.Name = "-" })},
		{"name leading digit", "", pool(func(p *Pool) { p.Name = "2pool" })},

		{"repo url dots", "", pool(func(p *Pool) { p.RepoURL = "https://github.com/test-org/test.repo_1" })},
		{"repo url empty", "pools[0].repo-url", pool(func(p *Pool) { p.RepoURL = "" })},
		{"repo url http", "pools[0].repo-url", pool(func(p *Pool) { p.RepoURL = "http://github.com/test-org/test-repo" })},
		{"repo url trailing slash", "pools[0].repo-url", pool(func(p *Pool) { p.RepoURL = "https://github.com/test-org/test-repo/" })},
		{"repo url org only", "pools[0].repo-url", pool(func(p *Pool) { p.RepoURL = "https://github.com/test-org" })},
		{"repo url extra path", "pools[0].repo-url", pool(func(p *Pool) { p.RepoURL = "https://github.com/test-org/test-repo/tree" })},
		{"repo url other host", "pools[0].repo-url", pool(func(p *Pool) { p.RepoURL = "https://example.com/test-org/test-repo" })},
		{"repo url dot repo", "pools[0].repo-url", pool(func(p *Pool) { p.RepoURL = "https://github.com/test-org/.." })},
		{"repo url .git", "pools[0].repo-url", pool(func(p *Pool) { p.RepoURL = "https://github.com/test-org/test-repo.git" })},
		{"repo url .GIT", "pools[0].repo-url", pool(func(p *Pool) { p.RepoURL = "https://github.com/test-org/test-repo.GIT" })},
		{"repo url git in name", "", pool(func(p *Pool) { p.RepoURL = "https://github.com/test-org/test.git-repo" })},

		{"max 1", "", pool(func(p *Pool) { p.Max = 1 })},
		{"max 0", "pools[0].max", pool(func(p *Pool) { p.Max, p.Idle = 0, 0 })},
		{"idle 0", "", pool(func(p *Pool) { p.Idle = 0 })},
		{"idle equals max", "", pool(func(p *Pool) { p.Idle = p.Max })},
		{"idle above max", "pools[0].idle", pool(func(p *Pool) { p.Idle = p.Max + 1 })},
		{"idle negative", "pools[0].idle", pool(func(p *Pool) { p.Idle = -1 })},

		{"runner cpus small", "", pool(func(p *Pool) { p.RunnerCPUs = 0.01 })},
		{"runner cpus below 0.01", "pools[0].runner-cpus", pool(func(p *Pool) { p.RunnerCPUs = 0.009 })},
		{"runner cpus 9e9", "", pool(func(p *Pool) { p.RunnerCPUs = 9e9 })},
		{"runner cpus largest", "", pool(func(p *Pool) { p.RunnerCPUs = math.Nextafter(maxCPUs, 0) })},
		{"runner cpus MaxInt64/1e9", "pools[0].runner-cpus", pool(func(p *Pool) { p.RunnerCPUs = maxCPUs })},
		{"runner cpus overflow nanocpus", "pools[0].runner-cpus", pool(func(p *Pool) { p.RunnerCPUs = 1e10 })},
		{"runner cpus 0", "pools[0].runner-cpus", pool(func(p *Pool) { p.RunnerCPUs = 0 })},
		{"runner cpus negative", "pools[0].runner-cpus", pool(func(p *Pool) { p.RunnerCPUs = -1 })},
		{"runner cpus inf", "pools[0].runner-cpus", pool(func(p *Pool) { p.RunnerCPUs = math.Inf(1) })},
		{"runner cpus nan", "pools[0].runner-cpus", pool(func(p *Pool) { p.RunnerCPUs = math.NaN() })},
		{"runner memory 256m", "", pool(func(p *Pool) { p.RunnerMemory = 256 * Mebibyte })},
		{"runner memory below 256m", "pools[0].runner-memory", pool(func(p *Pool) { p.RunnerMemory = 256*Mebibyte - 1 })},
		{"runner pids 64", "", pool(func(p *Pool) { p.RunnerPids = 64 })},
		{"runner pids 63", "pools[0].runner-pids", pool(func(p *Pool) { p.RunnerPids = 63 })},

		{"build cpus small", "", pool(func(p *Pool) { p.BuildCPUs = 0.01 })},
		{"build cpus below 0.01", "pools[0].build-cpus", pool(func(p *Pool) { p.BuildCPUs = 1e-10 })},
		{"build cpus 0", "pools[0].build-cpus", pool(func(p *Pool) { p.BuildCPUs = 0 })},
		{"build cpus 9e9", "", pool(func(p *Pool) { p.BuildCPUs = 9e9 })},
		{"build cpus MaxInt64/1e9", "pools[0].build-cpus", pool(func(p *Pool) { p.BuildCPUs = maxCPUs })},
		{"build cpus nan", "pools[0].build-cpus", pool(func(p *Pool) { p.BuildCPUs = math.NaN() })},
		{"build cpus inf", "pools[0].build-cpus", pool(func(p *Pool) { p.BuildCPUs = math.Inf(1) })},
		{"build memory 256m", "", pool(func(p *Pool) { p.BuildMemory = 256 * Mebibyte })},
		{"build memory below 256m", "pools[0].build-memory", pool(func(p *Pool) { p.BuildMemory = 256*Mebibyte - 1 })},
		{"build pids 64", "", pool(func(p *Pool) { p.BuildPids = 64 })},
		{"build pids 63", "pools[0].build-pids", pool(func(p *Pool) { p.BuildPids = 63 })},

		{"workflows two", "", pool(func(p *Pool) { p.AllowedWorkflows = []string{".github/workflows/a.yml", ".github/workflows/b.yaml"} })},
		{"workflows empty", "pools[0].allowed-workflows", pool(func(p *Pool) { p.AllowedWorkflows = nil })},
		{"workflow outside dir", "pools[0].allowed-workflows", pool(func(p *Pool) { p.AllowedWorkflows = []string{"deploy.yml"} })},
		{"workflow subdir", "pools[0].allowed-workflows", pool(func(p *Pool) { p.AllowedWorkflows = []string{".github/workflows/sub/a.yml"} })},
		{"workflow no file", "pools[0].allowed-workflows", pool(func(p *Pool) { p.AllowedWorkflows = []string{".github/workflows/"} })},
		{"workflow dot", "pools[0].allowed-workflows", pool(func(p *Pool) { p.AllowedWorkflows = []string{".github/workflows/."} })},
		{"workflow dot dot", "pools[0].allowed-workflows", pool(func(p *Pool) { p.AllowedWorkflows = []string{".github/workflows/.."} })},
		{"workflow extension only", "pools[0].allowed-workflows", pool(func(p *Pool) { p.AllowedWorkflows = []string{".github/workflows/.yml"} })},
		{"workflow no extension", "pools[0].allowed-workflows", pool(func(p *Pool) { p.AllowedWorkflows = []string{".github/workflows/deploy"} })},
		{"workflow json", "pools[0].allowed-workflows", pool(func(p *Pool) { p.AllowedWorkflows = []string{".github/workflows/deploy.json"} })},
		{"workflow upper extension", "pools[0].allowed-workflows", pool(func(p *Pool) { p.AllowedWorkflows = []string{".github/workflows/deploy.YML"} })},
		{"workflow with ref", "pools[0].allowed-workflows", pool(func(p *Pool) { p.AllowedWorkflows = []string{".github/workflows/deploy.yml@refs/heads/main"} })},
		{"workflow at sign", "pools[0].allowed-workflows", pool(func(p *Pool) { p.AllowedWorkflows = []string{".github/workflows/a@b.yml"} })},

		{"events push", "", pool(func(p *Pool) { p.AllowedEvents = []string{"push"} })},
		{"events dispatch", "", pool(func(p *Pool) { p.AllowedEvents = []string{"workflow_dispatch"} })},
		{"events schedule", "", pool(func(p *Pool) { p.AllowedEvents = []string{"schedule"} })},
		{"events all three", "", pool(func(p *Pool) { p.AllowedEvents = []string{"push", "workflow_dispatch", "schedule"} })},
		{"events empty", "pools[0].allowed-events", pool(func(p *Pool) { p.AllowedEvents = nil })},
		{"event pull_request_target", "pools[0].allowed-events", pool(func(p *Pool) { p.AllowedEvents = []string{"pull_request_target"} })},
		{"event workflow_run", "pools[0].allowed-events", pool(func(p *Pool) { p.AllowedEvents = []string{"push", "workflow_run"} })},
		{"event wrong case", "pools[0].allowed-events", pool(func(p *Pool) { p.AllowedEvents = []string{"Push"} })},
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

func TestValidatePoolCount(t *testing.T) {
	for _, n := range []int{0, 2, 3} {
		cfg := validConfig(t)
		cfg.Pools = make([]Pool, n)
		for i := range cfg.Pools {
			cfg.Pools[i] = validConfig(t).Pools[0]
		}
		errs := errorList(cfg.Validate())
		if len(errs) != 1 || !strings.Contains(errs[0].Error(), "this version supports exactly 1 pool") {
			t.Errorf("%d pools: Validate errors = %v, want only the pool count error", n, errs)
		}
	}
}

func TestValidateChecksEveryPool(t *testing.T) {
	cfg := validConfig(t)
	second := cfg.Pools[0]
	second.Max = 0
	cfg.Pools = append(cfg.Pools, second)
	err := cfg.Validate()
	if errs := errorList(err); len(errs) != 2 || !strings.Contains(err.Error(), "pools[1].max") {
		t.Fatalf("Validate error = %v, want the pool count error and pools[1].max", err)
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
