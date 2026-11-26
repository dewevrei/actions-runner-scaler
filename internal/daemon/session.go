package daemon

import (
	"errors"
	"sync/atomic"

	"github.com/actions/scaleset/listener"

	"github.com/dewevrei/actions-runner-scaler/internal/retry"
	"github.com/dewevrei/actions-runner-scaler/internal/scaler"
)

// sessionLoop runs listener.Run on the first session and on each new one
// (PLAN M5-5, R25, R41, R51, R88, R90, R99):
//
//   - Run ends with scaler.ErrFinalizing, or because finalize cancelled
//     listenCtx: the loop returns without classifying; finalize closes the
//     session.
//   - Any other end closes the session (without cancellation, 10 s, errors
//     logged only) and is classified by the call that failed. Transient,
//     SessionLost and RateLimited wait the backoff and the session's and
//     AcquireJobs' next allowed times, whichever is latest, then make a new
//     session, and the error is recorded as the last error (0002 SPEC
//     R153); Permanent starts the error drain.
//   - Once a drain has begun no new session is made, also when the drain
//     begins during the wait. A Run still alive keeps going, so a drain
//     keeps the session while it can (R54, R92).
func (d *Daemon) sessionLoop() {
	backoff, err := retry.NewDefaultBackoff(d.clock, d.jitter())
	if err != nil {
		d.log.Error("no session backoff; no new sessions", "err", err)
		return
	}
	d.sessMu.Lock()
	sess := d.session
	d.sessMu.Unlock()
	for sess != nil {
		var polled atomic.Bool
		err := d.runSession(sess, func() { polled.Store(true) })
		if errors.Is(err, scaler.ErrFinalizing) || d.listenCtx.Err() != nil {
			d.log.Info("listener stopped for finalize", "err", err)
			return
		}
		d.closeSession(d.log, sessionCloseTimeout)
		if polled.Load() {
			backoff.Reset()
		}
		op := failedOp(err)
		d.limits.Record(op, err)
		class := retry.Classify(d.listenCtx, op, err)
		switch class {
		case retry.Canceled:
			return
		case retry.Permanent:
			d.fatal(err)
			return
		}
		if d.draining() {
			d.log.Info("listener ended during a drain; no new session (R88)", "class", class, "err", err)
			return
		}
		d.log.Warn("listener ended; new session after backoff (R25)", "class", class, "err", err)
		d.rec.Record("message session", err) // 0002 SPEC R153
		sess = d.newSession(backoff, err)
	}
}

// runSession runs one listener on sess. polled is called after each
// poll the server answered (a poll cut by getMessagePollTimeout is not one).
func (d *Daemon) runSession(sess Session, polled func()) error {
	p := d.pool
	client := newSessionClient(sess, p.table, d.clock, d.log, d.deadlineA, polled)
	l, err := listener.New(client, listener.Config{
		ScaleSetID: p.scaleSetID,
		// R90: the pool's max, also while draining.
		MaxRunners: p.cfg.Max,
		Logger:     d.log.With("component", "listener"),
	})
	if err != nil {
		return err
	}
	return l.Run(d.listenCtx, p.scaler.ForSession(client))
}

// newSession waits, then makes a new session, again after each failure,
// until one is made. It returns nil when a drain begins or listenCtx ends
// first, or after a permanent error, which starts the error drain (R51).
func (d *Daemon) newSession(backoff *retry.Backoff, last error) Session {
	p := d.pool
	for {
		if !d.waitBeforeSession(backoff, last) {
			d.log.Info("no new session: a drain began or finalize started")
			return nil
		}
		ctx, cancel := withTimeout(d.listenCtx, d.clock, callTimeout)
		sess, err := p.gh.CreateSession(ctx, p.scaleSetID, d.cfg.Owner)
		cancel()
		if err == nil {
			if d.draining() || d.listenCtx.Err() != nil {
				// A drain began during the call: keep R88.
				d.setSession(sess)
				d.closeSession(d.log, sessionCloseTimeout)
				return nil
			}
			d.log.Info("new message session")
			d.setSession(sess)
			return sess
		}
		d.limits.Record(retry.OpSessionCreate, err)
		if !hasOp(err) {
			err = &retry.OpError{Op: retry.OpSessionCreate, Err: err}
		}
		switch class := retry.Classify(d.listenCtx, retry.OpSessionCreate, err); class {
		case retry.Canceled:
			return nil
		case retry.Permanent:
			d.fatal(err)
			return nil
		default:
			d.log.Warn("making a new session failed; retrying", "class", class, "err", err)
		}
		last = err
	}
}

// waitBeforeSession waits for the backoff, or the wait last's rate limit
// asks when longer, and then for the session's and AcquireJobs' allowed
// times (R52). It reports false when a drain begins or listenCtx ends
// first.
func (d *Daemon) waitBeforeSession(backoff *retry.Backoff, last error) bool {
	ctx, cancel := chanContext(d.listenCtx, d.drainStarted)
	defer cancel()
	if err := backoff.Wait(ctx, last); err != nil {
		return false
	}
	for _, op := range []retry.Op{retry.OpSessionCreate, retry.OpAcquireJobs} {
		if err := d.waitAllowed(ctx, op); err != nil {
			return false
		}
	}
	return ctx.Err() == nil && !d.draining()
}

// draining reports whether a drain of either kind began.
func (d *Daemon) draining() bool {
	select {
	case <-d.drainStarted:
		return true
	default:
		return d.pool.table.Drain() != scaler.DrainNone
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
