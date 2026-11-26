package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"

	"github.com/dewevrei/actions-runner-scaler/internal/config"
	"github.com/dewevrei/actions-runner-scaler/internal/docker"
)

// initTestKey is a throwaway key generated once per test binary.
var initTestKey = sync.OnceValues(func() (*rsa.PrivateKey, error) {
	return rsa.GenerateKey(rand.Reader, 2048)
})

// initKeyFile writes an App private key file of mode 0600 and returns its
// path.
func initKeyFile(t *testing.T) string {
	t.Helper()
	key, err := initTestKey()
	if err != nil {
		t.Fatalf("generate RSA key: %v", err)
	}
	data := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	return initSecretFile(t, "app.pem", data, 0o600)
}

// initSecretFile writes data to a file of the exact mode in a new directory.
func initSecretFile(t *testing.T, name string, data []byte, mode fs.FileMode) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
	return path
}

const initTestImageID = "sha256:abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789"

// fakeInitImages is the Docker side of init. It records the refs it was
// asked about.
type fakeInitImages struct {
	images map[string]docker.Image
	// err fails every lookup.
	err error
	// onInspect runs at the start of each lookup.
	onInspect func()
	calls     []string
	closed    bool
}

var _ initImageInspector = (*fakeInitImages)(nil)

func (f *fakeInitImages) ImageInspect(_ context.Context, ref string) (docker.Image, bool, error) {
	f.calls = append(f.calls, ref)
	if f.onInspect != nil {
		f.onInspect()
	}
	if f.err != nil {
		return docker.Image{}, false, f.err
	}
	img, ok := f.images[ref]
	return img, ok, nil
}

func (f *fakeInitImages) Close() error {
	f.closed = true
	return nil
}

// labelledRunnerImage is what image/build.sh leaves under the runner tag.
func labelledRunnerImage() docker.Image {
	return docker.Image{ID: initTestImageID, Architecture: "arm64", Labels: docker.RunnerImageLabels()}
}

// initFakeImages has the labelled runner image and the BuildKit image.
func initFakeImages() *fakeInitImages {
	return &fakeInitImages{images: map[string]docker.Image{
		docker.RunnerImageTag: labelledRunnerImage(),
		docker.BuildKitImage:  {ID: "sha256:" + strings.Repeat("b", 64), Architecture: "arm64"},
	}}
}

// initHarness runs init on fakes.
type initHarness struct {
	t        *testing.T
	images   *fakeInitImages
	stdin    string
	terminal bool
	// imagesOpened counts the connections to Docker.
	imagesOpened int
	// imagesErr fails the connection itself.
	imagesErr error
}

func newInitHarness(t *testing.T) *initHarness {
	t.Helper()
	return &initHarness{t: t, images: initFakeImages()}
}

func (h *initHarness) deps() initDeps {
	return initDeps{
		Images: func() (initImageInspector, error) {
			h.imagesOpened++
			if h.imagesErr != nil {
				return nil, h.imagesErr
			}
			return h.images, nil
		},
		Stdin:    strings.NewReader(h.stdin),
		Terminal: h.terminal,
	}
}

// run runs init with args and returns the exit code and both streams.
func (h *initHarness) run(args ...string) (code int, stdout, stderr string) {
	h.t.Helper()
	cmds := map[string]command{"init": {run: func(ctx context.Context, e env, args []string) error {
		return runInitCmd(ctx, e, h.deps(), args)
	}}}
	var out, errs bytes.Buffer
	code = dispatch(context.Background(), newEnv(&out, &errs), append([]string{"init"}, args...), cmds)
	return code, out.String(), errs.String()
}

// initFlags are the flags of a complete App configuration with one
// repository and both images.
func initFlagsApp(t *testing.T, path string) []string {
	t.Helper()
	return []string{
		"-config", path,
		"-app-client-id", "Iv1.0123456789abcdef", "-app-private-key-file", initKeyFile(t),
		"-repo", "https://github.com/acme/widget",
		"-runner-image", initTestImageID, "-buildkit-image", docker.BuildKitImage,
	}
}

func initConfigPath(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "config.toml")
}

// assertInitFailed checks exit 1 and that nothing exists at path or its
// directory entries besides the ones in keep.
func assertInitFailed(t *testing.T, code int, path string) {
	t.Helper()
	if code != 1 {
		t.Errorf("exit = %d, want 1", code)
	}
	if _, err := os.Lstat(path); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("config file: Lstat = %v, want it missing", err)
	}
	assertNoTempFiles(t, filepath.Dir(path))
}

// assertNoTempFiles checks that dir, if it exists, holds no entry other than
// the config file.
func assertNoTempFiles(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return
	}
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() != "config.toml" {
			t.Errorf("%s holds %s", dir, e.Name())
		}
	}
}

// assertNoSecret fails if any of streams holds value.
func assertNoSecret(t *testing.T, value string, streams ...string) {
	t.Helper()
	for _, s := range streams {
		if strings.Contains(s, value) {
			t.Errorf("output holds a credential value:\n%s", s)
		}
	}
}

func assertMode(t *testing.T, path string, want fs.FileMode) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != want {
		t.Errorf("%s mode = %v, want %v", path, info.Mode().Perm(), want)
	}
}

// 0003 SPEC R282, R283: every value by flags into a missing file.
func TestInitFlagsOnly(t *testing.T) {
	keyPath := initKeyFile(t)
	tokenPath := writeTokenFile(t, fixtureToken(t, "ghp_"))
	tests := []struct {
		name  string
		extra []string
		check func(t *testing.T, c *config.Config)
	}{
		{"App", []string{"-app-client-id", "Iv1.0123456789abcdef", "-app-private-key-file", keyPath}, func(t *testing.T, c *config.Config) {
			if c.GitHub != (config.GitHub{AppClientID: "Iv1.0123456789abcdef", AppPrivateKeyFile: keyPath}) {
				t.Errorf("github = %+v", c.GitHub)
			}
		}},
		{"token", []string{"-token-file", tokenPath}, func(t *testing.T, c *config.Config) {
			if c.GitHub != (config.GitHub{TokenFile: tokenPath}) {
				t.Errorf("github = %+v", c.GitHub)
			}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newInitHarness(t)
			path := initConfigPath(t)
			args := append([]string{
				"-config", path,
				"-repo", "https://github.com/acme/widget", "-repo", "https://github.com/acme/gadget=3",
				"-runner-image", initTestImageID, "-buildkit-image", docker.BuildKitImage,
			}, tt.extra...)
			code, stdout, stderr := h.run(args...)
			if code != 0 || stderr != "" {
				t.Fatalf("exit = %d, stderr = %q", code, stderr)
			}
			assertMode(t, path, 0o600)
			cfg, err := config.Load(path)
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			tt.check(t, cfg)
			wantTargets := []config.Target{
				{URL: "https://github.com/acme/widget", MaxRunners: 1},
				{URL: "https://github.com/acme/gadget", MaxRunners: 3},
			}
			if !reflect.DeepEqual(cfg.Targets, wantTargets) {
				t.Errorf("targets = %+v, want %+v", cfg.Targets, wantTargets)
			}
			if cfg.Runner.Image != initTestImageID || cfg.BuildKit.Image != docker.BuildKitImage {
				t.Errorf("images = %q, %q", cfg.Runner.Image, cfg.BuildKit.Image)
			}
			def := config.Default()
			if cfg.Scaling != def.Scaling || cfg.JobCheck != def.JobCheck || cfg.Shutdown != def.Shutdown ||
				cfg.Runner.CPUs != def.Runner.CPUs || cfg.BuildKit.Memory != def.BuildKit.Memory {
				t.Errorf("defaults not kept: %+v", cfg)
			}
			if strings.Contains(stdout, "[app/token]") || strings.Contains(stdout, "저장소 URL") {
				t.Errorf("a prompt was written:\n%s", stdout)
			}
			if h.imagesOpened != 0 {
				t.Errorf("Docker was opened with both images given")
			}
			if !strings.Contains(stdout, path) {
				t.Errorf("stdout does not name the file:\n%s", stdout)
			}
		})
	}
}

// 0003 SPEC R282: an existing file, a dangling link included, is left alone.
func TestInitExisting(t *testing.T) {
	tests := []struct {
		name string
		make func(t *testing.T, path string)
	}{
		{"file", func(t *testing.T, path string) {
			if err := os.WriteFile(path, []byte("keep\n"), 0o644); err != nil {
				t.Fatal(err)
			}
		}},
		{"dangling link", func(t *testing.T, path string) {
			if err := os.Symlink(path+".gone", path); err != nil {
				t.Fatal(err)
			}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newInitHarness(t)
			path := initConfigPath(t)
			tt.make(t, path)
			code, stdout, stderr := h.run(initFlagsApp(t, path)...)
			if code != 1 {
				t.Errorf("exit = %d, want 1", code)
			}
			want := "config file " + path + " already exists; edit it, or use add-repo and remove-repo"
			if strings.TrimSpace(stderr) != want {
				t.Errorf("stderr = %q, want %q", stderr, want)
			}
			if stdout != "" {
				t.Errorf("stdout = %q", stdout)
			}
			if data, err := os.ReadFile(path); err == nil && string(data) != "keep\n" {
				t.Errorf("file changed: %q", data)
			}
			if got := initDirNames(t, filepath.Dir(path)); len(got) != 1 {
				t.Errorf("directory holds %v", got)
			}
		})
	}
}

func initDirNames(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

// A file created between the check and the link is kept (0003 SPEC R282).
func TestInitLinkRace(t *testing.T) {
	h := newInitHarness(t)
	path := initConfigPath(t)
	// The Docker lookup runs between the existence check and the link.
	h.images.onInspect = func() {
		if err := os.WriteFile(path, []byte("rival\n"), 0o644); err != nil {
			t.Error(err)
		}
	}
	args := initFlagsApp(t, path)
	args = args[:len(args)-4] // leave both images to Docker
	code, _, stderr := h.run(args...)
	if code != 1 {
		t.Errorf("exit = %d, want 1", code)
	}
	if !strings.Contains(stderr, "config file "+path+" already exists; edit it, or use add-repo and remove-repo") {
		t.Errorf("stderr = %q", stderr)
	}
	if data, err := os.ReadFile(path); err != nil || string(data) != "rival\n" {
		t.Errorf("the other file = %q, %v; want it kept", data, err)
	}
	if got := initDirNames(t, filepath.Dir(path)); !slices.Equal(got, []string{"config.toml"}) {
		t.Errorf("directory holds %v, want only the other file (no temporary file)", got)
	}
}

// 0003 SPEC R283: the parent directory is made, mode 0700, only after every
// check passed, and only one level.
func TestInitParentDir(t *testing.T) {
	t.Run("made", func(t *testing.T) {
		h := newInitHarness(t)
		parent := filepath.Join(t.TempDir(), "actions-runner-scaler")
		path := filepath.Join(parent, "config.toml")
		if code, _, stderr := h.run(initFlagsApp(t, path)...); code != 0 {
			t.Fatalf("exit = %d, stderr = %q", code, stderr)
		}
		assertMode(t, parent, 0o700)
		assertMode(t, path, 0o600)
	})
	t.Run("not made after a flag check failed", func(t *testing.T) {
		h := newInitHarness(t)
		parent := filepath.Join(t.TempDir(), "actions-runner-scaler")
		args := append(initFlagsApp(t, filepath.Join(parent, "config.toml")), "-repo", "https://github.com/acme")
		code, _, _ := h.run(args...)
		assertInitFailed(t, code, filepath.Join(parent, "config.toml"))
		if _, err := os.Lstat(parent); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("the directory was made: %v", err)
		}
	})
	t.Run("not made after Decode failed", func(t *testing.T) {
		h := newInitHarness(t)
		parent := filepath.Join(t.TempDir(), "actions-runner-scaler")
		path := filepath.Join(parent, "config.toml")
		args := initFlagsApp(t, path)
		keyPath := args[slices.Index(args, "-app-private-key-file")+1]
		args = args[:len(args)-4]
		// The key file passed the flag check; it fails when the rendered
		// file is read back.
		h.images.onInspect = func() {
			if err := os.Chmod(keyPath, 0o644); err != nil {
				t.Error(err)
			}
		}
		code, _, stderr := h.run(args...)
		assertInitFailed(t, code, path)
		if _, err := os.Lstat(parent); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("the directory was made: %v", err)
		}
		if !strings.Contains(stderr, "wider than 0600") {
			t.Errorf("stderr = %q", stderr)
		}
	})
	t.Run("grandparent missing", func(t *testing.T) {
		h := newInitHarness(t)
		root := t.TempDir()
		path := filepath.Join(root, "a", "b", "config.toml")
		code, _, stderr := h.run(initFlagsApp(t, path)...)
		if code != 1 {
			t.Errorf("exit = %d, want 1", code)
		}
		if !strings.Contains(stderr, "create the parent first") {
			t.Errorf("stderr = %q", stderr)
		}
		if got := initDirNames(t, root); len(got) != 0 {
			t.Errorf("made %v", got)
		}
	})
}

// 0003 SPEC R284, R314, R277: the checks of -repo values.
func TestInitRepoFlag(t *testing.T) {
	tokenURL := "https://github.com/acme/" + fixtureToken(t, "ghp_")
	patURL := "https://github.com/acme/" + fixtureToken(t, "github_pat_")
	edgeURL := "https://github.com/acme/ghp_" + strings.Repeat("a", 20)
	shortURL := "https://github.com/acme/ghp_" + strings.Repeat("a", 19)
	tests := []struct {
		name string
		args []string
		// wantErr is in stderr; "" means the run succeeds with wantRepos.
		wantErr   string
		wantRepos map[string]int
		// secret is a value no stream may hold.
		secret string
	}{
		{"URL only", []string{"-repo", "https://github.com/acme/widget"}, "", map[string]int{"https://github.com/acme/widget": 1}, ""},
		{"URL and max-runners", []string{"-repo", "https://github.com/acme/widget=3"}, "", map[string]int{"https://github.com/acme/widget": 3}, ""},
		{"two repositories", []string{"-repo", "https://github.com/acme/widget=2", "-repo", "https://github.com/acme/gadget"}, "",
			map[string]int{"https://github.com/acme/widget": 2, "https://github.com/acme/gadget": 1}, ""},
		{"zero", []string{"-repo", "https://github.com/acme/widget=0"}, "max-runners must be an integer of 1 or more", nil, ""},
		{"not a number", []string{"-repo", "https://github.com/acme/widget=x"}, "max-runners must be an integer of 1 or more", nil, ""},
		{"empty number", []string{"-repo", "https://github.com/acme/widget="}, "max-runners must be an integer of 1 or more", nil, ""},
		{"organization", []string{"-repo", "https://github.com/acme"}, "names an organization or account", nil, ""},
		{"not a repository URL", []string{"-repo", "github.com/acme/widget"}, "must be https://github.com/<owner>/<repo>", nil, ""},
		{"overlap", []string{"-repo", "https://github.com/acme/widget", "-repo", "https://github.com/Acme/Widget=2"}, "listed more than once", nil, ""},
		{"@ in the URL", []string{"-repo", "https://user:pass@github.com/acme/widget"}, "value contains a credential; set the repository URL only", nil, "user:pass"},
		{"token in the URL", []string{"-repo", tokenURL}, "value contains a credential; set the repository URL only", nil, tokenURL},
		{"token in the URL and a number", []string{"-repo", tokenURL + "=2"}, "value contains a credential; set the repository URL only", nil, tokenURL},
		{"fine-grained token in the URL", []string{"-repo", patURL}, "value contains a credential; set the repository URL only", nil, patURL},
		{"prefix and 20 characters", []string{"-repo", edgeURL}, "value contains a credential; set the repository URL only", nil, edgeURL},
		// A repository named like a token prefix is not a credential.
		{"prefix and 19 characters", []string{"-repo", shortURL}, "", map[string]int{shortURL: 1}, ""},
		{"token as the number", []string{"-repo", "https://github.com/acme/widget=" + fixtureToken(t, "ghp_")}, "max-runners must be an integer of 1 or more", nil, fixtureToken(t, "ghp_")},
		{"token file with an App flag", []string{"-repo", "https://github.com/acme/widget", "-token-file", writeTokenFile(t, fixtureToken(t, "ghp_")), "-app-client-id", "x"}, "-token-file cannot be combined", nil, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newInitHarness(t)
			path := initConfigPath(t)
			base := []string{"-config", path, "-runner-image", initTestImageID, "-buildkit-image", docker.BuildKitImage}
			if !slices.Contains(tt.args, "-token-file") {
				base = append(base, "-app-client-id", "x", "-app-private-key-file", initKeyFile(t))
			}
			code, stdout, stderr := h.run(append(base, tt.args...)...)
			if tt.secret != "" {
				assertNoSecret(t, tt.secret, stdout, stderr)
			}
			if strings.Contains(stderr, "invalid value") {
				t.Errorf("the flag package printed a value:\n%s", stderr)
			}
			if tt.wantErr != "" {
				assertInitFailed(t, code, path)
				if !strings.Contains(stderr, tt.wantErr) {
					t.Errorf("stderr = %q, want it to contain %q", stderr, tt.wantErr)
				}
				return
			}
			if code != 0 {
				t.Fatalf("exit = %d, stderr = %q", code, stderr)
			}
			cfg, err := config.Load(path)
			if err != nil {
				t.Fatal(err)
			}
			got := make(map[string]int)
			for _, target := range cfg.Targets {
				got[target.URL] = target.MaxRunners
			}
			if !reflect.DeepEqual(got, tt.wantRepos) {
				t.Errorf("repositories = %v, want %v", got, tt.wantRepos)
			}
		})
	}
}

// The -repo flag only collects its values, so Go's flag package has no
// error to print them with (0003 SPEC R314).
func TestInitRepoFlagsSetNeverFails(t *testing.T) {
	var r initRepoFlags
	for _, v := range []string{"", "x", "https://user@github.com/a/b", fixtureToken(t, "ghp_"), "a=b=c"} {
		if err := r.Set(v); err != nil {
			t.Errorf("Set(%q) = %v", v, err)
		}
	}
	if len(r) != 5 || r.String() != "" {
		t.Errorf("collected %d values, String = %q", len(r), r.String())
	}
	// -repo with no value is the flag package's own error and prints no value.
	h := newInitHarness(t)
	code, _, stderr := h.run("-repo")
	if code != 1 || !strings.Contains(stderr, "flag needs an argument: -repo") {
		t.Errorf("exit = %d, stderr = %q", code, stderr)
	}
}

// Several wrong -repo values are all reported, each by its position.
func TestInitRepoFlagReportsEach(t *testing.T) {
	h := newInitHarness(t)
	path := initConfigPath(t)
	args := initFlagsApp(t, path)
	args = append(args, "-repo", "https://github.com/acme", "-repo", "https://u@github.com/acme/x")
	code, _, stderr := h.run(args...)
	assertInitFailed(t, code, path)
	for _, want := range []string{"-repo #2: ", "-repo #3: value contains a credential"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("stderr lacks %q:\n%s", want, stderr)
		}
	}
}

// 0003 SPEC R288: a key or token file named by a flag must pass its rules.
func TestInitBadKeyFiles(t *testing.T) {
	wideKey := func(t *testing.T) string {
		key, err := initTestKey()
		if err != nil {
			t.Fatal(err)
		}
		data := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
		return initSecretFile(t, "wide.pem", data, 0o644)
	}
	pastedKey := "-----BEGIN RSA PRIVATE KEY-----\nMIIEowIBAAKCAQEA\n-----END RSA PRIVATE KEY-----"
	token := fixtureToken(t, "ghp_")
	tests := []struct {
		name    string
		flags   func(t *testing.T) []string
		wantErr string
		secret  string
	}{
		{"App key wider mode", func(t *testing.T) []string {
			return []string{"-app-client-id", "x", "-app-private-key-file", wideKey(t)}
		}, "-app-private-key-file: file mode", ""},
		{"App key not PEM", func(t *testing.T) []string {
			return []string{"-app-client-id", "x", "-app-private-key-file", initSecretFile(t, "k", []byte("not pem\n"), 0o600)}
		}, "no PEM block found", ""},
		{"App key relative", func(t *testing.T) []string {
			return []string{"-app-client-id", "x", "-app-private-key-file", "app.pem"}
		}, "must be an absolute path", ""},
		{"App key missing", func(t *testing.T) []string {
			return []string{"-app-client-id", "x", "-app-private-key-file", filepath.Join(t.TempDir(), "nope.pem")}
		}, "-app-private-key-file: ", ""},
		{"key content in the flag", func(t *testing.T) []string {
			return []string{"-app-client-id", "x", "-app-private-key-file", pastedKey}
		}, "looks like key content", "MIIEowIBAAKCAQEA"},
		{"token file wider mode", func(t *testing.T) []string {
			return []string{"-token-file", initSecretFile(t, "t", []byte(token+"\n"), 0o644)}
		}, "-token-file: file mode", token},
		{"token file relative", func(t *testing.T) []string {
			return []string{"-token-file", "token"}
		}, "must be an absolute path", ""},
		{"token file two lines", func(t *testing.T) []string {
			return []string{"-token-file", initSecretFile(t, "t", []byte(token+"\nsecond\n"), 0o600)}
		}, "one line", token},
		{"token file empty", func(t *testing.T) []string {
			return []string{"-token-file", initSecretFile(t, "t", []byte("\n"), 0o600)}
		}, "empty", ""},
		{"token in the flag", func(t *testing.T) []string {
			return []string{"-token-file", token}
		}, "value looks like a token, not a file path; set the path of the token file", token},
		{"App key flag is a token", func(t *testing.T) []string {
			return []string{"-app-client-id", "x", "-app-private-key-file", token}
		}, "-app-private-key-file: ", token},
		{"token as client ID", func(t *testing.T) []string {
			return []string{"-app-client-id", token, "-app-private-key-file", initKeyFile(t)}
		}, "looks like a token", token},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newInitHarness(t)
			path := initConfigPath(t)
			args := append([]string{"-config", path, "-repo", "https://github.com/acme/widget",
				"-runner-image", initTestImageID, "-buildkit-image", docker.BuildKitImage}, tt.flags(t)...)
			code, stdout, stderr := h.run(args...)
			assertInitFailed(t, code, path)
			if !strings.Contains(stderr, tt.wantErr) {
				t.Errorf("stderr = %q, want it to contain %q", stderr, tt.wantErr)
			}
			if tt.secret != "" {
				assertNoSecret(t, tt.secret, stdout, stderr)
			}
			if strings.Contains(stderr, t.TempDir()) {
				t.Errorf("stderr names a temporary path:\n%s", stderr)
			}
		})
	}
}

// Other flag values with a rule are refused without being quoted.
func TestInitBadImageFlags(t *testing.T) {
	for _, tt := range []struct{ flag, value, want string }{
		{"-runner-image", "ubuntu:latest", "-runner-image: must be a local image ID"},
		{"-buildkit-image", "moby/buildkit:v0.35.1", "-buildkit-image: must be moby/buildkit"},
	} {
		h := newInitHarness(t)
		path := initConfigPath(t)
		args := append(initFlagsApp(t, path), tt.flag, tt.value)
		code, _, stderr := h.run(args...)
		assertInitFailed(t, code, path)
		if !strings.Contains(stderr, tt.want) || strings.Contains(stderr, tt.value) {
			t.Errorf("%s: stderr = %q", tt.flag, stderr)
		}
	}
}

// 0003 SPEC R286: the runner image comes from its tag only when the tag has
// this project's labels, and BuildKit from the constant.
func TestInitImageFill(t *testing.T) {
	tests := []struct {
		name         string
		buildkitHere bool
		wantLine     bool // the image/build.sh line for BuildKit
	}{
		{"BuildKit image local", true, false},
		{"BuildKit image missing", false, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newInitHarness(t)
			if !tt.buildkitHere {
				delete(h.images.images, docker.BuildKitImage)
			}
			path := initConfigPath(t)
			args := initFlagsApp(t, path)
			args = args[:len(args)-4]
			code, stdout, stderr := h.run(args...)
			if code != 0 {
				t.Fatalf("exit = %d, stderr = %q", code, stderr)
			}
			cfg, err := config.Load(path)
			if err != nil {
				t.Fatal(err)
			}
			if cfg.Runner.Image != initTestImageID || cfg.BuildKit.Image != docker.BuildKitImage {
				t.Errorf("images = %q, %q", cfg.Runner.Image, cfg.BuildKit.Image)
			}
			for _, want := range []string{"runner.image: " + initTestImageID, "buildkit.image: " + docker.BuildKitImage} {
				if !strings.Contains(stdout, want) {
					t.Errorf("stdout lacks %q:\n%s", want, stdout)
				}
			}
			if got := strings.Contains(stdout, "image/build.sh를 실행하면 받아 옴"); got != tt.wantLine {
				t.Errorf("build.sh line present = %v, want %v:\n%s", got, tt.wantLine, stdout)
			}
			if want := []string{docker.RunnerImageTag, docker.BuildKitImage}; !slices.Equal(h.images.calls, want) {
				t.Errorf("Docker was asked about %v, want %v", h.images.calls, want)
			}
			if !h.images.closed {
				t.Error("the Docker client was not closed")
			}
		})
	}
	t.Run("one image given", func(t *testing.T) {
		h := newInitHarness(t)
		path := initConfigPath(t)
		args := initFlagsApp(t, path)
		args = args[:len(args)-2] // -buildkit-image stays out
		code, _, stderr := h.run(args...)
		if code != 0 {
			t.Fatalf("exit = %d, stderr = %q", code, stderr)
		}
		if want := []string{docker.BuildKitImage}; !slices.Equal(h.images.calls, want) {
			t.Errorf("Docker was asked about %v, want %v", h.images.calls, want)
		}
	})
}

// A runner tag without both labels is not filled, and the reason is on
// stdout (0003 SPEC R286).
func TestInitImageUnlabeled(t *testing.T) {
	other := docker.RunnerImageLabels()
	other["org.opencontainers.image.source"] = "https://github.com/actions/runner"
	one := docker.RunnerImageLabels()
	delete(one, "org.opencontainers.image.title")
	for name, img := range map[string]docker.Image{
		"no labels":    {ID: initTestImageID, Architecture: "arm64"},
		"base source":  {ID: initTestImageID, Architecture: "arm64", Labels: other},
		"one label":    {ID: initTestImageID, Architecture: "arm64", Labels: one},
		"odd image ID": {ID: "not-an-id", Architecture: "arm64", Labels: docker.RunnerImageLabels()},
	} {
		t.Run(name, func(t *testing.T) {
			h := newInitHarness(t)
			h.images.images[docker.RunnerImageTag] = img
			path := initConfigPath(t)
			args := initFlagsApp(t, path)
			args = args[:len(args)-4]
			code, stdout, stderr := h.run(args...)
			assertInitFailed(t, code, path)
			if !strings.Contains(stdout, "runner.image를 채우지 못함") {
				t.Errorf("stdout lacks the reason:\n%s", stdout)
			}
			if !strings.Contains(stderr, "missing runner.image") || !strings.Contains(stderr, "-runner-image") {
				t.Errorf("stderr = %q", stderr)
			}
		})
	}
	t.Run("tag missing", func(t *testing.T) {
		h := newInitHarness(t)
		delete(h.images.images, docker.RunnerImageTag)
		path := initConfigPath(t)
		args := initFlagsApp(t, path)
		args = args[:len(args)-4]
		code, stdout, stderr := h.run(args...)
		assertInitFailed(t, code, path)
		if !strings.Contains(stdout, "image/build.sh를 먼저 실행") || !strings.Contains(stderr, "-runner-image") {
			t.Errorf("stdout = %q, stderr = %q", stdout, stderr)
		}
	})
}

// With Docker out of reach, runner.image is a missing value (0003 SPEC R286).
func TestInitDockerDown(t *testing.T) {
	down := errors.New("Cannot connect to the Docker daemon")
	tests := []struct {
		name  string
		setup func(h *initHarness)
	}{
		{"connection fails", func(h *initHarness) { h.imagesErr = down }},
		{"lookup fails", func(h *initHarness) { h.images.err = down }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newInitHarness(t)
			tt.setup(h)
			path := initConfigPath(t)
			args := initFlagsApp(t, path)
			args = args[:len(args)-4]
			code, stdout, stderr := h.run(args...)
			assertInitFailed(t, code, path)
			if !strings.Contains(stdout, "Docker에 닿지 못해") || !strings.Contains(stdout, down.Error()) {
				t.Errorf("stdout = %q", stdout)
			}
			if strings.Count(stdout, "Docker에 닿지 못해") != 1 {
				t.Errorf("the Docker failure is said more than once:\n%s", stdout)
			}
			if !strings.Contains(stderr, "-runner-image") {
				t.Errorf("stderr = %q", stderr)
			}
			if len(h.images.calls) > 1 {
				t.Errorf("Docker was asked %d times after it failed", len(h.images.calls))
			}
		})
	}
	t.Run("-runner-image given", func(t *testing.T) {
		h := newInitHarness(t)
		h.images.err = down
		path := initConfigPath(t)
		args := initFlagsApp(t, path)
		args = args[:len(args)-2] // -buildkit-image stays out
		code, _, stderr := h.run(args...)
		if code != 0 {
			t.Fatalf("exit = %d, stderr = %q", code, stderr)
		}
	})
}

// 0003 SPEC R287: without a terminal, each missing value is named by its
// flag and nothing is written or asked.
func TestInitNotTerminal(t *testing.T) {
	h := newInitHarness(t)
	h.images.images = nil // no runner image either
	path := initConfigPath(t)
	code, stdout, stderr := h.run("-config", path)
	assertInitFailed(t, code, path)
	lines := strings.Split(strings.TrimSpace(stderr), "\n")
	want := []string{"-app-client-id and -app-private-key-file, or -token-file", "-repo", "-runner-image"}
	if len(lines) != len(want) {
		t.Fatalf("stderr has %d lines, want %d:\n%s", len(lines), len(want), stderr)
	}
	for i, flag := range want {
		if !strings.HasPrefix(lines[i], "missing ") || !strings.Contains(lines[i], "set "+flag) {
			t.Errorf("line %d = %q, want it to name %q", i, lines[i], flag)
		}
	}
	if strings.Contains(stdout, "[app/token]") {
		t.Errorf("a prompt was written without a terminal:\n%s", stdout)
	}
	t.Run("one value missing", func(t *testing.T) {
		h := newInitHarness(t)
		path := initConfigPath(t)
		args := initFlagsApp(t, path)
		args = slices.Delete(args, slices.Index(args, "-app-private-key-file"), slices.Index(args, "-app-private-key-file")+2)
		code, _, stderr := h.run(args...)
		assertInitFailed(t, code, path)
		if strings.TrimSpace(stderr) != "missing GitHub App private key file: set -app-private-key-file" {
			t.Errorf("stderr = %q", stderr)
		}
	})
}

// 0003 SPEC R285: on a terminal the missing values are asked, in order, and
// only those.
func TestInitPrompts(t *testing.T) {
	t.Run("App branch", func(t *testing.T) {
		h := newInitHarness(t)
		h.terminal = true
		keyPath := initKeyFile(t)
		h.stdin = strings.Join([]string{
			"app", "Iv1.0123456789abcdef", keyPath,
			"https://github.com/acme/widget", "2", "https://github.com/acme/gadget", "", "",
		}, "\n") + "\n"
		path := initConfigPath(t)
		code, stdout, stderr := h.run("-config", path)
		if code != 0 {
			t.Fatalf("exit = %d, stderr = %q, stdout:\n%s", code, stderr, stdout)
		}
		cfg, err := config.Load(path)
		if err != nil {
			t.Fatal(err)
		}
		if cfg.GitHub.AppClientID != "Iv1.0123456789abcdef" || cfg.GitHub.AppPrivateKeyFile != keyPath {
			t.Errorf("github = %+v", cfg.GitHub)
		}
		want := []config.Target{{URL: "https://github.com/acme/widget", MaxRunners: 2}, {URL: "https://github.com/acme/gadget", MaxRunners: 1}}
		if !reflect.DeepEqual(cfg.Targets, want) {
			t.Errorf("targets = %+v, want %+v", cfg.Targets, want)
		}
		// The runner image came from Docker, so it is not asked.
		if cfg.Runner.Image != initTestImageID || strings.Contains(stdout, "runner.image (") {
			t.Errorf("runner.image = %q; stdout:\n%s", cfg.Runner.Image, stdout)
		}
		last := -1
		for _, q := range []string{"runner.image: " + initTestImageID, "자격 [app/token] (기본 app):", "GitHub App client ID:", "App 개인키 파일 경로:", "저장소 URL (빈 줄이면 끝):", "max-runners (기본 1):"} {
			i := strings.Index(stdout, q)
			if i < last {
				t.Errorf("%q is out of order:\n%s", q, stdout)
			}
			last = i
		}
	})
	t.Run("token branch and the image asked", func(t *testing.T) {
		h := newInitHarness(t)
		h.terminal = true
		h.images.images[docker.RunnerImageTag] = docker.Image{ID: initTestImageID} // no labels
		tokenPath := writeTokenFile(t, fixtureToken(t, "ghp_"))
		h.stdin = strings.Join([]string{"token", tokenPath, "https://github.com/acme/widget", "", "", initTestImageID}, "\n") + "\n"
		path := initConfigPath(t)
		code, stdout, stderr := h.run("-config", path)
		if code != 0 {
			t.Fatalf("exit = %d, stderr = %q, stdout:\n%s", code, stderr, stdout)
		}
		cfg, err := config.Load(path)
		if err != nil {
			t.Fatal(err)
		}
		if cfg.GitHub.TokenFile != tokenPath || cfg.Runner.Image != initTestImageID {
			t.Errorf("config = %+v", cfg)
		}
		if !strings.Contains(stdout, "runner.image (image/build.sh가 출력한 값):") {
			t.Errorf("the image was not asked:\n%s", stdout)
		}
		assertNoSecret(t, fixtureToken(t, "ghp_"), stdout, stderr)
	})
	t.Run("flags answer", func(t *testing.T) {
		// Everything is given except the repository.
		h := newInitHarness(t)
		h.terminal = true
		h.stdin = "https://github.com/acme/widget\n\n\n"
		path := initConfigPath(t)
		args := initFlagsApp(t, path)
		i := slices.Index(args, "-repo")
		args = slices.Delete(args, i, i+2)
		code, stdout, stderr := h.run(args...)
		if code != 0 {
			t.Fatalf("exit = %d, stderr = %q", code, stderr)
		}
		for _, notAsked := range []string{"자격", "client ID", "개인키", "토큰 파일", "runner.image ("} {
			if strings.Contains(stdout, notAsked) {
				t.Errorf("stdout asks %q, which a flag answered:\n%s", notAsked, stdout)
			}
		}
		if !strings.Contains(stdout, "저장소 URL") {
			t.Errorf("the repository was not asked:\n%s", stdout)
		}
	})
}

// A wrong answer shows its error and the same question again, end to end
// (0003 SPEC R285).
func TestInitPromptRetry(t *testing.T) {
	h := newInitHarness(t)
	h.terminal = true
	h.images.images = nil
	wide := initSecretFile(t, "t", []byte(fixtureToken(t, "ghp_")+"\n"), 0o644)
	good := writeTokenFile(t, fixtureToken(t, "ghp_"))
	h.stdin = strings.Join([]string{
		"token", "token", wide, good, // relative, then mode 0644, then fine
		"https://github.com/acme", "https://github.com/acme/widget", "", "",
		"ubuntu:latest", initTestImageID,
	}, "\n") + "\n"
	path := initConfigPath(t)
	code, stdout, stderr := h.run("-config", path)
	if code != 0 {
		t.Fatalf("exit = %d, stderr = %q, stdout:\n%s", code, stderr, stdout)
	}
	for _, msg := range []string{"must be an absolute path", "wider than 0600", "names an organization or account", "sha256:<64 hex digits>"} {
		if !strings.Contains(stdout, msg) {
			t.Errorf("stdout lacks %q:\n%s", msg, stdout)
		}
	}
	if n := strings.Count(stdout, "토큰 파일 경로:"); n != 3 {
		t.Errorf("the token path was asked %d times, want 3", n)
	}
}

// 0003 SPEC R285: input that ends at any question writes nothing.
func TestInitEOF(t *testing.T) {
	keyPath := initKeyFile(t)
	answers := []string{"app", "Iv1.0123456789abcdef", keyPath, "https://github.com/acme/widget", "", "", initTestImageID}
	for n := 0; n < len(answers); n++ {
		h := newInitHarness(t)
		h.terminal = true
		h.images.images = nil
		h.stdin = strings.Join(answers[:n], "\n")
		if n > 0 {
			h.stdin += "\n"
		}
		path := initConfigPath(t)
		code, _, stderr := h.run("-config", path)
		assertInitFailed(t, code, path)
		if !strings.Contains(stderr, "input ended") {
			t.Errorf("after %d answers: stderr = %q", n, stderr)
		}
	}
}

// 0003 SPEC R289: init reads images and writes the file and its directory
// only.
func TestInitTouchesNothingElse(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := os.Mkdir(filepath.Join(home, ".config"), 0o755); err != nil {
		t.Fatal(err)
	}
	h := newInitHarness(t)
	args := initFlagsApp(t, defaultConfigPath())
	args = args[2:]           // the default -config is used
	args = args[:len(args)-4] // both images come from Docker
	code, _, stderr := h.run(args...)
	if code != 0 {
		t.Fatalf("exit = %d, stderr = %q", code, stderr)
	}
	var got []string
	err := filepath.WalkDir(home, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(home, path)
		got = append(got, rel)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{".", ".config", filepath.Join(".config", "actions-runner-scaler"), filepath.Join(".config", "actions-runner-scaler", "config.toml")}
	if !slices.Equal(got, want) {
		t.Errorf("HOME holds %v, want %v", got, want)
	}
	if want := []string{docker.RunnerImageTag, docker.BuildKitImage}; !slices.Equal(h.images.calls, want) {
		t.Errorf("Docker was asked %v, want only the two image lookups %v", h.images.calls, want)
	}
}

// The command's dependencies are Docker image lookups and standard input:
// no GitHub client, no other Docker call (0003 SPEC R289).
func TestInitDepsHoldNoGitHub(t *testing.T) {
	typ := reflect.TypeOf(initDeps{})
	var names []string
	for i := range typ.NumField() {
		f := typ.Field(i)
		names = append(names, f.Name)
		if strings.Contains(f.Type.String(), "github") {
			t.Errorf("field %s has type %v", f.Name, f.Type)
		}
	}
	if want := []string{"Images", "Stdin", "Terminal"}; !slices.Equal(names, want) {
		t.Errorf("initDeps fields = %v, want %v", names, want)
	}
	if n := reflect.TypeFor[initImageInspector]().NumMethod(); n != 1 {
		t.Errorf("initImageInspector has %d methods, want only ImageInspect", n)
	}
}

// 0003 SPEC R277: the value in a token file is in neither stream nor the
// written file, however the run ends.
func TestInitNoTokenValue(t *testing.T) {
	token := fixtureToken(t, "github_pat_")
	tokenPath := writeTokenFile(t, token)
	t.Run("success", func(t *testing.T) {
		h := newInitHarness(t)
		path := initConfigPath(t)
		code, stdout, stderr := h.run("-config", path, "-token-file", tokenPath, "-repo", "https://github.com/acme/widget")
		if code != 0 {
			t.Fatalf("exit = %d, stderr = %q", code, stderr)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		assertNoSecret(t, token, stdout, stderr, string(data))
		if !strings.Contains(string(data), tokenPath) {
			t.Error("the file does not hold the token file's path")
		}
	})
	t.Run("failure", func(t *testing.T) {
		h := newInitHarness(t)
		path := initConfigPath(t)
		code, stdout, stderr := h.run("-config", path, "-token-file", tokenPath, "-repo", "https://github.com/acme", "-app-client-id", "x")
		assertInitFailed(t, code, path)
		assertNoSecret(t, token, stdout, stderr)
	})
	t.Run("on a terminal", func(t *testing.T) {
		h := newInitHarness(t)
		h.terminal = true
		h.stdin = "token\n" + token + "\n" + tokenPath + "\nhttps://github.com/acme/widget\n\n\n"
		path := initConfigPath(t)
		code, stdout, stderr := h.run("-config", path)
		if code != 0 {
			t.Fatalf("exit = %d, stderr = %q", code, stderr)
		}
		assertNoSecret(t, token, stdout, stderr)
	})
}

func TestInitUsage(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want int
	}{
		{"positional argument", []string{"extra"}, 1},
		{"empty -config", []string{"-config", ""}, 1},
		{"unknown flag", []string{"-bogus"}, 1},
		{"help", []string{"-h"}, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newInitHarness(t)
			code, stdout, stderr := h.run(tt.args...)
			if code != tt.want {
				t.Errorf("exit = %d, want %d (stderr %q)", code, tt.want, stderr)
			}
			if !strings.Contains(stderr, "usage: init") {
				t.Errorf("stderr lacks the usage:\n%s", stderr)
			}
			if stdout != "" || h.imagesOpened != 0 {
				t.Errorf("stdout = %q, Docker opened %d times", stdout, h.imagesOpened)
			}
			if strings.Contains(stderr, `"msg":"command failed"`) {
				t.Errorf("a command-line mistake is logged as a failure:\n%s", stderr)
			}
		})
	}
}

func TestIsTerminal(t *testing.T) {
	f, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if isTerminal(int(f.Fd())) {
		t.Error("/dev/null is a terminal")
	}
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close()
	if isTerminal(int(r.Fd())) {
		t.Error("a pipe is a terminal")
	}
	if isTerminal(-1) {
		t.Error("fd -1 is a terminal")
	}
	// The controlling terminal exists only when the tests run from one.
	tty, err := os.OpenFile("/dev/tty", os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		t.Skipf("no controlling terminal: %v", err)
	}
	defer tty.Close()
	if !isTerminal(int(tty.Fd())) {
		t.Error("/dev/tty is not a terminal")
	}
}
