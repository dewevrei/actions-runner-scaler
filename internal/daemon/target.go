package daemon

import (
	"context"
	"errors"
	"log/slog"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/dewevrei/actions-runner-scaler/internal/config"
	"github.com/dewevrei/actions-runner-scaler/internal/github"
	"github.com/dewevrei/actions-runner-scaler/internal/retry"
	"github.com/dewevrei/actions-runner-scaler/internal/scaler"
	"github.com/dewevrei/actions-runner-scaler/internal/status"
)

// targetState is one target's state (0002 SPEC R180).
type targetState int

const (
	// targetStarting: the start phase runs.
	targetStarting targetState = iota
	// targetRunning: the target made its session and the listener runs.
	targetRunning
	// targetErrorDrain: a permanent error of its session, scale set or
	// AcquireJobs call while running (0002 SPEC R171); its runners drain
	// (R173).
	targetErrorDrain
	// targetErrorWait: a start-phase call failed permanently (0002 SPEC
	// R172), or the target error drain ended (R173); the target has no
	// session and no runner, and retries its start phase (R174).
	targetErrorWait
	// targetStopped: a signal, a drain or finalize stopped the start phase
	// before it made a session (0002 SPEC R196); the target has no session
	// and no runner. It is not one of R180's four states: it exists while
	// the server drains or finalizes, before the first session (R57) or on
	// the way to the server error wait (server state starting), and briefly
	// before the server state becomes draining; status maps it by the
	// server state (statusState, 0002 PLAN M15).
	targetStopped
)

func (s targetState) String() string {
	switch s {
	case targetStarting:
		return "starting"
	case targetRunning:
		return "running"
	case targetErrorDrain:
		return "error-drain"
	case targetErrorWait:
		return "error-wait"
	case targetStopped:
		return "stopped"
	default:
		return "targetState(" + strconv.Itoa(int(s)) + ")"
	}
}

// statusState maps s to a state of R180's four, given the server's state
// (0002 PLAN M15). A stopped target is shown as running (R180 has no word
// for it, it had no error, and a target that runs takes no new job during
// the drain or finalize either) when the server state is running or
// draining, so R197's `정상 target 없음` never reads it. While the server
// state is starting (finalize before the first session, R57, or on the way
// to the server error wait) it is shown as starting, which R58 and R198 use
// for a scaler that has not started.
func (s targetState) statusState(server status.State) status.TargetState {
	switch s {
	case targetStarting:
		return status.TargetStarting
	case targetErrorDrain:
		return status.TargetErrorDraining
	case targetErrorWait:
		return status.TargetErrorWait
	case targetStopped:
		if server == status.Starting {
			return status.TargetStarting
		}
		return status.TargetRunning
	default: // targetRunning
		return status.TargetRunning
	}
}

// errNoClients is what the target's scale set adapters return before the
// start phase made the target's clients. No row of the target exists then,
// so nothing calls them.
var errNoClients = errors.New("daemon: the target's start phase has not made its GitHub clients")

// targetRun is one target's run-time objects and state (0002 PLAN M12-1).
// The scaler, judge and limits are made before any target's GitHub call;
// the GitHub clients, the scale set and the session come from the target's
// start phase, which runs in the target's own goroutine.
type targetRun struct {
	d   *Daemon
	cfg config.Target
	t   github.Target
	log *slog.Logger
	// auth is the target's credential (0003 SPEC R273); start-up sets it
	// before the target's goroutine starts, and it never changes.
	auth targetAuth
	// limits holds the R52 waits of the target's Actions service calls
	// (0002 SPEC R265). The credential check uses the waits auth names
	// (0002 SPEC R189, 0003 SPEC R280), and the REST calls the waits of the
	// target's credential (inst, R266, 0003 SPEC R280); rateLimits joins
	// these two.
	limits *retry.NextAllowed
	// inst points to the REST waits of the credential whose client gh is,
	// nil before the first bind (0002 SPEC R266, 0003 SPEC R280). bind
	// stores it with gh, so the REST client and its waits change together;
	// the scaler and the reconciler load it on each call.
	inst atomic.Pointer[retry.NextAllowed]
	// rec holds the target's last error; record also writes the server's.
	rec    *status.Recorder
	scaler *scaler.Scaler
	judge  *scaler.JobJudge
	// loops holds the target's session loop goroutines; run waits for them.
	loops sync.WaitGroup

	mu sync.Mutex
	// gh and scaleSetID are set by the start phase (bind); gh is nil before.
	gh         TargetGitHub
	scaleSetID int
	state      targetState
	// session is the open message session, nil once closed.
	session Session
	// fatalCh is closed when the target error drain begins (fatal); each
	// time the target starts running it gets a new one.
	fatalCh chan struct{}
	// nextRetryAt is the next retry's time in the target error wait (0002
	// SPEC R174), zero outside it; kept for status (0002 PLAN M15).
	nextRetryAt time.Time
	// loggedOnly is the last permanent error fatal only logged during a
	// server drain, so a repeat is not logged again (0002 SPEC R191).
	loggedOnly string
	// drainRecorded is set once the error that started the current target
	// error drain was recorded, so a repeat only logs at debug level.
	drainRecorded bool
}

// newTargetRun returns the target's run in state starting, without its
// scaler and judge.
func newTargetRun(d *Daemon, cfg config.Target, t github.Target) (*targetRun, error) {
	limits, err := retry.NewNextAllowed(d.clock)
	if err != nil {
		return nil, err
	}
	log := d.log.With("target", t.Key())
	rec, err := status.NewRecorder(d.cfg.Secrets, d.clock, log)
	if err != nil {
		return nil, err
	}
	return &targetRun{d: d, cfg: cfg, t: t, log: log, limits: limits, rec: rec}, nil
}

// Record records err as the target's last error and the server's (0002
// PLAN M12-4). It is the target Scaler's scaler.ErrorRecorder.
func (tr *targetRun) Record(where string, err error) {
	tr.rec.Record(where, err)
	tr.d.rec.Record(where, err)
}

// rateLimits returns the target's scaler.RateLimits: the REST calls wait
// per credential (0002 SPEC R266, 0003 SPEC R280), the other calls per
// target (R265).
func (tr *targetRun) rateLimits() scaler.RateLimits {
	return targetLimits{own: tr.limits, inst: &tr.inst}
}

// scope is where the target's start-phase calls wait and report.
func (tr *targetRun) scope() callScope {
	return callScope{limits: tr.limits, log: tr.log, record: tr.Record, fail: tr.fail}
}

// fail records a permanent start-phase error and returns the waitError
// that puts only this target in the target error wait (0002 SPEC R172).
func (tr *targetRun) fail(where string, err error) error {
	tr.Record(where, err)
	tr.log.Error("target start-up failed permanently; target error wait (0002 SPEC R172)", "step", where, "err", logged(err))
	return &waitError{where: where, err: err}
}

func (tr *targetRun) setState(s targetState) {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	tr.state = s
}

// setRunning puts the target in state running with a new fatalCh.
func (tr *targetRun) setRunning() {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	tr.state = targetRunning
	tr.fatalCh = make(chan struct{})
	tr.nextRetryAt = time.Time{}
	tr.loggedOnly = ""
}

// retryAt returns the next retry's time in the target error wait, zero
// outside it (0002 SPEC R174).
func (tr *targetRun) retryAt() time.Time {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	return tr.nextRetryAt
}

func (tr *targetRun) currentState() targetState {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	return tr.state
}

var _ status.TargetView = (*targetRun)(nil)

// TargetStatus is the target's line for status (0002 SPEC R152, R180, R205,
// R222; 0002 PLAN M15). Busy and idle follow the 0001 counting rules:
// runners whose removal began are left out. The table and the placer are
// read after tr.mu is released, so no lock of theirs nests inside it.
func (tr *targetRun) TargetStatus() status.Target {
	tr.mu.Lock()
	state, retryAt := tr.state, tr.nextRetryAt
	tr.mu.Unlock()

	key := tr.t.Key()
	c := tr.d.table.Counts(key)
	ss := tr.d.table.StartStatus(key)
	out := status.Target{
		URL:              tr.cfg.URL,
		State:            state.statusState(tr.d.st.State()),
		Busy:             c.Busy + c.Done,
		Idle:             c.Starting + c.Idle,
		WaitingJobs:      tr.d.placer.WaitingJobs(key),
		MaxRunners:       tr.cfg.MaxRunners,
		StartFailures:    ss.Failures,
		LastStartErrorAt: ss.LastErrorAt,
	}
	if ss.LastError != nil {
		out.LastStartError = ss.LastError.Error()
	}
	out.LastError, out.LastErrorAt = tr.rec.Last()
	if state == targetErrorWait && !retryAt.IsZero() {
		out.NextRetryAt = &retryAt
	}
	return out
}

// bind sets the clients and the scale set the start phase made, and rest,
// the REST waits of the clients' credential (0002 SPEC R266, 0003 SPEC
// R280).
func (tr *targetRun) bind(gh TargetGitHub, scaleSetID int, rest *retry.NextAllowed) {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	tr.gh, tr.scaleSetID = gh, scaleSetID
	tr.inst.Store(rest)
}

// github returns the target's clients and scale set ID; gh is nil before
// bind.
func (tr *targetRun) github() (gh TargetGitHub, scaleSetID int) {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	return tr.gh, tr.scaleSetID
}

func (tr *targetRun) setSession(s Session) {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	tr.session = s
}

func (tr *targetRun) currentSession() Session {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	return tr.session
}

// closeSession closes the target's open session, if any, with a timeout on
// a context without cancellation; errors are logged only (R99).
func (tr *targetRun) closeSession(log *slog.Logger, timeout time.Duration) {
	tr.mu.Lock()
	s := tr.session
	tr.session = nil
	tr.mu.Unlock()
	if s == nil {
		return
	}
	ctx, cancel := withTimeout(context.Background(), tr.d.clock, timeout)
	defer cancel()
	if err := s.Close(ctx); err != nil {
		log.Warn("closing the message session failed (R99)", "err", err)
		return
	}
	log.Info("message session closed")
}

// run is the target's goroutine: the start phase, then the session loop
// (0002 PLAN M12-3, M12-6), and after a permanent error the target error
// drain and the target error wait, whose retry that makes a session runs
// the session loop again (0002 PLAN M13). ctx ends the start phase only. It
// returns once every session loop it started returned.
func (tr *targetRun) run(ctx context.Context, label string) {
	defer tr.loops.Wait()
	running := tr.startPhase(ctx, label)
	if !running && tr.currentState() != targetErrorWait {
		return
	}
	for {
		if running && !tr.serve() {
			return
		}
		if running = tr.errorWait(label); !running {
			return
		}
	}
}

// serve runs the session loop on the target's session in its own goroutine
// until the loop ends or the target error drain begins (fatal). It reports
// whether that drain ran to its end, so the target is in the target error
// wait now; false means a drain or finalize ended the loop or stopped the
// target's drain, and the loop goes on until finalize ends it.
func (tr *targetRun) serve() bool {
	// The listener's context is a child of the daemon's, which finalize
	// cancels; the end cleanup of the target error drain cancels this one.
	ctx, cancel := context.WithCancel(tr.d.listenCtx)
	tr.mu.Lock()
	fatalCh := tr.fatalCh
	tr.mu.Unlock()
	done := make(chan struct{})
	tr.loops.Go(func() {
		defer close(done)
		tr.sessionLoop(ctx)
	})
	select {
	case <-fatalCh:
	case <-done:
		if !tr.drainAfterLoop() {
			cancel()
			return false
		}
	}
	return tr.errorDrain(cancel, done)
}

// drainAfterLoop reports, once the session loop ended, whether the target
// error drain began. A placement sets the target's drain mark before it
// calls fatal (0002 PLAN M14-3.4), and the loop, seeing the mark, may end in
// between; while the target still runs and no server drain began, the drain
// begins here, so the target is not left in error-drain with nobody running
// the drain and its retries. fatal then only records the error.
func (tr *targetRun) drainAfterLoop() bool {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	if tr.state == targetErrorDrain {
		return true
	}
	if tr.d.draining() || !tr.d.table.TargetDraining(tr.t.Key()) {
		return false
	}
	return tr.beginErrorDrainLocked()
}

// startPhase runs start and sets the state it ends in. It reports whether
// the session loop should run. Whatever the outcome, it tells the daemon
// that the start phase ended.
func (tr *targetRun) startPhase(ctx context.Context, label string) bool {
	d := tr.d
	defer func() { d.startsEnded <- struct{}{} }()
	err := tr.start(ctx, label, tr.scope(), d.retryIn)
	var we *waitError
	switch {
	case err == nil && ctx.Err() == nil && !d.draining():
		// A signal drain may begin between this check and the listener's
		// start; the listener then runs on the session during the drain,
		// as the other targets' do, which R54 and R92 allow.
		tr.setRunning()
		d.placer.SetActive(tr.t.Key(), true) // its messages now scale it
		tr.log.Info("first message session made")
		d.firstSessionMade()
		return true
	case err == nil:
		// A drain or finalize began while the session was being made: no
		// listener runs on it (R88, 0002 SPEC R196); finalize closes it.
		tr.log.Info("session made as the start phase was stopped; finalize closes it")
		tr.setState(targetStopped)
		return false
	case errors.As(err, &we):
		tr.setState(targetErrorWait)
		return false
	case ctx.Err() != nil:
		// A signal, a drain or finalize stopped the start phase (R57, 0002
		// SPEC R196).
		tr.log.Info("start phase stopped", "err", err)
		tr.setState(targetStopped)
		return false
	default:
		_ = tr.fail("start-up", err)
		tr.setState(targetErrorWait)
		return false
	}
}

// stepCaller makes one call of the start phase in scope sc: Daemon.retryIn
// on the first start (R24), targetRun.callOnce in a retry of the target
// error wait (0002 SPEC R193).
type stepCaller func(ctx context.Context, sc callScope, where string, op retry.Op, fn func(context.Context) error) error

// start is the target's start phase (0002 SPEC Design 시작 순서, R193):
// the credential check, which is the installation lookup and its
// permission for an App target (R236~R238) and R279's call for a token
// target (0003 SPEC R278, R279); the clients for that credential, whose
// REST calls wait with the other targets of the credential (R266, 0003
// SPEC R280); the registration cleanup (R241) and the scale set (R239),
// both in the default runner group; its system information; the session.
// call makes each call in scope sc. On the first start, transient errors
// are retried with R24's backoff, so they delay no other target (R177),
// and a permanent error returns a waitError for this target only (R172);
// in a retry every error ends the retry (R193).
func (tr *targetRun) start(ctx context.Context, label string, sc callScope, call stepCaller) error {
	d := tr.d

	key, err := tr.auth.check(ctx, call, sc, d.authWaits())
	if err != nil {
		return err
	}
	// The target moves to the credential's REST waits at bind, so a retry
	// that got another installation and fails before it leaves the old
	// client on the old waits (0002 SPEC R193, R266).
	rest := d.restWaits.get(key)
	gh, err := tr.auth.clients(key)
	if err != nil {
		return sc.fail("github clients", err)
	}

	// Every repository scale set is in the default runner group (0002 SPEC
	// R239).
	const groupID = github.DefaultRunnerGroupID
	regCleanup, err := scaler.NewRegistrationCleanup(scaler.RegistrationCleanupConfig{
		ScaleSets: gh, Runners: gh, Registry: gh, Resources: d.docker,
		Limits: pinnedLimits(tr.limits, rest), Clock: d.clock, Logger: tr.log,
	})
	if err != nil {
		return err
	}
	if err := regCleanup.Run(ctx, groupID, label); err != nil {
		return err
	}
	var setID int
	err = call(ctx, sc, "scale set", retry.OpGetOrCreateScaleSet, func(ctx context.Context) error {
		var err error
		setID, err = gh.GetOrCreateScaleSet(ctx, groupID, label)
		return err
	})
	if err != nil {
		return err
	}
	// No request in scaleset v0.5.0.
	info := d.cfg.SystemInfo
	info.ScaleSetID = setID
	gh.SetSystemInfo(info)
	tr.bind(gh, setID, rest)

	var sess Session
	err = call(ctx, sc, "message session", retry.OpSessionCreate, func(ctx context.Context) error {
		var err error
		sess, err = gh.CreateSession(ctx, setID, d.cfg.Owner)
		return err
	})
	if err != nil {
		return err
	}
	tr.setSession(sess)
	return nil
}

// targetScaleSet is the target Scaler's scaler.ScaleSetAPI on the target's
// scale set, bound once the start phase made it.
type targetScaleSet struct{ tr *targetRun }

var _ scaler.ScaleSetAPI = targetScaleSet{}

func (s targetScaleSet) GenerateJITConfig(ctx context.Context, name string) (scaler.JITRunner, error) {
	b, ok := s.bound()
	if !ok {
		return scaler.JITRunner{}, errNoClients
	}
	return b.GenerateJITConfig(ctx, name)
}

func (s targetScaleSet) RemoveRunner(ctx context.Context, id int64) error {
	b, ok := s.bound()
	if !ok {
		return errNoClients
	}
	return b.RemoveRunner(ctx, id)
}

func (s targetScaleSet) bound() (boundScaleSet, bool) {
	gh, id := s.tr.github()
	return boundScaleSet{gh: gh, id: id}, gh != nil
}

// targetFinder is the reconciler's scaler.RunnerFinder for the target
// (R242), bound like targetScaleSet.
type targetFinder struct{ tr *targetRun }

var _ scaler.RunnerFinder = targetFinder{}

func (f targetFinder) FindRunner(ctx context.Context, name string) (scaler.ListedRunner, bool, error) {
	gh, _ := f.tr.github()
	if gh == nil {
		return scaler.ListedRunner{}, false, errNoClients
	}
	return gh.FindRunner(ctx, name)
}
