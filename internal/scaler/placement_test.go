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

// Placement tests: fake sessions recording AcquireJobs, fake Docker, fake
// clock. Every placement here runs inside a Scale, synchronously, unless a
// test starts the kick goroutine.

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

func newMsg() *scaleset.RunnerScaleSetMessage { return &scaleset.RunnerScaleSetMessage{MessageID: 1} }

// assigned is a message whose Statistics count n assigned jobs.
func assigned(n int) *scaleset.RunnerScaleSetMessage {
	m := newMsg()
	m.Statistics = &scaleset.RunnerScaleSetStatistic{TotalAssignedJobs: n}
	return m
}

// seedBusy seeds a busy runner of target that received its own JobStarted.
func seedBusy(t *testing.T, h *harness, target, name string, id int64) {
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

// dropRow removes a runner's row as its watcher would after it ended; the
// OnRemoved kick is left unserved unless the kick goroutine runs.
func dropRow(h *harness, name string) {
	h.table.BeginExit(name)
	h.table.MarkRemoved(name)
}

// lastStarted is the name of h's last runner started.
func lastStarted(t *testing.T, h *harness) string {
	t.Helper()
	names := h.gh.jitNamesSnapshot()
	if len(names) == 0 {
		t.Fatal("no runner started")
	}
	return names[len(names)-1]
}

// backoff puts target in its start backoff until d from now (R61).
func backoff(h *harness, target string, d time.Duration) {
	now := h.clock.Now()
	h.table.RecordStartFailure(target, errors.New("failed"), now, now.Add(d))
}

// TestPlaceAcquireAtOnce: every JobAvailable is acquired in the Scale that
// brings it, also when the server is full; AcquireJobs comes before the
// placement's starts.
func TestPlaceAcquireAtOnce(t *testing.T) {
	t.Run("full server", func(t *testing.T) {
		a := placeHarnesses(t, 1, targetA(2, 0))[0]
		seedBusy(t, a, "a", "pool-00000001", 1)
		a.mustScale(withAvailable(newMsg(), 1, 2))
		if got := a.acq.requested(); a.acq.callCount() != 1 || !slices.Equal(got, []int64{1, 2}) {
			t.Fatalf("AcquireJobs %d calls with %v; want one call with [1 2] on a full server", a.acq.callCount(), got)
		}
		if a.gh.jitCalls() != 0 {
			t.Fatalf("JIT %d on a full server, want 0", a.gh.jitCalls())
		}
	})
	t.Run("before the starts", func(t *testing.T) {
		a := placeHarnesses(t, 5, targetA(3, 0))[0]
		order := &callLog{}
		a.acq.mu.Lock()
		a.acq.order = order
		a.acq.mu.Unlock()
		a.gh.mu.Lock()
		a.gh.duringJIT = func(string) { order.add("jit") }
		a.gh.mu.Unlock()
		a.mustScale(withAvailable(assigned(1), 7))
		if got := order.snapshot(); !slices.Equal(got, []string{"acquire 7", "jit"}) {
			t.Fatalf("calls %v, want AcquireJobs, then the assigned job's JIT", got)
		}
	})
}

// TestPlaceDesiredCount: a target's desired count is min(max-runners, idle +
// TotalAssignedJobs); the placement starts runners up to it and removes idle
// runners above it.
func TestPlaceDesiredCount(t *testing.T) {
	for _, tt := range []struct {
		name                 string
		max, idle, assigned  int
		idleRunners, busy    int
		wantStarts, wantRems int
	}{
		{name: "nothing", max: 3},
		{name: "idle only", max: 3, idle: 1, wantStarts: 1},
		{name: "idle plus assigned", max: 4, idle: 1, assigned: 2, wantStarts: 3},
		{name: "capped by max-runners", max: 3, assigned: 5, wantStarts: 3},
		{name: "busy runners count", max: 4, idle: 1, assigned: 2, busy: 2, wantStarts: 1},
		{name: "idle runners count", max: 4, assigned: 2, idleRunners: 1, wantStarts: 1},
		{name: "above desired", max: 4, idle: 1, assigned: 1, idleRunners: 3, wantRems: 1},
		{name: "all idle above", max: 3, idleRunners: 2, wantRems: 2},
	} {
		t.Run(tt.name, func(t *testing.T) {
			a := placeHarnesses(t, 100, targetA(tt.max, tt.idle))[0]
			for i := range tt.idleRunners {
				seedFor(t, a.table, "a", fmt.Sprintf("pool-0000001%d", i), int64(10+i), StateIdle, testEpoch.Add(time.Duration(i)*time.Second))
			}
			for i := range tt.busy {
				seedBusy(t, a, "a", fmt.Sprintf("pool-0000002%d", i), int64(20+i))
			}
			a.mustScale(assigned(tt.assigned))
			if n := startedFor(a); n != tt.wantStarts {
				t.Errorf("starts %d, want %d", n, tt.wantStarts)
			}
			if n := len(a.gh.removedIDs()); n != tt.wantRems {
				t.Errorf("removals %d, want %d", n, tt.wantRems)
			}
		})
	}
	t.Run("logged once per change", func(t *testing.T) {
		a := placeHarnesses(t, 100, targetA(3, 1))[0]
		a.mustScale(assigned(1))
		a.mustScale(assigned(1))
		a.mustScale(nil)
		want := `"msg":"desired runners","targetKey":"a","target":"https://github.com/acme/a","desired":2,"assignedJobs":1,"idle":1,"maxRunners":3`
		if n := strings.Count(a.logs.String(), want); n != 1 {
			t.Fatalf("%d lines %s, want 1 in\n%s", n, want, a.logs.String())
		}
	})
}

// TestPlaceGateFIFOAcrossTargets: with one slot on the server, the slot
// goes to the start asked for first, whichever target asked, not to the
// target first in config order.
func TestPlaceGateFIFOAcrossTargets(t *testing.T) {
	hs := placeHarnesses(t, 1, targetA(2, 0), targetB(2, 0))
	a, b := hs[0], hs[1]
	a.mustScale(assigned(1)) // a asks first and gets the slot
	if startedFor(a) != 1 {
		t.Fatalf("a's starts %d, want 1", startedFor(a))
	}
	a1 := lastStarted(t, a)
	b.mustScale(assigned(1)) // b asks second: no slot
	a.mustScale(assigned(2)) // a asks again, after b
	if startedFor(b) != 0 || startedFor(a) != 1 {
		t.Fatalf("starts a %d, b %d on a full server; want 1 and 0", startedFor(a), startedFor(b))
	}
	if a.placer.WaitingJobs("a") != 1 || b.placer.WaitingJobs("b") != 1 {
		t.Fatalf("waiting a %d, b %d; want 1 and 1", a.placer.WaitingJobs("a"), b.placer.WaitingJobs("b"))
	}

	dropRow(a, a1)
	a.mustScale(nil) // a's own Scale runs the placement; b's ask is older
	if startedFor(a) != 1 || startedFor(b) != 1 {
		t.Fatalf("starts a %d, b %d after the slot freed; want b's ask served first", startedFor(a), startedFor(b))
	}

	dropRow(b, lastStarted(t, b)) // b asks again now, after a's waiting asks
	b.mustScale(nil)
	if startedFor(a) != 2 || startedFor(b) != 1 {
		t.Fatalf("starts a %d, b %d after the second slot freed; want a's older ask served", startedFor(a), startedFor(b))
	}
}

// TestPlaceJobStartsBeforeIdle: when the gate is contested, a start for an
// assigned job goes before an older start that only fills idle (0002 SPEC
// R187); an idle fill also leaves a slot to a job whose target cannot start
// now.
func TestPlaceJobStartsBeforeIdle(t *testing.T) {
	t.Run("job before older idle", func(t *testing.T) {
		hs := placeHarnesses(t, 1, targetA(2, 0), targetB(2, 1))
		a, b := hs[0], hs[1]
		seedBusy(t, a, "a", "pool-0000000a", 1)
		b.mustScale(nil)         // b asks for its idle runner: no slot
		a.mustScale(assigned(2)) // a asks for its second job: no slot
		if startedFor(a)+startedFor(b) != 0 {
			t.Fatal("start on a full server")
		}
		dropRow(a, "pool-0000000a")
		b.mustScale(nil)
		if startedFor(a) != 1 || startedFor(b) != 0 {
			t.Fatalf("starts a %d, b %d; want a's job first", startedFor(a), startedFor(b))
		}
	})
	t.Run("blocked job keeps a slot", func(t *testing.T) {
		hs := placeHarnesses(t, 1, targetA(2, 0), targetB(2, 1))
		a, b := hs[0], hs[1]
		backoff(a, "a", time.Minute)
		a.mustScale(assigned(1))
		b.mustScale(nil)
		if startedFor(a) != 0 || startedFor(b) != 0 {
			t.Fatalf("starts a %d, b %d; want none: the one slot waits for a's job", startedFor(a), startedFor(b))
		}
		a.clock.Advance(time.Minute)
		b.mustScale(nil)
		if startedFor(a) != 1 || startedFor(b) != 0 {
			t.Fatalf("starts a %d, b %d after the backoff; want a's job", startedFor(a), startedFor(b))
		}
	})
	t.Run("job asks keep their own order", func(t *testing.T) {
		// a's idle ask is older than b's job ask, but a's job ask is newer:
		// b's job goes first.
		hs := placeHarnesses(t, 1, targetA(3, 1), targetB(2, 0))
		a, b := hs[0], hs[1]
		seedBusy(t, b, "b", "pool-0000000b", 1)
		a.mustScale(nil)         // a's idle ask
		b.mustScale(assigned(2)) // b's job ask
		a.mustScale(assigned(1)) // a's job ask
		if startedFor(a)+startedFor(b) != 0 {
			t.Fatal("start on a full server")
		}
		dropRow(b, "pool-0000000b")
		a.mustScale(nil)
		if startedFor(a) != 0 || startedFor(b) != 1 {
			t.Fatalf("starts a %d, b %d; want b's older job ask first", startedFor(a), startedFor(b))
		}
	})
	t.Run("target at max-runners holds no slot", func(t *testing.T) {
		hs := placeHarnesses(t, 2, targetA(1, 0), targetB(2, 1))
		a, b := hs[0], hs[1]
		seedFor(t, a.table, "a", "pool-0000000a", 1, StateDone, testEpoch)
		a.mustScale(assigned(1))
		if startedFor(a) != 0 || startedFor(b) != 1 {
			t.Fatalf("starts a %d, b %d; want b's idle runner in the free slot", startedFor(a), startedFor(b))
		}
	})
	t.Run("idle fill beyond the held slots", func(t *testing.T) {
		hs := placeHarnesses(t, 2, targetA(2, 0), targetB(2, 1))
		a, b := hs[0], hs[1]
		backoff(a, "a", time.Minute)
		a.mustScale(assigned(1))
		if startedFor(a) != 0 || startedFor(b) != 1 {
			t.Fatalf("starts a %d, b %d; want b's idle runner in the second slot", startedFor(a), startedFor(b))
		}
	})
}

// TestPlaceServerLimit: no placement starts a runner above the server limit
// (0002 SPEC R114); the jobs left without a runner are waiting jobs.
func TestPlaceServerLimit(t *testing.T) {
	hs := placeHarnesses(t, 2, targetA(5, 0), targetB(5, 1))
	a, b := hs[0], hs[1]
	a.mustScale(assigned(5))
	b.mustScale(assigned(1))
	if n := a.table.CountsAll().All(); n != 2 || startedFor(a) != 2 {
		t.Fatalf("rows %d, a's starts %d; want 2 and 2", n, startedFor(a))
	}
	if a.placer.WaitingJobs("a") != 3 || b.placer.WaitingJobs("b") != 1 {
		t.Fatalf("waiting a %d, b %d; want 3 and 1", a.placer.WaitingJobs("a"), b.placer.WaitingJobs("b"))
	}
	if strings.Contains(a.logs.String(), noRoomLine) {
		t.Fatal("a start reached Table.Add over the limit: the gate did not stop it")
	}
}

// noRoomLine is logged when Table.Add refuses a start; the gate must never
// let a start reach that check over the limit.
const noRoomLine = "runner start deferred; no room under max-runners or the server limit"

// TestPlaceAsksNumberedOnArrival: a target's asks are numbered when its
// Statistics arrive, so the gate serves the target whose count came first,
// not the target first in config order.
func TestPlaceAsksNumberedOnArrival(t *testing.T) {
	hs := placeHarnesses(t, 1, targetA(1, 0), targetB(1, 0))
	a, b := hs[0], hs[1]
	seedFor(t, a.table, "other", "other-00000001", 1, StateBusy, testEpoch) // fills the server
	b.placer.record(b.s, assigned(1))
	a.placer.record(a.s, assigned(1))
	dropRow(a, "other-00000001")
	a.placer.mu.Lock()
	a.placer.placeLocked()
	a.placer.mu.Unlock()
	if startedFor(a) != 0 || startedFor(b) != 1 {
		t.Fatalf("starts a %d, b %d; want b, whose count arrived first", startedFor(a), startedFor(b))
	}
}

// TestPlaceHeldJobSlots: idle fills of other targets leave a slot to each
// job ask of a target that cannot start now, but only to as many as its
// max-runners leaves room for.
func TestPlaceHeldJobSlots(t *testing.T) {
	t.Run("image pending", func(t *testing.T) {
		hs := placeHarnesses(t, 1, targetA(2, 0), targetB(2, 1))
		a, b := hs[0], hs[1]
		// Only the first lookup, a's, finds the image being pulled; a start
		// of b would find it.
		a.images.set(ImageIDs{Runner: "sha256:r", BuildKit: "sha256:b"}, func(n int) error {
			if n == 1 {
				return ErrImagePending
			}
			return nil
		})
		a.mustScale(assigned(1))
		if startedFor(a) != 0 || startedFor(b) != 0 {
			t.Fatalf("starts a %d, b %d; want none: the slot waits for a's job", startedFor(a), startedFor(b))
		}
	})
	t.Run("failed start", func(t *testing.T) {
		hs := placeHarnesses(t, 1, targetA(2, 0), targetB(2, 1))
		a, b := hs[0], hs[1]
		a.gh.setJITErr(func(string) error { return errTransient })
		a.mustScale(assigned(1))
		if a.table.StartStatus("a").Failures != 1 || startedFor(b) != 0 {
			t.Fatalf("a's failures %d, b's starts %d; want 1 and 0", a.table.StartStatus("a").Failures, startedFor(b))
		}
	})
	t.Run("capped by max-runners", func(t *testing.T) {
		// a has two job asks but room for one under max-runners 2 (a done
		// row); the third slot of the server is b's.
		hs := placeHarnesses(t, 3, targetA(2, 0), targetB(2, 1))
		a, b := hs[0], hs[1]
		seedFor(t, a.table, "a", "pool-0000000a", 1, StateDone, testEpoch)
		backoff(a, "a", time.Minute)
		a.mustScale(assigned(2))
		if startedFor(a) != 0 || startedFor(b) != 1 {
			t.Fatalf("starts a %d, b %d; want b's idle runner beside a's one held slot", startedFor(a), startedFor(b))
		}
	})
}

// TestPlacePreemptsIdleForJobs: when the server is full of other targets'
// idle runners, a job ask gets a slot: the placement removes the newest
// idle runner beyond what that target's assigned jobs need, and the next
// placement starts the job's runner. Busy runners are never removed.
func TestPlacePreemptsIdleForJobs(t *testing.T) {
	t.Run("idle runner removed", func(t *testing.T) {
		hs := placeHarnesses(t, 2, targetA(2, 0), targetB(2, 2))
		a, b := hs[0], hs[1]
		b.mustScale(nil)
		if startedFor(b) != 2 {
			t.Fatalf("b's idle starts %d, want 2", startedFor(b))
		}
		newest := lastStarted(t, b)
		a.mustScale(assigned(1))
		if !a.gone(newest) || len(b.gh.removedIDs()) != 1 {
			t.Fatalf("b's removals %v, newest %s gone %v; want only the newest idle runner removed",
				b.gh.removedIDs(), newest, a.gone(newest))
		}
		a.mustScale(nil) // the kicked placement
		if startedFor(a) != 1 || startedFor(b) != 2 {
			t.Fatalf("starts a %d, b %d; want a's job runner in the freed slot", startedFor(a), startedFor(b))
		}
		if n := a.table.CountsAll().All(); n != 2 {
			t.Fatalf("rows %d, want the server limit 2", n)
		}
		if strings.Contains(a.logs.String(), noRoomLine) {
			t.Fatal("a start reached Table.Add over the limit")
		}
	})
	t.Run("busy runners kept", func(t *testing.T) {
		hs := placeHarnesses(t, 2, targetA(2, 0), targetB(2, 0))
		a, b := hs[0], hs[1]
		seedBusy(t, b, "b", "pool-000000b1", 1)
		seedBusy(t, b, "b", "pool-000000b2", 2)
		a.mustScale(assigned(1))
		if n := len(b.gh.removedIDs()); n != 0 || startedFor(a) != 0 {
			t.Fatalf("b's removals %d, a's starts %d; want 0 and 0", n, startedFor(a))
		}
	})
	t.Run("idle runners a target's jobs need kept", func(t *testing.T) {
		hs := placeHarnesses(t, 2, targetA(2, 0), targetB(2, 0))
		a, b := hs[0], hs[1]
		b.mustScale(assigned(2)) // two runners for b's two jobs, still idle
		a.mustScale(assigned(1))
		if n := len(b.gh.removedIDs()); n != 0 {
			t.Fatalf("b's removals %d, want none: its assigned jobs need both", n)
		}
	})
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
					if err := h.s.Scale(context.Background(), withAvailable(assigned(j%4), req)); err != nil {
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
	if strings.Contains(a.logs.String(), noRoomLine) {
		t.Fatal("a start reached Table.Add over the limit: the gate did not stop it")
	}
}

func TestPlaceMaxRunnersSkipsToLaterTarget(t *testing.T) {
	// A is at its max-runners with a done runner; its earlier ask does not
	// block B's later ask from a free server slot.
	hs := placeHarnesses(t, 3, targetA(1, 0), targetB(2, 0))
	a, b := hs[0], hs[1]
	seedFor(t, a.table, "a", "pool-0000000a", 1, StateDone, testEpoch)
	a.mustScale(assigned(1))
	b.mustScale(assigned(1))
	if startedFor(a) != 0 || startedFor(b) != 1 {
		t.Fatalf("starts a %d at max-runners, b %d; want 0 and 1", startedFor(a), startedFor(b))
	}
}

func TestPlaceRowRemovedWithinOneSecond(t *testing.T) {
	hs := placeHarnesses(t, 1, targetA(1, 0), targetB(1, 0))
	a, b := hs[0], hs[1]
	seedBusy(t, a, "a", "pool-0000000a", 1)
	b.mustScale(assigned(1))
	if startedFor(b) != 0 {
		t.Fatal("start on a full server")
	}
	b.placer.Start()
	// The fake clock never moves here: the kicked placement runs with no
	// wait at all, well within R170's 1 s.
	at := a.clock.Now()
	dropRow(a, "pool-0000000a") // OnRemoved kicks a placement (0002 SPEC R170)
	waitUntil(t, "b's runner started", func() bool { return startedFor(b) == 1 })
	if d := a.clock.Now().Sub(at); d >= time.Second {
		t.Fatalf("placement %v after the row left, want within 1 s", d)
	}
}

func TestPlaceAfterReconcile(t *testing.T) {
	a := placeHarnesses(t, 5, targetA(3, 0))[0]
	backoff(a, "a", 10*time.Second)
	a.mustScale(assigned(1)) // a placement after the message: held by the backoff
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
	waitUntil(t, "the job's runner", func() bool { return startedFor(a) == 1 })
}

func TestPlaceStartsInSameScale(t *testing.T) {
	a := placeHarnesses(t, 5, targetA(3, 0))[0]
	at := a.clock.Now()
	var startedAt time.Time
	a.docker.mu.Lock()
	a.docker.duringStart = func(context.Context, string) { startedAt = a.clock.Now() }
	a.docker.mu.Unlock()
	a.mustScale(assigned(1))
	if startedFor(a) != 1 || !startedAt.Equal(at) {
		t.Fatalf("starts %d at %v; want StartRunner inside the Scale at %v (0002 SPEC R117)", startedFor(a), startedAt, at)
	}
}

func TestPlaceBackoffAndRateLimit(t *testing.T) {
	t.Run("start backoff", func(t *testing.T) {
		a := placeHarnesses(t, 5, targetA(3, 1))[0]
		backoff(a, "a", time.Minute)
		a.mustScale(assigned(1))
		if a.gh.jitCalls() != 0 || a.placer.WaitingJobs("a") != 1 {
			t.Fatalf("JIT %d, waiting %d during the start backoff; want 0 and 1 (0002 SPEC R186)", a.gh.jitCalls(), a.placer.WaitingJobs("a"))
		}
		a.mustScale(nil)
		if n := strings.Count(a.logs.String(), "runner start deferred by start backoff"); n != 1 {
			t.Fatalf("%d backoff lines over two placements, want 1", n)
		}
		a.clock.Advance(time.Minute)
		a.mustScale(nil)
		if a.gh.jitCalls() != 2 {
			t.Fatalf("JIT %d after the backoff, want the job's runner and the idle one", a.gh.jitCalls())
		}
	})
	t.Run("JIT rate limited", func(t *testing.T) {
		hs := placeHarnesses(t, 5, targetA(3, 0), targetB(3, 1))
		a, b := hs[0], hs[1]
		a.limits.Record(retry.OpGenerateJIT, &retry.RateLimitError{RetryAfter: time.Minute})
		a.mustScale(assigned(1))
		if a.gh.jitCalls() != 0 || startedFor(b) != 1 {
			t.Fatalf("a's JIT %d, b's starts %d; want 0 and b's idle runner", a.gh.jitCalls(), startedFor(b))
		}
	})
	t.Run("failed start not retried in the placement", func(t *testing.T) {
		a := placeHarnesses(t, 5, targetA(3, 0))[0]
		a.gh.setJITErr(func(string) error { return errTransient })
		a.mustScale(assigned(2))
		if a.gh.jitCalls() != 1 || a.table.StartStatus("a").Failures != 1 {
			t.Fatalf("JIT %d, failures %d; want one try counted", a.gh.jitCalls(), a.table.StartStatus("a").Failures)
		}
	})
}

// TestPlaceDrain: during a drain of the server nothing is acquired or
// started (0002 SPEC R220); a target that leaves running forgets its asks
// and its TotalAssignedJobs, and starts nothing until a new message counts
// its jobs again.
func TestPlaceDrain(t *testing.T) {
	t.Run("signal drain", func(t *testing.T) {
		a := placeHarnesses(t, 5, targetA(3, 1))[0]
		seedFor(t, a.table, "a", "pool-00000001", 1, StateIdle, testEpoch)
		a.placer.Start()
		a.table.SetDraining(DrainSignal)
		a.mustScale(withAvailable(assigned(2), 1))
		a.placer.Kick()
		a.mustScale(nil) // waits for a kicked placement holding the lock
		if a.acq.callCount() != 0 || a.gh.jitCalls() != 0 {
			t.Fatalf("AcquireJobs %d, JIT %d during a signal drain; want 0 and 0", a.acq.callCount(), a.gh.jitCalls())
		}
		if err := a.s.RemoveIdle(context.Background()); err != nil {
			t.Fatal(err)
		}
		if !a.gone("pool-00000001") {
			t.Fatal("idle runner kept during the drain (R53, R91)")
		}
	})
	t.Run("target leaves running", func(t *testing.T) {
		a := placeHarnesses(t, 1, targetA(2, 0))[0]
		seedBusy(t, a, "a", "pool-00000001", 1)
		a.mustScale(assigned(2))
		if a.placer.WaitingJobs("a") != 1 {
			t.Fatalf("waiting %d, want 1", a.placer.WaitingJobs("a"))
		}
		a.placer.SetActive("a", false)
		dropRow(a, "pool-00000001")
		a.mustScale(withAvailable(assigned(2), 3))
		if a.placer.WaitingJobs("a") != 0 || a.acq.callCount() != 0 || a.gh.jitCalls() != 0 {
			t.Fatalf("waiting %d, AcquireJobs %d, JIT %d; want 0, 0, 0", a.placer.WaitingJobs("a"), a.acq.callCount(), a.gh.jitCalls())
		}
		a.placer.SetActive("a", true)
		a.mustScale(nil)
		if a.gh.jitCalls() != 0 {
			t.Fatal("the old session's assigned jobs came back")
		}
		a.mustScale(assigned(1))
		if startedFor(a) != 1 {
			t.Fatalf("starts %d with the new session's count, want 1", startedFor(a))
		}
	})
	t.Run("drain marks during a start", func(t *testing.T) {
		hs := placeHarnesses(t, 5, targetA(3, 0), targetB(3, 0))
		a, b := hs[0], hs[1]
		a.gh.mu.Lock()
		a.gh.duringJIT = func(string) { a.table.SetTargetDraining("a") }
		a.gh.mu.Unlock()
		a.mustScale(assigned(2))
		b.mustScale(assigned(1))
		if a.gh.jitCalls() != 1 || b.gh.jitCalls() != 1 {
			t.Fatalf("a's JIT %d, b's JIT %d; want a stopped after its mark, b served", a.gh.jitCalls(), b.gh.jitCalls())
		}
	})
	t.Run("signal drain during a start", func(t *testing.T) {
		a := placeHarnesses(t, 5, targetA(3, 0))[0]
		a.docker.mu.Lock()
		a.docker.duringStart = func(context.Context, string) { a.table.SetDraining(DrainSignal) }
		a.docker.mu.Unlock()
		a.mustScale(assigned(2))
		if a.gh.jitCalls() != 1 {
			t.Fatalf("JIT %d after the drain began, want 1", a.gh.jitCalls())
		}
	})
}

// TestPlaceUsesServerContext: a placement's starts run on the Placer's
// context, so the end of the listener whose Scale ran the placement does
// not abort another target's start (0002 PLAN M14).
func TestPlaceUsesServerContext(t *testing.T) {
	hs := placeHarnesses(t, 5, targetA(3, 0), targetB(3, 0))
	a, b := hs[0], hs[1]
	backoff(a, "a", time.Second)
	a.mustScale(assigned(1)) // held by the backoff
	a.clock.Advance(time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	var startCtxErr error
	a.docker.mu.Lock()
	a.docker.duringStart = func(c context.Context, _ string) {
		cancel()
		startCtxErr = c.Err()
	}
	a.docker.mu.Unlock()
	if err := b.s.Scale(ctx, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("b's Scale = %v, want its own cancellation (message not acknowledged)", err)
	}
	if startCtxErr != nil || startedFor(a) != 1 {
		t.Fatalf("a's start context %v, a's starts %d; want the server context and a's runner", startCtxErr, startedFor(a))
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

// TestPlacementStartPending: a start that finds an image being pulled
// gives its counts back, logs one INFO line, counts no failure and starts
// nothing more for that target in the placement (0003 SPEC R319, 0002 SPEC
// R212).
func TestPlacementStartPending(t *testing.T) {
	t.Run("counts restored", func(t *testing.T) {
		// One slot on the server: a's pending start must give it back, or
		// b's idle runner would find no room.
		hs := placeHarnesses(t, 1, targetA(1, 1), targetB(1, 1))
		a, b := hs[0], hs[1]
		a.images.set(ImageIDs{Runner: "sha256:r", BuildKit: "sha256:b"}, func(n int) error {
			if n == 1 {
				return ErrImagePending
			}
			return nil
		})
		a.mustScale(nil)
		if startedFor(a) != 0 || startedFor(b) != 1 {
			t.Fatalf("starts a %d, b %d; want 0 and 1", startedFor(a), startedFor(b))
		}
		if n := a.images.callCount(); n != 2 {
			t.Fatalf("image lookups %d, want 2: a's pending start is not tried again", n)
		}
		if st := a.table.StartStatus("a"); st.Failures != 0 || !st.NextStartAt.IsZero() {
			t.Fatalf("a's start status %+v, want no failure", st)
		}
		if n := strings.Count(a.logs.String(), "runner start deferred; image being pulled"); n != 1 {
			t.Fatalf("%d deferred lines, want 1", n)
		}
	})
	t.Run("assigned jobs", func(t *testing.T) {
		a := placeHarnesses(t, 5, targetA(3, 0))[0]
		a.images.set(ImageIDs{}, func(int) error { return ErrImagePending })
		a.mustScale(assigned(2))
		if n := a.images.callCount(); n != 1 {
			t.Fatalf("image lookups %d, want 1", n)
		}
		if a.gh.jitCalls() != 0 || len(a.table.List()) != 0 {
			t.Fatal("a JIT request or a row while the image is pulled")
		}
		if n := a.placer.WaitingJobs("a"); n != 2 {
			t.Fatalf("waiting %d, want both jobs", n)
		}
		if a.table.StartStatus("a").Failures != 0 {
			t.Fatal("a deferred start counted as a failure")
		}
		// The pull ended: the next placement starts both.
		a.images.set(ImageIDs{Runner: "sha256:r", BuildKit: "sha256:b"}, nil)
		a.mustScale(nil)
		if n := startedFor(a); n != 2 {
			t.Fatalf("starts %d after the pull, want 2", n)
		}
	})
}
