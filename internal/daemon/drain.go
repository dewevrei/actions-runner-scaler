package daemon

import (
	"context"
	"errors"
	"time"

	"github.com/dewevrei/actions-runner-scaler/internal/retry"
	"github.com/dewevrei/actions-runner-scaler/internal/scaler"
)

// Target error wait retry intervals (0002 SPEC R174): the first retry 30 s
// after entering the wait, the interval doubled after each failed retry up
// to 30 minutes, with no limit on the number of retries.
const (
	targetRetryFirst = 30 * time.Second
	targetRetryMax   = 30 * time.Minute
)

// errRetryStopped means a drain or finalize began as a retry made its
// session; no listener runs on it and finalize closes it (0002 SPEC R192).
var errRetryStopped = errors.New("daemon: a drain or finalize began during the retry")

// fatal starts the target error drain on a permanent error of the target's
// message session call, scale set call or AcquireJobs (0002 SPEC R171). It
// is the target Scaler's Config.Fatal, which the Placer calls with its lock
// held, and the session loop's reaction to such an error. It never blocks,
// takes only tr.mu and the table's lock, and starts one drain however often
// it is called. Only a running target enters the drain: a late call after
// the drain ended or during the target error wait changes nothing, so it
// cannot leave a target that runs again with its drain mark set. During a
// server drain or finalize the error is logged and recorded as the target's
// last error only (R100, 0002 SPEC R191).
func (tr *targetRun) fatal(err error) {
	d := tr.d
	if d.draining() {
		tr.mu.Lock()
		repeat := tr.loggedOnly == err.Error()
		tr.loggedOnly = err.Error()
		tr.mu.Unlock()
		if !repeat {
			tr.Record("target error", err)
			tr.log.Warn("permanent error during a server drain; logged only (R100, 0002 SPEC R191)", "err", err)
		}
		return
	}
	tr.mu.Lock()
	tr.beginErrorDrainLocked()
	report := tr.state == targetErrorDrain && !tr.drainRecorded
	if report {
		tr.drainRecorded = true
	}
	state := tr.state
	tr.mu.Unlock()
	if !report {
		tr.log.Debug("permanent error again or while not running", "state", state, "err", err)
		return
	}
	tr.Record("target error drain", err)
	tr.log.Error("permanent error; target error drain started (0002 SPEC R171)", "err", err)
}

// beginErrorDrainLocked moves a running target into its error drain, with
// tr.mu held: the drain mark first, so no placement acquires or starts for
// it however late the drain itself begins (R51), then state error-drain and
// fatalCh closed. It reports false, changing nothing, for a target not
// running.
func (tr *targetRun) beginErrorDrainLocked() bool {
	if tr.state != targetRunning {
		return false
	}
	tr.d.table.SetTargetDraining(tr.t.Key())
	tr.state = targetErrorDrain
	tr.drainRecorded = false
	close(tr.fatalCh)
	return true
}

// errorDrain is the target error drain (0002 SPEC R173): the 0001 error
// drain rules on this target's runners and session only, then the end
// cleanup. cancelListen ends the session loop that sessDone reports. It
// reports whether the drain ran to its end; when a server drain or
// finalize begins first, the target's drain stops its own timeout and
// leaves its runners and session to the server's drain (0002 SPEC R179).
func (tr *targetRun) errorDrain(cancelListen context.CancelFunc, sessDone <-chan struct{}) bool {
	d := tr.d
	// No entry and no placement for the target until it runs again (0002
	// SPEC R210); a permanent AcquireJobs error made it inactive already.
	d.placer.SetActive(tr.t.Key(), false)
	// d.startCtx ends when a drain of the server or finalize begins.
	res := tr.scaler.DrainTarget(d.startCtx, d.drainTimeout)
	if res == scaler.DrainInterrupted {
		tr.log.Info("target error drain stopped: a server drain or finalize began, which waits for the target's runners (0002 SPEC R179)")
		return false
	}
	tr.log.Info("target error drain ended; cleaning up the target", "result", res)
	tr.endCleanup(cancelListen, sessDone)
	return true
}

// endCleanup is the target's share of finalize at the end of its error
// drain (0002 PLAN M13-2, M13-3; 0002 SPEC Design 오류 범위 "drain 끝의
// 정리"), in the stage shares of finalize (PLAN M5-8): the target's
// finalize mark, its watches, removal retries, job check timers and deny
// retries stopped (R190, R174), its listener ended, its work in flight waited for, its rows left removed with
// RemoveRunner tried once (R55), and its session closed (R99). It touches
// no other target's runners, and the server's reconciliation goes on.
func (tr *targetRun) endCleanup(cancelListen context.CancelFunc, sessDone <-chan struct{}) {
	d := tr.d
	log := tr.log.With("phase", "target cleanup")
	t0 := d.clock.Now()
	a := t0.Add(finalizeRunShare)
	log.Info("target cleanup started")

	// 1. Reports of the target's runners are no longer judged (R131), and
	// reconciliation skips its rows (R190).
	d.table.BeginTargetFinalize(tr.t.Key())
	// 2. Its watches and pending removal retries stop (R190), and so do its
	// job check timers and deny retries, which would otherwise call Docker
	// and GitHub for the target in its error wait (R174).
	tr.scaler.StopWatches()
	tr.judge.Stop()
	// 3. Its listener ends.
	cancelListen()
	if !d.waitUntil(sessDone, a) {
		log.Warn("the target's listener did not return in its share")
	}
	// 4. Its work in flight. As in finalize, a call still running when the
	// share ends finishes on its own; no new step begins.
	d.waitInFlight(log, a.Add(finalizeWaitShare), []waiter{
		{"scaler work", tr.scaler.WaitInFlight},
		{"runner watches", tr.scaler.Watcher().Wait},
		{"job checks", tr.judge.Wait},
	})
	// 5. Its rows left and its pending registrations.
	d.removeLeftovers(log, tr)
	// 6. Its session.
	tr.closeSession(log, finalizeCloseShare)
	log.Info("target cleanup ended", "took", d.clock.Now().Sub(t0))
}

// errorWait is the target error wait (0002 SPEC R174, 0002 PLAN M13-4):
// the target has no session and goal 0, and makes no GitHub call outside a
// retry. The first retry comes targetRetryFirst after entering it; each
// failed retry doubles the interval up to targetRetryMax, with no limit on
// the number of retries, and after a rate-limited retry the next one waits
// for the later of the interval and that call's R52 time. It reports true
// once a retry made a session and the target runs again (R194), false when
// a server drain or finalize began, which stops the retries (R192).
func (tr *targetRun) errorWait(app GitHub, label string) bool {
	d := tr.d
	ctx := d.startCtx
	tr.setState(targetErrorWait)
	d.placer.SetActive(tr.t.Key(), false) // 0002 SPEC R210
	interval := targetRetryFirst
	var last error
	for {
		at := d.clock.Now().Add(interval)
		if last != nil {
			op := failedOp(last)
			if lim := tr.limitsOf(op).At(op); lim.After(at) {
				at = lim
			}
		}
		if !tr.sleepUntil(ctx, at) {
			tr.log.Info("target retries stopped: a server drain or finalize began (0002 SPEC R192)")
			tr.setNextRetry(time.Time{})
			return false
		}
		err := tr.retry(ctx, app, label)
		if err == nil {
			return true
		}
		if ctx.Err() != nil || errors.Is(err, errRetryStopped) {
			tr.log.Info("target retry stopped: a server drain or finalize began (0002 SPEC R192)")
			tr.setNextRetry(time.Time{})
			return false
		}
		last = err
		interval = min(2*interval, targetRetryMax)
	}
}

// sleepUntil waits for the retry at at, which it shows as nextRetryAt once
// its timer is set, or until ctx ends; it reports whether at came.
func (tr *targetRun) sleepUntil(ctx context.Context, at time.Time) bool {
	fired, t := tr.d.timer(at.Sub(tr.d.clock.Now()))
	defer t.Stop()
	tr.setNextRetry(at)
	tr.log.Info("target error wait; next retry", "at", at)
	select {
	case <-ctx.Done():
		return false
	case <-fired:
		return true
	}
}

// setNextRetry sets nextRetryAt; zero means no retry is due.
func (tr *targetRun) setNextRetry(at time.Time) {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	tr.nextRetryAt = at
}

// limitsOf returns the R52 waits that op is recorded in: the installation
// lookup's are the server's (0002 SPEC R189), every other call's the
// target's (R188).
func (tr *targetRun) limitsOf(op retry.Op) *retry.NextAllowed {
	if op == retry.OpGetInstallation {
		return tr.d.limits
	}
	return tr.limits
}

// retry is one retry of the target error wait (0002 SPEC R193): the start
// phase from its start, each call made once, so a transient or permanent
// error ends the retry as a failure, which is recorded as the target's
// last error; R148's failure does not (0001 R17). A retry that made the
// session returns the target to running (R194): the target's marks are
// cleared, its watches and judge resume and it runs on that session. It returns
// ctx.Err() or errRetryStopped when a drain or finalize began (R192).
func (tr *targetRun) retry(ctx context.Context, app GitHub, label string) error {
	d := tr.d
	tr.log.Info("target retry: running the start phase again (0002 SPEC R193)")
	sc := tr.scope()
	sc.fail = func(where string, err error) error { return &waitError{where: where, err: err} }
	err := tr.start(ctx, app, label, sc, tr.callOnce)
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if err != nil {
		where := "target retry"
		var we *waitError
		if errors.As(err, &we) {
			where, err = we.where, we.err
		}
		tr.Record(where, err)
		tr.log.Warn("target retry failed", "step", where, "err", err)
		return err
	}
	if d.draining() {
		// The same window as the start phase's; finalize closes the session.
		return errRetryStopped
	}
	d.table.EndTargetError(tr.t.Key())
	tr.scaler.ResumeWatches()
	tr.judge.Resume()
	tr.setRunning()
	d.placer.SetActive(tr.t.Key(), true) // placement gives it runners again
	tr.log.Info("target retry made a session; running again (0002 SPEC R194)")
	d.firstSessionMade()
	return nil
}

// callOnce is the stepCaller of a retry (0002 SPEC R193): fn once, without
// R24's retry, and not at all while op's R52 wait in sc has not ended. Any
// failure returns sc.fail's error; ctx ending returns ctx.Err().
func (tr *targetRun) callOnce(ctx context.Context, sc callScope, where string, op retry.Op, fn func(context.Context) error) error {
	d := tr.d
	if err := ctx.Err(); err != nil {
		return err
	}
	if !sc.limits.Allowed(op) {
		return sc.fail(where, &retry.OpError{Op: op, Err: &retry.RateLimitError{
			RetryAfter: sc.limits.At(op).Sub(d.clock.Now()),
			Err:        errors.New("not allowed before the rate limit ends"),
		}})
	}
	callCtx, cancel := withTimeout(ctx, d.clock, callTimeout)
	err := fn(callCtx)
	cancel()
	if err == nil {
		return nil
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	sc.limits.Record(op, err)
	if !hasOp(err) {
		err = &retry.OpError{Op: op, Err: err}
	}
	return sc.fail(where, err)
}
