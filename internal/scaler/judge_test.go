package scaler

import (
	"context"
	"encoding/json"
	"errors"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dewevrei/actions-runner-scaler/internal/gate"
)

// allowed and denied are event report decisions; denied is a fork pull
// request with its head repository.
var (
	allowed = gate.Decision{Allow: true, HeadRepo: "owner/repo"}
	denied  = gate.Decision{Reason: gate.ReasonPullRequestFromOtherRepo, HeadRepo: "fork/repo"}
)

// newJudgeHarness wires a real JobJudge into a harness.
func newJudgeHarness(t *testing.T) (*harness, *JobJudge) {
	t.Helper()
	j := NewJobJudge()
	return newHarnessWith(t, testTarget(2, 0), j), j
}

// seedToken seeds runner name with GitHub ID id in state st (idle or busy)
// and returns its token.
func seedToken(t *testing.T, h *harness, name string, id int64, st State) string {
	t.Helper()
	seed(t, h.table, name, id, StateIdle, h.clock.Now())
	tok := mustToken(t)
	h.table.SetToken(name, tok)
	if st == StateBusy {
		h.table.MarkBusy(name, h.clock.Now())
	}
	return tok.Reveal()
}

// waitJudge waits for the judge goroutines and deny paths; it must not be
// used while a deny retry is pending.
func waitJudge(t *testing.T, j *JobJudge) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := j.work.wait(ctx); err != nil {
		t.Fatalf("judge work left: %v", err)
	}
}

// eventually waits until cond holds, yielding to the goroutines under test;
// the 10 s limit is a failsafe, not a timing assertion.
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		runtime.Gosched()
	}
}

func verdict(t *testing.T, h *harness, name string) Verdict {
	t.Helper()
	info, ok := h.table.Get(name)
	if !ok {
		t.Fatalf("runner %s not in table", name)
	}
	return info.Verdict
}

// assertDenied checks that the whole deny path ran once for runner name with
// GitHub ID id: runner container, resources, registration, row.
func assertDenied(t *testing.T, h *harness, name string, id int64) {
	t.Helper()
	if got := h.docker.runnerRemovedNames(); !slices.Equal(got, []string{name}) {
		t.Errorf("RemoveContainer calls = %v, want [%s]", got, name)
	}
	if got := h.docker.removedNames(); !slices.Equal(got, []string{name}) {
		t.Errorf("RemoveResources calls = %v, want [%s]", got, name)
	}
	if got := h.gh.removedIDs(); !slices.Equal(got, []int64{id}) {
		t.Errorf("RemoveRunner calls = %v, want [%d]", got, id)
	}
	if !h.gone(name) {
		t.Errorf("%s still in table", name)
	}
}

// waitDenied waits until the deny path for runner name has unregistered it,
// its last step, and then checks the whole path ran once. Unlike waitJudge
// it does not wait for a check that never ends.
func waitDenied(t *testing.T, h *harness, name string, id int64) {
	t.Helper()
	eventually(t, "the deny path of "+name, func() bool { return slices.Contains(h.gh.removedIDs(), id) })
	assertDenied(t, h, name, id)
}

// waitDenyWait waits until the deny path for runner name waits for its
// container, with the stop grace timer set.
func waitDenyWait(t *testing.T, h *harness, name string) {
	t.Helper()
	eventually(t, "the deny path's wait for "+name, func() bool {
		return slices.Contains(h.docker.waitedNames(), name) && h.clock.pending() == 1
	})
}

// TestJobJudgeBindsOnce: a JobJudge belongs to one Scaler.
func TestJobJudgeBindsOnce(t *testing.T) {
	h, j := newJudgeHarness(t)
	_, err := New(Config{
		Target: testTarget(2, 0), Table: NewTable(TableConfig{ServerLimit: testServerLimit, Secrets: h.secrets}), Clock: h.clock, ScaleSet: h.gh,
		Containers: h.docker, Images: h.images, Judge: j, Tokens: h.tokens, Limits: h.limits, Fatal: func(error) {},
		Secrets: h.secrets, Waiter: h.docker,
	})
	if err == nil {
		t.Fatal("New with a judge bound to another Scaler succeeded")
	}
}

// TestJobJudgeBindInstallsTimer: binding installs the job check timer in a
// table made without a BusyFunc, so R81 cannot be left unwired, and a second
// judge cannot take the same table.
func TestJobJudgeBindInstallsTimer(t *testing.T) {
	h, _ := newJudgeHarness(t)
	table := NewTable(TableConfig{ServerLimit: testServerLimit, Secrets: h.secrets})
	j := NewJobJudge()
	cfg := Config{
		Target: testTarget(2, 0), Table: table, Clock: h.clock, ScaleSet: h.gh,
		Containers: h.docker, Images: h.images, Judge: j, Tokens: h.tokens, Limits: h.limits, Fatal: func(error) {},
		Secrets: h.secrets, Waiter: h.docker,
	}
	if _, err := New(cfg); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(j.Stop)
	timers := h.clock.pending()
	seed(t, table, "pool-a", 101, StateBusy, h.clock.Now())
	if n := h.clock.pending(); n != timers+1 {
		t.Errorf("pending timers after busy = %d, want %d", n, timers+1)
	}

	cfg.Judge = NewJobJudge()
	if _, err := New(cfg); err == nil {
		t.Error("New with a table that has another judge's timer succeeded")
	}
}

// fixed returns an error function that returns errs in turn, then nil.
func fixed(errs ...error) func(string) error {
	var mu sync.Mutex
	return func(string) error {
		mu.Lock()
		defer mu.Unlock()
		if len(errs) == 0 {
			return nil
		}
		err := errs[0]
		errs = errs[1:]
		return err
	}
}

// TestDenyRecordedBeforeRemoval: the deny is recorded before the deny path
// waits for the runner container, so the hook reads it while the runner
// stops; the container is removed, with a 10 s bound, once it stopped, and
// the token is revoked only after that removal, then the rest of the runner
// goes (R11, R38).
func TestDenyRecordedBeforeRemoval(t *testing.T) {
	h, j := newJudgeHarness(t)
	tok := seedToken(t, h, "pool-a", 101, StateBusy)
	h.docker.addRunner("pool-a", "running", "running")

	var mu sync.Mutex
	var order []string
	note := func(s string) {
		mu.Lock()
		defer mu.Unlock()
		order = append(order, s)
	}
	h.docker.waitErr = func(name string) error {
		v, ok := h.table.Authenticate(name, tok, h.clock.Now())
		if !ok {
			note("wait: token refused")
		} else {
			note("wait: hook reads " + v.String())
		}
		// The hook killed the Worker, and the container stops.
		h.docker.exit(name)
		return nil
	}
	h.docker.setRemoveContainerErr(func(name string) error {
		v, ok := h.table.Authenticate(name, tok, h.clock.Now())
		if !ok {
			note("remove container: token refused")
		} else {
			note("remove container: hook reads " + v.String())
		}
		return nil
	})
	h.docker.setDuringRemove(func(_ context.Context, name string) error {
		info, _ := h.table.Get(name)
		if _, ok := h.table.Authenticate(name, tok, h.clock.Now()); ok {
			note("remove resources: verdict " + info.Verdict.String() + ", token valid")
		} else {
			note("remove resources: verdict " + info.Verdict.String() + ", token revoked")
		}
		return nil
	})
	h.gh.setRemoveErr(func(int64) error {
		note("remove runner")
		return nil
	})

	if v := j.Record("pool-a", "pull_request", denied); v != VerdictDeny {
		t.Fatalf("Record = %v, want deny", v)
	}
	waitJudge(t, j)

	want := []string{
		"wait: hook reads deny",
		"remove container: hook reads deny",
		"remove resources: verdict deny, token revoked",
		"remove runner",
	}
	mu.Lock()
	defer mu.Unlock()
	if !slices.Equal(order, want) {
		t.Errorf("order = %q, want %q", order, want)
	}
	left := h.docker.runnerRemovedLeftTimes()
	if len(left) != 1 || left[0] <= 9*time.Second || left[0] > removeContainerTimeout {
		t.Errorf("RemoveContainer time left = %v, want one just under %v", left, removeContainerTimeout)
	}
	if !h.gone("pool-a") {
		t.Error("runner still in table")
	}
}

// TestDenyRetriesEvery2sUntilRemoved: while removing the runner container
// fails, permanently or not, the token stays valid so the hook still reads
// the deny, and the owner tries again every 2 s, recording each new error
// once; the token is revoked once the removal succeeds (PLAN M4-2).
func TestDenyRetriesEvery2sUntilRemoved(t *testing.T) {
	h, j := newJudgeHarness(t)
	tok := seedToken(t, h, "pool-a", 101, StateBusy)
	h.docker.setRemoveContainerErr(fixed(statusError(400), statusError(400), errTransient))

	j.Reject("pool-a", "test")
	eventually(t, "the first attempt and its retry timer", func() bool {
		return len(h.docker.runnerRemovedNames()) == 1 && h.clock.pending() == 1
	})
	readsDeny := func(after string) {
		t.Helper()
		if v, ok := h.table.Authenticate("pool-a", tok, h.clock.Now()); !ok || v != VerdictDeny {
			t.Errorf("after %s the hook reads %v, %v; want deny", after, v, ok)
		}
	}
	readsDeny("a permanent error")
	if n := h.errs.count(); n != 1 {
		t.Errorf("errors recorded = %d, want 1", n)
	}

	h.clock.Advance(denyRetryInterval - time.Millisecond)
	if n := len(h.docker.runnerRemovedNames()); n != 1 {
		t.Fatalf("RemoveContainer calls before 2 s = %d, want 1", n)
	}
	h.clock.Advance(time.Millisecond)
	if n := len(h.docker.runnerRemovedNames()); n != 2 {
		t.Fatalf("RemoveContainer calls at 2 s = %d, want 2", n)
	}
	readsDeny("the same error again")
	if n := h.errs.count(); n != 1 {
		t.Errorf("errors recorded after a repeat = %d, want 1", n)
	}

	h.clock.Advance(denyRetryInterval)
	readsDeny("a transient error")
	if n := h.errs.count(); n != 2 {
		t.Errorf("errors recorded after a new error = %d, want 2", n)
	}
	h.clock.Advance(denyRetryInterval)
	if n := len(h.docker.runnerRemovedNames()); n != 4 {
		t.Fatalf("RemoveContainer calls = %d, want 4", n)
	}
	waitJudge(t, j)
	if !h.gone("pool-a") || !slices.Equal(h.gh.removedIDs(), []int64{101}) {
		t.Errorf("runner not removed after the container went: gone %v, RemoveRunner %v",
			h.gone("pool-a"), h.gh.removedIDs())
	}
	if !strings.Contains(h.logs.String(), "removed after retries") {
		t.Errorf("logs lack the recovery:\n%s", h.logs)
	}
}

// TestDenyNotFoundCountsAsRemoved: a runner container removal that finds
// the container gone counts as removed (R50).
func TestDenyNotFoundCountsAsRemoved(t *testing.T) {
	h, j := newJudgeHarness(t)
	seedToken(t, h, "pool-a", 101, StateBusy)
	h.docker.setRemoveContainerErr(fixed(statusError(404)))

	j.Reject("pool-a", "test")
	waitJudge(t, j)

	assertDenied(t, h, "pool-a", 101)
}

// TestDenyWaitsForRunnerStop: the deny path records the deny, then removes
// nothing while the runner container runs; once the container stops it is
// removed at once, and the token is revoked only then (R11).
func TestDenyWaitsForRunnerStop(t *testing.T) {
	h, j := newJudgeHarness(t)
	tok := seedToken(t, h, "pool-a", 101, StateBusy)
	h.docker.addRunner("pool-a", "running", "running")

	j.Reject("pool-a", "test")
	waitDenyWait(t, h, "pool-a")
	h.clock.Advance(denyStopGrace - time.Second)
	if n := h.docker.calls(); n != 0 {
		t.Fatalf("Docker calls while the runner runs = %d, want 0", n)
	}
	if v, ok := h.table.Authenticate("pool-a", tok, h.clock.Now()); !ok || v != VerdictDeny {
		t.Errorf("hook reads %v, %v while the runner stops; want deny", v, ok)
	}

	h.docker.exit("pool-a")
	waitJudge(t, j)
	assertDenied(t, h, "pool-a", 101)
	if n := h.clock.pending(); n != 0 {
		t.Errorf("pending timers = %d, want 0 (grace timer stopped)", n)
	}
	// slog writes the elapsed time in nanoseconds: 29 s on the Clock.
	if logs := h.logs.String(); !strings.Contains(logs, "denied runner's container stopped; removing it") ||
		!strings.Contains(logs, `"elapsed":29000000000`) {
		t.Errorf("logs lack the stop with its elapsed time:\n%s", logs)
	}
}

// TestDenyGraceEndsWait: a runner container still running 30 s after the
// deny is removed then, measured on the Clock (R11).
func TestDenyGraceEndsWait(t *testing.T) {
	h, j := newJudgeHarness(t)
	tok := seedToken(t, h, "pool-a", 101, StateBusy)
	h.docker.addRunner("pool-a", "running", "running")

	j.Reject("pool-a", "test")
	waitDenyWait(t, h, "pool-a")
	h.clock.Advance(denyStopGrace - time.Millisecond)
	assertStillWaiting(t, h, j, 1)
	if n := h.docker.calls(); n != 0 {
		t.Fatalf("Docker calls before the grace ended = %d, want 0", n)
	}
	if _, ok := h.table.Authenticate("pool-a", tok, h.clock.Now()); !ok {
		t.Error("token revoked before the removal")
	}
	h.clock.Advance(time.Millisecond)
	waitJudge(t, j)

	assertDenied(t, h, "pool-a", 101)
	if !strings.Contains(h.logs.String(), "runner did not stop within grace") {
		t.Errorf("logs lack the grace:\n%s", h.logs)
	}
}

// TestDenyNotFoundWaitEnds: a container that is gone counts as stopped, so
// the rest is removed without waiting for the grace.
func TestDenyNotFoundWaitEnds(t *testing.T) {
	h, j := newJudgeHarness(t)
	seedToken(t, h, "pool-a", 101, StateBusy)

	j.Reject("pool-a", "test")
	waitJudge(t, j)

	assertDenied(t, h, "pool-a", 101)
	if got := h.docker.waitedNames(); !slices.Equal(got, []string{"pool-a"}) {
		t.Errorf("Wait calls = %v, want [pool-a]", got)
	}
	if !h.clock.Now().Equal(testEpoch) {
		t.Errorf("clock moved to %v", h.clock.Now())
	}
	if !strings.Contains(h.logs.String(), "denied runner's container gone") {
		t.Errorf("logs lack the gone container:\n%s", h.logs)
	}
}

// waitErrLog is the deny path's log of a failed container wait.
const waitErrLog = "waiting for the denied runner's container failed"

// waitWaitRetry waits until the deny path for runner name has made wait
// calls in all and set the timer of its next try, next to the grace timer.
func waitWaitRetry(t *testing.T, h *harness, name string, calls int) {
	t.Helper()
	eventually(t, "the deny path's wait retry timer", func() bool {
		return count(h.docker.waitedNames(), name) == calls && h.clock.pending() == 2
	})
}

// TestDenyWaitErrorRetries: a wait that fails other than with not found is
// logged once per new error and tried again every 2 s; a container that
// stopped meanwhile is removed at that try, and one whose wait keeps failing
// is removed when the grace ends (PLAN M4-2).
func TestDenyWaitErrorRetries(t *testing.T) {
	t.Run("stops before the retry", func(t *testing.T) {
		h, j := newJudgeHarness(t)
		seedToken(t, h, "pool-a", 101, StateBusy)
		h.docker.addRunner("pool-a", "running", "running")
		h.docker.waitErr = fixed(statusError(500))

		j.Reject("pool-a", "test")
		waitWaitRetry(t, h, "pool-a", 1)
		h.docker.exit("pool-a")
		h.clock.Advance(denyRetryInterval - time.Millisecond)
		assertStillWaiting(t, h, j, 2)
		if n := len(h.docker.runnerRemovedNames()); n != 0 {
			t.Fatalf("RemoveContainer calls before the retry = %d, want 0", n)
		}
		h.clock.Advance(time.Millisecond)
		waitJudge(t, j)

		assertDenied(t, h, "pool-a", 101)
		if n := len(h.docker.waitedNames()); n != 2 {
			t.Errorf("Wait calls = %d, want 2", n)
		}
		if logs := h.logs.String(); !strings.Contains(logs, "denied runner's container stopped; removing it") ||
			!strings.Contains(logs, `"elapsed":2000000000`) {
			t.Errorf("logs lack the stop at 2 s:\n%s", logs)
		}
	})
	t.Run("keeps failing", func(t *testing.T) {
		h, j := newJudgeHarness(t)
		seedToken(t, h, "pool-a", 101, StateBusy)
		h.docker.addRunner("pool-a", "running", "running")
		h.docker.waitErr = func(string) error { return statusError(500) }

		j.Reject("pool-a", "test")
		waitWaitRetry(t, h, "pool-a", 1)
		// A try every 2 s: at 2, 4, ..., 28 s.
		for calls := 2; calls <= 15; calls++ {
			h.clock.Advance(denyRetryInterval)
			waitWaitRetry(t, h, "pool-a", calls)
		}
		h.clock.Advance(denyRetryInterval - time.Millisecond)
		assertStillWaiting(t, h, j, 2)
		if n := len(h.docker.runnerRemovedNames()); n != 0 {
			t.Fatalf("RemoveContainer calls before the grace ended = %d, want 0", n)
		}
		h.clock.Advance(time.Millisecond)
		waitJudge(t, j)

		if n := len(h.docker.runnerRemovedNames()); n != 1 {
			t.Errorf("RemoveContainer calls at 30 s = %d, want 1", n)
		}
		assertDenied(t, h, "pool-a", 101)
		logs := h.logs.String()
		if n := strings.Count(logs, waitErrLog); n != 1 {
			t.Errorf("wait error logged %d times, want 1 for a repeated error", n)
		}
		if !strings.Contains(logs, "runner did not stop within grace") {
			t.Errorf("logs lack the grace:\n%s", logs)
		}
	})
}

// assertStillWaiting checks that the deny path is still blocked in its
// wait: exactly timers timers are pending (1, the grace timer; 2 after a
// failed wait, with the timer of the next try) and the judge's work does
// not end within a short time. A deny path that went on to the removal
// would have stopped its timers and ended its work, so a check of the
// removal right after this one cannot pass merely because the removal had
// not run yet.
func assertStillWaiting(t *testing.T, h *harness, j *JobJudge, timers int) {
	t.Helper()
	if n := h.clock.pending(); n != timers {
		t.Fatalf("pending timers = %d, want %d", n, timers)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := j.work.wait(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("judge work wait = %v, want the deny path still waiting", err)
	}
}

// TestDenyHaltDuringGrace: Stop ends the wait for the runner container at
// once, also after a failed wait; finalize ends it when the grace ends, or
// at the next try after a failed wait, which then calls Docker no more.
// Either way nothing is removed and the row, its token revoked, is left to
// finalize.
func TestDenyHaltDuringGrace(t *testing.T) {
	for _, how := range []string{"Stop", "Stop after a wait error", "finalize", "finalize after a wait error"} {
		t.Run(how, func(t *testing.T) {
			h, j := newJudgeHarness(t)
			tok := seedToken(t, h, "pool-a", 101, StateBusy)
			h.docker.addRunner("pool-a", "running", "running")
			waitFails := strings.HasSuffix(how, "after a wait error")
			if waitFails {
				h.docker.waitErr = func(string) error { return statusError(500) }
			}

			j.Reject("pool-a", "test")
			if waitFails {
				waitWaitRetry(t, h, "pool-a", 1)
				h.clock.Advance(denyRetryInterval)
				waitWaitRetry(t, h, "pool-a", 2)
				assertStillWaiting(t, h, j, 2)
			} else {
				waitDenyWait(t, h, "pool-a")
			}
			switch how {
			case "finalize after a wait error":
				h.finalize()
				// Finalize sets timers of its own next to the deny path's two.
				others := h.clock.pending() - 2
				h.clock.Advance(denyRetryInterval)
				waitJudge(t, j)
				if n := len(h.docker.waitedNames()); n != 2 {
					t.Errorf("Wait calls after finalize = %d, want 2 (none after it)", n)
				}
				if n := h.clock.pending(); n != others {
					t.Errorf("pending timers = %d, want finalize's %d (grace timer stopped)", n, others)
				}
			case "finalize":
				h.finalize()
				h.clock.Advance(denyStopGrace)
				waitJudge(t, j)
			default:
				j.Stop()
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				// No clock move: Stop alone ends the wait.
				if err := j.Wait(ctx); err != nil {
					t.Fatalf("Wait: %v", err)
				}
				if n := h.clock.pending(); n != 0 {
					t.Errorf("pending timers after Stop = %d, want 0 (grace timer stopped)", n)
				}
			}
			h.clock.Advance(time.Minute)

			if n := h.docker.calls() + h.gh.calls(); n != 0 {
				t.Errorf("Docker and GitHub calls = %d, want 0", n)
			}
			info, _ := h.table.Get("pool-a")
			if info.AbandonReason != AbandonFinalize || info.Verdict != VerdictDeny {
				t.Errorf("row = %+v, want abandoned to finalize with the deny", info)
			}
			if _, ok := h.table.Authenticate("pool-a", tok, h.clock.Now()); ok {
				t.Error("token of a row left to finalize still valid")
			}
		})
	}
}

// TestDenyNotPendingSkipsGrace: when the verdict was not pending, as for a
// Reject after an allow (R102), the hook cannot see a deny, so the
// container is removed without waiting for it to stop.
func TestDenyNotPendingSkipsGrace(t *testing.T) {
	h, j := newJudgeHarness(t)
	seedToken(t, h, "pool-a", 101, StateBusy)
	h.docker.addRunner("pool-a", "running", "running")
	if !h.table.Allow("pool-a") {
		t.Fatal("Allow = false")
	}

	j.Reject("pool-a", "test")
	waitJudge(t, j)

	assertDenied(t, h, "pool-a", 101)
	if n := len(h.docker.waitedNames()); n != 0 {
		t.Errorf("Wait calls = %d, want 0", n)
	}
	if !strings.Contains(h.logs.String(), "verdict was not pending") {
		t.Errorf("logs lack the verdict:\n%s", h.logs)
	}
}

// TestRejectIsIdempotent: Reject twice, even at once, removes the runner
// once; Reject of a runner whose removal began does nothing (R102).
func TestRejectIsIdempotent(t *testing.T) {
	h, j := newJudgeHarness(t)
	seedToken(t, h, "pool-a", 101, StateBusy)
	seedToken(t, h, "pool-b", 102, StateBusy)
	h.table.BeginExit("pool-b")

	var wg sync.WaitGroup
	for range 4 {
		wg.Go(func() { j.Reject("pool-a", "test") })
	}
	wg.Wait()
	j.Reject("pool-b", "test")
	waitJudge(t, j)

	assertDenied(t, h, "pool-a", 101)
}

// TestJudgeDoesNothingOnceFinalizing: Record, Reject and the timers
// record nothing and make no call once finalize has begun, though Scale and
// the job check server checked the mark before (0002 SPEC R131).
func TestJudgeDoesNothingOnceFinalizing(t *testing.T) {
	h, j := newJudgeHarness(t)
	seedToken(t, h, "pool-a", 101, StateBusy)
	seedToken(t, h, "pool-b", 102, StateBusy)
	seedToken(t, h, "pool-c", 103, StateBusy)
	h.finalize()
	timers := h.clock.pending()

	if v := j.Record("pool-a", "push", allowed); v != VerdictPending {
		t.Errorf("Record allow = %v, want pending", v)
	}
	if v := j.Record("pool-c", "pull_request", denied); v != VerdictPending {
		t.Errorf("Record deny = %v, want pending", v)
	}
	j.Reject("pool-b", "test")
	j.BusyTimers().arm("pool-a", h.clock.Now())
	waitJudge(t, j)

	for _, n := range []string{"pool-a", "pool-c"} {
		if v := verdict(t, h, n); v != VerdictPending {
			t.Errorf("%s verdict = %v, want pending", n, v)
		}
	}
	if n := h.docker.calls(); n != 0 {
		t.Errorf("Docker calls = %d, want 0", n)
	}
	if n := h.clock.pending(); n != timers {
		t.Errorf("pending timers = %d, want %d", n, timers)
	}
	if st := h.state("pool-b"); st != StateBusy {
		t.Errorf("rejected runner state = %v, want busy (left to finalize)", st)
	}
}

// TestDenyRetriesStop: Stop and finalize both end the deny retries; after
// finalize the row is left to it.
func TestDenyRetriesStop(t *testing.T) {
	for _, how := range []string{"Stop", "finalize"} {
		t.Run(how, func(t *testing.T) {
			h, j := newJudgeHarness(t)
			tok := seedToken(t, h, "pool-a", 101, StateBusy)
			h.docker.setRemoveContainerErr(func(string) error { return errTransient })

			j.Reject("pool-a", "test")
			eventually(t, "the first attempt and its retry timer", func() bool {
				return len(h.docker.runnerRemovedNames()) == 1 && h.clock.pending() == 1
			})
			if how == "Stop" {
				j.Stop()
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				if err := j.Wait(ctx); err != nil {
					t.Fatalf("Wait: %v", err)
				}
				// The dropped retry leaves the row as the deny path had it;
				// finalize, which Stop presumes, removes it.
				info, _ := h.table.Get("pool-a")
				if info.State != StateExiting || info.Verdict != VerdictDeny || info.Abandoned {
					t.Errorf("row after Stop = %+v, want exiting, denied, not abandoned", info)
				}
				if v, ok := h.table.Authenticate("pool-a", tok, h.clock.Now()); !ok || v != VerdictDeny {
					t.Errorf("hook after Stop reads %v, %v; want deny", v, ok)
				}
			} else {
				h.finalize()
			}
			h.clock.Advance(time.Minute)

			if n := len(h.docker.runnerRemovedNames()); n != 1 {
				t.Errorf("RemoveContainer calls = %d, want 1", n)
			}
			if how == "finalize" {
				if info, _ := h.table.Get("pool-a"); info.AbandonReason != AbandonFinalize || info.Verdict != VerdictDeny {
					t.Errorf("row = %+v, want abandoned to finalize with the deny", info)
				}
				if _, ok := h.table.Authenticate("pool-a", tok, h.clock.Now()); ok {
					t.Error("token of a row left to finalize still valid")
				}
			}
		})
	}
}

// TestJudgeRaces: records, rejections, hook queries and timers on the same
// runners at once remove each runner exactly once (run with -race).
func TestJudgeRaces(t *testing.T) {
	h, j := newJudgeHarness(t)
	names := []string{"pool-a", "pool-b", "pool-c", "pool-d", "pool-e", "pool-f"}
	toks := make(map[string]string)
	for i, n := range names {
		toks[n] = seedToken(t, h, n, int64(101+i), StateBusy)
	}

	var wg sync.WaitGroup
	for i, n := range names {
		wg.Go(func() { j.Record(n, "push", allowed) })
		wg.Go(func() { h.table.Authenticate(n, toks[n], h.clock.Now()) })
		if i%2 == 0 {
			wg.Go(func() { j.Reject(n, "test") })
		}
	}
	wg.Go(func() { h.clock.Advance(busyTimeout) })
	wg.Wait()
	waitJudge(t, j)

	removed := h.docker.runnerRemovedNames()
	for i, n := range names {
		c := count(removed, n)
		info, ok := h.table.Get(n)
		switch {
		case c > 1:
			t.Errorf("%s container removed %d times", n, c)
		case i%2 == 0 && (c != 1 || ok):
			t.Errorf("rejected %s: removed %d times, in table %v", n, c, ok)
		case c == 0 && (!ok || info.Verdict != VerdictAllow):
			t.Errorf("%s neither removed nor allowed: %+v", n, info)
		case c == 1 && ok:
			t.Errorf("%s removed but still in table: %+v", n, info)
		}
	}
}

// TestRecordAllow: an allowed report records the allow before Record
// returns, stops the job check timer, and makes no Docker or GitHub call
// (0002 SPEC R123, R131).
func TestRecordAllow(t *testing.T) {
	h, j := newJudgeHarness(t)
	tok := seedToken(t, h, "pool-a", 101, StateBusy)
	if n := h.clock.pending(); n != 1 {
		t.Fatalf("pending timers after busy = %d, want 1 (the job check timer)", n)
	}

	if v := j.Record("pool-a", "push", allowed); v != VerdictAllow {
		t.Fatalf("Record = %v, want allow", v)
	}
	if v := verdict(t, h, "pool-a"); v != VerdictAllow {
		t.Fatalf("verdict = %v, want allow", v)
	}
	if n := h.clock.pending(); n != 0 {
		t.Errorf("pending timers after allow = %d, want 0 (timer stopped)", n)
	}
	if v, ok := h.table.Authenticate("pool-a", tok, h.clock.Now()); !ok || v != VerdictAllow {
		t.Errorf("hook reads %v, %v; want allow", v, ok)
	}
	h.clock.Advance(time.Hour)
	if n := h.docker.calls() + h.gh.calls(); n != 0 {
		t.Errorf("Docker and GitHub calls = %d, want 0", n)
	}
	if !strings.Contains(h.logs.String(), `"msg":"job allowed"`) {
		t.Errorf("logs lack the allow:\n%s", h.logs)
	}
}

// TestRecordDenyStartsDenyPath: a denied report records the deny before
// Record returns, without waiting for Docker; the deny path then removes
// the runner container within the 40 s of R11 on the Clock, even when the
// runner does not stop on its own (0002 SPEC R131, R145).
func TestRecordDenyStartsDenyPath(t *testing.T) {
	h, j := newJudgeHarness(t)
	tok := seedToken(t, h, "pool-a", 101, StateBusy)
	h.docker.addRunner("pool-a", "running", "running")
	start := h.clock.Now()

	if v := j.Record("pool-a", "pull_request", denied); v != VerdictDeny {
		t.Fatalf("Record = %v, want deny", v)
	}
	if v, ok := h.table.Authenticate("pool-a", tok, h.clock.Now()); !ok || v != VerdictDeny {
		t.Fatalf("hook reads %v, %v right after Record; want deny", v, ok)
	}
	waitDenyWait(t, h, "pool-a")
	if n := len(h.docker.runnerRemovedNames()); n != 0 {
		t.Fatalf("RemoveContainer calls before the grace = %d, want 0", n)
	}
	h.clock.Advance(denyStopGrace)
	waitJudge(t, j)

	assertDenied(t, h, "pool-a", 101)
	if d := h.clock.Now().Sub(start); d > 40*time.Second {
		t.Errorf("runner removed %v after the deny, want 40s or less", d)
	}
}

// TestRecordAfterExitReturnsRecorded: when another path already began the
// runner's removal, or the verdict is already recorded, Record changes
// nothing and returns what is recorded (0002 SPEC R132).
func TestRecordAfterExitReturnsRecorded(t *testing.T) {
	t.Run("job check timer denied", func(t *testing.T) {
		h, j := newJudgeHarness(t)
		seedToken(t, h, "pool-a", 101, StateBusy)
		h.docker.addRunner("pool-a", "running", "running")
		h.clock.Advance(busyTimeout)
		waitDenyWait(t, h, "pool-a")
		for _, d := range []gate.Decision{allowed, denied} {
			if v := j.Record("pool-a", "push", d); v != VerdictDeny {
				t.Errorf("Record(%+v) = %v, want the timer's deny", d, v)
			}
		}
		h.docker.exit("pool-a")
		waitJudge(t, j)
		assertDenied(t, h, "pool-a", 101)
	})
	t.Run("removal began while pending", func(t *testing.T) {
		h, j := newJudgeHarness(t)
		seedToken(t, h, "pool-a", 101, StateBusy)
		if !h.table.BeginExit("pool-a") {
			t.Fatal("BeginExit = false")
		}
		for _, d := range []gate.Decision{allowed, denied} {
			if v := j.Record("pool-a", "push", d); v != VerdictPending {
				t.Errorf("Record(%+v) = %v, want pending", d, v)
			}
		}
		waitJudge(t, j)
		if n := h.docker.calls(); n != 0 {
			t.Errorf("Docker calls = %d, want 0 (the other path owns the removal)", n)
		}
	})
	t.Run("allowed", func(t *testing.T) {
		h, j := newJudgeHarness(t)
		seedToken(t, h, "pool-a", 101, StateBusy)
		if v := j.Record("pool-a", "push", allowed); v != VerdictAllow {
			t.Fatalf("Record = %v, want allow", v)
		}
		if v := j.Record("pool-a", "pull_request", denied); v != VerdictAllow {
			t.Errorf("Record deny after allow = %v, want allow", v)
		}
		waitJudge(t, j)
		if n := h.docker.calls(); n != 0 || h.state("pool-a") != StateBusy {
			t.Errorf("Docker calls = %d, state %v; want 0 and busy", n, h.state("pool-a"))
		}
	})
	t.Run("unknown runner", func(t *testing.T) {
		h, j := newJudgeHarness(t)
		if v := j.Record("pool-x", "push", denied); v != VerdictPending {
			t.Errorf("Record = %v, want pending", v)
		}
		waitJudge(t, j)
		if n := h.docker.calls(); n != 0 {
			t.Errorf("Docker calls = %d, want 0", n)
		}
	})
}

// TestRecordHaltedRecordsNothing: after Stop or once finalize has begun,
// Record records nothing, calls nothing and returns pending (0002 SPEC
// R131).
func TestRecordHaltedRecordsNothing(t *testing.T) {
	for _, how := range []string{"Stop", "finalize"} {
		t.Run(how, func(t *testing.T) {
			h, j := newJudgeHarness(t)
			seedToken(t, h, "pool-a", 101, StateBusy)
			seedToken(t, h, "pool-b", 102, StateBusy)
			if how == "Stop" {
				j.Stop()
			} else {
				h.finalize()
			}
			if v := j.Record("pool-a", "push", allowed); v != VerdictPending {
				t.Errorf("Record allow = %v, want pending", v)
			}
			if v := j.Record("pool-b", "pull_request", denied); v != VerdictPending {
				t.Errorf("Record deny = %v, want pending", v)
			}
			waitJudge(t, j)
			for _, n := range []string{"pool-a", "pool-b"} {
				if info, _ := h.table.Get(n); info.Verdict != VerdictPending || info.State != StateBusy {
					t.Errorf("%s = %v/%v, want busy and pending", n, info.State, info.Verdict)
				}
			}
			if n := h.docker.calls() + h.gh.calls(); n != 0 {
				t.Errorf("Docker and GitHub calls = %d, want 0", n)
			}
		})
	}
}

// TestRecordCountsAsWork: Wait waits for a Record in progress, so finalize
// cannot pass a verdict being recorded (0002 PLAN M8-2).
func TestRecordCountsAsWork(t *testing.T) {
	for _, d := range []gate.Decision{allowed, denied} {
		h, j := newJudgeHarness(t)
		// Idle: no job check timer for Wait to wait for.
		seedToken(t, h, "pool-a", 101, StateIdle)
		// Record takes the timers' lock once the verdict is recorded; holding
		// it keeps Record in progress.
		j.busy.mu.Lock()
		var once sync.Once
		unlock := func() { once.Do(j.busy.mu.Unlock) }
		// Before the harness's cleanup, which takes the same lock.
		t.Cleanup(unlock)
		done := make(chan Verdict, 1)
		go func() { done <- j.Record("pool-a", "push", d) }()
		// An allow is recorded before the lock, a deny wins the exit first.
		eventually(t, "Record in progress", func() bool {
			info, _ := h.table.Get("pool-a")
			return info.Verdict != VerdictPending || info.State == StateExiting
		})

		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		err := j.Wait(ctx)
		cancel()
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("%+v: Wait = %v while Record runs, want it to wait", d, err)
		}
		unlock()
		<-done
		if d.Allow {
			ctx, cancel = context.WithTimeout(context.Background(), 10*time.Second)
			if err := j.Wait(ctx); err != nil {
				t.Errorf("Wait after Record = %v", err)
			}
			cancel()
		}
	}
}

// logLine returns the first JSON log line whose msg is msg and that has
// runner name, or nil.
func logLine(t *testing.T, logs, msg, name string) map[string]any {
	t.Helper()
	for _, l := range strings.Split(logs, "\n") {
		var m map[string]any
		if json.Unmarshal([]byte(l), &m) != nil {
			continue
		}
		if m["msg"] == msg && m["runner"] == name {
			return m
		}
	}
	return nil
}

// TestDenyLogLine: a denied report logs the deny line with runner, event,
// reason and, when known, head_repo; deny paths without a report (R81,
// R102) log it without event (0002 SPEC R130).
func TestDenyLogLine(t *testing.T) {
	const line = "job denied; waiting for the runner to stop"
	h, j := newJudgeHarness(t)
	for i, n := range []string{"pool-a", "pool-b", "pool-c", "pool-d"} {
		seedToken(t, h, n, int64(101+i), StateBusy)
	}
	j.Record("pool-a", "pull_request_target", denied)
	j.Record("pool-b", "", gate.Decision{Reason: gate.ReasonEventNameInvalid})
	j.Reject("pool-c", "test reject")
	waitJudge(t, j)
	h.clock.Advance(busyTimeout)
	waitJudge(t, j)
	logs := h.logs.String()

	for _, tc := range []struct {
		name string
		want map[string]any
		none []string
	}{
		{"pool-a", map[string]any{"event": "pull_request_target", "reason": gate.ReasonPullRequestFromOtherRepo, "head_repo": "fork/repo"}, nil},
		{"pool-b", map[string]any{"event": "", "reason": gate.ReasonEventNameInvalid}, []string{"head_repo"}},
		{"pool-c", map[string]any{"reason": "test reject"}, []string{"event", "head_repo"}},
		{"pool-d", map[string]any{"reason": "no allow verdict 3m0s after the runner became busy (R81)"}, []string{"event", "head_repo"}},
	} {
		m := logLine(t, logs, line, tc.name)
		if m == nil {
			t.Errorf("%s: no deny line in\n%s", tc.name, logs)
			continue
		}
		for k, v := range tc.want {
			if m[k] != v {
				t.Errorf("%s: %s = %v, want %v", tc.name, k, m[k], v)
			}
		}
		for _, k := range tc.none {
			if _, ok := m[k]; ok {
				t.Errorf("%s: deny line has %s: %v", tc.name, k, m)
			}
		}
	}
}

// TestJudgeOnlyOwnTarget: a target's judge records nothing for, and never
// rejects, a runner of another target on the shared table, and the busy
// mark of that runner arms no timer of this judge (0002 PLAN M11-5).
func TestJudgeOnlyOwnTarget(t *testing.T) {
	h, j := newJudgeHarness(t)
	timers := h.clock.pending()
	seedFor(t, h.table, "other", "pool-b", 2, StateBusy, h.clock.Now())
	if n := h.clock.pending(); n != timers {
		t.Fatalf("pending timers %d after another target's busy mark, want %d", n, timers)
	}
	if v := j.Record("pool-b", "push", gate.Decision{Allow: true}); v != VerdictPending {
		t.Fatalf("Record = %v, want pending", v)
	}
	if v := j.Record("pool-b", "push", gate.Decision{Reason: "x"}); v != VerdictPending {
		t.Fatalf("Record deny = %v, want pending", v)
	}
	j.Reject("pool-b", "test")
	if info, _ := h.table.Get("pool-b"); info.Verdict != VerdictPending || info.State != StateBusy {
		t.Fatalf("another target's runner changed: %+v", info)
	}
}
