package scaler

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/dewevrei/actions-runner-scaler/internal/retry"
)

// Retry limits for removals that run off the Scale path.
const (
	// maxRemoveRetries is how many times a transient failure to remove a
	// runner's resources is retried after the first attempt (SPEC Design,
	// retry rules: removals by watch and reconcile back off at most 10
	// times). What is left goes to the next start's R16.
	maxRemoveRetries = 10
	// maxUnregisterRetries is how many times a start cleanup's RemoveRunner
	// is retried after a transient error or a rate limit, together, before
	// it leaves the registration to finalize and the next start's R17. It is
	// the bound of the resource removal it follows.
	maxUnregisterRetries = maxRemoveRetries
)

// errNoBudget means the finalize budget ran out before a cleanup call could
// be made.
var errNoBudget = errors.New("finalize budget spent")

// callContext returns the context for one call: it keeps parent's
// cancellation and ends after d or at the finalize deadline, whichever comes
// first (M5-8), also when finalize begins after the call did (finalizeCtx).
// ok is false when the finalize budget is spent; the caller must not make
// the call.
//
// The deadline is measured on the injected Clock, because context deadlines
// follow the system clock: a call made during finalize gets the budget left
// as its timeout, and finalizeCtx ends on a Clock timer. A call ended by
// finalizeCtx fails with context.Canceled while parent goes on, which
// retry.Classify reads as transient.
func (s *Scaler) callContext(parent context.Context, d time.Duration) (ctx context.Context, cancel context.CancelFunc, ok bool) {
	if deadline, finalizing := s.table.FinalizeDeadline(); finalizing {
		s.armFinalizeDeadline()
		left := deadline.Sub(s.clock.Now())
		if left <= 0 {
			return nil, nil, false
		}
		d = min(d, left)
	}
	ctx, cancelCall := context.WithTimeout(parent, d)
	stop := context.AfterFunc(s.finalizeCtx, cancelCall)
	return ctx, func() {
		stop()
		cancelCall()
	}, true
}

// cleanupContext is callContext for a cleanup call: it ignores parent's
// cancellation, so cleanup finishes during shutdown, but still ends by the
// finalize deadline.
func (s *Scaler) cleanupContext(parent context.Context, d time.Duration) (ctx context.Context, cancel context.CancelFunc, ok bool) {
	return s.callContext(context.WithoutCancel(parent), d)
}

// removeCall makes the removal call fn on a cleanup context of
// cleanupTimeout. A failure that op classifies as Gone counts as success;
// errNoBudget means no call was made.
func (s *Scaler) removeCall(ctx context.Context, op retry.Op, fn func(context.Context) error) error {
	callCtx, cancel, ok := s.cleanupContext(ctx, cleanupTimeout)
	if !ok {
		return errNoBudget
	}
	defer cancel()
	err := fn(callCtx)
	if err != nil && retry.Classify(context.WithoutCancel(ctx), op, err) == retry.Gone {
		return nil
	}
	return err
}

// resourceRemoval removes one runner's containers, volumes and network
// after its removal began, then deletes the row (Table.MarkRemoved). A
// transient failure is retried on a timer with backoff, at most
// maxRemoveRetries times, so Scale never waits for it; a permanent failure
// is not retried. Either is logged and recorded once given up, and the row
// then stays exiting, counted against max, and abandoned with the reason
// (Table.AbandonExit), for reconciliation (M3-8), finalize and the next
// start's R16; a row abandoned on a permanent error is not claimed again
// (R42). Retries stop once finalize begins, because finalize removes every
// row left (M5-8).
type resourceRemoval struct {
	s *Scaler
	// ctx carries values only; cleanupContext drops its cancellation.
	ctx  context.Context
	name string
	log  *slog.Logger
	// owner is false for a start path that lost BeginExit to another
	// caller: it still removes what it made, but leaves the row's abandon
	// mark to the owner.
	owner bool
	// backoff is nil when it could not be made; the removal is then not
	// retried.
	backoff *retry.Backoff
	retries int
	// op classifies remove's errors.
	op retry.Op
	// remove makes one attempt with a call context (removeCall).
	remove func(context.Context) error
	// leftover is set for a resource with no row in the table (R42): the
	// removal then never touches the table.
	leftover bool
	// done, if set, is called once when the removal ends: with AbandonNone
	// once the resources are gone, or with the reason it stopped. It is not
	// called when finalize stops a pending retry timer (deferredWork.stop).
	done func(AbandonReason)
}

// removeResources begins removing name's resources. The first attempt runs
// before it returns; it reports whether the resources were gone after it.
// owner reports whether the caller won BeginExit (or ClaimAbandoned); only
// an owner marks the row abandoned when it stops early.
func (s *Scaler) removeResources(ctx context.Context, log *slog.Logger, name string, owner bool) bool {
	return s.newResourceRemoval(ctx, log, name, owner, nil).attempt()
}

// newResourceRemoval returns a removal of name's resources (RemoveResources)
// that has not made its first attempt yet. done may be nil
// (resourceRemoval.done).
func (s *Scaler) newResourceRemoval(ctx context.Context, log *slog.Logger, name string, owner bool, done func(AbandonReason)) *resourceRemoval {
	b, err := s.newRemovalBackoff()
	if err != nil {
		log.Error("no removal backoff; runner resource removal will not be retried", "err", err)
	}
	return &resourceRemoval{
		s: s, ctx: ctx, name: name, log: log, owner: owner, backoff: b, done: done,
		op: retry.OpContainerRemove,
		remove: func(ctx context.Context) error {
			return s.containers.RemoveResources(ctx, name)
		},
	}
}

func (r *resourceRemoval) attempt() bool {
	s := r.s
	err := s.removeCall(r.ctx, r.op, r.remove)
	if err == nil {
		if !r.leftover {
			s.table.MarkRemoved(r.name)
		}
		if r.retries > 0 {
			r.log.Info("runner resources removed after retries", "retries", r.retries)
		}
		r.end(AbandonNone)
		return true
	}
	const where = "remove runner resources"
	var reason AbandonReason
	switch class := retry.Classify(context.WithoutCancel(r.ctx), r.op, err); {
	case errors.Is(err, errNoBudget):
		reason = AbandonNoBudget
		r.log.Warn("finalize budget spent; runner resources left to the next start")
	case class == retry.Permanent:
		reason = AbandonPermanent
		r.log.Error("removing runner resources failed permanently; not retrying", "err", err)
		s.errs.Record(where, err)
	case s.table.Finalizing():
		reason = AbandonFinalize
		r.log.Warn("removing runner resources failed; left to finalize", "err", err)
		s.errs.Record(where, err)
	case r.backoff == nil:
		reason = AbandonNoBackoff
		r.log.Error("removing runner resources failed; not retrying without a backoff", "err", err)
		s.errs.Record(where, err)
	case r.retries >= maxRemoveRetries:
		reason = AbandonRetriesExhausted
		r.log.Error("removing runner resources failed; giving up", "retries", r.retries, "err", err)
		s.errs.Record(where, err)
	default:
		r.retries++
		wait := r.backoff.Next()
		r.log.Warn("removing runner resources failed; retrying",
			"class", class, "retry", r.retries, "in", wait, "err", err)
		if s.after(wait, r.retry) {
			return false
		}
		reason = AbandonFinalize
		r.log.Warn("finalize began; runner resources left to finalize", "err", err)
		s.errs.Record(where, err)
	}
	r.abandon(reason)
	return false
}

// abandon marks the row abandoned if this removal owns it, and ends the
// removal.
func (r *resourceRemoval) abandon(reason AbandonReason) {
	if r.owner {
		r.s.table.AbandonExit(r.name, reason)
	}
	r.end(reason)
}

// end reports the end of the removal to done, if set.
func (r *resourceRemoval) end(reason AbandonReason) {
	if r.done != nil {
		r.done(reason)
	}
}

// retire removes the resources and then the registration of a runner whose
// removal the caller owns: it won BeginExitAndRevoke or ClaimAbandoned, so
// the token is revoked already (R38), or it is the deny path, which revoked
// the token once the runner container was gone (JobJudge). It is the removal
// of watch and reconcile (R6, R42, R101): the first attempts run before it
// returns, and transient failures are retried on timers by the rules of the
// start cleanup (resourceRemoval, unregistration; SPEC Design retry table,
// "감시와 대조의 삭제"). Nothing here starts a drain (R98). id zero means no
// registration is known, so RemoveRunner is not called.
func (s *Scaler) retire(log *slog.Logger, name string, id int64, purpose unregisterPurpose) {
	// Both steps use cleanup contexts, which drop the cancellation of ctx;
	// it carries no values either.
	ctx := context.Background()
	s.removeResources(ctx, log, name, true)
	if id != 0 {
		s.unregister(ctx, log, name, id, purpose)
	}
}

// retry runs on a timer.
func (r *resourceRemoval) retry() {
	if r.s.table.Finalizing() {
		r.log.Info("finalize began; runner resource removal left to finalize")
		r.abandon(AbandonFinalize)
		return
	}
	r.attempt()
}

// unregistration removes the registration of a runner whose start failed
// after its JIT config existed (R18). A transient error is retried on a
// timer with the removal backoff, and while RemoveRunner is rate limited the
// retry waits for the later of the allowed time and the next backoff (R52);
// both count toward one bound, maxUnregisterRetries. The caller never waits
// for a retry, and no retry is scheduled or made once finalize began.
//
// The registration stays in PendingUnregistrations from the start until
// RemoveRunner succeeds, finds the runner gone, or fails permanently, so
// finalize can still try it once the row has left the table. Running out of
// retries, a spent finalize budget and finalize itself leave it there for
// finalize and the next start's R17.
type unregistration struct {
	s       *Scaler
	ctx     context.Context
	log     *slog.Logger
	id      int64
	purpose unregisterPurpose
	// backoff is nil when it could not be made; a failure then ends the
	// attempts.
	backoff *retry.Backoff
	retries int
	// lastErr is the last failure, nil while only the recorded allowed time
	// held the call back.
	lastErr error
}

// unregisterPurpose names why a registration is being removed, for logs
// and the error recorder.
type unregisterPurpose int

// The removals that go through unregistration.
const (
	// unregStart is the start cleanup (R18).
	unregStart unregisterPurpose = iota + 1
	// unregEnded is a runner whose container ended or disappeared (R6).
	unregEnded
	// unregDone is a done runner still there 5 minutes after its first
	// JobCompleted (R101).
	unregDone
	// unregAbandoned is an abandoned row that reconciliation claimed (R42).
	unregAbandoned
	// unregDenied is a runner the deny path removed (R11, R81, R102).
	unregDenied
)

// what ends "RemoveRunner for ..." in logs.
func (p unregisterPurpose) what() string {
	switch p {
	case unregStart:
		return "failed start"
	case unregEnded:
		return "ended runner"
	case unregDone:
		return "done runner"
	case unregAbandoned:
		return "abandoned runner"
	case unregDenied:
		return "denied runner"
	default:
		return "runner"
	}
}

// where names the call site for the error recorder.
func (p unregisterPurpose) where() string {
	switch p {
	case unregStart:
		return "start cleanup remove runner"
	default:
		return p.what() + " remove runner"
	}
}

// unregister begins removing registration id of runner name. The first
// attempt runs before it returns unless RemoveRunner is rate limited.
func (s *Scaler) unregister(ctx context.Context, log *slog.Logger, name string, id int64, purpose unregisterPurpose) {
	b, err := s.newRemovalBackoff()
	if err != nil {
		log.Error("no removal backoff; a failed RemoveRunner will not be retried", "err", err)
	}
	s.unregs.add(id, name)
	u := &unregistration{s: s, ctx: ctx, log: log, id: id, purpose: purpose, backoff: b}
	u.attempt()
}

// leftLog is the end of every message that leaves the registration pending.
const leftLog = "registration left to finalize and the next start"

// msg returns "RemoveRunner for <purpose> <text>".
func (u *unregistration) msg(text string) string {
	return "RemoveRunner for " + u.purpose.what() + " " + text
}

func (u *unregistration) attempt() {
	s := u.s
	if !s.limits.Allowed(retry.OpRemoveRunner) {
		u.retryLater(retry.RateLimited)
		return
	}
	callCtx, cancel, ok := s.cleanupContext(u.ctx, callTimeout)
	if !ok {
		u.log.Warn("finalize budget spent; "+leftLog, "runnerId", u.id)
		return
	}
	err := s.scaleSet.RemoveRunner(callCtx, u.id)
	cancel()
	if err == nil {
		s.unregs.remove(u.id)
		if u.retries > 0 {
			u.log.Info(u.msg("succeeded after retries"), "runnerId", u.id, "retries", u.retries)
		}
		return
	}
	s.limits.Record(retry.OpRemoveRunner, err)
	switch class := retry.Classify(context.WithoutCancel(u.ctx), retry.OpRemoveRunner, err); class {
	case retry.Gone:
		s.unregs.remove(u.id)
	case retry.Permanent:
		s.unregs.remove(u.id)
		u.log.Warn(u.msg("failed permanently; registration left to the next start"),
			"runnerId", u.id, "err", err)
		s.errs.Record(u.purpose.where(), err)
	default:
		u.lastErr = err
		u.retryLater(class)
	}
}

// retryLater schedules the next attempt after a failure of class, or gives
// up once the retries are used up. It never calls RemoveRunner itself, so a
// rate limit that is already over (Retry-After 0, a reset in the past) still
// waits one backoff step.
func (u *unregistration) retryLater(class retry.Class) {
	s := u.s
	if u.retries >= maxUnregisterRetries || u.backoff == nil {
		cause := u.lastErr
		if cause == nil {
			cause = errors.New("not allowed before the rate limit ends")
		}
		err := fmt.Errorf("gave up after %d retries: %w", u.retries, withOp(retry.OpRemoveRunner, cause))
		u.log.Error(u.msg("given up; "+leftLog),
			"runnerId", u.id, "class", class, "err", err)
		s.errs.Record(u.purpose.where(), err)
		return
	}
	u.retries++
	wait := u.backoff.Next()
	if until := s.limits.At(retry.OpRemoveRunner).Sub(s.clock.Now()); until > wait {
		wait = until
	}
	if class == retry.RateLimited {
		u.log.Info(u.msg("deferred by rate limit"),
			"runnerId", u.id, "in", wait, "retry", u.retries)
	} else {
		u.log.Warn(u.msg("failed; retrying"),
			"runnerId", u.id, "class", class, "in", wait, "retry", u.retries, "err", u.lastErr)
	}
	if !s.after(wait, u.retry) {
		u.log.Info("finalize began; "+leftLog, "runnerId", u.id)
	}
}

// retry runs on a timer.
func (u *unregistration) retry() {
	if u.s.table.Finalizing() {
		u.log.Info("finalize began; "+leftLog, "runnerId", u.id)
		return
	}
	u.attempt()
}

// newRemovalBackoff returns the backoff of one removal: 1 s doubling to
// 60 s, the default policy (R24). New makes one to check that it can.
func (s *Scaler) newRemovalBackoff() (*retry.Backoff, error) {
	return retry.NewDefaultBackoff(s.clock, s.jitter)
}

// after runs f on a timer once wait has passed and counts it as work in
// flight until it ran or was stopped, so WaitInFlight waits for it. It reports
// false, scheduling nothing, once finalize began.
func (s *Scaler) after(wait time.Duration, f func()) bool {
	if s.table.Finalizing() {
		return false
	}
	return s.deferred.after(wait, f)
}

// deferredWork holds the pending timers of cleanup steps so finalize can
// stop them (M5-8, R47).
type deferredWork struct {
	clock Clock
	work  *inFlight

	mu      sync.Mutex
	next    uint64
	timers  map[uint64]Timer
	stopped bool
}

func newDeferredWork(clock Clock, work *inFlight) *deferredWork {
	return &deferredWork{clock: clock, work: work, timers: make(map[uint64]Timer)}
}

// after schedules f unless stop was called. The timer callback removes its
// own entry first; if stop removed it already, the callback does not run f.
// Either way exactly one of stop and the callback ends the work count.
func (d *deferredWork) after(wait time.Duration, f func()) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.stopped {
		return false
	}
	id := d.next
	d.next++
	d.work.begin()
	// The callback runs in its own goroutine and takes d.mu, so it cannot
	// look for its entry before the entry is stored.
	d.timers[id] = d.clock.AfterFunc(wait, func() {
		d.mu.Lock()
		_, ok := d.timers[id]
		delete(d.timers, id)
		d.mu.Unlock()
		if !ok {
			// stop removed the entry but could not stop the timer.
			d.work.end()
			return
		}
		defer d.work.end()
		f()
	})
	return true
}

// stop prevents every pending f from running and refuses new ones. An f
// already running is not interrupted; it still counts as work in flight.
func (d *deferredWork) stop() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.stopped = true
	for id, t := range d.timers {
		delete(d.timers, id)
		if t.Stop() {
			d.work.end()
		}
	}
}

// inFlight counts running operations and wakes waiters when none remain.
// Unlike sync.WaitGroup, a new operation may begin while someone waits.
type inFlight struct {
	mu      sync.Mutex
	n       int
	waiters []chan struct{}
}

func (f *inFlight) begin() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.n++
}

func (f *inFlight) end() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.n--
	if f.n > 0 {
		return
	}
	for _, w := range f.waiters {
		close(w)
	}
	f.waiters = nil
}

func (f *inFlight) wait(ctx context.Context) error {
	f.mu.Lock()
	if f.n == 0 {
		f.mu.Unlock()
		return nil
	}
	w := make(chan struct{})
	f.waiters = append(f.waiters, w)
	f.mu.Unlock()
	select {
	case <-w:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
