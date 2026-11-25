package service

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

const testUID = 501

// fakeLaunchctl records calls in order. After Bootout, a loaded service
// still prints as loaded for stillLoaded more Print calls (-1: forever).
// Each call notes whether the plist existed when it was made.
type fakeLaunchctl struct {
	plistPath    string
	loaded       bool
	stillLoaded  int
	bootoutErr   error
	bootstrapErr error
	printErr     error
	// pollErr is returned by every Print after Bootout.
	pollErr error
	// hangPoll makes every Print after Bootout block until its ctx ends.
	hangPoll bool

	mu     sync.Mutex
	booted bool
	// bootoutDeadline is how long the Bootout ctx had left, or 0 if none.
	bootoutDeadline time.Duration
	calls           []string
	fileExists      []bool
}

func (f *fakeLaunchctl) record(call string) {
	f.calls = append(f.calls, call)
	_, err := os.Stat(f.plistPath)
	f.fileExists = append(f.fileExists, err == nil)
}

func (f *fakeLaunchctl) Print(ctx context.Context, target string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("print " + target)
	if f.booted && f.hangPoll {
		f.mu.Unlock()
		<-ctx.Done()
		f.mu.Lock()
		return false, ctx.Err()
	}
	if f.printErr != nil {
		return false, f.printErr
	}
	if f.booted && f.pollErr != nil {
		return false, f.pollErr
	}
	if f.loaded && f.booted && f.stillLoaded >= 0 {
		if f.stillLoaded == 0 {
			f.loaded = false
		} else {
			f.stillLoaded--
		}
	}
	return f.loaded, nil
}

func (f *fakeLaunchctl) Bootout(ctx context.Context, target string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("bootout " + target)
	if d, ok := ctx.Deadline(); ok {
		f.bootoutDeadline = time.Until(d)
	}
	f.booted = true
	return f.bootoutErr
}

func (f *fakeLaunchctl) Bootstrap(_ context.Context, domain, path string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("bootstrap " + domain + " " + path)
	return f.bootstrapErr
}

func (f *fakeLaunchctl) snapshot() ([]string, []bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...), append([]bool(nil), f.fileExists...)
}

const target = "gui/501/" + Label

func newTestManager(t *testing.T, f *fakeLaunchctl) (*Manager, string) {
	t.Helper()
	home := t.TempDir()
	m := NewManager(f, testUID, home, slog.New(slog.NewTextHandler(io.Discard, nil)))
	m.pollInterval = time.Millisecond
	m.printTimeout = 5 * time.Second
	// Tests must not wait minutes; ones that check the limit override this.
	m.stopLimit = func(time.Duration) time.Duration { return 5 * time.Second }
	f.plistPath = m.PlistPath(Label)
	return m, home
}

// recordLimit makes the stop wait 50ms and records the ExitTimeOut it was
// computed from.
func recordLimit(m *Manager) *time.Duration {
	var got time.Duration
	m.stopLimit = func(d time.Duration) time.Duration {
		got = d
		return 50 * time.Millisecond
	}
	return &got
}

func homeJob(home string) Job {
	j := testJob()
	j.Home = home
	j.DrainTimeout = time.Millisecond // ExitTimeOut 61s
	return j
}

func TestInstallFresh(t *testing.T) {
	f := &fakeLaunchctl{}
	m, home := newTestManager(t, f)
	j := homeJob(home)
	if err := m.Install(context.Background(), j); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(home, "Library", "LaunchAgents", Label+".plist")
	calls, exists := f.snapshot()
	want := []string{"print " + target, "bootstrap gui/501/ " + path}
	if strings.Join(calls, "\n") != strings.Join(want, "\n") {
		t.Fatalf("calls = %q, want %q", calls, want)
	}
	if exists[0] || !exists[1] {
		t.Errorf("plist existed at calls = %v, want written after print and before bootstrap", exists)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o644 {
		t.Errorf("plist mode = %v, want 0644", info.Mode().Perm())
	}
	got, _ := os.ReadFile(path)
	want2, _ := Plist(j)
	if string(got) != string(want2) {
		t.Errorf("plist content differs from Plist(j)")
	}
	logInfo, err := os.Stat(filepath.Join(home, "Library", "Logs", "actions-runner-scaler"))
	if err != nil || !logInfo.IsDir() || logInfo.Mode().Perm() != 0o700 {
		t.Errorf("log dir = %v, %v, want a 0700 directory", logInfo, err)
	}
	leftovers, _ := filepath.Glob(filepath.Join(filepath.Dir(path), ".tmp-*"))
	if len(leftovers) != 0 {
		t.Errorf("temporary files left: %v", leftovers)
	}
}

// The plist mode is 0644 even under a restrictive umask.
func TestInstallModeIgnoresUmask(t *testing.T) {
	old := syscall.Umask(0o077) // process-wide; tests in this package are not parallel
	defer syscall.Umask(old)
	f := &fakeLaunchctl{}
	m, home := newTestManager(t, f)
	path := m.PlistPath(Label)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	// An existing file with other permissions is replaced.
	if err := os.WriteFile(path, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := m.Install(context.Background(), homeJob(home)); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o644 {
		t.Errorf("plist mode = %v, want 0644", info.Mode().Perm())
	}
}

// A loaded service is booted out and the file is written only after print
// stops finding it.
func TestInstallWaitsForBootout(t *testing.T) {
	f := &fakeLaunchctl{loaded: true, stillLoaded: 3}
	m, home := newTestManager(t, f)
	writeInstalled(t, m, 1)
	if err := m.Install(context.Background(), homeJob(home)); err != nil {
		t.Fatal(err)
	}
	calls, exists := f.snapshot()
	want := []string{
		"print " + target, "bootout " + target,
		"print " + target, "print " + target, "print " + target, "print " + target,
		"bootstrap gui/501/ " + m.PlistPath(Label),
	}
	if strings.Join(calls, "\n") != strings.Join(want, "\n") {
		t.Fatalf("calls = %q, want %q", calls, want)
	}
	content, _ := os.ReadFile(m.PlistPath(Label))
	if !strings.Contains(string(content), "<integer>61</integer>") {
		t.Errorf("plist not rewritten:\n%s", content)
	}
	// The old file (ExitTimeOut 1) is still in place at every call before
	// bootstrap, and the new one is in place at bootstrap.
	for i, e := range exists {
		if !e {
			t.Errorf("plist missing at call %d (%s)", i, calls[i])
		}
	}
}

// A bootout that never takes effect fails at the stop limit, without
// writing or bootstrapping, and reports the bootout error. With no
// installed plist the limit comes from the new job's ExitTimeOut.
func TestInstallBootoutNeverConfirms(t *testing.T) {
	f := &fakeLaunchctl{loaded: true, stillLoaded: -1, bootoutErr: errors.New("launchctl bootout: exit status 5")}
	m, home := newTestManager(t, f)
	from := recordLimit(m)
	start := time.Now()
	err := m.Install(context.Background(), homeJob(home))
	if err == nil || !strings.Contains(err.Error(), "still loaded") || !strings.Contains(err.Error(), "exit status 5") {
		t.Fatalf("err = %v, want still loaded and the bootout error", err)
	}
	if d := time.Since(start); d < 50*time.Millisecond || d > 5*time.Second {
		t.Errorf("Install returned after %s, want about 50ms", d)
	}
	if *from != 61*time.Second {
		t.Errorf("limit computed from %s, want the job's ExitTimeOut 61s", *from)
	}
	calls, _ := f.snapshot()
	if n := strings.Count(strings.Join(calls, "\n"), "bootout"); n != 1 {
		t.Errorf("bootout called %d times, want 1: %q", n, calls)
	}
	assertNoWriteNoBootstrap(t, f, m)
}

// The wait is also bounded by the context.
func TestInstallBootoutCanceled(t *testing.T) {
	f := &fakeLaunchctl{loaded: true, stillLoaded: -1}
	m, home := newTestManager(t, f)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if err := m.Install(ctx, homeJob(home)); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v, want deadline exceeded", err)
	}
	assertNoWriteNoBootstrap(t, f, m)
}

// The limit uses the larger of the installed plist's ExitTimeOut, which
// the loaded job was started with, and the new job's.
func TestInstallLimitUsesInstalledExitTimeOut(t *testing.T) {
	for _, tt := range []struct {
		installed int
		want      time.Duration
	}{
		{3600, time.Hour},
		{1, 61 * time.Second},
	} {
		f := &fakeLaunchctl{loaded: true, stillLoaded: -1}
		m, home := newTestManager(t, f)
		writeInstalled(t, m, tt.installed)
		from := recordLimit(m)
		_ = m.Install(context.Background(), homeJob(home))
		if *from != tt.want {
			t.Errorf("installed ExitTimeOut %d: limit computed from %s, want %s", tt.installed, *from, tt.want)
		}
	}
}

func TestDefaultStopLimit(t *testing.T) {
	if got := defaultStopLimit(660 * time.Second); got != 690*time.Second {
		t.Errorf("defaultStopLimit(660s) = %s, want 690s", got)
	}
	huge := time.Duration(1<<63 - 1)
	if got := defaultStopLimit(huge); got != huge {
		t.Errorf("defaultStopLimit(max) = %s, want no overflow", got)
	}
}

// A bootout error is ignored when the service is gone anyway.
func TestInstallBootoutErrorButGone(t *testing.T) {
	f := &fakeLaunchctl{loaded: true, stillLoaded: 0, bootoutErr: errors.New("exit status 3")}
	m, home := newTestManager(t, f)
	if err := m.Install(context.Background(), homeJob(home)); err != nil {
		t.Fatal(err)
	}
}

func TestInstallErrors(t *testing.T) {
	t.Run("print fails", func(t *testing.T) {
		f := &fakeLaunchctl{printErr: errors.New("exec: no such file")}
		m, home := newTestManager(t, f)
		if err := m.Install(context.Background(), homeJob(home)); err == nil || !strings.Contains(err.Error(), "no such file") {
			t.Errorf("err = %v", err)
		}
		assertNoWriteNoBootstrap(t, f, m)
	})
	t.Run("bootstrap fails", func(t *testing.T) {
		f := &fakeLaunchctl{bootstrapErr: errors.New("exit status 5")}
		m, home := newTestManager(t, f)
		err := m.Install(context.Background(), homeJob(home))
		want := "plist written at " + m.PlistPath(Label) + " but not loaded"
		if err == nil || !strings.Contains(err.Error(), want) || !strings.Contains(err.Error(), "exit status 5") {
			t.Errorf("err = %v, want it to contain %q and the bootstrap error", err, want)
		}
	})
	t.Run("job home differs from the manager's", func(t *testing.T) {
		f := &fakeLaunchctl{}
		m, _ := newTestManager(t, f)
		j := homeJob(t.TempDir())
		if err := m.Install(context.Background(), j); err == nil || !strings.Contains(err.Error(), "home") {
			t.Errorf("err = %v, want a home mismatch", err)
		}
		if calls, _ := f.snapshot(); len(calls) != 0 {
			t.Errorf("launchctl calls = %q, want none", calls)
		}
		assertNoWriteNoBootstrap(t, f, m)
	})
	t.Run("invalid job touches nothing", func(t *testing.T) {
		f := &fakeLaunchctl{}
		m, home := newTestManager(t, f)
		j := homeJob(home)
		j.Program = "relative"
		if err := m.Install(context.Background(), j); err == nil {
			t.Error("Install succeeded")
		}
		if calls, _ := f.snapshot(); len(calls) != 0 {
			t.Errorf("launchctl calls = %q, want none", calls)
		}
		assertNoWriteNoBootstrap(t, f, m)
	})
}

func TestUninstall(t *testing.T) {
	f := &fakeLaunchctl{loaded: true, stillLoaded: 2}
	m, _ := newTestManager(t, f)
	writeInstalled(t, m, 1)
	installed, err := m.Uninstall(context.Background(), Label)
	if err != nil || !installed {
		t.Fatalf("Uninstall = %v, %v, want true, nil", installed, err)
	}
	calls, exists := f.snapshot()
	want := []string{"print " + target, "bootout " + target, "print " + target, "print " + target, "print " + target}
	if strings.Join(calls, "\n") != strings.Join(want, "\n") {
		t.Fatalf("calls = %q, want %q", calls, want)
	}
	for i, e := range exists {
		if !e {
			t.Errorf("plist removed before call %d (%s) confirmed the bootout", i, calls[i])
		}
	}
	if _, err := os.Stat(m.PlistPath(Label)); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("plist still there: %v", err)
	}
}

func TestUninstallNotLoadedNoFile(t *testing.T) {
	f := &fakeLaunchctl{}
	m, _ := newTestManager(t, f)
	installed, err := m.Uninstall(context.Background(), Label)
	if err != nil || installed {
		t.Fatalf("Uninstall = %v, %v, want false, nil", installed, err)
	}
	if calls, _ := f.snapshot(); len(calls) != 1 || calls[0] != "print "+target {
		t.Errorf("calls = %q, want one print", calls)
	}
}

// A plist that is present but not loaded still counts as installed.
func TestUninstallNotLoadedWithFile(t *testing.T) {
	f := &fakeLaunchctl{}
	m, _ := newTestManager(t, f)
	writeInstalled(t, m, 61)
	installed, err := m.Uninstall(context.Background(), Label)
	if err != nil || !installed {
		t.Fatalf("Uninstall = %v, %v, want true, nil", installed, err)
	}
	if _, err := os.Stat(m.PlistPath(Label)); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("plist still there: %v", err)
	}
}

// readOnlyAgentsDir makes the LaunchAgents directory unwritable for the
// rest of the test.
func readOnlyAgentsDir(t *testing.T, m *Manager) {
	t.Helper()
	dir := filepath.Dir(m.PlistPath(Label))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(dir, 0o755) })
}

// A failure after the stop says what state the service is left in.
func TestFailureAfterStopStatesTheState(t *testing.T) {
	t.Run("install, old plist kept", func(t *testing.T) {
		f := &fakeLaunchctl{loaded: true, stillLoaded: 0}
		m, home := newTestManager(t, f)
		writeInstalled(t, m, 61)
		readOnlyAgentsDir(t, m)
		err := m.Install(context.Background(), homeJob(home))
		if err == nil || !strings.Contains(err.Error(), "service stopped; old plist kept and loads at next login") {
			t.Errorf("err = %v", err)
		}
	})
	t.Run("install, no old plist", func(t *testing.T) {
		f := &fakeLaunchctl{loaded: true, stillLoaded: 0}
		m, home := newTestManager(t, f)
		readOnlyAgentsDir(t, m)
		err := m.Install(context.Background(), homeJob(home))
		if err == nil || !strings.Contains(err.Error(), "service stopped and not reinstalled") {
			t.Errorf("err = %v", err)
		}
	})
	t.Run("install, nothing was running", func(t *testing.T) {
		f := &fakeLaunchctl{}
		m, home := newTestManager(t, f)
		readOnlyAgentsDir(t, m)
		err := m.Install(context.Background(), homeJob(home))
		if err == nil || strings.Contains(err.Error(), "service stopped") || !strings.Contains(err.Error(), "write plist") {
			t.Errorf("err = %v", err)
		}
	})
	t.Run("uninstall, plist remains", func(t *testing.T) {
		f := &fakeLaunchctl{loaded: true, stillLoaded: 0}
		m, _ := newTestManager(t, f)
		writeInstalled(t, m, 61)
		readOnlyAgentsDir(t, m)
		_, err := m.Uninstall(context.Background(), Label)
		if err == nil || !strings.Contains(err.Error(), "service stopped; plist remains and loads at next login") {
			t.Errorf("err = %v", err)
		}
	})
}

// The log directory is created before the stop, so failing to create it
// leaves the running service alone.
func TestInstallLogDirFailureBeforeStop(t *testing.T) {
	f := &fakeLaunchctl{loaded: true, stillLoaded: 0}
	m, home := newTestManager(t, f)
	if err := os.MkdirAll(filepath.Join(home, "Library"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "Library", "Logs"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := m.Install(context.Background(), homeJob(home)); err == nil || !strings.Contains(err.Error(), "log directory") {
		t.Errorf("err = %v, want a log directory error", err)
	}
	if calls, _ := f.snapshot(); len(calls) != 0 {
		t.Errorf("launchctl calls = %q, want none", calls)
	}
}

// Each print has its own timeout, so a hung print cannot outlast the stop
// deadline.
func TestHungPrintTimesOut(t *testing.T) {
	f := &fakeLaunchctl{loaded: true, stillLoaded: -1, hangPoll: true}
	m, home := newTestManager(t, f)
	m.printTimeout = 20 * time.Millisecond
	start := time.Now()
	err := m.Install(context.Background(), homeJob(home))
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v, want the print timeout", err)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("Install returned after %s, want about 20ms", d)
	}
	assertNoWriteNoBootstrap(t, f, m)
}

func TestUninstallBootoutNeverConfirms(t *testing.T) {
	f := &fakeLaunchctl{loaded: true, stillLoaded: -1}
	m, _ := newTestManager(t, f)
	writeInstalled(t, m, 0) // not a valid ExitTimeOut: falls back to 10m + 60s
	from := recordLimit(m)
	if _, err := m.Uninstall(context.Background(), Label); err == nil || !strings.Contains(err.Error(), "still loaded") {
		t.Errorf("err = %v, want still loaded", err)
	}
	if *from != 660*time.Second {
		t.Errorf("limit computed from %s, want the default 660s", *from)
	}
	if _, err := os.Stat(m.PlistPath(Label)); err != nil {
		t.Errorf("plist removed although the bootout was not confirmed: %v", err)
	}
}

// Uninstall keeps the plist whenever the bootout is not confirmed.
func TestUninstallKeepsFileWithoutConfirmation(t *testing.T) {
	tests := []struct {
		name    string
		fake    func() *fakeLaunchctl
		timeout time.Duration
		wantErr string
	}{
		{"print fails", func() *fakeLaunchctl {
			return &fakeLaunchctl{printErr: errors.New("launchctl print: exit status 5")}
		}, 0, "exit status 5"},
		{"print fails while polling", func() *fakeLaunchctl {
			return &fakeLaunchctl{loaded: true, stillLoaded: -1, pollErr: errors.New("poll broke")}
		}, 0, "poll broke"},
		{"context canceled", func() *fakeLaunchctl {
			return &fakeLaunchctl{loaded: true, stillLoaded: -1}
		}, 30 * time.Millisecond, context.DeadlineExceeded.Error()},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := tt.fake()
			m, _ := newTestManager(t, f)
			writeInstalled(t, m, 61)
			ctx := context.Background()
			if tt.timeout > 0 {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, tt.timeout)
				defer cancel()
			}
			_, err := m.Uninstall(ctx, Label)
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("err = %v, want it to contain %q", err, tt.wantErr)
			}
			if _, err := os.Stat(m.PlistPath(Label)); err != nil {
				t.Errorf("plist removed: %v", err)
			}
		})
	}
}

// A Print error while polling after bootout stops Install before it writes.
func TestInstallPollPrintError(t *testing.T) {
	f := &fakeLaunchctl{loaded: true, stillLoaded: -1, pollErr: errors.New("poll broke")}
	m, home := newTestManager(t, f)
	if err := m.Install(context.Background(), homeJob(home)); err == nil || !strings.Contains(err.Error(), "poll broke") {
		t.Errorf("err = %v, want the poll error", err)
	}
	assertNoWriteNoBootstrap(t, f, m)
}

// Bootout runs under the stop limit as its timeout.
func TestBootoutHasTimeout(t *testing.T) {
	f := &fakeLaunchctl{loaded: true, stillLoaded: 0}
	m, home := newTestManager(t, f)
	recordLimit(m) // 50ms
	if err := m.Install(context.Background(), homeJob(home)); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	d := f.bootoutDeadline
	f.mu.Unlock()
	if d <= 0 || d > 50*time.Millisecond {
		t.Errorf("bootout ctx had %s left, want a deadline within the 50ms limit", d)
	}
}

// writeInstalled writes a plist with the given ExitTimeOut as if an earlier
// install had put it there.
func writeInstalled(t *testing.T, m *Manager, exitTimeOut int) {
	t.Helper()
	path := m.PlistPath(Label)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	data := `<plist version="1.0"><dict><key>ExitTimeOut</key><integer>` +
		strconv.Itoa(exitTimeOut) + `</integer></dict></plist>`
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
}

func assertNoWriteNoBootstrap(t *testing.T, f *fakeLaunchctl, m *Manager) {
	t.Helper()
	calls, _ := f.snapshot()
	for _, c := range calls {
		if strings.HasPrefix(c, "bootstrap") {
			t.Errorf("bootstrap called: %q", calls)
		}
	}
	if _, err := os.Stat(m.PlistPath(Label)); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("plist written: %v", err)
	}
}
