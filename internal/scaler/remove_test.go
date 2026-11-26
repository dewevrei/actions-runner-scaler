package scaler

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dewevrei/actions-runner-scaler/internal/retry"
)

// failTimes returns an error function that fails the first n calls with err.
func failTimes(n int32, err error) func(string) error {
	var calls atomic.Int32
	return func(string) error {
		if calls.Add(1) <= n {
			return err
		}
		return nil
	}
}

// TestStartCleanupRetriesResources: a failed start whose resource removal
// fails twice holds its place against max (R3) until a retry succeeds; then
// the row leaves and the next Scale starts a runner.
func TestStartCleanupRetriesResources(t *testing.T) {
	h := newHarness(t, testTarget(1, 0))
	h.docker.setStartErr(func(string) error { return errTransient })
	h.docker.setRemoveErr(failTimes(2, errTransient))
	h.mustScale(statsMsg(1, 1))
	name := h.gh.jitNames[0]
	if h.state(name) != StateExiting || h.table.Counts().All() != 1 {
		t.Fatalf("state %v all %d; want exiting and 1", h.state(name), h.table.Counts().All())
	}
	if got := h.gh.removedIDs(); !slices.Equal(got, []int64{100}) {
		t.Fatalf("RemoveRunner ids %v, want [100] right after the first attempt (R18)", got)
	}

	h.docker.setStartErr(nil)
	h.mustScale(nil)
	if n := len(h.docker.startedNames()); n != 1 {
		t.Fatalf("starts %d while the exiting row fills max, want 1 (R3)", n)
	}

	h.clock.Advance(time.Second) // first retry, fails
	if n := len(h.docker.removedNames()); n != 2 || h.state(name) != StateExiting {
		t.Fatalf("after 1s: removals %d state %v; want 2 and exiting", n, h.state(name))
	}
	h.clock.Advance(2*time.Second - time.Millisecond)
	if n := len(h.docker.removedNames()); n != 2 {
		t.Fatalf("second retry before its 2s backoff: removals %d", n)
	}
	h.clock.Advance(time.Millisecond) // second retry, succeeds
	if !h.gone(name) || h.table.Counts().All() != 0 {
		t.Fatalf("row not removed after a successful retry; all %d", h.table.Counts().All())
	}
	if h.errs.count() != 0 {
		t.Fatalf("recorded %d errors for a removal that succeeded", h.errs.count())
	}

	h.mustScale(nil)
	if n := len(h.docker.startedNames()); n != 2 {
		t.Fatalf("starts %d after the row left, want 2", n)
	}
}

// TestScaleDownRetriesResourcesThenGivesUp: ten retries at 1, 2, 4 … 60 s,
// then the removal stops with one log and last error; the row stays exiting
// for reconciliation.
func TestScaleDownRetriesResourcesThenGivesUp(t *testing.T) {
	h := newHarness(t, testTarget(2, 0))
	seed(t, h.table, "pool-a", 1, StateIdle, testEpoch)
	tok := mustToken(t)
	h.table.SetToken("pool-a", tok)
	h.docker.setRemoveErr(func(string) error { return errTransient })
	h.mustScale(statsMsg(1, 0))
	if _, ok := h.table.Authenticate("pool-a", tok.Reveal(), testEpoch); ok {
		t.Fatal("token valid after removal began (R38)")
	}

	// Retries at 1, 3, 7, 15, 31, 63, 123, 183, 243, 303 s.
	h.clock.Advance(303*time.Second - time.Millisecond)
	if n := len(h.docker.removedNames()); n != 10 {
		t.Fatalf("removals %d before the tenth retry, want 10", n)
	}
	if h.errs.count() != 0 {
		t.Fatal("recorded before giving up")
	}
	h.clock.Advance(time.Millisecond)
	h.clock.Advance(time.Hour)
	if n := len(h.docker.removedNames()); n != 1+maxRemoveRetries {
		t.Fatalf("removals %d, want %d", n, 1+maxRemoveRetries)
	}
	if h.clock.pending() != 0 {
		t.Fatal("retry still pending after giving up")
	}
	if last := h.errs.last(); h.errs.count() != 1 || last.where != "remove runner resources" || !errors.Is(last.err, errTransient) {
		t.Fatalf("recorded %d, last %+v; want the removal error once", h.errs.count(), last)
	}
	if info, _ := h.table.Get("pool-a"); info.State != StateExiting || info.AbandonReason != AbandonRetriesExhausted {
		t.Fatalf("row %+v, want exiting and abandoned after the retries for reconciliation", info)
	}
	if !h.table.ClaimAbandoned("pool-a") || h.table.ClaimAbandoned("pool-a") {
		t.Fatal("abandoned row not claimable exactly once")
	}
	if !strings.Contains(h.logs.String(), "giving up") {
		t.Fatal("no log when giving up")
	}
	if h.fatalCount() != 0 {
		t.Fatal("removal failure reported fatal")
	}
	if err := h.s.WaitInFlight(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestResourceRemovalPermanentNotRetried(t *testing.T) {
	h := newHarness(t, testTarget(2, 0))
	seed(t, h.table, "pool-a", 1, StateIdle, testEpoch)
	h.docker.setRemoveErr(func(string) error { return statusError(400) })
	h.mustScale(statsMsg(1, 0))
	h.clock.Advance(time.Hour)
	if n := len(h.docker.removedNames()); n != 1 || h.errs.count() != 1 {
		t.Fatalf("removals %d recorded %d; want 1 and 1", n, h.errs.count())
	}
	// R42: a target that failed permanently is not handed to reconciliation.
	if info, _ := h.table.Get("pool-a"); info.State != StateExiting || info.AbandonReason != AbandonPermanent {
		t.Fatalf("row %+v, want exiting and abandoned as permanent", info)
	}
	if h.table.ClaimAbandoned("pool-a") {
		t.Fatal("row abandoned on a permanent error was claimed (R42)")
	}
}

// TestUnregisterZeroRateLimit: a rate limit whose wait is already over must
// not make RemoveRunner call itself again at once (R52).
func TestUnregisterZeroRateLimit(t *testing.T) {
	for _, tt := range []struct {
		name string
		err  error
		wait time.Duration
	}{
		// The reset is in the past: the backoff's first step decides.
		{"reset in the past", &retry.RateLimitError{Reset: testEpoch.Add(-time.Minute)}, time.Second},
		// No hints at all: the default wait (R52).
		{"retry after 0", &retry.RateLimitError{RetryAfter: 0}, retry.DefaultRateLimitWait},
	} {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t, testTarget(1, 0))
			h.docker.setStartErr(func(string) error { return errTransient })
			var calls atomic.Int32
			h.gh.setRemoveErr(func(int64) error {
				if calls.Add(1) == 1 {
					return tt.err
				}
				return nil
			})
			h.mustScale(statsMsg(1, 1))
			if n := len(h.gh.removedIDs()); n != 1 {
				t.Fatalf("RemoveRunner calls %d, want 1 (no immediate retry)", n)
			}
			if h.clock.pending() != 1 {
				t.Fatalf("pending timers %d, want the deferred RemoveRunner", h.clock.pending())
			}
			h.clock.Advance(tt.wait - time.Millisecond)
			if n := len(h.gh.removedIDs()); n != 1 {
				t.Fatalf("RemoveRunner calls %d before %v", n, tt.wait)
			}
			h.clock.Advance(time.Millisecond)
			if n := len(h.gh.removedIDs()); n != 2 || h.clock.pending() != 0 {
				t.Fatalf("calls %d pending %d at %v; want 2 and 0", n, h.clock.pending(), tt.wait)
			}
		})
	}
}

func TestUnregisterRateLimitGivesUp(t *testing.T) {
	h := newHarness(t, testTarget(1, 0))
	h.docker.setStartErr(func(string) error { return errTransient })
	h.gh.setRemoveErr(func(int64) error { return &retry.RateLimitError{Reset: testEpoch.Add(-time.Minute)} })
	h.mustScale(statsMsg(1, 1))
	h.clock.Advance(time.Hour)
	if n := len(h.gh.removedIDs()); n != 1+maxUnregisterRetries {
		t.Fatalf("RemoveRunner calls %d, want %d", n, 1+maxUnregisterRetries)
	}
	if h.clock.pending() != 0 {
		t.Fatal("deferral still pending after giving up")
	}
	last := h.errs.last()
	var rl *retry.RateLimitError
	if h.errs.count() != 1 || last.where != "start cleanup remove runner" || !errors.As(last.err, &rl) {
		t.Fatalf("recorded %d, last %+v; want the rate limit once", h.errs.count(), last)
	}
	if strings.Count(last.err.Error(), string(retry.OpRemoveRunner)) != 1 {
		t.Fatalf("error %q names the call more than once", last.err)
	}
}

// TestFinalizeStopsDeferredWork: once finalize begins, pending retries and
// deferred RemoveRunner calls are stopped, WaitInFlight returns, and no Docker
// or GitHub call follows, AcquireJobs included (R47, M5-8).
func TestFinalizeStopsDeferredWork(t *testing.T) {
	for _, tt := range []struct {
		name  string
		begin func(h *harness)
	}{
		{"Scaler.BeginFinalize", func(h *harness) { h.finalize() }},
		// Only the table mark: WaitInFlight stops the timers itself.
		{"table mark only", func(h *harness) { h.table.BeginFinalize(h.clock.Now().Add(45 * time.Second)) }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t, testTarget(2, 0))
			h.docker.setStartErr(func(string) error { return errTransient })
			h.docker.setRemoveErr(func(string) error { return errTransient })
			h.gh.setRemoveErr(func(int64) error { return errRateLimited() })
			h.mustScale(statsMsg(1, 1))
			if h.clock.pending() != 2 {
				t.Fatalf("pending timers %d, want a resource retry and a deferred RemoveRunner", h.clock.pending())
			}
			seed(t, h.table, "pool-idle", 7, StateIdle, testEpoch)

			tt.begin(h)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if err := h.s.WaitInFlight(ctx); err != nil {
				t.Fatalf("WaitInFlight = %v", err)
			}
			// Only the finalize deadline's timer is left (armFinalizeDeadline).
			if h.clock.pending() != 1 {
				t.Fatalf("pending timers %d after finalize, want only the finalize deadline", h.clock.pending())
			}
			name := h.gh.jitNames[0]
			if got := h.s.PendingUnregistrations(); !slices.Equal(got, []Unregistration{{name, 100}}) {
				t.Fatalf("pending unregistrations %v, want the stopped RemoveRunner for finalize", got)
			}

			github := func() int { return h.gh.calls() + h.acq.callCount() }
			docker, gh := h.docker.calls(), github()
			h.clock.Advance(time.Hour)
			if err := h.scale(withAvailable(statsMsg(2, 2), 9)); !errors.Is(err, ErrFinalizing) {
				t.Fatalf("Scale with available jobs = %v, want ErrFinalizing", err)
			}
			if err := h.s.RemoveIdle(context.Background()); err != nil {
				t.Fatal(err)
			}
			if h.docker.calls() != docker || github() != gh {
				t.Fatalf("calls after finalize: Docker %d→%d, GitHub %d→%d; want none (R47)",
					docker, h.docker.calls(), gh, github())
			}
		})
	}
}

// TestCleanupBoundByFinalizeBudget: a start path cleaning up during
// finalize gives each call at most the budget left, and makes no call once
// it is spent.
func TestCleanupBoundByFinalizeBudget(t *testing.T) {
	t.Run("budget left", func(t *testing.T) {
		h := newHarness(t, testTarget(1, 0))
		h.docker.duringStart = func(context.Context, string) {
			h.s.BeginFinalize(h.clock.Now().Add(5 * time.Second))
		}
		h.scaleFinalizing(statsMsg(1, 1))
		left := h.docker.removeLeftTimes()
		if len(left) != 1 || left[0] > 5*time.Second || left[0] <= 0 {
			t.Fatalf("time left on RemoveResources %v, want one call within 5s", left)
		}
		// unregister's RemoveRunner is bound by the budget too.
		if left := h.gh.removeLeftTimes(); len(left) != 1 || left[0] > 5*time.Second || left[0] <= 0 {
			t.Fatalf("time left on RemoveRunner %v, want one call within 5s", left)
		}
	})
	t.Run("normal deadline", func(t *testing.T) {
		h := newHarness(t, testTarget(1, 0))
		h.docker.setStartErr(func(string) error { return errTransient })
		h.mustScale(statsMsg(1, 1))
		if left := h.docker.removeLeftTimes(); len(left) != 1 || left[0] <= 5*time.Second || left[0] > cleanupTimeout {
			t.Fatalf("time left on RemoveResources %v, want about %v", left, cleanupTimeout)
		}
	})
	t.Run("budget spent", func(t *testing.T) {
		h := newHarness(t, testTarget(1, 0))
		h.docker.duringStart = func(context.Context, string) {
			h.s.BeginFinalize(h.clock.Now())
		}
		h.scaleFinalizing(statsMsg(1, 1))
		name := h.gh.jitNames[0]
		if len(h.docker.removedNames()) != 0 || len(h.gh.removedIDs()) != 0 {
			t.Fatal("cleanup call made with no budget left")
		}
		if info, _ := h.table.Get(name); info.State != StateExiting || info.AbandonReason != AbandonNoBudget {
			t.Fatalf("row %+v, want exiting and abandoned for lack of budget", info)
		}
		if !strings.Contains(h.logs.String(), "budget spent") {
			t.Fatal("no log for the spent budget")
		}
	})
}

// TestWaitInFlightWaitsForRetries: outside finalize, a pending retry counts as
// work in flight.
func TestWaitInFlightWaitsForRetries(t *testing.T) {
	h := newHarness(t, testTarget(1, 0))
	h.docker.setStartErr(func(string) error { return errTransient })
	h.docker.setRemoveErr(failTimes(1, errTransient))
	h.mustScale(statsMsg(1, 1))

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := h.s.WaitInFlight(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("WaitInFlight with a retry pending = %v, want canceled", err)
	}
	waited := make(chan error, 1)
	go func() { waited <- h.s.WaitInFlight(context.Background()) }()
	h.clock.Advance(time.Second)
	if err := <-waited; err != nil {
		t.Fatal(err)
	}
	if !h.gone(h.gh.jitNames[0]) {
		t.Fatal("retry did not remove the runner")
	}
}

// TestDeferredWorkStopRace stops timers while they fire; the work count
// must end at zero whichever side wins.
func TestDeferredWorkStopRace(t *testing.T) {
	for range 50 {
		clock := newFakeClock()
		var work inFlight
		d := newDeferredWork(clock, &work)
		var ran atomic.Int32
		for range 8 {
			d.after(time.Second, func() { ran.Add(1) })
		}
		done := make(chan struct{})
		go func() {
			defer close(done)
			clock.Advance(time.Second)
		}()
		d.stop()
		<-done
		if d.after(time.Second, func() {}) {
			t.Fatal("after scheduled once stopped")
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		if err := work.wait(ctx); err != nil {
			cancel()
			t.Fatalf("work left in flight: %v (ran %d)", err, ran.Load())
		}
		cancel()
	}
}

// TestPendingUnregistrations: a start cleanup's RemoveRunner that is not
// confirmed stays listed after the row left the table, for finalize (M5-8),
// and leaves the list once it succeeds, finds the runner gone, or fails
// permanently.
func TestPendingUnregistrations(t *testing.T) {
	start := func(t *testing.T, removeErr func(int64) error) (*harness, string) {
		t.Helper()
		h := newHarness(t, testTarget(1, 0))
		h.docker.setStartErr(func(string) error { return errTransient })
		h.gh.setRemoveErr(removeErr)
		h.mustScale(statsMsg(1, 1))
		name := h.gh.jitNames[0]
		if !h.gone(name) {
			t.Fatal("row still in the table after its resources were removed")
		}
		return h, name
	}
	t.Run("deferred by rate limit, then removed", func(t *testing.T) {
		var calls atomic.Int32
		h, name := start(t, func(int64) error {
			if calls.Add(1) == 1 {
				return errRateLimited()
			}
			return nil
		})
		if got := h.s.PendingUnregistrations(); !slices.Equal(got, []Unregistration{{name, 100}}) {
			t.Fatalf("pending %v, want [{%s 100}]", got, name)
		}
		h.clock.Advance(retry.DefaultRateLimitWait)
		if n := len(h.gh.removedIDs()); n != 2 {
			t.Fatalf("RemoveRunner calls %d, want the deferred one", n)
		}
		if got := h.s.PendingUnregistrations(); len(got) != 0 {
			t.Fatalf("pending %v after success, want none", got)
		}
		if !strings.Contains(h.logs.String(), "deferred by rate limit") {
			t.Fatal("no log for the deferral")
		}
	})
	for _, tt := range []struct {
		name    string
		err     error
		pending bool
	}{
		{"success", nil, false},
		{"gone", retry.ErrRunnerGone, false},
		{"permanent", errPermanent, false},
		// Retried, then given up (TestUnregisterTransientRetries).
		{"transient", errTransient, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			h, name := start(t, func(int64) error { return tt.err })
			h.clock.Advance(time.Hour)
			got := h.s.PendingUnregistrations()
			if want := tt.pending; (len(got) == 1) != want || len(got) > 1 {
				t.Fatalf("pending %v, want listed %v", got, want)
			}
			if tt.pending && got[0] != (Unregistration{name, 100}) {
				t.Fatalf("pending %v, want [{%s 100}]", got, name)
			}
			if tt.pending && !strings.Contains(h.logs.String(), "registration left to finalize and the next start") {
				t.Fatal("log does not say where the registration was left")
			}
		})
	}
}

// TestFinalizeRateLimitedUnregister: during finalize a start cleanup makes
// no RemoveRunner call before the allowed time and schedules nothing; the
// registration stays listed for finalize (R52, M5-8).
func TestFinalizeRateLimitedUnregister(t *testing.T) {
	h := newHarness(t, testTarget(1, 0))
	h.limits.Record(retry.OpRemoveRunner, errRateLimited())
	h.docker.duringStart = func(context.Context, string) {
		h.s.BeginFinalize(h.clock.Now().Add(45 * time.Second))
	}
	h.scaleFinalizing(statsMsg(1, 1))
	name := h.gh.jitNames[0]
	if n := len(h.gh.removedIDs()); n != 0 {
		t.Fatalf("RemoveRunner calls %d before the allowed time, want 0", n)
	}
	if h.clock.pending() != 1 {
		t.Fatalf("pending timers %d during finalize, want only the finalize deadline", h.clock.pending())
	}
	if got := h.s.PendingUnregistrations(); !slices.Equal(got, []Unregistration{{name, 100}}) {
		t.Fatalf("pending %v, want [{%s 100}]", got, name)
	}
	if err := h.s.WaitInFlight(context.Background()); err != nil {
		t.Fatal(err)
	}
	h.clock.Advance(time.Hour)
	if n := len(h.gh.removedIDs()); n != 0 {
		t.Fatalf("RemoveRunner calls %d after the allowed time during finalize, want 0", n)
	}
}

// TestRemovalWithoutBackoff: if a removal backoff cannot be made, the
// removal is tried once, not retried, and the row is abandoned.
func TestRemovalWithoutBackoff(t *testing.T) {
	h := newHarness(t, testTarget(2, 0))
	seed(t, h.table, "pool-a", 1, StateIdle, testEpoch)
	h.s.jitter = nil // NewDefaultBackoff refuses a nil random source.
	if _, err := h.s.newRemovalBackoff(); err == nil {
		t.Fatal("newRemovalBackoff accepted a nil jitter")
	}
	h.docker.setRemoveErr(func(string) error { return errTransient })
	h.mustScale(statsMsg(1, 0))
	h.clock.Advance(time.Hour)
	if n := len(h.docker.removedNames()); n != 1 || h.clock.pending() != 0 {
		t.Fatalf("removals %d pending %d; want 1 and 0", n, h.clock.pending())
	}
	if info, _ := h.table.Get("pool-a"); info.AbandonReason != AbandonNoBackoff {
		t.Fatalf("row %+v, want abandoned without a backoff", info)
	}
	if h.errs.count() != 1 {
		t.Fatalf("recorded %d, want 1", h.errs.count())
	}
}

// TestFinalizeDeadlineEndsCallInFlight: a removal call that began before
// finalize ends at the finalize deadline, measured on the Clock (M5-8).
func TestFinalizeDeadlineEndsCallInFlight(t *testing.T) {
	h := newHarness(t, testTarget(2, 0))
	seed(t, h.table, "pool-a", 1, StateIdle, testEpoch)
	inRemove := make(chan struct{})
	ended := make(chan struct{})
	h.docker.duringRemove = func(ctx context.Context, _ string) error {
		close(inRemove)
		<-ctx.Done()
		close(ended)
		return ctx.Err()
	}
	scaled := make(chan error, 1)
	go func() { scaled <- h.scale(statsMsg(1, 0)) }()
	<-inRemove

	const budget = 5 * time.Second
	h.s.BeginFinalize(h.clock.Now().Add(budget))
	h.clock.Advance(budget - time.Millisecond)
	select {
	case <-ended:
		t.Fatal("call ended before the finalize deadline")
	default:
	}
	h.clock.Advance(time.Millisecond)
	select {
	case <-ended:
	case <-time.After(10 * time.Second):
		t.Fatal("call still running after the finalize deadline")
	}
	if err := <-scaled; !errors.Is(err, ErrFinalizing) {
		t.Fatalf("Scale = %v, want ErrFinalizing", err)
	}
	if info, _ := h.table.Get("pool-a"); info.State != StateExiting || info.AbandonReason != AbandonFinalize {
		t.Fatalf("row %+v, want exiting and abandoned to finalize", info)
	}
	if err := h.s.WaitInFlight(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// TestDockerNotFoundIsRemoved: a resource removal that finds nothing counts
// as success (R50).
func TestDockerNotFoundIsRemoved(t *testing.T) {
	h := newHarness(t, testTarget(2, 0))
	seed(t, h.table, "pool-a", 1, StateIdle, testEpoch)
	h.docker.setRemoveErr(func(string) error { return statusError(404) })
	h.mustScale(statsMsg(1, 0))
	if !h.gone("pool-a") {
		t.Fatalf("state %v, want removed", h.state("pool-a"))
	}
	if h.errs.count() != 0 || h.clock.pending() != 0 {
		t.Fatalf("recorded %d pending %d; want 0 and 0", h.errs.count(), h.clock.pending())
	}
}

// TestResourceRetryAfterFinalizeMark: a retry that fires after the finalize
// mark was set (before its timer could be stopped) makes no call and leaves
// the row abandoned to finalize.
func TestResourceRetryAfterFinalizeMark(t *testing.T) {
	h := newHarness(t, testTarget(2, 0))
	seed(t, h.table, "pool-a", 1, StateIdle, testEpoch)
	h.docker.setRemoveErr(func(string) error { return errTransient })
	h.mustScale(statsMsg(1, 0))
	if h.clock.pending() != 1 {
		t.Fatalf("pending timers %d, want the retry", h.clock.pending())
	}
	// Only the table mark: the timer is not stopped.
	h.table.BeginFinalize(h.clock.Now().Add(45 * time.Second))
	h.clock.Advance(time.Second)
	if n := len(h.docker.removedNames()); n != 1 {
		t.Fatalf("removals %d, want no call from the retry", n)
	}
	if info, _ := h.table.Get("pool-a"); info.State != StateExiting || info.AbandonReason != AbandonFinalize {
		t.Fatalf("row %+v, want exiting and abandoned to finalize", info)
	}
	if !strings.Contains(h.logs.String(), "runner resource removal left to finalize") {
		t.Fatal("no log for the retry left to finalize")
	}
}

// TestUnregisterTransientRetries: a start cleanup's RemoveRunner that fails
// with a transient error is retried with the removal backoff, at most
// maxUnregisterRetries times, waits for a rate limit, and stops at finalize.
func TestUnregisterTransientRetries(t *testing.T) {
	start := func(t *testing.T, removeErr func(int64) error) (*harness, string) {
		t.Helper()
		h := newHarness(t, testTarget(1, 0))
		h.docker.setStartErr(func(string) error { return errTransient })
		h.gh.setRemoveErr(removeErr)
		h.mustScale(statsMsg(1, 1))
		return h, h.gh.jitNames[0]
	}
	t.Run("succeeds on a retry", func(t *testing.T) {
		var calls atomic.Int32
		h, _ := start(t, func(int64) error {
			if calls.Add(1) <= 2 {
				return errTransient
			}
			return nil
		})
		// Retries at 1 s and 1 + 2 s.
		h.clock.Advance(time.Second)
		if n := len(h.gh.removedIDs()); n != 2 {
			t.Fatalf("RemoveRunner calls %d at 1s, want 2", n)
		}
		h.clock.Advance(2*time.Second - time.Millisecond)
		if n := len(h.gh.removedIDs()); n != 2 {
			t.Fatalf("RemoveRunner calls %d before the 2s backoff, want 2", n)
		}
		h.clock.Advance(time.Millisecond)
		if n := len(h.gh.removedIDs()); n != 3 {
			t.Fatalf("RemoveRunner calls %d at 3s, want 3", n)
		}
		if got := h.s.PendingUnregistrations(); len(got) != 0 {
			t.Fatalf("pending %v after success, want none", got)
		}
		if h.errs.count() != 0 || h.clock.pending() != 0 {
			t.Fatalf("recorded %d pending %d; want 0 and 0", h.errs.count(), h.clock.pending())
		}
	})
	t.Run("gives up", func(t *testing.T) {
		h, name := start(t, func(int64) error { return errTransient })
		h.clock.Advance(time.Hour)
		if n := len(h.gh.removedIDs()); n != 1+maxUnregisterRetries {
			t.Fatalf("RemoveRunner calls %d, want %d", n, 1+maxUnregisterRetries)
		}
		last := h.errs.last()
		if h.errs.count() != 1 || last.where != "start cleanup remove runner" || !errors.Is(last.err, errTransient) {
			t.Fatalf("recorded %d, last %+v; want the transient error once", h.errs.count(), last)
		}
		if strings.Count(last.err.Error(), string(retry.OpRemoveRunner)) != 1 {
			t.Fatalf("error %q names the call more than once", last.err)
		}
		if got := h.s.PendingUnregistrations(); !slices.Equal(got, []Unregistration{{name, 100}}) {
			t.Fatalf("pending %v, want [{%s 100}] for finalize", got, name)
		}
	})
	t.Run("waits for a rate limit", func(t *testing.T) {
		h := newHarness(t, testTarget(1, 0))
		h.docker.setStartErr(func(string) error { return errTransient })
		var calls atomic.Int32
		h.gh.setRemoveErr(func(int64) error {
			if calls.Add(1) == 1 {
				// Scale-down hit the limit meanwhile.
				h.limits.Record(retry.OpRemoveRunner, errRateLimited())
				return errTransient
			}
			return nil
		})
		h.mustScale(statsMsg(1, 1))
		h.clock.Advance(retry.DefaultRateLimitWait - time.Millisecond)
		if n := len(h.gh.removedIDs()); n != 1 {
			t.Fatalf("RemoveRunner calls %d before the allowed time, want 1 (R52)", n)
		}
		h.clock.Advance(time.Millisecond)
		if n := len(h.gh.removedIDs()); n != 2 {
			t.Fatalf("RemoveRunner calls %d at the allowed time, want 2", n)
		}
		if got := h.s.PendingUnregistrations(); len(got) != 0 {
			t.Fatalf("pending %v, want none", got)
		}
	})
	t.Run("stops at finalize", func(t *testing.T) {
		h, name := start(t, func(int64) error { return errTransient })
		h.finalize()
		if err := h.s.WaitInFlight(context.Background()); err != nil {
			t.Fatal(err)
		}
		h.clock.Advance(time.Hour)
		if n := len(h.gh.removedIDs()); n != 1 {
			t.Fatalf("RemoveRunner calls %d after finalize, want 1", n)
		}
		if got := h.s.PendingUnregistrations(); !slices.Equal(got, []Unregistration{{name, 100}}) {
			t.Fatalf("pending %v, want [{%s 100}] for finalize", got, name)
		}
	})
}
