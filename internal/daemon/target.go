package daemon

import (
	"context"
	"errors"
	"log/slog"
	"strconv"
	"sync"
	"time"

	"github.com/dewevrei/actions-runner-scaler/internal/config"
	"github.com/dewevrei/actions-runner-scaler/internal/github"
	"github.com/dewevrei/actions-runner-scaler/internal/retry"
	"github.com/dewevrei/actions-runner-scaler/internal/scaler"
	"github.com/dewevrei/actions-runner-scaler/internal/status"
)

// targetState is one target's state (0002 SPEC R180). 0002 PLAN M13 adds
// the target error drain.
type targetState int

const (
	// targetStarting: the start phase runs.
	targetStarting targetState = iota
	// targetRunning: the first session was made and the listener runs.
	targetRunning
	// targetErrorWait: a start-phase call failed permanently (0002 SPEC
	// R172); the target has no session and no runner.
	targetErrorWait
)

func (s targetState) String() string {
	switch s {
	case targetStarting:
		return "starting"
	case targetRunning:
		return "running"
	case targetErrorWait:
		return "error-wait"
	default:
		return "targetState(" + strconv.Itoa(int(s)) + ")"
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
	// limits holds the R52 waits of the calls made with this target's
	// installation token (0002 SPEC R188). The installation lookup uses the
	// daemon's shared limits instead (R189).
	limits *retry.NextAllowed
	// rec holds the target's last error; record also writes the server's.
	rec    *status.Recorder
	scaler *scaler.Scaler
	judge  *scaler.JobJudge
	// listenCtx is the target's listener context, a child of the daemon's,
	// which finalize cancels.
	listenCtx    context.Context
	listenCancel context.CancelFunc

	mu sync.Mutex
	// gh and scaleSetID are set by the start phase (bind); gh is nil before.
	gh         TargetGitHub
	scaleSetID int
	state      targetState
	// session is the open message session, nil once closed.
	session Session
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
	tr := &targetRun{d: d, cfg: cfg, t: t, log: log, limits: limits, rec: rec}
	tr.listenCtx, tr.listenCancel = context.WithCancel(d.listenCtx)
	return tr, nil
}

// Record records err as the target's last error and the server's (0002
// PLAN M12-4). It is the target Scaler's scaler.ErrorRecorder.
func (tr *targetRun) Record(where string, err error) {
	tr.rec.Record(where, err)
	tr.d.rec.Record(where, err)
}

// scope is where the target's start-phase calls wait and report.
func (tr *targetRun) scope() callScope {
	return callScope{limits: tr.limits, log: tr.log, record: tr.Record, fail: tr.fail}
}

// fail records a permanent start-phase error and returns the waitError
// that puts only this target in the target error wait (0002 SPEC R172).
func (tr *targetRun) fail(where string, err error) error {
	tr.Record(where, err)
	tr.log.Error("target start-up failed permanently; target error wait (0002 SPEC R172)", "step", where, "err", err)
	return &waitError{where: where, err: err}
}

func (tr *targetRun) setState(s targetState) {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	tr.state = s
}

func (tr *targetRun) currentState() targetState {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	return tr.state
}

// bind sets the clients and the scale set the start phase made.
func (tr *targetRun) bind(gh TargetGitHub, scaleSetID int) {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	tr.gh, tr.scaleSetID = gh, scaleSetID
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
// (0002 PLAN M12-3, M12-6). ctx ends the start phase only.
func (tr *targetRun) run(ctx context.Context, app GitHub, label string) {
	if tr.startPhase(ctx, app, label) {
		tr.sessionLoop()
	}
}

// startPhase runs start and sets the state it ends in. It reports whether
// the session loop should run. Whatever the outcome, it tells the daemon
// that the start phase ended.
func (tr *targetRun) startPhase(ctx context.Context, app GitHub, label string) bool {
	d := tr.d
	defer func() { d.startsEnded <- struct{}{} }()
	err := tr.start(ctx, app, label)
	var we *waitError
	switch {
	case err == nil && ctx.Err() == nil && !d.draining():
		tr.setState(targetRunning)
		tr.log.Info("first message session made")
		d.firstSessionMade()
		return true
	case err == nil:
		// A drain or finalize began while the session was being made: no
		// listener runs on it (R88, 0002 SPEC R196); finalize closes it.
		tr.log.Info("session made as the start phase was stopped; finalize closes it")
		return false
	case errors.As(err, &we):
		tr.setState(targetErrorWait)
		return false
	case ctx.Err() != nil:
		// A signal, a drain or finalize stopped the start phase (R57, 0002
		// SPEC R196).
		tr.log.Info("start phase stopped", "err", err)
		return false
	default:
		_ = tr.fail("start-up", err)
		tr.setState(targetErrorWait)
		return false
	}
}

// start is the target's start phase (0002 SPEC Design 시작 순서): the
// installation (R108~R110), the runner group (R111), R148, the scale set
// (R112), its system information, the first session. Transient errors are
// retried here with R24's backoff, so they delay no other target (R177); a
// permanent error returns a waitError for this target only (R172).
func (tr *targetRun) start(ctx context.Context, app GitHub, label string) error {
	d := tr.d
	sc := tr.scope()

	// The installation lookup uses the App JWT and the one wait all
	// targets share (0002 SPEC R189).
	appScope := sc
	appScope.limits = d.limits
	var inst github.Installation
	err := d.retryIn(ctx, appScope, "installation", retry.OpGetInstallation, func(ctx context.Context) error {
		var err error
		inst, err = app.Installation(ctx, tr.t)
		return err
	})
	if err != nil {
		return err
	}
	if err := github.CheckPermission(inst, tr.t); err != nil {
		return sc.fail("installation", err)
	}
	gh, err := app.Target(tr.t, inst.ID)
	if err != nil {
		return sc.fail("github clients", err)
	}

	var groupID int
	err = d.retryIn(ctx, sc, "runner group", retry.OpGetRunnerGroup, func(ctx context.Context) error {
		var err error
		groupID, err = gh.RunnerGroupID(ctx, tr.cfg.RunnerGroup)
		return err
	})
	if err != nil {
		return err
	}

	regCleanup, err := scaler.NewRegistrationCleanup(scaler.RegistrationCleanupConfig{
		ScaleSets: gh, Runners: gh, Registry: gh, Resources: d.docker,
		Limits: tr.limits, Clock: d.clock, Logger: tr.log,
	})
	if err != nil {
		return err
	}
	if err := regCleanup.Run(ctx, groupID, label); err != nil {
		return err
	}
	var setID int
	err = d.retryIn(ctx, sc, "scale set", retry.OpGetOrCreateScaleSet, func(ctx context.Context) error {
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
	tr.bind(gh, setID)

	var sess Session
	err = d.retryIn(ctx, sc, "message session", retry.OpSessionCreate, func(ctx context.Context) error {
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
// (R149), bound like targetScaleSet.
type targetFinder struct{ tr *targetRun }

var _ scaler.RunnerFinder = targetFinder{}

func (f targetFinder) FindRunner(ctx context.Context, name string) (scaler.ListedRunner, bool, error) {
	gh, _ := f.tr.github()
	if gh == nil {
		return scaler.ListedRunner{}, false, errNoClients
	}
	return gh.FindRunner(ctx, name)
}
