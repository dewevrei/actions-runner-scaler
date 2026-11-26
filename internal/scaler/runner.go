// Package scaler keeps the server's runner table, shared by every target, and
// turns each target's scale set messages into runner starts and removals.
//
// Lock order, the same everywhere (0002 PLAN M14): a Scaler's scaleMu,
// then the Placer's lock, then Table.mu. Table.mu is never held across a call
// out of the table, and nothing called under the Placer's lock takes a
// scaleMu or calls a Placer method other than Kick.
//
// The package talks to GitHub and Docker only through the small interfaces
// it defines (ScaleSetAPI, JobAcquirer, Containers, Judge); the
// daemon wires the real clients in. It never imports the github or docker
// packages.
package scaler

import (
	"strconv"
	"time"

	"github.com/dewevrei/actions-runner-scaler/internal/gate"
)

// State is where a runner is in its life. The zero State is not valid.
type State int

// Runner states. A runner moves forward only:
// starting → idle → busy → done → exiting, where any state before exiting
// may skip ahead to exiting, and starting may go straight to busy or done
// when a message arrives before the start path finishes. Once its resources
// are gone the runner leaves the table (Table.MarkRemoved): there is no
// "removed" state, the row is deleted, so a lookup of its name fails like
// that of a name never added.
const (
	// StateStarting means the start path owns the runner: its JIT config or
	// containers may not exist yet.
	StateStarting State = iota + 1
	// StateIdle means the containers run and no job has been seen.
	StateIdle
	// StateBusy means a job was seen (R81's sources).
	StateBusy
	// StateDone means JobCompleted arrived and the container is still there
	// (SPEC Design "끝난 runner"). It counts toward max (R3) but not toward the
	// live runners of R1, R2 and R37.
	StateDone
	// StateExiting means one caller won BeginExit and is removing the
	// runner's resources and registration. It counts toward max (R3) until
	// the resources are gone.
	StateExiting
)

func (s State) String() string {
	switch s {
	case StateStarting:
		return "starting"
	case StateIdle:
		return "idle"
	case StateBusy:
		return "busy"
	case StateDone:
		return "done"
	case StateExiting:
		return "exiting"
	default:
		return "State(" + strconv.Itoa(int(s)) + ")"
	}
}

// Verdict is the job check result the hook reads. The zero Verdict is
// VerdictPending.
type Verdict int

// Verdicts. A verdict leaves VerdictPending at most once.
const (
	VerdictPending Verdict = iota
	VerdictAllow
	VerdictDeny
)

// String returns the value used in the GET /v1/verdict response.
func (v Verdict) String() string {
	switch v {
	case VerdictPending:
		return "pending"
	case VerdictAllow:
		return "allow"
	case VerdictDeny:
		return "deny"
	default:
		return "Verdict(" + strconv.Itoa(int(v)) + ")"
	}
}

// Job is what a JobStarted message says about the job a runner took. The
// verdict never reads it (0002 SPEC R123).
type Job struct {
	// ID is JobMessageBase.JobID, compared for R102.
	ID string
	// WorkflowRunID, Owner, Repository and Event are for logs only (0002
	// SPEC Design 판정 "정책").
	WorkflowRunID int64
	Owner         string
	Repository    string
	Event         string
}

// RunnerInfo is a copy of one row of the table. Changing it changes nothing
// in the table. It never holds the token.
type RunnerInfo struct {
	Name string
	// Target is the key of the runner's target (TargetConfig.Key, 0002 PLAN
	// M11-1); it never changes.
	Target string
	// ID is the GitHub runner ID from the JIT config; zero until the JIT
	// config is generated.
	ID      int64
	State   State
	Verdict Verdict
	// CreatedAt is when the start path added the row.
	CreatedAt time.Time
	// BusyAt is the first time the runner was marked busy; zero if never.
	BusyAt time.Time
	// Job is the first JobStarted's job; nil until one arrives.
	Job *Job
	// CompletedJobID and CompletedAt come from the first JobCompleted that
	// moved the runner to done; CompletedAt is zero until then (R101).
	CompletedJobID string
	CompletedAt    time.Time
	// RemoveGivenUp is set when scale-down's RemoveRunner failed
	// permanently, so scale-down never picks the runner again (R94).
	RemoveGivenUp bool
	// DoneCleanupStarted is set once BeginDoneCleanup has been won (R101).
	DoneCleanupStarted bool
	// Abandoned is set on an exiting runner whose removal owner stopped
	// before all its resources were confirmed gone (Table.AbandonExit), until
	// reconciliation takes it over with Table.ClaimAbandoned (M3-8).
	// AbandonReason says why; it is AbandonNone exactly when Abandoned is
	// false.
	Abandoned     bool
	AbandonReason AbandonReason
}

// AbandonReason says why the owner of an exiting runner stopped removing it
// (Table.AbandonExit). The zero AbandonReason is AbandonNone.
type AbandonReason int

// Abandon reasons. Every reason but AbandonPermanent leaves the row for
// Table.ClaimAbandoned.
const (
	// AbandonNone means the row is not abandoned.
	AbandonNone AbandonReason = iota
	// AbandonPermanent means a removal failed with a permanent error. The row
	// is never claimed, so the target is not retried (R42); finalize and the
	// next start's R16 and R17 still remove what is left.
	AbandonPermanent
	// AbandonRetriesExhausted means transient failures used up the retries
	// (maxRemoveRetries).
	AbandonRetriesExhausted
	// AbandonNoBackoff means a transient failure could not be retried
	// because no backoff could be made.
	AbandonNoBackoff
	// AbandonNoBudget means the finalize budget ran out before the call.
	AbandonNoBudget
	// AbandonFinalize means finalize began, which removes every row left
	// (M5-8).
	AbandonFinalize
	// AbandonErrorDrain means RemoveRunner failed during the error drain,
	// which then removed at most the runner container (R53); the BuildKit
	// container, the network and the registration may remain, and the runner
	// container too when its removal failed with a transient error.
	AbandonErrorDrain
)

func (r AbandonReason) String() string {
	switch r {
	case AbandonNone:
		return "none"
	case AbandonPermanent:
		return "permanent error"
	case AbandonRetriesExhausted:
		return "retries exhausted"
	case AbandonNoBackoff:
		return "no backoff"
	case AbandonNoBudget:
		return "finalize budget spent"
	case AbandonFinalize:
		return "finalize"
	case AbandonErrorDrain:
		return "error drain"
	default:
		return "AbandonReason(" + strconv.Itoa(int(r)) + ")"
	}
}

// row is the table's mutable record of one runner. Only Table touches it,
// and only while holding the table's mutex.
type row struct {
	name    string
	target  string
	id      int64
	state   State
	verdict Verdict
	// token is cleared when revoked; the zero Token matches nothing (R14).
	token gate.Token
	// seq orders rows created at the same instant, keeping "oldest first"
	// deterministic.
	seq                uint64
	createdAt          time.Time
	busyAt             time.Time
	job                *Job
	completedJobID     string
	completedAt        time.Time
	removeGivenUp      bool
	doneCleanupStarted bool
	// abandonReason is AbandonNone unless the row is abandoned.
	abandonReason AbandonReason
}

func (r *row) info() RunnerInfo {
	info := RunnerInfo{
		Name:               r.name,
		Target:             r.target,
		ID:                 r.id,
		State:              r.state,
		Verdict:            r.verdict,
		CreatedAt:          r.createdAt,
		BusyAt:             r.busyAt,
		CompletedJobID:     r.completedJobID,
		CompletedAt:        r.completedAt,
		RemoveGivenUp:      r.removeGivenUp,
		DoneCleanupStarted: r.doneCleanupStarted,
		Abandoned:          r.abandonReason != AbandonNone,
		AbandonReason:      r.abandonReason,
	}
	if r.job != nil {
		j := *r.job
		info.Job = &j
	}
	return info
}

// live reports whether the row counts as an existing runner for R1, R2 and
// R37.
func (r *row) live() bool {
	return r.state == StateStarting || r.state == StateIdle || r.state == StateBusy
}

// idleRemovable reports whether scale-down may remove the row (R37, R94).
func (r *row) idleRemovable() bool {
	return r.state == StateIdle && !r.removeGivenUp
}

// ending reports whether removal has begun.
func (r *row) ending() bool {
	return r.state == StateExiting
}
