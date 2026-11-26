package scaler

import (
	"cmp"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"log/slog"
	mathrand "math/rand/v2"
	"slices"
	"sync"
	"time"

	"github.com/actions/scaleset"

	"github.com/dewevrei/actions-runner-scaler/internal/gate"
	"github.com/dewevrei/actions-runner-scaler/internal/retry"
)

// Timeouts for the calls the scaler makes. A call that hits its own deadline
// is transient (retry.Classify), unlike shutdown of the caller's context,
// except when the server had already answered with a rate limit: that stays
// rate limited and waits per R52. During finalize every cleanup call also
// ends at the finalize deadline (cleanupContext).
const (
	// callTimeout bounds one GitHub call (SPEC: the run lookup's HTTP timeout
	// is 30 s; the scaler uses the same bound for its other calls).
	callTimeout = 30 * time.Second
	// cleanupTimeout bounds one RemoveResources call made on a context that
	// ignores cancellation, so cleanup runs to the end even during shutdown.
	// It is finalize's 20 s bound for removing what is left (PLAN M5-8).
	cleanupTimeout = 20 * time.Second
	// removeContainerTimeout bounds removing only a runner container, as the
	// deny path does (PLAN M4-2: 10 s).
	removeContainerTimeout = 10 * time.Second
	// startRunnerTimeout bounds Containers.StartRunner so a hung Docker call
	// cannot stall Scale, and with it the message session. Not from SPEC;
	// chosen here from two facts. R4 allows 60 s from the first message to
	// the job's started_at, so a start slower than that misses R4 anyway.
	// And while Scale runs the listener does not poll the session; SPEC
	// Design records that a session left unclosed became replaceable
	// between 30 and 61 s (observed 2026-11-20). 30 s, the bound of one
	// GitHub call, keeps one runner's JIT call and container start within
	// 60 s together. That alone does not keep R4: AcquireJobs (up to 30 s)
	// runs first in the same Scale, and earlier starts in the same Scale add
	// their time too.
	//
	// Worst case of one Scale, every call running to its bound:
	//   AcquireJobs                          30 s (callTimeout), once
	//   per runner started, up to max-runners:
	//     GenerateJITConfig                  30 s (callTimeout)
	//     StartRunner                        30 s (startRunnerTimeout)
	//     its undo inside the docker package 10 s (docker undoTimeout)
	//     cleanup RemoveResources            20 s (cleanupTimeout)
	//     cleanup RemoveRunner               30 s (callTimeout)
	//   = 30 s + 120 s per runner, 270 s at max 2. A scale-down instead
	//   spends up to 30 s (RemoveRunner) + 20 s (RemoveResources) per idle
	//   runner removed. Retries run on timers, never inside Scale.
	// 미확인 (unverified): whether a session that is not polled for that long
	// expires as an unclosed one does; if it does, the next GetMessage or
	// DeleteMessage fails and the daemon makes a new session (R25).
	startRunnerTimeout = 30 * time.Second
)

// ErrFinalizing is returned by Scale whenever finalize has begun by the time
// it ends, in place of nil: the listener then does not acknowledge the
// message (DeleteMessage is a GitHub call, R47), and the message comes again
// to the next session after a restart. With available jobs AcquireJobs is
// not called.
var ErrFinalizing = errors.New("scaler: finalizing")

// Start failure backoff (R61): after startBackoffAfter failures in a row,
// the next start waits startBackoffInitial, doubling up to startBackoffMax.
const (
	startBackoffAfter   = 3
	startBackoffInitial = 10 * time.Second
	startBackoffMax     = 5 * time.Minute
)

// ScaleSetAPI is the scale set calls for one runner. The daemon adapts the
// github package to it; errors must carry retry's status, sentinels and
// rate-limit types so retry.Classify can read them.
type ScaleSetAPI interface {
	// GenerateJITConfig registers a runner named name and returns its JIT
	// config (GenerateJitRunnerConfig).
	GenerateJITConfig(ctx context.Context, name string) (JITRunner, error)
	// RemoveRunner removes the runner registration.
	RemoveRunner(ctx context.Context, runnerID int64) error
}

// JobAcquirer acquires available jobs for the current message session
// (MessageSessionClient.AcquireJobs).
type JobAcquirer interface {
	AcquireJobs(ctx context.Context, requestIDs []int64) ([]int64, error)
}

// Containers is the Docker side of one runner. The daemon adapts the docker
// package to it.
type Containers interface {
	// StartRunner creates the runner's network, BuildKit container and
	// runner container, then writes jit and token to the runner's stdin as
	// two lines. On failure it removes what it made.
	StartRunner(ctx context.Context, name string, jit JITConfig, token gate.Token) error
	// RemoveResources removes both containers with their volumes and the
	// network. Not found counts as success.
	RemoveResources(ctx context.Context, name string) error
	// RemoveContainer force-removes only the runner container. Not found
	// counts as success. The error drain uses it when RemoveRunner fails
	// (R53), and the deny path once the runner stopped or its grace ended
	// (R11).
	RemoveContainer(ctx context.Context, name string) error
}

// Judge removes runners through the deny path (M4). The verdict itself
// comes from the hook's event report, never from Scale (0002 SPEC R123).
// Scale calls Reject while it holds its own lock, so it must return without
// waiting for GitHub or Docker and must not call back into the Scaler.
// Scale checks the finalize mark before the call, but finalize can begin
// between that check and the call, so a call may still arrive just after
// finalize has begun. Implementations must check Table.Finalizing
// themselves and do nothing once it is set; work they start is not counted
// by WaitInFlight. JobJudge is the implementation, which New binds to the
// Scaler (it then uses the Scaler's table, clock, logger, error recorder
// and removal rules); tests may give a fake.
type Judge interface {
	// Reject removes runner name through the deny path, as for a denied job
	// (R11, R102). A redelivered message calls it again for the same runner,
	// so it must be idempotent: a second call for a runner whose removal
	// has begun must do nothing (Table.BeginExit decides the one owner).
	Reject(name, reason string)
}

// Watcher waits for a started runner's container to end (M3-8). Watch is
// called from inside Scale while Scale holds its lock, so it must not
// block: it starts the wait in its own goroutine and returns. Like Judge,
// a call may arrive just after finalize has begun (MarkStarted is checked
// before Watch is called), so the watcher must check Table.Finalizing
// itself; its goroutines are not counted by WaitInFlight. RunnerWatcher is
// the implementation; the Scaler makes it from Config.Waiter.
type Watcher interface {
	Watch(name string)
}

// ErrorRecorder keeps the last error for status (R26, R86, R94). where names
// the call site. Every call replaces the last error; callers that can fail
// repeatedly suppress repeats themselves (errorOnce).
type ErrorRecorder interface {
	Record(where string, err error)
}

// SecretRegistry hides registered values from logs (logging.Secrets).
// Register and Unregister count, so a value registered twice stays until it
// is unregistered twice. Redact replaces every value registered at the
// time of the call. The Table calls Register and Unregister with its mutex
// held, so none of the methods may call the Table. Implementations must be
// comparable (a pointer type such as *logging.Secrets), because New checks
// that the Table and the Config hold the same registry.
type SecretRegistry interface {
	Register(value string)
	Unregister(value string)
	Redact(text string) string
}

// TokenSource makes job check tokens (gate.TokenGenerator).
type TokenSource interface {
	New() (gate.Token, error)
}

// RateLimits holds the next allowed time per kind of call (retry.NextAllowed).
type RateLimits interface {
	Record(op retry.Op, err error) bool
	Allowed(op retry.Op) bool
	At(op retry.Op) time.Time
}

// TargetConfig is what the Scaler takes from one [[targets]] entry (0002 PLAN
// M9).
type TargetConfig struct {
	// Key tells targets apart: the lower-case organization name (github
	// Target.Key).
	Key string
	// Label is the scale set name and runner name prefix (scaling.label).
	Label string
	// MaxRunners is targets.max-runners and Idle is targets.idle.
	MaxRunners, Idle int
}

// Config holds a Scaler's settings and dependencies. Fields marked optional
// may be left nil.
type Config struct {
	// Target supplies the target's key, the runner name prefix, max-runners
	// and idle.
	Target TargetConfig

	Table      *Table
	Clock      Clock
	ScaleSet   ScaleSetAPI
	Containers Containers
	Judge      Judge
	Tokens     TokenSource
	Limits     RateLimits
	// Fatal receives the permanent errors of R51 that start the target's
	// error drain (0002 SPEC R171). Scale sets the target's drain mark
	// (Table.SetTargetDraining) right before the call, so the next Scale
	// starts no runner however late the drain itself begins. Scale calls
	// Fatal while holding its lock, so it must not block and must not call
	// RemoveIdle or Scale (that would deadlock); the drain it starts runs in
	// another goroutine. During a drain Scale still calls AcquireJobs (R89,
	// until 0002 PLAN M14), so a lasting permanent error calls Fatal again
	// on every Scale: it must be idempotent.
	Fatal func(error)
	// Secrets hides the JIT config and the job check token in the stored
	// start error (R31). It is required, because without it a start failure
	// would be stored with both values in clear; it should be the registry
	// given to the Table, which registers the token there.
	Secrets SecretRegistry

	// Waiter waits for runner containers to end. The Scaler's RunnerWatcher
	// (Scaler.Watcher) uses it to watch every runner it starts (R6).
	Waiter ContainerWaiter

	// Logger is optional; nil discards logs.
	Logger *slog.Logger
	// Errors is optional.
	Errors ErrorRecorder
	// Random is the source for runner name suffixes; nil means crypto/rand.
	Random io.Reader
	// Jitter returns values in [0, 1) for the start backoff; nil means
	// math/rand/v2.Float64.
	Jitter func() float64
}

// Scaler turns scale set messages into runner starts and removals for one
// target. Every target has its own Scaler, and all of them share one Table
// (0002 PLAN M11-4); a Scaler acts only on its own target's rows. Bind it to
// a message session with ForSession.
type Scaler struct {
	target     TargetConfig
	table      *Table
	clock      Clock
	scaleSet   ScaleSetAPI
	containers Containers
	judge      Judge
	tokens     TokenSource
	limits     RateLimits
	fatal      func(error)
	// log carries the target key; baseLog is Config.Logger without it, for
	// the server-wide Reconciler.
	log     *slog.Logger
	baseLog *slog.Logger
	// watcher is what the start path calls; it is watch unless a test
	// replaces it.
	watcher Watcher
	watch   *RunnerWatcher
	// waiter is Config.Waiter; the RunnerWatcher and the deny path both
	// wait for runner containers with it.
	waiter  ContainerWaiter
	errs    ErrorRecorder
	secrets SecretRegistry
	random  io.Reader
	// jitter seeds the backoffs this scaler makes.
	jitter func() float64

	// work counts Scale and RemoveIdle calls, start paths and deferred
	// cleanup steps in flight, for finalize (M5-8). deferred holds the
	// timers of the latter.
	work     inFlight
	deferred *deferredWork
	// finalizeCtx is canceled at the finalize deadline, measured on the
	// Clock (armFinalizeDeadline). Every call context made by callContext
	// ends with it, so a cleanup call that began before finalize still ends
	// within the budget.
	finalizeCtx    context.Context
	finalizeCancel context.CancelFunc
	finalizeArmed  sync.Once
	// unregs holds the registrations a start cleanup could not confirm
	// removed (PendingUnregistrations).
	unregs pendingUnregistrations

	// scaleMu serializes Scale calls; the fields below belong to it.
	scaleMu      sync.Mutex
	assigned     int
	startBackoff *retry.Backoff
}

// New returns a Scaler. It returns an error when a required dependency is
// missing or max-runners is not positive.
func New(cfg Config) (*Scaler, error) {
	var missing []string
	for _, d := range []struct {
		name    string
		missing bool
	}{
		{"Table", cfg.Table == nil},
		{"Clock", cfg.Clock == nil},
		{"ScaleSet", cfg.ScaleSet == nil},
		{"Containers", cfg.Containers == nil},
		{"Judge", cfg.Judge == nil},
		{"Tokens", cfg.Tokens == nil},
		{"Limits", cfg.Limits == nil},
		{"Fatal", cfg.Fatal == nil},
		{"Secrets", cfg.Secrets == nil},
		{"Waiter", cfg.Waiter == nil},
	} {
		if d.missing {
			missing = append(missing, d.name)
		}
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("scaler: missing %v", missing)
	}
	// The Table registers the job check token in its own registry, and the
	// stored start error is redacted with cfg.Secrets, so the two must be the
	// same registry or the token would stay in clear (R31).
	if cfg.Table.secrets != cfg.Secrets {
		return nil, errors.New("scaler: Secrets must be the registry given to NewTable")
	}
	if cfg.Target.MaxRunners < 1 {
		return nil, fmt.Errorf("scaler: max-runners %d must be positive", cfg.Target.MaxRunners)
	}
	if cfg.Jitter == nil {
		cfg.Jitter = mathrand.Float64
	}
	backoff, err := retry.NewBackoff(cfg.Clock, cfg.Jitter, startBackoffInitial, startBackoffMax)
	if err != nil {
		return nil, fmt.Errorf("scaler: %w", err)
	}
	s := &Scaler{
		target:       cfg.Target,
		table:        cfg.Table,
		clock:        cfg.Clock,
		scaleSet:     cfg.ScaleSet,
		containers:   cfg.Containers,
		judge:        cfg.Judge,
		tokens:       cfg.Tokens,
		limits:       cfg.Limits,
		fatal:        cfg.Fatal,
		log:          cfg.Logger,
		errs:         cfg.Errors,
		secrets:      cfg.Secrets,
		random:       cfg.Random,
		jitter:       cfg.Jitter,
		startBackoff: backoff,
	}
	s.deferred = newDeferredWork(cfg.Clock, &s.work)
	s.finalizeCtx, s.finalizeCancel = context.WithCancel(context.Background())
	s.unregs.ids = make(map[int64]string)
	if _, err := s.newRemovalBackoff(); err != nil {
		return nil, fmt.Errorf("scaler: removal backoff: %w", err)
	}
	if s.log == nil {
		s.log = slog.New(slog.DiscardHandler)
	}
	s.baseLog = s.log
	s.log = s.log.With("targetKey", cfg.Target.Key)
	s.waiter = cfg.Waiter
	s.watch = newRunnerWatcher(s, cfg.Waiter)
	s.watcher = s.watch
	if s.errs == nil {
		s.errs = noRecorder{}
	}
	if s.random == nil {
		s.random = rand.Reader
	}
	if b, ok := cfg.Judge.(judgeBinder); ok {
		if err := b.bind(s); err != nil {
			s.finalizeCancel()
			return nil, fmt.Errorf("scaler: %w", err)
		}
	}
	return s, nil
}

type noRecorder struct{}

func (noRecorder) Record(string, error) {}

type noSecrets struct{}

func (noSecrets) Register(string)           {}
func (noSecrets) Unregister(string)         {}
func (noSecrets) Redact(text string) string { return text }

// SessionScaler is a Scaler bound to one message session. It implements
// listener.Scaler.
type SessionScaler struct {
	s   *Scaler
	acq JobAcquirer
}

// ForSession binds the scaler to the session whose AcquireJobs it calls.
// The daemon makes one per listener.Run.
func (s *Scaler) ForSession(acq JobAcquirer) *SessionScaler {
	return &SessionScaler{s: s, acq: acq}
}

// Scale handles one message, or a nil message when the long poll timed out
// (listener.Scaler).
//
// Order: JobStarted, JobCompleted, AcquireJobs (also while draining, R89),
// then the count with starts or removals. An AcquireJobs error other than a
// permanent one is returned after the job messages are handled, so the
// message is not acked and comes again (R25, R52). A permanent one sets the
// error drain, goes to Fatal, and Scale returns nil without starting
// anything (R51). Runner start and removal failures never make Scale fail
// (R64). Once finalize has begun Scale begins no Docker or GitHub call, and
// a Scale that ends after finalize began returns ErrFinalizing instead of
// nil, so the message is not acknowledged (R47).
//
// A Scale in progress counts as work in flight for WaitInFlight.
func (ss *SessionScaler) Scale(ctx context.Context, msg *scaleset.RunnerScaleSetMessage) error {
	return ss.s.scale(ctx, ss.acq, msg)
}

func (s *Scaler) scale(ctx context.Context, acq JobAcquirer, msg *scaleset.RunnerScaleSetMessage) error {
	// Counted before any finalize check, so a finalize that begins after a
	// check still waits for this call (WaitInFlight).
	s.work.begin()
	defer s.work.end()
	s.scaleMu.Lock()
	defer s.scaleMu.Unlock()

	err := s.handle(ctx, acq, msg)
	if err == nil && s.table.Finalizing() {
		s.log.Info("message not acknowledged; finalize began")
		return ErrFinalizing
	}
	return err
}

// handle is Scale's work, with scaleMu held.
func (s *Scaler) handle(ctx context.Context, acq JobAcquirer, msg *scaleset.RunnerScaleSetMessage) error {
	if msg != nil {
		for _, m := range msg.JobStartedMessages {
			s.jobStarted(m)
		}
		for _, m := range msg.JobCompletedMessages {
			s.jobCompleted(m)
		}
		// A nil Statistics keeps the last one, like a nil message.
		if msg.Statistics != nil {
			s.assigned = msg.Statistics.TotalAssignedJobs
		}
	}

	fatal, err := s.acquire(ctx, acq, msg)
	if err != nil {
		return err
	}
	if fatal {
		return nil
	}
	return s.converge(ctx)
}

func (s *Scaler) jobStarted(m *scaleset.JobStarted) {
	if m == nil || m.RunnerName == "" {
		s.log.Debug("JobStarted without runner name ignored")
		return
	}
	name := m.RunnerName
	log := s.log.With("runner", name, "jobId", m.JobID)
	if s.table.otherTarget(name, s.target.Key) {
		log.Debug("JobStarted for a runner not of this target ignored")
		return
	}
	job := Job{
		ID:            m.JobID,
		WorkflowRunID: m.WorkflowRunID,
		Owner:         m.OwnerName,
		Repository:    m.RepositoryName,
		Event:         m.EventName,
	}
	switch s.table.RecordJobStarted(name, job) {
	case JobStartedUnknown:
		log.Debug("JobStarted for unknown runner ignored")
	case JobStartedIgnored:
		log.Debug("JobStarted for runner being removed ignored")
	case JobStartedDoneSame:
		log.Debug("JobStarted again for the job the runner completed")
	case JobStartedDoneOther:
		if s.table.Finalizing() {
			log.Info("JobStarted for another job on a done runner; token revoked, left to finalize")
			return
		}
		log.Info("JobStarted for another job on a done runner; token revoked, rejecting runner")
		s.judge.Reject(name, "JobStarted for job "+m.JobID+" after the runner completed another job")
	case JobStartedOtherJob:
		log.Warn("JobStarted for a second job on one runner ignored")
	case JobStartedRecorded:
		// A busy source of R81; the verdict comes from the hook's event
		// report alone (0002 SPEC R123, R133).
		s.table.MarkBusy(name, s.clock.Now())
		log.Info("job started", "workflowRunId", m.WorkflowRunID)
	}
}

func (s *Scaler) jobCompleted(m *scaleset.JobCompleted) {
	if m == nil || m.RunnerName == "" {
		s.log.Debug("JobCompleted without runner name ignored")
		return
	}
	log := s.log.With("runner", m.RunnerName, "jobId", m.JobID)
	if s.table.otherTarget(m.RunnerName, s.target.Key) {
		log.Debug("JobCompleted for a runner not of this target ignored")
		return
	}
	switch s.table.RecordJobCompleted(m.RunnerName, m.JobID, s.clock.Now()) {
	case JobCompletedUnknown:
		log.Debug("JobCompleted for unknown runner ignored")
	case JobCompletedIgnored:
		log.Debug("JobCompleted for runner being removed ignored")
	case JobCompletedOtherJob:
		log.Info("JobCompleted for a job the runner did not start; state unchanged")
	case JobCompletedAlreadyDone:
		log.Debug("JobCompleted again; first time kept")
	case JobCompletedDone:
		log.Info("job completed", "result", m.Result)
		s.armDoneCleanup(log, m.RunnerName)
	}
}

// armDoneCleanup sets the runner's R101 timer, once per runner: the caller
// got JobCompletedDone, which RecordJobCompleted returns only for the first
// JobCompleted, so a redelivered message neither moves the base time nor
// sets a second timer. The timer is deferred work: it counts for
// WaitInFlight and finalize stops it (M5-8). It runs on every drain kind.
// The 30-second reconciliation is the backup for a done runner without a
// timer (Reconciler).
func (s *Scaler) armDoneCleanup(log *slog.Logger, name string) {
	if !s.after(doneCleanupDelay, func() { s.doneCleanup(name) }) {
		log.Info("finalize began; no R101 timer for the done runner")
	}
}

// acquire calls AcquireJobs for the message's available jobs. It reports
// fatal when the error was permanent and went to Fatal; any other failure
// comes back as an error for Scale to return.
func (s *Scaler) acquire(ctx context.Context, acq JobAcquirer, msg *scaleset.RunnerScaleSetMessage) (fatal bool, err error) {
	if msg == nil {
		return false, nil
	}
	var ids []int64
	for _, m := range msg.JobAvailableMessages {
		if m != nil {
			ids = append(ids, m.RunnerRequestID)
		}
	}
	if len(ids) == 0 {
		return false, nil
	}

	if s.table.Finalizing() {
		s.log.Info("AcquireJobs skipped; finalize began")
		return false, ErrFinalizing
	}
	if !s.limits.Allowed(retry.OpAcquireJobs) {
		until := s.limits.At(retry.OpAcquireJobs)
		s.log.Info("AcquireJobs deferred by rate limit", "until", until)
		return false, &retry.OpError{Op: retry.OpAcquireJobs, Err: &retry.RateLimitError{
			RetryAfter: until.Sub(s.clock.Now()),
			Err:        errors.New("not allowed before the rate limit ends"),
		}}
	}

	callCtx, cancel := context.WithTimeout(ctx, callTimeout)
	acquired, err := acq.AcquireJobs(callCtx, ids)
	cancel()
	if err == nil {
		s.log.Debug("jobs acquired", "requested", len(ids), "acquired", len(acquired))
		return false, nil
	}
	s.limits.Record(retry.OpAcquireJobs, err)
	err = withOp(retry.OpAcquireJobs, err)
	class := retry.Classify(ctx, retry.OpAcquireJobs, err)
	if class == retry.Permanent {
		s.log.Error("AcquireJobs failed permanently", "err", err)
		// Set here, not only by the daemon's drain, so the next Scale starts
		// nothing even if the drain begins later (R51). Only this target
		// drains (0002 SPEC R171); during a server drain the target does not
		// enter its error drain (R100, 0002 SPEC R191).
		// A server drain that begins between the check and the mark leaves
		// the mark set, which is harmless: drainKind prefers the server's.
		if s.table.Drain() == DrainNone {
			s.table.SetTargetDraining(s.target.Key)
		}
		s.fatal(err)
		return true, nil
	}
	s.log.Warn("AcquireJobs failed", "class", class, "err", err)
	return false, err
}

// goal is the number of live runners Scale aims for (R1, R7); zero while
// draining.
func (s *Scaler) goal() int {
	if s.draining() {
		return 0
	}
	return min(s.target.MaxRunners, s.target.Idle+s.assigned)
}

// draining reports whether the target's runners drain: a server drain or
// finalize, or the target's own error drain (0002 SPEC R173).
func (s *Scaler) draining() bool {
	return s.table.Draining() || s.table.TargetDraining(s.target.Key)
}

// drainKind is the drain whose rules the target's scale-down follows: the
// server's, or DrainError during the target's error drain (0002 SPEC
// R173). A signal drain that begins during the target's error drain takes
// over (0002 SPEC R179).
func (s *Scaler) drainKind() DrainKind {
	if k := s.table.Drain(); k != DrainNone {
		return k
	}
	if s.table.TargetDraining(s.target.Key) {
		return DrainError
	}
	return DrainNone
}

// converge starts or removes the target's runners to reach its goal (R1,
// R2, R3, R37). Starts are bounded by the target's max-runners and by the
// server limit over all targets (0002 SPEC R114); Table.Add checks both again
// for each start, so a start of another target in between cannot pass them.
func (s *Scaler) converge(ctx context.Context) error {
	c := s.table.Counts(s.target.Key)
	target := s.goal()
	maxRunners := s.target.MaxRunners
	live, all := c.Live(), c.All()
	room := s.table.ServerLimit() - s.table.CountsAll().All()
	toStart := max(0, min(target-live, maxRunners-all, room))
	s.log.Debug("scale",
		"assigned", s.assigned, "target", target, "live", live, "all", all,
		"serverRoom", room, "draining", s.draining(), "toStart", toStart)

	if toStart > 0 {
		return s.startRunners(ctx, toStart)
	}
	if live > target {
		return s.removeIdle(ctx, live-target)
	}
	return nil
}

// RemoveIdle removes the idle runners beyond the target, which while
// draining is every idle runner (R37, R53, R91). Scale does the same after
// each message; the drain code calls RemoveIdle when no message comes, for
// example after Run returned during an error drain. It waits for a Scale in
// progress, counts as work in flight for WaitInFlight, calls nothing once
// finalize began, and returns ctx.Err() when ctx ends during a RemoveRunner
// call. Finalize should cancel ctx, as it cancels the listener's, so that a
// RemoveRunner in flight ends early.
func (s *Scaler) RemoveIdle(ctx context.Context) error {
	s.work.begin()
	defer s.work.end()
	return s.removeIdleCounted(ctx)
}

// removeIdleCounted is RemoveIdle for a caller that already counted the
// call as work in flight (Drain counts it before starting its goroutine).
func (s *Scaler) removeIdleCounted(ctx context.Context) error {
	s.scaleMu.Lock()
	defer s.scaleMu.Unlock()
	live, target := s.table.Counts(s.target.Key).Live(), s.goal()
	if live <= target {
		return nil
	}
	return s.removeIdle(ctx, live-target)
}

// removeIdle calls RemoveRunner for up to n idle runners of the target,
// oldest first (R37), by the rules of removeIdleOne.
func (s *Scaler) removeIdle(ctx context.Context, n int) error {
	drain := s.drainKind()
	draining := s.draining()
	tried := 0
	for _, r := range s.table.RemovalCandidates(s.target.Key) {
		if tried == n {
			return nil
		}
		called, stop, err := s.removeIdleOne(ctx, r, drain, draining)
		if called {
			tried++
		}
		if err != nil || stop {
			return err
		}
	}
	return nil
}

// removeIdleRunner applies scale-down's rules to the one runner name, if it
// is still idle and its removal was not given up (R37, R94). Reconciliation
// calls it for an idle runner whose BuildKit container is gone or stopped
// (R67). Like RemoveIdle it waits for a Scale in progress, counts as work in
// flight, and calls nothing once finalize began.
func (s *Scaler) removeIdleRunner(ctx context.Context, name string) error {
	s.work.begin()
	defer s.work.end()
	s.scaleMu.Lock()
	defer s.scaleMu.Unlock()
	info, ok := s.table.Get(name)
	if !ok || info.Target != s.target.Key || !s.table.IdleRemovable(name) {
		return nil
	}
	_, _, err := s.removeIdleOne(ctx, info, s.drainKind(), s.draining())
	return err
}

// removeIdleOne calls RemoveRunner for the idle runner r (R37). During a
// drain a JobStillRunningError marks the runner busy so the drain waits for
// it (R53, R91); during an error drain any other RemoveRunner failure
// removes the runner container only (R53). called reports whether
// RemoveRunner was called; stop that scale-down must not go on to the next
// runner (rate limit, finalize, cancellation).
//
// Right before RemoveRunner it checks again that finalize has not begun
// (finalize removes every runner itself, M5-8) and that the runner is still
// idle; the steps after a RemoveRunner (beginRemoval, removeRunnerContainer)
// go ahead only while the runner is still idle, and during finalize their
// calls end by the finalize deadline.
func (s *Scaler) removeIdleOne(ctx context.Context, r RunnerInfo, drain DrainKind, draining bool) (called, stop bool, err error) {
	if !s.limits.Allowed(retry.OpRemoveRunner) {
		s.log.Info("scale-down stopped by rate limit",
			"until", s.limits.At(retry.OpRemoveRunner))
		return false, true, nil
	}
	log := s.log.With("runner", r.Name)
	if s.table.Finalizing() {
		s.log.Info("scale-down stopped; finalize began")
		return false, true, nil
	}
	if !s.table.IdleRemovable(r.Name) {
		log.Debug("runner no longer idle; not removed")
		return false, false, nil
	}
	callCtx, cancel, ok := s.callContext(ctx, callTimeout)
	if !ok {
		return false, true, nil
	}
	err = s.scaleSet.RemoveRunner(callCtx, r.ID)
	cancel()
	var class retry.Class
	if err != nil {
		s.limits.Record(retry.OpRemoveRunner, err)
		class = retry.Classify(ctx, retry.OpRemoveRunner, err)
	}

	switch {
	case err == nil || class == retry.Gone:
		s.beginRemoval(ctx, log, r.Name)
	case class == retry.Canceled:
		return true, true, ctx.Err()
	case errors.Is(err, retry.ErrJobStillRunning) && draining:
		log.Info("runner took a job during drain; waiting for it", "drain", drain)
		s.table.MarkBusy(r.Name, s.clock.Now())
	case drain == DrainError:
		s.removeRunnerContainer(ctx, log, r.Name, class, err)
		if class == retry.RateLimited {
			return true, true, nil
		}
	case class == retry.RateLimited:
		log.Warn("RemoveRunner rate limited; stopping scale-down", "err", err)
		return true, true, nil
	case class == retry.Permanent:
		log.Error("RemoveRunner failed permanently; keeping runner", "err", err)
		s.table.GiveUpRemoval(r.Name)
		s.errs.Record("scale-down remove runner", err)
	default:
		log.Info("RemoveRunner failed; retrying on a later scale", "class", class, "err", err)
	}
	return true, false, nil
}

// beginRemoval removes the resources of a runner whose registration is
// gone, if the runner is still idle: BeginExitIfIdle moves it to exiting
// and revokes its token in one step (R37, R38). A runner that took a job
// while RemoveRunner ran keeps its containers; its watcher removes them
// when the runner ends.
func (s *Scaler) beginRemoval(ctx context.Context, log *slog.Logger, name string) {
	if !s.table.BeginExitIfIdle(name) {
		if info, ok := s.table.Get(name); ok && info.State != StateExiting {
			log.Warn("runner unregistered but no longer idle; containers left to its watcher",
				"state", info.State)
		}
		return
	}
	log.Info("idle runner unregistered; removing")
	s.removeResources(ctx, log, name, true)
}

// removeRunnerContainer handles an idle runner whose RemoveRunner failed
// during an error drain for a reason other than JobStillRunningError: it
// removes only the runner container and leaves the registration to the next
// start's R17 (R53). It does nothing unless the runner is still idle, so a
// runner that took a job meanwhile keeps its container. The row stays
// exiting and is abandoned (Table.AbandonExit), so finalize or
// reconciliation can still remove the BuildKit container and network and
// try RemoveRunner (R55, M3-8, M5-8); after a permanent failure to remove
// the runner container it is abandoned as AbandonPermanent, which
// reconciliation does not claim (R42). Not found counts as removed.
func (s *Scaler) removeRunnerContainer(ctx context.Context, log *slog.Logger, name string, class retry.Class, cause error) {
	if !s.table.BeginExitIfIdle(name) {
		log.Info("RemoveRunner failed during error drain; runner no longer idle, container kept",
			"class", class, "err", cause)
		return
	}
	reason := AbandonErrorDrain
	defer func() { s.table.AbandonExit(name, reason) }()
	log.Warn("RemoveRunner failed during error drain; removing the runner container only",
		"class", class, "err", cause)
	s.errs.Record("error drain remove runner", cause)

	callCtx, cancel, ok := s.cleanupContext(ctx, removeContainerTimeout)
	if !ok {
		reason = AbandonNoBudget
		log.Warn("finalize budget spent; runner container left to the next start")
		return
	}
	err := s.containers.RemoveContainer(callCtx, name)
	cancel()
	if err != nil {
		class := retry.Classify(context.WithoutCancel(ctx), retry.OpContainerRemove, err)
		if class != retry.Gone {
			if class == retry.Permanent {
				reason = AbandonPermanent
			}
			log.Error("removing runner container during error drain failed", "class", class, "err", err)
			s.errs.Record("error drain remove runner container", err)
			return
		}
	}
	log.Info("runner container removed; registration left to finalize and the next start")
}

// BeginFinalize starts finalize (M5-8): it sets the table's finalize mark
// with the end of the budget, measured on the Clock, stops every deferred
// cleanup step that has not started (R101 timers among them), and stops the
// RunnerWatcher. Every cleanup and scale-down
// call ends by deadline, also one that began before finalize; Scale,
// RemoveIdle, start paths and cleanup steps already running finish what
// they began, and WaitInFlight waits for them. From then on the scaler
// begins no new step, so once WaitInFlight returns it makes no Docker or
// GitHub call (R47). Only the first call sets the deadline.
//
// The JIT, StartRunner and AcquireJobs calls of a Scale are not bound by
// the deadline; finalize ends them by cancelling the listener's context.
func (s *Scaler) BeginFinalize(deadline time.Time) {
	s.table.BeginFinalize(deadline)
	s.armFinalizeDeadline()
	s.deferred.stop()
	s.watch.Stop()
}

// armFinalizeDeadline cancels finalizeCtx at the table's finalize deadline,
// once, if finalize has begun.
func (s *Scaler) armFinalizeDeadline() {
	deadline, ok := s.table.FinalizeDeadline()
	if !ok {
		return
	}
	s.finalizeArmed.Do(func() {
		left := deadline.Sub(s.clock.Now())
		if left <= 0 {
			s.finalizeCancel()
			return
		}
		s.clock.AfterFunc(left, s.finalizeCancel)
	})
}

// StopWatches stops the target's RunnerWatcher and its pending removal
// retries and R101 timers, before the target enters the target error wait
// (0002 SPEC R190). Unlike BeginFinalize it leaves the table and the other
// targets alone, and ResumeWatches undoes it.
func (s *Scaler) StopWatches() {
	s.deferred.stop()
	s.watch.Stop()
}

// ResumeWatches lets the watcher and the removal retries work again when
// the target returns to running (0002 SPEC R194). It does nothing once
// finalize has begun.
func (s *Scaler) ResumeWatches() {
	s.deferred.resume(s.table.Finalizing)
	s.watch.resume()
}

// WaitInFlight waits until nothing the scaler began is running or pending,
// or ctx ends: Scale and RemoveIdle calls, start paths, the removals that
// watch and reconcile began, and the steps on timers (resource removal
// retries, RemoveRunner retries, R101 timers). It does not wait for the
// RunnerWatcher's goroutines, which wait for containers without a timeout;
// Watcher().Wait does. Finalize (M5-8) calls it after BeginFinalize and cancelling the
// listener, so start paths clean up what they made and a scale-down step in
// flight finishes. If the table's finalize mark was set without
// BeginFinalize, WaitInFlight stops the pending steps and arms the deadline
// itself; outside finalize it also waits for pending retries, which may take
// minutes.
func (s *Scaler) WaitInFlight(ctx context.Context) error {
	if s.table.Finalizing() {
		s.armFinalizeDeadline()
		s.deferred.stop()
		s.watch.Stop()
	}
	return s.work.wait(ctx)
}

// Unregistration is a runner registration whose removal a start cleanup
// began but could not confirm (R18): RemoveRunner is waiting for a retry
// after a rate limit or a transient error, ran out of retries or finalize
// budget, or was stopped by finalize.
type Unregistration struct {
	// Name is the runner's name. Its row may have left the table already.
	Name string
	// RunnerID is the GitHub runner ID for RemoveRunner.
	RunnerID int64
}

// PendingUnregistrations returns the registrations a start cleanup could not
// confirm removed, by runner ID. Finalize (M5-8) tries RemoveRunner once for
// each, as for the rows it removes; an ID may also belong to a row still in
// the table, so finalize tries each ID once. An entry leaves when its
// RemoveRunner succeeds, finds the runner gone, or fails permanently.
func (s *Scaler) PendingUnregistrations() []Unregistration {
	return s.unregs.list()
}

// pendingUnregistrations is the set behind PendingUnregistrations.
type pendingUnregistrations struct {
	mu  sync.Mutex
	ids map[int64]string
}

func (p *pendingUnregistrations) add(id int64, name string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.ids[id] = name
}

func (p *pendingUnregistrations) remove(id int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.ids, id)
}

func (p *pendingUnregistrations) list() []Unregistration {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]Unregistration, 0, len(p.ids))
	for id, name := range p.ids {
		out = append(out, Unregistration{Name: name, RunnerID: id})
	}
	slices.SortFunc(out, func(a, b Unregistration) int { return cmp.Compare(a.RunnerID, b.RunnerID) })
	return out
}

// withOp wraps err with op unless an adapter already did, so the message
// names the call once. retry.Classify reads only the op it is given, so the
// class does not change.
func withOp(op retry.Op, err error) error {
	var opErr *retry.OpError
	if errors.As(err, &opErr) {
		return err
	}
	return &retry.OpError{Op: op, Err: err}
}
