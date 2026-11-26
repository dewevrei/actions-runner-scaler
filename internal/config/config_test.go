package config

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/BurntSushi/toml"
)

// testRSAKey is a throwaway key generated once per test binary.
var testRSAKey = sync.OnceValues(func() (*rsa.PrivateKey, error) {
	return rsa.GenerateKey(rand.Reader, 2048)
})

func rsaKey(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	key, err := testRSAKey()
	if err != nil {
		t.Fatalf("generate RSA key: %v", err)
	}
	return key
}

func pkcs1PEM(t *testing.T) []byte {
	t.Helper()
	return pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(rsaKey(t))})
}

func pkcs8PEM(t *testing.T) []byte {
	t.Helper()
	der, err := x509.MarshalPKCS8PrivateKey(rsaKey(t))
	if err != nil {
		t.Fatalf("marshal PKCS#8: %v", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
}

// writeFile writes data to a new file in a temporary directory and sets its
// mode exactly, independent of the umask.
func writeFile(t *testing.T, name string, data []byte, mode os.FileMode) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatalf("chmod %s: %v", name, err)
	}
	return path
}

func writeKey(t *testing.T, data []byte, mode os.FileMode) string {
	t.Helper()
	return writeFile(t, "app.pem", data, mode)
}

var (
	testRunnerImage   = "sha256:" + strings.Repeat("a", 64)
	testBuildKitImage = "moby/buildkit:v0.0.0-rootless@sha256:" + strings.Repeat("b", 64)
)

// requiredTOML holds only the keys without defaults, with keyPath as the App
// private key file.
func requiredTOML(keyPath string) string {
	return `
[github]
app-client-id = "test-client-id"
app-private-key-file = "` + keyPath + `"

[runner]
image = "` + testRunnerImage + `"

[buildkit]
image = "` + testBuildKitImage + `"

[[targets]]
url = "https://github.com/acme/widget"
`
}

// withRunner adds lines to the [runner] table of content.
func withRunner(content, lines string) string {
	return strings.Replace(content, "[runner]\n", "[runner]\n"+lines, 1)
}

// withBuildKit adds lines to the [buildkit] table of content.
func withBuildKit(content, lines string) string {
	return strings.Replace(content, "[buildkit]\n", "[buildkit]\n"+lines, 1)
}

func loadString(t *testing.T, content string) (*Config, error) {
	t.Helper()
	return Load(writeFile(t, "config.toml", []byte(content), 0o600))
}

// errorList returns the errors joined in err.
func errorList(err error) []error {
	if err == nil {
		return nil
	}
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		return joined.Unwrap()
	}
	return []error{err}
}

func TestLoadDefaultsMatchSpecTable(t *testing.T) {
	key := writeKey(t, pkcs1PEM(t), 0o600)
	cfg, err := loadString(t, requiredTOML(key))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	// Literal values from the SPEC settings table, not the package
	// constants, so a changed constant fails here.
	want := Config{
		GitHub: GitHub{
			AppClientID:       "test-client-id",
			AppPrivateKeyFile: key,
		},
		Runner:   Container{Image: testRunnerImage, CPUs: 1, Memory: 1 << 30, Pids: 512},
		BuildKit: Container{Image: testBuildKitImage, CPUs: 2, Memory: 2 << 30, Pids: 1024},
		JobCheck: JobCheck{Listen: "127.0.0.1:8765"},
		Shutdown: Shutdown{DrainTimeout: 10 * time.Minute},
		Scaling:  Scaling{Label: "linux-arm64-deploy"},
		Targets:  []Target{{URL: "https://github.com/acme/widget", MaxRunners: 1, Idle: 0}},
	}
	if !reflect.DeepEqual(*cfg, want) {
		t.Errorf("Load defaults:\n got %+v\nwant %+v", *cfg, want)
	}
}

// TestDefaults proves 0002 SPEC R121, R122, R235: the defaults of the
// settings table, as literals.
func TestDefaults(t *testing.T) {
	c := Default()
	if c.Runner != (Container{CPUs: 1, Memory: 1 << 30, Pids: 512}) {
		t.Errorf("runner defaults = %+v, want 1 CPU, 1g, 512", c.Runner)
	}
	if c.BuildKit != (Container{CPUs: 2, Memory: 2 << 30, Pids: 1024}) {
		t.Errorf("buildkit defaults = %+v, want 2 CPU, 2g, 1024", c.BuildKit)
	}
	if c.Scaling.Label != "linux-arm64-deploy" {
		t.Errorf("label default = %q", c.Scaling.Label)
	}
	if got := DefaultTarget(); got != (Target{MaxRunners: 1, Idle: 0}) {
		t.Errorf("target defaults = %+v, want max-runners 1, idle 0", got)
	}
}

func TestLoadExplicitValues(t *testing.T) {
	key := writeKey(t, pkcs8PEM(t), 0o400)
	content := withRunner(requiredTOML(key), "cpus = 1.5\nmemory = \"1536m\"\npids = 2048\n")
	content = withBuildKit(content, "cpus = 4\nmemory = \"4g\"\npids = 4096\n")
	cfg, err := loadString(t, content+`
max-runners = 5
idle = 1

[scaling]
label = "ci-pool"

[jobcheck]
listen = "[::1]:9000"

[shutdown]
drain-timeout = "1h"
`)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	tg := cfg.Targets[0]
	got := []any{cfg.Scaling.Label, tg.MaxRunners, tg.Idle,
		cfg.Runner.CPUs, cfg.Runner.Memory, cfg.Runner.Pids,
		cfg.BuildKit.CPUs, cfg.BuildKit.Memory, cfg.BuildKit.Pids,
		cfg.JobCheck.Listen, cfg.Shutdown.DrainTimeout}
	want := []any{"ci-pool", 5, 1,
		1.5, 1536 * Mebibyte, int64(2048),
		4.0, 4 * Gibibyte, int64(4096),
		"[::1]:9000", time.Hour}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Load explicit values:\n got %v\nwant %v", got, want)
	}
}

func TestLoadCollectsAllErrors(t *testing.T) {
	key := writeKey(t, pkcs1PEM(t), 0o600)
	content := strings.Replace(requiredTOML(key), "[runner]", "unknown-one = true\n\n[runner]", 1) + `
max-runners = 0
unknown-two = "x"
`
	_, err := loadString(t, content)
	errs := errorList(err)
	if len(errs) != 3 {
		t.Fatalf("Load returned %d errors, want 3: %v", len(errs), err)
	}
	for _, want := range []string{`"github.unknown-one"`, `"targets[0].unknown-two"`, "targets[0].max-runners"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("Load error does not mention %s:\n%v", want, err)
		}
	}
}

// A value of the wrong TOML type stops only its own table, so unknown keys
// and range errors elsewhere are still reported (R28). The message names the
// key and types but not the value (R31).
func TestLoadTypeErrorDoesNotHideOtherTables(t *testing.T) {
	key := writeKey(t, pkcs1PEM(t), 0o600)
	content := strings.Replace(requiredTOML(key), "[runner]", "unknown-one = true\n\n[runner]", 1) + `
max-runners = 0

[jobcheck]
listen = 87654321
`
	_, err := loadString(t, content)
	errs := errorList(err)
	if len(errs) != 3 {
		t.Fatalf("Load returned %d errors, want 3: %v", len(errs), err)
	}
	for _, want := range []string{`"jobcheck.listen"`, "incompatible types", `"github.unknown-one"`, "targets[0].max-runners"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("Load error does not mention %s:\n%v", want, err)
		}
	}
	if strings.Contains(err.Error(), "87654321") {
		t.Errorf("error quotes the value: %v", err)
	}
}

func TestLoadTypeErrorInTarget(t *testing.T) {
	key := writeKey(t, pkcs1PEM(t), 0o600)
	tests := []struct {
		name string
		add  string // appended to the [[targets]] table
		want []string
	}{
		// The bad max-runners keeps its default, so idle is still checked.
		{name: "target key", add: "max-runners = \"SECRETVALUE\"\nidle = -1\n", want: []string{
			"targets[0].max-runners: ", `unknown key "unknown-top"`, "targets[0].idle: must be between",
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := loadString(t, "unknown-top = 1\n"+requiredTOML(key)+tt.add)
			errs := errorList(err)
			if len(errs) != len(tt.want) {
				t.Fatalf("Load returned %d errors, want %d: %v", len(errs), len(tt.want), err)
			}
			for i, want := range tt.want {
				if !strings.Contains(errs[i].Error(), want) {
					t.Errorf("error %d = %v, want it to contain %s", i, errs[i], want)
				}
			}
			if strings.Contains(err.Error(), "SECRETVALUE") {
				t.Errorf("error quotes the value: %v", err)
			}
		})
	}
}

// Several bad keys in one table are all reported, in the same order on every
// run, although the decoder walks a table in map order (R28).
func TestLoadReportsEveryBadKeyDeterministically(t *testing.T) {
	key := writeKey(t, pkcs1PEM(t), 0o600)
	path := writeFile(t, "config.toml", []byte(requiredTOML(key)+`
max-runners = "two"
runner-group = 5
typo = 1
idle = -1
`), 0o600)
	want := []string{
		// Decoding problems in key order, the R234 message after its
		// table, then validation.
		"targets[0].max-runners: ",
		`unknown key "targets[0].typo"`,
		"targets[0].runner-group: no longer used",
		"targets[0].idle: must be between",
	}
	_, first := Load(path)
	errs := errorList(first)
	if len(errs) != len(want) {
		t.Fatalf("Load returned %d errors, want %d:\n%v", len(errs), len(want), first)
	}
	for i, w := range want {
		if !strings.Contains(errs[i].Error(), w) {
			t.Errorf("error %d = %v, want it to contain %s", i, errs[i], w)
		}
	}
	for range 50 {
		if _, err := Load(path); err.Error() != first.Error() {
			t.Fatalf("Load error changed between runs:\n%v\n---\n%v", first, err)
		}
	}
}

// TOML keys are case-sensitive: [GitHub] is not [github], even though the
// decoder matches struct fields case-insensitively.
func TestLoadTableNameIsCaseSensitive(t *testing.T) {
	key := writeKey(t, pkcs1PEM(t), 0o600)
	_, err := loadString(t, requiredTOML(key)+"\n[GitHub]\napp-client-id = \"other\"\n")
	errs := errorList(err)
	if len(errs) != 1 || !strings.Contains(err.Error(), `unknown key "GitHub"`) {
		t.Fatalf("Load error = %v, want only the unknown table GitHub", err)
	}
}

// A table or the targets array given as another TOML type is a type error,
// not a stop of the whole decoder or a misleading target count error.
func TestLoadTableOfWrongType(t *testing.T) {
	key := writeKey(t, pkcs1PEM(t), 0o600)
	base := requiredTOML(key)
	targetStart := strings.Index(base, "[[targets]]")
	tests := []struct {
		name    string
		content string
		want    string
	}{
		{name: "table", content: "jobcheck = 5\n" + base, want: "jobcheck: "},
		{name: "targets", content: "targets = 5\n" + base[:targetStart], want: "targets: "},
		{name: "target entry", content: "targets = [5]\n" + base[:targetStart], want: "targets[0]: "},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := loadString(t, tt.content)
			errs := errorList(err)
			if len(errs) != 1 || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Load error = %v, want one type error for %s", err, tt.want)
			}
		})
	}
}

// TestLoadNoTargets proves 0002 SPEC R105: no target is an error with the
// SPEC text, and any number from 1 on loads.
func TestLoadNoTargets(t *testing.T) {
	key := writeKey(t, pkcs1PEM(t), 0o600)
	base := requiredTOML(key)
	targetStart := strings.Index(base, "[[targets]]")
	oneTarget := base[targetStart:]
	other := func(repo string) string {
		return "\n[[targets]]\nurl = \"https://github.com/acme/" + repo + "\"\n"
	}

	for _, tt := range []struct{ name, content string }{
		{"zero", base[:targetStart]},
		{"empty array", "targets = []\n" + base[:targetStart]},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, err := loadString(t, tt.content)
			errs := errorList(err)
			const want = "targets: at least 1 [[targets]] entry is required, got 0"
			if len(errs) != 1 || errs[0].Error() != want {
				t.Fatalf("Load error = %v, want only %q", err, want)
			}
		})
	}
	t.Run("one", func(t *testing.T) {
		if _, err := loadString(t, base); err != nil {
			t.Fatalf("Load: %v", err)
		}
	})
	t.Run("three", func(t *testing.T) {
		cfg, err := loadString(t, base+other("second")+other("third"))
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if len(cfg.Targets) != 3 {
			t.Errorf("targets = %d, want 3", len(cfg.Targets))
		}
	})
	_ = oneTarget
}

// TestLoadRejects0001Format proves 0002 SPEC R107: [[pools]] and
// github.app-installation-id each give their SPEC message, the keys inside
// [[pools]] give no unknown-key error, and the other errors of the file are
// reported with them.
func TestLoadRejects0001Format(t *testing.T) {
	key := writeKey(t, pkcs1PEM(t), 0o600)
	content := `
[github]
app-client-id = "test-client-id"
app-installation-id = 12345
app-private-key-file = "` + key + `"

[runner]
image = "` + testRunnerImage + `"

[buildkit]
image = "` + testBuildKitImage + `"

[jobcheck]
listen = 87654321

[[pools]]
name = "linux-arm64-deploy"
repo-url = "https://github.com/acme/widget"
max = 2
idle = 0
runner-cpus = 1
runner-memory = "1g"
allowed-workflows = [".github/workflows/deploy.yml"]
allowed-events = ["push"]
`
	_, err := loadString(t, content)
	errs := errorList(err)
	want := []string{
		"github.app-installation-id: no longer used; the installation is looked up for each target",
		`pools: [[pools]] is the 0001 format; move max to [[targets]] max-runners, idle to [[targets]] idle, runner-*/build-* to [runner]/[buildkit] cpus, memory, pids, name to [scaling] label, and set [[targets]] url = "https://github.com/<owner>/<repo>" for each repository; allowed-workflows and allowed-events no longer exist`,
	}
	for _, w := range want {
		n := 0
		for _, e := range errs {
			if e.Error() == w {
				n++
			}
		}
		if n != 1 {
			t.Errorf("the error %q appears %d times in:\n%v", w, n, err)
		}
	}
	// The two messages, the jobcheck type error and the missing target.
	if len(errs) != 4 {
		t.Errorf("Load returned %d errors, want 4:\n%v", len(errs), err)
	}
	if strings.Contains(err.Error(), "unknown key") {
		t.Errorf("a key of the 0001 file is reported as unknown:\n%v", err)
	}
	if !strings.Contains(err.Error(), "jobcheck.listen") {
		t.Errorf("the other error of the file is missing:\n%v", err)
	}
}

// TestLoadDuplicateRepository proves 0002 SPEC R233: owner and repository
// names that differ only in case are one error that lists both values, and
// two repositories of one owner are no error.
func TestLoadDuplicateRepository(t *testing.T) {
	key := writeKey(t, pkcs1PEM(t), 0o600)
	withURLs := func(urls ...string) string {
		content := strings.Replace(requiredTOML(key), "https://github.com/acme/widget", urls[0], 1)
		for _, u := range urls[1:] {
			content += "\n[[targets]]\nurl = \"" + u + "\"\n"
		}
		return content
	}

	_, err := loadString(t, withURLs("https://github.com/Acme/Repo", "https://github.com/acme/repo", "https://github.com/acme/unrelated"))
	errs := errorList(err)
	const want = "targets: repository listed more than once (owner and repository names compare without case): https://github.com/Acme/Repo, https://github.com/acme/repo"
	if len(errs) != 1 || errs[0].Error() != want {
		t.Fatalf("Load error = %v, want only %q", err, want)
	}

	if _, err := loadString(t, withURLs("https://github.com/acme/a", "https://github.com/acme/b")); err != nil {
		t.Errorf("two repositories of one owner: Load = %v, want no error", err)
	}
}

// TestLoadRunnerGroupKey proves 0002 SPEC R234: the key gives its message
// whatever its value, an empty one included.
func TestLoadRunnerGroupKey(t *testing.T) {
	key := writeKey(t, pkcs1PEM(t), 0o600)
	const want = "targets[0].runner-group: no longer used; repository scale sets always use the default runner group, remove the key"
	for _, value := range []string{`""`, `"x"`, `5`} {
		_, err := loadString(t, requiredTOML(key)+"runner-group = "+value+"\n")
		errs := errorList(err)
		if len(errs) != 1 || errs[0].Error() != want {
			t.Errorf("runner-group = %s: Load error = %v, want only %q", value, err, want)
		}
	}
}

// TestOrganizationURLMessage proves 0002 SPEC R232: an organization or
// account URL gives the SPEC text after the key.
func TestOrganizationURLMessage(t *testing.T) {
	key := writeKey(t, pkcs1PEM(t), 0o600)
	_, err := loadString(t, strings.Replace(requiredTOML(key), "https://github.com/acme/widget", "https://github.com/acme", 1))
	errs := errorList(err)
	const want = `targets[0].url: "https://github.com/acme" names an organization or account; list each repository as its own [[targets]] url = "https://github.com/<owner>/<repo>" and set its max-runners (default 1)`
	if len(errs) != 1 || errs[0].Error() != want {
		t.Fatalf("Load error = %v, want only %q", err, want)
	}
}

func TestLoadUnknownKeys(t *testing.T) {
	key := writeKey(t, pkcs1PEM(t), 0o600)
	base := requiredTOML(key)
	tests := []struct {
		name string
		add  string // appended to the [[targets]] table unless it starts a table
		want []string
	}{
		{name: "target key", add: "maximum = 3\n", want: []string{`"targets[0].maximum"`}},
		{name: "upper-case target key", add: "MAX = 3\n", want: []string{`"targets[0].MAX"`}},
		{name: "unknown table reported once", add: "[cache]\nsize = 1\npath = \"x\"\n", want: []string{`"cache"`}},
		{name: "upper-case table", add: "[JobCheck]\nlisten = \"127.0.0.1:1\"\n", want: []string{`"JobCheck"`}},
		{name: "key in known table", add: "[shutdown]\ngrace = \"1m\"\n", want: []string{`"shutdown.grace"`}},
		{name: "key in scaling", add: "[scaling]\nname = \"x\"\n", want: []string{`"scaling.name"`}},
		{name: "0001 key in runner", add: "[runner]\nruner-cpus = 1\n", want: nil},
	}
	for _, tt := range tests {
		if tt.want == nil {
			continue
		}
		t.Run(tt.name, func(t *testing.T) {
			_, err := loadString(t, base+tt.add)
			errs := errorList(err)
			if len(errs) != len(tt.want) {
				t.Fatalf("Load returned %d errors, want %d: %v", len(errs), len(tt.want), err)
			}
			for i, want := range tt.want {
				if !strings.Contains(errs[i].Error(), want) {
					t.Errorf("error %d = %v, want it to mention %s", i, errs[i], want)
				}
			}
		})
	}
}

// A secret pasted as a bare key must not be echoed in full (R31).
func TestLoadUnknownKeyShortensSuspiciousNames(t *testing.T) {
	key := writeKey(t, pkcs1PEM(t), 0o600)
	base := requiredTOML(key)
	long := "fake" + strings.Repeat("x", 37) // 41 characters
	tests := []struct {
		name string
		add  string
		want string
		leak string
	}{
		{name: "40 characters shown", add: strings.Repeat("y", 40) + " = 1\n", want: `"targets[0].` + strings.Repeat("y", 40) + `"`},
		{name: "longer than 40", add: long + " = 1\n", want: `"targets[0].fake…(41 chars)"`, leak: long},
		{name: "unusual characters", add: `"ghs_Ab+/=" = 1` + "\n", want: `"targets[0].ghs_…(9 chars)"`, leak: "Ab+/="},
		{name: "unknown table", add: `["tok+en".x]` + "\n", want: `"tok+…(6 chars)"`, leak: "tok+en"},
		{name: "upper case only", add: "Max_Idle = 1\n", want: `"targets[0].Max_Idle"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := loadString(t, base+tt.add)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Load error = %v, want it to contain %s", err, tt.want)
			}
			if tt.leak != "" && strings.Contains(err.Error(), tt.leak) {
				t.Errorf("error echoes %q: %v", tt.leak, err)
			}
		})
	}
}

// A syntax error right after a pasted bare key must not echo it either.
func TestLoadSyntaxErrorShortensLastKey(t *testing.T) {
	key := writeKey(t, pkcs1PEM(t), 0o600)
	long := "fake" + strings.Repeat("z", 40)
	_, err := loadString(t, requiredTOML(key)+long+" = \n")
	if err == nil || strings.Contains(err.Error(), long) || !strings.Contains(err.Error(), "fake…(44 chars)") {
		t.Fatalf("Load error = %v, want the last key shortened", err)
	}
}

func TestLoadDecodeErrors(t *testing.T) {
	key := writeKey(t, pkcs1PEM(t), 0o600)
	base := requiredTOML(key)
	tests := []struct {
		name string
		add  string
	}{
		{name: "syntax", add: "max-runners = \n"},
		{name: "wrong type", add: "max-runners = \"two\"\n"},
		{name: "targets as table", add: "[targets]\nurl = \"x\"\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, err := loadString(t, base+tt.add)
			if err == nil {
				t.Fatalf("Load = %+v, want error", cfg)
			}
			if !strings.Contains(err.Error(), "config ") {
				t.Errorf("error %v does not name the config file", err)
			}
		})
	}
}

// A malformed size or duration must not stop the decoder: it is reported
// with unknown keys and range errors, once per key.
func TestLoadCollectsMalformedSizesAndDurations(t *testing.T) {
	key := writeKey(t, pkcs1PEM(t), 0o600)
	content := withRunner(requiredTOML(key), "memory = \"1x\"\n")
	content = withBuildKit(content, "memory = \"1 GB\"\n") + `
max-runners = 0
typo = 1

[shutdown]
drain-timeout = 600
`
	_, err := loadString(t, content)
	errs := errorList(err)
	want := []string{
		`unknown key "targets[0].typo"`,
		"runner.memory: invalid size",
		"buildkit.memory: invalid size",
		"shutdown.drain-timeout: invalid duration",
		"targets[0].max-runners: must be at least 1",
	}
	if len(errs) != len(want) {
		t.Fatalf("Load returned %d errors, want %d:\n%v", len(errs), len(want), err)
	}
	for _, w := range want {
		if !strings.Contains(err.Error(), w) {
			t.Errorf("Load error does not contain %q:\n%v", w, err)
		}
	}
}

// Sizes and durations accept any primitive TOML value as text.
func TestLoadSizeAsInteger(t *testing.T) {
	key := writeKey(t, pkcs1PEM(t), 0o600)
	cfg, err := loadString(t, withRunner(requiredTOML(key), "memory = 536870912\n"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := cfg.Runner.Memory; got != 512*Mebibyte {
		t.Errorf("runner.memory = %s, want 512m", got)
	}
}

// A syntax error must not quote the offending text, which may be a pasted
// secret (R31).
func TestLoadSyntaxErrorHidesValue(t *testing.T) {
	key := writeKey(t, pkcs1PEM(t), 0o600)
	const secret = "tSECRETVALUE"
	_, err := loadString(t, requiredTOML(key)+"max-runners = "+secret+"\n")
	if err == nil {
		t.Fatal("Load succeeded, want syntax error")
	}
	if strings.Contains(err.Error(), "SECRETVALUE") {
		t.Errorf("error quotes the value: %v", err)
	}
	if !strings.Contains(err.Error(), "line ") || !strings.Contains(err.Error(), `max-runners"`) {
		t.Errorf("error %v lacks the position or key", err)
	}
}

func TestLoadMissingFile(t *testing.T) {
	if _, err := Load(filepath.Join(t.TempDir(), "missing.toml")); err == nil {
		t.Fatal("Load of a missing file succeeded")
	}
}

// An explicit zero or empty value must reach validation rather than be
// replaced by the default.
func TestLoadExplicitZeroIsNotDefaulted(t *testing.T) {
	key := writeKey(t, pkcs1PEM(t), 0o600)
	tests := []struct {
		content string
		want    string
	}{
		{content: requiredTOML(key) + "max-runners = 0\n", want: "targets[0].max-runners"},
		{content: strings.Replace(requiredTOML(key), "https://github.com/acme/widget", "", 1), want: "targets[0].url"},
		{content: withRunner(requiredTOML(key), "cpus = 0\n"), want: "runner.cpus"},
		{content: withBuildKit(requiredTOML(key), "pids = 0\n"), want: "buildkit.pids"},
		{content: requiredTOML(key) + "[scaling]\nlabel = \"\"\n", want: "scaling.label"},
		{content: requiredTOML(key) + "[shutdown]\ndrain-timeout = \"0s\"\n", want: "shutdown.drain-timeout"},
		{content: requiredTOML(key) + "[jobcheck]\nlisten = \"\"\n", want: "jobcheck.listen"},
	}
	for _, tt := range tests {
		t.Run(tt.want, func(t *testing.T) {
			_, err := loadString(t, tt.content)
			errs := errorList(err)
			if len(errs) != 1 || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Load error = %v, want one error for %s", err, tt.want)
			}
		})
	}
}

func TestLoadBrokenPEMIsConfigError(t *testing.T) {
	key := writeKey(t, []byte("-----BEGIN RSA PRIVATE KEY-----\nbm90IGEga2V5\n-----END RSA PRIVATE KEY-----\n"), 0o600)
	_, err := loadString(t, requiredTOML(key))
	errs := errorList(err)
	if len(errs) != 1 || !strings.Contains(err.Error(), "github.app-private-key-file") {
		t.Fatalf("Load error = %v, want one github.app-private-key-file error", err)
	}
}

// The example file must use only known keys and show every key. The token
// keys are commented-out examples (0003 SPEC Design 설정 파일); they are
// checked with the comment marks removed.
func TestExampleConfigListsEveryKey(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "config.example.toml"))
	if err != nil {
		t.Fatal(err)
	}
	const commented = `# token-file = "<absolute-path-to-token-file>"`
	if n := strings.Count(string(data), commented); n != 2 {
		t.Fatalf("example has %d commented token-file lines, want 2 (under [github] and [[targets]])", n)
	}
	uncommented := strings.ReplaceAll(string(data), "# token-file = ", "token-file = ")
	for _, content := range []string{string(data), uncommented} {
		if _, unknown, _, err := decode([]byte(content), "config"); err != nil || len(unknown) != 0 {
			t.Fatalf("decode example: err %v, unknown keys %v", err, unknown)
		}
	}

	var raw map[string]any
	if _, err := toml.Decode(uncommented, &raw); err != nil {
		t.Fatalf("decode example: %v", err)
	}
	for _, section := range tomlKeys(reflect.TypeFor[Config]()) {
		table, ok := raw[section.name].(map[string]any)
		if !ok {
			t.Errorf("example has no [%s] table", section.name)
			continue
		}
		for _, k := range tomlKeys(section.typ) {
			if _, ok := table[k.name]; !ok {
				t.Errorf("example has no %s.%s", section.name, k.name)
			}
		}
	}
	targets, ok := raw["targets"].([]map[string]any)
	if !ok || len(targets) < 1 {
		t.Fatalf("example must have a [[targets]] entry, got %v", raw["targets"])
	}
	for _, k := range tomlKeys(reflect.TypeFor[Target]()) {
		if _, ok := targets[0][k.name]; !ok {
			t.Errorf("example has no targets.%s", k.name)
		}
	}
	if _, ok := raw["pools"]; ok {
		t.Error("example still has [[pools]]")
	}
}

// TestExampleConfigLoads proves 0002 SPEC R231, R235: with its placeholders
// filled, the example file loads, and its target gets max-runners 1.
func TestExampleConfigLoads(t *testing.T) {
	cfg, err := loadString(t, exampleTOML(t, writeKey(t, pkcs1PEM(t), 0o600)))
	if err != nil {
		t.Fatalf("Load example: %v", err)
	}
	if len(cfg.Targets) != 1 || cfg.Targets[0] != (Target{URL: "https://github.com/acme/widget", MaxRunners: 1}) {
		t.Errorf("example targets = %+v, want one acme/widget with max-runners 1", cfg.Targets)
	}
}

// exampleTOML returns config.example.toml with its placeholders filled and
// keyPath as the App private key file.
func exampleTOML(t *testing.T, keyPath string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "config.example.toml"))
	if err != nil {
		t.Fatal(err)
	}
	return strings.NewReplacer(
		"<app-client-id>", "test-client-id",
		"<absolute-path-to-app-key>", keyPath,
		"sha256:<runner-image-id>", testRunnerImage,
		"moby/buildkit:<version>-rootless@sha256:<index-digest>", testBuildKitImage,
		"<owner>/<repo>", "acme/widget",
	).Replace(string(data))
}

// broken0002 is a 0002 file, both App keys and no token key, that breaks
// many rules.
func broken0002(keyPath string) string {
	return `unknown-top = 1

[github]
app-client-id = "  "
app-private-key-file = "` + keyPath + `"

[runner]
image = "runner:latest"
memory = "1x"

[buildkit]
image = "moby/buildkit:v0.34.2-rootless"
cpus = 0

[jobcheck]
listen = 87654321

[scaling]
label = "Bad_Label"

[shutdown]
drain-timeout = "0s"

[[targets]]
url = "https://github.com/Acme/Widget"
max-runners = "two"
runner-group = 1

[[targets]]
url = "https://github.com/acme/widget"
idle = 5

[[targets]]
url = "https://github.com/acme"

[[targets]]
url = "https://github.com/acme/x.git"
typo = 2
`
}

// want0002Errors is the error that the 0002 code (commit 31a9a10) gave for
// broken0002 with a key file of mode 0644 holding no PEM, the config file
// path written as <path>.
const want0002Errors = `config <path>: jobcheck.listen: toml: line 16 (last key "jobcheck.listen"): incompatible types: TOML value has type int64; destination has type string
runner.memory: invalid size "1x": want digits with an optional b, k, m or g suffix
config <path>: targets[0].max-runners: toml: line 26 (last key "targets.max-runners"): incompatible types: TOML value has type string; destination has type integer
targets[0].runner-group: no longer used; repository scale sets always use the default runner group, remove the key
unknown key "targets[3].typo"
unknown key "unknown-top"
github.app-client-id: is required
github.app-private-key-file: file mode -rw-r--r-- is wider than 0600 or has special bits; use 0600 or 0400
github.app-private-key-file: no PEM block found
runner.image: must be a local image ID sha256:<64 hex digits>, got "runner:latest"
buildkit.image: must be moby/buildkit:<version>-rootless@sha256:<64 hex digits>, got "moby/buildkit:v0.34.2-rootless"
buildkit.cpus: must be at least 0.01 and fit in int64 nanoCPUs, got 0
shutdown.drain-timeout: must be greater than 0, got 0s
scaling.label: must be 1-40 characters of a-z, 0-9 and -, starting with a-z or 0-9, got "Bad_Label"
targets[1].idle: must be between 0 and max-runners (1), got 5
targets[2].url: "https://github.com/acme" names an organization or account; list each repository as its own [[targets]] url = "https://github.com/<owner>/<repo>" and set its max-runners (default 1)
targets[3].url: must be https://github.com/<owner>/<repo> without a trailing "/" or ".git", got "https://github.com/acme/x.git"
targets: repository listed more than once (owner and repository names compare without case): https://github.com/Acme/Widget, https://github.com/acme/widget`

// TestLoad0002FormatUnchanged proves 0003 SPEC R271: the example file and a
// 0002 file load to the 0002 values with no token file, and a broken 0002
// file gives the 0002 error text.
func TestLoad0002FormatUnchanged(t *testing.T) {
	key := writeKey(t, pkcs1PEM(t), 0o600)
	want := Config{
		GitHub:   GitHub{AppClientID: "test-client-id", AppPrivateKeyFile: key},
		Runner:   Container{Image: testRunnerImage, CPUs: 1, Memory: 1 << 30, Pids: 512},
		BuildKit: Container{Image: testBuildKitImage, CPUs: 2, Memory: 2 << 30, Pids: 1024},
		JobCheck: JobCheck{Listen: "127.0.0.1:8765"},
		Shutdown: Shutdown{DrainTimeout: 10 * time.Minute},
		Scaling:  Scaling{Label: "linux-arm64-deploy"},
		Targets:  []Target{{URL: "https://github.com/acme/widget", MaxRunners: 1}},
	}
	for name, content := range map[string]string{
		"example":   exampleTOML(t, key),
		"0002 file": requiredTOML(key),
	} {
		cfg, err := loadString(t, content)
		if err != nil {
			t.Fatalf("%s: Load: %v", name, err)
		}
		if !reflect.DeepEqual(*cfg, want) {
			t.Errorf("%s: Load:\n got %+v\nwant %+v", name, *cfg, want)
		}
	}

	path := writeFile(t, "config.toml", []byte(broken0002(writeKey(t, []byte("garbage"), 0o644))), 0o600)
	_, err := Load(path)
	if err == nil {
		t.Fatal("Load of the broken 0002 file succeeded")
	}
	if got := strings.ReplaceAll(err.Error(), path, "<path>"); got != want0002Errors {
		t.Errorf("Load error:\n%s\nwant the 0002 text:\n%s", got, want0002Errors)
	}
}

type tomlKey struct {
	name string
	typ  reflect.Type
}

func tomlKeys(t reflect.Type) []tomlKey {
	var keys []tomlKey
	for i := range t.NumField() {
		f := t.Field(i)
		if tag := f.Tag.Get("toml"); tag != "" && tag != "-" {
			keys = append(keys, tomlKey{name: tag, typ: f.Type})
		}
	}
	return keys
}
