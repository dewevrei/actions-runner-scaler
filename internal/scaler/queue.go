package scaler

import (
	"cmp"
	"slices"
	"time"

	"github.com/actions/scaleset"
)

// forgetAfter is how long past its QueueTime an entry stays: GitHub cancels
// a self-hosted job after 24 h in the queue, plus 1 h (0002 SPEC R227).
const forgetAfter = 25 * time.Hour

// WaitingList holds the jobs GitHub offered or assigned to running targets
// that no runner has started yet (0002 SPEC R206). It is a plain data
// structure: no lock, no call, no clock. Methods that need the time take it,
// and the owner serializes every call. It lives in memory only (R211).
type WaitingList struct {
	entries []*Entry                // insertion order
	stats   map[string]*targetStats // per target key
	seq     uint64                  // entries inserted so far
}

// Entry is one job of the list (0002 SPEC R206). Its fields are read and
// changed only inside this package.
type Entry struct {
	target      string    // target key
	order       int       // index of the target in config order (R184)
	requestID   int64     // RunnerRequestID; 0 until a message with a non-zero value matches
	runID       int64     // WorkflowRunID
	jobName     string    // JobDisplayName
	assigned    bool      // after JobAssigned or a successful AcquireJobs
	jobID       string    // JobID of the last assignment or new offer
	queueTime   time.Time // message QueueTime; firstSeen when the message's is zero
	firstSeen   time.Time
	lastHeard   time.Time // last JobAvailable, JobAssigned or take-back applied (R206, R209)
	takenBack   bool      // R209
	assignedSeq uint64    // the target's statsSeq when it became assigned, plus 1 (R226)
	seq         uint64    // insertion number, the last tie-breaker of R184
}

// targetStats is one target's state between messages with Statistics.
type targetStats struct {
	statsSeq    uint64 // messages with Statistics applied
	prevDeficit int    // deficit of the last of them, 0 before the first (R226)
	unlisted    int    // R219 value, kept until the next of them
}

// Events is what one Apply changed, for logging. Each element is a copy of
// the entry right after its change.
type Events struct {
	Offered   []Entry // JobAvailable applied (R206, R207)
	Assigned  []Entry // JobAssigned applied (R206, R207)
	TakenBack []Entry // take-back mark set (R209)
	Removed   []Entry // removed by JobStarted or JobCompleted (R208)
}

// StatsResult is what one Statistics call found.
type StatsResult struct {
	Dropped         []Entry // removed by R226, as they were
	Unlisted        int     // the R219 value now in force
	UnlistedChanged bool    // Unlisted differs from the value before the call
	Warn            bool    // R221: TotalAvailableJobs exceeds Known
	Known           int     // the target's unassigned or taken-back entries (R221)
}

// NewWaitingList returns an empty list (0002 SPEC R211).
func NewWaitingList() *WaitingList {
	return &WaitingList{stats: make(map[string]*targetStats)}
}

// Entry state rules.

// holdsAssignment reports whether GitHub should count e as assigned and not
// yet started: R219 and R226 count these.
func (e *Entry) holdsAssignment() bool { return e.assigned && !e.takenBack }

// unclaimed reports whether e is a job GitHub counts as available (R221).
func (e *Entry) unclaimed() bool { return !e.assigned || e.takenBack }

// assign applies a JobAssigned (0002 SPEC R207). An assignment that follows
// none, or follows a take-back, starts a new R226 age at seq.
func (e *Entry) assign(jobID string, seq uint64) {
	if !e.holdsAssignment() {
		e.assignedSeq = seq
	}
	e.assigned, e.takenBack, e.jobID = true, false, jobID
}

// reoffer applies a JobAvailable to a known entry (0002 SPEC R207): after a
// take-back or under a new JobID the job is unassigned again; a plain
// redelivery keeps an AcquireJobs assignment (R214).
func (e *Entry) reoffer(jobID string) {
	if e.takenBack || (jobID != "" && jobID != e.jobID) {
		e.assigned, e.takenBack, e.jobID = false, false, jobID
	}
}

// withdraw sets the take-back mark when jobID is e's latest assignment, so
// a take-back that arrives beside the next assignment changes nothing
// (0002 SPEC R209).
func (e *Entry) withdraw(jobID string) bool {
	if !e.assigned || jobID != e.jobID {
		return false
	}
	e.takenBack = true
	return true
}

// expired reports whether e waited past forgetAfter at now (0002 SPEC R227).
func (e *Entry) expired(now time.Time) bool { return now.Sub(e.queueTime) > forgetAfter }

// placementOrder is the order of 0002 SPEC R184: QueueTime, first seen,
// config order, RunnerRequestID, run ID, job name, then insertion.
func placementOrder(a, b *Entry) int {
	return cmp.Or(
		a.queueTime.Compare(b.queueTime),
		a.firstSeen.Compare(b.firstSeen),
		cmp.Compare(a.order, b.order),
		cmp.Compare(a.requestID, b.requestID),
		cmp.Compare(a.runID, b.runID),
		cmp.Compare(a.jobName, b.jobName),
		cmp.Compare(a.seq, b.seq),
	)
}

// staleOrder orders R226 drop candidates: earliest lastHeard, then R184.
func staleOrder(a, b *Entry) int {
	return cmp.Or(a.lastHeard.Compare(b.lastHeard), placementOrder(a, b))
}

// Matching messages to entries.

// jobIdentity is what a job message says about which job it is.
type jobIdentity struct {
	requestID int64
	runID     int64
	jobName   string
}

func identityOf(b scaleset.JobMessageBase) jobIdentity {
	return jobIdentity{requestID: b.RunnerRequestID, runID: b.WorkflowRunID, jobName: b.JobDisplayName}
}

// sameRequest: both RunnerRequestIDs are set and equal (0002 SPEC R206).
func (id jobIdentity) sameRequest(e *Entry) bool {
	return id.requestID != 0 && e.requestID == id.requestID
}

// sameRun: one RunnerRequestID is 0, and run ID and job name agree (R206).
func (id jobIdentity) sameRun(e *Entry) bool {
	return (id.requestID == 0 || e.requestID == 0) && e.runID == id.runID && e.jobName == id.jobName
}

// lookup returns target's entry for id, or nil. A RunnerRequestID match wins
// over a run and name match; an entry without a RunnerRequestID takes the
// message's (0002 SPEC R206).
func (l *WaitingList) lookup(target string, id jobIdentity) *Entry {
	e := l.first(target, id.sameRequest)
	if e == nil {
		e = l.first(target, id.sameRun)
	}
	if e != nil && e.requestID == 0 {
		e.requestID = id.requestID
	}
	return e
}

// first returns target's first entry in insertion order that ok accepts.
func (l *WaitingList) first(target string, ok func(*Entry) bool) *Entry {
	for _, e := range l.entries {
		if e.target == target && ok(e) {
			return e
		}
	}
	return nil
}

// Applying job messages.

// effect is what applying one job message did.
type effect int

const (
	effectNone effect = iota
	effectOffered
	effectAssigned
	effectTakenBack
	effectRemoved
)

// record adds a copy of e to the list of ev that fx names.
func (ev *Events) record(fx effect, e *Entry) {
	switch fx {
	case effectOffered:
		ev.Offered = append(ev.Offered, *e)
	case effectAssigned:
		ev.Assigned = append(ev.Assigned, *e)
	case effectTakenBack:
		ev.TakenBack = append(ev.TakenBack, *e)
	case effectRemoved:
		ev.Removed = append(ev.Removed, *e)
	}
}

// Apply applies the job messages of msg for target, whose config index is
// order, kind by kind: JobAvailable, JobAssigned, JobStarted, JobCompleted
// (0002 SPEC R206–R209). The caller passes only messages of a running
// target (R210).
func (l *WaitingList) Apply(target string, order int, msg *scaleset.RunnerScaleSetMessage, now time.Time) Events {
	var ev Events
	if msg == nil {
		return ev
	}
	for _, m := range msg.JobAvailableMessages {
		ev.record(l.applyOffer(target, order, m.JobMessageBase, now))
	}
	for _, m := range msg.JobAssignedMessages {
		ev.record(l.applyAssignment(target, order, m.JobMessageBase, now))
	}
	for _, m := range msg.JobStartedMessages {
		ev.record(l.applyStart(target, m.JobMessageBase))
	}
	for _, m := range msg.JobCompletedMessages {
		ev.record(l.applyCompletion(target, m, now))
	}
	return ev
}

func (l *WaitingList) applyOffer(target string, order int, b scaleset.JobMessageBase, now time.Time) (effect, *Entry) {
	e := l.findOrInsert(target, order, b, now)
	e.reoffer(b.JobID)
	return effectOffered, e
}

func (l *WaitingList) applyAssignment(target string, order int, b scaleset.JobMessageBase, now time.Time) (effect, *Entry) {
	e := l.findOrInsert(target, order, b, now)
	e.assign(b.JobID, l.nextStatsSeq(target))
	return effectAssigned, e
}

func (l *WaitingList) applyStart(target string, b scaleset.JobMessageBase) (effect, *Entry) {
	e := l.lookup(target, identityOf(b))
	if e == nil {
		return effectNone, nil
	}
	l.Remove(e)
	return effectRemoved, e
}

// applyCompletion removes the entry, unless m is a take-back: no runner and
// no finish time (0002 SPEC R208, R209).
func (l *WaitingList) applyCompletion(target string, m *scaleset.JobCompleted, now time.Time) (effect, *Entry) {
	e := l.lookup(target, identityOf(m.JobMessageBase))
	if e == nil {
		return effectNone, nil
	}
	if m.RunnerName == "" && m.FinishTime.IsZero() {
		if !e.withdraw(m.JobID) {
			return effectNone, nil
		}
		e.lastHeard = now
		return effectTakenBack, e
	}
	l.Remove(e)
	return effectRemoved, e
}

// findOrInsert returns the entry the message is about, heard at now, and
// adds an unassigned one when there is none (0002 SPEC R206, R207).
// QueueTime and first seen of a known entry never change.
func (l *WaitingList) findOrInsert(target string, order int, b scaleset.JobMessageBase, now time.Time) *Entry {
	if e := l.lookup(target, identityOf(b)); e != nil {
		e.lastHeard = now
		return e
	}
	queued := b.QueueTime
	if queued.IsZero() {
		queued = now
	}
	l.seq++
	e := &Entry{
		target:    target,
		order:     order,
		requestID: b.RunnerRequestID,
		runID:     b.WorkflowRunID,
		jobName:   b.JobDisplayName,
		jobID:     b.JobID,
		queueTime: queued,
		firstSeen: now,
		lastHeard: now,
		seq:       l.seq,
	}
	l.entries = append(l.entries, e)
	return e
}

// Changes made by the placement.

// MarkAcquired records that AcquireJobs returned e's RunnerRequestID (0002
// SPEC R214).
func (l *WaitingList) MarkAcquired(e *Entry) {
	if !e.assigned {
		e.assignedSeq = l.nextStatsSeq(e.target)
	}
	e.assigned = true
}

// Remove removes e; it does nothing when e is not in the list.
func (l *WaitingList) Remove(e *Entry) {
	if i := slices.Index(l.entries, e); i >= 0 {
		l.entries = slices.Delete(l.entries, i, i+1)
	}
}

// DropTarget removes every entry of target (0002 SPEC R210).
func (l *WaitingList) DropTarget(target string) {
	l.removeWhere(func(e *Entry) bool { return e.target == target })
}

// Expire removes the entries queued more than 25 h before now and returns
// them (0002 SPEC R227).
func (l *WaitingList) Expire(now time.Time) []Entry {
	return l.removeWhere(func(e *Entry) bool { return e.expired(now) })
}

// removeWhere removes the entries drop accepts and returns copies of them.
func (l *WaitingList) removeWhere(drop func(*Entry) bool) []Entry {
	var gone []Entry
	l.entries = slices.DeleteFunc(l.entries, func(e *Entry) bool {
		if !drop(e) {
			return false
		}
		gone = append(gone, *e)
		return true
	})
	return gone
}

// Reconciling with Statistics.

// statsOf returns target's state, made on first use.
func (l *WaitingList) statsOf(target string) *targetStats {
	ts := l.stats[target]
	if ts == nil {
		ts = &targetStats{}
		l.stats[target] = ts
	}
	return ts
}

// nextStatsSeq is the assignedSeq of an entry assigned now: one past the
// target's last message with Statistics (0002 SPEC R226).
func (l *WaitingList) nextStatsSeq(target string) uint64 {
	if ts := l.stats[target]; ts != nil {
		return ts.statsSeq + 1
	}
	return 1
}

// Statistics reconciles target's entries with a message's Statistics,
// after Apply of that message; started is the target's number of runners
// that received their own JobStarted (0002 SPEC R224). The caller calls it
// only when Statistics is not nil, so an empty poll or a message without
// Statistics keeps the R219 value (R219, R221, R226).
func (l *WaitingList) Statistics(target string, st scaleset.RunnerScaleSetStatistic, started int) StatsResult {
	ts := l.statsOf(target)
	ts.statsSeq++
	pending := st.TotalAssignedJobs - started // assigned jobs GitHub has not seen start
	held := l.filter(func(e *Entry) bool { return e.target == target && e.holdsAssignment() })
	live := len(held)
	deficit := live - pending

	var res StatsResult
	if ts.prevDeficit > 0 && deficit > 0 {
		res.Dropped = l.dropStale(held, ts.statsSeq-1, min(ts.prevDeficit, deficit))
		live -= len(res.Dropped)
	}
	ts.prevDeficit = deficit

	unlisted := max(0, pending-live)
	res.Unlisted, res.UnlistedChanged = unlisted, unlisted != ts.unlisted
	ts.unlisted = unlisted

	res.Known = l.count(func(e *Entry) bool { return e.target == target && e.unclaimed() })
	res.Warn = st.TotalAvailableJobs > res.Known
	return res
}

// dropStale removes up to n of held that were assigned by the message with
// Statistics numbered before, in staleOrder, and returns them (0002 SPEC
// R226).
func (l *WaitingList) dropStale(held []*Entry, before uint64, n int) []Entry {
	old := slices.DeleteFunc(held, func(e *Entry) bool { return e.assignedSeq > before })
	slices.SortFunc(old, staleOrder)
	var dropped []Entry
	for _, e := range old[:min(n, len(old))] {
		l.Remove(e)
		dropped = append(dropped, *e)
	}
	return dropped
}

// Reading the list.

// Ordered returns the entries of targets active accepts, without taken-back
// entries (R225), in placement order (0002 SPEC R184). The caller may call
// MarkAcquired, Remove or DropTarget while it walks them; Remove of an entry
// already gone does nothing.
func (l *WaitingList) Ordered(active func(target string) bool) []*Entry {
	out := l.filter(func(e *Entry) bool { return !e.takenBack && active(e.target) })
	slices.SortFunc(out, placementOrder)
	return out
}

// Unlisted returns target's R219 value from its last message with
// Statistics (0002 SPEC R219).
func (l *WaitingList) Unlisted(target string) int {
	if ts := l.stats[target]; ts != nil {
		return ts.unlisted
	}
	return 0
}

// Count returns the number of target's entries, taken-back ones included.
func (l *WaitingList) Count(target string) int {
	return l.count(func(e *Entry) bool { return e.target == target })
}

// filter returns the entries keep accepts, in insertion order, in a new slice.
func (l *WaitingList) filter(keep func(*Entry) bool) []*Entry {
	var out []*Entry
	for _, e := range l.entries {
		if keep(e) {
			out = append(out, e)
		}
	}
	return out
}

func (l *WaitingList) count(keep func(*Entry) bool) int {
	n := 0
	for _, e := range l.entries {
		if keep(e) {
			n++
		}
	}
	return n
}
