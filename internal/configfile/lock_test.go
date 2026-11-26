package configfile

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// Environment of the re-executed test binary (TestHelperProcess).
const (
	envHelper = "CONFIGFILE_TEST_HELPER"
	envConfig = "CONFIGFILE_TEST_CONFIG"
	envLog    = "CONFIGFILE_TEST_LOG"
	envID     = "CONFIGFILE_TEST_ID"
)

const waitLimit = 10 * time.Second

// TestHelperProcess is not a test: the tests below run the test binary again
// with envHelper set so that a second process contends for the lock.
func TestHelperProcess(t *testing.T) {
	mode := os.Getenv(envHelper)
	if mode == "" {
		return
	}
	if err := runHelper(mode); err != nil {
		_, _ = os.Stderr.WriteString(err.Error() + "\n")
		os.Exit(2)
	}
	os.Exit(0)
}

func runHelper(mode string) error {
	unlock, err := Lock(os.Getenv(envConfig), io.Discard)
	if err != nil {
		return err
	}
	defer unlock()
	switch mode {
	case "hold":
		// Hold the lock until the parent closes stdin.
		if _, err := os.Stdout.WriteString("locked\n"); err != nil {
			return err
		}
		_, err := io.Copy(io.Discard, os.Stdin)
		return err
	case "append":
		id := os.Getenv(envID)
		if err := appendLine(os.Getenv(envLog), "start "+id); err != nil {
			return err
		}
		time.Sleep(200 * time.Millisecond)
		return appendLine(os.Getenv(envLog), "end "+id)
	}
	return errors.New("unknown helper mode " + mode)
}

func appendLine(path, line string) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.WriteString(line + "\n"); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

func helperCmd(t *testing.T, mode, config string, env ...string) *exec.Cmd {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestHelperProcess$")
	cmd.Env = append(os.Environ(), envHelper+"="+mode, envConfig+"="+config)
	cmd.Env = append(cmd.Env, env...)
	cmd.Stderr = os.Stderr
	return cmd
}

// newConfig writes a settings file in a new directory and returns its path
// and the lock path Lock uses for it.
func newConfig(t *testing.T) (config, lockPath string) {
	t.Helper()
	config = filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(config, []byte("original\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	realPath, err := filepath.EvalSymlinks(config)
	if err != nil {
		t.Fatal(err)
	}
	return config, realPath + ".lock"
}

// chanWriter hands each write to a channel, so a test can wait for it.
type chanWriter chan string

func (w chanWriter) Write(p []byte) (int, error) {
	w <- string(p)
	return len(p), nil
}

type lockResult struct {
	unlock func()
	err    error
}

func lockAsync(config string, notify io.Writer) <-chan lockResult {
	done := make(chan lockResult, 1)
	go func() {
		unlock, err := Lock(config, notify)
		done <- lockResult{unlock, err}
	}()
	return done
}

// expectBlocked waits for the one notice line and checks that Lock is still
// waiting.
func expectBlocked(t *testing.T, notes chanWriter, done <-chan lockResult, lockPath string) {
	t.Helper()
	select {
	case got := <-notes:
		if want := "Blocking: waiting for file lock on " + lockPath + "\n"; got != want {
			t.Fatalf("notice = %q, want %q", got, want)
		}
	case r := <-done:
		t.Fatalf("Lock returned while the lock was held: err=%v", r.err)
	case <-time.After(waitLimit):
		t.Fatal("no notice while the lock was held")
	}
	select {
	case r := <-done:
		t.Fatalf("Lock returned while the lock was held: err=%v", r.err)
	case <-time.After(200 * time.Millisecond):
	}
}

// expectAcquired waits for Lock to return and checks no second notice came.
func expectAcquired(t *testing.T, notes chanWriter, done <-chan lockResult) {
	t.Helper()
	select {
	case r := <-done:
		if r.err != nil {
			t.Fatalf("Lock: %v", r.err)
		}
		r.unlock()
	case <-time.After(waitLimit):
		t.Fatal("Lock did not return after the holder let go")
	}
	select {
	case extra := <-notes:
		t.Fatalf("second notice %q", extra)
	default:
	}
}

func TestLockAnnouncesOnlyWhenBlocked(t *testing.T) {
	config, lockPath := newConfig(t)

	var quiet bytes.Buffer
	unlock, err := Lock(config, &quiet)
	if err != nil {
		t.Fatalf("Lock: %v", err)
	}
	unlock()
	if quiet.Len() != 0 {
		t.Fatalf("notice on a free lock: %q", quiet.String())
	}

	child := helperCmd(t, "hold", config)
	stdin, err := child.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := child.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = stdin.Close()
		_ = child.Wait()
	}()
	line, err := bufio.NewReader(stdout).ReadString('\n')
	if err != nil || line != "locked\n" {
		t.Fatalf("child: %q, %v", line, err)
	}

	notes := make(chanWriter, 4)
	done := lockAsync(config, notes)
	expectBlocked(t, notes, done, lockPath)

	_ = stdin.Close()
	if err := child.Wait(); err != nil {
		t.Fatalf("child: %v", err)
	}
	expectAcquired(t, notes, done)
}

func TestLockSerializesGoroutines(t *testing.T) {
	config, lockPath := newConfig(t)
	unlock, err := Lock(config, io.Discard)
	if err != nil {
		t.Fatalf("Lock: %v", err)
	}
	notes := make(chanWriter, 4)
	done := lockAsync(config, notes)
	expectBlocked(t, notes, done, lockPath)
	unlock()
	unlock() // a second call is harmless
	expectAcquired(t, notes, done)
}

func TestLockSerializesProcesses(t *testing.T) {
	config, _ := newConfig(t)
	logPath := filepath.Join(t.TempDir(), "log")

	var children []*exec.Cmd
	for _, id := range []string{"a", "b"} {
		child := helperCmd(t, "append", config, envLog+"="+logPath, envID+"="+id)
		if err := child.Start(); err != nil {
			t.Fatal(err)
		}
		children = append(children, child)
	}
	for _, child := range children {
		if err := child.Wait(); err != nil {
			t.Fatalf("child: %v", err)
		}
	}

	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	got := string(data)
	ab := "start a\nend a\nstart b\nend b\n"
	ba := "start b\nend b\nstart a\nend a\n"
	if got != ab && got != ba {
		t.Fatalf("log interleaved or incomplete:\n%s", got)
	}
}

func TestLockMissingConfig(t *testing.T) {
	dir := t.TempDir()
	config := filepath.Join(dir, "config.toml")
	if _, err := Lock(config, io.Discard); err == nil {
		t.Fatal("Lock of a missing settings file succeeded")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("files made: %v", entries)
	}
}

// assertUnchanged checks the settings file written by newConfig.
func assertUnchanged(t *testing.T, config string) {
	t.Helper()
	data, err := os.ReadFile(config)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "original\n" {
		t.Fatalf("settings file changed: %q", data)
	}
}

func TestLockRefusesSymlink(t *testing.T) {
	config, lockPath := newConfig(t)
	target := filepath.Join(t.TempDir(), "elsewhere")
	if err := os.Symlink(target, lockPath); err != nil {
		t.Fatal(err)
	}
	_, err := Lock(config, io.Discard)
	if err == nil || !strings.Contains(err.Error(), lockPath) {
		t.Fatalf("Lock error = %v, want one naming %s", err, lockPath)
	}
	if _, err := os.Lstat(target); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("link target made: %v", err)
	}
	assertUnchanged(t, config)
}

func TestLockRefusesNonRegular(t *testing.T) {
	tests := []struct {
		name string
		make func(path string) error
	}{
		{"directory", func(path string) error { return os.Mkdir(path, 0o700) }},
		{"FIFO", func(path string) error { return unix.Mkfifo(path, 0o600) }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			config, lockPath := newConfig(t)
			if err := tt.make(lockPath); err != nil {
				t.Fatal(err)
			}
			var r lockResult
			select {
			case r = <-lockAsync(config, io.Discard):
			case <-time.After(waitLimit):
				t.Fatal("Lock blocked")
			}
			if r.err == nil || !strings.Contains(r.err.Error(), lockPath) {
				t.Fatalf("Lock error = %v, want one naming %s", r.err, lockPath)
			}
			assertUnchanged(t, config)
		})
	}
}

func TestLockFollowsConfigSymlink(t *testing.T) {
	realConfig, lockPath := newConfig(t)
	linkDir := t.TempDir()
	link := filepath.Join(linkDir, "linked.toml")
	if err := os.Symlink(realConfig, link); err != nil {
		t.Fatal(err)
	}
	unlock, err := Lock(link, io.Discard)
	if err != nil {
		t.Fatalf("Lock: %v", err)
	}
	unlock()
	if _, err := os.Stat(lockPath); err != nil {
		t.Fatalf("no lock file next to the real file: %v", err)
	}
	if _, err := os.Lstat(link + ".lock"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("lock file next to the link: %v", err)
	}
}

func TestLockFileKept(t *testing.T) {
	config, lockPath := newConfig(t)
	unlock, err := Lock(config, io.Discard)
	if err != nil {
		t.Fatalf("Lock: %v", err)
	}
	unlock()
	info, err := os.Lstat(lockPath)
	if err != nil {
		t.Fatalf("lock file removed: %v", err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
		t.Fatalf("lock file mode = %v, want regular 0600", info.Mode())
	}
}
