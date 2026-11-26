package scaler

import (
	"context"
	"slices"
	"sync"
	"testing"
	"time"
)

// assertTimerAt checks that runner name, busy since busyAt, is not denied
// just before busyAt+180 s, is denied exactly then, and is removed through
// the deny path once its container stops (R81).
func assertTimerAt(t *testing.T, h *harness, name string, id int64, busyAt time.Time) {
	t.Helper()
	h.clock.Advance(busyAt.Add(busyTimeout - time.Millisecond).Sub(h.clock.Now()))
	if n := len(h.docker.runnerRemovedNames()); n != 0 || h.gone(name) || verdict(t, h, name) != VerdictPending {
		t.Fatalf("at 179.999 s: RemoveContainer calls = %d, gone %v, verdict %v; want nothing yet",
			n, h.gone(name), verdict(t, h, name))
	}
	h.clock.Advance(time.Millisecond)
	// Without a container to wait for, the removal may have ended already.
	if info, ok := h.table.Get(name); ok && info.Verdict != VerdictDeny {
		t.Fatalf("verdict at 180 s = %v, want deny", info.Verdict)
	}
	// The hook kills the Worker on the deny, and the container stops.
	h.docker.exit(name)
	waitDenied(t, h, name, id)
	if !h.clock.Now().Equal(busyAt.Add(busyTimeout)) {
		t.Errorf("clock = %v, want busy + 180 s", h.clock.Now())
	}
}

// TestBusyTimerFiresAt180s: each busy source starts the timer, which denies
// a runner without an allow verdict exactly 180 s later (R53, R81, R83,
// R91, R103).
func TestBusyTimerFiresAt180s(t *testing.T) {
	const name, id = "pool-a", int64(101)
	sources := map[string]func(t *testing.T, h *harness, tok string){
		"hook query": func(t *testing.T, h *harness, tok string) {
			if _, ok := h.table.Authenticate(name, tok, h.clock.Now()); !ok {
				t.Fatal("token refused")
			}
		},
		"JobStarted": func(t *testing.T, h *harness, _ string) {
			h.mustScale(withStarted(statsMsg(1, 1), name, "job-1"))
		},
		"JobStillRunningError during drain": func(t *testing.T, h *harness, _ string) {
			h.table.SetDraining(DrainSignal)
			h.gh.setRemoveErr(func(int64) error { return errStillRunning() })
			if err := h.s.RemoveIdle(context.Background()); err != nil {
				t.Fatal(err)
			}
			h.gh.setRemoveErr(nil)
			h.gh.mu.Lock()
			h.gh.removed = nil
			h.gh.mu.Unlock()
		},
		"Runner.Worker process (R83)": func(t *testing.T, h *harness, _ string) {
			r, _ := newTestReconciler(t, h)
			h.docker.addRunner(name, "running", "running")
			h.docker.setProcs(name, "/home/runner/bin/Runner.Worker", "spawnclient", "1", "2")
			roundAndList(t, r)
		},
		"repository runner list (R103)": func(t *testing.T, h *harness, _ string) {
			r, repo := newTestReconciler(t, h)
			h.docker.addRunner(name, "running", "running")
			repo.set(RepoRunner{ID: id, Name: name, Busy: true})
			roundAndList(t, r)
		},
	}
	for source, markBusy := range sources {
		t.Run(source, func(t *testing.T) {
			h, _ := newJudgeHarness(t)
			tok := seedToken(t, h, name, id, StateIdle)
			h.clock.Advance(7 * time.Second)

			busyAt := h.clock.Now()
			markBusy(t, h, tok)
			if info, _ := h.table.Get(name); !info.BusyAt.Equal(busyAt) {
				t.Fatalf("busy at %v, want %v", info.BusyAt, busyAt)
			}
			// The table's observer fired once next to the job check hook,
			// whose firing assertTimerAt proves.
			if got := h.busyCalls(); !slices.Equal(got, []string{name}) {
				t.Errorf("observer calls = %v, want [%s]", got, name)
			}
			assertTimerAt(t, h, name, id, busyAt)
		})
	}
}

// TestBusyTimerDenyRetries: when the timer owns the deny path, the deny is
// recorded and a failed container removal is retried every 2 s until
// confirmed (R81, PLAN M4-2).
func TestBusyTimerDenyRetries(t *testing.T) {
	h, _ := newJudgeHarness(t)
	tok := seedToken(t, h, "pool-a", 101, StateBusy)
	h.docker.setRemoveContainerErr(fixed(statusError(400), errTransient))
	calls := func() int { return len(h.docker.runnerRemovedNames()) }

	h.clock.Advance(busyTimeout)
	// No container to wait for, so the removal begins at once.
	eventually(t, "the first attempt and its retry timer", func() bool {
		return calls() == 1 && h.clock.pending() == 1
	})
	if v, ok := h.table.Authenticate("pool-a", tok, h.clock.Now()); !ok || v != VerdictDeny {
		t.Errorf("hook reads %v, %v; want deny", v, ok)
	}
	h.clock.Advance(denyRetryInterval - time.Millisecond)
	if n := calls(); n != 1 {
		t.Fatalf("RemoveContainer calls before 2 s = %d, want 1", n)
	}
	h.clock.Advance(time.Millisecond)
	if n := calls(); n != 2 || h.gone("pool-a") {
		t.Fatalf("RemoveContainer calls at 2 s = %d, gone %v; want 2, still there", n, h.gone("pool-a"))
	}
	h.clock.Advance(denyRetryInterval)
	if n := calls(); n != 3 {
		t.Fatalf("RemoveContainer calls at 4 s = %d, want 3", n)
	}
	if !h.gone("pool-a") || !slices.Equal(h.gh.removedIDs(), []int64{101}) {
		t.Errorf("runner not removed once the container went: gone %v, RemoveRunner %v",
			h.gone("pool-a"), h.gh.removedIDs())
	}
}

// TestBusyTimerCountsFromFirstSource: a later busy source does not move the
// timer.
func TestBusyTimerCountsFromFirstSource(t *testing.T) {
	h, _ := newJudgeHarness(t)
	tok := seedToken(t, h, "pool-a", 101, StateIdle)
	busyAt := h.clock.Now()
	h.table.MarkBusy("pool-a", busyAt)
	h.clock.Advance(100 * time.Second)
	h.table.Authenticate("pool-a", tok, h.clock.Now())
	h.table.MarkBusyUnjudged("pool-a", h.clock.Now())

	assertTimerAt(t, h, "pool-a", 101, busyAt)
}

// TestBusyTimerAllow: an allow before 180 s keeps the runner, whether the
// recorded allow stopped the timer or the timer finds the verdict when it
// fires.
func TestBusyTimerAllow(t *testing.T) {
	t.Run("record", func(t *testing.T) {
		h, j := newJudgeHarness(t)
		seedToken(t, h, "pool-a", 101, StateBusy)
		h.clock.Advance(busyTimeout - time.Second)
		if v := j.Record("pool-a", "push", allowed); v != VerdictAllow {
			t.Fatalf("Record = %v, want allow", v)
		}
		h.clock.Advance(time.Hour)
		if n := h.docker.calls(); n != 0 || verdict(t, h, "pool-a") != VerdictAllow {
			t.Errorf("Docker calls = %d, verdict %v; want 0 and allow", n, verdict(t, h, "pool-a"))
		}
	})
	t.Run("verdict at firing", func(t *testing.T) {
		h, _ := newJudgeHarness(t)
		seedToken(t, h, "pool-a", 101, StateBusy)
		if !h.table.Allow("pool-a") {
			t.Fatal("Allow = false")
		}
		h.clock.Advance(time.Hour)
		if n := h.docker.calls(); n != 0 || h.state("pool-a") != StateBusy {
			t.Errorf("Docker calls = %d, state %v; want 0 and busy", n, h.state("pool-a"))
		}
	})
}

// TestBusyTimerDuringDrain: the timer runs on both drain kinds, so a job
// whose JobStarted never came is removed (R92).
func TestBusyTimerDuringDrain(t *testing.T) {
	for _, kind := range []DrainKind{DrainSignal, DrainError} {
		t.Run(kind.String(), func(t *testing.T) {
			h, _ := newJudgeHarness(t)
			seedToken(t, h, "pool-a", 101, StateIdle)
			h.table.SetDraining(kind)
			busyAt := h.clock.Now()
			h.table.MarkBusy("pool-a", busyAt)
			assertTimerAt(t, h, "pool-a", 101, busyAt)
		})
	}
}

// TestBusyTimerFinalize: a BusyFunc call that comes just after finalize
// began arms nothing, and a timer that fires after finalize began calls
// nothing (M4-3).
func TestBusyTimerFinalize(t *testing.T) {
	t.Run("arm", func(t *testing.T) {
		h, j := newJudgeHarness(t)
		seedToken(t, h, "pool-a", 101, StateBusy)
		h.finalize()
		timers := h.clock.pending()
		j.BusyTimers().arm("pool-b", h.clock.Now())
		if n := h.clock.pending(); n != timers {
			t.Errorf("pending timers = %d, want %d", n, timers)
		}
	})
	t.Run("fire", func(t *testing.T) {
		h, _ := newJudgeHarness(t)
		seedToken(t, h, "pool-a", 101, StateBusy)
		h.table.BeginFinalize(h.clock.Now().Add(time.Hour))
		h.clock.Advance(busyTimeout)
		if n := h.docker.calls() + h.gh.calls(); n != 0 {
			t.Errorf("calls = %d, want 0", n)
		}
		if st := h.state("pool-a"); st != StateBusy {
			t.Errorf("state = %v, want busy (left to finalize)", st)
		}
	})
}

// TestBusyTimersStopAndWait: Stop prevents every pending timer, Wait then
// returns at once, and no timer is armed afterwards.
func TestBusyTimersStopAndWait(t *testing.T) {
	h, j := newJudgeHarness(t)
	for i, n := range []string{"pool-a", "pool-b", "pool-c"} {
		seedToken(t, h, n, int64(101+i), StateBusy)
	}
	b := j.BusyTimers()
	b.Stop()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := b.Wait(ctx); err != nil {
		t.Fatalf("Wait: %v", err)
	}
	b.arm("pool-a", h.clock.Now())
	h.clock.Advance(time.Hour)
	if n := h.docker.calls(); n != 0 {
		t.Errorf("Docker calls = %d, want 0", n)
	}
}

// TestBusyTimerAllowRace: an allow that races with the timer either keeps
// the runner allowed or loses to the removal; never both (run with -race).
func TestBusyTimerAllowRace(t *testing.T) {
	h, j := newJudgeHarness(t)
	seedToken(t, h, "pool-a", 101, StateBusy)
	h.clock.Advance(busyTimeout - time.Millisecond)

	var allowed bool
	var wg sync.WaitGroup
	wg.Go(func() { allowed = h.table.Allow("pool-a") })
	wg.Go(func() { h.clock.Advance(time.Millisecond) })
	wg.Wait()
	waitJudge(t, j)

	removed := len(h.docker.runnerRemovedNames())
	switch {
	case allowed && (removed != 0 || h.gone("pool-a")):
		t.Errorf("allowed runner removed: RemoveContainer calls = %d", removed)
	case !allowed && (removed != 1 || !h.gone("pool-a")):
		t.Errorf("refused allow but runner not removed: RemoveContainer calls = %d", removed)
	}
}
