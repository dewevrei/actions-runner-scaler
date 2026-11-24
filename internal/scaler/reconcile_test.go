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
	r, err := NewReconciler(ReconcilerConfig{Scaler: h.s, Resources: h.docker, Remover: h.docker, Processes: h.docker, Runners: repo})
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

// waitListing waits for the R103 goroutine only.
func (r *Reconciler) waitListing(ctx context.Context) error {
	return r.lists.wait(ctx)
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
	if err == nil || !strings.Contains(err.Error(), "Scaler") || !strings.Contains(err.Error(), "Remover") ||
		!strings.Contains(err.Error(), "Runners") {
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
// removed one by one by ID: a lone runner container is force-removed,
// unpaired BuildKit containers and networks go too, and a runner's
// containers go before its network; a row's resources stay.
func TestReconcileLeftovers(t *testing.T) {
	h := newHarness(t, testPool(2, 0))
	seed(t, h.table, "pool-a", 1, StateIdle, testEpoch)
	h.docker.addRunner("pool-a", "running", "running")
	h.docker.addResource(Resource{Kind: ResourceRunner, Name: "pool-0000aaaa", RunnerName: "pool-0000aaaa", State: "running"})
	h.docker.addResource(Resource{Kind: ResourceBuildKit, Name: "pool-0000bbbb-buildkit", RunnerName: "pool-0000bbbb", State: "running"})
	h.docker.addResource(Resource{Kind: ResourceNetwork, Name: "pool-0000cccc-net", RunnerName: "pool-0000cccc"})
	h.docker.addRunner("pool-0000dddd", "exited", "running")
	r, _ := newTestReconciler(t, h)
	r.round()
	want := []string{"pool-0000aaaa", "pool-0000bbbb-buildkit", "pool-0000cccc-net",
		"pool-0000dddd", "pool-0000dddd-buildkit", "pool-0000dddd-net"}
	if got := h.docker.idRemovedNames(); !slices.Equal(got, want) {
		t.Fatalf("removed %v, want %v", got, want)
	}
	for _, c := range h.docker.idRemovalCalls() {
		if c.kind != c.res.Kind {
			t.Errorf("%s removed as %q, want %q", c.res.Name, c.kind, c.res.Kind)
		}
	}
	if n := len(h.docker.removedNames()); n != 0 {
		t.Fatalf("RemoveResources calls %d for leftovers, want 0 (removal is by ID)", n)
	}
	if h.gone("pool-a") || !h.docker.has("pool-a", ResourceRunner) {
		t.Fatal("paired runner touched")
	}
	if n := len(h.gh.removedIDs()); n != 0 {
		t.Fatalf("RemoveRunner calls %d for leftovers, want 0 (registration is R17's)", n)
	}
	r.round()
	if n := len(h.docker.idRemovedNames()); n != len(want) {
		t.Fatalf("removals %d after a second round, want %d", n, len(want))
	}
}

// TestReconcileLeftoversOtherPools: the reconciler of pool "pool" leaves the
// resources of other pools, names it never gives and resources without a
// runner-name label alone, and still removes its own.
func TestReconcileLeftoversOtherPools(t *testing.T) {
	h := newHarness(t, testPool(2, 0))
	for _, name := range []string{
		"other-0000aaaa",      // another pool
		"pool-x-0000aaaa",     // a pool whose name starts with "pool-"
		"pool-0000aaa",        // suffix too short
		"pool-0000aaaaa",      // suffix too long
		"pool-0000AAAA",       // upper-case hex
		"pool-0000aaag",       // not hex
		"pool",                // no suffix
		"xpool-0000aaaa",      // prefix not at the start
		"pool-0000aaaa-other", // trailing text
	} {
		h.docker.addRunner(name, "running", "running")
	}
	h.docker.addResource(Resource{Kind: ResourceRunner, Name: "unlabelled", State: "running"})
	h.docker.addRunner("pool-0123cdef", "running", "running")
	r, _ := newTestReconciler(t, h)
	r.round()
	want := []string{"pool-0123cdef", "pool-0123cdef-buildkit", "pool-0123cdef-net"}
	if got := h.docker.idRemovedNames(); !slices.Equal(got, want) {
		t.Fatalf("removed %v, want only %v", got, want)
	}
}

// TestReconcileLeftoverPermanentNotRetried: a leftover whose removal failed
// with a permanent error is not tried again while it is listed (R42); a new
// resource of that runner is.
func TestReconcileLeftoverPermanentNotRetried(t *testing.T) {
	h := newHarness(t, testPool(2, 0))
	h.docker.addResource(Resource{Kind: ResourceBuildKit, Name: "pool-0000bbbb-buildkit", RunnerName: "pool-0000bbbb", State: "running"})
	h.docker.setRemoveIDErr(func(Resource) error { return statusError(400) })
	r, _ := newTestReconciler(t, h)
	r.round()
	h.clock.Advance(time.Hour)
	r.round()
	r.round()
	if n := len(h.docker.idRemovedNames()); n != 1 {
		t.Fatalf("removals %d, want 1", n)
	}
	if h.errs.count() != 1 || h.fatalCount() != 0 {
		t.Fatalf("errors %d fatal %d; want 1 and 0", h.errs.count(), h.fatalCount())
	}
	h.docker.addResource(Resource{Kind: ResourceRunner, Name: "pool-0000bbbb", RunnerName: "pool-0000bbbb", State: "exited"})
	r.round()
	if n := len(h.docker.idRemovedNames()); n != 2 {
		t.Fatalf("removals %d after a new resource appeared, want 2", n)
	}
}

// TestReconcileLeftoverPermanentOnlyThatID: when one of a runner's
// containers fails permanently and the other transiently, only the first is
// remembered: once the second's retries are used up, the next round tries
// it again and not the first (R42). The network waits while either
// container is left.
func TestReconcileLeftoverPermanentOnlyThatID(t *testing.T) {
	h := newHarness(t, testPool(2, 0))
	h.docker.addRunner("pool-0000dddd", "exited", "exited")
	h.docker.setRemoveIDErr(func(r Resource) error {
		switch r.Kind {
		case ResourceRunner:
			return statusError(400)
		case ResourceBuildKit:
			return errTransient
		}
		return nil
	})
	r, _ := newTestReconciler(t, h)
	tries := func(name string) int { return count(h.docker.idRemovedNames(), name) }
	r.round()
	// The BuildKit container's first attempt and its maxRemoveRetries
	// retries.
	h.clock.Advance(24 * time.Hour)
	if tries("pool-0000dddd") != 1 || tries("pool-0000dddd-buildkit") != 1+maxRemoveRetries {
		t.Fatalf("tries: runner %d BuildKit %d; want 1 and %d",
			tries("pool-0000dddd"), tries("pool-0000dddd-buildkit"), 1+maxRemoveRetries)
	}
	r.round()
	if tries("pool-0000dddd") != 1 {
		t.Fatal("permanently failed runner container tried again")
	}
	if tries("pool-0000dddd-buildkit") != 2+maxRemoveRetries {
		t.Fatal("BuildKit container whose retries ran out not tried again; only permanent failures are remembered")
	}
	if tries("pool-0000dddd-net") != 0 || !h.docker.has("pool-0000dddd", ResourceNetwork) {
		t.Fatal("network tried while a container of its runner is left")
	}
}

// TestReconcileLeftoverNetworkAfterContainers: a network whose runner
// container is still being removed is not tried in that round (Docker
// refuses a network with a container attached); once the container is gone,
// the next round removes it, once.
func TestReconcileLeftoverNetworkAfterContainers(t *testing.T) {
	h := newHarness(t, testPool(2, 0))
	h.docker.addRunner("pool-0000dddd", "exited", "")
	fail := failTimes(1, errTransient)
	h.docker.setRemoveIDErr(func(r Resource) error {
		if r.Kind == ResourceRunner {
			return fail(r.Name)
		}
		return nil
	})
	r, _ := newTestReconciler(t, h)
	r.round()
	if got := h.docker.idRemovedNames(); !slices.Equal(got, []string{"pool-0000dddd"}) {
		t.Fatalf("removals %v, want only the runner container while it is being removed", got)
	}
	h.clock.Advance(time.Second) // the container's retry succeeds
	if h.docker.has("pool-0000dddd", ResourceRunner) {
		t.Fatal("runner container not removed by its retry")
	}
	r.round()
	r.round()
	want := []string{"pool-0000dddd", "pool-0000dddd", "pool-0000dddd-net"}
	if got := h.docker.idRemovedNames(); !slices.Equal(got, want) || h.docker.has("pool-0000dddd", ResourceNetwork) {
		t.Fatalf("removals %v, want %v and the network gone", got, want)
	}
}

// TestReconcileLeftoverNetworkAfterPermanentContainer: a network waits while
// its runner container is kept after a permanent failure, and is removed
// once that container is no longer listed.
func TestReconcileLeftoverNetworkAfterPermanentContainer(t *testing.T) {
	h := newHarness(t, testPool(2, 0))
	h.docker.addRunner("pool-0000dddd", "exited", "")
	h.docker.setRemoveIDErr(func(r Resource) error {
		if r.Kind == ResourceRunner {
			return statusError(400)
		}
		return nil
	})
	r, _ := newTestReconciler(t, h)
	r.round()
	r.round()
	if got := h.docker.idRemovedNames(); !slices.Equal(got, []string{"pool-0000dddd"}) {
		t.Fatalf("removals %v, want the runner container once and no network", got)
	}
	h.docker.drop("pool-0000dddd", ResourceRunner) // removed some other way
	r.round()
	if h.docker.has("pool-0000dddd", ResourceNetwork) {
		t.Fatal("network kept after its container was gone")
	}
}

// TestReconcileLeftoverTransientRetried: a transient failure is retried on
// the removal backoff, and rounds meanwhile start no second removal.
func TestReconcileLeftoverTransientRetried(t *testing.T) {
	h := newHarness(t, testPool(2, 0))
	h.docker.addResource(Resource{Kind: ResourceNetwork, Name: "pool-0000cccc-net", RunnerName: "pool-0000cccc"})
	fail := failTimes(1, errTransient)
	h.docker.setRemoveIDErr(func(r Resource) error { return fail(r.Name) })
	r, _ := newTestReconciler(t, h)
	r.round()
	r.round()
	if n := len(h.docker.idRemovedNames()); n != 1 {
		t.Fatalf("removals %d while a retry is pending, want 1", n)
	}
	h.clock.Advance(time.Second)
	if n := len(h.docker.idRemovedNames()); n != 2 || h.docker.has("pool-0000cccc", ResourceNetwork) {
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

// TestReconcileR67States: R67 removes for a BuildKit container that is gone
// or has ended (exited, dead) only; one that is created, paused,
// restarting or being removed may still run, so the runner stays.
func TestReconcileR67States(t *testing.T) {
	for state, remove := range map[string]bool{
		"exited":     true,
		"dead":       true,
		"running":    false,
		"created":    false,
		"paused":     false,
		"restarting": false,
		"removing":   false,
	} {
		t.Run(state, func(t *testing.T) {
			h := newHarness(t, testPool(2, 0))
			seed(t, h.table, "pool-a", 1, StateIdle, testEpoch)
			h.docker.addRunner("pool-a", "running", state)
			r, _ := newTestReconciler(t, h)
			r.round()
			if removed := h.gone("pool-a"); removed != remove {
				t.Fatalf("BuildKit %s: runner removed %v, want %v", state, removed, remove)
			}
			if called := slices.Contains(h.gh.removedIDs(), 1); called != remove {
				t.Fatalf("BuildKit %s: RemoveRunner called %v, want %v", state, called, remove)
			}
		})
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

// TestReconcileR83ProcessFailure: a process list that keeps failing is
// logged at Warn and recorded once per runner; a different error, or the
// same one after a success, is again. Nothing is marked busy and no drain
// starts.
func TestReconcileR83ProcessFailure(t *testing.T) {
	h := newHarness(t, testPool(4, 0))
	for _, name := range []string{"pool-a", "pool-b"} {
		seed(t, h.table, name, 1, StateIdle, testEpoch)
		h.docker.addRunner(name, "running", "running")
	}
	var mu sync.Mutex
	procErr := errTransient
	h.docker.procErr = func(string) error {
		mu.Lock()
		defer mu.Unlock()
		return procErr
	}
	setProcErr := func(err error) {
		mu.Lock()
		defer mu.Unlock()
		procErr = err
	}
	warnings := func() int {
		return strings.Count(h.logs.String(), `"level":"WARN","msg":"listing runner processes failed`)
	}
	r, _ := newTestReconciler(t, h)
	for range 3 {
		r.round()
	}
	if h.docker.procCalls() != 6 {
		t.Fatalf("process lists %d, want 6 (each runner each round)", h.docker.procCalls())
	}
	if warnings() != 2 || h.errs.count() != 2 || h.errs.last().where != "reconcile list runner processes" {
		t.Fatalf("warnings %d recorded %d (%q); want 2 and 2, one per runner",
			warnings(), h.errs.count(), h.errs.last().where)
	}
	setProcErr(errPermanent)
	r.round()
	if warnings() != 4 || h.errs.count() != 4 {
		t.Fatalf("warnings %d recorded %d after a different error; want 4 and 4", warnings(), h.errs.count())
	}
	setProcErr(nil)
	r.round()
	setProcErr(errPermanent)
	r.round()
	if warnings() != 6 || h.errs.count() != 6 {
		t.Fatalf("warnings %d recorded %d after a success and the error again; want 6 and 6", warnings(), h.errs.count())
	}
	if h.state("pool-a") != StateIdle || h.state("pool-b") != StateIdle || h.fatalCount() != 0 || h.table.Drain() != DrainNone {
		t.Fatal("failed process list changed state or started a drain (R98)")
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

// TestReconcileR103Deadline: R103's whole list runs under one 20 s
// deadline (repoRunnersTimeout, PLAN M3-8).
func TestReconcileR103Deadline(t *testing.T) {
	h := newHarness(t, testPool(2, 0))
	seed(t, h.table, "pool-a", 1, StateIdle, testEpoch)
	h.docker.addRunner("pool-a", "running", "running")
	r, repo := newTestReconciler(t, h)
	var (
		left time.Duration
		has  bool
	)
	repo.during = func(ctx context.Context) {
		var d time.Time
		d, has = ctx.Deadline()
		left = time.Until(d)
	}
	roundAndList(t, r)
	if repo.callCount() != 1 {
		t.Fatalf("lists %d, want 1", repo.callCount())
	}
	// The deadline is on the system clock; the lower bound only allows for
	// a slow test run.
	if !has || left > repoRunnersTimeout || left < repoRunnersTimeout-5*time.Second {
		t.Fatalf("list deadline set %v with %v left, want within %v", has, left, repoRunnersTimeout)
	}
}

// TestReconcileR103EndsAtFinalizeDeadline: a list in flight when finalize
// begins ends at the finalize deadline, measured on the injected clock, and
// its end is not recorded as an error.
func TestReconcileR103EndsAtFinalizeDeadline(t *testing.T) {
	h := newHarness(t, testPool(2, 0))
	seed(t, h.table, "pool-a", 1, StateIdle, testEpoch)
	h.docker.addRunner("pool-a", "running", "running")
	r, repo := newTestReconciler(t, h)
	started := make(chan struct{})
	repo.during = func(ctx context.Context) {
		close(started)
		<-ctx.Done()
	}
	r.round()
	waitDone(t, started, "R103 list")
	h.finalize() // 45 s budget
	h.clock.Advance(45*time.Second - time.Millisecond)
	if !r.listing.Load() {
		t.Fatal("list ended before the finalize deadline")
	}
	h.clock.Advance(time.Millisecond)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := r.waitListing(ctx); err != nil {
		t.Fatalf("list still running after the finalize deadline: %v", err)
	}
	if h.errs.count() != 0 {
		t.Fatalf("recorded %d errors (%q), want 0", h.errs.count(), h.errs.last().where)
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

// TestReconcileFinalize: once finalize began, rounds stop and a round run
// anyway makes no call; Wait returns. A list in flight is
// TestReconcileR103EndsAtFinalizeDeadline.
func TestReconcileFinalize(t *testing.T) {
	h := newHarness(t, testPool(2, 0))
	seed(t, h.table, "pool-a", 1, StateIdle, testEpoch)
	h.docker.addRunner("pool-a", "running", "exited")
	h.docker.addResource(Resource{Kind: ResourceNetwork, Name: "pool-0000cccc-net", RunnerName: "pool-0000cccc"})
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
		seed(t, h.table, "pool-0000eeee", 9, StateIdle, testEpoch)
		h.docker.addRunner("pool-0000eeee", "running", "running")
	}
	r.round()
	if h.gone("pool-0000eeee") || len(h.docker.idRemovedNames()) != 0 || len(h.docker.removedNames()) != 0 {
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
