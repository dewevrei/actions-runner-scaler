package service

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/dewevrei/actions-runner-scaler/internal/status"
)

// Daemon reaches the running scaler. The Manager signals only a pid that
// Find returned, so it never signals a process other than the one serving
// the scaler's status socket.
type Daemon interface {
	// Find queries the status socket. running is false when nothing listens
	// on it. pid is the process serving the socket as the kernel records
	// the peer, not a value from the report.
	Find(ctx context.Context) (r status.Report, pid int, running bool, err error)
	// Watch starts watching pid for its exit. Watching starts before the
	// signal, so an exit right after the signal is not missed. If pid has
	// already exited, the watch is already done.
	Watch(pid int) (ExitWatch, error)
	// Terminate sends SIGTERM to pid.
	Terminate(pid int) error
}

// ExitWatch reports the exit of a watched process.
type ExitWatch interface {
	// Wait returns nil once the process has exited, or ctx's error if ctx
	// ends first. It may be called again after ctx's error.
	Wait(ctx context.Context) error
	// Exited reports, without blocking, whether the process has exited.
	Exited() bool
	// Close releases the watch.
	Close() error
}

// ErrStillRunning matches Stop's error when the scaler has not exited
// within its limit. The scaler keeps draining.
var ErrStillRunning = errors.New("the scaler is still running")

// StillRunningError is Stop's error when the scaler has not exited within
// Limit. Signaled reports whether Stop sent it SIGTERM, so a signal drain is
// under way and the scaler exits 0 when it ends.
type StillRunningError struct {
	PID      int
	Limit    time.Duration
	Signaled bool
}

func (e *StillRunningError) Error() string {
	return fmt.Sprintf("%v: pid %d has not exited %s after the stop began; it keeps draining, so check status and run the command again later", ErrStillRunning, e.PID, e.Limit)
}

// Is makes errors.Is(err, ErrStillRunning) true.
func (e *StillRunningError) Is(target error) bool { return target == ErrStillRunning }

// Stop sends SIGTERM to the scaler serving the status socket and waits
// until that process exits, for at most StopLimit(drainTimeout). It
// reports whether a scaler was running; with none, it does nothing. After
// watching the pid it queries the socket again and signals only if the
// same pid still serves it.
//
// The first signal starts a signal drain (R20), or exits at once in
// error-wait (R48) and before the first session (R57). A scaler already
// draining is not signaled, because a second signal cuts a signal drain
// and a signal during an error drain exits at once (R21); Stop only waits,
// and signals once if the scaler moves on to error-wait. The scaler exits
// 0 on a signal (R84), so launchd does not restart it; the loaded agent
// starts it again at the next login or with Start.
//
// While waiting, Stop prints a progress line every progressInterval. If
// ctx ends, Stop returns ctx's error and the scaler keeps draining.
func (m *Manager) Stop(ctx context.Context, drainTimeout time.Duration) (running bool, err error) {
	r, pid, running, err := m.daemon.Find(ctx)
	if err != nil {
		return false, fmt.Errorf("query the scaler's status socket: %w", err)
	}
	if !running {
		return false, nil
	}
	if pid <= 1 || pid == os.Getpid() {
		return true, fmt.Errorf("the status socket reports pid %d; not signaling it", pid)
	}
	w, err := m.daemon.Watch(pid)
	if err != nil {
		return true, fmt.Errorf("watch the scaler (pid %d): %w", pid, err)
	}
	defer w.Close()
	// Ask again now that the watch is set: the same pid still serving the
	// socket means the watch follows that process, not a later process
	// that reused the pid number in between.
	r, again, running, err := m.daemon.Find(ctx)
	switch {
	case err != nil:
		return true, fmt.Errorf("query the scaler's status socket: %w", err)
	case !running:
		if waitBriefly(ctx, w) {
			fmt.Fprintf(m.out, "the scaler (pid %d) exited\n", pid)
			return true, nil
		}
		return true, fmt.Errorf("the scaler (pid %d) stopped serving its status socket but has not exited; not signaling it", pid)
	case again != pid:
		return true, fmt.Errorf("the status socket moved from pid %d to pid %d; run the command again", pid, again)
	}

	signaled := false
	terminate := func(state status.State) error {
		// The watch follows the process; once it has exited its pid may
		// be reused, so it is never signaled then.
		if w.Exited() {
			fmt.Fprintf(m.out, "the scaler (pid %d) has exited; not signaling it\n", pid)
			return nil
		}
		if err := m.daemon.Terminate(pid); err != nil {
			return fmt.Errorf("send SIGTERM to the scaler (pid %d): %w", pid, err)
		}
		signaled = true
		fmt.Fprintf(m.out, "sent SIGTERM to the scaler (pid %d, state %s)\n", pid, state)
		m.logger.Info("sent SIGTERM to the scaler", "pid", pid, "state", string(state))
		return nil
	}
	if r.State == status.Draining {
		fmt.Fprintf(m.out, "the scaler (pid %d) is already draining; waiting without another signal, which would cut the drain\n", pid)
	} else if err := terminate(r.State); err != nil {
		return true, err
	}

	limit := m.stopLimit(drainTimeout)
	fmt.Fprintf(m.out, "waiting up to %s for it to exit (drain-timeout %s)\n", limit, drainTimeout)
	start := time.Now()
	waitCtx, cancel := context.WithTimeout(ctx, limit)
	defer cancel()
	for {
		tickCtx, tickCancel := context.WithTimeout(waitCtx, m.progressInterval)
		err := w.Wait(tickCtx)
		tickCancel()
		elapsed := time.Since(start).Round(time.Second)
		switch {
		case err == nil:
			fmt.Fprintf(m.out, "the scaler (pid %d) exited after %s\n", pid, elapsed)
			m.logger.Info("the scaler exited", "pid", pid, "after", elapsed.String())
			return true, nil
		case ctx.Err() != nil:
			return true, fmt.Errorf("stopped waiting for the scaler (pid %d), which keeps draining: %w", pid, ctx.Err())
		case waitCtx.Err() != nil:
			return true, &StillRunningError{PID: pid, Limit: limit, Signaled: signaled}
		case !errors.Is(err, context.DeadlineExceeded):
			return true, fmt.Errorf("wait for the scaler (pid %d): %w", pid, err)
		}
		if err := m.progress(ctx, pid, elapsed, signaled, terminate); err != nil {
			return true, err
		}
	}
}

// exitGrace is how long Stop waits for a scaler that stopped serving its
// socket between two queries to exit, before giving up without a signal.
const exitGrace = time.Second

// waitBriefly reports whether w's process exits within exitGrace.
func waitBriefly(ctx context.Context, w ExitWatch) bool {
	ctx, cancel := context.WithTimeout(ctx, exitGrace)
	defer cancel()
	return w.Wait(ctx) == nil
}

// progress prints one "still waiting" line from the status socket, and
// signals the scaler if it has reached error-wait without a signal from
// Stop.
func (m *Manager) progress(ctx context.Context, pid int, elapsed time.Duration, signaled bool, terminate func(status.State) error) error {
	r, got, running, err := m.daemon.Find(ctx)
	switch {
	case err != nil:
		fmt.Fprintf(m.out, "still waiting (%s); status unreadable: %v\n", elapsed, err)
	case !running:
		fmt.Fprintf(m.out, "still waiting (%s); the status socket has closed\n", elapsed)
	case got != pid:
		fmt.Fprintf(m.out, "still waiting (%s); the status socket is now served by pid %d\n", elapsed, got)
	default:
		busy := 0
		for _, t := range r.Targets {
			busy += t.Busy
		}
		fmt.Fprintf(m.out, "still waiting (%s); state %s, busy runners %d\n", elapsed, r.State, busy)
		if r.State == status.ErrorWait && !signaled {
			return terminate(r.State)
		}
	}
	return nil
}
