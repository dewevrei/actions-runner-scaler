package service

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dewevrei/actions-runner-scaler/internal/status"
)

// fakeDaemon stands in for the scaler behind the status socket. No real
// process is signaled. Find returns states in order, repeating the last,
// until the process has exited, and then reports nothing running.
type fakeDaemon struct {
	running bool
	pid     int
	states  []status.State
	findErr error
	// exitOnTerm makes the process exit when it is terminated.
	exitOnTerm bool
	// exitAfterFinds makes the process exit after that many Find calls
	// (0: never by count).
	exitAfterFinds int
	// laterPID, if set, is the pid every Find after the first returns.
	laterPID int
	// targets, if set, are the report's targets instead of one target with
	// one busy runner.
	targets []status.Target
	// lc, if set, receives every call so its log shows the order of
	// launchctl and daemon calls.
	lc *fakeLaunchctl

	mu         sync.Mutex
	finds      int
	terminated int
	calls      []string
	exitOnce   sync.Once
	exited     chan struct{}
}

func runningDaemon(pid int, states ...status.State) *fakeDaemon {
	return &fakeDaemon{running: true, pid: pid, states: states}
}

func (d *fakeDaemon) exitCh() chan struct{} {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.exited == nil {
		d.exited = make(chan struct{})
	}
	return d.exited
}

func (d *fakeDaemon) exit() { d.exitOnce.Do(func() { close(d.exitCh()) }) }

func (d *fakeDaemon) hasExited() bool {
	select {
	case <-d.exitCh():
		return true
	default:
		return false
	}
}

func (d *fakeDaemon) note(call string) {
	d.mu.Lock()
	d.calls = append(d.calls, call)
	d.mu.Unlock()
	if d.lc != nil {
		d.lc.note(call)
	}
}

func (d *fakeDaemon) Find(context.Context) (status.Report, int, bool, error) {
	d.note("find")
	if d.findErr != nil {
		return status.Report{}, 0, false, d.findErr
	}
	if !d.running || d.hasExited() {
		return status.Report{}, 0, false, nil
	}
	d.mu.Lock()
	d.finds++
	n := d.finds
	state := status.Running
	if len(d.states) > 0 {
		state = d.states[min(n-1, len(d.states)-1)]
	}
	d.mu.Unlock()
	targets := d.targets
	if targets == nil {
		targets = []status.Target{{URL: "p", Busy: 1}}
	}
	r := status.Report{State: state, Targets: targets}
	if d.exitAfterFinds > 0 && n >= d.exitAfterFinds {
		d.exit()
	}
	if d.laterPID != 0 && n > 1 {
		return r, d.laterPID, true, nil
	}
	return r, d.pid, true, nil
}

func (d *fakeDaemon) Watch(pid int) (ExitWatch, error) {
	d.note(fmt.Sprintf("watch %d", pid))
	return fakeWatch{ch: d.exitCh()}, nil
}

func (d *fakeDaemon) Terminate(pid int) error {
	d.note(fmt.Sprintf("terminate %d", pid))
	d.mu.Lock()
	d.terminated++
	d.mu.Unlock()
	if d.exitOnTerm {
		d.exit()
	}
	return nil
}

func (d *fakeDaemon) snapshot() (calls []string, terminated int) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.calls...), d.terminated
}

type fakeWatch struct{ ch chan struct{} }

func (w fakeWatch) Wait(ctx context.Context) error {
	select {
	case <-w.ch:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (w fakeWatch) Exited() bool {
	select {
	case <-w.ch:
		return true
	default:
		return false
	}
}

func (fakeWatch) Close() error { return nil }

// stopManager returns a Manager around d whose output goes to the returned
// buffer.
func stopManager(t *testing.T, d *fakeDaemon) (*Manager, *syncBuffer) {
	t.Helper()
	m, _ := newTestManagerWithDaemon(t, &fakeLaunchctl{}, d)
	out := &syncBuffer{}
	m.out = out
	return m, out
}

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.String()
}

// Stop signals the pid from the status socket once, after watching it, and
// returns when that process exits.
func TestStopSendsSIGTERMAndWaits(t *testing.T) {
	d := runningDaemon(4242)
	d.exitOnTerm = true
	m, out := stopManager(t, d)
	running, err := m.Stop(context.Background(), testDrain)
	if err != nil || !running {
		t.Fatalf("Stop = %v, %v, want true, nil", running, err)
	}
	calls, n := d.snapshot()
	want := []string{"find", "watch 4242", "find", "terminate 4242"}
	if strings.Join(calls, "\n") != strings.Join(want, "\n") || n != 1 {
		t.Errorf("calls = %q (terminated %d), want %q", calls, n, want)
	}
	for _, s := range []string{"sent SIGTERM to the scaler (pid 4242, state running)", "the scaler (pid 4242) exited"} {
		if !strings.Contains(out.String(), s) {
			t.Errorf("output lacks %q:\n%s", s, out)
		}
	}
}

// Stop keeps waiting, printing progress from the status socket, until the
// process exits, and does not signal again.
func TestStopWaitsUntilExit(t *testing.T) {
	d := runningDaemon(4242, status.Running, status.Running, status.Draining)
	d.exitAfterFinds = 5
	m, out := stopManager(t, d)
	running, err := m.Stop(context.Background(), testDrain)
	if err != nil || !running {
		t.Fatalf("Stop = %v, %v, want true, nil", running, err)
	}
	if !d.hasExited() {
		t.Error("Stop returned before the process exited")
	}
	if _, n := d.snapshot(); n != 1 {
		t.Errorf("terminated %d times, want 1", n)
	}
	if !strings.Contains(out.String(), "still waiting") || !strings.Contains(out.String(), "state draining, busy runners 1") {
		t.Errorf("output lacks progress lines:\n%s", out)
	}
}

// The progress line's busy count adds the busy runners of every target
// (0002 SPEC R152).
func TestStopBusySumOverTargets(t *testing.T) {
	d := runningDaemon(4242, status.Running, status.Running, status.Draining)
	d.exitAfterFinds = 5
	d.targets = []status.Target{{URL: "a", Busy: 1}, {URL: "b", Busy: 2}}
	m, out := stopManager(t, d)
	if _, err := m.Stop(context.Background(), testDrain); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "state draining, busy runners 3") {
		t.Errorf("output lacks busy runners 3:\n%s", out)
	}
}

// A scaler that does not exit within the limit is an ErrStillRunning error
// at the limit; it was signaled once and is left draining.
func TestStopTimeout(t *testing.T) {
	d := runningDaemon(4242)
	m, _ := stopManager(t, d)
	var gotDrain time.Duration
	m.stopLimit = func(drain time.Duration) time.Duration {
		gotDrain = drain
		return 50 * time.Millisecond
	}
	start := time.Now()
	running, err := m.Stop(context.Background(), 25*time.Minute)
	if !running || !errors.Is(err, ErrStillRunning) {
		t.Fatalf("Stop = %v, %v, want true, ErrStillRunning", running, err)
	}
	if el := time.Since(start); el < 50*time.Millisecond || el > 5*time.Second {
		t.Errorf("Stop returned after %s, want about 50ms", el)
	}
	if gotDrain != 25*time.Minute {
		t.Errorf("limit computed from drain %s, want 25m", gotDrain)
	}
	if _, n := d.snapshot(); n != 1 {
		t.Errorf("terminated %d times, want 1", n)
	}
}

// Canceling ctx ends the wait with ctx's error, not ErrStillRunning.
func TestStopCanceled(t *testing.T) {
	d := runningDaemon(4242)
	m, _ := stopManager(t, d)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	_, err := m.Stop(ctx, testDrain)
	if !errors.Is(err, context.DeadlineExceeded) || errors.Is(err, ErrStillRunning) {
		t.Errorf("err = %v, want the context's error", err)
	}
}

func TestStopNothingRunning(t *testing.T) {
	d := &fakeDaemon{}
	m, out := stopManager(t, d)
	running, err := m.Stop(context.Background(), testDrain)
	if err != nil || running {
		t.Fatalf("Stop = %v, %v, want false, nil", running, err)
	}
	if calls, _ := d.snapshot(); strings.Join(calls, ",") != "find" {
		t.Errorf("calls = %q, want only find", calls)
	}
	if out.String() != "" {
		t.Errorf("output = %q, want none", out)
	}
}

// A scaler already draining is waited for without a signal, which would cut
// the drain (R21).
func TestStopAlreadyDraining(t *testing.T) {
	d := runningDaemon(4242, status.Draining)
	d.exitAfterFinds = 3
	m, out := stopManager(t, d)
	if _, err := m.Stop(context.Background(), testDrain); err != nil {
		t.Fatal(err)
	}
	if calls, n := d.snapshot(); n != 0 {
		t.Errorf("terminated %d times, want 0: %q", n, calls)
	}
	if !strings.Contains(out.String(), "already draining") {
		t.Errorf("output lacks the draining notice:\n%s", out)
	}
}

// An error drain ends in error-wait, which needs a signal to exit (R48):
// Stop signals once when it sees error-wait.
func TestStopDrainingThenErrorWait(t *testing.T) {
	d := runningDaemon(4242, status.Draining, status.Draining, status.ErrorWait)
	d.exitOnTerm = true
	m, _ := stopManager(t, d)
	if _, err := m.Stop(context.Background(), testDrain); err != nil {
		t.Fatal(err)
	}
	calls, n := d.snapshot()
	if n != 1 || calls[len(calls)-1] != "terminate 4242" {
		t.Errorf("calls = %q (terminated %d), want one terminate after error-wait", calls, n)
	}
}

// After the watch is set, Stop asks again. A different pid on the socket
// is an error, and a scaler that has meanwhile exited is not signaled.
func TestStopRechecksAfterWatch(t *testing.T) {
	t.Run("pid changed", func(t *testing.T) {
		d := runningDaemon(4242)
		d.laterPID = 5353
		m, _ := stopManager(t, d)
		if _, err := m.Stop(context.Background(), testDrain); err == nil || !strings.Contains(err.Error(), "moved from pid 4242 to pid 5353") {
			t.Errorf("err = %v, want the moved socket", err)
		}
		if _, n := d.snapshot(); n != 0 {
			t.Errorf("terminated %d times, want 0", n)
		}
	})
	t.Run("exited meanwhile", func(t *testing.T) {
		d := runningDaemon(4242)
		d.exitAfterFinds = 1
		m, _ := stopManager(t, d)
		running, err := m.Stop(context.Background(), testDrain)
		if err != nil || !running {
			t.Errorf("Stop = %v, %v, want true, nil", running, err)
		}
		if _, n := d.snapshot(); n != 0 {
			t.Errorf("terminated %d times, want 0", n)
		}
	})
}

// Stop never signals a pid that could hit a process group, init or itself.
func TestStopRefusesBadPID(t *testing.T) {
	for _, pid := range []int{0, 1, -1, os.Getpid()} {
		d := runningDaemon(pid)
		m, _ := stopManager(t, d)
		if _, err := m.Stop(context.Background(), testDrain); err == nil {
			t.Errorf("pid %d: Stop succeeded", pid)
		}
		if calls, _ := d.snapshot(); strings.Join(calls, ",") != "find" {
			t.Errorf("pid %d: calls = %q, want only find", pid, calls)
		}
	}
}

func TestStopFindError(t *testing.T) {
	d := &fakeDaemon{findErr: errors.New("permission denied")}
	m, _ := stopManager(t, d)
	if _, err := m.Stop(context.Background(), testDrain); err == nil || !strings.Contains(err.Error(), "permission denied") {
		t.Errorf("err = %v, want the find error", err)
	}
	if _, n := d.snapshot(); n != 0 {
		t.Errorf("terminated %d times, want 0", n)
	}
}

func TestStopLimit(t *testing.T) {
	if got := StopLimit(10 * time.Minute); got != 11*time.Minute+30*time.Second {
		t.Errorf("StopLimit(10m) = %s, want 11m30s", got)
	}
	huge := time.Duration(1<<63 - 1)
	if got := StopLimit(huge); got != huge {
		t.Errorf("StopLimit(max) = %s, want no overflow", got)
	}
}

// When the agent is loaded and the scaler runs, uninstall stops it
// gracefully before the bootout, and the bootout comes only after the exit.
func TestUninstallStopsGracefullyFirst(t *testing.T) {
	f := &fakeLaunchctl{loaded: true, stillLoaded: 0}
	d := runningDaemon(4242)
	d.exitOnTerm = true
	m, _ := newTestManagerWithDaemon(t, f, d)
	writeInstalled(t, m)
	installed, err := m.Uninstall(context.Background(), Label, testDrain)
	if err != nil || !installed {
		t.Fatalf("Uninstall = %v, %v", installed, err)
	}
	calls, _ := f.snapshot()
	want := []string{"print " + target, "find", "watch 4242", "find", "terminate 4242", "bootout " + target, "print " + target}
	if strings.Join(calls, "\n") != strings.Join(want, "\n") {
		t.Fatalf("calls = %q, want %q", calls, want)
	}
}

// Install over a loaded, running agent stops the scaler gracefully, then
// boots out, writes and bootstraps.
func TestInstallStopsGracefullyFirst(t *testing.T) {
	f := &fakeLaunchctl{loaded: true, stillLoaded: 0}
	d := runningDaemon(4242)
	d.exitOnTerm = true
	m, home := newTestManagerWithDaemon(t, f, d)
	var gotDrain time.Duration
	m.stopLimit = func(drain time.Duration) time.Duration { gotDrain = drain; return 5 * time.Second }
	if err := m.Install(context.Background(), homeJob(home), 25*time.Minute); err != nil {
		t.Fatal(err)
	}
	calls, _ := f.snapshot()
	want := []string{
		"print " + target, "find", "watch 4242", "find", "terminate 4242", "bootout " + target, "print " + target,
		"bootstrap gui/501/ " + m.PlistPath(Label),
	}
	if strings.Join(calls, "\n") != strings.Join(want, "\n") {
		t.Fatalf("calls = %q, want %q", calls, want)
	}
	if gotDrain != 25*time.Minute {
		t.Errorf("stop waited for drain %s, want the config's 25m", gotDrain)
	}
}

// A scaler that does not exit in time leaves everything as it was: no
// bootout, no write, no removal.
func TestStopTimeoutChangesNothing(t *testing.T) {
	t.Run("install", func(t *testing.T) {
		f := &fakeLaunchctl{loaded: true, stillLoaded: 0}
		m, home := newTestManagerWithDaemon(t, f, runningDaemon(4242))
		m.stopLimit = func(time.Duration) time.Duration { return 30 * time.Millisecond }
		err := m.Install(context.Background(), homeJob(home), testDrain)
		if !errors.Is(err, ErrStillRunning) || !strings.Contains(err.Error(), "plist was not changed") {
			t.Errorf("err = %v, want ErrStillRunning and nothing changed", err)
		}
		assertNoBootout(t, f)
		assertNoWriteNoBootstrap(t, f, m)
	})
	t.Run("uninstall", func(t *testing.T) {
		f := &fakeLaunchctl{loaded: true, stillLoaded: 0}
		m, _ := newTestManagerWithDaemon(t, f, runningDaemon(4242))
		writeInstalled(t, m)
		m.stopLimit = func(time.Duration) time.Duration { return 30 * time.Millisecond }
		if _, err := m.Uninstall(context.Background(), Label, testDrain); !errors.Is(err, ErrStillRunning) {
			t.Errorf("err = %v, want ErrStillRunning", err)
		}
		assertNoBootout(t, f)
		if _, err := os.Stat(m.PlistPath(Label)); errors.Is(err, fs.ErrNotExist) {
			t.Error("plist removed although the scaler is still running")
		}
	})
}

// Without a loaded agent, install and uninstall do not touch the daemon.
func TestNotLoadedLeavesDaemonAlone(t *testing.T) {
	f := &fakeLaunchctl{}
	d := runningDaemon(4242)
	m, home := newTestManagerWithDaemon(t, f, d)
	if err := m.Install(context.Background(), homeJob(home), testDrain); err != nil {
		t.Fatal(err)
	}
	if calls, _ := d.snapshot(); len(calls) != 0 {
		t.Errorf("daemon calls = %q, want none", calls)
	}
}

// A process that exits just as Stop would signal it is not signaled: its
// pid may already belong to another process.
func TestStopDoesNotSignalExitedProcess(t *testing.T) {
	// Find 1 and the recheck say draining; Find 3 says error-wait, and the
	// process exits at that moment, before Stop would signal it.
	d := runningDaemon(4242, status.Draining, status.Draining, status.ErrorWait)
	d.exitAfterFinds = 3
	m, out := stopManager(t, d)
	if _, err := m.Stop(context.Background(), testDrain); err != nil {
		t.Fatal(err)
	}
	if _, n := d.snapshot(); n != 0 {
		t.Errorf("terminated %d times, want 0", n)
	}
	if !strings.Contains(out.String(), "has exited; not signaling it") {
		t.Errorf("output lacks the notice:\n%s", out)
	}
}

// If the scaler was stopped but the bootout never takes effect, the error
// says the scaler is stopped and the old agent is still loaded.
func TestUnloadStoppedButBootoutUnconfirmed(t *testing.T) {
	f := &fakeLaunchctl{loaded: true, stillLoaded: -1}
	d := runningDaemon(4242)
	d.exitOnTerm = true
	m, _ := newTestManagerWithDaemon(t, f, d)
	writeInstalled(t, m)
	m.bootoutLimit = 50 * time.Millisecond
	_, err := m.Uninstall(context.Background(), Label, testDrain)
	for _, want := range []string{"the scaler is stopped, but", "still loaded with the old plist", "still loaded 50ms after bootout"} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("err = %v, want it to contain %q", err, want)
		}
	}
	if _, err := os.Stat(m.PlistPath(Label)); err != nil {
		t.Errorf("plist removed: %v", err)
	}
}

// When the stop times out after Stop's SIGTERM, the error says the scaler
// will stay stopped after its drain; without Stop's signal it does not.
func TestStopTimeoutAfterSignalSaysStaysStopped(t *testing.T) {
	for _, tt := range []struct {
		state status.State
		want  bool
	}{{status.Running, true}, {status.Draining, false}} {
		f := &fakeLaunchctl{loaded: true, stillLoaded: 0}
		m, home := newTestManagerWithDaemon(t, f, runningDaemon(4242, tt.state))
		m.stopLimit = func(time.Duration) time.Duration { return 30 * time.Millisecond }
		err := m.Install(context.Background(), homeJob(home), testDrain)
		var sr *StillRunningError
		if !errors.As(err, &sr) || sr.Signaled != tt.want {
			t.Errorf("%s: err = %v, want a StillRunningError with Signaled %v", tt.state, err, tt.want)
		}
		if got := strings.Contains(fmt.Sprint(err), "stays stopped until"); got != tt.want {
			t.Errorf("%s: err = %v, want the stays-stopped hint %v", tt.state, err, tt.want)
		}
	}
}

func TestInstalledConfigPath(t *testing.T) {
	m, _ := newTestManager(t, &fakeLaunchctl{})
	if _, err := m.InstalledConfigPath(Label); err == nil {
		t.Error("no plist: InstalledConfigPath succeeded")
	}
	writeInstalled(t, m) // has no ProgramArguments
	if _, err := m.InstalledConfigPath(Label); err == nil || !strings.Contains(err.Error(), "no -config") {
		t.Errorf("err = %v, want no -config", err)
	}
}

func assertNoBootout(t *testing.T, f *fakeLaunchctl) {
	t.Helper()
	calls, _ := f.snapshot()
	for _, c := range calls {
		if strings.HasPrefix(c, "bootout") {
			t.Errorf("bootout called: %q", calls)
		}
	}
}
