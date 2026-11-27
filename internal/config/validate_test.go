package config

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"slices"
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
	target.URL = "https://github.com/acme/widget"
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

		// The image forms are in TestImageValue (0003 SPEC R318).
		{"runner image ID", "", func(c *Config) { c.Runner.Image = "sha256:" + hex64 }},
		{"runner image empty", "runner.image", func(c *Config) { c.Runner.Image = "" }},
		{"runner image upper case name", "runner.image", func(c *Config) { c.Runner.Image = "Runner:latest" }},
		{"buildkit image", "", func(c *Config) { c.BuildKit.Image = "moby/buildkit:v0.34.2-rootless@sha256:" + hex64 }},
		{"buildkit image empty", "buildkit.image", func(c *Config) { c.BuildKit.Image = "" }},
		{"buildkit image space", "buildkit.image", func(c *Config) { c.BuildKit.Image = "moby/buildkit rootless" }},

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
		{"runner memory 6m", "", func(c *Config) { c.Runner.Memory = 6 * Mebibyte }},
		{"runner memory below 6m", "runner.memory", func(c *Config) { c.Runner.Memory = 6*Mebibyte - 1 }},
		{"runner memory 0", "runner.memory", func(c *Config) { c.Runner.Memory = 0 }},
		{"runner pids 1", "", func(c *Config) { c.Runner.Pids = 1 }},
		{"runner pids 0", "runner.pids", func(c *Config) { c.Runner.Pids = 0 }},
		{"runner pids negative", "runner.pids", func(c *Config) { c.Runner.Pids = -1 }},

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
		{"buildkit memory 6m", "", func(c *Config) { c.BuildKit.Memory = 6 * Mebibyte }},
		{"buildkit memory below 6m", "buildkit.memory", func(c *Config) { c.BuildKit.Memory = 6*Mebibyte - 1 }},
		{"buildkit memory 0", "buildkit.memory", func(c *Config) { c.BuildKit.Memory = 0 }},
		{"buildkit pids 1", "", func(c *Config) { c.BuildKit.Pids = 1 }},
		{"buildkit pids 0", "buildkit.pids", func(c *Config) { c.BuildKit.Pids = 0 }},
		{"buildkit pids negative", "buildkit.pids", func(c *Config) { c.BuildKit.Pids = -1 }},
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

// TestValidateTargetURL proves 0002 SPEC R231, R232 and 0003 SPEC R314: an
// accepted URL gives no error; a URL with "@" gives the R314 message, an
// organization or account URL the R232 one, and any other rejected URL the
// R231 one.
func TestValidateTargetURL(t *testing.T) {
	for _, c := range targetURLCases {
		cfg := validConfig(t)
		cfg.Targets[0].URL = c.url
		errs := errorList(cfg.Validate())
		if c.ok {
			if len(errs) != 0 {
				t.Errorf("url %q: Validate = %v, want no error", c.url, errs)
			}
			continue
		}
		want := fmt.Sprintf(`targets[0].url: must be https://github.com/<owner>/<repo> without a trailing "/" or ".git", got %q`, c.url)
		switch {
		case strings.Contains(c.url, "@"):
			want = "targets[0].url: value contains a credential; set the repository URL only"
		case c.owner:
			want = fmt.Sprintf(`targets[0].url: %q names an organization or account; list each repository as its own [[targets]] url = "https://github.com/<owner>/<repo>" and set its max-runners (default 1)`, c.url)
		}
		if len(errs) != 1 || errs[0].Error() != want {
			t.Errorf("url %q: Validate = %v, want only %q", c.url, errs, want)
		}
	}
}

// TestTargetURLRuleMatchesGitHub proves 0002 SPEC R231, R232: this
// package's rule and github.ParseTargetURL give the same answer.
func TestTargetURLRuleMatchesGitHub(t *testing.T) {
	for _, c := range targetURLCases {
		_, err := github.ParseTargetURL(c.url)
		_, got := repoURL(c.url)
		if got != (err == nil) || got != c.ok {
			t.Errorf("url %q: config accepts %v, github.ParseTargetURL error = %v, want accepted %v", c.url, got, err, c.ok)
		}
	}
}

// targetURLCases are the rows of github's TestParseTargetURL, and more. ok
// is whether the URL is accepted; owner is whether a rejected URL gets the
// R232 message instead of the R231 one.
var targetURLCases = []struct {
	url   string
	ok    bool
	owner bool
}{
	{"https://github.com/acme-1/repo.name_x-1", true, false},
	{"https://github.com/Acme/Widget", true, false},
	{"https://github.com/a/b", true, false},
	{"https://github.com/acme/.github", true, false},
	{"https://github.com/acme/x.gitx", true, false},
	{"https://github.com/acme/...", true, false},
	{"https://github.com/acme", false, true},
	{"https://github.com/enterprises", false, false},
	{"https://github.com/Enterprises", false, false},
	{"https://github.com/Acme-1", false, true},
	{"https://github.com/enterprises/x", false, false},
	{"https://github.com/Enterprises/x", false, false},
	{"https://github.com/acme/widget/", false, false},
	{"https://github.com/acme/widget.git", false, false},
	{"https://github.com/acme/widget.GIT", false, false},
	{"https://github.com/acme/.git", false, false},
	{"https://github.com/acme/.", false, false},
	{"https://github.com/acme/..", false, false},
	{"https://github.com/acme/widget/tree", false, false},
	{"https://github.com/acme/widget?x=1", false, false},
	{"https://github.com/acme/widget#frag", false, false},
	{"http://github.com/acme/widget", false, false},
	{"https://github.com//widget", false, false},
	{"https://github.com/acme/", false, false},
	{"https://github.com/ac_me/widget", false, false},
	{"https://github.com/acme/wid get", false, false},
	{"https://github.com/acme/wid/get", false, false},
	{"https://github.com/acme/widget\n", false, false},
	{"https://github.com/acme\n", false, false},
	{"", false, false},
	{"https://github.com", false, false},
	{"https://github.com/", false, false},
	{"https://example.com/acme/widget", false, false},
	{"https://www.github.com/acme/widget", false, false},
	{"HTTPS://github.com/acme/widget", false, false},
	{" https://github.com/acme/widget", false, false},
	{"github.com/acme/widget", false, false},
	{"https://user@github.com/acme/widget", false, false},
}

func TestValidateChecksEveryTarget(t *testing.T) {
	cfg := validConfig(t)
	second := cfg.Targets[0]
	second.URL = "https://github.com/acme/other"
	second.MaxRunners = 0
	cfg.Targets = append(cfg.Targets, second)
	err := cfg.Validate()
	if errs := errorList(err); len(errs) != 1 || !strings.Contains(err.Error(), "targets[1].max-runners") {
		t.Fatalf("Validate error = %v, want only targets[1].max-runners", err)
	}
}

// TestValidateDuplicateRepository proves 0002 SPEC R233: only the
// overlapping values are listed, in one error, whatever the case. URLs that
// break R231 are left out of the comparison and get only their own error.
func TestValidateDuplicateRepository(t *testing.T) {
	cfg := validConfig(t)
	cfg.Targets[0].URL = "https://github.com/Acme/Repo"
	for _, url := range []string{
		"https://github.com/acme/solo",
		"https://github.com/ACME/REPO",
		"https://github.com/other/repo",
		"https://github.com/acme/repo",
		"https://github.com/acme/x.git",
		"https://github.com/acme/X.GIT",
		"https://github.com/acme/.",
		"https://github.com/acme/.",
	} {
		extra := cfg.Targets[0]
		extra.URL = url
		cfg.Targets = append(cfg.Targets, extra)
	}
	errs := errorList(cfg.Validate())
	const want = "targets: repository listed more than once (owner and repository names compare without case): https://github.com/Acme/Repo, https://github.com/ACME/REPO, https://github.com/acme/repo"
	var dup []string
	for _, e := range errs {
		if strings.HasPrefix(e.Error(), "targets: ") {
			dup = append(dup, e.Error())
		}
	}
	if len(dup) != 1 || dup[0] != want {
		t.Fatalf("duplicate errors = %q, want only %q", dup, want)
	}
	// One R231 error for each of the four bad URLs.
	if len(errs) != 5 {
		t.Errorf("Validate returned %d errors, want 5: %v", len(errs), errs)
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
		{name: "0750", data: pkcs1, mode: 0o750, wantErr: 1},
		{name: "0700", data: pkcs1, mode: 0o700},
		{name: "0500", data: pkcs1, mode: 0o500},
		{name: "setuid 0600", data: pkcs1, mode: 0o600 | os.ModeSetuid},
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

// R30 as OpenSSH checks it (st_mode & 077): a special bit on a file with no
// group or other permission bit is not refused, and one with such a bit is.
func TestReadAppPrivateKeySpecialBits(t *testing.T) {
	for _, mode := range []os.FileMode{
		0o600 | os.ModeSetgid,
		0o400 | os.ModeSetgid,
		0o600 | os.ModeSticky,
		0o400 | os.ModeSticky,
	} {
		t.Run(mode.String(), func(t *testing.T) {
			path := writeKeySpecial(t, mode)
			if key, err := ReadAppPrivateKey(path); err != nil || key == nil {
				t.Fatalf("ReadAppPrivateKey = %v, %v; want the key", key, err)
			}
		})
	}
	for _, mode := range []os.FileMode{0o640 | os.ModeSetgid, 0o644 | os.ModeSticky} {
		t.Run(mode.String(), func(t *testing.T) {
			path := writeKeySpecial(t, mode)
			key, err := ReadAppPrivateKey(path)
			if key != nil || err == nil {
				t.Fatalf("ReadAppPrivateKey = %v, %v; want the mode error", key, err)
			}
			if errs := errorList(err); len(errs) != 1 || !strings.Contains(err.Error(), "group or other bits") {
				t.Errorf("errors = %v, want one mode error", errs)
			}
		})
	}
}

// tokenConfig is validConfig with a token file as the default credential.
func tokenConfig(t *testing.T) Config {
	t.Helper()
	cfg := validConfig(t)
	cfg.GitHub = GitHub{TokenFile: writeToken(t, fixtureToken("ghp_")+"\n", 0o600)}
	return cfg
}

const (
	wantConflict = "github: set either app-client-id and app-private-key-file, or token-file, not both"
	noDefault    = "github: no default credential (app-client-id and app-private-key-file, or token-file) for targets without their own: "
)

// errorTexts returns the texts of the errors joined in err.
func errorTexts(err error) []string {
	var texts []string
	for _, e := range errorList(err) {
		texts = append(texts, e.Error())
	}
	return texts
}

// TestCredentialRules proves 0003 SPEC R272, R273, R274: one default
// credential at most, the other App key required beside one, and every
// target without its own token file or App listed when there is no default.
func TestCredentialRules(t *testing.T) {
	addTarget := func(c *Config, url, tokenFile string) {
		target := DefaultTarget()
		target.URL, target.TokenFile = url, tokenFile
		c.Targets = append(c.Targets, target)
	}
	tests := []struct {
		name   string
		modify func(t *testing.T, c *Config)
		want   []string
	}{
		{name: "App only (0002)", modify: func(*testing.T, *Config) {}},
		{name: "token only", modify: func(t *testing.T, c *Config) { *c = tokenConfig(t) }},
		{name: "token with app-client-id", want: []string{wantConflict}, modify: func(t *testing.T, c *Config) {
			*c = tokenConfig(t)
			c.GitHub.AppClientID = "test-client-id"
		}},
		{name: "token with app-private-key-file", want: []string{wantConflict}, modify: func(t *testing.T, c *Config) {
			*c = tokenConfig(t)
			c.GitHub.AppPrivateKeyFile = writeKey(t, pkcs1PEM(t), 0o600)
		}},
		{name: "token with both App keys", want: []string{wantConflict}, modify: func(t *testing.T, c *Config) {
			app := c.GitHub
			*c = tokenConfig(t)
			c.GitHub.AppClientID, c.GitHub.AppPrivateKeyFile = app.AppClientID, app.AppPrivateKeyFile
		}},
		{name: "token with blank app-client-id", want: []string{wantConflict}, modify: func(t *testing.T, c *Config) {
			*c = tokenConfig(t)
			c.GitHub.AppClientID = " "
		}},
		{name: "app-client-id only", want: []string{"github.app-private-key-file: is required"}, modify: func(_ *testing.T, c *Config) {
			c.GitHub.AppPrivateKeyFile = ""
		}},
		{name: "app-private-key-file only", want: []string{"github.app-client-id: is required"}, modify: func(_ *testing.T, c *Config) {
			c.GitHub.AppClientID = ""
		}},
		{name: "no default, two targets without a credential", want: []string{noDefault + "targets[0] (https://github.com/acme/widget), targets[2] (https://github.com/acme/third)"}, modify: func(t *testing.T, c *Config) {
			c.GitHub = GitHub{}
			addTarget(c, "https://github.com/acme/second", writeToken(t, fixtureToken("github_pat_"), 0o600))
			addTarget(c, "https://github.com/acme/third", "")
		}},
		{name: "no default, a bad URL quoted", want: []string{
			noDefault + `targets[0] ("https://github.com/acme/x.git")`,
			`targets[0].url: must be https://github.com/<owner>/<repo> without a trailing "/" or ".git", got "https://github.com/acme/x.git"`,
		}, modify: func(_ *testing.T, c *Config) {
			c.GitHub = GitHub{}
			c.Targets[0].URL = "https://github.com/acme/x.git"
		}},
		{name: "no default, every target with token-file", modify: func(t *testing.T, c *Config) {
			c.GitHub = GitHub{}
			c.Targets[0].TokenFile = writeToken(t, fixtureToken("ghp_"), 0o600)
			addTarget(c, "https://github.com/acme/second", writeToken(t, fixtureToken("github_pat_"), 0o600))
		}},
		{name: "no default and no target", want: []string{"targets: at least 1 [[targets]] entry is required, got 0"}, modify: func(_ *testing.T, c *Config) {
			c.GitHub = GitHub{}
			c.Targets = nil
		}},
		{name: "App default and a token target", modify: func(t *testing.T, c *Config) {
			addTarget(c, "https://github.com/other/repo", writeToken(t, fixtureToken("github_pat_"), 0o600))
		}},
		{name: "token default and a token target", modify: func(t *testing.T, c *Config) {
			*c = tokenConfig(t)
			addTarget(c, "https://github.com/other/repo", writeToken(t, fixtureToken("github_pat_"), 0o600))
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := validConfig(t)
			tt.modify(t, &cfg)
			if got := errorTexts(cfg.Validate()); !slices.Equal(got, tt.want) {
				t.Errorf("Validate errors:\n got %q\nwant %q", got, tt.want)
			}
		})
	}
}

// githubTOML is requiredTOML with github as the body of [github]; an empty
// github leaves the table out.
func githubTOML(github string) string {
	content := requiredTOML("KEY")
	start := strings.Index(content, "[github]")
	end := strings.Index(content, "[runner]")
	if github == "" {
		return content[:start] + content[end:]
	}
	return content[:start] + "[github]\n" + github + "\n" + content[end:]
}

// TestCredentialRulesDecoded proves the rules through Load: a key that
// failed to decode still counts as written, so it adds no "is required"
// and no R274 error of its own.
func TestCredentialRulesDecoded(t *testing.T) {
	key := writeKey(t, pkcs1PEM(t), 0o600)
	tokenFile := writeToken(t, fixtureToken("ghp_"), 0o600)
	tests := []struct {
		name    string
		content string
		want    []string // prefixes, in order
	}{
		{
			name:    "token-file of the wrong type beside the App",
			content: githubTOML(`app-client-id = "id"` + "\n" + `app-private-key-file = "` + key + `"` + "\ntoken-file = 5"),
			want:    []string{"config ", wantConflict},
		},
		{
			name:    "app-client-id of the wrong type beside a token",
			content: githubTOML(`app-client-id = 5` + "\n" + `token-file = "` + tokenFile + `"`),
			want:    []string{"config ", wantConflict},
		},
		{
			name:    "app-client-id of the wrong type beside the key",
			content: githubTOML(`app-client-id = 5` + "\n" + `app-private-key-file = "` + key + `"`),
			want:    []string{"config "},
		},
		{
			name:    "target token-file of the wrong type and no default",
			content: githubTOML("") + "token-file = 5\n",
			want:    []string{"config "},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := loadString(t, tt.content)
			got := errorTexts(err)
			if len(got) != len(tt.want) {
				t.Fatalf("Load errors = %q, want %d", got, len(tt.want))
			}
			for i, w := range tt.want {
				if !strings.HasPrefix(got[i], w) {
					t.Errorf("error %d = %q, want it to start with %q", i, got[i], w)
				}
			}
		})
	}
}

// TestTokenValueInPathKey proves 0003 SPEC R276 and R277 for both token file
// keys: a token in place of the path gives only the R276 message, before
// any file access, and is not echoed.
func TestTokenValueInPathKey(t *testing.T) {
	keys := []struct {
		key string
		set func(c *Config, value string)
	}{
		{"github.token-file", func(c *Config, v string) { c.GitHub = GitHub{TokenFile: v} }},
		{"targets[0].token-file", func(c *Config, v string) { c.Targets[0].TokenFile = v }},
	}
	for _, k := range keys {
		for _, prefix := range TokenPrefixes() {
			t.Run(k.key+" "+prefix, func(t *testing.T) {
				// The value names no file, so a file access would add
				// a file error.
				value := fixtureToken(prefix)
				cfg := validConfig(t)
				k.set(&cfg, value)
				err := cfg.Validate()
				want := k.key + ": value looks like a token, not a file path; set the path of the token file"
				if got := errorTexts(err); len(got) != 1 || got[0] != want {
					t.Fatalf("Validate = %q, want only %q", got, want)
				}
				if strings.Contains(err.Error(), value[len(prefix):]) {
					t.Errorf("error echoes the value: %v", err)
				}
			})
		}
	}
}

// TestTokenFileRulesInValidate proves that both token file keys get the
// R275 rules under their key names, every error on its own line.
func TestTokenFileRulesInValidate(t *testing.T) {
	const modeErr = "file mode -rw-r--r-- is wider than 0600 (group or other bits are set); use 0600 or 0400"
	for _, tt := range []struct {
		name string
		set  func(t *testing.T, c *Config)
		want []string
	}{
		{"github relative", func(_ *testing.T, c *Config) { c.GitHub = GitHub{TokenFile: "pat.txt"} },
			[]string{"github.token-file: must be an absolute path"}},
		{"target mode", func(t *testing.T, c *Config) { c.Targets[0].TokenFile = writeToken(t, fixtureToken("ghp_"), 0o644) },
			[]string{"targets[0].token-file: " + modeErr}},
		{"target mode and empty", func(t *testing.T, c *Config) { c.Targets[0].TokenFile = writeToken(t, "", 0o644) },
			[]string{"targets[0].token-file: " + modeErr, "targets[0].token-file: token file is empty"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cfg := validConfig(t)
			tt.set(t, &cfg)
			if got := errorTexts(cfg.Validate()); !slices.Equal(got, tt.want) {
				t.Errorf("Validate errors:\n got %q\nwant %q", got, tt.want)
			}
		})
	}
}

// credentialURLs returns URLs that hold a credential (0003 SPEC R314), built
// at run time.
func credentialURLs() []string {
	urls := []string{
		"https://x@github.com/o/r",
		"https://" + fixtureToken("ghp_") + "@github.com/acme/widget",
	}
	for _, prefix := range TokenPrefixes() {
		urls = append(urls,
			"https://github.com/acme/"+prefix+tokenChars(20),
			"https://github.com/acme/"+prefix+tokenChars(30)+"/")
	}
	return urls
}

// TestURLCredential proves 0003 SPEC R314 and R277: a credential URL gets
// only the R314 message, is left out of R233 and shown as targets[<i>] in
// R274; a prefix with fewer than 20 characters is an ordinary URL.
func TestURLCredential(t *testing.T) {
	const want = "targets[0].url: value contains a credential; set the repository URL only"
	for _, url := range credentialURLs() {
		// Twice, so R233 would list it, and with no default credential,
		// so R274 would.
		cfg := validConfig(t)
		cfg.GitHub = GitHub{}
		cfg.Targets = append(cfg.Targets, cfg.Targets[0])
		cfg.Targets[0].URL, cfg.Targets[1].URL = url, url
		err := cfg.Validate()
		wantAll := []string{
			noDefault + "targets[0], targets[1]",
			want,
			strings.Replace(want, "[0]", "[1]", 1),
		}
		if got := errorTexts(err); !slices.Equal(got, wantAll) {
			t.Errorf("Validate errors:\n got %q\nwant %q", got, wantAll)
		}
		if err != nil && strings.Contains(err.Error(), strings.TrimPrefix(url, "https://")) {
			t.Errorf("error echoes the URL: %v", err)
		}
	}
	for _, url := range []string{
		"https://github.com/acme/ghp_x",
		"https://github.com/acme/ghp_" + tokenChars(19),
		"https://github.com/acme/github_pat_" + tokenChars(19),
	} {
		cfg := validConfig(t)
		cfg.Targets[0].URL = url
		if err := cfg.Validate(); err != nil {
			t.Errorf("url %q: Validate = %v, want no error", url, err)
		}
	}
}

// TestValidateRepositoryURL proves that the exported helper gives the
// targets.url messages without the key (0003 SPEC R314, 0002 SPEC R231,
// R232), and that RepositoryKey agrees with R233.
func TestValidateRepositoryURL(t *testing.T) {
	urls := credentialURLs()
	for _, c := range targetURLCases {
		urls = append(urls, c.url)
	}
	for _, url := range urls {
		cfg := validConfig(t)
		cfg.Targets[0].URL = url
		var want string
		if got := errorTexts(cfg.Validate()); len(got) == 1 {
			want = strings.TrimPrefix(got[0], "targets[0].url: ")
		}
		var got string
		if err := ValidateRepositoryURL(url); err != nil {
			got = err.Error()
		}
		if got != want {
			t.Errorf("ValidateRepositoryURL(%q) = %q, want %q", url, got, want)
		}

		key, ok := RepositoryKey(url)
		if ok != (want == "") {
			t.Errorf("RepositoryKey(%q) ok = %v, want %v", url, ok, want == "")
		}
		// R233: the key is the owner and repository without case.
		if wantKey := strings.ToLower(strings.TrimPrefix(url, "https://github.com/")); ok && key != wantKey {
			t.Errorf("RepositoryKey(%q) = %q, want %q", url, key, wantKey)
		}
	}
}
