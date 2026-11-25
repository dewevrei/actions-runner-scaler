package service

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/dewevrei/actions-runner-scaler/internal/status"
)

// Environment of the helper process that TestMain runs instead of the
// tests: it serves a status socket and exits 0 on SIGTERM, like the
// scaler.
const (
	helperEnv       = "SERVICE_TEST_HELPER"
	helperSocketEnv = "SERVICE_TEST_SOCKET"
)

func TestMain(m *testing.M) {
	if os.Getenv(helperEnv) == "status-server" {
		os.Exit(statusServerHelper(os.Getenv(helperSocketEnv)))
	}
	os.Exit(m.Run())
}

type runningReporter struct{}

func (runningReporter) Report() status.Report { return status.Report{State: status.Running} }

func statusServerHelper(path string) int {
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGTERM)
	srv, err := status.Listen(path, runningReporter{}, nil)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	go srv.Serve()
	fmt.Println("ready")
	<-sigs
	_ = srv.Shutdown(context.Background())
	return 0
}

// startHelper starts the helper serving a status socket at path and waits
// until it listens. The process is killed and reaped when the test ends.
func startHelper(t *testing.T, path string) *exec.Cmd {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^$")
	cmd.Env = append(os.Environ(), helperEnv+"=status-server", helperSocketEnv+"="+path)
	cmd.Stderr = os.Stderr
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	waited := make(chan error, 1)
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		<-waited
	})
	line, err := bufio.NewReader(out).ReadString('\n')
	go func() { waited <- cmd.Wait() }()
	if err != nil || line != "ready\n" {
		t.Fatalf("helper did not start: %q, %v", line, err)
	}
	return cmd
}

// The real SocketDaemon reads the serving process's pid from the socket
// (not this process's), sees its exit through kqueue after SIGTERM, and
// then finds nothing running. Only the test's own child is signaled.
func TestSocketDaemonStopsTheServingProcess(t *testing.T) {
	dir, err := os.MkdirTemp("", "sd") // short: unix socket paths allow 104 bytes
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	path := filepath.Join(dir, "status.sock")
	cmd := startHelper(t, path)
	ctx := context.Background()

	d := NewSocketDaemon(path)
	r, pid, running, err := d.Find(ctx)
	if err != nil || !running || r.State != status.Running {
		t.Fatalf("Find = %+v, %d, %v, %v, want running", r, pid, running, err)
	}
	if pid != cmd.Process.Pid {
		t.Fatalf("Find pid = %d, want the helper's %d (this process is %d)", pid, cmd.Process.Pid, os.Getpid())
	}

	w, err := d.Watch(pid)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	short, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel()
	if err := w.Wait(short); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Wait before the signal = %v, want a timeout", err)
	}
	if err := d.Terminate(pid); err != nil {
		t.Fatal(err)
	}
	long, cancel2 := context.WithTimeout(ctx, 10*time.Second)
	defer cancel2()
	if err := w.Wait(long); err != nil {
		t.Fatalf("Wait after SIGTERM = %v, want the exit", err)
	}
	if _, _, running, err := d.Find(ctx); err != nil || running {
		t.Errorf("Find after exit = %v, %v, want not running", running, err)
	}
}

// Watching a pid that no longer exists is already done. (The test does not
// signal the reaped pid, which another process may have reused.)
func TestSocketDaemonGoneProcess(t *testing.T) {
	cmd := exec.Command(os.Args[0], "-test.run=^$")
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	pid := cmd.Process.Pid // reaped, so gone
	d := NewSocketDaemon("")
	w, err := d.Watch(pid)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := w.Wait(ctx); err != nil {
		t.Errorf("Wait = %v, want nil for a gone process", err)
	}
}
