package scaler

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/dewevrei/actions-runner-scaler/internal/gate"
	"github.com/dewevrei/actions-runner-scaler/internal/retry"
)

const (
	// denyRetryInterval is how often the owner of a deny path tries again to
	// remove a runner container whose removal failed (PLAN M4-2).
	denyRetryInterval = 2 * time.Second
	// denyStopGrace is how long the deny path waits, after recording the
	// deny, for the runner container to stop on its own before removing it.
	// On the deny the hook kills the job's Worker; the runner then reports
	// the job failed and exits within seconds.
	denyStopGrace = 30 * time.Second
)

// JobJudge is the Judge: it records the verdict the job check server made
// from a runner's event report (0002 SPEC R123) and removes the runner of a
// job it denies.
//
// Record runs in the job check server's handler: an allow records the
// verdict and stops the runner's job check timer; a deny starts the deny
// path. It needs no session and makes no GitHub call, so it works while the
// session is down (0002 SPEC R133).
//
// The deny path, shared by a denied report, Reject (R102) and the job check
// timer (R81), runs for the one caller that wins the runner's exit. It
// records the deny at once, so the hook reads it and kills the job's
// Worker: no step runs any more, and the runner reports the job failed and
// exits. When the verdict was not pending, as for a Reject after an
// allow, the hook cannot see the deny and the wait below is skipped. In a
// new goroutine it waits for the runner container to stop, at most
// denyStopGrace on the Clock (a container that is gone counts as stopped,
// and a failed wait is tried again every 2 seconds), then removes the runner
// container with a 10-second bound, revokes the token once the container is
// confirmed gone (not found included), and removes the BuildKit container,
// the network and the registration by the removal rules of watch and
// reconcile (Scaler.retire). While the container removal fails, the owner
// tries again every 2 seconds whatever the error, logging and recording
// each new error once.
//
// New binds a JobJudge to the Scaler it is given to, which installs the
// job check timers in that Scaler's Table; it must not be used before.
// Stop ends the waits for denied runners' containers, stops the deny
// retries and the timers, and refuses new records; finalize calls it after
// Scaler.BeginFinalize (M5-8), and every step also checks the table's
// finalize mark itself. Wait waits for all of it to end, a Record in
// progress included.
//
// It is safe for concurrent use.
type JobJudge struct {
	busy *BusyTimers

	// ctx ends every wait for a denied runner's container; Stop cancels it.
	ctx    context.Context
	cancel context.CancelFunc
	// work counts the Records in progress, the deny paths Reject began, the
	// waits for denied runners' containers and the pending deny retries.
	work inFlight

	// s, log and retries are set once by bind, before any use.
	s   *Scaler
	log *slog.Logger
	// retries holds the timers of the deny retries.
	retries *deferredWork

	mu      sync.Mutex
	bound   bool
	stopped bool
}

// judgeBinder is implemented by a Judge that New binds to the Scaler.
type judgeBinder interface {
	bind(s *Scaler) error
}

// NewJobJudge returns a JobJudge to give to New as Config.Judge.
func NewJobJudge() *JobJudge {
	ctx, cancel := context.WithCancel(context.Background())
	j := &JobJudge{ctx: ctx, cancel: cancel}
	j.busy = &BusyTimers{j: j, timers: make(map[string]Timer)}
	return j
}

// bind gives the judge the Scaler whose table, clock, logger, error recorder
// and removal rules it uses, and installs the job check timers in the
// Scaler's table, so R81 cannot be left unwired. New calls it once. It fails
// when the judge or the table is bound already.
func (j *JobJudge) bind(s *Scaler) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.bound {
		return errors.New("judge already belongs to another Scaler")
	}
	// The hook reads these once installed, so they are set before it is
	// published and rolled back if it cannot be.
	j.s = s
	j.log = s.log.With("component", "judge")
	j.retries = newDeferredWork(s.clock, &j.work)
	if err := s.table.setJobCheckTimer(j.busy.arm); err != nil {
		j.s, j.log, j.retries = nil, nil, nil
		return err
	}
	j.bound = true
	return nil
}

// BusyTimers returns the judge's job check timers (R81), which bind
// installed in the table. Stop and Wait cover them too.
func (j *JobJudge) BusyTimers() *BusyTimers { return j.busy }

// Record records d, the verdict on the event report of runner name with
// event name event, and returns the runner's verdict after the attempt. It
// runs in the caller's goroutine and returns once the verdict is recorded,
// without waiting for Docker or GitHub (0002 SPEC R131): a deny starts the
// deny path, whose wait and removal run in a new goroutine. A Record in
// progress counts for Wait, so finalize waits for it (0002 PLAN M8-2). Once
// Stop was called or finalize has begun it records nothing and returns
// VerdictPending. When another path already began the runner's removal, it
// returns what that path recorded; a recorded verdict is never changed
// (0002 SPEC R132). The job check server calls it only after New bound the
// judge, so j.s is read without j.mu, as in Reject.
func (j *JobJudge) Record(name, event string, d gate.Decision) Verdict {
	if j.s == nil {
		return VerdictPending
	}
	log := j.log.With("runner", name, "event", event)
	if !j.begin() {
		log.Info("judge stopped; event report not recorded")
		return VerdictPending
	}
	defer j.work.end()
	if j.halted() {
		log.Info("judge stopped or finalize began; event report not recorded")
		return VerdictPending
	}
	if d.Allow {
		return j.allow(log, name)
	}
	if d.HeadRepo != "" {
		log = log.With("head_repo", d.HeadRepo) // 0002 SPEC R130
	}
	// BeginExitUnlessAllowed: a report racing with an allow keeps the
	// runner (0002 SPEC R132).
	return j.deny(log, name, d.Reason, j.s.table.BeginExitUnlessAllowed)
}

// Reject removes runner name through the deny path (Judge, R102). The
// runner's exit is decided before it returns, by Table.BeginExit, so a
// second call for the same runner does nothing; the removal runs in a new
// goroutine. It does nothing once finalize has begun or Stop was called.
func (j *JobJudge) Reject(name, reason string) {
	if j.s == nil {
		return
	}
	log := j.log.With("runner", name)
	if j.s.table.Finalizing() {
		log.Info("finalize began; runner not rejected", "reason", reason)
		return
	}
	if !j.begin() {
		log.Info("judge stopped; runner not rejected", "reason", reason)
		return
	}
	if !j.s.table.BeginExit(name) {
		j.work.end()
		log.Debug("runner removal already begun; reject ignored", "reason", reason)
		return
	}
	go func() {
		defer j.work.end()
		j.removeDenied(log, name, reason)
	}()
}

// Stop ends the waits for denied runners' containers, stops the pending
// deny retries and the job check timers, and refuses new records. A deny
// path in progress finishes its current call and leaves the runner to
// finalize.
//
// Precondition: finalize has begun (Scaler.BeginFinalize), as in M5-8,
// which then removes every row left. Without finalize, a row whose pending
// deny retry Stop cancelled stays exiting with its deny recorded, so its
// hook has killed the Worker or every runner process and no step runs (R10),
// until the next start's R16; a row a deny path itself stopped on is
// revoked and abandoned for reconciliation (denial.leave).
func (j *JobJudge) Stop() {
	j.mu.Lock()
	j.stopped = true
	retries := j.retries
	j.mu.Unlock()
	j.cancel()
	j.busy.Stop()
	if retries != nil {
		retries.stop()
	}
}

// Wait waits until every timer callback, Record, deny path and pending deny
// retry has ended, or ctx ends. Before Stop a pending timer or retry can keep
// it waiting for minutes.
func (j *JobJudge) Wait(ctx context.Context) error {
	// A timer callback may schedule a deny retry, so the timers go first.
	if err := j.busy.Wait(ctx); err != nil {
		return err
	}
	return j.work.wait(ctx)
}

// begin counts one unit of work unless Stop was called.
func (j *JobJudge) begin() bool {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.stopped {
		return false
	}
	j.work.begin()
	return true
}

// halted reports whether Stop was called or finalize has begun; the judge
// then records nothing and begins no Docker or GitHub call.
func (j *JobJudge) halted() bool {
	return j.ctx.Err() != nil || j.s.table.Finalizing()
}

// allow records the allow verdict and stops the runner's job check timer,
// and returns the runner's verdict afterwards. Table.Allow refuses when the
// runner's removal began meanwhile, as when the timer fired first.
func (j *JobJudge) allow(log *slog.Logger, name string) Verdict {
	if !j.s.table.Allow(name) {
		info, _ := j.s.table.Get(name)
		log.Info("job passed the check but the allow was not recorded",
			"state", info.State, "verdict", info.Verdict)
		return info.Verdict
	}
	j.busy.cancel(name)
	log.Info("job allowed")
	return VerdictAllow
}

// deny begins the deny path for runner name if begin, a BeginExit variant,
// wins the runner's exit. It records the deny before it returns and does
// not wait for the runner container. It returns the runner's verdict
// afterwards.
func (j *JobJudge) deny(log *slog.Logger, name, reason string, begin func(string) bool) Verdict {
	if j.halted() {
		log.Info("judge stopped or finalize began; runner left to finalize", "reason", reason)
		return j.verdict(name)
	}
	if !begin(name) {
		log.Debug("runner removal already begun or job allowed; deny path not started", "reason", reason)
		return j.verdict(name)
	}
	return j.removeDenied(log, name, reason)
}

// verdict returns runner name's recorded verdict, pending when it is not in
// the table.
func (j *JobJudge) verdict(name string) Verdict {
	info, _ := j.s.table.Get(name)
	return info.Verdict
}

// removeDenied runs the deny path for runner name, whose exit the caller
// won. It records the deny at once, so the hook kills the job's Worker,
// and leaves the wait for the container and its removal to a new goroutine.
// When the verdict was not pending the hook cannot see the deny, so the
// container is removed without waiting for it to stop. It returns the
// verdict it left recorded. log carries the runner name and, for a denied
// event report, the event name and head repository (0002 SPEC R130).
func (j *JobJudge) removeDenied(log *slog.Logger, name, reason string) Verdict {
	j.busy.cancel(name)
	info, _ := j.s.table.Get(name)
	d := &denial{j: j, name: name, id: info.ID, log: log}
	if j.s.table.Deny(name) {
		log.Info("job denied; waiting for the runner to stop", "reason", reason)
		d.awaitStop()
		return VerdictDeny
	}
	info, _ = j.s.table.Get(name)
	log.Info("job denied but the verdict was not pending, deny not recorded; removing the runner container",
		"reason", reason, "verdict", info.Verdict)
	d.start(d.attempt)
	return info.Verdict
}

// denial is one runner's deny path after its owner won the runner's exit.
type denial struct {
	j    *JobJudge
	name string
	// id is the GitHub runner ID; zero when none is known.
	id  int64
	log *slog.Logger
	// errs logs and records each new container removal error once.
	errs    errorOnce
	retries int
}

// start runs f in a new goroutine that the judge's work counts, so Wait
// waits for it. Once Stop was called it runs nothing and leaves the runner
// to finalize.
func (d *denial) start(f func()) {
	j := d.j
	if !j.begin() {
		d.leave("judge stopped")
		return
	}
	go func() {
		defer j.work.end()
		f()
	}()
}

// awaitStop waits, in a new goroutine, for the runner container to stop
// after the hook killed its Worker, at most denyStopGrace on the Clock,
// then removes it, unless the judge stopped or finalize began. Stop ends
// the wait at once, since the wait's context derives from the judge's;
// finalize without Stop is seen when the wait ends.
func (d *denial) awaitStop() {
	d.start(func() {
		j := d.j
		began := j.s.clock.Now()
		ctx, cancel := context.WithCancel(j.ctx)
		defer cancel()
		grace := j.s.clock.AfterFunc(denyStopGrace, cancel)
		err := d.waitStopped(ctx)
		grace.Stop()
		if j.halted() {
			d.leave("judge stopped or finalize began")
			return
		}
		switch {
		case err == nil:
			d.log.Info("denied runner's container stopped; removing it", "elapsed", j.s.clock.Now().Sub(began))
		case isNotFound(err):
			d.log.Info("denied runner's container gone; removing the rest", "elapsed", j.s.clock.Now().Sub(began))
		default:
			// Only the grace ends ctx without halting the judge.
			d.log.Info("runner did not stop within grace; removing its container", "grace", denyStopGrace)
		}
		d.attempt()
	})
}

// waitStopped waits until the runner container stops or ctx ends. It
// returns nil once the container stopped, the not-found error when the
// container is gone, and otherwise ctx's error, or the last wait error
// once the judge halted. A wait that fails another way is tried again
// every denyRetryInterval on the Clock, logging each new error once, so
// only a stop, a gone container, the grace, Stop or finalize ends it.
func (d *denial) waitStopped(ctx context.Context) error {
	s := d.j.s
	last := ""
	for {
		_, err := s.waiter.Wait(ctx, d.name)
		if err == nil || isNotFound(err) || ctx.Err() != nil {
			return err
		}
		err = withOp(retry.OpContainerWait, err)
		if msg := err.Error(); msg != last {
			last = msg
			d.log.Warn("waiting for the denied runner's container failed; retrying every 2s",
				"class", retry.Classify(context.Background(), retry.OpContainerWait, err), "err", err)
		}
		retryAt := make(chan struct{})
		t := s.clock.AfterFunc(denyRetryInterval, func() { close(retryAt) })
		select {
		case <-retryAt:
		case <-ctx.Done():
			t.Stop()
			return ctx.Err()
		}
		// Finalize without Stop leaves ctx alive; no more Docker calls.
		if d.j.halted() {
			return err
		}
	}
}

// attempt removes the runner container once. On success it goes on; on
// failure it tries again in 2 seconds.
func (d *denial) attempt() {
	j, s := d.j, d.j.s
	if j.halted() {
		d.leave("judge stopped or finalize began")
		return
	}
	ctx, cancel, ok := s.cleanupContext(context.Background(), removeContainerTimeout)
	if !ok {
		d.leave("finalize budget spent")
		return
	}
	err := s.containers.RemoveContainer(ctx, d.name)
	cancel()
	if err != nil {
		err = withOp(retry.OpContainerRemove, err)
		if class := retry.Classify(context.Background(), retry.OpContainerRemove, err); class != retry.Gone {
			d.failed(class, err)
			return
		}
	}
	d.removed()
}

// failed handles a container removal that was not confirmed: the token
// stays valid, so the hook still reads the deny and no step runs (R10), and
// the owner tries again whatever the error. Neither watch nor reconcile
// takes the runner over, since it is exiting.
func (d *denial) failed(class retry.Class, err error) {
	d.retries++
	d.errs.report(d.log, d.j.s.errs, "deny remove runner container",
		"removing the denied runner's container failed; retrying", class, err)
	if !d.j.retries.after(denyRetryInterval, d.attempt) {
		d.leave("judge stopped")
	}
}

// removed revokes the token once the runner container is gone and removes
// the rest of the runner (R11, R38).
func (d *denial) removed() {
	j, s := d.j, d.j.s
	if d.retries > 0 {
		d.errs.reset(d.log, "denied runner's container removed after retries")
	}
	d.log.Info("denied runner's container removed")
	s.table.RevokeToken(d.name)
	if j.halted() {
		d.leave("judge stopped or finalize began")
		return
	}
	s.retire(d.log, d.name, d.id, unregDenied)
}

// leave stops the deny path and leaves the exiting row to finalize, which
// removes every row left (M5-8). It revokes the token first: a hook that
// has not read the deny yet then gets 401 and keeps waiting (R66), and the
// row meets Scaler.retire's precondition, so it is also safe when Stop came
// without finalize and reconciliation claims the row (ClaimAbandoned).
func (d *denial) leave(why string) {
	s := d.j.s
	s.table.RevokeToken(d.name)
	s.table.AbandonExit(d.name, AbandonFinalize)
	d.log.Info("deny path stopped; runner left to finalize", "why", why)
}
