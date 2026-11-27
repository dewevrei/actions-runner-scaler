package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dewevrei/actions-runner-scaler/internal/config"
	"github.com/dewevrei/actions-runner-scaler/internal/configfile"
)

const repoEditHex = "0000000000000000000000000000000000000000000000000000000000000000"

type repoEditOutput struct {
	stdout, stderr string
	code           int
}

// repoEditExec runs the real command table.
func repoEditExec(t *testing.T, args ...string) repoEditOutput {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := dispatch(context.Background(), newEnv(&stdout, &stderr), args, commands(refusingHosts()))
	return repoEditOutput{stdout.String(), stderr.String(), code}
}

// repoEditExecWith runs add-repo and remove-repo on deps.
func repoEditExecWith(t *testing.T, deps repoEditDeps, args ...string) repoEditOutput {
	t.Helper()
	cmds := map[string]command{
		"add-repo":    {run: func(_ context.Context, e env, a []string) error { return addRepo(e, deps, a) }},
		"remove-repo": {run: func(_ context.Context, e env, a []string) error { return removeRepo(e, deps, a) }},
	}
	var stdout, stderr bytes.Buffer
	code := dispatch(context.Background(), newEnv(&stdout, &stderr), args, cmds)
	return repoEditOutput{stdout.String(), stderr.String(), code}
}

// repoEditWrite writes content to a config file of the given mode in a new
// directory and returns its path.
func repoEditWrite(t *testing.T, content string, mode os.FileMode) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
	return path
}

// repoEditTestdata returns a file of internal/configfile/testdata.
func repoEditTestdata(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "internal", "configfile", "testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// repoEditValidConfig writes a config file that config.Load accepts, with a
// target for each url.
func repoEditValidConfig(t *testing.T, urls ...string) string {
	t.Helper()
	token := writeTokenFile(t, fixtureToken(t, "ghp_"))
	var b strings.Builder
	fmt.Fprintf(&b, "[github]\ntoken-file = %q\n\n[runner]\nimage = \"sha256:%s\"\n\n[buildkit]\nimage = \"moby/buildkit:v0.1.0-rootless@sha256:%s\"\n",
		token, repoEditHex, repoEditHex)
	for _, u := range urls {
		fmt.Fprintf(&b, "\n[[targets]]\nurl = %q\nmax-runners = 1\n", u)
	}
	return repoEditWrite(t, b.String(), 0o600)
}

func repoEditRead(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// repoEditCut returns s without the part from the start of from through the
// end of through, which follows it.
func repoEditCut(t *testing.T, s, from, through string) string {
	t.Helper()
	start := strings.Index(s, from)
	last := strings.Index(s, through)
	if start < 0 || last < start {
		t.Fatalf("cannot cut %q through %q", from, through)
	}
	return s[:start] + s[last+len(through):]
}

func TestRepoEditRefusals(t *testing.T) {
	const addURL = "https://github.com/acme/new"
	dup := repoEditTestdata(t, "commented.toml") + "\n[[targets]]\nurl = \"https://github.com/Acme/API\"\nmax-runners = 1\n"
	single := "[[targets]]\nurl = \"https://github.com/acme/api\"\nmax-runners = 1\n"
	inline := repoEditTestdata(t, "inline-targets.toml")
	dotted := repoEditTestdata(t, "dotted-targets.toml")
	multiline := repoEditTestdata(t, "multiline-string.toml")
	commented := repoEditTestdata(t, "commented.toml")

	tests := []struct {
		name string
		file string
		// args returns the command and its arguments, without -config.
		args   func(t *testing.T) []string
		stderr string
		paste  bool
	}{
		{"add: not TOML", "= broken\n", urlArgs("add-repo", addURL), "not TOML", false},
		{"add: R231 URL", commented, urlArgs("add-repo", "https://github.com/acme/new/"), "must be https://github.com/<owner>/<repo>", false},
		{"add: R231 URL with .git", commented, urlArgs("add-repo", "https://github.com/acme/new.git"), "must be https://github.com/<owner>/<repo>", false},
		{"add: R232 organization URL", commented, urlArgs("add-repo", "https://github.com/acme"), "names an organization or account", false},
		{"add: already there, case differs", commented, urlArgs("add-repo", "https://github.com/ACME/Api"), "already in the config file", false},
		{"add: max-runners 0", commented, urlArgs("add-repo", addURL, "-max-runners", "0"), "-max-runners must be an integer of at least 1", false},
		{"add: max-runners text", commented, urlArgs("add-repo", addURL, "-max-runners", "many"), "-max-runners must be", false},
		{"add: idle above the default max-runners", commented, urlArgs("add-repo", addURL, "-idle", "2"), "-idle must be an integer from 0 to max-runners (1)", false},
		{"add: idle above max-runners", commented, urlArgs("add-repo", addURL, "-max-runners", "2", "-idle", "3"), "-idle must be an integer from 0 to max-runners (2)", false},
		{"add: idle negative", commented, urlArgs("add-repo", addURL, "-idle", "-1"), "-idle must be", false},
		{"add: token file relative", commented, urlArgs("add-repo", addURL, "-token-file", "token"), "-token-file: must be an absolute path", false},
		{"add: token file empty", commented, urlArgs("add-repo", addURL, "-token-file", ""), "-token-file", false},
		{"add: token file wider than 0600", commented, func(t *testing.T) []string {
			path := writeTokenFile(t, fixtureToken(t, "ghp_"))
			if err := os.Chmod(path, 0o644); err != nil {
				t.Fatal(err)
			}
			return []string{"add-repo", "-token-file", path, addURL}
		}, "-token-file: file mode", false},
		{"add: token file is a token", commented, func(t *testing.T) []string {
			return []string{"add-repo", "-token-file", fixtureToken(t, "github_pat_"), addURL}
		}, "value looks like a token, not a file path", false},
		{"add: file lists a repository twice", dup, urlArgs("add-repo", addURL), "more than once", false},
		{"add: inline targets", inline, urlArgs("add-repo", addURL, "-max-runners", "1"), "[[targets]]", true},
		{"add: dotted targets", dotted, urlArgs("add-repo", addURL), "header lines", true},
		{"add: dotted targets, already there", dotted, urlArgs("add-repo", "https://github.com/Acme/API"), "already in the config file", false},
		{"add: inline targets, bad URL", inline, urlArgs("add-repo", "https://github.com/acme"), "names an organization", false},
		{"add: inline targets, bad max-runners", inline, urlArgs("add-repo", addURL, "-max-runners", "0"), "-max-runners", false},
		{"add: inline targets, bad idle", inline, urlArgs("add-repo", addURL, "-idle", "5"), "-idle", false},
		{"add: inline targets, token file is a token", inline, func(t *testing.T) []string {
			return []string{"add-repo", "-token-file", fixtureToken(t, "ghp_"), addURL}
		}, "value looks like a token", false},
		{"add: inline targets, already there", inline, urlArgs("add-repo", "https://github.com/acme/api"), "already in the config file", false},
		{"remove: not TOML", "= broken\n", urlArgs("remove-repo", "https://github.com/acme/api"), "not TOML", false},
		{"remove: R231 URL", commented, urlArgs("remove-repo", "http://github.com/acme/api"), "must be https://github.com/<owner>/<repo>", false},
		{"remove: R232 organization URL", commented, urlArgs("remove-repo", "https://github.com/acme"), "names an organization or account", false},
		{"remove: missing", commented, urlArgs("remove-repo", "https://github.com/acme/none"), "is not in the config file", false},
		{"remove: file lists a repository twice", dup, urlArgs("remove-repo", "https://github.com/acme/web"), "more than once", false},
		{"remove: the last target", single, urlArgs("remove-repo", "https://github.com/acme/api"), "without a repository", false},
		{"remove: inline targets", inline, urlArgs("remove-repo", "https://github.com/acme/api"), "header lines", false},
		{"remove: dotted targets", dotted, urlArgs("remove-repo", "https://github.com/acme/api"), "header lines", false},
		{"remove: multi-line string", multiline, urlArgs("remove-repo", "https://github.com/acme/api"), "would not read back", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := repoEditWrite(t, tt.file, 0o640)
			args := tt.args(t)
			out := repoEditExec(t, append([]string{args[0], "-config", cfg}, args[1:]...)...)

			if out.code != 1 {
				t.Errorf("exit = %d, want 1", out.code)
			}
			if out.stdout != "" {
				t.Errorf("stdout = %q, want empty", out.stdout)
			}
			if !strings.Contains(out.stderr, tt.stderr) {
				t.Errorf("stderr = %q, want it to contain %q", out.stderr, tt.stderr)
			}
			if strings.Contains(out.stderr, `"level"`) {
				t.Errorf("stderr = %q, want plain text, not a log line", out.stderr)
			}
			if got := repoEditRead(t, cfg); got != tt.file {
				t.Errorf("the file changed:\n%s", got)
			}
			if strings.Contains(out.stderr, "[[targets]]\nurl") != tt.paste {
				t.Errorf("paste block shown = %v, want %v; stderr:\n%s", !tt.paste, tt.paste, out.stderr)
			}
			if tt.paste {
				want := "\n" + configfile.TargetBlock(configfile.TargetEntry{URL: addURL, MaxRunners: 1})
				if !strings.Contains(out.stderr, want) {
					t.Errorf("stderr lacks the block of the arguments:\n%s", out.stderr)
				}
			}
		})
	}
}

// urlArgs returns a builder of the arguments command, flags and url.
func urlArgs(command, url string, flags ...string) func(*testing.T) []string {
	return func(*testing.T) []string {
		return append(append([]string{command}, flags...), url)
	}
}

func TestRepoEditAddKeepsBytes(t *testing.T) {
	orig := repoEditTestdata(t, "commented.toml")
	cfg := repoEditWrite(t, orig, 0o640)
	tokenFile := writeTokenFile(t, fixtureToken(t, "ghp_"))

	out := repoEditExec(t, "add-repo", "-config", cfg, "-max-runners", "3", "-idle", "1", "-token-file", tokenFile, "https://github.com/acme/new")
	if out.code != 0 || out.stderr != "" {
		t.Fatalf("exit = %d, stderr = %q", out.code, out.stderr)
	}
	idle := 1
	block := configfile.TargetBlock(configfile.TargetEntry{URL: "https://github.com/acme/new", MaxRunners: 3, Idle: &idle, TokenFile: tokenFile})
	if got, want := repoEditRead(t, cfg), orig+"\n"+block; got != want {
		t.Errorf("file =\n%s\nwant the original, a blank line and the block", got)
	}
	info, err := os.Stat(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o640 {
		t.Errorf("mode = %v, want 0640", info.Mode().Perm())
	}
	// Only the file and its lock remain; no temporary file.
	entries, err := os.ReadDir(filepath.Dir(cfg))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	if want := []string{"config.toml", "config.toml.lock"}; !reflect.DeepEqual(names, want) {
		t.Errorf("directory holds %v, want %v", names, want)
	}
}

func TestRepoEditRemoveBlockOnly(t *testing.T) {
	orig := repoEditTestdata(t, "commented.toml")
	cfg := repoEditWrite(t, orig, 0o600)

	out := repoEditExec(t, "remove-repo", "-config", cfg, "https://github.com/acme/web")
	if out.code != 0 || out.stderr != "" {
		t.Fatalf("exit = %d, stderr = %q", out.code, out.stderr)
	}
	want := repoEditCut(t, orig, "  [[targets]]  # second", "token-file = \"/keys/web-token\"\n")
	if got := repoEditRead(t, cfg); got != want {
		t.Errorf("file =\n%s\nwant\n%s", got, want)
	}
}

func TestRepoEditSymlink(t *testing.T) {
	dir := t.TempDir()
	realDir := filepath.Join(dir, "real")
	if err := os.Mkdir(realDir, 0o700); err != nil {
		t.Fatal(err)
	}
	orig := repoEditTestdata(t, "commented.toml")
	realPath := filepath.Join(realDir, "config.toml")
	if err := os.WriteFile(realPath, []byte(orig), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link.toml")
	if err := os.Symlink(realPath, link); err != nil {
		t.Fatal(err)
	}

	out := repoEditExec(t, "add-repo", "-config", link, "https://github.com/acme/new")
	if out.code != 0 {
		t.Fatalf("exit = %d, stderr = %q", out.code, out.stderr)
	}
	info, err := os.Lstat(link)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		t.Error("the link is no longer a symbolic link")
	}
	if got := repoEditRead(t, realPath); !strings.Contains(got, "https://github.com/acme/new") || !strings.HasPrefix(got, orig) {
		t.Errorf("the real file did not change as asked:\n%s", got)
	}
	if _, err := os.Stat(realPath + ".lock"); err != nil {
		t.Errorf("no lock file next to the real file: %v", err)
	}
	if _, err := os.Lstat(link + ".lock"); err == nil {
		t.Error("a lock file was made next to the link")
	}
}

func TestRepoEditOutput(t *testing.T) {
	t.Run("add", func(t *testing.T) {
		cfg := repoEditValidConfig(t, "https://github.com/acme/api")
		out := repoEditExec(t, "add-repo", "-config", cfg, "https://github.com/acme/new")
		want := "https://github.com/acme/new\n스케일러를 다시 시작해야 반영됨\n"
		if out.code != 0 || out.stdout != want || out.stderr != "" {
			t.Errorf("exit = %d, stdout = %q, stderr = %q; want 0, %q, empty", out.code, out.stdout, out.stderr, want)
		}
	})
	t.Run("remove", func(t *testing.T) {
		cfg := repoEditValidConfig(t, "https://github.com/acme/api", "https://github.com/acme/web")
		out := repoEditExec(t, "remove-repo", "-config", cfg, "https://github.com/acme/web")
		lines := strings.Split(strings.TrimSuffix(out.stdout, "\n"), "\n")
		if out.code != 0 || out.stderr != "" || len(lines) != 3 {
			t.Fatalf("exit = %d, stdout = %q, stderr = %q", out.code, out.stdout, out.stderr)
		}
		if lines[0] != "https://github.com/acme/web" || lines[1] != "스케일러를 다시 시작해야 반영됨" {
			t.Errorf("first lines = %q", lines[:2])
		}
		for _, want := range []string{"scale set", "GitHub에 남음", "delete-scaleset"} {
			if !strings.Contains(lines[2], want) {
				t.Errorf("scale set line %q lacks %q", lines[2], want)
			}
		}
	})
}

func TestRepoEditOtherProblems(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	cfg := repoEditValidConfig(t, "https://github.com/acme/api")
	data := strings.Replace(repoEditRead(t, cfg), "image = \"sha256:"+repoEditHex+"\"", `image = "Not-An-Image"`, 1)
	if err := os.WriteFile(cfg, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}

	out := repoEditExec(t, "add-repo", "-config", cfg, "https://github.com/acme/new")
	if out.code != 0 || out.stderr != "" {
		t.Fatalf("exit = %d, stderr = %q", out.code, out.stderr)
	}
	if got := repoEditRead(t, cfg); !strings.HasPrefix(got, data) || !strings.Contains(got, "https://github.com/acme/new") {
		t.Errorf("the file was not changed:\n%s", got)
	}
	_, after, found := strings.Cut(out.stdout, "config still has problems:\n")
	if !found {
		t.Fatalf("stdout = %q, want the problems after \"config still has problems:\"", out.stdout)
	}
	if !strings.Contains(after, "runner.image") || strings.Contains(after, "targets") {
		t.Errorf("problems = %q, want only the image error", after)
	}

	// Nothing is made outside the file and its lock: no scaler lock or
	// socket under HOME.
	if entries, err := os.ReadDir(home); err != nil || len(entries) != 0 {
		t.Errorf("HOME holds %v (%v), want it empty", entries, err)
	}
	// The command's dependencies have no GitHub, Docker or scaler lock.
	typ := reflect.TypeFor[repoEditDeps]()
	var names []string
	for i := range typ.NumField() {
		names = append(names, typ.Field(i).Name)
	}
	if want := []string{"Lock", "ReadFile", "Replace", "LoadConfig"}; !reflect.DeepEqual(names, want) {
		t.Errorf("dependencies = %v, want %v", names, want)
	}
}

func TestRepoEditOrderAndFailure(t *testing.T) {
	type steps struct {
		calls []string
		// paths holds the path each step that takes one was given.
		paths   []string
		notify  io.Writer
		lockErr error
		replace error
	}
	deps := func(s *steps, content string) repoEditDeps {
		return repoEditDeps{
			Lock: func(path string, notify io.Writer) (func(), error) {
				s.calls = append(s.calls, "lock")
				s.paths = append(s.paths, path)
				s.notify = notify
				return func() { s.calls = append(s.calls, "unlock") }, s.lockErr
			},
			ReadFile: func(path string) ([]byte, error) {
				s.calls = append(s.calls, "read")
				s.paths = append(s.paths, path)
				return []byte(content), nil
			},
			Replace: func(path string, _ []byte) error {
				s.calls = append(s.calls, "replace")
				s.paths = append(s.paths, path)
				return s.replace
			},
			LoadConfig: func(path string) error {
				s.calls = append(s.calls, "load")
				s.paths = append(s.paths, path)
				return nil
			},
		}
	}
	const file = "[[targets]]\nurl = \"https://github.com/a/one\"\nmax-runners = 1\n"
	cfg := repoEditWrite(t, file, 0o600)

	// The path is resolved once; every step gets the file the link names.
	t.Run("a link is resolved once", func(t *testing.T) {
		realPath, err := filepath.EvalSymlinks(cfg)
		if err != nil {
			t.Fatal(err)
		}
		link := filepath.Join(t.TempDir(), "link.toml")
		if err := os.Symlink(cfg, link); err != nil {
			t.Fatal(err)
		}
		var s steps
		if out := repoEditExecWith(t, deps(&s, file), "add-repo", "-config", link, "https://github.com/a/two"); out.code != 0 {
			t.Fatalf("exit = %d, stderr = %q", out.code, out.stderr)
		}
		want := []string{realPath, realPath, realPath, realPath}
		if !reflect.DeepEqual(s.paths, want) {
			t.Errorf("paths = %v, want %v", s.paths, want)
		}
	})
	t.Run("a missing file", func(t *testing.T) {
		var s steps
		out := repoEditExecWith(t, deps(&s, file), "add-repo", "-config", filepath.Join(t.TempDir(), "none.toml"), "https://github.com/a/two")
		if out.code != 1 || !strings.Contains(out.stderr, "none.toml") || len(s.calls) != 0 {
			t.Errorf("exit = %d, stderr = %q, calls = %v", out.code, out.stderr, s.calls)
		}
	})
	t.Run("success", func(t *testing.T) {
		var s steps
		var stdout, stderr bytes.Buffer
		cmds := map[string]command{"add-repo": {run: func(_ context.Context, e env, a []string) error { return addRepo(e, deps(&s, file), a) }}}
		e := newEnv(&stdout, &stderr)
		if code := dispatch(context.Background(), e, []string{"add-repo", "-config", cfg, "https://github.com/a/two"}, cmds); code != 0 {
			t.Fatalf("exit = %d, stderr = %q", code, stderr.String())
		}
		if want := []string{"lock", "read", "replace", "unlock", "load"}; !reflect.DeepEqual(s.calls, want) {
			t.Errorf("calls = %v, want %v", s.calls, want)
		}
		if s.notify != io.Writer(&stderr) {
			t.Error("the lock's notice does not go to stderr")
		}
	})
	t.Run("arguments are checked before the lock", func(t *testing.T) {
		var s steps
		out := repoEditExecWith(t, deps(&s, file), "add-repo", "-config", cfg, "-max-runners", "0", "https://github.com/a/two")
		if out.code != 1 || len(s.calls) != 0 {
			t.Errorf("exit = %d, calls = %v, want 1 and none", out.code, s.calls)
		}
	})
	t.Run("lock failure", func(t *testing.T) {
		s := steps{lockErr: errors.New("lock is broken")}
		out := repoEditExecWith(t, deps(&s, file), "remove-repo", "-config", cfg, "https://github.com/a/one")
		if out.code != 1 || !strings.Contains(out.stderr, "lock is broken") {
			t.Errorf("exit = %d, stderr = %q", out.code, out.stderr)
		}
		if want := []string{"lock"}; !reflect.DeepEqual(s.calls, want) {
			t.Errorf("calls = %v, want %v", s.calls, want)
		}
	})
	t.Run("replace failure", func(t *testing.T) {
		s := steps{replace: errors.New("disk is full")}
		out := repoEditExecWith(t, deps(&s, file), "add-repo", "-config", cfg, "https://github.com/a/two")
		if out.code != 1 || !strings.Contains(out.stderr, "disk is full") || out.stdout != "" {
			t.Errorf("exit = %d, stdout = %q, stderr = %q", out.code, out.stdout, out.stderr)
		}
		if want := []string{"lock", "read", "replace", "unlock"}; !reflect.DeepEqual(s.calls, want) {
			t.Errorf("calls = %v, want %v (no read-back after a failure)", s.calls, want)
		}
	})
	t.Run("refusal replaces nothing", func(t *testing.T) {
		var s steps
		out := repoEditExecWith(t, deps(&s, file), "add-repo", "-config", cfg, "https://github.com/a/one")
		if out.code != 1 {
			t.Errorf("exit = %d", out.code)
		}
		if want := []string{"lock", "read", "unlock"}; !reflect.DeepEqual(s.calls, want) {
			t.Errorf("calls = %v, want %v", s.calls, want)
		}
	})
}

func TestRepoEditUsage(t *testing.T) {
	cfg := repoEditWrite(t, repoEditTestdata(t, "commented.toml"), 0o600)
	for _, args := range [][]string{
		{"add-repo", "-config", cfg},
		{"add-repo", "-config", cfg, "https://github.com/a/b", "https://github.com/a/c"},
		{"add-repo", "-config", cfg, "https://github.com/a/b", "-max-runners", "2"},
		{"remove-repo", "-config", cfg},
		{"remove-repo", "-config", cfg, "https://github.com/a/b", "extra"},
		{"remove-repo", "-config", cfg, "-max-runners", "2", "https://github.com/acme/api"},
	} {
		out := repoEditExec(t, args...)
		if out.code != 1 || out.stdout != "" || out.stderr == "" {
			t.Errorf("%v: exit = %d, stdout = %q, stderr = %q", args, out.code, out.stdout, out.stderr)
		}
	}
	if _, err := os.Stat(cfg + ".lock"); err == nil {
		t.Error("a usage error made a lock file")
	}
}

func TestRepoEditMissingConfig(t *testing.T) {
	cfg := filepath.Join(t.TempDir(), "none.toml")
	out := repoEditExec(t, "add-repo", "-config", cfg, "https://github.com/a/b")
	if out.code != 1 || !strings.Contains(out.stderr, "none.toml") {
		t.Errorf("exit = %d, stderr = %q", out.code, out.stderr)
	}
	if entries, _ := os.ReadDir(filepath.Dir(cfg)); len(entries) != 0 {
		t.Errorf("the directory holds %v, want it empty", entries)
	}
}

// syncBuffer is a bytes.Buffer that two goroutines may use.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// A command that finds the lock held says so on stderr, waits and then
// edits (0003 SPEC R315).
func TestRepoEditWaitsForLock(t *testing.T) {
	orig := repoEditTestdata(t, "commented.toml")
	cfg := repoEditWrite(t, orig, 0o600)
	unlock, err := configfile.Lock(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()

	var stdout, stderr syncBuffer
	done := make(chan int)
	go func() {
		done <- dispatch(context.Background(), newEnv(&stdout, &stderr),
			[]string{"add-repo", "-config", cfg, "https://github.com/acme/new"}, commands(refusingHosts()))
	}()

	deadline := time.Now().Add(10 * time.Second)
	for !strings.Contains(stderr.String(), "Blocking: waiting for file lock on ") {
		if time.Now().After(deadline) {
			t.Fatalf("no waiting notice; stderr = %q", stderr.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got := repoEditRead(t, cfg); got != orig {
		t.Error("the file changed while the lock was held")
	}
	unlock()
	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("exit = %d, stderr = %q", code, stderr.String())
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the command did not finish after the lock was released")
	}
	if got := repoEditRead(t, cfg); !strings.Contains(got, "https://github.com/acme/new") {
		t.Errorf("the file was not changed:\n%s", got)
	}
}

// Environment of the re-executed test binary (TestRepoEditHelperProcess).
const (
	repoEditHelperEnv = "REPOEDIT_TEST_ARGS"
	// repoEditHelperPause is how long a helper holds the lock after
	// reading, so that a command without a lock would lose a change.
	repoEditHelperPause = 300 * time.Millisecond
)

// TestRepoEditHelperProcess is not a test: TestRepoEditConcurrent runs the
// test binary again with the command line in repoEditHelperEnv.
func TestRepoEditHelperProcess(t *testing.T) {
	args := os.Getenv(repoEditHelperEnv)
	if args == "" {
		return
	}
	deps := newRepoEditDeps()
	read := deps.ReadFile
	deps.ReadFile = func(path string) ([]byte, error) {
		data, err := read(path)
		time.Sleep(repoEditHelperPause)
		return data, err
	}
	cmds := map[string]command{"add-repo": {run: func(_ context.Context, e env, a []string) error { return addRepo(e, deps, a) }}}
	os.Exit(dispatch(context.Background(), newEnv(os.Stdout, os.Stderr), strings.Split(args, "\n"), cmds))
}

func TestRepoEditConcurrent(t *testing.T) {
	cfg := repoEditValidConfig(t, "https://github.com/acme/api")
	urls := []string{"https://github.com/acme/c1", "https://github.com/acme/c2", "https://github.com/acme/c3"}

	var cmds []*exec.Cmd
	var outputs []*syncBuffer
	for _, url := range urls {
		cmd := exec.Command(os.Args[0], "-test.run=^TestRepoEditHelperProcess$")
		cmd.Env = append(os.Environ(), repoEditHelperEnv+"="+strings.Join([]string{"add-repo", "-config", cfg, url}, "\n"))
		out := new(syncBuffer)
		cmd.Stdout, cmd.Stderr = out, out
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		cmds = append(cmds, cmd)
		outputs = append(outputs, out)
	}
	for i, cmd := range cmds {
		if err := cmd.Wait(); err != nil {
			t.Errorf("child %d: %v\n%s", i, err, outputs[i])
		}
	}

	loaded, err := config.Load(cfg)
	if err != nil {
		t.Fatalf("the file does not load: %v", err)
	}
	var got []string
	for _, target := range loaded.Targets {
		got = append(got, target.URL)
	}
	want := append([]string{"https://github.com/acme/api"}, urls...)
	if !sameSet(got, want) {
		t.Errorf("targets = %v, want %v", got, want)
	}
}

func TestRepoEditEntry(t *testing.T) {
	const url = "https://github.com/acme/new"
	str := func(s string) *string { return &s }
	zero, five := 0, 5
	tests := []struct {
		name string
		args addRepoArgs
		want configfile.TargetEntry
	}{
		{"URL only", addRepoArgs{url: url}, configfile.TargetEntry{URL: url, MaxRunners: 1}},
		{"max-runners", addRepoArgs{url: url, maxRunners: str("5")}, configfile.TargetEntry{URL: url, MaxRunners: 5}},
		{"idle zero is kept", addRepoArgs{url: url, idle: str("0")}, configfile.TargetEntry{URL: url, MaxRunners: 1, Idle: &zero}},
		{"idle equal to max-runners", addRepoArgs{url: url, maxRunners: str("5"), idle: str("5")}, configfile.TargetEntry{URL: url, MaxRunners: 5, Idle: &five}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := tt.args.entry()
			if err != nil || !reflect.DeepEqual(got, tt.want) {
				t.Errorf("entry = %+v, %v; want %+v", got, err, tt.want)
			}
		})
	}
}

// TOML cannot hold a path that is not UTF-8, so add-repo refuses it before
// it would write the block. Some file systems do not take such a name.
func TestRepoEditTokenFileNotUTF8(t *testing.T) {
	path := filepath.Join(t.TempDir(), "token\xff")
	if err := os.WriteFile(path, []byte(fixtureToken(t, "ghp_")+"\n"), 0o600); err != nil {
		t.Skipf("the file system takes no name that is not UTF-8: %v", err)
	}
	_, err := addRepoArgs{url: "https://github.com/acme/new", tokenFile: &path}.entry()
	if err == nil || !strings.Contains(err.Error(), "UTF-8") {
		t.Errorf("error = %v, want a UTF-8 refusal", err)
	}
	if err != nil && strings.Contains(err.Error(), path) {
		t.Error("the error holds the path")
	}
}

func sameSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	seen := make(map[string]int)
	for _, s := range a {
		seen[s]++
	}
	for _, s := range b {
		seen[s]--
	}
	for _, n := range seen {
		if n != 0 {
			return false
		}
	}
	return true
}

func TestRepoEditCredentialURL(t *testing.T) {
	const msg = "value contains a credential; set the repository URL only"
	token := fixtureToken(t, "ghp_")
	pat := fixtureToken(t, "github_pat_")
	urls := []string{
		"https://x@github.com/o/r",
		"https://" + token + "@github.com/o/r",
		"https://github.com/o/" + token,
		"https://github.com/" + pat + "/r",
	}
	orig := repoEditTestdata(t, "commented.toml")
	for _, command := range []string{"add-repo", "remove-repo"} {
		for i, url := range urls {
			t.Run(fmt.Sprintf("%s %d", command, i), func(t *testing.T) {
				cfg := repoEditWrite(t, orig, 0o600)
				out := repoEditExec(t, command, "-config", cfg, url)
				if out.code != 1 || out.stdout != "" || !strings.Contains(out.stderr, msg) {
					t.Fatalf("exit = %d, stdout = %q, stderr = %q", out.code, out.stdout, out.stderr)
				}
				for _, secret := range []string{url, token, pat, "x@github"} {
					if strings.Contains(out.stdout+out.stderr, secret) {
						t.Errorf("output holds %q", secret)
					}
				}
				if got := repoEditRead(t, cfg); got != orig {
					t.Error("the file changed")
				}
				if _, err := os.Stat(cfg + ".lock"); err == nil {
					t.Error("a lock file was made for an argument that failed its check")
				}
			})
		}
	}
}

func TestRepoEditNoTokenValue(t *testing.T) {
	token := fixtureToken(t, "github_pat_")
	tokenFile := writeTokenFile(t, token)

	t.Run("edit", func(t *testing.T) {
		cfg := repoEditValidConfig(t, "https://github.com/acme/api")
		out := repoEditExec(t, "add-repo", "-config", cfg, "-token-file", tokenFile, "https://github.com/acme/new")
		if out.code != 0 {
			t.Fatalf("exit = %d, stderr = %q", out.code, out.stderr)
		}
		got := repoEditRead(t, cfg)
		if !strings.Contains(got, "token-file = "+fmt.Sprintf("%q", tokenFile)) {
			t.Errorf("the block has no token-file path:\n%s", got)
		}
		for name, text := range map[string]string{"stdout": out.stdout, "stderr": out.stderr, "file": got} {
			if strings.Contains(text, token) {
				t.Errorf("%s holds the token", name)
			}
		}
	})
	t.Run("paste block", func(t *testing.T) {
		cfg := repoEditWrite(t, repoEditTestdata(t, "inline-targets.toml"), 0o600)
		out := repoEditExec(t, "add-repo", "-config", cfg, "-token-file", tokenFile, "https://github.com/acme/new")
		if out.code != 1 || !strings.Contains(out.stderr, "token-file = "+fmt.Sprintf("%q", tokenFile)) {
			t.Fatalf("exit = %d, stderr = %q", out.code, out.stderr)
		}
		if strings.Contains(out.stdout+out.stderr, token) {
			t.Error("the output holds the token")
		}
	})
	t.Run("token file refused", func(t *testing.T) {
		wide := writeTokenFile(t, token)
		if err := os.Chmod(wide, 0o666); err != nil {
			t.Fatal(err)
		}
		cfg := repoEditValidConfig(t, "https://github.com/acme/api")
		out := repoEditExec(t, "add-repo", "-config", cfg, "-token-file", wide, "https://github.com/acme/new")
		if out.code != 1 || strings.Contains(out.stdout+out.stderr, token) || strings.Contains(out.stderr, wide) {
			t.Errorf("exit = %d, stdout = %q, stderr = %q", out.code, out.stdout, out.stderr)
		}
	})
}
