package service

import (
	"context"
	"errors"
	"fmt"
	"time"

	"golang.org/x/sys/unix"

	"github.com/dewevrei/actions-runner-scaler/internal/status"
)

// kqueuePoll is how long one kevent call waits before ExitWatch.Wait checks
// its context again.
const kqueuePoll = 100 * time.Millisecond

// SocketDaemon is the Daemon reached through the status socket at a path.
// It reads the pid with status.QueryPeer, signals with kill(2) and watches
// for the exit with kqueue(2) EVFILT_PROC NOTE_EXIT, which follows the
// process itself rather than its pid number.
type SocketDaemon struct {
	path string
}

// NewSocketDaemon returns a SocketDaemon for the status socket at path,
// normally status.DefaultSocketPath().
func NewSocketDaemon(path string) *SocketDaemon {
	return &SocketDaemon{path: path}
}

// Find queries the status socket; see Daemon.
func (d *SocketDaemon) Find(ctx context.Context) (status.Report, int, bool, error) {
	r, pid, err := status.QueryPeer(ctx, d.path)
	if errors.Is(err, status.ErrNotRunning) {
		return status.Report{}, 0, false, nil
	}
	if err != nil {
		return status.Report{}, 0, false, err
	}
	return r, pid, true, nil
}

// Terminate sends SIGTERM to pid. A pid that has already exited is not an
// error.
func (d *SocketDaemon) Terminate(pid int) error {
	if err := unix.Kill(pid, unix.SIGTERM); err != nil && !errors.Is(err, unix.ESRCH) {
		return err
	}
	return nil
}

// Watch registers pid with a new kqueue for NOTE_EXIT. A pid that has
// already exited (ESRCH) gives a watch that is already done.
func (d *SocketDaemon) Watch(pid int) (ExitWatch, error) {
	kq, err := unix.Kqueue()
	if err != nil {
		return nil, fmt.Errorf("kqueue: %w", err)
	}
	unix.CloseOnExec(kq)
	var ev unix.Kevent_t
	unix.SetKevent(&ev, pid, unix.EVFILT_PROC, unix.EV_ADD|unix.EV_ONESHOT)
	ev.Fflags = unix.NOTE_EXIT
	if _, err := unix.Kevent(kq, []unix.Kevent_t{ev}, nil, nil); err != nil {
		unix.Close(kq)
		if errors.Is(err, unix.ESRCH) {
			return &kqueueWatch{kq: -1, exited: true}, nil
		}
		return nil, fmt.Errorf("kevent: %w", err)
	}
	return &kqueueWatch{kq: kq}, nil
}

// kqueueWatch waits on a kqueue holding one EVFILT_PROC NOTE_EXIT event. It
// is used from one goroutine.
type kqueueWatch struct {
	kq     int
	exited bool
}

func (w *kqueueWatch) Wait(ctx context.Context) error {
	events := make([]unix.Kevent_t, 1)
	for !w.exited {
		if err := ctx.Err(); err != nil {
			return err
		}
		ts := unix.NsecToTimespec(int64(kqueuePoll))
		n, err := unix.Kevent(w.kq, nil, events, &ts)
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if err != nil {
			return fmt.Errorf("kevent: %w", err)
		}
		if n > 0 && events[0].Fflags&unix.NOTE_EXIT != 0 {
			w.exited = true
		}
	}
	return nil
}

// Exited polls the kqueue once with a zero timeout.
func (w *kqueueWatch) Exited() bool {
	if w.exited {
		return true
	}
	events := make([]unix.Kevent_t, 1)
	var zero unix.Timespec
	n, err := unix.Kevent(w.kq, nil, events, &zero)
	if err == nil && n > 0 && events[0].Fflags&unix.NOTE_EXIT != 0 {
		w.exited = true
	}
	return w.exited
}

func (w *kqueueWatch) Close() error {
	if w.kq < 0 {
		return nil
	}
	err := unix.Close(w.kq)
	w.kq = -1
	return err
}
