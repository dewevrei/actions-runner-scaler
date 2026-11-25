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
	if got := dispatch(context.Background(), newEnv(&stdout, &stderr), []string{"status"}, commands()); got != 1 {
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
	if got := dispatch(context.Background(), newEnv(&stdout, &stderr), []string{"status", "extra"}, commands()); got != 1 {
		t.Errorf("exit = %d, want 1", got)
	}
	if !strings.Contains(stderr.String(), "usage: status") {
		t.Errorf("stderr = %q, want the usage", stderr.String())
	}
}

// The command prints what the daemon's socket serves and maps the state to
// the exit code (R26, R45, R56, R58).
func TestShowStatus(t *testing.T) {
	tests := []struct {
		state    status.State
		wantHead string
		wantExit int
	}{
		{status.Starting, "상태: 시작 중\n", 0},
		{status.Running, "상태: 정상\n", 0},
		{status.Draining, "상태: drain 중\n", 0},
		{status.ErrorWait, "상태: 오류 대기\n", 1},
	}
	for _, tt := range tests {
		t.Run(string(tt.state), func(t *testing.T) {
			dir, err := os.MkdirTemp("", "st")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { os.RemoveAll(dir) })
			path := filepath.Join(dir, "status.sock")

			rec, err := status.NewRecorder(logging.NewSecrets(), nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			st, err := status.New(rec)
			if err != nil {
				t.Fatal(err)
			}
			st.SetState(tt.state)
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

			// Through dispatch, so the exit code and the absence of a
			// "command failed" log are both checked.
			cmds := map[string]command{"status": {run: func(ctx context.Context, e env, _ []string) error {
				return showStatus(ctx, e, path)
			}}}
			var stdout, stderr bytes.Buffer
			if got := dispatch(context.Background(), newEnv(&stdout, &stderr), []string{"status"}, cmds); got != tt.wantExit {
				t.Errorf("exit = %d, want %d", got, tt.wantExit)
			}
			if stderr.Len() != 0 {
				t.Errorf("stderr = %q, want nothing", stderr.String())
			}
			if !strings.HasPrefix(stdout.String(), tt.wantHead) {
				t.Errorf("stdout = %q, want it to start with %q", stdout.String(), tt.wantHead)
			}
		})
	}
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
