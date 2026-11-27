package configfile

import (
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dewevrei/actions-runner-scaler/internal/config"
)

// renderTestKey is a throwaway key generated once per test binary.
var renderTestKey = sync.OnceValues(func() (*rsa.PrivateKey, error) {
	return rsa.GenerateKey(rand.Reader, 2048)
})

// renderFiles are the secret files the values of a test point at.
type renderFiles struct {
	key, token string
}

// newRenderFiles writes an App key file and a token file, both mode 0600.
// The token is built here, so no token-shaped literal is committed.
func newRenderFiles(t *testing.T) renderFiles {
	t.Helper()
	key, err := renderTestKey()
	if err != nil {
		t.Fatalf("generate RSA key: %v", err)
	}
	dir := t.TempDir()
	f := renderFiles{key: filepath.Join(dir, "app.pem"), token: filepath.Join(dir, "token")}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	for path, data := range map[string][]byte{f.key: keyPEM, f.token: []byte("ghp_" + strings.Repeat("aB3dE5", 6) + "\n")} {
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(path, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return f
}

const (
	renderRunnerImage   = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	renderBuildKitImage = "moby/buildkit:v0.35.1-rootless@sha256:7d02292556e66feea6d2d89b7ec143503b57e4f59a4efc6b7ece8547e4d3e5be"
)

func renderValues(f renderFiles, token bool) Values {
	v := Values{
		RunnerImage:   renderRunnerImage,
		BuildKitImage: renderBuildKitImage,
		Targets: []TargetEntry{
			{URL: "https://github.com/acme/widget", MaxRunners: 1},
			{URL: "https://github.com/Acme/Gadget", MaxRunners: 3},
		},
	}
	if token {
		v.TokenFile = f.token
	} else {
		v.AppClientID = "Iv1.0123456789abcdef"
		v.AppPrivateKeyFile = f.key
	}
	return v
}

// wantConfig is what Render's output must load to: v and the defaults.
func wantConfig(v Values) config.Config {
	c := config.Default()
	c.GitHub = config.GitHub{AppClientID: v.AppClientID, AppPrivateKeyFile: v.AppPrivateKeyFile, TokenFile: v.TokenFile}
	c.Runner.Image = v.RunnerImage
	c.BuildKit.Image = v.BuildKitImage
	for _, t := range v.Targets {
		idle := config.DefaultIdle
		if t.Idle != nil {
			idle = *t.Idle
		}
		c.Targets = append(c.Targets, config.Target{URL: t.URL, MaxRunners: t.MaxRunners, Idle: idle, TokenFile: t.TokenFile})
	}
	return c
}

// 0003 SPEC R283: the file loads, with the given values and the defaults.
func TestRenderLoads(t *testing.T) {
	f := newRenderFiles(t)
	idle := 2
	tests := []struct {
		name string
		v    Values
	}{
		{"App", renderValues(f, false)},
		{"token", renderValues(f, true)},
		{"target keys", Values{
			AppClientID: "x", AppPrivateKeyFile: f.key, RunnerImage: renderRunnerImage, BuildKitImage: renderBuildKitImage,
			Targets: []TargetEntry{{URL: "https://github.com/acme/widget", MaxRunners: 2, Idle: &idle, TokenFile: f.token}},
		}},
		{"image references", Values{
			TokenFile: f.token, RunnerImage: "actions-runner-scaler-runner:local",
			BuildKitImage: "moby/buildkit@sha256:7d02292556e66feea6d2d89b7ec143503b57e4f59a4efc6b7ece8547e4d3e5be",
			Targets:       []TargetEntry{{URL: "https://github.com/acme/widget", MaxRunners: 1}},
		}},
		{"awkward client ID", Values{
			AppClientID: "a\"b\\c'd", AppPrivateKeyFile: f.key, RunnerImage: renderRunnerImage, BuildKitImage: renderBuildKitImage,
			Targets: []TargetEntry{{URL: "https://github.com/acme/widget", MaxRunners: 1}},
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			data := Render(tt.v)
			path := filepath.Join(t.TempDir(), "config.toml")
			if err := os.WriteFile(path, data, 0o600); err != nil {
				t.Fatal(err)
			}
			want := wantConfig(tt.v)
			for name, load := range map[string]func() (*config.Config, error){
				"Load":   func() (*config.Config, error) { return config.Load(path) },
				"Decode": func() (*config.Config, error) { return config.Decode(data) },
			} {
				got, err := load()
				if err != nil {
					t.Fatalf("%s: %v\n%s", name, err, data)
				}
				if !reflect.DeepEqual(*got, want) {
					t.Errorf("%s = %+v, want %+v", name, *got, want)
				}
			}
		})
	}
}

var tableHeader = regexp.MustCompile(`^\[{1,2}[a-z]+\]{1,2}$`)

// tableOrder returns the table headers of a TOML text in file order,
// ignoring comment lines.
func tableOrder(text string) []string {
	var order []string
	for _, line := range strings.Split(text, "\n") {
		if tableHeader.MatchString(line) {
			order = append(order, line)
		}
	}
	return order
}

// defaultKeys returns "<table>.<key>" of every key a text writes with a
// "# default" comment.
func defaultKeys(text string) map[string]bool {
	keys := make(map[string]bool)
	table := ""
	for _, line := range strings.Split(text, "\n") {
		if tableHeader.MatchString(line) {
			table = strings.Trim(line, "[]")
			continue
		}
		key, rest, ok := strings.Cut(line, " = ")
		if ok && !strings.HasPrefix(key, "#") && strings.Contains(rest, "# default") {
			keys[table+"."+key] = true
		}
	}
	return keys
}

// The file has the tables of config.example.toml in its order, and a
// "# default" comment on the same keys (0003 SPEC R283, Design init).
func TestRenderFollowsExample(t *testing.T) {
	example, err := os.ReadFile("../../config.example.toml")
	if err != nil {
		t.Fatal(err)
	}
	// One repository, as the example has.
	one := string(Render(Values{
		AppClientID: "x", AppPrivateKeyFile: "/k", RunnerImage: renderRunnerImage, BuildKitImage: renderBuildKitImage,
		Targets: []TargetEntry{{URL: "https://github.com/acme/widget", MaxRunners: 1}},
	}))
	if want, have := tableOrder(string(example)), tableOrder(one); !reflect.DeepEqual(have, want) {
		t.Errorf("table order = %v, want %v", have, want)
	}
	want, have := defaultKeys(string(example)), defaultKeys(one)
	if !reflect.DeepEqual(have, want) {
		t.Errorf("keys marked default = %v, want %v", have, want)
	}
	if len(want) == 0 {
		t.Fatal("the example has no key marked default")
	}
}

// imageComments returns the comment lines directly above the image key of
// each table of text, by table name.
func imageComments(text string) map[string][]string {
	out := make(map[string][]string)
	table := ""
	var comments []string
	for _, line := range strings.Split(text, "\n") {
		switch {
		case tableHeader.MatchString(line):
			table, comments = strings.Trim(line, "[]"), nil
		case strings.HasPrefix(line, "#"):
			comments = append(comments, line)
		case strings.HasPrefix(line, "image = "):
			out[table] = comments
			comments = nil
		default:
			comments = nil
		}
	}
	return out
}

// 0003 SPEC R318, R320: the comment above each image key, in the rendered
// file and in the example, says it takes an image reference or a local
// image ID and that a missing reference is pulled; the two files carry the
// same lines.
func TestRenderImageComments(t *testing.T) {
	example, err := os.ReadFile("../../config.example.toml")
	if err != nil {
		t.Fatal(err)
	}
	rendered := string(Render(Values{
		AppClientID: "x", AppPrivateKeyFile: "/k", RunnerImage: renderRunnerImage, BuildKitImage: renderBuildKitImage,
		Targets: []TargetEntry{{URL: "https://github.com/acme/widget", MaxRunners: 1}},
	}))
	want, have := imageComments(string(example)), imageComments(rendered)
	if !reflect.DeepEqual(have, want) {
		t.Errorf("image comments = %q, want the example's %q", have, want)
	}
	for _, table := range []string{"runner", "buildkit"} {
		text := strings.Join(want[table], "\n")
		for _, phrase := range []string{"image reference <name>[:<tag>][@<digest>]", "local image ID", "missing locally is pulled"} {
			if !strings.Contains(text, phrase) {
				t.Errorf("[%s] image comment lacks %q:\n%s", table, phrase, text)
			}
		}
	}
	if !strings.Contains(string(example), `image = "actions-runner-scaler-runner:local"`) {
		t.Error("the example's [runner] image is not actions-runner-scaler-runner:local")
	}
}

// keyLine returns the first line of text that starts with prefix.
func keyLine(text, prefix string) string {
	for _, line := range strings.Split(text, "\n") {
		if strings.HasPrefix(line, prefix) {
			return line
		}
	}
	return ""
}

// The keys added by the convention fixes, scaling.server-limit, docker.host
// and log.level, are written with their default and the same comment as in
// the example, and the rendered file loads to their defaults.
func TestRenderNewKeysMatchExample(t *testing.T) {
	example, err := os.ReadFile("../../config.example.toml")
	if err != nil {
		t.Fatal(err)
	}
	got := string(Render(Values{
		AppClientID: "x", AppPrivateKeyFile: "/k", RunnerImage: renderRunnerImage, BuildKitImage: renderBuildKitImage,
		Targets: []TargetEntry{{URL: "https://github.com/acme/widget", MaxRunners: 1}},
	}))
	for _, prefix := range []string{"server-limit = ", "host = ", "level = "} {
		want := keyLine(string(example), prefix)
		if want == "" || !strings.Contains(want, "# default;") {
			t.Fatalf("the example has no %q line with a default comment: %q", prefix, want)
		}
		if have := keyLine(got, prefix); have != want {
			t.Errorf("rendered %q line = %q, want the example's %q", prefix, have, want)
		}
	}
}

// buildkit.enabled is written first in [buildkit], false with the default
// mark and the example's comment (TestRenderLoads shows it loads as off).
func TestRenderBuildKitEnabled(t *testing.T) {
	example, err := os.ReadFile("../../config.example.toml")
	if err != nil {
		t.Fatal(err)
	}
	got := string(Render(Values{
		AppClientID: "x", AppPrivateKeyFile: "/k", RunnerImage: renderRunnerImage, BuildKitImage: renderBuildKitImage,
		Targets: []TargetEntry{{URL: "https://github.com/acme/widget", MaxRunners: 1}},
	}))
	want := "[buildkit]\n" + buildKitEnabledComment + "enabled = false # default\n"
	for name, text := range map[string]string{"rendered": got, "example": string(example)} {
		if !strings.Contains(text, want) {
			t.Errorf("%s file lacks %q", name, want)
		}
	}
}

// A key that is not at its default has no "# default" comment, and the
// comments of a token file replace the App's.
func TestRenderDefaultMarks(t *testing.T) {
	got := string(Render(renderValues(renderFiles{key: "/k", token: "/t"}, true)))
	for _, line := range []string{
		"max-runners = 1 # default\n",
		"max-runners = 3\n",
		"cpus = 1 # default\n",
		"memory = \"2g\" # default\n",
		"drain-timeout = \"10m\" # default\n",
		"label = \"linux-arm64-deploy\" # default; scale set name and runs-on label\n",
	} {
		if !strings.Contains(got, line) {
			t.Errorf("output lacks %q:\n%s", line, got)
		}
	}
	if n := strings.Count(got, "idle = 0 # default\n"); n != 2 {
		t.Errorf("idle = 0 # default appears %d times, want once per target", n)
	}
	for _, absent := range []string{"app-client-id", "app-private-key-file"} {
		if strings.Contains(got, absent+" =") {
			t.Errorf("a token file output sets %s", absent)
		}
	}
}

// Each repository is its TargetBlock, with only the default marks added.
func TestRenderTargetsAreTargetBlocks(t *testing.T) {
	idle := 1
	targets := []TargetEntry{
		{URL: "https://github.com/acme/widget", MaxRunners: 4, Idle: &idle, TokenFile: "/path/to/token"},
		{URL: "https://github.com/acme/gadget", MaxRunners: 1},
	}
	got := string(Render(Values{Targets: targets}))
	for _, t0 := range targets {
		block := TargetBlock(t0)
		if t0.Idle == nil {
			block = strings.Replace(block, "max-runners = 1\n", "max-runners = 1 # default\nidle = 0 # default\n", 1)
		}
		if !strings.Contains(got, "\n"+block) {
			t.Errorf("output lacks the block of %s:\n%s", t0.URL, block)
		}
	}
	if strings.Contains(got, "max-runners = 4 # default") || strings.Contains(got, "idle = 1 # default") {
		t.Errorf("a value that is not the default is marked default:\n%s", got)
	}
}

func TestRenderNoSecretValues(t *testing.T) {
	f := newRenderFiles(t)
	token, err := os.ReadFile(f.token)
	if err != nil {
		t.Fatal(err)
	}
	got := Render(renderValues(f, true))
	if bytes.Contains(got, bytes.TrimSpace(token)) {
		t.Error("the file holds the token")
	}
}

func TestShortDuration(t *testing.T) {
	tests := []struct {
		d    time.Duration
		want string
	}{
		{10 * time.Minute, "10m"},
		{time.Hour, "1h"},
		{90 * time.Minute, "1h30m"},
		{30 * time.Second, "30s"},
		{90 * time.Second, "1m30s"},
	}
	for _, tt := range tests {
		if got := shortDuration(tt.d); got != tt.want {
			t.Errorf("shortDuration(%v) = %q, want %q", tt.d, got, tt.want)
		}
		// The shortened text parses back to the same duration.
		if back, err := time.ParseDuration(shortDuration(tt.d)); err != nil || back != tt.d {
			t.Errorf("shortDuration(%v) = %q parses to %v, %v", tt.d, shortDuration(tt.d), back, err)
		}
	}
}
