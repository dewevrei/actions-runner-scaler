package status

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"
)

const (
	// socketMode is the socket file's mode: owner read and write (R43).
	// Connecting to a unix socket needs write permission on it.
	socketMode fs.FileMode = 0o600
	// dirMode is the mode Listen gives the socket's directory.
	dirMode fs.FileMode = 0o700
	// writeTimeout bounds writing one report, so a client that stops
	// reading cannot hold a connection.
	writeTimeout = 5 * time.Second
	// Accept failures other than a closed listener are retried after a
	// delay that doubles from minAcceptDelay up to maxAcceptDelay.
	minAcceptDelay = 5 * time.Millisecond
	maxAcceptDelay = time.Second
)

// DefaultSocketPath returns ~/.local/state/actions-runner-scaler/status.sock
// (SPEC "외부 계약").
func DefaultSocketPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("status: socket path: %w", err)
	}
	return filepath.Join(home, ".local", "state", "actions-runner-scaler", "status.sock"), nil
}

// Reporter builds the report for one connection. *Status implements it.
type Reporter interface {
	Report() Report
}

// Server answers each connection on a unix socket with one JSON line of the
// report and closes it. It serves only the socket, never the job check
// address (R43).
type Server struct {
	path string
	ln   *net.UnixListener
	rep  Reporter
	log  *slog.Logger
	done chan struct{}

	mu       sync.Mutex
	shutdown bool
	conns    map[net.Conn]struct{}
	wg       sync.WaitGroup
}

// Listen removes a socket left at path by an earlier run, binds path and
// sets its mode to 0600 before returning, so no connection is accepted
// before the mode is right (R43, R60). The caller holds the daemon lock, so
// the socket removed is never a running process's.
//
// The socket's directory is created, or tightened if it already exists, to
// 0700 before the bind. bind creates the socket with the umask applied, so
// until the chmod its mode may be looser than 0600; the owner-only
// directory keeps other users from reaching it in that window. Setting the
// umask instead would change it for the whole process and race with other
// goroutines that create files.
//
// A non-socket file at path is not removed and is an error. A nil log
// discards.
func Listen(path string, rep Reporter, log *slog.Logger) (*Server, error) {
	if rep == nil {
		return nil, errors.New("status: nil Reporter")
	}
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	if err := prepareDir(filepath.Dir(path), log); err != nil {
		return nil, err
	}
	if err := removeStale(path, log); err != nil {
		return nil, err
	}
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		return nil, fmt.Errorf("status: listen on %s: %w", path, err)
	}
	if err := setSocketMode(path); err != nil {
		_ = ln.Close()
		return nil, err
	}
	return &Server{
		path:  path,
		ln:    ln,
		rep:   rep,
		log:   log,
		done:  make(chan struct{}),
		conns: make(map[net.Conn]struct{}),
	}, nil
}

// prepareDir makes dir an owner-only directory owned by this user.
func prepareDir(dir string, log *slog.Logger) error {
	if err := os.MkdirAll(dir, dirMode); err != nil {
		return fmt.Errorf("status: create %s: %w", dir, err)
	}
	fi, err := os.Lstat(dir)
	if err != nil {
		return fmt.Errorf("status: %w", err)
	}
	if !fi.IsDir() {
		return fmt.Errorf("status: %s is not a directory", dir)
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); ok && int(st.Uid) != os.Getuid() {
		return fmt.Errorf("status: %s is owned by uid %d, not this user", dir, st.Uid)
	}
	if fi.Mode().Perm()&^dirMode != 0 {
		if err := os.Chmod(dir, dirMode); err != nil {
			return fmt.Errorf("status: restrict %s: %w", dir, err)
		}
		log.Info("status directory restricted to the owner", "dir", dir, "was", fi.Mode().Perm())
	}
	return nil
}

// removeStale removes a socket left at path. A missing path is fine; any
// other kind of file is left alone and reported.
func removeStale(path string, log *slog.Logger) error {
	fi, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("status: %w", err)
	}
	if fi.Mode().Type() != fs.ModeSocket {
		return fmt.Errorf("status: %s exists and is not a socket; not removing it", path)
	}
	if err := os.Remove(path); err != nil {
		return fmt.Errorf("status: remove stale socket: %w", err)
	}
	log.Info("removed stale status socket", "path", path)
	return nil
}

// setSocketMode sets the socket's mode to 0600 and checks it.
func setSocketMode(path string) error {
	if err := os.Chmod(path, socketMode); err != nil {
		return fmt.Errorf("status: chmod %s: %w", path, err)
	}
	fi, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("status: %w", err)
	}
	if fi.Mode().Type() != fs.ModeSocket || fi.Mode().Perm() != socketMode {
		return fmt.Errorf("status: %s has mode %v, want a socket with %v", path, fi.Mode(), socketMode)
	}
	return nil
}

// Path returns the socket path.
func (s *Server) Path() string { return s.path }

// Serve accepts connections until Shutdown and returns nil then. Other
// accept errors are logged and retried after a short delay.
func (s *Server) Serve() error {
	delay := minAcceptDelay
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			if s.isShutdown() || errors.Is(err, net.ErrClosed) {
				return nil
			}
			s.log.Warn("status socket accept failed; retrying", "in", delay, "err", err)
			t := time.NewTimer(delay)
			select {
			case <-t.C:
			case <-s.done:
				t.Stop()
				return nil
			}
			delay = min(2*delay, maxAcceptDelay)
			continue
		}
		delay = minAcceptDelay
		if !s.track(conn) {
			_ = conn.Close()
			return nil
		}
		go s.handle(conn)
	}
}

// track registers conn for Shutdown. It reports false once Shutdown began.
func (s *Server) track(conn net.Conn) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.shutdown {
		return false
	}
	s.conns[conn] = struct{}{}
	s.wg.Add(1)
	return true
}

func (s *Server) handle(conn net.Conn) {
	defer func() {
		_ = conn.Close()
		s.mu.Lock()
		delete(s.conns, conn)
		s.mu.Unlock()
		s.wg.Done()
	}()
	if err := conn.SetWriteDeadline(time.Now().Add(writeTimeout)); err != nil {
		s.log.Warn("status socket: set deadline failed", "err", err)
		return
	}
	// Encode ends the value with a newline, so the client reads one line.
	if err := json.NewEncoder(conn).Encode(s.rep.Report()); err != nil {
		s.log.Info("status socket: writing the report failed", "err", err)
	}
}

func (s *Server) isShutdown() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.shutdown
}

// Shutdown stops accepting, which also removes the socket file, and waits
// for connections in flight until ctx ends. Then it closes the rest and
// returns ctx's error. Calling it again only waits again.
func (s *Server) Shutdown(ctx context.Context) error {
	s.mu.Lock()
	first := !s.shutdown
	s.shutdown = true
	s.mu.Unlock()
	if first {
		close(s.done)
		// UnixListener.Close unlinks the socket file it created.
		_ = s.ln.Close()
	}

	waited := make(chan struct{})
	go func() {
		s.wg.Wait()
		close(waited)
	}()
	select {
	case <-waited:
		return nil
	case <-ctx.Done():
	}
	s.mu.Lock()
	for c := range s.conns {
		_ = c.Close()
	}
	s.mu.Unlock()
	<-waited
	return ctx.Err()
}
