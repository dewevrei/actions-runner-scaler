package status

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/dewevrei/actions-runner-scaler/internal/logging"
)

// shortDir returns a temp dir short enough for a unix socket path (macOS
// allows 104 bytes).
func shortDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "st")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

type staticReporter struct{ r Report }

func (s staticReporter) Report() Report { return s.r }

// serve starts a server at path and stops it when the test ends.
func serve(t *testing.T, path string, rep Reporter) *Server {
	t.Helper()
	srv, err := Listen(path, rep, nil)
	if err != nil {
		t.Fatal(err)
	}
	served := make(chan error, 1)
	go func() { served <- srv.Serve() }()
	t.Cleanup(func() {
		if err := srv.Shutdown(context.Background()); err != nil {
			t.Errorf("Shutdown: %v", err)
		}
		if err := <-served; err != nil {
			t.Errorf("Serve: %v", err)
		}
	})
	return srv
}

func TestListenMode(t *testing.T) {
	dir := shortDir(t)
	path := filepath.Join(dir, "state", "status.sock")
	serve(t, path, staticReporter{Report{State: Running}})

	fi, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Type() != fs.ModeSocket || fi.Mode().Perm() != 0o600 {
		t.Errorf("socket mode = %v, want a socket with 0600", fi.Mode())
	}
	di, err := os.Stat(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	if di.Mode().Perm() != 0o700 {
		t.Errorf("dir mode = %v, want 0700", di.Mode().Perm())
	}
}

// An existing directory open to others is restricted to the owner before
// the bind, and the socket still ends up 0600.
func TestListenRestrictsDir(t *testing.T) {
	dir := filepath.Join(shortDir(t), "d")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o777); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "s.sock")
	serve(t, path, staticReporter{Report{State: Running}})
	di, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if di.Mode().Perm() != 0o700 {
		t.Errorf("dir mode = %v, want 0700", di.Mode().Perm())
	}
	fi, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("socket mode = %v, want 0600", fi.Mode().Perm())
	}
}

// R60: a socket left by an earlier run is removed before listening.
func TestListenRemovesStaleSocket(t *testing.T) {
	path := filepath.Join(shortDir(t), "status.sock")
	old, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	old.SetUnlinkOnClose(false)
	old.Close()
	if _, err := os.Lstat(path); err != nil {
		t.Fatalf("stale socket missing before the test: %v", err)
	}

	serve(t, path, staticReporter{Report{State: Starting}})
	r, err := Query(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	if r.State != Starting {
		t.Errorf("state = %q, want starting", r.State)
	}
}

func TestListenKeepsNonSocket(t *testing.T) {
	path := filepath.Join(shortDir(t), "status.sock")
	if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Listen(path, staticReporter{}, nil); err == nil {
		t.Fatal("Listen over a regular file succeeded, want an error")
	}
	if b, err := os.ReadFile(path); err != nil || string(b) != "x" {
		t.Errorf("regular file changed: %q, %v", b, err)
	}
}

func TestListenNeedsReporter(t *testing.T) {
	if _, err := Listen(filepath.Join(shortDir(t), "s"), nil, nil); err == nil {
		t.Fatal("Listen(nil reporter) succeeded, want an error")
	}
}

// The server sends the live report on each connection, and Shutdown
// removes the socket so a later query fails.
func TestServeAndShutdown(t *testing.T) {
	path := filepath.Join(shortDir(t), "status.sock")
	rec, _, _ := newTestRecorder(t, logging.NewSecrets())
	st := mustNew(t, rec)
	srv, err := Listen(path, st, nil)
	if err != nil {
		t.Fatal(err)
	}
	if srv.Path() != path {
		t.Errorf("Path() = %q", srv.Path())
	}
	served := make(chan error, 1)
	go func() { served <- srv.Serve() }()

	st.AddPool("p", &fakeTable{})
	st.SetState(Draining)
	r, err := Query(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	if r.State != Draining || len(r.Pools) != 1 || r.Pools[0].Name != "p" {
		t.Errorf("report = %+v", r)
	}

	if err := srv.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := <-served; err != nil {
		t.Errorf("Serve = %v, want nil after Shutdown", err)
	}
	if _, err := os.Lstat(path); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("socket after Shutdown: %v, want it removed", err)
	}
	if _, err := Query(context.Background(), path); err == nil {
		t.Error("Query after Shutdown succeeded")
	}
	// A second Shutdown is harmless.
	if err := srv.Shutdown(context.Background()); err != nil {
		t.Errorf("second Shutdown: %v", err)
	}
}

// Shutdown before Serve releases the listener and Serve then returns nil.
func TestShutdownBeforeServe(t *testing.T) {
	path := filepath.Join(shortDir(t), "status.sock")
	srv, err := Listen(path, staticReporter{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := srv.Serve(); err != nil {
		t.Errorf("Serve after Shutdown = %v, want nil", err)
	}
}

func TestDefaultSocketPath(t *testing.T) {
	t.Setenv("HOME", "/home/someone")
	got, err := DefaultSocketPath()
	if err != nil {
		t.Fatal(err)
	}
	if want := "/home/someone/.local/state/actions-runner-scaler/status.sock"; got != want {
		t.Errorf("DefaultSocketPath() = %q, want %q", got, want)
	}
}

// bigReporter returns a report too large for the socket buffer, so the
// handler blocks writing while the client does not read. called is closed
// on the first Report.
type bigReporter struct {
	called chan struct{}
	once   sync.Once
}

func (b *bigReporter) Report() Report {
	b.once.Do(func() { close(b.called) })
	return Report{State: Running, LastError: strings.Repeat("x", 8<<20)}
}

// Shutdown with an expired ctx closes connections still being written and
// returns ctx's error.
func TestShutdownForceCloses(t *testing.T) {
	path := filepath.Join(shortDir(t), "status.sock")
	rep := &bigReporter{called: make(chan struct{})}
	srv, err := Listen(path, rep, nil)
	if err != nil {
		t.Fatal(err)
	}
	served := make(chan error, 1)
	go func() { served <- srv.Serve() }()

	conn, err := net.Dial("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	<-rep.called
	// Read the start of the report so the write is under way.
	head := make([]byte, 16)
	if _, err := io.ReadFull(conn, head); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := srv.Shutdown(ctx); !errors.Is(err, context.Canceled) {
		t.Errorf("Shutdown = %v, want context.Canceled", err)
	}
	if err := <-served; err != nil {
		t.Errorf("Serve = %v, want nil", err)
	}
	// The client gets less than the whole report, then EOF or a reset.
	n, _ := io.Copy(io.Discard, conn)
	if n+int64(len(head)) >= 8<<20 {
		t.Errorf("client read %d bytes, want the write cut short", n)
	}
}
