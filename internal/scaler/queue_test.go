package scaler

import (
	"testing"
	"time"

	"github.com/actions/scaleset"
)

// wlEpoch is the fake clock's start; each test adds durations to it.
var wlEpoch = time.Date(2026, 11, 26, 9, 0, 0, 0, time.UTC)

func wlJob(req, run int64, name, jobID string, qt time.Time) scaleset.JobMessageBase {
	return scaleset.JobMessageBase{RunnerRequestID: req, WorkflowRunID: run, JobDisplayName: name, JobID: jobID, QueueTime: qt}
}

func wlAvailable(b scaleset.JobMessageBase) *scaleset.JobAvailable {
	return &scaleset.JobAvailable{JobMessageBase: b}
}

func wlAssigned(b scaleset.JobMessageBase) *scaleset.JobAssigned {
	return &scaleset.JobAssigned{JobMessageBase: b}
}

func wlStarted(b scaleset.JobMessageBase) *scaleset.JobStarted {
	return &scaleset.JobStarted{RunnerName: "runner-1", JobMessageBase: b}
}

func wlCompleted(b scaleset.JobMessageBase, runner string, finish time.Time, result string) *scaleset.JobCompleted {
	b.FinishTime = finish
	return &scaleset.JobCompleted{RunnerName: runner, Result: result, JobMessageBase: b}
}

// wlTakeBack is a JobCompleted with no runner and no finish time (R209).
func wlTakeBack(b scaleset.JobMessageBase) *scaleset.JobCompleted {
	return wlCompleted(b, "", time.Time{}, "canceled")
}

func wlMsg(parts ...any) *scaleset.RunnerScaleSetMessage {
	m := &scaleset.RunnerScaleSetMessage{}
	for _, p := range parts {
		switch p := p.(type) {
		case *scaleset.JobAvailable:
			m.JobAvailableMessages = append(m.JobAvailableMessages, p)
		case *scaleset.JobAssigned:
			m.JobAssignedMessages = append(m.JobAssignedMessages, p)
		case *scaleset.JobStarted:
			m.JobStartedMessages = append(m.JobStartedMessages, p)
		case *scaleset.JobCompleted:
			m.JobCompletedMessages = append(m.JobCompletedMessages, p)
		default:
			panic("wlMsg: unknown part")
		}
	}
	return m
}

// wlEntries returns target's entries in insertion order.
func wlEntries(l *WaitingList, target string) []*entry {
	var out []*entry
	for _, e := range l.entries {
		if e.target == target {
			out = append(out, e)
		}
	}
	return out
}

func wlOnly(t *testing.T, l *WaitingList, target string) *entry {
	t.Helper()
	es := wlEntries(l, target)
	if len(es) != 1 {
		t.Fatalf("entries of %s = %d, want 1", target, len(es))
	}
	return es[0]
}

func allActive(string) bool { return true }

func TestNewListEmpty(t *testing.T) {
	l := NewWaitingList()
	if n := l.Count("A"); n != 0 {
		t.Errorf("Count = %d, want 0", n)
	}
	if got := l.Ordered(allActive); len(got) != 0 {
		t.Errorf("Ordered = %d entries, want 0", len(got))
	}
	if u := l.Unlisted("A"); u != 0 {
		t.Errorf("Unlisted = %d, want 0", u)
	}
	if gone := l.Expire(wlEpoch.Add(100 * time.Hour)); len(gone) != 0 {
		t.Errorf("Expire = %d, want 0", len(gone))
	}
}

func TestListMatchByRequestIDThenName(t *testing.T) {
	t.Run("two non-zero IDs stay two entries", func(t *testing.T) {
		l := NewWaitingList()
		l.Apply("A", 0, wlMsg(wlAvailable(wlJob(1, 10, "build", "j1", wlEpoch)), wlAvailable(wlJob(2, 10, "build", "j2", wlEpoch))), wlEpoch)
		if n := l.Count("A"); n != 2 {
			t.Fatalf("Count = %d, want 2", n)
		}
	})
	t.Run("zero on one side matches by run and name and takes the ID", func(t *testing.T) {
		l := NewWaitingList()
		l.Apply("A", 0, wlMsg(wlAssigned(wlJob(0, 10, "build", "j1", time.Time{}))), wlEpoch)
		ev := l.Apply("A", 0, wlMsg(wlAvailable(wlJob(7, 10, "build", "j1", wlEpoch))), wlEpoch.Add(time.Second))
		e := wlOnly(t, l, "A")
		if e.requestID != 7 {
			t.Errorf("requestID = %d, want 7", e.requestID)
		}
		if len(ev.Offered) != 1 || ev.Offered[0].requestID != 7 {
			t.Errorf("Offered = %+v, want one with requestID 7", ev.Offered)
		}
		// An ID-less message still matches the entry by run and name.
		l.Apply("A", 0, wlMsg(wlStarted(wlJob(0, 10, "build", "j1", time.Time{}))), wlEpoch.Add(2*time.Second))
		if n := l.Count("A"); n != 0 {
			t.Errorf("Count after JobStarted = %d, want 0", n)
		}
	})
	t.Run("ID match wins over name match", func(t *testing.T) {
		l := NewWaitingList()
		l.Apply("A", 0, wlMsg(
			wlAvailable(wlJob(5, 10, "test", "j5", wlEpoch)),
			wlAvailable(wlJob(0, 10, "build", "j0", wlEpoch)),
		), wlEpoch)
		l.Apply("A", 0, wlMsg(wlAssigned(wlJob(5, 10, "build", "j5b", time.Time{}))), wlEpoch.Add(time.Second))
		es := wlEntries(l, "A")
		if len(es) != 2 {
			t.Fatalf("entries = %d, want 2", len(es))
		}
		byID, byName := es[0], es[1]
		if !byID.assigned || byID.jobID != "j5b" {
			t.Errorf("ID match: assigned %v jobID %q, want true j5b", byID.assigned, byID.jobID)
		}
		if byName.assigned || byName.requestID != 0 || byName.jobID != "j0" || !byName.lastHeard.Equal(wlEpoch) {
			t.Errorf("name match changed: %+v", *byName)
		}
	})
	t.Run("targets are apart", func(t *testing.T) {
		l := NewWaitingList()
		b := wlJob(1, 10, "build", "j1", wlEpoch)
		l.Apply("A", 0, wlMsg(wlAvailable(b)), wlEpoch)
		l.Apply("B", 1, wlMsg(wlAvailable(b)), wlEpoch)
		l.Apply("B", 1, wlMsg(wlStarted(b)), wlEpoch)
		if l.Count("A") != 1 || l.Count("B") != 0 {
			t.Errorf("Count A %d B %d, want 1 0", l.Count("A"), l.Count("B"))
		}
	})
}

func TestListZeroQueueTimeUsesFirstSeen(t *testing.T) {
	l := NewWaitingList()
	seen := wlEpoch.Add(time.Minute)
	l.Apply("A", 0, wlMsg(
		wlAvailable(wlJob(1, 1, "early", "j1", wlEpoch)),
		wlAvailable(wlJob(3, 3, "late", "j3", seen.Add(time.Second))),
		wlAssigned(wlJob(2, 2, "zero", "j2", time.Time{})),
	), seen)
	var zero *entry
	for _, e := range wlEntries(l, "A") {
		if e.requestID == 2 {
			zero = e
		}
	}
	if zero == nil {
		t.Fatal("no entry for request 2")
	}
	if !zero.queueTime.Equal(seen) {
		t.Errorf("queueTime = %v, want first seen %v", zero.queueTime, seen)
	}
	got := l.Ordered(allActive)
	want := []int64{1, 2, 3}
	if len(got) != len(want) {
		t.Fatalf("Ordered = %d entries, want %d", len(got), len(want))
	}
	for i, e := range got {
		if e.requestID != want[i] {
			t.Errorf("Ordered[%d] = %d, want %d", i, e.requestID, want[i])
		}
	}
}

func TestListApplyOrder(t *testing.T) {
	t.Run("new assignment before take-back of the old one", func(t *testing.T) {
		l := NewWaitingList()
		b := wlJob(1, 10, "build", "j1", wlEpoch)
		l.Apply("A", 0, wlMsg(wlAssigned(b)), wlEpoch)
		nb := b
		nb.JobID = "j2"
		// The take-back is listed first; Apply still reads assignments first.
		m := wlMsg(wlTakeBack(b), wlAssigned(nb))
		ev := l.Apply("A", 0, m, wlEpoch.Add(time.Second))
		e := wlOnly(t, l, "A")
		if e.takenBack || !e.assigned || e.jobID != "j2" {
			t.Errorf("entry = takenBack %v assigned %v jobID %q, want false true j2", e.takenBack, e.assigned, e.jobID)
		}
		if len(ev.TakenBack) != 0 {
			t.Errorf("TakenBack events = %d, want 0", len(ev.TakenBack))
		}
	})
	t.Run("available before started", func(t *testing.T) {
		l := NewWaitingList()
		b := wlJob(1, 10, "build", "j1", wlEpoch)
		ev := l.Apply("A", 0, wlMsg(wlStarted(b), wlAvailable(b)), wlEpoch)
		if n := l.Count("A"); n != 0 {
			t.Errorf("Count = %d, want 0", n)
		}
		if len(ev.Offered) != 1 || len(ev.Removed) != 1 {
			t.Errorf("events offered %d removed %d, want 1 1", len(ev.Offered), len(ev.Removed))
		}
	})
	t.Run("nil message", func(t *testing.T) {
		l := NewWaitingList()
		ev := l.Apply("A", 0, nil, wlEpoch)
		if len(ev.Offered)+len(ev.Assigned)+len(ev.TakenBack)+len(ev.Removed) != 0 || l.Count("A") != 0 {
			t.Errorf("nil message changed the list: %+v", ev)
		}
	})
}

func TestListRedeliveryKeepsOneEntry(t *testing.T) {
	l := NewWaitingList()
	qt := wlEpoch.Add(-time.Minute)
	b := wlJob(1, 10, "build", "j1", qt)
	l.Apply("A", 0, wlMsg(wlAvailable(b)), wlEpoch)
	again := b
	again.QueueTime = wlEpoch.Add(time.Hour) // a later QueueTime must not move the entry
	l.Apply("A", 0, wlMsg(wlAvailable(again)), wlEpoch.Add(time.Second))
	asg := b
	asg.QueueTime = time.Time{}
	ev := l.Apply("A", 0, wlMsg(wlAssigned(asg)), wlEpoch.Add(2*time.Second))
	l.Apply("A", 0, wlMsg(wlAssigned(asg)), wlEpoch.Add(3*time.Second))
	e := wlOnly(t, l, "A")
	if !e.queueTime.Equal(qt) || !e.firstSeen.Equal(wlEpoch) {
		t.Errorf("queueTime %v firstSeen %v, want %v %v", e.queueTime, e.firstSeen, qt, wlEpoch)
	}
	if want := wlEpoch.Add(3 * time.Second); !e.lastHeard.Equal(want) {
		t.Errorf("lastHeard = %v, want %v", e.lastHeard, want)
	}
	if !e.assigned {
		t.Error("entry not assigned after JobAssigned")
	}
	if len(ev.Assigned) != 1 || ev.Assigned[0].requestID != 1 || ev.Assigned[0].runID != 10 ||
		ev.Assigned[0].jobName != "build" || ev.Assigned[0].jobID != "j1" {
		t.Errorf("Assigned events = %+v", ev.Assigned)
	}
}

func TestListAvailableAfterTakeBackUnassigns(t *testing.T) {
	l := NewWaitingList()
	b := wlJob(1, 10, "build", "j1", wlEpoch)
	l.Apply("A", 0, wlMsg(wlAssigned(b)), wlEpoch)
	l.Apply("A", 0, wlMsg(wlTakeBack(b)), wlEpoch.Add(time.Second))
	l.Apply("A", 0, wlMsg(wlAvailable(b)), wlEpoch.Add(2*time.Second))
	e := wlOnly(t, l, "A")
	if e.assigned || e.takenBack || e.jobID != "j1" {
		t.Errorf("entry = assigned %v takenBack %v jobID %q, want false false j1", e.assigned, e.takenBack, e.jobID)
	}
	if got := l.Ordered(allActive); len(got) != 1 {
		t.Errorf("Ordered = %d, want 1 (back in line)", len(got))
	}
}

func TestListAvailableNewJobIDUnassigns(t *testing.T) {
	l := NewWaitingList()
	b := wlJob(1, 10, "build", "j1", wlEpoch)
	l.Apply("A", 0, wlMsg(wlAssigned(b)), wlEpoch)
	nb := b
	nb.JobID = "j2"
	l.Apply("A", 0, wlMsg(wlAvailable(nb)), wlEpoch.Add(time.Second))
	e := wlOnly(t, l, "A")
	if e.assigned || e.jobID != "j2" {
		t.Errorf("entry = assigned %v jobID %q, want false j2", e.assigned, e.jobID)
	}
}

func TestListAvailableSameJobIDKeepsAssigned(t *testing.T) {
	l := NewWaitingList()
	b := wlJob(1, 10, "build", "j1", wlEpoch)
	l.Apply("A", 0, wlMsg(wlAvailable(b)), wlEpoch)
	l.MarkAcquired(wlOnly(t, l, "A"))
	l.Apply("A", 0, wlMsg(wlAvailable(b)), wlEpoch.Add(time.Second))
	noID := b
	noID.JobID = ""
	l.Apply("A", 0, wlMsg(wlAvailable(noID)), wlEpoch.Add(2*time.Second))
	e := wlOnly(t, l, "A")
	if !e.assigned || e.jobID != "j1" {
		t.Errorf("entry = assigned %v jobID %q, want true j1", e.assigned, e.jobID)
	}
}

func TestListStartedAndCompletedRemove(t *testing.T) {
	b := wlJob(1, 10, "build", "j1", wlEpoch)
	for _, tc := range []struct {
		name string
		msg  *scaleset.RunnerScaleSetMessage
	}{
		{"started", wlMsg(wlStarted(b))},
		{"completed by a runner", wlMsg(wlCompleted(b, "runner-1", time.Time{}, "succeeded"))},
		{"canceled with finish time", wlMsg(wlCompleted(b, "", wlEpoch.Add(time.Minute), "canceled"))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			l := NewWaitingList()
			l.Apply("A", 0, wlMsg(wlAssigned(b)), wlEpoch)
			ev := l.Apply("A", 0, tc.msg, wlEpoch.Add(time.Minute))
			if n := l.Count("A"); n != 0 {
				t.Errorf("Count = %d, want 0", n)
			}
			if len(ev.Removed) != 1 || ev.Removed[0].requestID != 1 {
				t.Errorf("Removed = %+v, want the entry", ev.Removed)
			}
		})
	}
	t.Run("no match changes nothing", func(t *testing.T) {
		l := NewWaitingList()
		l.Apply("A", 0, wlMsg(wlAssigned(b)), wlEpoch)
		other := wlJob(2, 11, "test", "j2", wlEpoch)
		ev := l.Apply("A", 0, wlMsg(wlStarted(other), wlCompleted(other, "runner-1", wlEpoch, "succeeded")), wlEpoch)
		if l.Count("A") != 1 || len(ev.Removed) != 0 {
			t.Errorf("Count %d removed %d, want 1 0", l.Count("A"), len(ev.Removed))
		}
	})
}

func TestListTakeBack(t *testing.T) {
	b := wlJob(1, 10, "build", "j1", wlEpoch)
	t.Run("assigned with the same JobID is marked", func(t *testing.T) {
		l := NewWaitingList()
		l.Apply("A", 0, wlMsg(wlAssigned(b)), wlEpoch)
		at := wlEpoch.Add(5 * time.Minute)
		ev := l.Apply("A", 0, wlMsg(wlTakeBack(b)), at)
		e := wlOnly(t, l, "A")
		if !e.takenBack || !e.assigned || !e.lastHeard.Equal(at) {
			t.Errorf("entry = takenBack %v assigned %v lastHeard %v, want true true %v", e.takenBack, e.assigned, e.lastHeard, at)
		}
		if len(ev.TakenBack) != 1 || len(ev.Removed) != 0 {
			t.Errorf("events takenBack %d removed %d, want 1 0", len(ev.TakenBack), len(ev.Removed))
		}
		// R225: out of the placement order, still counted.
		if got := l.Ordered(allActive); len(got) != 0 {
			t.Errorf("Ordered = %d, want 0", len(got))
		}
		if n := l.Count("A"); n != 1 {
			t.Errorf("Count = %d, want 1", n)
		}
		// Only R207 clears the mark: JobAssigned again.
		nb := b
		nb.JobID = "j2"
		l.Apply("A", 0, wlMsg(wlAssigned(nb)), at.Add(time.Second))
		if e.takenBack || !e.assigned || e.jobID != "j2" {
			t.Errorf("after reassign: takenBack %v assigned %v jobID %q", e.takenBack, e.assigned, e.jobID)
		}
	})
	t.Run("unassigned entry is not marked", func(t *testing.T) {
		l := NewWaitingList()
		l.Apply("A", 0, wlMsg(wlAvailable(b)), wlEpoch)
		ev := l.Apply("A", 0, wlMsg(wlTakeBack(b)), wlEpoch.Add(time.Minute))
		e := wlOnly(t, l, "A")
		if e.takenBack || !e.lastHeard.Equal(wlEpoch) || len(ev.TakenBack) != 0 {
			t.Errorf("entry changed: takenBack %v lastHeard %v", e.takenBack, e.lastHeard)
		}
	})
	t.Run("different JobID is not marked", func(t *testing.T) {
		l := NewWaitingList()
		l.Apply("A", 0, wlMsg(wlAssigned(b)), wlEpoch)
		old := b
		old.JobID = "j0"
		l.Apply("A", 0, wlMsg(wlTakeBack(old)), wlEpoch.Add(time.Minute))
		e := wlOnly(t, l, "A")
		if e.takenBack || !e.lastHeard.Equal(wlEpoch) {
			t.Errorf("entry changed: takenBack %v lastHeard %v", e.takenBack, e.lastHeard)
		}
	})
	t.Run("no match adds nothing", func(t *testing.T) {
		l := NewWaitingList()
		l.Apply("A", 0, wlMsg(wlTakeBack(b)), wlEpoch)
		if n := l.Count("A"); n != 0 {
			t.Errorf("Count = %d, want 0", n)
		}
	})
}

func TestListDropTarget(t *testing.T) {
	l := NewWaitingList()
	l.Apply("A", 0, wlMsg(wlAvailable(wlJob(1, 10, "build", "j1", wlEpoch)), wlAssigned(wlJob(2, 10, "test", "j2", wlEpoch))), wlEpoch)
	l.Apply("B", 1, wlMsg(wlAvailable(wlJob(1, 10, "build", "j1", wlEpoch))), wlEpoch)
	l.DropTarget("A")
	if l.Count("A") != 0 || l.Count("B") != 1 {
		t.Errorf("Count A %d B %d, want 0 1", l.Count("A"), l.Count("B"))
	}
	if got := l.Ordered(allActive); len(got) != 1 || got[0].target != "B" {
		t.Errorf("Ordered = %d entries, want B's one", len(got))
	}
}

func TestListRemoveAndMarkAcquired(t *testing.T) {
	l := NewWaitingList()
	l.Apply("A", 0, wlMsg(wlAvailable(wlJob(1, 10, "build", "j1", wlEpoch)), wlAvailable(wlJob(2, 10, "test", "j2", wlEpoch))), wlEpoch)
	es := l.Ordered(allActive)
	l.MarkAcquired(es[0])
	if !es[0].assigned || es[0].assignedSeq != 1 {
		t.Errorf("MarkAcquired: assigned %v assignedSeq %d, want true 1", es[0].assigned, es[0].assignedSeq)
	}
	l.Remove(es[1])
	l.Remove(es[1]) // a second Remove does nothing
	if got := wlEntries(l, "A"); len(got) != 1 || got[0] != es[0] {
		t.Errorf("entries after Remove = %d, want the first", len(got))
	}
}

// wlAssignN adds n assigned entries to target, request IDs from first.
func wlAssignN(l *WaitingList, target string, first int64, n int, now time.Time) {
	for i := range int64(n) {
		l.Apply(target, 0, wlMsg(wlAssigned(wlJob(first+i, 100+first+i, "job", "j", time.Time{}))), now)
	}
}

func TestListStatisticsUnlisted(t *testing.T) {
	l := NewWaitingList()
	wlAssignN(l, "A", 1, 3, wlEpoch)
	l.Apply("A", 0, wlMsg(wlTakeBack(wlJob(3, 103, "job", "j", time.Time{}))), wlEpoch) // taken back: not counted
	l.Apply("A", 0, wlMsg(wlAvailable(wlJob(9, 109, "job", "j9", wlEpoch))), wlEpoch)   // unassigned: not counted

	// 6 assigned − 1 started − 2 assigned entries = 3.
	res := l.Statistics("A", scaleset.RunnerScaleSetStatistic{TotalAssignedJobs: 6}, 1)
	if res.Unlisted != 3 || !res.UnlistedChanged || l.Unlisted("A") != 3 {
		t.Errorf("Unlisted = %d changed %v stored %d, want 3 true 3", res.Unlisted, res.UnlistedChanged, l.Unlisted("A"))
	}
	// A message without Statistics never calls Statistics: the value stays.
	l.Apply("A", 0, wlMsg(wlStarted(wlJob(1, 101, "job", "j", time.Time{}))), wlEpoch.Add(time.Second))
	if u := l.Unlisted("A"); u != 3 {
		t.Errorf("Unlisted after message without Statistics = %d, want 3", u)
	}
	// 5 − 2 − 1 = 2.
	res = l.Statistics("A", scaleset.RunnerScaleSetStatistic{TotalAssignedJobs: 5}, 2)
	if res.Unlisted != 2 || !res.UnlistedChanged {
		t.Errorf("Unlisted = %d changed %v, want 2 true", res.Unlisted, res.UnlistedChanged)
	}
	res = l.Statistics("A", scaleset.RunnerScaleSetStatistic{TotalAssignedJobs: 5}, 2)
	if res.Unlisted != 2 || res.UnlistedChanged {
		t.Errorf("same input: Unlisted = %d changed %v, want 2 false", res.Unlisted, res.UnlistedChanged)
	}
	// Never below 0.
	res = l.Statistics("A", scaleset.RunnerScaleSetStatistic{TotalAssignedJobs: 0}, 4)
	if res.Unlisted != 0 {
		t.Errorf("Unlisted = %d, want 0", res.Unlisted)
	}
	if u := l.Unlisted("B"); u != 0 {
		t.Errorf("Unlisted of another target = %d, want 0", u)
	}
}

func TestListAvailableWarn(t *testing.T) {
	l := NewWaitingList()
	l.Apply("A", 0, wlMsg(
		wlAvailable(wlJob(1, 101, "job", "j1", wlEpoch)), // unassigned
		wlAssigned(wlJob(2, 102, "job", "j2", wlEpoch)),  // assigned, then taken back
		wlAssigned(wlJob(3, 103, "job", "j3", wlEpoch)),  // assigned
	), wlEpoch)
	l.Apply("A", 0, wlMsg(wlTakeBack(wlJob(2, 102, "job", "j2", wlEpoch))), wlEpoch)
	l.Apply("B", 1, wlMsg(wlAvailable(wlJob(4, 104, "job", "j4", wlEpoch))), wlEpoch) // other target
	for _, tc := range []struct {
		available int
		warn      bool
	}{{3, true}, {2, false}, {1, false}} {
		res := l.Statistics("A", scaleset.RunnerScaleSetStatistic{TotalAvailableJobs: tc.available, TotalAssignedJobs: 1}, 0)
		if res.Warn != tc.warn || res.Known != 2 {
			t.Errorf("available %d: Warn %v Known %d, want %v 2", tc.available, res.Warn, res.Known, tc.warn)
		}
	}
}

func TestListDropAfterTwoDeficits(t *testing.T) {
	t.Run("two deficits drop the smaller count, quietest first", func(t *testing.T) {
		l := NewWaitingList()
		// Entries 1, 2, 3 assigned; last heard 1 < 3 < 2.
		wlAssignN(l, "A", 1, 3, wlEpoch)
		l.Apply("A", 0, wlMsg(wlAssigned(wlJob(2, 102, "job", "j", time.Time{}))), wlEpoch.Add(2*time.Second))
		l.Apply("A", 0, wlMsg(wlAssigned(wlJob(3, 103, "job", "j", time.Time{}))), wlEpoch.Add(time.Second))
		// A taken-back entry is neither counted nor dropped.
		l.Apply("A", 0, wlMsg(wlAssigned(wlJob(4, 104, "job", "j", time.Time{})), wlTakeBack(wlJob(4, 104, "job", "j", time.Time{}))), wlEpoch)

		st := scaleset.RunnerScaleSetStatistic{TotalAssignedJobs: 2}
		// 3 live − (2 − 1 started) = 2: first deficit, nothing dropped.
		if res := l.Statistics("A", st, 1); len(res.Dropped) != 0 {
			t.Fatalf("first deficit dropped %d", len(res.Dropped))
		}
		// A message without Statistics between keeps the two consecutive.
		l.Apply("A", 0, wlMsg(), wlEpoch.Add(3*time.Second))
		st.TotalAssignedJobs = 3
		// 3 − (3 − 1) = 1: min(2, 1) = 1 dropped.
		res := l.Statistics("A", st, 1)
		if len(res.Dropped) != 1 {
			t.Fatalf("dropped %d, want 1", len(res.Dropped))
		}
		d := res.Dropped[0]
		if d.requestID != 1 || d.runID != 101 || d.jobName != "job" {
			t.Errorf("dropped %+v, want request 1 run 101 job", d)
		}
		if l.Count("A") != 3 || res.Unlisted != 0 {
			t.Errorf("Count %d Unlisted %d, want 3 0", l.Count("A"), res.Unlisted)
		}
		// Deficit still 1 > 0 after 1 > 0: one more, the next quietest (3).
		st.TotalAssignedJobs = 2
		res = l.Statistics("A", st, 1)
		if len(res.Dropped) != 1 || res.Dropped[0].requestID != 3 {
			t.Errorf("dropped %+v, want request 3", res.Dropped)
		}
	})
	t.Run("ties in last heard follow the R184 order", func(t *testing.T) {
		l := NewWaitingList()
		wlAssignN(l, "A", 6, 1, wlEpoch) // request 6 first, then 5, same times
		wlAssignN(l, "A", 5, 1, wlEpoch)
		st := scaleset.RunnerScaleSetStatistic{TotalAssignedJobs: 0}
		l.Statistics("A", st, 0)
		res := l.Statistics("A", st, 0)
		if len(res.Dropped) != 2 || res.Dropped[0].requestID != 5 || res.Dropped[1].requestID != 6 {
			t.Errorf("dropped %+v, want 5 then 6", res.Dropped)
		}
	})
	t.Run("one deficit alone drops nothing", func(t *testing.T) {
		l := NewWaitingList()
		wlAssignN(l, "A", 1, 2, wlEpoch)
		for i, tal := range []int{1, 2, 1, 2, 1} { // deficits 1, 0, 1, 0, 1
			if res := l.Statistics("A", scaleset.RunnerScaleSetStatistic{TotalAssignedJobs: tal}, 0); len(res.Dropped) != 0 {
				t.Errorf("message %d dropped %d", i, len(res.Dropped))
			}
		}
	})
	t.Run("entry assigned since the previous message is kept", func(t *testing.T) {
		l := NewWaitingList()
		wlAssignN(l, "A", 1, 1, wlEpoch.Add(time.Minute))
		st := scaleset.RunnerScaleSetStatistic{TotalAssignedJobs: 0}
		l.Statistics("A", st, 0) // deficit 1
		// Entry 2 assigned after that message, heard before entry 1.
		wlAssignN(l, "A", 2, 1, wlEpoch)
		// And entry 3 made assigned again by AcquireJobs after that message.
		l.Apply("A", 0, wlMsg(wlAvailable(wlJob(3, 103, "job", "j", wlEpoch))), wlEpoch)
		l.MarkAcquired(wlEntries(l, "A")[2])
		res := l.Statistics("A", st, 0) // deficit 3: min(1, 3) = 1, only entry 1 qualifies
		if len(res.Dropped) != 1 || res.Dropped[0].requestID != 1 {
			t.Fatalf("dropped %+v, want only request 1", res.Dropped)
		}
		if l.Count("A") != 2 {
			t.Errorf("Count = %d, want 2", l.Count("A"))
		}
	})
	t.Run("re-assignment after a take-back is new", func(t *testing.T) {
		l := NewWaitingList()
		j := wlJob(1, 101, "job", "j1", wlEpoch)
		l.Apply("A", 0, wlMsg(wlAssigned(j)), wlEpoch)
		st := scaleset.RunnerScaleSetStatistic{TotalAssignedJobs: 0}
		l.Statistics("A", st, 0) // deficit 1
		l.Apply("A", 0, wlMsg(wlTakeBack(j)), wlEpoch.Add(time.Second))
		l.Apply("A", 0, wlMsg(wlAssigned(j)), wlEpoch.Add(2*time.Second)) // assigned again after that message
		if res := l.Statistics("A", st, 0); len(res.Dropped) != 0 {
			t.Errorf("dropped %d, want 0: the assignment is new since the previous message", len(res.Dropped))
		}
	})
	t.Run("only new assignments drop nothing", func(t *testing.T) {
		l := NewWaitingList()
		b := wlJob(1, 101, "job", "j1", wlEpoch)
		l.Apply("A", 0, wlMsg(wlAssigned(b)), wlEpoch)
		st := scaleset.RunnerScaleSetStatistic{TotalAssignedJobs: 0}
		l.Statistics("A", st, 0) // deficit 1
		nb := b
		nb.JobID = "j2"
		l.Apply("A", 0, wlMsg(wlAvailable(nb)), wlEpoch) // unassigned again
		l.MarkAcquired(wlOnly(t, l, "A"))                // assigned after that message
		if res := l.Statistics("A", st, 0); len(res.Dropped) != 0 {
			t.Errorf("dropped %d, want 0", len(res.Dropped))
		}
	})
	t.Run("targets are apart", func(t *testing.T) {
		l := NewWaitingList()
		wlAssignN(l, "A", 1, 1, wlEpoch)
		wlAssignN(l, "B", 1, 1, wlEpoch)
		st := scaleset.RunnerScaleSetStatistic{TotalAssignedJobs: 0}
		l.Statistics("A", st, 0)
		l.Statistics("B", scaleset.RunnerScaleSetStatistic{TotalAssignedJobs: 1}, 0)
		l.Statistics("B", st, 0) // B's first deficit
		if res := l.Statistics("A", st, 0); len(res.Dropped) != 1 || res.Dropped[0].target != "A" {
			t.Errorf("A dropped %+v, want A's entry", res.Dropped)
		}
		if l.Count("B") != 1 {
			t.Errorf("Count B = %d, want 1", l.Count("B"))
		}
	})
}

func TestListExpireAfter25Hours(t *testing.T) {
	l := NewWaitingList()
	l.Apply("A", 0, wlMsg(
		wlAvailable(wlJob(1, 101, "old", "j1", wlEpoch)),
		wlAssigned(wlJob(2, 102, "new", "j2", wlEpoch.Add(time.Second))),
	), wlEpoch.Add(time.Second))
	l.Apply("A", 0, wlMsg(wlTakeBack(wlJob(2, 102, "new", "j2", time.Time{}))), wlEpoch.Add(time.Second))
	if gone := l.Expire(wlEpoch.Add(25 * time.Hour)); len(gone) != 0 {
		t.Errorf("at 25 h expired %d, want 0", len(gone))
	}
	gone := l.Expire(wlEpoch.Add(25*time.Hour + time.Second))
	if len(gone) != 1 || gone[0].requestID != 1 || !gone[0].queueTime.Equal(wlEpoch) || gone[0].target != "A" {
		t.Fatalf("expired %+v, want request 1 queued at %v", gone, wlEpoch)
	}
	if l.Count("A") != 1 {
		t.Errorf("Count = %d, want 1", l.Count("A"))
	}
	// A taken-back entry expires too.
	if gone := l.Expire(wlEpoch.Add(25*time.Hour + 2*time.Second)); len(gone) != 1 || gone[0].requestID != 2 {
		t.Errorf("expired %+v, want request 2", gone)
	}
}

func TestListOrdered(t *testing.T) {
	l := NewWaitingList()
	t0 := wlEpoch
	at := func(d time.Duration) time.Time { return t0.Add(d) }
	// Earliest QueueTime first, whatever the target.
	l.Apply("B", 1, wlMsg(wlAvailable(wlJob(11, 1, "q-early", "", at(-time.Hour)))), at(0))
	// Same QueueTime: first seen decides before config order.
	l.Apply("B", 1, wlMsg(wlAvailable(wlJob(21, 2, "seen-first", "", at(0)))), at(0))
	l.Apply("A", 0, wlMsg(wlAvailable(wlJob(22, 2, "seen-later", "", at(0)))), at(time.Second))
	// Same QueueTime and first seen: config order.
	l.Apply("B", 1, wlMsg(wlAvailable(wlJob(31, 3, "order-1", "", at(time.Minute)))), at(time.Minute))
	l.Apply("A", 0, wlMsg(wlAvailable(wlJob(32, 3, "order-0", "", at(time.Minute)))), at(time.Minute))
	// Then RunnerRequestID.
	l.Apply("A", 0, wlMsg(wlAvailable(wlJob(42, 4, "req-42", "", at(2*time.Minute)))), at(2*time.Minute))
	l.Apply("A", 0, wlMsg(wlAvailable(wlJob(41, 4, "req-41", "", at(2*time.Minute)))), at(2*time.Minute))
	// Then run ID, then job name (request IDs 0).
	l.Apply("A", 0, wlMsg(
		wlAvailable(wlJob(0, 6, "b", "", at(3*time.Minute))),
		wlAvailable(wlJob(0, 6, "a", "", at(3*time.Minute))),
		wlAvailable(wlJob(0, 5, "z", "", at(3*time.Minute))),
	), at(3*time.Minute))
	// Then insertion: two targets with the same config index.
	l.Apply("D", 2, wlMsg(wlAvailable(wlJob(51, 7, "ins", "", at(4*time.Minute)))), at(4*time.Minute))
	l.Apply("C", 2, wlMsg(wlAvailable(wlJob(51, 7, "ins", "", at(4*time.Minute)))), at(4*time.Minute))
	// Excluded: a taken-back entry and an inactive target's entry.
	l.Apply("A", 0, wlMsg(wlAssigned(wlJob(61, 8, "taken-back", "j61", at(-2*time.Hour)))), at(0))
	l.Apply("A", 0, wlMsg(wlTakeBack(wlJob(61, 8, "taken-back", "j61", time.Time{}))), at(0))
	l.Apply("E", 3, wlMsg(wlAvailable(wlJob(71, 9, "inactive", "", at(-3*time.Hour)))), at(0))

	got := l.Ordered(func(target string) bool { return target != "E" })
	want := []string{"q-early", "seen-first", "seen-later", "order-0", "order-1", "req-41", "req-42", "z", "a", "b", "ins@D", "ins@C"}
	if len(got) != len(want) {
		t.Fatalf("Ordered = %d entries, want %d", len(got), len(want))
	}
	for i, e := range got {
		name := e.jobName
		if name == "ins" {
			name += "@" + e.target
		}
		if name != want[i] {
			t.Errorf("Ordered[%d] = %s, want %s", i, name, want[i])
		}
	}
}
