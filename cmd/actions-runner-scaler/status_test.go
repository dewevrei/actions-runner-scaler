package main

import (
	"bytes"
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dewevrei/actions-runner-scaler/internal/logging"
	"github.com/dewevrei/actions-runner-scaler/internal/status"
)

// R27: with no scaler running, status reads the default path under HOME,
// prints "실행 중 아님" and exits 1.
func TestStatusNotRunning(t *testing.T) {
	home, err := os.MkdirTemp("", "st")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(home) })
	t.Setenv("HOME", home)

	var stdout, stderr bytes.Buffer
	if got := dispatch(context.Background(), newEnv(&stdout, &stderr), []string{"status"}, commands(refusingHosts())); got != 1 {
		t.Errorf("exit = %d, want 1", got)
	}
	if got := stdout.String(); got != status.NotRunning+"\n" {
		t.Errorf("stdout = %q, want %q", got, status.NotRunning+"\n")
	}
	// stderr is not checked: under $TMPDIR the default path can exceed the
	// unix socket limit (EINVAL), which is logged. TestStatusMissingSocketQuiet
	// checks the quiet ENOENT case.
}

// Only a bare reportedError skips the log; joined with another failure it
// is logged.
func TestDispatchReportedError(t *testing.T) {
	cmds := map[string]command{
		"reported": {run: func(context.Context, env, []string) error {
			return reportedError{err: errors.New("error-wait")}
		}},
		"joined": {run: func(context.Context, env, []string) error {
			return errors.Join(reportedError{err: errors.New("error-wait")}, errors.New("boom"))
		}},
	}
	for name, wantLog := range map[string]bool{"reported": false, "joined": true} {
		t.Run(name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			if got := dispatch(context.Background(), newEnv(&stdout, &stderr), []string{name}, cmds); got != 1 {
				t.Errorf("exit = %d, want 1", got)
			}
			if logged := strings.Contains(stderr.String(), `"msg":"command failed"`); logged != wantLog {
				t.Errorf("logged = %v, want %v: %q", logged, wantLog, stderr.String())
			}
		})
	}
}

func TestStatusUsage(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if got := dispatch(context.Background(), newEnv(&stdout, &stderr), []string{"status", "extra"}, commands(refusingHosts())); got != 1 {
		t.Errorf("exit = %d, want 1", got)
	}
	if !strings.Contains(stderr.String(), "usage: status") {
		t.Errorf("stderr = %q, want the usage", stderr.String())
	}
}

// fakeTarget is a status.TargetView with a fixed snapshot.
type fakeTarget struct{ t status.Target }

func (f fakeTarget) TargetStatus() status.Target { return f.t }

// The command prints what the daemon's socket serves and maps the state to
// the exit code (R26, R45, R56, R58). The running state needs a running or
// starting target to exit 0 (0002 SPEC R197).
func TestShowStatus(t *testing.T) {
	live := status.Target{URL: "u", State: status.TargetRunning}
	tests := []struct {
		name     string
		state    status.State
		targets  []status.Target
		wantHead string
		wantExit int
	}{
		{"starting", status.Starting, nil, "상태: 시작 중\n", 0},
		{"running", status.Running, []status.Target{live}, "상태: 정상\n", 0},
		{"draining", status.Draining, nil, "상태: drain 중\n", 0},
		{"error-wait", status.ErrorWait, nil, "상태: 오류 대기\n", 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := serveStatus(t, tt.state, tt.targets)
			// Through dispatch, so the exit code and the absence of a
			// "command failed" log are both checked.
			exit, stdout, stderr := runStatusAt(t, path)
			if exit != tt.wantExit {
				t.Errorf("exit = %d, want %d", exit, tt.wantExit)
			}
			if stderr != "" {
				t.Errorf("stderr = %q, want nothing", stderr)
			}
			if !strings.HasPrefix(stdout, tt.wantHead) {
				t.Errorf("stdout = %q, want it to start with %q", stdout, tt.wantHead)
			}
		})
	}
}

// A running scaler whose targets are all in error-draining or error-wait
// exits 1 without a "command failed" log, so a watcher learns that no
// target takes jobs (0002 SPEC R197).
func TestStatusExitCodeNoRunningTarget(t *testing.T) {
	path := serveStatus(t, status.Running, []status.Target{
		{URL: "a", State: status.TargetErrorDraining},
		{URL: "b", State: status.TargetErrorWait},
	})
	exit, stdout, stderr := runStatusAt(t, path)
	if exit != 1 || stderr != "" || !strings.HasPrefix(stdout, "상태: 정상 target 없음\n") {
		t.Errorf("exit %d, stdout %q, stderr %q; want 1, the 정상 target 없음 head, empty", exit, stdout, stderr)
	}
}

// serveStatus serves a Status with the state and targets on a socket that
// is closed when the test ends, and returns its path.
func serveStatus(t *testing.T, state status.State, targets []status.Target) string {
	t.Helper()
	path := filepath.Join(shortTempDir(t), "status.sock")
	rec, err := status.NewRecorder(logging.NewSecrets(), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	st, err := status.New(rec)
	if err != nil {
		t.Fatal(err)
	}
	st.SetState(state)
	for _, target := range targets {
		st.AddTarget(fakeTarget{target})
	}
	srv, err := status.Listen(path, st, nil)
	if err != nil {
		t.Fatal(err)
	}
	served := make(chan error, 1)
	go func() { served <- srv.Serve() }()
	t.Cleanup(func() {
		_ = srv.Shutdown(context.Background())
		<-served
	})
	return path
}

// runStatusAt runs the status command against path through dispatch.
func runStatusAt(t *testing.T, path string) (exit int, stdout, stderr string) {
	t.Helper()
	cmds := map[string]command{"status": {run: func(ctx context.Context, e env, _ []string) error {
		return showStatus(ctx, e, path)
	}}}
	var out, errOut bytes.Buffer
	exit = dispatch(context.Background(), newEnv(&out, &errOut), []string{"status"}, cmds)
	return exit, out.String(), errOut.String()
}

func shortTempDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "st")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

// A missing socket is "not running": exit 1 and nothing on stderr.
func TestStatusMissingSocketQuiet(t *testing.T) {
	exit, out, errOut := runStatusAt(t, filepath.Join(shortTempDir(t), "status.sock"))
	if exit != 1 || out != status.NotRunning+"\n" || errOut != "" {
		t.Errorf("exit %d, stdout %q, stderr %q; want 1, %q, empty", exit, out, errOut, status.NotRunning+"\n")
	}
}

// A socket the user may not open (EACCES) is a fault, not "not running":
// exit 1 and dispatch logs it.
func TestStatusPermissionDeniedLogged(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores the socket mode")
	}
	path := filepath.Join(shortTempDir(t), "status.sock")
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	if err := os.Chmod(path, 0); err != nil {
		t.Fatal(err)
	}
	exit, _, errOut := runStatusAt(t, path)
	if exit != 1 || !strings.Contains(errOut, `"msg":"command failed"`) {
		t.Errorf("exit %d, stderr %q; want 1 and a command failed log", exit, errOut)
	}
	if !strings.Contains(errOut, "permission denied") {
		t.Errorf("stderr %q, want the EACCES cause", errOut)
	}
}

// A server that accepts and never answers is a fault: exit 1 and logged.
func TestStatusSilentServerLogged(t *testing.T) {
	path := filepath.Join(shortTempDir(t), "status.sock")
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	conns := make(chan net.Conn, 1)
	go func() {
		c, err := ln.Accept()
		if err == nil {
			conns <- c
		}
		close(conns)
	}()
	t.Cleanup(func() {
		ln.Close()
		for c := range conns {
			c.Close()
		}
	})

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	var out, errOut bytes.Buffer
	cmds := map[string]command{"status": {run: func(_ context.Context, e env, _ []string) error {
		return showStatus(ctx, e, path)
	}}}
	exit := dispatch(context.Background(), newEnv(&out, &errOut), []string{"status"}, cmds)
	if exit != 1 || !strings.Contains(errOut.String(), `"msg":"command failed"`) {
		t.Errorf("exit %d, stderr %q; want 1 and a command failed log", exit, errOut.String())
	}
	if out.String() != status.Unreadable+"\n" {
		t.Errorf("stdout %q, want %q", out.String(), status.Unreadable+"\n")
	}
}
