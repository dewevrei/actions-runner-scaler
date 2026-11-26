package scaler

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/actions/scaleset"

	"github.com/dewevrei/actions-runner-scaler/internal/retry"
)

// Placement tests (0002 PLAN M14 Proof): fake sessions recording
// AcquireJobs, fake Docker, fake clock. Every placement here runs inside a
// Scale, synchronously, unless a test starts the kick goroutine.

// placeHarnesses returns one harness per target, all on one table with the
// server limit limit and one Placer, in config order.
func placeHarnesses(t *testing.T, limit int, targets ...TargetConfig) []*harness {
	t.Helper()
	hs := []*harness{newLimitHarness(t, targets[0], limit)}
	for _, tc := range targets[1:] {
		hs = append(hs, newTargetHarness(t, hs[0], tc))
	}
	return hs
}

func targetA(maxRunners, idle int) TargetConfig {
	return TargetConfig{Key: "a", URL: "https://github.com/acme/a", Label: "pool", MaxRunners: maxRunners, Idle: idle}
}

func targetB(maxRunners, idle int) TargetConfig {
	return TargetConfig{Key: "b", URL: "https://github.com/acme/b", Label: "pool", MaxRunners: maxRunners, Idle: idle}
}

// Message builders: job req is RunnerRequestID req, workflow run req, job
// name "job", JobID "j<req>".

func newMsg() *scaleset.RunnerScaleSetMessage { return &scaleset.RunnerScaleSetMessage{MessageID: 1} }

func jobBase(req int64, queued time.Time) scaleset.JobMessageBase {
	return scaleset.JobMessageBase{RunnerRequestID: req, WorkflowRunID: req, JobDisplayName: "job",
		JobID: fmt.Sprintf("j%d", req), QueueTime: queued}
}

func offer(m *scaleset.RunnerScaleSetMessage, req int64, queued time.Time) *scaleset.RunnerScaleSetMessage {
	m.JobAvailableMessages = append(m.JobAvailableMessages, &scaleset.JobAvailable{JobMessageBase: jobBase(req, queued)})
	return m
}

func assign(m *scaleset.RunnerScaleSetMessage, req int64, queued time.Time) *scaleset.RunnerScaleSetMessage {
	m.JobAssignedMessages = append(m.JobAssignedMessages, &scaleset.JobAssigned{JobMessageBase: jobBase(req, queued)})
	return m
}

func jobStartedOn(m *scaleset.RunnerScaleSetMessage, req int64, runner string) *scaleset.RunnerScaleSetMessage {
	m.JobStartedMessages = append(m.JobStartedMessages, &scaleset.JobStarted{RunnerName: runner, JobMessageBase: jobBase(req, time.Time{})})
	return m
}

// takeBack is a JobCompleted without runner and finish time (0002 SPEC R209).
func takeBack(m *scaleset.RunnerScaleSetMessage, req int64) *scaleset.RunnerScaleSetMessage {
	m.JobCompletedMessages = append(m.JobCompletedMessages, &scaleset.JobCompleted{JobMessageBase: jobBase(req, time.Time{})})
	return m
}

func withStats(m *scaleset.RunnerScaleSetMessage, assigned, available int) *scaleset.RunnerScaleSetMessage {
	m.Statistics = &scaleset.RunnerScaleSetStatistic{TotalAssignedJobs: assigned, TotalAvailableJobs: available}
	return m
}

// seedStarted seeds a busy runner of target that received its own JobStarted.
func seedStarted(t *testing.T, h *harness, target, name string, id int64) {
	t.Helper()
	seedFor(t, h.table, target, name, id, StateBusy, h.clock.Now())
	if r := h.table.RecordJobStarted(name, Job{ID: "job-" + name}); r != JobStartedRecorded {
		t.Fatalf("seed %s: JobStarted %v", name, r)
	}
}

// startedFor counts the runners started for h's target (its JIT calls that
// led to a container).
func startedFor(h *harness) int {
	n := 0
	for _, name := range h.docker.startedNames() {
		if slices.Contains(h.gh.jitNamesSnapshot(), name) {
			n++
		}
	}
	return n
}

func (f *fakeScaleSet) jitNamesSnapshot() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.jitNames)
}

// dropRow removes a runner's row as its watcher would after it ended.
func dropRow(h *harness, name string) {
	h.table.BeginExit(name)
	h.table.MarkRemoved(name)
}

// backoff puts target in its start backoff until d from now (R61).
func backoff(h *harness, target string, d time.Duration) {
	now := h.clock.Now()
	h.table.RecordStartFailure(target, errors.New("failed"), now, now.Add(d))
}

func TestPlaceFullServerNoAcquire(t *testing.T) {
	hs := placeHarnesses(t, 1, targetA(2, 0))
	a := hs[0]
	seedStarted(t, a, "a", "pool-00000001", 1)
	a.mustScale(offer(newMsg(), 1, testEpoch))
	if n := a.acq.callCount(); n != 0 || a.gh.jitCalls() != 0 {
		t.Fatalf("AcquireJobs %d, JIT %d on a full server; want 0 and 0 (0002 SPEC R169, R114)", n, a.gh.jitCalls())
	}
}

func TestPlaceRowRemovedWithinOneSecond(t *testing.T) {
	hs := placeHarnesses(t, 1, targetA(1, 0), targetB(1, 0))
	a, b := hs[0], hs[1]
	seedStarted(t, a, "a", "pool-0000000a", 1)
	b.mustScale(offer(newMsg(), 7, testEpoch))
	if b.acq.callCount() != 0 {
		t.Fatal("AcquireJobs on a full server")
	}
	order := &callLog{}
	b.acq.mu.Lock()
	b.acq.order = order
	b.acq.mu.Unlock()
	b.gh.mu.Lock()
	b.gh.duringJIT = func(string) { order.add("jit") }
	b.gh.mu.Unlock()
	b.docker.mu.Lock()
	b.docker.duringStart = func(context.Context, string) { order.add("start") }
	b.docker.mu.Unlock()
	b.placer.Start()

	// The fake clock never moves here: the kicked placement runs with no
	// wait at all, well within R170's 1 s.
	at := a.clock.Now()
	dropRow(a, "pool-0000000a") // OnRemoved kicks a placement (0002 SPEC R170)
	waitUntil(t, "b's runner started", func() bool { return len(order.snapshot()) == 3 })
	if got := order.snapshot(); !slices.Equal(got, []string{"acquire 7", "jit", "start"}) {
		t.Fatalf("calls %v, want AcquireJobs, JIT, StartRunner (0002 SPEC R214)", got)
	}
	if d := a.clock.Now().Sub(at); d >= time.Second {
		t.Fatalf("placement %v after the row left, want within 1 s", d)
	}
}

func TestPlaceQueueTimeAcrossTargets(t *testing.T) {
	// One slot frees; the entry first in 0002 SPEC R184's order gets it.
	place := func(t *testing.T, setup func(a, b *harness)) (a, b *harness) {
		hs := placeHarnesses(t, 1, targetA(2, 0), targetB(2, 0))
		a, b = hs[0], hs[1]
		seedStarted(t, a, "a", "pool-0000000a", 1)
		setup(a, b)
		dropRow(a, "pool-0000000a")
		a.mustScale(nil)
		return a, b
	}
	t.Run("earlier QueueTime of another target", func(t *testing.T) {
		a, b := place(t, func(a, b *harness) {
			a.mustScale(assign(newMsg(), 1, testEpoch.Add(20*time.Second)))
			b.mustScale(assign(newMsg(), 2, testEpoch.Add(10*time.Second)))
		})
		if startedFor(b) != 1 || startedFor(a) != 0 {
			t.Fatalf("starts a %d b %d; want b's earlier job first", startedFor(a), startedFor(b))
		}
	})
	t.Run("same QueueTime: first seen", func(t *testing.T) {
		a, b := place(t, func(a, b *harness) {
			b.mustScale(assign(newMsg(), 2, testEpoch))
			a.clock.Advance(time.Second)
			a.mustScale(assign(newMsg(), 1, testEpoch))
		})
		if startedFor(b) != 1 || startedFor(a) != 0 {
			t.Fatalf("starts a %d b %d; want b, seen first", startedFor(a), startedFor(b))
		}
	})
	t.Run("same QueueTime and first seen: config order", func(t *testing.T) {
		a, b := place(t, func(a, b *harness) {
			b.mustScale(assign(newMsg(), 2, testEpoch))
			a.mustScale(assign(newMsg(), 1, testEpoch))
		})
		if startedFor(a) != 1 || startedFor(b) != 0 {
			t.Fatalf("starts a %d b %d; want a, first in config", startedFor(a), startedFor(b))
		}
	})
	t.Run("same target: RunnerRequestID", func(t *testing.T) {
		a, _ := place(t, func(a, b *harness) {
			a.mustScale(offer(offer(newMsg(), 9, testEpoch), 3, testEpoch))
		})
		if got := a.acq.requested(); !slices.Equal(got, []int64{3}) {
			t.Fatalf("acquired %v, want the lower RunnerRequestID 3", got)
		}
	})
	t.Run("zero QueueTime uses first seen", func(t *testing.T) {
		a, b := place(t, func(a, b *harness) {
			a.clock.Advance(time.Minute)
			a.mustScale(assign(newMsg(), 1, time.Time{})) // queued at +1m
			b.mustScale(assign(newMsg(), 2, testEpoch.Add(30*time.Second)))
		})
		if startedFor(b) != 1 || startedFor(a) != 0 {
			t.Fatalf("starts a %d b %d; want b, queued earlier", startedFor(a), startedFor(b))
		}
	})
}

func TestPlaceJobNoLongerAvailable(t *testing.T) {
	a := placeHarnesses(t, 1, targetA(2, 0))[0]
	a.acq.result = func(ids []int64) ([]int64, error) {
		if ids[0] == 1 {
			return nil, nil
		}
		return ids, nil
	}
	a.mustScale(offer(offer(newMsg(), 1, testEpoch), 2, testEpoch.Add(time.Second)))
	if got := a.acq.requested(); !slices.Equal(got, []int64{1, 2}) {
		t.Fatalf("acquired %v, want 1 then 2", got)
	}
	if startedFor(a) != 1 || a.placer.WaitingJobs("a") != 0 {
		t.Fatalf("starts %d, waiting %d; want one runner for job 2 and job 1 removed", startedFor(a), a.placer.WaitingJobs("a"))
	}
	if !strings.Contains(a.logs.String(), `"msg":"job no longer available","component":"placement","target":"https://github.com/acme/a","runnerRequestId":1`) {
		t.Fatalf("no INFO job no longer available:\n%s", a.logs.String())
	}
}

func TestPlaceAcquireTransientSkipsTarget(t *testing.T) {
	a := placeHarnesses(t, 5, targetA(3, 1))[0]
	a.acq.setErr(errTransient)
	a.mustScale(offer(offer(newMsg(), 1, testEpoch), 2, testEpoch.Add(time.Second)))
	if got := a.acq.requested(); !slices.Equal(got, []int64{1}) {
		t.Fatalf("acquired %v, want only the first entry (0002 SPEC R216)", got)
	}
	if a.placer.WaitingJobs("a") != 2 || startedFor(a) != 0 {
		t.Fatalf("waiting %d, starts %d; want both entries kept and no start (idle fill skipped, R229)",
			a.placer.WaitingJobs("a"), startedFor(a))
	}
	if last := a.errs.last(); last.where != "AcquireJobs" {
		t.Fatalf("last error %+v, want the AcquireJobs error", last)
	}
}

func TestPlaceAcquireSessionLost(t *testing.T) {
	a := placeHarnesses(t, 5, targetA(3, 0))[0]
	a.acq.setErr(errSessionLost)
	a.mustScale(offer(newMsg(), 1, testEpoch))
	if a.acq.lostCount() != 1 || a.placer.WaitingJobs("a") != 1 {
		t.Fatalf("SessionLost %d, waiting %d; want the listener ended once and the entry kept",
			a.acq.lostCount(), a.placer.WaitingJobs("a"))
	}
}

func TestPlaceAcquirePermanent(t *testing.T) {
	a := placeHarnesses(t, 5, targetA(3, 0))[0]
	a.acq.setErr(errPermanent)
	a.mustScale(offer(offer(newMsg(), 1, testEpoch), 2, testEpoch))
	if a.fatalCount() != 1 || !a.table.TargetDraining("a") || a.placer.WaitingJobs("a") != 0 {
		t.Fatalf("fatal %d, drain %v, waiting %d; want target error drain with entries dropped (0002 SPEC R171, R210)",
			a.fatalCount(), a.table.TargetDraining("a"), a.placer.WaitingJobs("a"))
	}
	if a.acq.callCount() != 1 || startedFor(a) != 0 {
		t.Fatalf("AcquireJobs %d, starts %d; want 1 and 0", a.acq.callCount(), startedFor(a))
	}
}

// TestPlacePermanentWithCoveredEntry: a permanent error after another entry
// of the target was covered leaves waitingJobs 0, never negative (0002 SPEC
// R222, R210).
func TestPlacePermanentWithCoveredEntry(t *testing.T) {
	a := placeHarnesses(t, 2, targetA(2, 0))[0]
	seedFor(t, a.table, "a", "pool-00000001", 1, StateIdle, testEpoch)
	seedFor(t, a.table, "a", "pool-00000002", 2, StateIdle, testEpoch)
	a.acq.setErr(errPermanent) // job 1 takes one idle runner; job 2's AcquireJobs fails
	a.mustScale(offer(assign(newMsg(), 1, testEpoch), 2, testEpoch.Add(time.Second)))
	if a.fatalCount() != 1 {
		t.Fatalf("fatal %d, want 1", a.fatalCount())
	}
	if n := a.placer.WaitingJobs("a"); n != 0 {
		t.Fatalf("waiting %d, want 0", n)
	}
}

func TestPlaceStartFailedEntryRetriesWithoutAcquire(t *testing.T) {
	a := placeHarnesses(t, 5, targetA(3, 1))[0]
	var fails atomic.Int32
	a.docker.setStartErr(func(string) error {
		if fails.Add(1) == 1 {
			return errTransient
		}
		return nil
	})
	a.mustScale(offer(newMsg(), 1, testEpoch))
	if live := a.table.Counts("a").Live(); a.acq.callCount() != 1 || live != 0 || a.placer.WaitingJobs("a") != 1 {
		t.Fatalf("AcquireJobs %d, runners %d, waiting %d; want 1, 0, 1", a.acq.callCount(), live, a.placer.WaitingJobs("a"))
	}
	a.mustScale(nil)
	if live := a.table.Counts("a").Live(); a.acq.callCount() != 1 || live == 0 || a.placer.WaitingJobs("a") != 0 {
		t.Fatalf("AcquireJobs %d, runners %d; want the assigned entry's runner without AcquireJobs (0002 SPEC R213, R214)",
			a.acq.callCount(), live)
	}
}

func TestPlaceAssignedEntryNoAcquire(t *testing.T) {
	a := placeHarnesses(t, 5, targetA(3, 0))[0]
	a.mustScale(assign(newMsg(), 1, testEpoch))
	if a.acq.callCount() != 0 || startedFor(a) != 1 {
		t.Fatalf("AcquireJobs %d, starts %d; want 0 and 1 (0002 SPEC R214)", a.acq.callCount(), startedFor(a))
	}
}

func TestPlaceIdleRunnerCoversWhenFull(t *testing.T) {
	a := placeHarnesses(t, 1, targetA(2, 0))[0]
	seedFor(t, a.table, "a", "pool-00000001", 1, StateIdle, testEpoch)
	a.mustScale(offer(newMsg(), 1, testEpoch))
	if a.acq.callCount() != 1 || a.gh.jitCalls() != 0 || len(a.gh.removedIDs()) != 0 {
		t.Fatalf("AcquireJobs %d, JIT %d, removed %v; want one AcquireJobs and the idle runner kept as cover",
			a.acq.callCount(), a.gh.jitCalls(), a.gh.removedIDs())
	}
}

func TestPlaceBusyNotStartedCountsOnce(t *testing.T) {
	a := placeHarnesses(t, 5, targetA(3, 0))[0]
	seedFor(t, a.table, "a", "pool-00000001", 1, StateBusy, testEpoch) // busy by the hook
	a.mustScale(assign(newMsg(), 1, testEpoch))
	if a.gh.jitCalls() != 0 {
		t.Fatal("a second runner for an entry covered by a busy runner without JobStarted (0002 SPEC R224)")
	}
	// Its JobStarted removes the entry and takes the runner out of the cover.
	a.mustScale(jobStartedOn(newMsg(), 1, "pool-00000001"))
	a.mustScale(assign(newMsg(), 2, testEpoch))
	if a.gh.jitCalls() != 1 {
		t.Fatalf("JIT %d after JobStarted, want a runner for the next entry", a.gh.jitCalls())
	}
}

func TestPlaceTakenBackSkipsCover(t *testing.T) {
	a := placeHarnesses(t, 1, targetA(2, 0))[0]
	seedFor(t, a.table, "a", "pool-00000001", 1, StateIdle, testEpoch)
	a.mustScale(assign(newMsg(), 1, testEpoch))
	a.mustScale(assign(takeBack(newMsg(), 1), 2, testEpoch.Add(time.Second)))
	if a.gh.jitCalls() != 0 || len(a.gh.removedIDs()) != 0 {
		t.Fatalf("JIT %d, removed %v; want job 2 on the idle runner", a.gh.jitCalls(), a.gh.removedIDs())
	}
	if n := a.placer.WaitingJobs("a"); n != 1 {
		t.Fatalf("waiting %d, want the taken-back entry (0002 SPEC R225, R222)", n)
	}
}

func TestPlaceReacquireAfterTakeBack(t *testing.T) {
	// idle 1 keeps the runner while the entry is taken back, which covers
	// nothing (0002 SPEC R225, R185).
	a := placeHarnesses(t, 1, targetA(2, 1))[0]
	seedFor(t, a.table, "a", "pool-00000001", 1, StateIdle, testEpoch)
	a.mustScale(assign(newMsg(), 1, testEpoch))
	a.mustScale(takeBack(newMsg(), 1))
	a.mustScale(offer(newMsg(), 1, testEpoch))
	if got := a.acq.requested(); !slices.Equal(got, []int64{1}) || a.gh.jitCalls() != 0 {
		t.Fatalf("acquired %v, JIT %d; want one AcquireJobs and the idle runner (0002 SPEC R207)", got, a.gh.jitCalls())
	}
	a.mustScale(offer(newMsg(), 1, testEpoch)) // same JobID again: stays assigned
	if a.acq.callCount() != 1 {
		t.Fatalf("AcquireJobs %d after a plain redelivery, want 1", a.acq.callCount())
	}
}

func TestPlaceBusyCoverOnlyForAssigned(t *testing.T) {
	a := placeHarnesses(t, 1, targetA(2, 0))[0]
	seedFor(t, a.table, "a", "pool-00000001", 1, StateBusy, testEpoch)
	a.mustScale(offer(newMsg(), 1, testEpoch))
	if a.acq.callCount() != 0 {
		t.Fatal("AcquireJobs for an unassigned entry with only busy cover on a full server (0002 SPEC R213 (2))")
	}
	a.mustScale(assign(newMsg(), 2, testEpoch.Add(time.Second)))
	if a.gh.jitCalls() != 0 || a.placer.WaitingJobs("a") != 1 {
		t.Fatalf("JIT %d, waiting %d; want the assigned entry on the busy cover", a.gh.jitCalls(), a.placer.WaitingJobs("a"))
	}
}

func TestPlaceNoCoverLimitReached(t *testing.T) {
	a := placeHarnesses(t, 5, targetA(1, 0))[0]
	seedStarted(t, a, "a", "pool-00000001", 1)
	a.mustScale(assign(offer(newMsg(), 1, testEpoch), 2, testEpoch))
	if a.acq.callCount() != 0 || a.gh.jitCalls() != 0 {
		t.Fatalf("AcquireJobs %d, JIT %d at max-runners; want 0 and 0 (0002 SPEC R169)", a.acq.callCount(), a.gh.jitCalls())
	}
}

func TestPlaceDoneRunnerNotCounted(t *testing.T) {
	a := placeHarnesses(t, 5, targetA(3, 0))[0]
	seedFor(t, a.table, "a", "pool-00000001", 1, StateDone, testEpoch)
	a.mustScale(withStats(newMsg(), 0, 0))
	if a.gh.jitCalls() != 0 || len(a.gh.removedIDs()) != 0 {
		t.Fatal("a done runner changed the goal")
	}
	// One assigned entry and TotalAssignedJobs 1 twice: no deficit, so no
	// drop, and one runner (0002 SPEC R219, R226, R115).
	a.mustScale(withStats(assign(newMsg(), 1, testEpoch), 1, 0))
	a.mustScale(withStats(newMsg(), 1, 0))
	if a.gh.jitCalls() != 1 || a.placer.WaitingJobs("a") != 0 {
		t.Fatalf("JIT %d, waiting %d; want one runner and the entry kept and covered", a.gh.jitCalls(), a.placer.WaitingJobs("a"))
	}
	if strings.Contains(a.logs.String(), "waiting job dropped") {
		t.Fatal("the done runner made a deficit")
	}
}

func TestPlaceSkippedEntryHoldsRoom(t *testing.T) {
	hs := placeHarnesses(t, 3, targetA(3, 0), targetB(3, 1))
	a, b := hs[0], hs[1]
	backoff(a, "a", time.Minute)
	a.mustScale(assign(newMsg(), 1, testEpoch))
	b.mustScale(withStats(newMsg(), 3, 0)) // three assigned jobs not in the list
	if startedFor(b) != 2 || startedFor(a) != 0 {
		t.Fatalf("starts a %d b %d; want b 2, leaving one slot for a's skipped entry and no idle fill (0002 SPEC R229)",
			startedFor(a), startedFor(b))
	}
}

func TestPlaceIdleFillAfterEntries(t *testing.T) {
	hs := placeHarnesses(t, 2, targetA(2, 2), targetB(2, 0))
	a, b := hs[0], hs[1]
	b.mustScale(assign(assign(newMsg(), 1, testEpoch), 2, testEpoch))
	if startedFor(b) != 2 || startedFor(a) != 0 {
		t.Fatalf("starts a %d b %d; want the entries first (0002 SPEC R187)", startedFor(a), startedFor(b))
	}
	names := b.gh.jitNamesSnapshot()
	b.mustScale(jobStartedOn(jobStartedOn(newMsg(), 1, names[0]), 2, names[1]))
	if startedFor(a) != 0 {
		t.Fatal("idle start on a full server")
	}
	dropRow(b, names[0]) // its job ended
	b.mustScale(nil)
	if startedFor(a) != 1 {
		t.Fatalf("a's idle starts %d once every entry has a runner, want 1", startedFor(a))
	}
}

func TestPlaceUnlistedBeforeIdle(t *testing.T) {
	hs := placeHarnesses(t, 2, targetA(3, 1), targetB(2, 0))
	a, b := hs[0], hs[1]
	b.mustScale(withStats(newMsg(), 2, 0))
	if startedFor(b) != 2 || startedFor(a) != 0 {
		t.Fatalf("starts a %d b %d; want b's unlisted jobs before a's idle (0002 SPEC R187)", startedFor(a), startedFor(b))
	}
	names := b.gh.jitNamesSnapshot()
	// One runner takes a job; the statistics count it: unlisted 1, cover 1.
	b.mustScale(withStats(jobStartedOn(newMsg(), 9, names[0]), 2, 0))
	// It ends between messages: the value stays 1 (R219).
	dropRow(b, names[0])
	b.mustScale(nil)
	// The remaining idle runner turns busy by the hook: still cover (R228).
	b.table.MarkBusy(names[1], b.clock.Now())
	b.mustScale(nil)
	if n := b.gh.jitCalls(); n != 2 {
		t.Fatalf("b's JIT %d, want no extra start (0002 SPEC R219, R228)", n)
	}
	if startedFor(a) != 1 {
		t.Fatalf("a's idle starts %d, want 1 in the freed slot", startedFor(a))
	}
}

func TestPlaceScaleDownBeyondGoal(t *testing.T) {
	a := placeHarnesses(t, 10, targetA(5, 0))[0]
	for i := range 3 {
		seedFor(t, a.table, "a", fmt.Sprintf("pool-0000000%d", i+1), int64(i+1), StateIdle, testEpoch.Add(time.Duration(i)*time.Second))
	}
	seedFor(t, a.table, "a", "pool-00000009", 9, StateBusy, testEpoch)
	backoff(a, "a", time.Minute)
	a.mustScale(assign(newMsg(), 1, testEpoch))
	// goal = skipped 1; live 4 − 1 = 3 removed, all idle; the busy runner
	// stays.
	if got := a.gh.removedIDs(); !slices.Equal(got, []int64{1, 2, 3}) {
		t.Fatalf("removed %v, want the idle runners above the goal, oldest first (0002 SPEC R185)", got)
	}
	if a.state("pool-00000009") != StateBusy {
		t.Fatal("busy runner touched")
	}
}

func TestPlaceSkippedShareKept(t *testing.T) {
	a := placeHarnesses(t, 10, targetA(5, 0))[0]
	for i := range 3 {
		seedFor(t, a.table, "a", fmt.Sprintf("pool-0000000%d", i+1), int64(i+1), StateIdle, testEpoch.Add(time.Duration(i)*time.Second))
	}
	a.acq.setErr(errTransient)
	a.mustScale(offer(newMsg(), 1, testEpoch)) // acquires with cover, fails: skipped
	if got := a.gh.removedIDs(); !slices.Equal(got, []int64{1, 2}) {
		t.Fatalf("removed %v, want two, keeping one for the skipped entry (0002 SPEC R185)", got)
	}
}

func TestPlaceBackoffAndRateLimitSkip(t *testing.T) {
	t.Run("start backoff", func(t *testing.T) {
		a := placeHarnesses(t, 5, targetA(3, 1))[0]
		backoff(a, "a", time.Minute)
		a.mustScale(assign(newMsg(), 1, testEpoch))
		if a.gh.jitCalls() != 0 || a.placer.WaitingJobs("a") != 1 {
			t.Fatalf("JIT %d, waiting %d during the start backoff; want 0 and 1 (0002 SPEC R186)", a.gh.jitCalls(), a.placer.WaitingJobs("a"))
		}
		a.clock.Advance(time.Minute)
		a.mustScale(nil)
		if a.gh.jitCalls() != 2 {
			t.Fatalf("JIT %d after the backoff, want the entry's runner and the idle one", a.gh.jitCalls())
		}
	})
	t.Run("AcquireJobs rate limited", func(t *testing.T) {
		a := placeHarnesses(t, 5, targetA(3, 0))[0]
		a.limits.Record(retry.OpAcquireJobs, &retry.RateLimitError{RetryAfter: time.Minute})
		a.mustScale(offer(newMsg(), 1, testEpoch))
		if a.acq.callCount() != 0 || a.gh.jitCalls() != 0 {
			t.Fatalf("AcquireJobs %d, JIT %d while rate limited; want 0 and 0", a.acq.callCount(), a.gh.jitCalls())
		}
	})
	t.Run("JIT rate limited", func(t *testing.T) {
		hs := placeHarnesses(t, 5, targetA(3, 1), targetB(3, 1))
		a, b := hs[0], hs[1]
		a.limits.Record(retry.OpGenerateJIT, &retry.RateLimitError{RetryAfter: time.Minute})
		a.mustScale(assign(newMsg(), 1, testEpoch))
		if a.gh.jitCalls() != 0 || startedFor(b) != 0 {
			t.Fatalf("a's JIT %d, b's idle starts %d; want 0 and 0: the skipped entry stops every idle fill (R229)",
				a.gh.jitCalls(), startedFor(b))
		}
	})
}

func TestPlaceMaxRunnersPerTarget(t *testing.T) {
	a := placeHarnesses(t, 10, targetA(2, 1))[0]
	a.mustScale(withStats(offer(offer(offer(newMsg(), 1, testEpoch), 2, testEpoch), 3, testEpoch), 5, 3))
	if n := a.table.Counts("a").All(); n != 2 {
		t.Fatalf("runners %d, want max-runners 2 (0002 SPEC R115)", n)
	}
}

func TestPlaceMaxRunnersSkipsToLaterTarget(t *testing.T) {
	// A is at its max-runners; its earlier entry does not block B's later
	// entry from a free server slot (0002 SPEC R247, R213 (4)).
	hs := placeHarnesses(t, 3, targetA(1, 0), targetB(2, 0))
	a, b := hs[0], hs[1]
	seedStarted(t, a, "a", "pool-0000000a", 1)
	a.mustScale(assign(newMsg(), 1, testEpoch))
	b.mustScale(assign(newMsg(), 2, testEpoch.Add(10*time.Second)))
	if a.acq.callCount() != 0 || startedFor(a) != 0 {
		t.Fatalf("A: AcquireJobs %d, starts %d at max-runners; want 0 and 0", a.acq.callCount(), startedFor(a))
	}
	if b.acq.callCount() != 0 || startedFor(b) != 1 {
		t.Fatalf("B: AcquireJobs %d, starts %d; want 0 and 1 in the same placement", b.acq.callCount(), startedFor(b))
	}
}

func TestPlaceTargetErrorDrainDropsEntries(t *testing.T) {
	a := placeHarnesses(t, 1, targetA(2, 0))[0]
	seedStarted(t, a, "a", "pool-00000001", 1)
	a.mustScale(offer(assign(newMsg(), 1, testEpoch), 2, testEpoch))
	if a.placer.WaitingJobs("a") != 2 {
		t.Fatalf("waiting %d, want 2", a.placer.WaitingJobs("a"))
	}
	a.placer.SetActive("a", false)
	dropRow(a, "pool-00000001")
	a.mustScale(offer(newMsg(), 3, testEpoch))
	if a.placer.WaitingJobs("a") != 0 || a.acq.callCount() != 0 || a.gh.jitCalls() != 0 {
		t.Fatalf("waiting %d, AcquireJobs %d, JIT %d; want the entries dropped and none added (0002 SPEC R210, R220)",
			a.placer.WaitingJobs("a"), a.acq.callCount(), a.gh.jitCalls())
	}
	a.placer.SetActive("a", true)
	a.mustScale(nil)
	if a.acq.callCount() != 0 || a.gh.jitCalls() != 0 {
		t.Fatal("dropped entries came back")
	}
}

func TestPlaceSignalDrainNoAcquire(t *testing.T) {
	a := placeHarnesses(t, 5, targetA(3, 1))[0]
	a.placer.Start()
	a.table.SetDraining(DrainSignal)
	a.mustScale(offer(newMsg(), 1, testEpoch))
	a.placer.Kick()
	a.mustScale(nil) // waits for a kicked placement holding the lock
	if a.acq.callCount() != 0 || a.gh.jitCalls() != 0 {
		t.Fatalf("AcquireJobs %d, JIT %d during a signal drain; want 0 and 0 (0002 SPEC R220)", a.acq.callCount(), a.gh.jitCalls())
	}
}

func TestPlaceAfterScaleAndReconcile(t *testing.T) {
	a := placeHarnesses(t, 5, targetA(3, 0))[0]
	backoff(a, "a", 10*time.Second)
	a.mustScale(assign(newMsg(), 1, testEpoch)) // a placement after the message: skipped
	if a.gh.jitCalls() != 0 {
		t.Fatal("start during the backoff")
	}
	r, err := NewReconciler(ReconcilerConfig{
		Targets:   map[string]ReconcileTarget{"a": {Scaler: a.s, Finder: &fakeOrg{}}},
		Resources: a.docker, Remover: a.docker, Processes: a.docker,
		OnRound: a.placer.Kick,
	})
	if err != nil {
		t.Fatal(err)
	}
	a.placer.Start()
	r.Start()
	t.Cleanup(func() {
		r.Stop()
		_ = r.Wait(context.Background())
	})
	a.clock.Advance(reconcileInterval) // the round's end kicks a placement (0002 SPEC R212)
	waitUntil(t, "the entry's runner", func() bool { return startedFor(a) == 1 })
}

func TestPlaceStartsInSameScale(t *testing.T) {
	a := placeHarnesses(t, 5, targetA(3, 0))[0]
	at := a.clock.Now()
	var startedAt time.Time
	a.docker.mu.Lock()
	a.docker.duringStart = func(context.Context, string) { startedAt = a.clock.Now() }
	a.docker.mu.Unlock()
	a.mustScale(offer(newMsg(), 1, testEpoch))
	if startedFor(a) != 1 || !startedAt.Equal(at) {
		t.Fatalf("starts %d at %v; want StartRunner inside the Scale at %v (0002 SPEC R117)", startedFor(a), startedAt, at)
	}
}

func TestPlaceConcurrentTargetsServerLimit(t *testing.T) {
	const limit = 3
	hs := placeHarnesses(t, limit, targetA(3, 1), targetB(3, 1))
	a := hs[0]
	var over atomic.Int32
	a.docker.mu.Lock()
	a.docker.duringStart = func(context.Context, string) {
		if a.table.CountsAll().All() > limit {
			over.Add(1)
		}
	}
	a.docker.mu.Unlock()
	a.placer.Start()
	done := make(chan struct{})
	go func() {
		defer close(done)
		var wg sync.WaitGroup
		for i, h := range hs {
			wg.Go(func() {
				for j := range 10 {
					req := int64(100*i + j + 1)
					if err := h.s.Scale(context.Background(), withStats(offer(newMsg(), req, testEpoch), j%4, 1)); err != nil {
						t.Errorf("Scale: %v", err)
					}
					h.placer.Kick()
					if names := h.gh.jitNamesSnapshot(); j%3 == 2 && len(names) > 0 {
						dropRow(h, names[len(names)-1])
					}
				}
			})
		}
		wg.Wait()
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("deadlock: Scale calls and kicks did not finish in 10 s")
	}
	if over.Load() != 0 || a.table.CountsAll().All() > limit {
		t.Fatalf("over the server limit %d times, %d rows now (0002 SPEC R114)", over.Load(), a.table.CountsAll().All())
	}
}

func TestPlaceWaitingJobs(t *testing.T) {
	a := placeHarnesses(t, 2, targetA(1, 0))[0]
	a.mustScale(assign(assign(newMsg(), 1, testEpoch), 2, testEpoch.Add(time.Second))) // 1 covered, 2 not: max 1
	a.mustScale(takeBack(assign(newMsg(), 3, testEpoch.Add(2*time.Second)), 3))        // taken back
	if n := a.placer.WaitingJobs("a"); n != 2 {
		t.Fatalf("waiting %d, want 2: the uncovered and the taken-back entry (0002 SPEC R222)", n)
	}
	backoff(a, "a", time.Minute)
	a.mustScale(nil)
	if n := a.placer.WaitingJobs("a"); n != 3 {
		t.Fatalf("waiting %d with every entry skipped, want 3", n)
	}
}

func TestPlaceDropLogFields(t *testing.T) {
	a := placeHarnesses(t, 1, targetA(1, 0))[0]
	seedStarted(t, a, "a", "pool-00000001", 1)
	a.mustScale(withStats(assign(newMsg(), 5, testEpoch), 0, 0))
	a.mustScale(withStats(newMsg(), 0, 0))
	want := `"msg":"waiting job dropped; GitHub counts fewer assigned jobs","component":"placement","target":"https://github.com/acme/a","runnerRequestId":5,"workflowRunId":5,"jobDisplayName":"job","totalAssignedJobs":0,"startedRunners":1`
	if !strings.Contains(a.logs.String(), want) {
		t.Fatalf("no R226 line %s in\n%s", want, a.logs.String())
	}
	queued := a.clock.Now().Add(-26 * time.Hour)
	a.mustScale(offer(newMsg(), 6, queued))
	want = fmt.Sprintf(`"msg":"waiting job expired","component":"placement","target":"https://github.com/acme/a","runnerRequestId":6,"queueTime":"%s"`,
		queued.Format(time.RFC3339Nano))
	if !strings.Contains(a.logs.String(), want) {
		t.Fatalf("no R227 line %s in\n%s", want, a.logs.String())
	}
}

func TestPlaceExpireEachPlacement(t *testing.T) {
	a := placeHarnesses(t, 1, targetA(1, 0))[0]
	seedStarted(t, a, "a", "pool-00000001", 1)
	a.mustScale(offer(newMsg(), 1, a.clock.Now()))
	a.clock.Advance(forgetAfter)
	a.mustScale(nil)
	if a.placer.WaitingJobs("a") != 1 {
		t.Fatal("expired at exactly 25 h")
	}
	a.clock.Advance(time.Second)
	a.placer.Start()
	a.placer.Kick()
	waitUntil(t, "the entry expired by a kicked placement (0002 SPEC R227)", func() bool {
		return a.placer.WaitingJobs("a") == 0
	})
}

func TestPlaceUnknownAvailableWarn(t *testing.T) {
	a := placeHarnesses(t, 1, targetA(1, 0))[0]
	seedStarted(t, a, "a", "pool-00000001", 1)
	a.mustScale(withStats(offer(newMsg(), 1, testEpoch), 0, 1))
	const warn = "GitHub counts more available jobs than the scaler knows"
	if strings.Contains(a.logs.String(), warn) {
		t.Fatal("WARN with TotalAvailableJobs equal to the known entries")
	}
	a.mustScale(withStats(newMsg(), 0, 2))
	if !strings.Contains(a.logs.String(), `"msg":"`+warn+`","component":"placement","target":"https://github.com/acme/a","github":2,"known":1`) {
		t.Fatalf("no R221 WARN in\n%s", a.logs.String())
	}
}

func TestPlaceOneEntryOneStart(t *testing.T) {
	t.Run("entry only", func(t *testing.T) {
		a := placeHarnesses(t, 5, targetA(3, 0))[0]
		a.mustScale(offer(newMsg(), 1, testEpoch))
		a.mustScale(nil)
		if a.gh.jitCalls() != 1 {
			t.Fatalf("JIT %d over two placements, want 1", a.gh.jitCalls())
		}
	})
	t.Run("unlisted 1, no cover", func(t *testing.T) {
		a := placeHarnesses(t, 5, targetA(3, 0))[0]
		a.mustScale(withStats(newMsg(), 1, 0))
		if a.gh.jitCalls() != 1 {
			t.Fatalf("JIT %d, want one start in stage 5 and none in stage 6", a.gh.jitCalls())
		}
	})
	t.Run("failed start", func(t *testing.T) {
		a := placeHarnesses(t, 5, targetA(3, 0))[0]
		a.docker.setStartErr(func(string) error { return errTransient })
		a.mustScale(offer(newMsg(), 1, testEpoch))
		if a.gh.jitCalls() != 1 || a.placer.WaitingJobs("a") != 1 {
			t.Fatalf("JIT %d, waiting %d; want one try and the entry skipped, not covered (0002 SPEC R222, R229)",
				a.gh.jitCalls(), a.placer.WaitingJobs("a"))
		}
	})
}

func TestPlaceGoalWhenStage6Skipped(t *testing.T) {
	hs := placeHarnesses(t, 10, targetA(4, 1), targetB(2, 0))
	a, b := hs[0], hs[1]
	for i := range 3 {
		seedFor(t, a.table, "a", fmt.Sprintf("pool-0000000%d", i+1), int64(i+1), StateIdle, testEpoch.Add(time.Duration(i)*time.Second))
	}
	backoff(b, "b", time.Minute)
	b.mustScale(assign(newMsg(), 1, testEpoch)) // skipped: stage 6 starts nothing
	if got := a.gh.removedIDs(); !slices.Equal(got, []int64{1, 2}) {
		t.Fatalf("a's removals %v, want two above its goal of idle 1 (0002 SPEC R185, R229)", got)
	}
}

func TestPlaceRechecksBeforeEachCall(t *testing.T) {
	t.Run("signal drain", func(t *testing.T) {
		a := placeHarnesses(t, 5, targetA(3, 0))[0]
		a.docker.mu.Lock()
		a.docker.duringStart = func(context.Context, string) { a.table.SetDraining(DrainSignal) }
		a.docker.mu.Unlock()
		a.mustScale(offer(assign(newMsg(), 1, testEpoch), 2, testEpoch.Add(time.Second)))
		if a.gh.jitCalls() != 1 || a.acq.callCount() != 0 {
			t.Fatalf("JIT %d, AcquireJobs %d after the drain began; want 1 and 0 (0002 SPEC R220)", a.gh.jitCalls(), a.acq.callCount())
		}
	})
	t.Run("target drain mark", func(t *testing.T) {
		hs := placeHarnesses(t, 5, targetA(3, 0), targetB(3, 0))
		a, b := hs[0], hs[1]
		a.gh.mu.Lock()
		a.gh.duringJIT = func(string) { a.table.SetTargetDraining("a") }
		a.gh.mu.Unlock()
		a.mustScale(offer(assign(newMsg(), 1, testEpoch), 2, testEpoch.Add(time.Second)))
		b.mustScale(assign(newMsg(), 3, testEpoch.Add(2*time.Second)))
		if a.gh.jitCalls() != 1 || a.acq.callCount() != 0 || b.gh.jitCalls() != 1 {
			t.Fatalf("a's JIT %d, AcquireJobs %d, b's JIT %d; want a stopped after its mark, b served", a.gh.jitCalls(), a.acq.callCount(), b.gh.jitCalls())
		}
	})
}

func TestPlaceUsesServerContext(t *testing.T) {
	hs := placeHarnesses(t, 5, targetA(3, 0), targetB(3, 0))
	a, b := hs[0], hs[1]
	backoff(a, "a", time.Second)
	a.mustScale(offer(newMsg(), 1, testEpoch)) // waits for the backoff
	a.clock.Advance(time.Second)
	// The next placement runs from b's Scale; b's listener ends during a's
	// AcquireJobs.
	ctx, cancel := context.WithCancel(context.Background())
	var acquireCtxErr error
	a.acq.mu.Lock()
	a.acq.hook = func(c context.Context, _ []int64) {
		cancel()
		acquireCtxErr = c.Err()
	}
	a.acq.mu.Unlock()
	if err := b.s.Scale(ctx, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("b's Scale = %v, want its own cancellation (message not acknowledged)", err)
	}
	if acquireCtxErr != nil || startedFor(a) != 1 {
		t.Fatalf("a's AcquireJobs context %v, a's starts %d; want the server context and a's runner (0002 PLAN M14)",
			acquireCtxErr, startedFor(a))
	}
}

func TestPlaceNoSessionSkipsUnassigned(t *testing.T) {
	a := placeHarnesses(t, 5, targetA(3, 0))[0]
	a.placer.SetSession("a", nil)
	a.mustScale(assign(offer(newMsg(), 1, testEpoch), 2, testEpoch))
	if a.acq.callCount() != 0 || a.gh.jitCalls() != 1 || a.placer.WaitingJobs("a") != 1 {
		t.Fatalf("AcquireJobs %d, JIT %d, waiting %d; want 0, the assigned entry's runner, 1 (0002 SPEC R213, R222)",
			a.acq.callCount(), a.gh.jitCalls(), a.placer.WaitingJobs("a"))
	}
}

func TestRemoveIdleTwiceTolerated(t *testing.T) {
	for _, tt := range []struct {
		name string
		err  error
	}{
		{"404", statusError(404)},
		{"permanent", statusError(400)},
	} {
		t.Run(tt.name, func(t *testing.T) {
			a := placeHarnesses(t, 5, targetA(3, 0))[0]
			seedFor(t, a.table, "a", "pool-00000001", 1, StateIdle, testEpoch)
			inFirst, release := make(chan struct{}), make(chan struct{})
			var calls atomic.Int32
			a.gh.setRemoveErr(func(int64) error {
				if calls.Add(1) == 1 {
					close(inFirst)
					<-release
					return tt.err
				}
				return nil
			})
			// Reconciliation's removal enters RemoveRunner first and waits.
			recon := make(chan error, 1)
			go func() { recon <- a.s.removeIdleRunner(context.Background(), "pool-00000001") }()
			<-inFirst
			// The placement removes the same runner meanwhile.
			a.placer.Start()
			a.placer.Kick()
			waitUntil(t, "the placement removed the runner", func() bool { return a.gone("pool-00000001") })
			close(release)
			if err := <-recon; err != nil {
				t.Fatal(err)
			}
			if got := a.docker.removedNames(); !slices.Equal(got, []string{"pool-00000001"}) {
				t.Fatalf("resources removed %v, want once", got)
			}
			if a.errs.count() != 0 {
				t.Fatalf("recorded %+v, want nothing for the second RemoveRunner", a.errs.last())
			}
		})
	}
}

// TestPlaceTableAndListOneSection: no placement runs between a message's
// table steps and its list update (0002 PLAN M14-1). The busy mark of the
// JobStarted runs a placement as a kick would, if it can take the Placer's
// lock: it would see the runner started while its entry is still listed and
// start a second runner that R185 removes later (0002 SPEC R185, R224).
func TestPlaceTableAndListOneSection(t *testing.T) {
	a := placeHarnesses(t, 5, targetA(3, 0))[0]
	seedFor(t, a.table, "a", "pool-00000001", 1, StateIdle, testEpoch)
	a.mustScale(assign(newMsg(), 1, testEpoch)) // the idle runner covers job 1
	if a.gh.jitCalls() != 0 {
		t.Fatalf("JIT %d with the entry covered, want 0", a.gh.jitCalls())
	}
	var between atomic.Int32
	a.mu.Lock()
	a.onBusy = func(string) {
		if a.placer.mu.TryLock() {
			between.Add(1)
			a.placer.placeLocked()
			a.placer.mu.Unlock()
		}
	}
	a.mu.Unlock()
	a.mustScale(jobStartedOn(newMsg(), 1, "pool-00000001"))
	if between.Load() != 0 || a.gh.jitCalls() != 0 {
		t.Fatalf("placements between table and list %d, JIT %d; want 0 and 0", between.Load(), a.gh.jitCalls())
	}
}

// TestPlaceSessionLostClearsSession: after SessionLost the next placement
// makes no AcquireJobs on the dead session; the session loop's new session
// gets it (0002 SPEC R213 (1), R216).
func TestPlaceSessionLostClearsSession(t *testing.T) {
	a := placeHarnesses(t, 5, targetA(3, 0))[0]
	a.acq.setErr(errSessionLost)
	a.mustScale(offer(newMsg(), 1, testEpoch))
	a.mustScale(nil)
	if n := a.acq.callCount(); n != 1 || a.placer.WaitingJobs("a") != 1 {
		t.Fatalf("AcquireJobs %d, waiting %d; want 1 and the entry kept without a session", n, a.placer.WaitingJobs("a"))
	}
	a.acq.setErr(nil)
	a.placer.SetSession("a", a.acq) // the new session
	a.mustScale(nil)
	if n := a.acq.callCount(); n != 2 || startedFor(a) != 1 {
		t.Fatalf("AcquireJobs %d, starts %d on the new session; want 2 and 1", n, startedFor(a))
	}
}

// TestPlaceWaitingJobsWhenHalted: a placement that ends at once in a drain
// still refreshes waitingJobs; it covered nothing (0002 SPEC R222, R220).
func TestPlaceWaitingJobsWhenHalted(t *testing.T) {
	a := placeHarnesses(t, 5, targetA(3, 0))[0]
	a.table.SetDraining(DrainSignal)
	a.mustScale(offer(offer(newMsg(), 1, testEpoch), 2, testEpoch))
	if n := a.placer.WaitingJobs("a"); n != 2 || a.acq.callCount() != 0 {
		t.Fatalf("waiting %d, AcquireJobs %d; want 2 and 0", n, a.acq.callCount())
	}
}
