package daemon

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"

	"github.com/actions/scaleset/listener"

	"github.com/dewevrei/actions-runner-scaler/internal/retry"
	"github.com/dewevrei/actions-runner-scaler/internal/scaler"
)

// sessionLoop runs listener.Run on the target's session and on each new
// one (PLAN M5-5, R25, R41, R51, R88, R99), in a goroutine of the target
// with its own backoff and listener context ctx (0002 PLAN M12-6):
//
//   - Run ends with scaler.ErrFinalizing, or because ctx ended (finalize, or
//     the end cleanup of the target error drain): the loop returns without
//     classifying; the one who ended it closes the session.
//   - Any other end closes the session (without cancellation, 10 s, errors
//     logged only) and is classified by the call that failed. Transient,
//     SessionLost and RateLimited wait the backoff and the target's session
//     and AcquireJobs next allowed times, whichever is latest, then make a
//     new session, and the error is recorded as the last error (0002 SPEC
//     R153); Permanent starts the target's error drain (0002 SPEC R171).
//   - Once a drain of the server or of the target has begun no new session
//     is made, also when the drain begins during the wait. A Run still
//     alive keeps going, so a drain keeps the session while it can (R54,
//     R92).
func (tr *targetRun) sessionLoop(ctx context.Context) {
	d := tr.d
	backoff, err := retry.NewDefaultBackoff(d.clock, d.jitter())
	if err != nil {
		tr.log.Error("no session backoff; no new sessions", "err", err)
		return
	}
	sess := tr.currentSession()
	for sess != nil {
		var polled atomic.Bool
		err := tr.runSession(ctx, sess, func() { polled.Store(true) })
		if errors.Is(err, scaler.ErrFinalizing) || ctx.Err() != nil {
			tr.log.Info("listener stopped for finalize or the target error drain's cleanup", "err", err)
			return
		}
		tr.closeSession(tr.log, sessionCloseTimeout)
		if polled.Load() {
			backoff.Reset()
		}
		op := failedOp(err)
		tr.limits.Record(op, err)
		class := retry.Classify(ctx, op, err)
		switch class {
		case retry.Canceled:
			return
		case retry.Permanent:
			tr.fatal(err)
			return
		}
		if tr.stopping() {
			tr.log.Info("listener ended during a drain; no new session (R88)", "class", class, "err", err)
			return
		}
		tr.log.Warn("listener ended; new session after backoff (R25)", "class", class, "err", err)
		tr.Record("message session", err) // 0002 SPEC R153
		sess = tr.newSession(ctx, backoff, err)
	}
}

// stopping reports whether a drain of the server or the target's error
// drain began: the target then makes no new session (R88, 0002 SPEC R173).
func (tr *targetRun) stopping() bool {
	return tr.d.draining() || tr.d.table.TargetDraining(tr.t.Key())
}

// runSession runs one listener on sess. polled is called after each
// poll the server answered (a poll cut by getMessagePollTimeout is not one).
func (tr *targetRun) runSession(ctx context.Context, sess Session, polled func()) error {
	d := tr.d
	_, setID := tr.github()
	client := newSessionClient(sess, d.table, d.clock, tr.log, d.deadlineA, polled)
	l, err := listener.New(client, listener.Config{
		ScaleSetID: setID,
		// 0002 SPEC R116: the target's max-runners, also while draining.
		MaxRunners: tr.cfg.MaxRunners,
		Logger:     tr.log.With("component", "listener"),
	})
	if err != nil {
		return err
	}
	// The placement acquires the target's jobs on this session while the
	// listener runs, and only then (0002 PLAN M14-4).
	runCtx, cancelRun := context.WithCancel(ctx)
	defer cancelRun()
	ps := &placementSession{sessionClient: client, cancel: cancelRun}
	d.placer.SetSession(tr.t.Key(), ps)
	err = l.Run(runCtx, tr.scaler)
	d.placer.SetSession(tr.t.Key(), nil)
	// Only when SessionLost's cancel ended Run: a lost recorded while Run
	// was ending for its own reason must not hide that reason.
	if lost := ps.lostErr(); lost != nil && ctx.Err() == nil && errors.Is(err, context.Canceled) {
		// Run ended because the placement found the session gone: a new
		// session per R25 (0002 SPEC R216).
		return lost
	}
	return err
}

// placementSession is the scaler.PlacementSession of one listener run:
// AcquireJobs on the run's session, and SessionLost, which ends the run so
// the session loop makes a new session (R25, 0002 SPEC R216).
type placementSession struct {
	*sessionClient
	cancel context.CancelFunc

	mu   sync.Mutex
	lost error
}

var _ scaler.PlacementSession = (*placementSession)(nil)

// SessionLost records err and ends the listener run. It never blocks.
func (s *placementSession) SessionLost(err error) {
	s.mu.Lock()
	if s.lost == nil {
		s.lost = err
	}
	s.mu.Unlock()
	s.cancel()
}

func (s *placementSession) lostErr() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lost
}

// newSession waits, then makes a new session, again after each failure,
// until one is made. It returns nil when a drain begins or ctx ends first,
// or after a permanent error, which starts the target's error drain (0002
// SPEC R171).
func (tr *targetRun) newSession(ctx context.Context, backoff *retry.Backoff, last error) Session {
	d := tr.d
	gh, setID := tr.github()
	for {
		if !tr.waitBeforeSession(ctx, backoff, last) {
			tr.log.Info("no new session: a drain began or finalize started")
			return nil
		}
		callCtx, cancel := withTimeout(ctx, d.clock, callTimeout)
		sess, err := gh.CreateSession(callCtx, setID, d.cfg.Owner)
		cancel()
		if err == nil {
			tr.setSession(sess)
			if tr.stopping() || ctx.Err() != nil {
				// A drain began during the call: keep R88.
				tr.closeSession(tr.log, sessionCloseTimeout)
				return nil
			}
			tr.log.Info("new message session")
			return sess
		}
		tr.limits.Record(retry.OpSessionCreate, err)
		if !hasOp(err) {
			err = &retry.OpError{Op: retry.OpSessionCreate, Err: err}
		}
		switch class := retry.Classify(ctx, retry.OpSessionCreate, err); class {
		case retry.Canceled:
			return nil
		case retry.Permanent:
			tr.fatal(err)
			return nil
		default:
			tr.log.Warn("making a new session failed; retrying", "class", class, "err", err)
		}
		last = err
	}
}

// waitBeforeSession waits for the backoff, or the wait last's rate limit
// asks when longer, and then for the target's session and AcquireJobs
// allowed times (R52, 0002 SPEC R265). It reports false when a drain begins
// or parent ends first.
func (tr *targetRun) waitBeforeSession(parent context.Context, backoff *retry.Backoff, last error) bool {
	d := tr.d
	ctx, cancel := chanContext(parent, d.drainStarted)
	defer cancel()
	if err := backoff.Wait(ctx, last); err != nil {
		return false
	}
	for _, op := range []retry.Op{retry.OpSessionCreate, retry.OpAcquireJobs} {
		if err := d.waitAllowed(ctx, tr.limits, tr.log, op); err != nil {
			return false
		}
	}
	return ctx.Err() == nil && !tr.stopping()
}

// draining reports whether a drain of either kind began.
func (d *Daemon) draining() bool {
	select {
	case <-d.drainStarted:
		return true
	default:
		return d.table.Drain() != scaler.DrainNone
	}
}

// failedOp returns the call that ended Run: the outermost retry.OpError in
// err, which the session adapter and the scaler put around each call's
// error. Without one, as for the listener's own checks, it is GetMessage.
func failedOp(err error) retry.Op {
	var oe *retry.OpError
	if errors.As(err, &oe) {
		return oe.Op
	}
	return retry.OpGetMessage
}
