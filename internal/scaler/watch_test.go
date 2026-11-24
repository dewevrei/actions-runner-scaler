package scaler

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"
)

// waitDone waits for ch with a failsafe timeout; it is not a timing
// assertion.
func waitDone(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(10 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
	}
}

// watchSeeded seeds an idle runner with containers and a known token and
// watches it. It returns the token and the channel closed when the watch
// ends.
func watchSeeded(t *testing.T, h *harness, name string, id int64) (token string, done <-chan struct{}) {
	t.Helper()
	seed(t, h.table, name, id, StateIdle, h.clock.Now())
	tok := mustToken(t)
	h.table.SetToken(name, tok)
	h.docker.addRunner(name, "running", "running")
	h.s.Watcher().Watch(name)
	done, ok := h.s.Watcher().watchDone(name)
	if !ok {
		t.Fatalf("%s not watched", name)
	}
	return tok.Reveal(), done
}

func count(names []string, name string) int {
	n := 0
	for _, v := range names {
		if v == name {
			n++
		}
	}
	return n
}

// TestWatchExitRemovesRunner: a runner container that ends is removed once,
// resources then registration, and its token stops working (R6, R38).
func TestWatchExitRemovesRunner(t *testing.T) {
	h := newHarness(t, testPool(2, 0))
	tok, done := watchSeeded(t, h, "pool-a", 7)
	h.docker.exit("pool-a")
	waitDone(t, done, "watch of pool-a")

	if n := count(h.docker.removedNames(), "pool-a"); n != 1 {
		t.Fatalf("RemoveResources calls %d, want 1", n)
	}
	if got := h.gh.removedIDs(); !slices.Equal(got, []int64{7}) {
		t.Fatalf("RemoveRunner ids %v, want [7]", got)
	}
	if !h.gone("pool-a") || h.docker.has("pool-a", ResourceRunner) {
		t.Fatal("runner left in table or Docker")
	}
	if _, ok := h.table.Authenticate("pool-a", tok, h.clock.Now()); ok {
		t.Fatal("token valid after the container ended (R38)")
	}
	if len(h.s.PendingUnregistrations()) != 0 || h.fatalCount() != 0 {
		t.Fatal("registration pending or fatal reported")
	}
	if err := h.s.WaitInFlight(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// TestWatchStartedRunner: the start path watches every runner it starts.
func TestWatchStartedRunner(t *testing.T) {
	h := newHarness(t, testPool(1, 0))
	h.mustScale(statsMsg(1, 1))
	name := h.docker.startedNames()[0]
	done, ok := h.s.Watcher().watchDone(name)
	if !ok {
		t.Fatal("started runner not watched")
	}
	h.docker.exit(name)
	waitDone(t, done, "watch")
	if !h.gone(name) || len(h.gh.removedIDs()) != 1 {
		t.Fatal("ended runner not removed")
	}
}

// TestWatchNotFoundIsSuccess: a container already gone, and removals that
// find nothing, count as success (R50).
func TestWatchNotFoundIsSuccess(t *testing.T) {
	h := newHarness(t, testPool(2, 0))
	h.docker.setRemoveErr(func(string) error { return statusError(404) })
	h.gh.setRemoveErr(func(int64) error { return statusError(404) })
	seed(t, h.table, "pool-a", 7, StateIdle, testEpoch)
	h.s.Watcher().Watch("pool-a") // no container: Wait reports 404
	done, ok := h.s.Watcher().watchDone("pool-a")
	if ok {
		waitDone(t, done, "watch")
	}
	if err := h.s.WaitInFlight(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !h.gone("pool-a") || h.errs.count() != 0 || len(h.s.PendingUnregistrations()) != 0 {
		t.Fatalf("gone %v errors %d pending %v; want removed without errors",
			h.gone("pool-a"), h.errs.count(), h.s.PendingUnregistrations())
	}
	if h.clock.pending() != 0 {
		t.Fatal("retry scheduled after not found")
	}
}

// TestWatchPermanentNotRetried: permanent removal errors are logged and
// recorded, never retried, and start no drain (R42, R98).
func TestWatchPermanentNotRetried(t *testing.T) {
	h := newHarness(t, testPool(2, 0))
	h.docker.setRemoveErr(func(string) error { return statusError(400) })
	h.gh.setRemoveErr(func(int64) error { return errPermanent })
	_, done := watchSeeded(t, h, "pool-a", 7)
	h.docker.exit("pool-a")
	waitDone(t, done, "watch")
	h.clock.Advance(time.Hour)
	if n := len(h.docker.removedNames()); n != 1 {
		t.Fatalf("RemoveResources calls %d, want 1", n)
	}
	if n := len(h.gh.removedIDs()); n != 1 {
		t.Fatalf("RemoveRunner calls %d, want 1", n)
	}
	if h.errs.count() != 2 || h.fatalCount() != 0 || h.table.Drain() != DrainNone {
		t.Fatalf("errors %d fatal %d drain %v; want 2, 0, none", h.errs.count(), h.fatalCount(), h.table.Drain())
	}
	if info, _ := h.table.Get("pool-a"); info.AbandonReason != AbandonPermanent {
		t.Fatalf("row %+v, want abandoned as permanent", info)
	}
	if h.errs.last().where != unregEnded.where {
		t.Fatalf("last error at %q, want %q", h.errs.last().where, unregEnded.where)
	}
}

// TestWatchTransientRetried: a transient removal failure is retried on the
// removal backoff.
func TestWatchTransientRetried(t *testing.T) {
	h := newHarness(t, testPool(2, 0))
	h.docker.setRemoveErr(failTimes(1, errTransient))
	_, done := watchSeeded(t, h, "pool-a", 7)
	h.docker.exit("pool-a")
	waitDone(t, done, "watch")
	if h.gone("pool-a") {
		t.Fatal("removed despite the failure")
	}
	h.clock.Advance(time.Second)
	if !h.gone("pool-a") || len(h.docker.removedNames()) != 2 {
		t.Fatal("not removed by the retry")
	}
}

// TestWatchAfterFinalize: a Watch that comes just after finalize began makes
// no call, and finalize ends the waits in progress.
func TestWatchAfterFinalize(t *testing.T) {
	h := newHarness(t, testPool(2, 0))
	_, done := watchSeeded(t, h, "pool-a", 7)
	seed(t, h.table, "pool-b", 8, StateIdle, testEpoch)
	h.docker.addRunner("pool-b", "running", "running")
	h.finalize()
	waitDone(t, done, "watch stopped by finalize")
	waits := h.docker.queries()

	h.s.Watcher().Watch("pool-b")
	if _, ok := h.s.Watcher().watchDone("pool-b"); ok {
		t.Fatal("runner watched after finalize")
	}
	h.docker.exit("pool-a")
	if err := h.s.Watcher().Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	if h.docker.queries() != waits || h.docker.calls() != 0 || h.gh.calls() != 0 {
		t.Fatalf("calls after finalize: queries %d→%d Docker %d GitHub %d; want none",
			waits, h.docker.queries(), h.docker.calls(), h.gh.calls())
	}
}

// TestWatchStop: Watch returns at once; Stop ends the waits and Wait sees
// them return without removing anything.
func TestWatchStop(t *testing.T) {
	h := newHarness(t, testPool(2, 0))
	watchSeeded(t, h, "pool-a", 7)
	h.s.Watcher().Stop()
	if err := h.s.Watcher().Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	if h.docker.calls() != 0 || h.gh.calls() != 0 || h.gone("pool-a") {
		t.Fatal("stopped watch removed the runner")
	}
	h.s.Watcher().Watch("pool-a")
	if _, ok := h.s.Watcher().watchDone("pool-a"); ok {
		t.Fatal("watch started after Stop")
	}
}

// TestWatchWaitFailureLeftToReconcile: a failed wait is logged and recorded
// and leaves the runner to reconciliation, which removes it once its
// container has stopped.
func TestWatchWaitFailureLeftToReconcile(t *testing.T) {
	h := newHarness(t, testPool(2, 0))
	h.docker.waitErr = func(string) error { return errTransient }
	_, done := watchSeeded(t, h, "pool-a", 7)
	waitDone(t, done, "watch")
	if h.gone("pool-a") || h.errs.count() != 1 || h.errs.last().where != "watch runner container" {
		t.Fatalf("gone %v errors %d; want kept and one recorded error", h.gone("pool-a"), h.errs.count())
	}
	if !strings.Contains(h.logs.String(), "left to reconciliation") {
		t.Fatal("no log for the failed wait")
	}
	h.docker.exit("pool-a")
	r, _ := newTestReconciler(t, h)
	r.round()
	if !h.gone("pool-a") {
		t.Fatal("reconciliation did not remove the stopped runner")
	}
}

// TestWatchAndReconcileRemoveOnce: the watcher and a reconciliation round
// that both see the ended runner remove it once.
func TestWatchAndReconcileRemoveOnce(t *testing.T) {
	h := newHarness(t, testPool(2, 0))
	_, done := watchSeeded(t, h, "pool-a", 7)
	r, _ := newTestReconciler(t, h)
	h.docker.exit("pool-a")
	r.round()
	waitDone(t, done, "watch")
	if err := h.s.WaitInFlight(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := count(h.docker.removedNames(), "pool-a"); n != 1 {
		t.Fatalf("RemoveResources calls %d, want 1", n)
	}
	if n := len(h.gh.removedIDs()); n != 1 {
		t.Fatalf("RemoveRunner calls %d, want 1", n)
	}
}

// doneRunner starts a runner through Scale, sends JobStarted and then
// JobCompleted, and returns its name and the time of the first
// JobCompleted.
func doneRunner(t *testing.T, h *harness) (string, time.Time) {
	t.Helper()
	// Other live runners keep their places: the messages ask for one more.
	live := h.table.Counts().Live()
	h.mustScale(statsMsg(1, live+1))
	name := h.docker.startedNames()[len(h.docker.startedNames())-1]
	h.mustScale(withStarted(statsMsg(2, live+1), name, "job-1"))
	at := h.clock.Now()
	h.mustScale(withCompleted(statsMsg(3, live), name, "job-1"))
	if h.state(name) != StateDone {
		t.Fatalf("state %v, want done", h.state(name))
	}
	return name, at
}

// TestDoneCleanupTimer: nothing is removed at 4 min 59 s; the runner's own
// timer removes it at exactly 5 min, without a reconciliation, once, with
// its registration (R101).
func TestDoneCleanupTimer(t *testing.T) {
	h := newHarness(t, testPool(2, 0))
	name, _ := doneRunner(t, h)
	h.clock.Advance(doneCleanupDelay - time.Second)
	if count(h.docker.removedNames(), name) != 0 || h.gh.calls() != 1 {
		t.Fatal("removal began before 5 minutes")
	}
	h.clock.Advance(time.Second)
	if n := count(h.docker.removedNames(), name); n != 1 || !h.gone(name) {
		t.Fatalf("removals %d at 5 minutes, want 1 and the row gone", n)
	}
	if got := h.gh.removedIDs(); !slices.Equal(got, []int64{100}) {
		t.Fatalf("RemoveRunner ids %v, want [100]", got)
	}
	if h.docker.listCalls() != 0 {
		t.Fatal("the timer waited for a reconciliation")
	}
	h.clock.Advance(time.Hour)
	if n := count(h.docker.removedNames(), name); n != 1 {
		t.Fatalf("removals %d, want 1", n)
	}
}

// TestDoneCleanupRedelivered: a JobCompleted delivered again neither moves
// the base time nor sets a second timer (R101).
func TestDoneCleanupRedelivered(t *testing.T) {
	h := newHarness(t, testPool(2, 0))
	name, at := doneRunner(t, h)
	timers := h.clock.pending()
	h.clock.Advance(time.Minute)
	h.mustScale(withCompleted(statsMsg(4, 0), name, "job-1"))
	if info, _ := h.table.Get(name); !info.CompletedAt.Equal(at) {
		t.Fatalf("CompletedAt %v, want %v", info.CompletedAt, at)
	}
	if h.clock.pending() != timers {
		t.Fatalf("timers %d after redelivery, want %d", h.clock.pending(), timers)
	}
	h.clock.Advance(4*time.Minute - time.Millisecond)
	if h.gone(name) || count(h.docker.removedNames(), name) != 0 {
		t.Fatal("removed before 5 minutes from the first JobCompleted")
	}
	h.clock.Advance(time.Millisecond)
	if n := count(h.docker.removedNames(), name); n != 1 {
		t.Fatalf("removals %d, want 1", n)
	}
}

// TestDoneCleanupDuringDrain: R101 applies on both drain kinds.
func TestDoneCleanupDuringDrain(t *testing.T) {
	for _, kind := range []DrainKind{DrainSignal, DrainError} {
		t.Run(kind.String(), func(t *testing.T) {
			h := newHarness(t, testPool(2, 0))
			name, _ := doneRunner(t, h)
			h.table.SetDraining(kind)
			h.clock.Advance(doneCleanupDelay)
			if !h.gone(name) || count(h.docker.removedNames(), name) != 1 {
				t.Fatal("done runner not removed during drain")
			}
		})
	}
}

// TestDoneCleanupFinalize: once finalize began the timer makes no call.
func TestDoneCleanupFinalize(t *testing.T) {
	h := newHarness(t, testPool(2, 0))
	name, _ := doneRunner(t, h)
	docker, gh := h.docker.calls(), h.gh.calls()
	h.finalize()
	h.clock.Advance(time.Hour)
	if h.docker.calls() != docker || h.gh.calls() != gh || h.gone(name) {
		t.Fatalf("calls after finalize: Docker %d→%d GitHub %d→%d", docker, h.docker.calls(), gh, h.gh.calls())
	}
	if err := h.s.WaitInFlight(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// TestDoneCleanupOnce: the timer and reconciliation at the same moment,
// and repeated calls, begin the removal once.
func TestDoneCleanupOnce(t *testing.T) {
	h := newHarness(t, testPool(2, 0))
	name, _ := doneRunner(t, h)
	r, _ := newTestReconciler(t, h)
	r.Start() // ticks land on the 5-minute mark too
	h.clock.Advance(doneCleanupDelay)
	if h.s.doneCleanup(name) {
		t.Fatal("second doneCleanup began a removal")
	}
	h.clock.Advance(time.Minute)
	if n := count(h.docker.removedNames(), name); n != 1 {
		t.Fatalf("removals %d, want 1", n)
	}
	if n := len(h.gh.removedIDs()); n != 1 {
		t.Fatalf("RemoveRunner calls %d, want 1", n)
	}
}

// TestDoneCleanupByReconcile: a done runner with no timer is removed by the
// first reconciliation after its 5 minutes (R101's backup).
func TestDoneCleanupByReconcile(t *testing.T) {
	h := newHarness(t, testPool(2, 0))
	seed(t, h.table, "pool-a", 7, StateDone, h.clock.Now()) // no timer
	h.docker.addRunner("pool-a", "running", "running")
	r, _ := newTestReconciler(t, h)
	h.clock.Advance(10 * time.Second)
	r.Start() // ticks at 40 s, 70 s, … 310 s
	h.clock.Advance(doneCleanupDelay - 10*time.Second - time.Millisecond)
	if count(h.docker.removedNames(), "pool-a") != 0 {
		t.Fatal("removed before 5 minutes")
	}
	h.clock.Advance(10 * time.Second)
	if count(h.docker.removedNames(), "pool-a") != 0 {
		t.Fatal("removed before the next round")
	}
	h.clock.Advance(time.Millisecond) // the round at 310 s
	if !h.gone("pool-a") || count(h.docker.removedNames(), "pool-a") != 1 {
		t.Fatal("first round after 5 minutes did not remove the done runner")
	}
}

// TestDoneCleanupWithin5m30s: even when RemoveResources takes 20 s and the
// repository runner list hangs, the runner container is gone within 5 min
// 30 s of the first JobCompleted: the timer begins at exactly 5 min, and the
// rounds that come meanwhile neither delay nor repeat it (R101).
func TestDoneCleanupWithin5m30s(t *testing.T) {
	h := newHarness(t, testPool(3, 0))
	r, repo := newTestReconciler(t, h)
	// An unjudged idle runner makes every round start R103.
	seed(t, h.table, "pool-b", 50, StateIdle, h.clock.Now())
	h.docker.addRunner("pool-b", "running", "running")
	repo.during = func(ctx context.Context) { <-ctx.Done() }

	h.clock.Advance(7 * time.Second)
	r.Start() // ticks at 37 s, 67 s, …
	// JobCompleted at 19 s: the 5-minute mark (319 s) falls between ticks,
	// so only the runner's own timer can begin the removal at that moment.
	h.clock.Advance(12 * time.Second)
	name, at := doneRunner(t, h)

	var began, ended time.Time
	h.docker.setDuringRemove(func(ctx context.Context, n string) error {
		if n == name {
			began = h.clock.Now()
			h.clock.Advance(cleanupTimeout) // the call takes its whole bound
			ended = h.clock.Now()
		}
		return nil
	})
	h.clock.Advance(10 * time.Minute)

	if !began.Equal(at.Add(doneCleanupDelay)) {
		t.Fatalf("removal began %v after JobCompleted, want exactly %v", began.Sub(at), doneCleanupDelay)
	}
	if d := ended.Sub(at); d > doneCleanupDelay+30*time.Second {
		t.Fatalf("container gone %v after JobCompleted, want at most 5m30s", d)
	}
	if h.docker.has(name, ResourceRunner) || count(h.docker.removedNames(), name) != 1 {
		t.Fatal("runner container not removed exactly once")
	}
	if n := repo.callCount(); n != 1 {
		t.Fatalf("repository runner lists %d, want 1 (one at a time)", n)
	}
}

func TestDoneCleanupNotFatal(t *testing.T) {
	h := newHarness(t, testPool(2, 0))
	h.docker.setRemoveErr(func(string) error { return statusError(400) })
	name, _ := doneRunner(t, h)
	h.clock.Advance(doneCleanupDelay)
	if h.fatalCount() != 0 || h.table.Drain() != DrainNone {
		t.Fatal("R101 removal failure started a drain")
	}
	if last := h.errs.last(); !errors.Is(last.err, statusError(400)) {
		t.Fatalf("last error %+v, want the removal error", last)
	}
	if info, _ := h.table.Get(name); info.AbandonReason != AbandonPermanent {
		t.Fatalf("row %+v, want abandoned as permanent", info)
	}
}
