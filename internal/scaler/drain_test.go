package scaler

import (
	"context"
	"errors"
	"runtime"
	"slices"
	"testing"
	"time"
)

// startDrain runs Drain in a goroutine and returns its result channel.
func startDrain(h *harness, ctx context.Context, kind DrainKind, timeout time.Duration) <-chan DrainResult {
	ch := make(chan DrainResult, 1)
	go func() { ch <- h.s.Drain(ctx, kind, timeout) }()
	return ch
}

// waitPending waits until the fake clock has at least n pending timers.
func waitPending(t *testing.T, c *fakeClock, n int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for c.pending() < n {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %d timers (have %d)", n, c.pending())
		}
		runtime.Gosched()
	}
}

// waitUntil waits until cond holds, with a failsafe timeout.
func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		runtime.Gosched()
	}
}

func drainResult(t *testing.T, ch <-chan DrainResult) DrainResult {
	t.Helper()
	select {
	case r := <-ch:
		return r
	case <-time.After(10 * time.Second):
		t.Fatal("Drain did not return")
		return 0
	}
}

func notReturned(t *testing.T, ch <-chan DrainResult) {
	t.Helper()
	select {
	case r := <-ch:
		t.Fatalf("Drain returned %v early", r)
	default:
	}
}

// remove takes a runner out of the table as its watcher would.
func remove(h *harness, name string) {
	h.table.BeginExitAndRevoke(name)
	h.table.MarkRemoved(name)
}

func TestDrainSignalWaitsForBusyDoneAndStarting(t *testing.T) {
	h := newHarness(t, testTarget(4, 0))
	now := h.clock.Now()
	seed(t, h.table, "pool-00000001", 1, StateBusy, now)
	seed(t, h.table, "pool-00000002", 2, StateDone, now)
	seed(t, h.table, "pool-00000003", 3, StateStarting, now)
	seed(t, h.table, "pool-00000004", 4, StateExiting, now)

	done := startDrain(h, context.Background(), DrainSignal, 10*time.Minute)
	// The timeout timer and the first poll timer.
	waitPending(t, h.clock, 2)
	if h.table.Drain() != DrainSignal {
		t.Fatalf("drain = %v, want signal", h.table.Drain())
	}

	for _, name := range []string{"pool-00000001", "pool-00000002"} {
		remove(h, name)
		h.clock.Advance(drainPollInterval)
		waitPending(t, h.clock, 2)
		notReturned(t, done)
	}
	// The starting runner is still waited for; an exiting row is not.
	remove(h, "pool-00000003")
	h.clock.Advance(drainPollInterval)
	if r := drainResult(t, done); r != DrainCompleted {
		t.Fatalf("Drain = %v, want completed", r)
	}
}

func TestDrainErrorDoesNotWaitForStarting(t *testing.T) {
	h := newHarness(t, testTarget(4, 0))
	now := h.clock.Now()
	seed(t, h.table, "pool-00000001", 1, StateDone, now)
	seed(t, h.table, "pool-00000003", 3, StateStarting, now)

	done := startDrain(h, context.Background(), DrainError, 10*time.Minute)
	waitPending(t, h.clock, 2)
	notReturned(t, done)
	remove(h, "pool-00000001")
	h.clock.Advance(drainPollInterval)
	if r := drainResult(t, done); r != DrainCompleted {
		t.Fatalf("Drain = %v, want completed", r)
	}
}

func TestDrainKeepsSignalKind(t *testing.T) {
	h := newHarness(t, testTarget(2, 0))
	h.table.SetDraining(DrainSignal)
	if r := h.s.Drain(context.Background(), DrainError, time.Minute); r != DrainCompleted {
		t.Fatalf("Drain = %v, want completed", r)
	}
	if h.table.Drain() != DrainSignal {
		t.Fatalf("drain = %v, want signal kept (R100)", h.table.Drain())
	}
}

func TestDrainTimesOut(t *testing.T) {
	h := newHarness(t, testTarget(2, 0))
	seed(t, h.table, "pool-00000001", 1, StateBusy, h.clock.Now())
	timeout := 12 * time.Second
	done := startDrain(h, context.Background(), DrainSignal, timeout)
	waitPending(t, h.clock, 2)
	h.clock.Advance(timeout - time.Nanosecond)
	waitPending(t, h.clock, 2)
	notReturned(t, done)
	h.clock.Advance(time.Nanosecond)
	if r := drainResult(t, done); r != DrainTimedOut {
		t.Fatalf("Drain = %v, want timed out", r)
	}
}

// A second signal ends the drain at once, even while a Scale holds the
// scale-down lock (the background RemoveIdle waits for it).
func TestDrainInterruptedWhileScaleHoldsLock(t *testing.T) {
	h := newHarness(t, testTarget(2, 0))
	now := h.clock.Now()
	seed(t, h.table, "pool-00000001", 1, StateBusy, now)
	seed(t, h.table, "pool-00000002", 2, StateIdle, now)
	h.s.scaleMu.Lock()
	ctx, cancel := context.WithCancel(context.Background())
	done := startDrain(h, ctx, DrainSignal, time.Hour)
	waitPending(t, h.clock, 2)
	cancel()
	if r := drainResult(t, done); r != DrainInterrupted {
		t.Fatalf("Drain = %v, want interrupted", r)
	}
	// The background scale-down still counts as work in flight.
	waitCtx, waitCancel := context.WithTimeout(context.Background(), time.Millisecond)
	if err := h.s.WaitInFlight(waitCtx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("WaitInFlight = %v, want the drain's scale-down still counted", err)
	}
	waitCancel()
	// The daemon finalizes next; the waiting pass then calls nothing.
	h.finalize()
	h.s.scaleMu.Unlock()
	if err := h.s.WaitInFlight(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := h.gh.calls(); n != 0 {
		t.Fatalf("RemoveRunner calls = %d, want 0 once finalize began", n)
	}
}

func TestDrainInterruptedBeforeStart(t *testing.T) {
	h := newHarness(t, testTarget(2, 0))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if r := h.s.Drain(ctx, DrainError, time.Hour); r != DrainInterrupted {
		t.Fatalf("Drain = %v, want interrupted", r)
	}
}

// Without messages the drain removes idle runners itself (R37, R91), and a
// runner that took a job meanwhile is waited for as busy.
func TestDrainRemovesIdleWithoutMessages(t *testing.T) {
	h := newHarness(t, testTarget(3, 0))
	now := h.clock.Now()
	seed(t, h.table, "pool-00000001", 1, StateIdle, now)
	seed(t, h.table, "pool-00000002", 2, StateIdle, now)
	h.gh.setRemoveErr(func(id int64) error {
		if id == 2 {
			return errStillRunning()
		}
		return nil
	})

	done := startDrain(h, context.Background(), DrainSignal, time.Hour)
	waitUntil(t, "idle runners handled", func() bool {
		_, ok := h.table.Get("pool-00000001")
		info, ok2 := h.table.Get("pool-00000002")
		return !ok && ok2 && info.State == StateBusy
	})
	if got := h.gh.removedIDs(); !slices.Equal(got, []int64{1, 2}) {
		t.Fatalf("RemoveRunner IDs = %v, want [1 2]", got)
	}
	if got := h.docker.removedNames(); !slices.Equal(got, []string{"pool-00000001"}) {
		t.Fatalf("removed resources = %v, want only the unregistered runner", got)
	}
	waitPending(t, h.clock, 2)
	notReturned(t, done)

	remove(h, "pool-00000002")
	advanceUntil(t, h.clock, drainPollInterval, func() bool {
		select {
		case r := <-done:
			if r != DrainCompleted {
				t.Fatalf("Drain = %v, want completed", r)
			}
			return true
		default:
			return false
		}
	})
}

// During an error drain a RemoveRunner failure other than
// JobStillRunningError removes only the runner container (R53).
func TestDrainErrorRemovesContainerOnly(t *testing.T) {
	h := newHarness(t, testTarget(2, 0))
	seed(t, h.table, "pool-00000001", 1, StateIdle, h.clock.Now())
	h.gh.setRemoveErr(func(int64) error { return statusError(500) })
	done := startDrain(h, context.Background(), DrainError, time.Hour)
	if r := drainResult(t, done); r != DrainCompleted {
		t.Fatalf("Drain = %v, want completed", r)
	}
	if err := h.s.WaitInFlight(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := h.docker.runnerRemovedNames(); !slices.Equal(got, []string{"pool-00000001"}) {
		t.Fatalf("runner containers removed = %v", got)
	}
	if got := h.docker.removedNames(); len(got) != 0 {
		t.Fatalf("RemoveResources = %v, want none", got)
	}
	info, ok := h.table.Get("pool-00000001")
	if !ok || info.AbandonReason != AbandonErrorDrain {
		t.Fatalf("row = %+v, %v; want abandoned for error drain", info, ok)
	}
}

func TestDrainResultString(t *testing.T) {
	for r, want := range map[DrainResult]string{
		DrainCompleted: "completed", DrainTimedOut: "timed out", DrainInterrupted: "interrupted",
		DrainResult(9): "DrainResult(9)",
	} {
		if got := r.String(); got != want {
			t.Errorf("%d.String() = %q, want %q", int(r), got, want)
		}
	}
}

// When the drain looks complete but an idle runner's RemoveRunner failed
// transiently, one more pass retries it before the drain ends.
func TestDrainRetriesListedIdleOnce(t *testing.T) {
	for _, tt := range []struct {
		name      string
		failUntil int
		wantGone  bool
	}{
		{"second try succeeds", 1, true},
		{"second try fails too", 99, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t, testTarget(2, 0))
			seed(t, h.table, "pool-00000001", 1, StateIdle, h.clock.Now())
			calls := 0
			h.gh.setRemoveErr(func(int64) error {
				calls++
				if calls <= tt.failUntil {
					return statusError(503)
				}
				return nil
			})
			done := startDrain(h, context.Background(), DrainSignal, time.Hour)
			if r := drainResult(t, done); r != DrainCompleted {
				t.Fatalf("Drain = %v", r)
			}
			if err := h.s.WaitInFlight(context.Background()); err != nil {
				t.Fatal(err)
			}
			if n := h.gh.calls(); n != 2 {
				t.Fatalf("RemoveRunner calls = %d, want 2", n)
			}
			if gone := h.gone("pool-00000001"); gone != tt.wantGone {
				t.Fatalf("runner gone = %v, want %v", gone, tt.wantGone)
			}
		})
	}
}

// TestDrainCountsAllTargets: the drain is table-wide, so a target's drain is
// complete only when no runner of any target is left to wait for, while its
// scale-down passes remove only its own idle runners (0002 PLAN M11-5).
func TestDrainCountsAllTargets(t *testing.T) {
	a := newHarness(t, testTarget(2, 0))
	b := newTargetHarness(t, a, TargetConfig{Key: "other", Label: "pool", MaxRunners: 2})
	now := a.clock.Now()
	seed(t, a.table, "pool-00000001", 1, StateIdle, now)
	seedFor(t, a.table, "other", "pool-00000002", 2, StateBusy, now)
	seedFor(t, a.table, "other", "pool-00000003", 3, StateIdle, now)

	done := startDrain(a, context.Background(), DrainSignal, 10*time.Minute)
	waitPending(t, a.clock, 2)
	waitUntil(t, "a's idle runner removed", func() bool { return slices.Equal(a.gh.removedIDs(), []int64{1}) })
	a.clock.Advance(drainPollInterval)
	waitPending(t, a.clock, 2)
	notReturned(t, done)
	if len(b.gh.removedIDs()) != 0 || b.state("pool-00000003") != StateIdle {
		t.Fatalf("a's drain removed b's runners: %v", b.gh.removedIDs())
	}

	remove(b, "pool-00000002")
	a.clock.Advance(drainPollInterval)
	if r := drainResult(t, done); r != DrainCompleted {
		t.Fatalf("Drain = %v, want completed", r)
	}
}
