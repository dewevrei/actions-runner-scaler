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
app-installation-id = 12345
app-private-key-file = "` + keyPath + `"

[runner]
image = "` + testRunnerImage + `"

[buildkit]
image = "` + testBuildKitImage + `"

[[pools]]
repo-url = "https://github.com/test-org/test-repo"
allowed-workflows = [".github/workflows/deploy.yml"]
`
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

	// Literal values from the SPEC.md settings table, not the package
	// constants, so a changed constant fails here.
	want := Config{
		GitHub: GitHub{
			AppClientID:       "test-client-id",
			AppInstallationID: 12345,
			AppPrivateKeyFile: key,
		},
		Runner:   ImageSection{Image: testRunnerImage},
		BuildKit: ImageSection{Image: testBuildKitImage},
		JobCheck: JobCheck{Listen: "127.0.0.1:8765"},
		Shutdown: Shutdown{DrainTimeout: 10 * time.Minute},
		Pools: []Pool{{
			Name:             "linux-arm64-deploy",
			RepoURL:          "https://github.com/test-org/test-repo",
			Max:              2,
			Idle:             0,
			RunnerCPUs:       1,
			RunnerMemory:     1 << 30,
			RunnerPids:       512,
			BuildCPUs:        2,
			BuildMemory:      2 << 30,
			BuildPids:        1024,
			AllowedWorkflows: []string{".github/workflows/deploy.yml"},
			AllowedEvents:    []string{"push"},
		}},
	}
	if !reflect.DeepEqual(*cfg, want) {
		t.Errorf("Load defaults:\n got %+v\nwant %+v", *cfg, want)
	}
}

func TestLoadExplicitValues(t *testing.T) {
	key := writeKey(t, pkcs8PEM(t), 0o400)
	cfg, err := loadString(t, requiredTOML(key)+`
name = "ci-pool"
max = 5
idle = 1
runner-cpus = 1.5
runner-memory = "1536m"
runner-pids = 2048
build-cpus = 4
build-memory = "4g"
build-pids = 4096
allowed-events = ["workflow_dispatch", "schedule"]

[jobcheck]
listen = "[::1]:9000"

[shutdown]
drain-timeout = "1h"
`)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	p := cfg.Pools[0]
	got := []any{p.Name, p.Max, p.Idle, p.RunnerCPUs, p.RunnerMemory, p.RunnerPids,
		p.BuildCPUs, p.BuildMemory, p.BuildPids, p.AllowedEvents, cfg.JobCheck.Listen, cfg.Shutdown.DrainTimeout}
	want := []any{"ci-pool", 5, 1, 1.5, 1536 * Mebibyte, int64(2048),
		4.0, 4 * Gibibyte, int64(4096), []string{"workflow_dispatch", "schedule"}, "[::1]:9000", time.Hour}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Load explicit values:\n got %v\nwant %v", got, want)
	}
}

// Defaults must not leak between pools or between calls.
func TestDefaultPoolReturnsFreshSlices(t *testing.T) {
	a := DefaultPool()
	a.AllowedEvents[0] = "schedule"
	if got := DefaultPool().AllowedEvents; !reflect.DeepEqual(got, []string{"push"}) {
		t.Errorf("DefaultPool().AllowedEvents = %v after mutating a copy", got)
	}
}

func TestLoadCollectsAllErrors(t *testing.T) {
	key := writeKey(t, pkcs1PEM(t), 0o600)
	content := strings.Replace(requiredTOML(key), "[runner]", "unknown-one = true\n\n[runner]", 1) + `
max = 0
unknown-two = "x"
`
	_, err := loadString(t, content)
	errs := errorList(err)
	if len(errs) != 3 {
		t.Fatalf("Load returned %d errors, want 3: %v", len(errs), err)
	}
	for _, want := range []string{`"github.unknown-one"`, `"pools[0].unknown-two"`, "pools[0].max"} {
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
max = 0

[jobcheck]
listen = 87654321
`
	_, err := loadString(t, content)
	errs := errorList(err)
	if len(errs) != 3 {
		t.Fatalf("Load returned %d errors, want 3: %v", len(errs), err)
	}
	for _, want := range []string{`"jobcheck.listen"`, "incompatible types", `"github.unknown-one"`, "pools[0].max"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("Load error does not mention %s:\n%v", want, err)
		}
	}
	if strings.Contains(err.Error(), "87654321") {
		t.Errorf("error quotes the value: %v", err)
	}
}

func TestLoadTypeErrorInPool(t *testing.T) {
	key := writeKey(t, pkcs1PEM(t), 0o600)
	tests := []struct {
		name string
		add  string // appended to the [[pools]] table
		want []string
	}{
		// The bad max keeps its default, so idle is still checked.
		{name: "pool key", add: "max = \"SECRETVALUE\"\nidle = -1\n", want: []string{
			"pools[0].max: ", `unknown key "unknown-top"`, "pools[0].idle: must be between",
		}},
		{name: "pool entry", add: "allowed-events = [1]\n", want: []string{
			"pools[0].allowed-events: ", `unknown key "unknown-top"`,
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
max = "two"
runner-pids = true
allowed-events = "push"
typo = 1
idle = -1
`), 0o600)
	want := []string{
		// Decoding problems in key order, then validation.
		"pools[0].allowed-events: ",
		"pools[0].max: ",
		"pools[0].runner-pids: ",
		`unknown key "pools[0].typo"`,
		"pools[0].idle: must be between",
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

// A table or the pools array given as another TOML type is a type error,
// not a stop of the whole decoder or a misleading pool count error.
func TestLoadTableOfWrongType(t *testing.T) {
	key := writeKey(t, pkcs1PEM(t), 0o600)
	base := requiredTOML(key)
	poolStart := strings.Index(base, "[[pools]]")
	tests := []struct {
		name    string
		content string
		want    string
	}{
		{name: "table", content: "jobcheck = 5\n" + base, want: "jobcheck: "},
		{name: "pools", content: "pools = 5\n" + base[:poolStart], want: "pools: "},
		{name: "pool entry", content: "pools = [5]\n" + base[:poolStart], want: "pools[0]: "},
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

func TestLoadPoolCount(t *testing.T) {
	key := writeKey(t, pkcs1PEM(t), 0o600)
	base := requiredTOML(key)
	poolStart := strings.Index(base, "[[pools]]")
	onePool := base[poolStart:]

	tests := []struct {
		name    string
		content string
		ok      bool
	}{
		{name: "zero", content: base[:poolStart]},
		{name: "empty array", content: "pools = []\n" + base[:poolStart]},
		{name: "one", content: base, ok: true},
		{name: "two", content: base + "\n" + onePool},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := loadString(t, tt.content)
			if tt.ok {
				if err != nil {
					t.Fatalf("Load: %v", err)
				}
				return
			}
			errs := errorList(err)
			if len(errs) != 1 || !strings.Contains(err.Error(), "exactly 1 pool") {
				t.Fatalf("Load error = %v, want only the pool count error", err)
			}
		})
	}
}

func TestLoadUnknownKeys(t *testing.T) {
	key := writeKey(t, pkcs1PEM(t), 0o600)
	base := requiredTOML(key)
	tests := []struct {
		name string
		add  string // appended to the [[pools]] table unless it starts a table
		want []string
	}{
		{name: "pool key", add: "maximum = 3\n", want: []string{`"pools[0].maximum"`}},
		{name: "upper-case pool key", add: "MAX = 3\n", want: []string{`"pools[0].MAX"`}},
		{name: "unknown table reported once", add: "[cache]\nsize = 1\npath = \"x\"\n", want: []string{`"cache"`}},
		{name: "upper-case table", add: "[JobCheck]\nlisten = \"127.0.0.1:1\"\n", want: []string{`"JobCheck"`}},
		{name: "key in known table", add: "[shutdown]\ngrace = \"1m\"\n", want: []string{`"shutdown.grace"`}},
	}
	for _, tt := range tests {
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
		{name: "40 characters shown", add: strings.Repeat("y", 40) + " = 1\n", want: `"pools[0].` + strings.Repeat("y", 40) + `"`},
		{name: "longer than 40", add: long + " = 1\n", want: `"pools[0].fake…(41 chars)"`, leak: long},
		{name: "unusual characters", add: `"ghs_Ab+/=" = 1` + "\n", want: `"pools[0].ghs_…(9 chars)"`, leak: "Ab+/="},
		{name: "unknown table", add: `["tok+en".x]` + "\n", want: `"tok+…(6 chars)"`, leak: "tok+en"},
		{name: "upper case only", add: "Max_Idle = 1\n", want: `"pools[0].Max_Idle"`},
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
		{name: "syntax", add: "max = \n"},
		{name: "wrong type", add: "max = \"two\"\n"},
		{name: "pools as table", add: "[pools]\nname = \"x\"\n"},
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
	content := requiredTOML(key) + `
max = 0
runner-memory = "1x"
build-memory = "1 GB"
typo = 1

[shutdown]
drain-timeout = 600
`
	_, err := loadString(t, content)
	errs := errorList(err)
	want := []string{
		`unknown key "pools[0].typo"`,
		"pools[0].runner-memory: invalid size",
		"pools[0].build-memory: invalid size",
		"shutdown.drain-timeout: invalid duration",
		"pools[0].max: must be at least 1",
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
	cfg, err := loadString(t, requiredTOML(key)+"runner-memory = 536870912\n")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := cfg.Pools[0].RunnerMemory; got != 512*Mebibyte {
		t.Errorf("runner-memory = %s, want 512m", got)
	}
}

// A syntax error must not quote the offending text, which may be a pasted
// secret (R31).
func TestLoadSyntaxErrorHidesValue(t *testing.T) {
	key := writeKey(t, pkcs1PEM(t), 0o600)
	const secret = "tSECRETVALUE"
	_, err := loadString(t, requiredTOML(key)+"max = "+secret+"\n")
	if err == nil {
		t.Fatal("Load succeeded, want syntax error")
	}
	if strings.Contains(err.Error(), "SECRETVALUE") {
		t.Errorf("error quotes the value: %v", err)
	}
	if !strings.Contains(err.Error(), "line ") || !strings.Contains(err.Error(), `max"`) {
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
		add  string
		want string
	}{
		{add: "max = 0\n", want: "pools[0].max"},
		{add: "runner-cpus = 0\n", want: "pools[0].runner-cpus"},
		{add: "build-pids = 0\n", want: "pools[0].build-pids"},
		{add: "name = \"\"\n", want: "pools[0].name"},
		{add: "allowed-events = []\n", want: "pools[0].allowed-events"},
		{add: "[shutdown]\ndrain-timeout = \"0s\"\n", want: "shutdown.drain-timeout"},
		{add: "[jobcheck]\nlisten = \"\"\n", want: "jobcheck.listen"},
	}
	for _, tt := range tests {
		t.Run(tt.want, func(t *testing.T) {
			_, err := loadString(t, requiredTOML(key)+tt.add)
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

// The example file must use only known keys and show every key.
func TestExampleConfigListsEveryKey(t *testing.T) {
	path := filepath.Join("..", "..", "config.example.toml")
	if _, unknown, _, err := decodeFile(path); err != nil || len(unknown) != 0 {
		t.Fatalf("decode example: err %v, unknown keys %v", err, unknown)
	}

	var raw map[string]any
	if _, err := toml.DecodeFile(path, &raw); err != nil {
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
	pools, ok := raw["pools"].([]map[string]any)
	if !ok || len(pools) != 1 {
		t.Fatalf("example must have exactly one [[pools]] entry, got %v", raw["pools"])
	}
	for _, k := range tomlKeys(reflect.TypeFor[Pool]()) {
		if _, ok := pools[0][k.name]; !ok {
			t.Errorf("example has no pools.%s", k.name)
		}
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
