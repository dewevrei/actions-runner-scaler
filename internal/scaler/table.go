package scaler

import (
	"cmp"
	"errors"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/dewevrei/actions-runner-scaler/internal/gate"
)

// ErrDuplicateName is returned by Table.Add for a name already in the
// table. It is unrelated to retry.ErrRunnerExists, which GitHub reports for
// a registration.
var ErrDuplicateName = errors.New("scaler: runner name already in table")

// BusyFunc is called once per runner, outside the table's mutex, when the
// runner is first marked busy. The job check timer (R81) is one, which New
// installs from the JobJudge; NewTable takes another for observers. It is
// not called once finalize has begun, which stops the timers (M5-8): the
// mark is read under the mutex together with the busy transition, so a call
// can still follow a finalize that begins after the mutex is released, and
// the timer code must check the mark itself. Scale and RemoveIdle may call
// MarkBusy while holding the Scaler's lock, so BusyFunc must not block and
// must not call Scale or RemoveIdle.
type BusyFunc func(name string, at time.Time)

// DrainKind says why the pool drains. The zero DrainKind is DrainNone.
type DrainKind int

// Drain kinds.
const (
	// DrainNone means no drain has started.
	DrainNone DrainKind = iota
	// DrainSignal is the drain after SIGTERM or SIGINT (R20, R91, R92).
	DrainSignal
	// DrainError is the drain after a permanent session, scale set or
	// AcquireJobs error (R51, R53, R55).
	DrainError
)

func (k DrainKind) String() string {
	switch k {
	case DrainNone:
		return "none"
	case DrainSignal:
		return "signal"
	case DrainError:
		return "error"
	default:
		return "DrainKind(" + strconv.Itoa(int(k)) + ")"
	}
}

// Table is the pool's runner table: every runner's state, token, verdict and
// job, plus the pool-wide drain, finalize and start-failure state. All
// transitions happen under one mutex after checking the current state, so
// concurrent callers (Scale, the job check server, judges, timers, watchers)
// agree on one outcome. The table never calls Docker or GitHub, and it calls
// the BusyFunc only after releasing the mutex.
//
// A job check token is registered with the SecretRegistry while the table
// holds it, from SetToken until it is revoked or the row is removed, so logs
// never show it (R31).
//
// It is safe for concurrent use.
type Table struct {
	onBusy  BusyFunc
	secrets SecretRegistry

	mu sync.Mutex
	// jobCheck is the job check timer's BusyFunc (R81), installed once by
	// the JobJudge bound to the Scaler that uses this table
	// (setJobCheckTimer); nil until then.
	jobCheck         BusyFunc
	rows             map[string]*row
	seq              uint64
	drain            DrainKind
	finalizing       bool
	finalizeDeadline time.Time
	start            StartStatus
}

// NewTable returns an empty table. onBusy may be nil; it is called next to
// the job check timer, which New installs when given a JobJudge, and is
// meant for observers. secrets is called
// with the table's mutex held, so it must not call the table. A table used
// by a Scaler must get the same registry as Config.Secrets (New checks
// this); a nil secrets leaves the token unregistered, so New rejects it.
func NewTable(onBusy BusyFunc, secrets SecretRegistry) *Table {
	if onBusy == nil {
		onBusy = func(string, time.Time) {}
	}
	if secrets == nil {
		secrets = noSecrets{}
	}
	return &Table{onBusy: onBusy, secrets: secrets, rows: make(map[string]*row)}
}

// Add puts a new runner in StateStarting. It has no token until SetToken.
// It returns ErrDuplicateName when the name is in the table.
func (t *Table) Add(name string, at time.Time) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if _, ok := t.rows[name]; ok {
		return ErrDuplicateName
	}
	t.seq++
	t.rows[name] = &row{name: name, state: StateStarting, seq: t.seq, createdAt: at}
	return nil
}

// SetToken gives a starting runner its job check token and registers it as
// a secret until it is revoked. It reports false, setting nothing, when the
// runner is unknown or its removal has begun.
func (t *Table) SetToken(name string, token gate.Token) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	r, ok := t.rows[name]
	if !ok || r.ending() {
		return false
	}
	t.clearTokenLocked(r)
	r.token = token
	if v := token.Reveal(); v != "" {
		t.secrets.Register(v)
	}
	return true
}

// clearTokenLocked makes r's token match nothing and unregisters it.
func (t *Table) clearTokenLocked(r *row) {
	if v := r.token.Reveal(); v != "" {
		t.secrets.Unregister(v)
	}
	r.token = gate.Token{}
}

// SetRunnerID records the GitHub runner ID from the JIT config. It reports
// whether the runner is in the table.
func (t *Table) SetRunnerID(name string, id int64) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	r, ok := t.rows[name]
	if !ok {
		return false
	}
	r.id = id
	return true
}

// MarkStarted moves a starting runner to idle once its containers run. A
// runner a message already moved to busy or done stays there. It reports
// false when the runner is unknown, its removal has begun, or finalize has
// begun, in which case the start path must clean up what it made and not
// watch the runner (M3-7, M5-8). Checking the finalize mark here, under the
// mutex, leaves no gap between the check and the transition.
func (t *Table) MarkStarted(name string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	r, ok := t.rows[name]
	if !ok || r.ending() || t.finalizing {
		return false
	}
	if r.state == StateStarting {
		r.state = StateIdle
	}
	return true
}

// MarkBusy records that the runner took a job. Only the first call records
// the time, moves a starting or idle runner to busy, and calls the
// BusyFunc; later calls, calls for done runners, and calls for runners whose
// removal has begun do nothing. Once finalize has begun the first call still
// records the time and the state but does not call the BusyFunc (M5-8). It
// reports whether this call was the first.
func (t *Table) MarkBusy(name string, at time.Time) bool {
	t.mu.Lock()
	first, notify := t.markBusyLocked(name, at)
	hook := t.jobCheck
	t.mu.Unlock()
	if notify {
		t.notifyBusy(hook, name, at)
	}
	return first
}

// MarkBusyUnjudged is MarkBusy for what reconciliation discovers: a
// Runner.Worker process (R83) or the repository runner list saying busy
// (R103). Under the same lock as the transition it first checks that the
// runner has no allow verdict and is not done, so a discovery that raced
// with an allow or a JobCompleted changes nothing. It reports whether this
// call was the first busy mark.
func (t *Table) MarkBusyUnjudged(name string, at time.Time) bool {
	t.mu.Lock()
	r, ok := t.rows[name]
	if !ok || r.verdict == VerdictAllow || r.state == StateDone {
		t.mu.Unlock()
		return false
	}
	first, notify := t.markBusyLocked(name, at)
	hook := t.jobCheck
	t.mu.Unlock()
	if notify {
		t.notifyBusy(hook, name, at)
	}
	return first
}

// setJobCheckTimer installs the job check timer's BusyFunc. It fails when
// one is installed already, so one table never serves two judges.
func (t *Table) setJobCheckTimer(f BusyFunc) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.jobCheck != nil {
		return errors.New("table already has a job check timer")
	}
	t.jobCheck = f
	return nil
}

// notifyBusy calls the BusyFunc and the job check timer hook, if any. The
// caller read hook under the mutex and has released it.
func (t *Table) notifyBusy(hook BusyFunc, name string, at time.Time) {
	t.onBusy(name, at)
	if hook != nil {
		hook(name, at)
	}
}

// markBusyLocked does MarkBusy's transition. first reports whether this call
// was the first; notify whether the caller must call the BusyFunc after
// releasing the mutex, which is never once finalize has begun.
func (t *Table) markBusyLocked(name string, at time.Time) (first, notify bool) {
	r, ok := t.rows[name]
	if !ok || !r.busyAt.IsZero() {
		return false, false
	}
	if r.state != StateStarting && r.state != StateIdle && r.state != StateBusy {
		return false, false
	}
	r.busyAt = at
	r.state = StateBusy
	return true, !t.finalizing
}

// Authenticate checks a hook's token in constant time. On a match it marks
// the runner busy as MarkBusy does (the first matching call is a busy
// source, R81; no BusyFunc once finalize has begun) and returns the verdict.
// On a mismatch, an unknown name, or a revoked token it returns false and
// records nothing (R14, R40).
func (t *Table) Authenticate(name, presented string, at time.Time) (Verdict, bool) {
	t.mu.Lock()
	r, ok := t.rows[name]
	if !ok || !r.token.Equal(presented) {
		t.mu.Unlock()
		return VerdictPending, false
	}
	_, notify := t.markBusyLocked(name, at)
	v := r.verdict
	hook := t.jobCheck
	t.mu.Unlock()
	if notify {
		t.notifyBusy(hook, name, at)
	}
	return v, true
}

// JobStartedResult says what RecordJobStarted did.
type JobStartedResult int

// RecordJobStarted outcomes.
const (
	// JobStartedUnknown means no runner has the name.
	JobStartedUnknown JobStartedResult = iota + 1
	// JobStartedIgnored means the runner's removal has begun.
	JobStartedIgnored
	// JobStartedRecorded means the runner is live and the job is recorded
	// (or was already, with the same job ID). The caller marks it busy and
	// starts judging.
	JobStartedRecorded
	// JobStartedOtherJob means a live runner already recorded a different
	// job ID; the first job is kept.
	JobStartedOtherJob
	// JobStartedDoneSame means a done runner got JobStarted for the job it
	// completed; it stays done (R102).
	JobStartedDoneSame
	// JobStartedDoneOther means a done runner got JobStarted for another job.
	// Its token is revoked; the caller must reject the runner (R102).
	JobStartedDoneOther
)

// RecordJobStarted records the first JobStarted's job for the runner. When a
// done runner gets another job's JobStarted it revokes the runner's token in
// the same step, so the hook of that job is refused from then on (R102).
func (t *Table) RecordJobStarted(name string, job Job) JobStartedResult {
	t.mu.Lock()
	defer t.mu.Unlock()
	r, ok := t.rows[name]
	switch {
	case !ok:
		return JobStartedUnknown
	case r.ending():
		return JobStartedIgnored
	case r.state == StateDone:
		if job.ID == r.completedJobID {
			return JobStartedDoneSame
		}
		t.clearTokenLocked(r)
		return JobStartedDoneOther
	case r.job != nil && r.job.ID != job.ID:
		return JobStartedOtherJob
	}
	if r.job == nil {
		j := job
		r.job = &j
	}
	return JobStartedRecorded
}

// JobCompletedResult says what RecordJobCompleted did.
type JobCompletedResult int

// RecordJobCompleted outcomes.
const (
	// JobCompletedUnknown means no runner has the name.
	JobCompletedUnknown JobCompletedResult = iota + 1
	// JobCompletedIgnored means the runner's removal has begun.
	JobCompletedIgnored
	// JobCompletedOtherJob means the runner's JobStarted named another job;
	// nothing changed (R102).
	JobCompletedOtherJob
	// JobCompletedDone means the runner moved to done and the time was
	// recorded.
	JobCompletedDone
	// JobCompletedAlreadyDone means the runner was done already; the first
	// time is kept (R101).
	JobCompletedAlreadyDone
)

// RecordJobCompleted moves the runner to done and records the first
// JobCompleted time, unless its JobStarted named a different job (R102).
func (t *Table) RecordJobCompleted(name, jobID string, at time.Time) JobCompletedResult {
	t.mu.Lock()
	defer t.mu.Unlock()
	r, ok := t.rows[name]
	switch {
	case !ok:
		return JobCompletedUnknown
	case r.ending():
		return JobCompletedIgnored
	case r.state == StateDone:
		return JobCompletedAlreadyDone
	case r.job != nil && r.job.ID != jobID:
		return JobCompletedOtherJob
	}
	r.state = StateDone
	r.completedJobID = jobID
	r.completedAt = at
	return JobCompletedDone
}

// BeginJudging reports true to exactly one caller per runner, and only while
// the runner is live, so a redelivered JobStarted never starts a second
// judge.
func (t *Table) BeginJudging(name string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	r, ok := t.rows[name]
	if !ok || r.judging || !r.live() {
		return false
	}
	r.judging = true
	return true
}

// Allow records an allow verdict. It succeeds only from pending and only
// while the runner is idle or busy, so a runner whose removal has begun
// keeps answering pending.
func (t *Table) Allow(name string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	r, ok := t.rows[name]
	if !ok || r.verdict != VerdictPending || (r.state != StateIdle && r.state != StateBusy) {
		return false
	}
	r.verdict = VerdictAllow
	return true
}

// Deny records a deny verdict. It succeeds only from pending, in any state;
// the deny path records it as soon as it wins the runner's exit, so the
// hook can kill the job's Worker (R11).
func (t *Table) Deny(name string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	r, ok := t.rows[name]
	if !ok || r.verdict != VerdictPending {
		return false
	}
	r.verdict = VerdictDeny
	return true
}

// BeginExit moves the runner to exiting. It reports true to exactly one
// caller, who then owns removing the runner's resources and registration.
// The token stays valid, so the hook can read the deny the deny path then
// records and kill the job's Worker (M4-2); callers that do not need that
// use BeginExitAndRevoke.
func (t *Table) BeginExit(name string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	r, ok := t.rows[name]
	if !ok || r.ending() {
		return false
	}
	r.state = StateExiting
	return true
}

// BeginExitUnlessAllowed is BeginExit for the job check timer (R81): it wins
// only while the runner has no allow verdict, checked under the same lock as
// the transition, so an allow that lands just before the timer fires keeps
// the runner, and one that lands just after is refused (Allow needs idle or
// busy). The token stays valid, as with BeginExit.
func (t *Table) BeginExitUnlessAllowed(name string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	r, ok := t.rows[name]
	if !ok || r.ending() || r.verdict == VerdictAllow {
		return false
	}
	r.state = StateExiting
	return true
}

// BeginExitAndRevoke is BeginExit that also revokes the token in the same
// step under the mutex, so no Authenticate succeeds once removal has begun
// (R14, R38). It reports true to exactly one caller, as BeginExit does; a
// caller that loses revokes nothing.
func (t *Table) BeginExitAndRevoke(name string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	r, ok := t.rows[name]
	if !ok || r.ending() {
		return false
	}
	r.state = StateExiting
	t.clearTokenLocked(r)
	return true
}

// IdleRemovable reports whether scale-down may still pick the runner: it is
// idle and its removal was not given up (R37, R94). Scale-down asks right
// before each RemoveRunner, because the runner may have taken a job since
// the candidates were listed.
func (t *Table) IdleRemovable(name string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	r, ok := t.rows[name]
	return ok && r.idleRemovable()
}

// BeginExitIfIdle is BeginExitAndRevoke for scale-down and the error
// drain's container removal (R37, R53): it wins only while the runner is
// idle and its removal was not given up, so a runner that took a job after
// RemoveRunner was called is never removed by them.
func (t *Table) BeginExitIfIdle(name string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	r, ok := t.rows[name]
	if !ok || !r.idleRemovable() {
		return false
	}
	r.state = StateExiting
	t.clearTokenLocked(r)
	return true
}

// AbandonExit records that the owner of an exiting runner stopped removing
// it before every resource was confirmed gone, and why (AbandonReason):
// Containers, the network and the registration may remain. The row stays
// exiting, counted against max (R3), until ClaimAbandoned hands it to a new
// owner, or for good when reason is AbandonPermanent. Only the owner may
// call it, the caller that won BeginExit or ClaimAbandoned; a caller that
// lost must not, or a second owner could claim the row while the first is
// still removing it.
//
// A permanent mark stays: a later call does not replace it, so a target that
// failed permanently is never retried (R42). It reports whether the mark is
// set as asked; only exiting runners take one, and reason must not be
// AbandonNone.
func (t *Table) AbandonExit(name string, reason AbandonReason) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	r, ok := t.rows[name]
	if !ok || !r.ending() || reason == AbandonNone {
		return false
	}
	if r.abandonReason == AbandonPermanent {
		return reason == AbandonPermanent
	}
	r.abandonReason = reason
	return true
}

// ClaimAbandoned reports true to exactly one caller per AbandonExit and
// clears the mark; that caller, normally reconciliation (M3-8), then owns
// removing the runner's resources and registration as the BeginExit winner
// did. It never hands over a row abandoned with AbandonPermanent (R42).
// Finalize (M5-8) does not need a claim: it removes every row left.
func (t *Table) ClaimAbandoned(name string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	r, ok := t.rows[name]
	if !ok || r.abandonReason == AbandonNone || r.abandonReason == AbandonPermanent {
		return false
	}
	r.abandonReason = AbandonNone
	return true
}

// MarkRemoved records that the runner's resources are gone: it revokes the
// token (R38) and deletes the row, so the runner counts toward nothing and
// every later call for the name acts as for a name never added (an
// authentication fails, a message is for an unknown runner). It reports
// whether the runner was in the table.
func (t *Table) MarkRemoved(name string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	r, ok := t.rows[name]
	if !ok {
		return false
	}
	t.clearTokenLocked(r)
	delete(t.rows, name)
	return true
}

// RevokeToken makes the runner's token match nothing from now on and
// unregisters it as a secret (R14, R38).
func (t *Table) RevokeToken(name string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if r, ok := t.rows[name]; ok {
		t.clearTokenLocked(r)
	}
}

// GiveUpRemoval marks the runner so scale-down never picks it again (R94).
func (t *Table) GiveUpRemoval(name string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	r, ok := t.rows[name]
	if !ok {
		return false
	}
	r.removeGivenUp = true
	return true
}

// BeginDoneCleanup reports true to exactly one caller per done runner, for
// R101's one-time removal. It does not look at the time; the caller checks
// that 5 minutes have passed since RunnerInfo.CompletedAt.
func (t *Table) BeginDoneCleanup(name string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	r, ok := t.rows[name]
	if !ok || r.state != StateDone || r.doneCleanupStarted {
		return false
	}
	r.doneCleanupStarted = true
	return true
}

// Get returns a copy of the runner's row.
func (t *Table) Get(name string) (RunnerInfo, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	r, ok := t.rows[name]
	if !ok {
		return RunnerInfo{}, false
	}
	return r.info(), true
}

// List returns copies of all rows, oldest first.
func (t *Table) List() []RunnerInfo {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.sortedLocked(func(*row) bool { return true })
}

// RemovalCandidates returns the idle runners scale-down may remove, oldest
// first: idle only (R37) and never one whose removal was given up (R94).
func (t *Table) RemovalCandidates() []RunnerInfo {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.sortedLocked((*row).idleRemovable)
}

func (t *Table) sortedLocked(keep func(*row) bool) []RunnerInfo {
	rows := make([]*row, 0, len(t.rows))
	for _, r := range t.rows {
		if keep(r) {
			rows = append(rows, r)
		}
	}
	slices.SortFunc(rows, func(a, b *row) int {
		if c := a.createdAt.Compare(b.createdAt); c != 0 {
			return c
		}
		return cmp.Compare(a.seq, b.seq)
	})
	out := make([]RunnerInfo, len(rows))
	for i, r := range rows {
		out[i] = r.info()
	}
	return out
}

// Counts is the number of runners in each state. Removed runners are not in
// the table, so they are not counted.
type Counts struct {
	Starting, Idle, Busy, Done, Exiting int
}

// Live is the existing runners of R1, R2 and R37; done runners are not
// among them.
func (c Counts) Live() int { return c.Starting + c.Idle + c.Busy }

// All is the runners that may hold a container, the number R3 bounds.
func (c Counts) All() int { return c.Live() + c.Done + c.Exiting }

// Counts returns the number of runners in each state.
func (t *Table) Counts() Counts {
	t.mu.Lock()
	defer t.mu.Unlock()
	var c Counts
	for _, r := range t.rows {
		switch r.state {
		case StateStarting:
			c.Starting++
		case StateIdle:
			c.Idle++
		case StateBusy:
			c.Busy++
		case StateDone:
			c.Done++
		case StateExiting:
			c.Exiting++
		}
	}
	return c
}

// SetDraining starts a drain of the given kind: no new runners and every
// idle runner is removed. There is no way back. Only the first call sets
// the kind, so a permanent error during a signal drain keeps it a signal
// drain (M5-6). It reports whether this call started the drain; kind must
// not be DrainNone.
func (t *Table) SetDraining(kind DrainKind) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if kind == DrainNone || t.drain != DrainNone {
		return false
	}
	t.drain = kind
	return true
}

// Draining reports whether a drain or finalize has started.
func (t *Table) Draining() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.drain != DrainNone || t.finalizing
}

// Drain returns the kind of the drain; DrainNone when none was started with
// SetDraining, even while finalizing.
func (t *Table) Drain() DrainKind {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.drain
}

// BeginFinalize sets the finalize mark and the end of the finalize budget,
// measured on the scaler's Clock (M5-8). Start paths check the mark between
// steps and clean up what they made; cleanup calls end by the deadline. It
// implies draining. Only the first call sets the deadline.
//
// The daemon calls Scaler.BeginFinalize, which also stops deferred cleanup.
func (t *Table) BeginFinalize(deadline time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.finalizing {
		return
	}
	t.finalizing = true
	t.finalizeDeadline = deadline
}

// Finalizing reports whether the finalize mark is set.
func (t *Table) Finalizing() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.finalizing
}

// FinalizeDeadline returns the end of the finalize budget. ok is false
// before finalize begins.
func (t *Table) FinalizeDeadline() (deadline time.Time, ok bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.finalizeDeadline, t.finalizing
}

// StartStatus is the pool's runner start record (R61, R86).
type StartStatus struct {
	// Failures counts start failures since the last success.
	Failures int
	// LastError and LastErrorAt describe the latest failure. The Scaler
	// redacts LastError before storing it, while the runner's JIT config and
	// token are still registered secrets (R31); it keeps only the text.
	LastError   error
	LastErrorAt time.Time
	// NextStartAt is the earliest time a start may begin; zero means now.
	NextStartAt time.Time
}

// RecordStartFailure counts one start failure, remembers err as given, and
// sets the earliest next start. It returns the new failure count. The
// caller redacts err first (R31).
func (t *Table) RecordStartFailure(err error, at, nextStartAt time.Time) int {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.start.Failures++
	t.start.LastError = err
	t.start.LastErrorAt = at
	t.start.NextStartAt = nextStartAt
	return t.start.Failures
}

// RecordStartSuccess resets the failure count and the backoff (R61). The
// last error stays for status (R86).
func (t *Table) RecordStartSuccess() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.start.Failures = 0
	t.start.NextStartAt = time.Time{}
}

// StartStatus returns the pool's start record.
func (t *Table) StartStatus() StartStatus {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.start
}
