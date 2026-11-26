// Package daemon runs the scaler service: the start order, the message
// session loops, the signal and error drains, the staged finalize and the
// error wait (PLAN M5). Each target has its own goroutine for its start
// phase and session loop, and all targets share one runner table,
// reconciler, job check server and status (0002 PLAN M12). It wires the
// config, docker, github, gate, scaler and status packages together through
// the adapters in adapters.go; every outside dependency comes in through
// Config, so tests run it on fakes and a fake clock.
package daemon

import (
	"context"
	"crypto/rsa"
	"errors"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/actions/scaleset"

	"github.com/dewevrei/actions-runner-scaler/internal/config"
	"github.com/dewevrei/actions-runner-scaler/internal/docker"
	"github.com/dewevrei/actions-runner-scaler/internal/gate"
	"github.com/dewevrei/actions-runner-scaler/internal/logging"
	"github.com/dewevrei/actions-runner-scaler/internal/retry"
	"github.com/dewevrei/actions-runner-scaler/internal/scaler"
	"github.com/dewevrei/actions-runner-scaler/internal/status"
)

// Finalize budget (PLAN M5-8). Each stage gets its own share, so a stage
// that runs long never takes a later stage's share: 15 + 10 + 20 + 8 + 2 =
// 55 s at most, 5 s within launchd's ExitTimeOut of drain-timeout + 60 s.
const (
	// finalizeRunShare is stages 1-3, up to deadline A: the scaler's
	// cleanup calls and the listener's return.
	finalizeRunShare = 15 * time.Second
	// finalizeWaitShare is stage 4, after A: work in flight, the docker
	// undo of a start path among it.
	finalizeWaitShare = 10 * time.Second
	// finalizeCleanupShare is stage 5, removing the rows left; each of its
	// calls gets finalizeCallShare.
	finalizeCleanupShare = 20 * time.Second
	finalizeCallShare    = 10 * time.Second
	// finalizeCloseShare is stage 6, closing the session (R99).
	finalizeCloseShare = 8 * time.Second
	// finalizeShutdownShare is stage 7, the job check server's Shutdown.
	finalizeShutdownShare = 2 * time.Second
)

// Other bounds.
const (
	// sessionCloseTimeout bounds closing a session that ended outside
	// finalize (PLAN M5-5: WithoutCancel + 10 s).
	sessionCloseTimeout = 10 * time.Second
	// callTimeout bounds one start-up call. Not from SPEC: the bound the
	// scaler gives one GitHub call, used for the Docker calls too.
	callTimeout = 30 * time.Second
	// exposureTimeout bounds the R79 check. Not from SPEC: a local TCP
	// connection answers at once, and ExposureUnknown carries on anyway.
	exposureTimeout = 5 * time.Second
	// statusShutdownTimeout bounds stopping the status socket when no
	// finalize for exit stopped it: after the error wait, a lock-held or
	// unexpected exit. On exit after a drain, finalize's stage 7 stops it
	// together with the job check server within finalizeShutdownShare, so
	// finalize stays within 55 s. The socket answers each connection with
	// one short line.
	statusShutdownTimeout = 2 * time.Second
)

// StatusServer is the status socket (*status.Server).
type StatusServer interface {
	Serve() error
	Shutdown(ctx context.Context) error
}

// JobServer is the job check server (*gate.Server).
type JobServer interface {
	Listen() error
	Serve() error
	Shutdown(ctx context.Context) error
}

var (
	_ StatusServer = (*status.Server)(nil)
	_ JobServer    = (*gate.Server)(nil)
)

// Config holds a Daemon's dependencies. All fields are required except
// those marked optional.
type Config struct {
	// Clock paces every wait, timer and budget of the daemon and the scaler.
	Clock scaler.Clock
	// Signals receives SIGTERM and SIGINT (signal.Notify).
	Signals <-chan os.Signal
	// Logger should redact Secrets (logging.RedactingHandler).
	Logger *slog.Logger
	// Secrets is the process's one secret set (R31).
	Secrets *logging.Secrets

	// Lock takes the single-instance lock (R78); unlock releases it.
	Lock func() (unlock func(), err error)
	// ListenStatus removes a stale socket and listens (R60).
	ListenStatus func(rep status.Reporter) (StatusServer, error)
	// LoadConfig reads and validates the configuration and parses the App
	// private key (R28, R30, 0002 R105~R107, R166). On error the daemon still runs R16 and
	// then enters the error wait.
	LoadConfig func() (*config.Config, *rsa.PrivateKey, error)
	// Docker is the Docker client; it needs no configuration.
	Docker Docker
	// CheckExposure is the R79 check (docker.CheckExposure).
	CheckExposure func(ctx context.Context) docker.Exposure
	// NewGitHub makes the GitHub side from the configuration (NewGitHub).
	NewGitHub func(cfg *config.Config, key *rsa.PrivateKey) (GitHub, error)
	// NewJobServer makes the job check server (gate.NewServer).
	NewJobServer func(addr string, auth gate.Authenticator) (JobServer, error)
	// Owner names the message session's owner, normally the host name.
	Owner string
	// SystemInfo is sent with GitHub requests; the daemon fills ScaleSetID.
	SystemInfo scaleset.SystemInfo

	// Jitter returns values in [0, 1) for the backoffs; optional, nil means
	// math/rand/v2.Float64.
	Jitter func() float64
	// Random is the source of runner name suffixes; optional, nil means
	// crypto/rand.
	Random io.Reader
}

// Daemon is one run of the scaler service. Use it once.
type Daemon struct {
	cfg   Config
	log   *slog.Logger
	clock scaler.Clock
	// limits is the R52 wait of the installation lookup, which every target
	// shares (0002 SPEC R189); each target has its own for the rest (R188).
	limits *retry.NextAllowed
	rec    *status.Recorder
	st     *status.Status
	sig    *signals

	// The shared objects (0002 SPEC Design 공유 자원); nil until start-up
	// built them.
	docker       dockerAdapter
	drainTimeout time.Duration
	table        *scaler.Table
	recon        *scaler.Reconciler
	server       JobServer
	// targets holds every target in config order (0002 PLAN M12-1); empty
	// until start-up built them.
	targets []*targetRun

	// startsEnded receives once per target whose start phase ended;
	// firstSession is closed by the first target that made its first
	// session (0002 SPEC R178). startCtx is every start phase's context;
	// a drain of either kind or finalize cancels it (R57, 0002 SPEC R196).
	startsEnded  chan struct{}
	firstSession chan struct{}
	firstOnce    sync.Once
	startCtx     context.Context
	cancelStarts context.CancelFunc

	// listenCtx is the parent of every target's listener context; finalize
	// cancels it (M5-2).
	listenCtx    context.Context
	listenCancel context.CancelFunc
	// deadlineA ends at finalize deadline A, ending a DeleteMessage in
	// flight (M5-5).
	deadlineA context.Context
	cancelA   context.CancelFunc
	// timerA is the Clock timer that ends deadlineA; nil until finalize
	// sets it. Run stops it when it returns.
	timerA scaler.Timer

	// drainStarted is closed once a drain of either kind begins, so the
	// session loops stop waiting to make a new session (R20, R88).
	drainStarted chan struct{}
	drainOnce    sync.Once
	// fatalCh is closed by the first fatal error that starts an error
	// drain (R51).
	fatalCh   chan struct{}
	fatalOnce sync.Once
	// runDone is closed when every target's goroutine returned; nil until
	// they start.
	runDone chan struct{}

	// statusSrv is the status socket; statusDown is set once it was shut
	// down.
	statusSrv  StatusServer
	statusMu   sync.Mutex
	statusDown bool

	finalized bool
}

// New returns a Daemon. It returns an error when a required dependency is
// missing.
func New(cfg Config) (*Daemon, error) {
	var missing []string
	for _, d := range []struct {
		name    string
		missing bool
	}{
		{"Clock", cfg.Clock == nil},
		{"Signals", cfg.Signals == nil},
		{"Logger", cfg.Logger == nil},
		{"Secrets", cfg.Secrets == nil},
		{"Lock", cfg.Lock == nil},
		{"ListenStatus", cfg.ListenStatus == nil},
		{"LoadConfig", cfg.LoadConfig == nil},
		{"Docker", cfg.Docker == nil},
		{"CheckExposure", cfg.CheckExposure == nil},
		{"NewGitHub", cfg.NewGitHub == nil},
		{"NewJobServer", cfg.NewJobServer == nil},
		{"Owner", cfg.Owner == ""},
	} {
		if d.missing {
			missing = append(missing, d.name)
		}
	}
	if len(missing) > 0 {
		return nil, errors.New("daemon: missing " + strings.Join(missing, ", "))
	}
	limits, err := retry.NewNextAllowed(cfg.Clock)
	if err != nil {
		return nil, err
	}
	d := &Daemon{
		cfg:          cfg,
		log:          cfg.Logger,
		clock:        cfg.Clock,
		limits:       limits,
		drainStarted: make(chan struct{}),
		fatalCh:      make(chan struct{}),
		firstSession: make(chan struct{}),
	}
	d.listenCtx, d.listenCancel = context.WithCancel(context.Background())
	d.startCtx, d.cancelStarts = context.WithCancel(d.listenCtx)
	d.deadlineA, d.cancelA = context.WithCancel(context.Background())
	return d, nil
}

// Run runs the service until it ends. It returns nil for every end caused
// by a signal (R84): before the first session (R57), at the end of a signal
// drain (R92), on a signal during an error drain (R21), and in the error
// wait (R48). It returns an error when the lock is held (R78), the status
// socket cannot listen (R60), or something unexpected fails (R85).
//
// ctx is for tests; ending it acts like a signal.
func (d *Daemon) Run(ctx context.Context) error {
	defer d.listenCancel()
	defer d.cancelA()
	defer func() {
		if d.timerA != nil {
			d.timerA.Stop()
		}
	}()
	unlock, err := d.cfg.Lock()
	if err != nil {
		d.log.Error("cannot take the scaler lock; is another scaler running? (R78)", "err", err)
		return err
	}
	defer unlock()

	rec, err := status.NewRecorder(d.cfg.Secrets, d.clock, d.log)
	if err != nil {
		d.log.Error("cannot make the status recorder", "err", err)
		return err
	}
	st, err := status.New(rec)
	if err != nil {
		d.log.Error("cannot make the status", "err", err)
		return err
	}
	d.rec, d.st = rec, st
	srv, err := d.cfg.ListenStatus(st)
	if err != nil {
		d.log.Error("cannot listen on the status socket (R60)", "err", err)
		return err
	}
	d.statusSrv = srv
	go func() {
		if err := srv.Serve(); err != nil {
			d.log.Error("status socket stopped", "err", err)
		}
	}()
	// On exit after finalize, stage 7 stopped the socket already.
	defer func() {
		ctx, cancel := withTimeout(context.Background(), d.clock, statusShutdownTimeout)
		defer cancel()
		d.shutdownStatus(ctx)
	}()

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	d.sig = watchSignals(runCtx, ctx, d.cfg.Signals, d.log)
	return d.run(runCtx)
}

// run is Run after the lock and the status socket.
func (d *Daemon) run(ctx context.Context) error {
	err := d.start(ctx)
	var we *waitError
	switch {
	case err == nil:
		return d.serve(ctx)
	case errors.As(err, &we):
		d.finalize(false)
		return d.errorWait(ctx)
	case ctx.Err() != nil || d.sig.received():
		d.log.Info("signal before the first session; exiting (R57)")
		d.finalize(true)
		return nil
	case d.fatalStarted():
		// A fatal error ended start-up before the first session: the error
		// wait, as for any permanent start-up error (R46).
		d.finalize(false)
		return d.errorWait(ctx)
	default:
		d.log.Error("start-up failed", "err", err)
		d.finalize(true)
		return err
	}
}

// serve runs after start-up: every target's session loop runs in its own
// goroutine already. It waits for a signal or a fatal error, then a drain,
// then finalize, and after an error drain the error wait.
func (d *Daemon) serve(ctx context.Context) error {
	d.st.SetState(status.Running)
	d.log.Info("running")

	select {
	case <-d.sig.first:
		// Targets still in their start phase stop and make no session
		// (0002 SPEC R196), before the drain mark, so none of them starts
		// a listener in between.
		d.startDrain()
		if !d.table.SetDraining(scaler.DrainSignal) {
			// The server's error drain (fatal) began just before the
			// signal: a signal during it finalizes at once (R21).
			d.log.Info("signal during the error drain; finalizing (R21)")
			d.finalize(true)
			return nil
		}
		d.st.SetState(status.Draining)
		d.log.Info("signal received; signal drain started (R20)")
		drainCtx, cancel := chanContext(ctx, d.sig.second)
		res := d.drainAll(drainCtx, scaler.DrainSignal)
		cancel()
		d.log.Info("signal drain ended; finalizing", "result", res)
		d.finalize(true)
		return nil
	case <-d.fatalCh:
		d.st.SetState(status.Draining)
		drainCtx, cancel := chanContext(ctx, d.sig.first)
		res := d.drainAll(drainCtx, scaler.DrainError)
		cancel()
		d.log.Info("error drain ended; finalizing", "result", res)
		interrupted := res == scaler.DrainInterrupted
		d.finalize(interrupted)
		if interrupted {
			d.log.Info("signal during the error drain; exiting (R21)")
			return nil
		}
		return d.errorWait(ctx)
	case <-ctx.Done():
		d.finalize(true)
		return nil
	}
}

// drainAll runs every target's drain in parallel, so each target's
// scale-down removes its own idle runners while completion counts the
// runners of all targets (0002 PLAN M11-5, M12-7). It returns the worst
// result: interrupted, then timed out, then completed.
func (d *Daemon) drainAll(ctx context.Context, kind scaler.DrainKind) scaler.DrainResult {
	results := make([]scaler.DrainResult, len(d.targets))
	var wg sync.WaitGroup
	for i, tr := range d.targets {
		wg.Go(func() { results[i] = tr.scaler.Drain(ctx, kind, d.drainTimeout) })
	}
	wg.Wait()
	res := scaler.DrainCompleted
	for _, r := range results {
		switch r {
		case scaler.DrainInterrupted:
			return r
		case scaler.DrainTimedOut:
			res = r
		}
	}
	return res
}

// firstSessionMade marks that a target made its first session (0002 SPEC
// R178).
func (d *Daemon) firstSessionMade() {
	d.firstOnce.Do(func() { close(d.firstSession) })
}

// fatal starts the server's error drain (R51) on an error that every
// target shares: the job check server stopping. A target's permanent
// session, scale set or AcquireJobs error goes to targetRun.fatal instead
// (0002 SPEC R171). It starts the error drain once, however often it is
// called (M5-5); during a signal drain it only logs (R100). It does not
// block.
func (d *Daemon) fatal(err error) {
	t := d.table
	if !t.SetDraining(scaler.DrainError) && t.Drain() == scaler.DrainSignal {
		d.log.Warn("permanent error during the signal drain; logged only (R100)", "err", err)
		return
	}
	started := false
	d.fatalOnce.Do(func() {
		started = true
		d.log.Error("permanent error; error drain started (R51)", "err", err)
		d.rec.Record("error drain", err)
		d.startDrain()
		close(d.fatalCh)
	})
	if !started {
		d.log.Debug("permanent error again during the error drain", "err", err)
	}
}

// fatalStarted reports whether fatal closed fatalCh.
func (d *Daemon) fatalStarted() bool {
	select {
	case <-d.fatalCh:
		return true
	default:
		return false
	}
}

// startDrain tells the session loops that a drain began, and stops the
// start phases still running (0002 SPEC R196).
func (d *Daemon) startDrain() {
	d.drainOnce.Do(func() {
		d.cancelStarts()
		close(d.drainStarted)
	})
}

// errorWait holds only the lock and the status socket until a signal
// (R45-R48). Finalize ran before, so nothing calls GitHub or Docker.
func (d *Daemon) errorWait(ctx context.Context) error {
	d.st.SetState(status.ErrorWait)
	last, _ := d.rec.Last()
	d.log.Error("error wait: fix the cause and restart the scaler (R46)", "lastError", last)
	select {
	case <-d.sig.first:
	case <-ctx.Done():
	}
	d.log.Info("signal in the error wait; exiting (R48)")
	return nil
}

// finalize removes what is left within the staged budget (PLAN M5-8). It
// is the same for exit and for entering the error wait, and runs once.
// Stages whose objects start-up never built are skipped. exiting says the
// process ends next; stage 7 then stops the status socket too, in parallel
// with the job check server and within the same share, so that the whole
// exit stays within 55 s. Before the error wait the socket stays up (R45).
func (d *Daemon) finalize(exiting bool) {
	if d.finalized {
		return
	}
	d.finalized = true
	d.cancelStarts()
	if d.table == nil {
		d.listenCancel()
		return
	}
	if d.st != nil {
		if st := d.st.Report().State; st == status.Running {
			d.st.SetState(status.Draining)
		}
	}
	log := d.log.With("phase", "finalize")
	t0 := d.clock.Now()
	a := t0.Add(finalizeRunShare)
	log.Info("finalize started")

	// 1. The finalize mark, deadline A, pending cleanup timers stopped, for
	// every target (0002 PLAN M12-7).
	for _, tr := range d.targets {
		tr.scaler.BeginFinalize(a)
	}
	d.armDeadlineA(a)
	// 2. Stop the judges (their 180 s timers and deny retries), the
	// watchers, the reconciler (its R103 goroutine); BeginFinalize stopped
	// the R101 timers.
	for _, tr := range d.targets {
		tr.judge.Stop()
		tr.scaler.Watcher().Stop()
	}
	if d.recon != nil {
		d.recon.Stop()
	}
	// 3. The listeners end; a DeleteMessage in flight ends at A.
	d.listenCancel()
	if d.runDone != nil && !d.waitUntil(d.runDone, a) {
		log.Warn("the listeners did not return by deadline A")
	}
	// 4. Everything in flight, until A + 10 s.
	d.waitInFlight(log, a.Add(finalizeWaitShare), d.finalizeWaiters())
	// 5. Remove the rows left and the pending registrations.
	d.removeLeftovers(log, nil)
	// 6. Close every target's session, in parallel within one share.
	var wg sync.WaitGroup
	for _, tr := range d.targets {
		wg.Go(func() { tr.closeSession(log.With("target", tr.t.Key()), finalizeCloseShare) })
	}
	wg.Wait()
	// 7. Stop the job check server, and on exit the status socket, in
	// parallel within one share.
	ctx, cancel := withTimeout(context.Background(), d.clock, finalizeShutdownShare)
	if d.server != nil {
		wg.Go(func() {
			if err := d.server.Shutdown(ctx); err != nil {
				log.Warn("job check server closed after its shutdown share", "err", err)
			}
		})
	}
	if exiting {
		wg.Go(func() { d.shutdownStatus(ctx) })
	}
	wg.Wait()
	cancel()
	log.Info("finalize ended", "took", d.clock.Now().Sub(t0))
}

// shutdownStatus stops the status socket once.
func (d *Daemon) shutdownStatus(ctx context.Context) {
	d.statusMu.Lock()
	if d.statusSrv == nil || d.statusDown {
		d.statusMu.Unlock()
		return
	}
	d.statusDown = true
	srv := d.statusSrv
	d.statusMu.Unlock()
	if err := srv.Shutdown(ctx); err != nil {
		d.log.Warn("stopping the status socket", "err", err)
	}
}

// armDeadlineA ends deadlineA at a, on the Clock.
func (d *Daemon) armDeadlineA(a time.Time) {
	left := a.Sub(d.clock.Now())
	if left <= 0 {
		d.cancelA()
		return
	}
	d.timerA = d.clock.AfterFunc(left, d.cancelA)
}

// waiter is one wait of finalize's stage 4.
type waiter struct {
	what string
	wait func(context.Context) error
}

// waitInFlight is finalize's stage 4: every wait in waits, all within one
// share that ends at until.
func (d *Daemon) waitInFlight(log *slog.Logger, until time.Time, waits []waiter) {
	ctx, cancel := withTimeout(context.Background(), d.clock, max(until.Sub(d.clock.Now()), 0))
	defer cancel()
	for _, w := range waits {
		if err := w.wait(ctx); err != nil {
			log.Warn("still running after the wait share; going on", "what", w.what)
		}
	}
}

// finalizeWaiters is what finalize's stage 4 waits for: every target's
// work, the shared reconciler and the listeners.
func (d *Daemon) finalizeWaiters() []waiter {
	var waits []waiter
	for _, tr := range d.targets {
		waits = append(waits,
			waiter{"scaler work", tr.scaler.WaitInFlight},
			waiter{"runner watches", tr.scaler.Watcher().Wait},
			waiter{"job checks", tr.judge.Wait})
	}
	if d.recon != nil {
		waits = append(waits, waiter{"reconciliation", d.recon.Wait})
	}
	if d.runDone != nil {
		waits = append(waits, waiter{"listener", func(ctx context.Context) error {
			select {
			case <-d.runDone:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		}})
	}
	return waits
}

// removeLeftovers is finalize's stage 5: for every row left, whatever its
// state or owner, RemoveResources and then RemoveRunner once; for every
// pending registration not among the rows, RemoveRunner once (R21, R55,
// R92). With only set, it is the same for that target's rows and
// registrations alone, at the end of its error drain (0002 PLAN M13-2).
// All in parallel on contexts without the caller's cancellation. The
// stage gets finalizeCleanupShare whatever the earlier stages used, on one
// stage context; each call gets finalizeCallShare or what is left of the
// stage, whichever is less, so every call ends with the stage and none
// starts after it. What is left goes to the next start's R16 and R17.
func (d *Daemon) removeLeftovers(log *slog.Logger, only *targetRun) {
	targets := d.targets
	if only != nil {
		targets = []*targetRun{only}
	}
	st := &cleanupStage{d: d, end: d.clock.Now().Add(finalizeCleanupShare)}
	var cancel context.CancelFunc
	st.ctx, cancel = withTimeout(context.Background(), d.clock, finalizeCleanupShare)
	defer cancel()

	// A runner ID belongs to one target's organization.
	type reg struct {
		target string
		id     int64
	}
	var wg sync.WaitGroup
	seen := make(map[reg]bool)
	for _, r := range d.table.List() {
		if only != nil && r.Target != only.t.Key() {
			continue
		}
		if r.ID != 0 {
			seen[reg{r.Target, r.ID}] = true
		}
		wg.Go(func() { st.removeRow(log, r) })
	}
	for _, tr := range targets {
		for _, u := range tr.scaler.PendingUnregistrations() {
			k := reg{tr.t.Key(), u.RunnerID}
			if seen[k] {
				continue
			}
			seen[k] = true
			wg.Go(func() { st.unregisterOnce(log.With("runner", u.Name), tr, u.RunnerID) })
		}
	}
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	// The stage does not wait past its share, which would take stages 6 and
	// 7's shares or the 55 s total. When the share is spent every call's
	// context has ended and no call starts any more (call), so a goroutine
	// still returning from a call makes no new Docker or GitHub call (R47);
	// only its log line and MarkRemoved on the finished table can come after
	// "finalize ended".
	select {
	case <-done:
	case <-st.ctx.Done():
		log.Warn("cleanup share spent; what is left goes to the next start (R16, R17)")
	}
}

// cleanupStage is finalize's stage 5 window.
type cleanupStage struct {
	d   *Daemon
	ctx context.Context
	end time.Time
}

// call returns the context of one call: finalizeCallShare or the rest of
// the stage, whichever is less. ok is false once the stage is over.
func (st *cleanupStage) call() (ctx context.Context, cancel context.CancelFunc, ok bool) {
	left := st.end.Sub(st.d.clock.Now())
	if left <= 0 || st.ctx.Err() != nil {
		return nil, nil, false
	}
	ctx, cancel = withTimeout(st.ctx, st.d.clock, min(finalizeCallShare, left))
	return ctx, cancel, true
}

// removeRow removes one row's resources, then its registration.
func (st *cleanupStage) removeRow(log *slog.Logger, r scaler.RunnerInfo) {
	d := st.d
	log = log.With("runner", r.Name, "state", r.State)
	ctx, cancel, ok := st.call()
	if !ok {
		log.Warn("cleanup share spent; runner left to the next start (R16, R17)")
		return
	}
	err := d.cfg.Docker.RemoveResources(ctx, r.Name)
	cancel()
	if err == nil || retry.Classify(context.Background(), retry.OpContainerRemove, err) == retry.Gone {
		d.table.MarkRemoved(r.Name)
		log.Info("runner resources removed")
	} else {
		log.Warn("removing runner resources failed; left to the next start (R16)", "err", err)
	}
	if tr := d.target(r.Target); tr != nil && r.ID != 0 {
		st.unregisterOnce(log, tr, r.ID)
	}
}

// unregisterOnce calls RemoveRunner once on tr's organization, unless tr's
// rate limit has not ended (PLAN M1-4: a call made once is left to the next
// start's R17) or the stage is over.
func (st *cleanupStage) unregisterOnce(log *slog.Logger, tr *targetRun, id int64) {
	gh, _ := tr.github()
	if gh == nil {
		return
	}
	if !tr.limits.Allowed(retry.OpRemoveRunner) {
		log.Info("RemoveRunner rate limited; registration left to the next start (R17)",
			"runnerId", id, "until", tr.limits.At(retry.OpRemoveRunner))
		return
	}
	ctx, cancel, ok := st.call()
	if !ok {
		log.Warn("cleanup share spent; registration left to the next start (R17)", "runnerId", id)
		return
	}
	err := gh.RemoveRunner(ctx, id)
	cancel()
	if err != nil {
		tr.limits.Record(retry.OpRemoveRunner, err)
		if retry.Classify(context.Background(), retry.OpRemoveRunner, err) != retry.Gone {
			log.Warn("RemoveRunner failed; registration left to the next start (R17)", "runnerId", id, "err", err)
			return
		}
	}
	log.Info("registration removed", "runnerId", id)
}

// target returns the target whose key is key, nil when none is.
func (d *Daemon) target(key string) *targetRun {
	for _, tr := range d.targets {
		if tr.t.Key() == key {
			return tr
		}
	}
	return nil
}

// waitUntil waits for ch to close until t on the Clock. It reports whether
// ch closed.
func (d *Daemon) waitUntil(ch <-chan struct{}, t time.Time) bool {
	left := t.Sub(d.clock.Now())
	if left <= 0 {
		select {
		case <-ch:
			return true
		default:
			return false
		}
	}
	fired, tm := d.timer(left)
	defer tm.Stop()
	select {
	case <-ch:
		return true
	case <-fired:
		select {
		case <-ch:
			return true
		default:
			return false
		}
	}
}

// timer returns a channel closed once d has passed on the Clock, and the
// Timer to stop it with, so a wait that ends early leaves no timer behind.
func (d *Daemon) timer(dur time.Duration) (<-chan struct{}, scaler.Timer) {
	fired := make(chan struct{})
	t := d.clock.AfterFunc(dur, func() { close(fired) })
	return fired, t
}

// sleep waits dur on the Clock, or until ctx ends, which it returns.
func (d *Daemon) sleep(ctx context.Context, dur time.Duration) error {
	fired, t := d.timer(dur)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-fired:
		return nil
	}
}

// signals turns the signal channel into two events: the first and the
// second signal.
type signals struct {
	first, second chan struct{}
}

// watchSignals reads ch until ctx ends. Ending parent counts as a signal,
// for tests.
func watchSignals(ctx, parent context.Context, ch <-chan os.Signal, log *slog.Logger) *signals {
	s := &signals{first: make(chan struct{}), second: make(chan struct{})}
	go func() {
		n := 0
		got := func() {
			n++
			switch n {
			case 1:
				close(s.first)
			case 2:
				close(s.second)
			}
		}
		for {
			select {
			case <-ctx.Done():
				if parent.Err() != nil && n < 2 {
					for n < 2 {
						got()
					}
				}
				return
			case sig, ok := <-ch:
				if !ok {
					return
				}
				log.Info("signal received", "signal", sig.String(), "count", n+1)
				got()
			}
		}
	}()
	return s
}

// received reports whether the first signal came.
func (s *signals) received() bool {
	select {
	case <-s.first:
		return true
	default:
		return false
	}
}

// chanContext returns a context that ends when ch closes, parent ends, or
// cancel is called. Call cancel when done with it.
func chanContext(parent context.Context, ch <-chan struct{}) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(parent)
	go func() {
		defer cancel()
		select {
		case <-ch:
		case <-ctx.Done():
		}
	}()
	return ctx, cancel
}
