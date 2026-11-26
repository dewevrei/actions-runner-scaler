// The waiting job list of 0002 SPEC R206–R211, R219, R221, R225–R227.
//
// Adapted from github.com/kunchenguid/runner-up 013cc74
// internal/coordinator/queue.go (waitLocked, takeBackLocked, keysOf,
// expireLocked), internal/coordinator/service.go (Handle) and
// internal/plan/plan.go (Rank, fifo).
// Copyright (c) 2026 Kun Chen
// MIT License

package scaler

import (
	"cmp"
	"slices"
	"time"

	"github.com/actions/scaleset"
)

// expireAfter is how long an entry waits past its QueueTime before the list
// forgets it: GitHub's 24 h queue limit plus 1 h (0002 SPEC R227).
const expireAfter = 25 * time.Hour

// entry is one job GitHub offered or assigned to a target that has not yet
// started on a runner (0002 SPEC R206).
type entry struct {
	target      string    // target key
	order       int       // index of the target in config order (R184)
	requestID   int64     // RunnerRequestID; 0 until a message with a non-zero value matches
	runID       int64     // WorkflowRunID
	jobName     string    // JobDisplayName
	assigned    bool      // true after JobAssigned or a successful AcquireJobs
	jobID       string    // JobID of the last matching message
	queueTime   time.Time // message QueueTime; firstSeen when the message's is zero
	firstSeen   time.Time
	lastHeard   time.Time // last JobAvailable, JobAssigned or take-back applied (R206, R209)
	takenBack   bool      // R209
	assignedSeq uint64    // the target's statsSeq when it became assigned, plus 1 (R226)
	seq         uint64    // insertion counter, the last tie-breaker of R184
}

type targetStats struct {
	statsSeq    uint64 // number of messages with Statistics applied for this target
	prevDeficit int    // deficit of the previous message with Statistics (R226)
	havePrev    bool
	unlisted    int // R219 value, kept until the next message with Statistics
}

// WaitingList is the server's list of jobs waiting for a runner (0002 SPEC
// R206). It holds no lock, makes no call and reads no clock: every method
// that needs the time takes it, and the owner serializes all calls.
type WaitingList struct {
	entries []*entry // insertion order
	stats   map[string]*targetStats
	seq     uint64
}

// NewWaitingList returns an empty list; it lives in memory only (0002 SPEC
// R211).
func NewWaitingList() *WaitingList {
	return &WaitingList{stats: make(map[string]*targetStats)}
}

// Events is what one Apply changed, for logging. Each element is a copy of
// the entry right after the change.
type Events struct {
	Offered   []entry // JobAvailable applied to an entry
	Assigned  []entry // JobAssigned applied to an entry
	TakenBack []entry // take-back mark set (R209)
	Removed   []entry // removed by JobStarted or JobCompleted (R208)
}

// StatsResult is what one Statistics call found.
type StatsResult struct {
	Dropped         []entry // entries removed by R226, as they were
	Unlisted        int     // the R219 value now in force
	UnlistedChanged bool    // Unlisted differs from the value before the call
	Warn            bool    // R221: TotalAvailableJobs exceeds Known
	Known           int     // unassigned or taken-back entries of the target (R221)
}

func (l *WaitingList) statsOf(target string) *targetStats {
	s, ok := l.stats[target]
	if !ok {
		s = &targetStats{}
		l.stats[target] = s
	}
	return s
}

// match finds the entry of target a message is about (0002 SPEC R206): a
// RunnerRequestID match wins over a run ID and job name match, which is
// used only when either side's RunnerRequestID is 0.
func (l *WaitingList) match(target string, base scaleset.JobMessageBase) *entry {
	var byName *entry
	for _, e := range l.entries {
		if e.target != target {
			continue
		}
		if e.requestID != 0 && base.RunnerRequestID != 0 {
			if e.requestID == base.RunnerRequestID {
				return e
			}
			continue
		}
		if byName == nil && e.runID == base.WorkflowRunID && e.jobName == base.JobDisplayName {
			byName = e
		}
	}
	if byName != nil && byName.requestID == 0 {
		byName.requestID = base.RunnerRequestID
	}
	return byName
}

// Apply applies the job messages of msg for target, whose config index is
// order: every JobAvailable, then JobAssigned, JobStarted, JobCompleted
// (0002 SPEC R206–R209). The caller applies only messages of a target in
// state running (R210).
func (l *WaitingList) Apply(target string, order int, msg *scaleset.RunnerScaleSetMessage, now time.Time) Events {
	var ev Events
	if msg == nil {
		return ev
	}
	for _, m := range msg.JobAvailableMessages {
		ev.Offered = append(ev.Offered, *l.offer(target, order, m.JobMessageBase, false, now))
	}
	for _, m := range msg.JobAssignedMessages {
		ev.Assigned = append(ev.Assigned, *l.offer(target, order, m.JobMessageBase, true, now))
	}
	for _, m := range msg.JobStartedMessages {
		if e := l.match(target, m.JobMessageBase); e != nil {
			l.Remove(e)
			ev.Removed = append(ev.Removed, *e)
		}
	}
	for _, m := range msg.JobCompletedMessages {
		e := l.match(target, m.JobMessageBase)
		if e == nil {
			continue
		}
		if m.RunnerName == "" && m.FinishTime.IsZero() {
			// Take-back (R209): only of the entry's latest assignment, so a
			// take-back sent with the next assignment in one message is ignored.
			if e.assigned && m.JobID == e.jobID {
				e.takenBack = true
				e.lastHeard = now
				ev.TakenBack = append(ev.TakenBack, *e)
			}
			continue
		}
		l.Remove(e)
		ev.Removed = append(ev.Removed, *e)
	}
	return ev
}

// offer applies one JobAvailable (assigned false) or JobAssigned (assigned
// true) and returns the entry it changed or added (0002 SPEC R206, R207).
func (l *WaitingList) offer(target string, order int, base scaleset.JobMessageBase, assigned bool, now time.Time) *entry {
	st := l.statsOf(target)
	e := l.match(target, base)
	if e == nil {
		qt := base.QueueTime
		if qt.IsZero() {
			qt = now
		}
		l.seq++
		e = &entry{
			target:    target,
			order:     order,
			requestID: base.RunnerRequestID,
			runID:     base.WorkflowRunID,
			jobName:   base.JobDisplayName,
			assigned:  assigned,
			jobID:     base.JobID,
			queueTime: qt,
			firstSeen: now,
			lastHeard: now,
			seq:       l.seq,
		}
		if assigned {
			e.assignedSeq = st.statsSeq + 1
		}
		l.entries = append(l.entries, e)
		return e
	}
	e.lastHeard = now
	if assigned {
		// A take-back ended the earlier assignment (R209), so an assignment
		// after it is new for R226.
		if !e.assigned || e.takenBack {
			e.assignedSeq = st.statsSeq + 1
		}
		e.assigned, e.jobID, e.takenBack = true, base.JobID, false
		return e
	}
	// A new offer after a take-back or under a new JobID makes the entry
	// unassigned again; a plain redelivery keeps an AcquireJobs assignment.
	if e.takenBack || (base.JobID != "" && base.JobID != e.jobID) {
		e.assigned, e.jobID, e.takenBack = false, base.JobID, false
	}
	return e
}

// MarkAcquired records that AcquireJobs returned e's RunnerRequestID (0002
// SPEC R214).
func (l *WaitingList) MarkAcquired(e *entry) {
	if !e.assigned {
		e.assignedSeq = l.statsOf(e.target).statsSeq + 1
	}
	e.assigned = true
}

// Remove removes e from the list; it does nothing when e is not in it.
func (l *WaitingList) Remove(e *entry) {
	if i := slices.Index(l.entries, e); i >= 0 {
		l.entries = slices.Delete(l.entries, i, i+1)
	}
}

// DropTarget removes every entry of target (0002 SPEC R210).
func (l *WaitingList) DropTarget(target string) {
	l.entries = slices.DeleteFunc(l.entries, func(e *entry) bool { return e.target == target })
}

// Statistics reconciles target's entries with the Statistics of a message,
// after Apply of the same message; started is the target's number of
// runners that received their own JobStarted (0002 SPEC R224). The caller
// calls it only for a message whose Statistics is not nil, so an empty poll
// or a message without Statistics keeps the R219 value (R219, R221, R226).
func (l *WaitingList) Statistics(target string, st scaleset.RunnerScaleSetStatistic, started int) StatsResult {
	ts := l.statsOf(target)
	ts.statsSeq++
	k := ts.statsSeq
	live := func(e *entry) bool { return e.target == target && e.assigned && !e.takenBack }

	n := 0
	for _, e := range l.entries {
		if live(e) {
			n++
		}
	}
	deficit := n - (st.TotalAssignedJobs - started)

	var res StatsResult
	// R226: two deficits in a row drop the smaller of the two, among entries
	// assigned already at the previous message, quietest first.
	if ts.havePrev && ts.prevDeficit > 0 && deficit > 0 {
		var old []*entry
		for _, e := range l.entries {
			if live(e) && e.assignedSeq <= k-1 {
				old = append(old, e)
			}
		}
		slices.SortFunc(old, func(a, b *entry) int {
			if c := a.lastHeard.Compare(b.lastHeard); c != 0 {
				return c
			}
			return compareEntries(a, b)
		})
		for _, e := range old[:min(ts.prevDeficit, deficit, len(old))] {
			l.Remove(e)
			res.Dropped = append(res.Dropped, *e)
		}
		n -= len(res.Dropped)
	}
	ts.prevDeficit, ts.havePrev = deficit, true

	// R219: assigned jobs GitHub counts that the list does not hold.
	prev := ts.unlisted
	ts.unlisted = max(0, st.TotalAssignedJobs-started-n)
	res.Unlisted, res.UnlistedChanged = ts.unlisted, ts.unlisted != prev

	// R221: compare available jobs with the entries not assigned.
	for _, e := range l.entries {
		if e.target == target && (!e.assigned || e.takenBack) {
			res.Known++
		}
	}
	res.Warn = st.TotalAvailableJobs > res.Known
	return res
}

// Expire removes the entries whose QueueTime is more than 25 h before now
// and returns them (0002 SPEC R227).
func (l *WaitingList) Expire(now time.Time) []entry {
	var gone []entry
	l.entries = slices.DeleteFunc(l.entries, func(e *entry) bool {
		if now.Sub(e.queueTime) > expireAfter {
			gone = append(gone, *e)
			return true
		}
		return false
	})
	return gone
}

// Ordered returns the entries of targets for which active reports true,
// without taken-back entries (R225), in placement order (0002 SPEC R184).
// The entries are heap pointers, so a caller may call MarkAcquired, Remove
// or DropTarget while it walks them; Remove of an entry already removed does
// nothing.
func (l *WaitingList) Ordered(active func(target string) bool) []*entry {
	var out []*entry
	for _, e := range l.entries {
		if !e.takenBack && active(e.target) {
			out = append(out, e)
		}
	}
	slices.SortFunc(out, compareEntries)
	return out
}

// compareEntries is the order of 0002 SPEC R184: QueueTime, first seen,
// config order, RunnerRequestID, run ID and job name, then insertion.
func compareEntries(a, b *entry) int {
	if c := a.queueTime.Compare(b.queueTime); c != 0 {
		return c
	}
	if c := a.firstSeen.Compare(b.firstSeen); c != 0 {
		return c
	}
	return cmp.Or(
		cmp.Compare(a.order, b.order),
		cmp.Compare(a.requestID, b.requestID),
		cmp.Compare(a.runID, b.runID),
		cmp.Compare(a.jobName, b.jobName),
		cmp.Compare(a.seq, b.seq),
	)
}

// Unlisted returns target's R219 value from its last message with
// Statistics (0002 SPEC R219).
func (l *WaitingList) Unlisted(target string) int {
	if s, ok := l.stats[target]; ok {
		return s.unlisted
	}
	return 0
}

// Count returns the number of target's entries, taken-back ones included.
func (l *WaitingList) Count(target string) int {
	n := 0
	for _, e := range l.entries {
		if e.target == target {
			n++
		}
	}
	return n
}
