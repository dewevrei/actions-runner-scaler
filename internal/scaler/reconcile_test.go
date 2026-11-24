package scaler

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dewevrei/actions-runner-scaler/internal/retry"
)

// fakeRepo is the repository runner list.
type fakeRepo struct {
	mu      sync.Mutex
	runners []RepoRunner
	calls   int
	err     func() error
	// during runs inside the call with its context.
	during func(ctx context.Context)
}

func (f *fakeRepo) ListRepoRunners(ctx context.Context) ([]RepoRunner, error) {
	f.mu.Lock()
	f.calls++
	errFn, during, out := f.err, f.during, slices.Clone(f.runners)
	f.mu.Unlock()
	if during != nil {
		during(ctx)
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if errFn != nil {
		if err := errFn(); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func (f *fakeRepo) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func (f *fakeRepo) set(runners ...RepoRunner) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.runners = runners
}

func (f *fakeRepo) setErr(fn func() error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.err = fn
}

func newTestReconciler(t *testing.T, h *harness) (*Reconciler, *fakeRepo) {
	t.Helper()
	repo := &fakeRepo{}
	r, err := NewReconciler(ReconcilerConfig{Scaler: h.s, Resources: h.docker, Processes: h.docker, Runners: repo})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		r.Stop()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := r.Wait(ctx); err != nil {
			t.Errorf("reconciler left work: %v", err)
		}
	})
	return r, repo
}

// roundAndList runs one round and waits for its R103 goroutine.
func roundAndList(t *testing.T, r *Reconciler) {
	t.Helper()
	r.round()
	if err := r.waitListing(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func (f *fakeContainers) listCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lists
}

func (f *fakeContainers) setDuringRemove(fn func(context.Context, string) error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.duringRemove = fn
}

func TestNewReconcilerRequiresDependencies(t *testing.T) {
	_, err := NewReconciler(ReconcilerConfig{})
	if err == nil || !strings.Contains(err.Error(), "Scaler") || !strings.Contains(err.Error(), "Runners") {
		t.Fatalf("NewReconciler with nothing = %v, want an error naming the missing ones", err)
	}
}

// TestReconcileTicks: rounds come every 30 s on the injected clock, and none
// after Stop.
func TestReconcileTicks(t *testing.T) {
	h := newHarness(t, testPool(2, 0))
	r, _ := newTestReconciler(t, h)
	r.Start()
	r.Start()
	h.clock.Advance(reconcileInterval - time.Millisecond)
	if n := h.docker.listCalls(); n != 0 {
		t.Fatalf("lists %d before 30 s", n)
	}
	h.clock.Advance(time.Millisecond)
	if n := h.docker.listCalls(); n != 1 {
		t.Fatalf("lists %d at 30 s, want 1", n)
	}
	h.clock.Advance(2 * reconcileInterval)
	if n := h.docker.listCalls(); n != 3 {
		t.Fatalf("lists %d at 90 s, want 3", n)
	}
	r.Stop()
	if err := r.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	h.clock.Advance(time.Hour)
	if n := h.docker.listCalls(); n != 3 {
		t.Fatalf("lists %d after Stop, want 3", n)
	}
}

// TestReconcileTableOnlyRows: a row without a runner container, or with a
// stopped one, is handled as ended; a starting row is left to its start
// path (R6, R42).
func TestReconcileTableOnlyRows(t *testing.T) {
	h := newHarness(t, testPool(4, 0))
	seed(t, h.table, "pool-idle", 1, StateIdle, testEpoch)
	seed(t, h.table, "pool-busy", 2, StateBusy, testEpoch)
	seed(t, h.table, "pool-start", 3, StateStarting, testEpoch)
	seed(t, h.table, "pool-exited", 4, StateIdle, testEpoch)
	h.docker.addRunner("pool-exited", "exited", "running")
	r, _ := newTestReconciler(t, h)
	r.round()
	for _, name := range []string{"pool-idle", "pool-busy", "pool-exited"} {
		if !h.gone(name) {
			t.Errorf("%s not removed", name)
		}
	}
	if h.gone("pool-start") || slices.Contains(h.docker.removedNames(), "pool-start") {
		t.Fatal("starting row touched")
	}
	ids := h.gh.removedIDs()
	slices.Sort(ids)
	if !slices.Equal(ids, []int64{1, 2, 4}) {
		t.Fatalf("RemoveRunner ids %v, want [1 2 4]", ids)
	}
}

// TestReconcileLeftovers: resources whose runner is not in the table are
// removed by runner-name label: a lone runner container is force-removed,
// unpaired BuildKit containers and networks go too; a row's resources stay.
func TestReconcileLeftovers(t *testing.T) {
	h := newHarness(t, testPool(2, 0))
	seed(t, h.table, "pool-a", 1, StateIdle, testEpoch)
	h.docker.addRunner("pool-a", "running", "running")
	h.docker.addResource(Resource{Kind: ResourceRunner, Name: "pool-x", RunnerName: "pool-x", State: "running"})
	h.docker.addResource(Resource{Kind: ResourceBuildKit, Name: "pool-y-buildkit", RunnerName: "pool-y", State: "running"})
	h.docker.addResource(Resource{Kind: ResourceNetwork, Name: "pool-z-net", RunnerName: "pool-z"})
	r, _ := newTestReconciler(t, h)
	r.round()
	got := h.docker.removedNames()
	slices.Sort(got)
	if !slices.Equal(got, []string{"pool-x", "pool-y", "pool-z"}) {
		t.Fatalf("removed %v, want the three leftovers", got)
	}
	if h.gone("pool-a") || !h.docker.has("pool-a", ResourceRunner) {
		t.Fatal("paired runner touched")
	}
	if n := len(h.gh.removedIDs()); n != 0 {
		t.Fatalf("RemoveRunner calls %d for leftovers, want 0 (registration is R17's)", n)
	}
	r.round()
	if n := len(h.docker.removedNames()); n != 3 {
		t.Fatalf("removals %d after a second round, want 3", n)
	}
}

// TestReconcileLeftoverPermanentNotRetried: a leftover whose removal failed
// with a permanent error is not tried again while it is listed (R42); a new
// resource of that runner is.
func TestReconcileLeftoverPermanentNotRetried(t *testing.T) {
	h := newHarness(t, testPool(2, 0))
	h.docker.addResource(Resource{Kind: ResourceBuildKit, Name: "pool-y-buildkit", RunnerName: "pool-y", State: "running"})
	h.docker.setRemoveErr(func(string) error { return statusError(400) })
	r, _ := newTestReconciler(t, h)
	r.round()
	h.clock.Advance(time.Hour)
	r.round()
	r.round()
	if n := len(h.docker.removedNames()); n != 1 {
		t.Fatalf("removals %d, want 1", n)
	}
	if h.errs.count() != 1 || h.fatalCount() != 0 {
		t.Fatalf("errors %d fatal %d; want 1 and 0", h.errs.count(), h.fatalCount())
	}
	h.docker.addResource(Resource{Kind: ResourceNetwork, Name: "pool-y-net", RunnerName: "pool-y"})
	r.round()
	if n := len(h.docker.removedNames()); n != 2 {
		t.Fatalf("removals %d after a new resource appeared, want 2", n)
	}
}

// TestReconcileLeftoverTransientRetried: a transient failure is retried on
// the removal backoff, and rounds meanwhile start no second removal.
func TestReconcileLeftoverTransientRetried(t *testing.T) {
	h := newHarness(t, testPool(2, 0))
	h.docker.addResource(Resource{Kind: ResourceNetwork, Name: "pool-z-net", RunnerName: "pool-z"})
	h.docker.setRemoveErr(failTimes(1, errTransient))
	r, _ := newTestReconciler(t, h)
	r.round()
	r.round()
	if n := len(h.docker.removedNames()); n != 1 {
		t.Fatalf("removals %d while a retry is pending, want 1", n)
	}
	h.clock.Advance(time.Second)
	if n := len(h.docker.removedNames()); n != 2 || h.docker.has("pool-z", ResourceNetwork) {
		t.Fatalf("removals %d, want 2 and the network gone", n)
	}
}

// TestReconcileR67: an idle runner whose BuildKit container is stopped or
// gone is removed by the scale-down rules: RemoveRunner, then its
// resources. A busy runner is left alone.
func TestReconcileR67(t *testing.T) {
	h := newHarness(t, testPool(4, 0))
	seed(t, h.table, "pool-stopped", 1, StateIdle, testEpoch)
	h.docker.addRunner("pool-stopped", "running", "exited")
	seed(t, h.table, "pool-missing", 2, StateIdle, testEpoch)
	h.docker.addRunner("pool-missing", "running", "")
	seed(t, h.table, "pool-busy", 3, StateBusy, testEpoch)
	h.docker.addRunner("pool-busy", "running", "exited")
	unregisteredFirst := true
	h.docker.setDuringRemove(func(_ context.Context, name string) error {
		id := map[string]int64{"pool-stopped": 1, "pool-missing": 2}[name]
		if !slices.Contains(h.gh.removedIDs(), id) {
			unregisteredFirst = false
		}
		return nil
	})
	r, _ := newTestReconciler(t, h)
	r.round()
	if !h.gone("pool-stopped") || !h.gone("pool-missing") {
		t.Fatal("idle runner with a broken BuildKit not removed (R67)")
	}
	if !unregisteredFirst {
		t.Fatal("resources removed before RemoveRunner (R37)")
	}
	if h.gone("pool-busy") || slices.Contains(h.gh.removedIDs(), 3) {
		t.Fatal("busy runner removed")
	}
}

// TestReconcileR67Permanent: R67 follows R94: a permanent RemoveRunner error
// keeps the runner and is not retried.
func TestReconcileR67Permanent(t *testing.T) {
	h := newHarness(t, testPool(2, 0))
	seed(t, h.table, "pool-a", 1, StateIdle, testEpoch)
	h.docker.addRunner("pool-a", "running", "exited")
	h.gh.setRemoveErr(func(int64) error { return errPermanent })
	r, _ := newTestReconciler(t, h)
	r.round()
	r.round()
	if n := len(h.gh.removedIDs()); n != 1 || h.gone("pool-a") || h.fatalCount() != 0 {
		t.Fatalf("RemoveRunner calls %d gone %v; want 1 and kept", n, h.gone("pool-a"))
	}
}

// TestReconcileClaimsAbandoned: an abandoned row is claimed and removed once
// more; one abandoned on a permanent error is never touched (R42).
func TestReconcileClaimsAbandoned(t *testing.T) {
	h := newHarness(t, testPool(4, 0))
	seed(t, h.table, "pool-a", 1, StateExiting, testEpoch)
	h.table.AbandonExit("pool-a", AbandonRetriesExhausted)
	h.docker.addRunner("pool-a", "running", "running")
	seed(t, h.table, "pool-p", 2, StateExiting, testEpoch)
	h.table.AbandonExit("pool-p", AbandonPermanent)
	h.docker.addRunner("pool-p", "running", "running")
	seed(t, h.table, "pool-o", 3, StateExiting, testEpoch) // owned, not abandoned
	h.docker.addRunner("pool-o", "running", "running")
	r, _ := newTestReconciler(t, h)
	r.round()
	r.round()
	if !h.gone("pool-a") || count(h.docker.removedNames(), "pool-a") != 1 {
		t.Fatal("abandoned row not removed exactly once")
	}
	if got := h.gh.removedIDs(); !slices.Equal(got, []int64{1}) {
		t.Fatalf("RemoveRunner ids %v, want [1]", got)
	}
	for _, name := range []string{"pool-p", "pool-o"} {
		if h.gone(name) || count(h.docker.removedNames(), name) != 0 {
			t.Fatalf("%s touched", name)
		}
	}
}

// TestReconcileR83: Runner.Worker in an unjudged runner marks it busy, and
// nothing else; done runners and allowed runners are not marked.
func TestReconcileR83(t *testing.T) {
	h := newHarness(t, testPool(4, 0))
	worker := "/home/runner/bin/Runner.Worker spawnclient 107 110"
	for _, name := range []string{"pool-a", "pool-b", "pool-allowed"} {
		seed(t, h.table, name, 1, StateIdle, testEpoch)
		h.docker.addRunner(name, "running", "running")
	}
	seed(t, h.table, "pool-done", 4, StateDone, testEpoch)
	h.docker.addRunner("pool-done", "running", "running")
	h.table.Allow("pool-allowed")
	h.docker.setProcs("pool-a", "/sbin/docker-init -- /usr/local/bin/scaler-entrypoint ./run.sh", worker)
	h.docker.setProcs("pool-b", "/home/runner/bin/Runner.Listener run")
	h.docker.setProcs("pool-done", worker)
	h.docker.setProcs("pool-allowed", worker)
	r, _ := newTestReconciler(t, h)
	r.round()
	if h.state("pool-a") != StateBusy || !slices.Equal(h.busyCalls(), []string{"pool-a"}) {
		t.Fatalf("pool-a %v busy calls %v; want busy and one BusyFunc", h.state("pool-a"), h.busyCalls())
	}
	if h.state("pool-b") != StateIdle || h.state("pool-done") != StateDone || h.state("pool-allowed") != StateIdle {
		t.Fatal("runner without Worker, done or allowed runner marked busy")
	}
	if h.docker.calls() != 0 || h.gh.calls() != 0 {
		t.Fatal("R83 removed something")
	}
	procs := h.docker.procCalls()
	r.round()
	if n := h.docker.procCalls() - procs; n != 1 {
		t.Fatalf("process lists %d in the second round, want 1 (pool-b only)", n)
	}
}

func TestHasWorker(t *testing.T) {
	for cmd, want := range map[string]bool{
		"/home/runner/bin/Runner.Worker spawnclient 1 2": true,
		"Runner.Worker":                    true,
		"dotnet bin/Runner.Worker.dll":     true,
		"/home/runner/bin/Runner.Listener": false,
		"grep Runner.Workers":              false,
		"":                                 false,
	} {
		if got := hasWorker([]string{cmd}); got != want {
			t.Errorf("hasWorker(%q) = %v, want %v", cmd, got, want)
		}
	}
}

// TestReconcileR103: the repository list's busy flag marks an unjudged,
// not done runner busy; it is rechecked under the table's lock.
func TestReconcileR103(t *testing.T) {
	h := newHarness(t, testPool(4, 0))
	for _, name := range []string{"pool-a", "pool-b", "pool-allowed"} {
		seed(t, h.table, name, 1, StateIdle, testEpoch)
		h.docker.addRunner(name, "running", "running")
	}
	seed(t, h.table, "pool-done", 4, StateDone, testEpoch)
	h.docker.addRunner("pool-done", "running", "running")
	h.table.Allow("pool-allowed")
	r, repo := newTestReconciler(t, h)
	repo.set(
		RepoRunner{ID: 1, Name: "pool-a", Busy: true},
		RepoRunner{ID: 2, Name: "pool-b"},
		RepoRunner{ID: 3, Name: "pool-done", Busy: true},
		RepoRunner{ID: 4, Name: "pool-allowed", Busy: true},
		RepoRunner{ID: 5, Name: "other", Busy: true},
	)
	roundAndList(t, r)
	if h.state("pool-a") != StateBusy || h.state("pool-b") != StateIdle ||
		h.state("pool-done") != StateDone || h.state("pool-allowed") != StateIdle {
		t.Fatalf("states a=%v b=%v done=%v allowed=%v", h.state("pool-a"), h.state("pool-b"),
			h.state("pool-done"), h.state("pool-allowed"))
	}
	if !slices.Equal(h.busyCalls(), []string{"pool-a"}) {
		t.Fatalf("busy calls %v", h.busyCalls())
	}
}

// TestReconcileR103NotNeeded: with every runner judged, busy or done, R103
// does not list.
func TestReconcileR103NotNeeded(t *testing.T) {
	h := newHarness(t, testPool(4, 0))
	seed(t, h.table, "pool-busy", 1, StateBusy, testEpoch)
	h.docker.addRunner("pool-busy", "running", "running")
	seed(t, h.table, "pool-done", 2, StateDone, testEpoch)
	h.docker.addRunner("pool-done", "running", "running")
	r, repo := newTestReconciler(t, h)
	roundAndList(t, r)
	if repo.callCount() != 0 {
		t.Fatal("listed with no runner to check")
	}
}

// TestReconcileR103Failure: a failed list is logged and skipped; the same
// error again is logged and recorded once, a different one again (R103).
func TestReconcileR103Failure(t *testing.T) {
	h := newHarness(t, testPool(2, 0))
	seed(t, h.table, "pool-a", 1, StateIdle, testEpoch)
	h.docker.addRunner("pool-a", "running", "running")
	r, repo := newTestReconciler(t, h)
	repo.set(RepoRunner{Name: "pool-a", Busy: true})
	repo.setErr(func() error { return errPermanent })
	roundAndList(t, r)
	roundAndList(t, r)
	roundAndList(t, r)
	if n := repo.callCount(); n != 3 {
		t.Fatalf("lists %d, want 3 (a permanent error is retried next round)", n)
	}
	if h.errs.count() != 1 || h.errs.last().where != "reconcile list repository runners" {
		t.Fatalf("recorded %d (%q), want 1", h.errs.count(), h.errs.last().where)
	}
	if n := strings.Count(h.logs.String(), `"level":"WARN","msg":"listing repository runners failed`); n != 1 {
		t.Fatalf("warnings %d, want 1", n)
	}
	repo.setErr(func() error { return errTransient })
	roundAndList(t, r)
	if h.errs.count() != 2 {
		t.Fatalf("recorded %d after a different error, want 2", h.errs.count())
	}
	if h.state("pool-a") != StateIdle || h.fatalCount() != 0 || h.table.Drain() != DrainNone {
		t.Fatal("failed list changed state or started a drain (R98)")
	}
	repo.setErr(nil)
	roundAndList(t, r)
	if h.state("pool-a") != StateBusy {
		t.Fatal("not marked busy once the list worked")
	}
}

// TestReconcileR103RateLimit: after a rate limit the list is not called
// before R52's time.
func TestReconcileR103RateLimit(t *testing.T) {
	h := newHarness(t, testPool(2, 0))
	seed(t, h.table, "pool-a", 1, StateIdle, testEpoch)
	h.docker.addRunner("pool-a", "running", "running")
	r, repo := newTestReconciler(t, h)
	repo.setErr(func() error { return &retry.RateLimitError{RetryAfter: 90 * time.Second} })
	roundAndList(t, r)
	for range 2 {
		h.clock.Advance(reconcileInterval)
		roundAndList(t, r)
	}
	if n := repo.callCount(); n != 1 {
		t.Fatalf("lists %d before the rate limit ended, want 1", n)
	}
	h.clock.Advance(reconcileInterval)
	roundAndList(t, r)
	if n := repo.callCount(); n != 2 {
		t.Fatalf("lists %d at the allowed time, want 2", n)
	}
}

// TestReconcileR103OneAtATime: a hanging list does not hold up the round,
// and no second list starts while it runs; Stop ends it, and no list
// follows (M5-8).
func TestReconcileR103OneAtATime(t *testing.T) {
	h := newHarness(t, testPool(2, 0))
	seed(t, h.table, "pool-a", 1, StateIdle, testEpoch)
	h.docker.addRunner("pool-a", "running", "running")
	r, repo := newTestReconciler(t, h)
	started := make(chan struct{})
	var once sync.Once
	repo.during = func(ctx context.Context) {
		once.Do(func() { close(started) })
		<-ctx.Done()
	}
	r.Start()
	h.clock.Advance(reconcileInterval)
	waitDone(t, started, "R103 list")
	h.clock.Advance(3 * reconcileInterval)
	if n := h.docker.listCalls(); n != 4 {
		t.Fatalf("Docker lists %d, want 4 (rounds not held up)", n)
	}
	if n := repo.callCount(); n != 1 {
		t.Fatalf("repository lists %d, want 1", n)
	}
	r.Stop()
	if err := r.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	h.clock.Advance(time.Hour)
	if n := repo.callCount(); n != 1 {
		t.Fatalf("repository lists %d after Stop, want 1", n)
	}
}

// TestReconcileFinalize: once finalize began rounds make no call, a list in
// flight ends at the finalize deadline, and after Stop and Wait nothing is
// called.
func TestReconcileFinalize(t *testing.T) {
	h := newHarness(t, testPool(2, 0))
	seed(t, h.table, "pool-a", 1, StateIdle, testEpoch)
	h.docker.addRunner("pool-a", "running", "exited")
	h.docker.addResource(Resource{Kind: ResourceNetwork, Name: "pool-z-net", RunnerName: "pool-z"})
	r, repo := newTestReconciler(t, h)
	r.Start()
	h.finalize()
	queries, docker, gh := h.docker.queries(), h.docker.calls(), h.gh.calls()
	h.clock.Advance(time.Hour)
	r.round()
	if err := r.Wait(context.Background()); err != nil {
		t.Fatalf("Wait = %v; rounds should stop at finalize", err)
	}
	if h.docker.queries() != queries || h.docker.calls() != docker || h.gh.calls() != gh || repo.callCount() != 0 {
		t.Fatal("calls after finalize")
	}
}

// TestReconcileListFailure: a failed Docker list skips the round's Docker
// work, is logged and recorded once while it repeats, and starts no drain.
func TestReconcileListFailure(t *testing.T) {
	h := newHarness(t, testPool(2, 0))
	seed(t, h.table, "pool-a", 1, StateBusy, testEpoch)
	h.docker.listErr = func() error { return errTransient }
	r, _ := newTestReconciler(t, h)
	r.round()
	r.round()
	if h.gone("pool-a") || h.errs.count() != 1 || h.fatalCount() != 0 {
		t.Fatalf("gone %v errors %d; want kept and 1", h.gone("pool-a"), h.errs.count())
	}
}

// TestReconcileNewRunnerNotLeftover: a runner whose row and containers
// appear after the round's table snapshot is not removed as a leftover.
func TestReconcileNewRunnerNotLeftover(t *testing.T) {
	h := newHarness(t, testPool(2, 0))
	r, _ := newTestReconciler(t, h)
	h.docker.beforeList = func() {
		seed(t, h.table, "pool-new", 9, StateIdle, testEpoch)
		h.docker.addRunner("pool-new", "running", "running")
	}
	r.round()
	if h.gone("pool-new") || len(h.docker.removedNames()) != 0 {
		t.Fatal("new runner removed as a leftover")
	}
}

func TestErrorOnce(t *testing.T) {
	h := newHarness(t, testPool(1, 0))
	var e errorOnce
	log := h.s.log
	e.report(log, h.errs, "w", "m", retry.Permanent, errPermanent)
	e.report(log, h.errs, "w", "m", retry.Permanent, errPermanent)
	e.reset(log, "ok again")
	e.report(log, h.errs, "w", "m", retry.Permanent, errPermanent)
	if h.errs.count() != 2 {
		t.Fatalf("recorded %d, want 2", h.errs.count())
	}
	if !errors.Is(h.errs.last().err, errPermanent) {
		t.Fatal("wrong error recorded")
	}
}
