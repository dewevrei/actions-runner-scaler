package scaler

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dewevrei/actions-runner-scaler/internal/retry"
)

// runAsync runs f in a goroutine and returns its result channel.
func runAsync(f func() error) <-chan error {
	ch := make(chan error, 1)
	go func() { ch <- f() }()
	return ch
}

// waitErr waits for a result with a failsafe timeout.
func waitErr(t *testing.T, ch <-chan error) error {
	t.Helper()
	select {
	case err := <-ch:
		return err
	case <-time.After(10 * time.Second):
		t.Fatal("timed out")
		return nil
	}
}

// advanceUntil advances the fake clock by d each time until done reports
// true, waiting for a goroutine to register its timer first.
func advanceUntil(t *testing.T, c *fakeClock, d time.Duration, done func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !done() {
		if time.Now().After(deadline) {
			t.Fatal("timed out")
		}
		if c.pending() > 0 {
			c.Advance(d)
		}
		runtime.Gosched() // let the goroutine under test run
	}
}

func newResourceCleanup(t *testing.T, h *harness) *ResourceCleanup {
	t.Helper()
	c, err := NewResourceCleanup(ResourceCleanupConfig{
		Resources: h.docker, Remover: h.docker, Clock: h.clock,
		Logger: slog.New(slog.NewJSONHandler(h.logs, nil)), Jitter: func() float64 { return 0 },
	})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestNewCleanupRequiresDependencies(t *testing.T) {
	if _, err := NewResourceCleanup(ResourceCleanupConfig{}); err == nil {
		t.Fatal("NewResourceCleanup accepted nothing")
	}
	if _, err := NewRegistrationCleanup(RegistrationCleanupConfig{}); err == nil {
		t.Fatal("NewRegistrationCleanup accepted nothing")
	}
}

// TestResourceCleanupRemovesAll: every managed resource of an earlier run
// is removed by ID, whatever pool it belongs to and with or without a
// runner-name label: every container first, then every network (R16).
func TestResourceCleanupRemovesAll(t *testing.T) {
	h := newHarness(t, testPool(1, 0))
	h.docker.addRunner("pool-a", "running", "running")
	h.docker.addRunner("pool-b", "exited", "")
	h.docker.addResource(Resource{Kind: ResourceNetwork, Name: "pool-c-net", RunnerName: "pool-c"})
	h.docker.addRunner("other-0000aaaa", "running", "running")
	// No runner-name label; one without a kind label either.
	h.docker.addResource(Resource{Kind: ResourceBuildKit, Name: "stray-buildkit", State: "running"})
	h.docker.addResource(Resource{Name: "stray", State: "created"})
	h.docker.addResource(Resource{Name: "stray-net"})
	if err := newResourceCleanup(t, h).Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"other-0000aaaa", "other-0000aaaa-buildkit", "pool-a", "pool-a-buildkit", "pool-b", "stray", "stray-buildkit",
		"other-0000aaaa-net", "pool-a-net", "pool-b-net", "pool-c-net", "stray-net",
	}
	if got := h.docker.idRemovedNames(); !slices.Equal(got, want) {
		t.Fatalf("removed %v, want %v", got, want)
	}
	for _, c := range h.docker.idRemovalCalls() {
		wantKind := c.res.Kind
		if c.res.Name == "stray-net" {
			wantKind = ResourceNetwork // no kind label, no state: a network
		}
		if c.kind != wantKind {
			t.Errorf("%s removed as %q, want %q", c.res.Name, c.kind, wantKind)
		}
	}
	if res := h.docker.resources(); len(res) != 0 {
		t.Fatalf("left %v", res)
	}
	if n := len(h.docker.removedNames()); n != 0 {
		t.Fatalf("RemoveResources calls %d, want 0 (removal is by ID)", n)
	}
}

// TestResourceCleanupTargetFailure: one resource's failure is logged and
// the others are still removed; one already gone counts as removed.
func TestResourceCleanupTargetFailure(t *testing.T) {
	h := newHarness(t, testPool(1, 0))
	h.docker.addRunner("pool-a", "running", "running")
	h.docker.addRunner("pool-b", "running", "running")
	h.docker.setRemoveIDErr(func(r Resource) error {
		switch r.Name {
		case "pool-a":
			return statusError(500)
		case "pool-b-net":
			return statusError(404)
		}
		return nil
	})
	if err := newResourceCleanup(t, h).Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := len(h.docker.idRemovedNames()); n != 6 {
		t.Fatalf("removals %d, want all 6 tried", n)
	}
	if !h.docker.has("pool-a", ResourceRunner) || h.docker.has("pool-b", ResourceRunner) || h.docker.has("pool-a", ResourceBuildKit) {
		t.Fatalf("left %v; want only the failed runner container and pool-b's network", h.docker.resources())
	}
	if !strings.Contains(h.logs.String(), `"removed":5,"found":6`) {
		t.Fatalf("summary not logged with 5 of 6 removed: %s", h.logs.String())
	}
}

// TestResourceCleanupListPermanent: a permanent list error is logged and
// the start goes on (R16).
func TestResourceCleanupListPermanent(t *testing.T) {
	h := newHarness(t, testPool(1, 0))
	h.docker.listErr = func() error { return statusError(400) }
	if err := newResourceCleanup(t, h).Run(context.Background()); err != nil {
		t.Fatalf("Run = %v, want nil", err)
	}
	if h.docker.listCalls() != 1 || len(h.docker.idRemovedNames()) != 0 {
		t.Fatal("permanent list error retried or removed something")
	}
}

// TestResourceCleanupListTransient: transient list errors are retried on
// the 1 to 60 s backoff until the list works (R24).
func TestResourceCleanupListTransient(t *testing.T) {
	h := newHarness(t, testPool(1, 0))
	h.docker.addRunner("pool-a", "running", "running")
	var mu sync.Mutex
	fails := 5
	h.docker.listErr = func() error {
		mu.Lock()
		defer mu.Unlock()
		if fails > 0 {
			fails--
			return errTransient
		}
		return nil
	}
	start := h.clock.Now()
	done := runAsync(func() error { return newResourceCleanup(t, h).Run(context.Background()) })
	var result error
	finished := false
	advanceUntil(t, h.clock, time.Second, func() bool {
		select {
		case result = <-done:
			finished = true
		default:
		}
		return finished
	})
	if result != nil {
		t.Fatal(result)
	}
	// Waits of 1, 2, 4, 8 and 16 s: 31 s.
	if d := h.clock.Now().Sub(start); d < 31*time.Second {
		t.Fatalf("retried after %v, want the backoff (31 s)", d)
	}
	if h.docker.listCalls() != 6 || !slices.Equal(h.docker.idRemovedNames(), []string{"pool-a", "pool-a-buildkit", "pool-a-net"}) {
		t.Fatalf("lists %d removed %v", h.docker.listCalls(), h.docker.idRemovedNames())
	}
}

// TestResourceCleanupCanceled: a signal during the backoff ends Run with the
// context's error (R57).
func TestResourceCleanupCanceled(t *testing.T) {
	h := newHarness(t, testPool(1, 0))
	h.docker.listErr = func() error { return errTransient }
	ctx, cancel := context.WithCancel(context.Background())
	done := runAsync(func() error { return newResourceCleanup(t, h).Run(ctx) })
	cancel()
	if err := waitErr(t, done); !errors.Is(err, context.Canceled) {
		t.Fatalf("Run = %v, want canceled", err)
	}
}

// fakeRegistry is the GitHub side of R17.
type fakeRegistry struct {
	mu       sync.Mutex
	setID    int
	setFound bool
	setErr   func(call int) error
	setCalls []time.Time
	clock    Clock

	regs      map[string]RegisteredRunner
	lookupErr func(name string) error
	lookups   []string
	removeErr func(id int64) error
	removed   []int64

	listErr   func(call int) error
	listCalls []time.Time
	runners   []RepoRunner
}

func (f *fakeRegistry) FindScaleSet(ctx context.Context, name string) (int, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.setCalls = append(f.setCalls, f.clock.Now())
	if f.setErr != nil {
		if err := f.setErr(len(f.setCalls)); err != nil {
			return 0, false, err
		}
	}
	return f.setID, f.setFound, nil
}

func (f *fakeRegistry) ListRepoRunners(ctx context.Context) ([]RepoRunner, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.listCalls = append(f.listCalls, f.clock.Now())
	if f.listErr != nil {
		if err := f.listErr(len(f.listCalls)); err != nil {
			return nil, err
		}
	}
	return slices.Clone(f.runners), nil
}

func (f *fakeRegistry) GetRunnerByName(ctx context.Context, name string) (RegisteredRunner, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lookups = append(f.lookups, name)
	if f.lookupErr != nil {
		if err := f.lookupErr(name); err != nil {
			return RegisteredRunner{}, false, err
		}
	}
	r, ok := f.regs[name]
	return r, ok, nil
}

func (f *fakeRegistry) RemoveRunner(ctx context.Context, id int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.removed = append(f.removed, id)
	if f.removeErr != nil {
		return f.removeErr(id)
	}
	return nil
}

func (f *fakeRegistry) snapshot() (sets, lists []time.Time, lookups []string, removed []int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.setCalls), slices.Clone(f.listCalls), slices.Clone(f.lookups), slices.Clone(f.removed)
}

// Runner names of pool "pool" in the R17 tests.
const (
	regA = "pool-0000000a" // scale set 5, removable
	regB = "pool-0000000b" // another scale set
	regC = "pool-0000000c" // container alive
	regD = "pool-0000000d" // scale set 5, removable
	regF = "pool-0000000f" // no longer registered
)

// newRegistry has scale set 5 with runners regA to regF above and runners
// whose names "pool"'s runners never have: other-0000000e (another prefix),
// pool-x-0000000a (a pool whose name starts with "pool-") and pool-g (no
// hex suffix). All three are registered in scale set 5, so only the name
// keeps them.
func newRegistry(h *harness) *fakeRegistry {
	h.docker.addRunner(regC, "running", "running")
	h.docker.addRunner(regD, "exited", "")
	return &fakeRegistry{
		clock: h.clock, setID: 5, setFound: true,
		runners: []RepoRunner{
			{ID: 1, Name: regA}, {ID: 2, Name: regB}, {ID: 3, Name: regC},
			{ID: 4, Name: regD}, {ID: 5, Name: "other-0000000e"}, {ID: 6, Name: regF},
			{ID: 7, Name: "pool-x-0000000a"}, {ID: 8, Name: "pool-g"},
		},
		regs: map[string]RegisteredRunner{
			regA:              {ID: 1, Name: regA, ScaleSetID: 5},
			regB:              {ID: 2, Name: regB, ScaleSetID: 6},
			regC:              {ID: 3, Name: regC, ScaleSetID: 5},
			regD:              {ID: 4, Name: regD, ScaleSetID: 5},
			"other-0000000e":  {ID: 5, Name: "other-0000000e", ScaleSetID: 5},
			"pool-x-0000000a": {ID: 7, Name: "pool-x-0000000a", ScaleSetID: 5},
			"pool-g":          {ID: 8, Name: "pool-g", ScaleSetID: 5},
		},
	}
}

func newRegistrationCleanup(t *testing.T, h *harness, reg *fakeRegistry) *RegistrationCleanup {
	t.Helper()
	c, err := NewRegistrationCleanup(RegistrationCleanupConfig{
		ScaleSets: reg, Runners: reg, Registry: reg, Resources: h.docker,
		Limits: h.limits, Clock: h.clock, Logger: slog.New(slog.NewJSONHandler(h.logs, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// TestRegistrationCleanup: only registrations named as the pool's runners
// ("<pool>-" and 8 hex characters), with the pool's scale set ID and no
// live container are removed (R17).
func TestRegistrationCleanup(t *testing.T) {
	h := newHarness(t, testPool(1, 0))
	reg := newRegistry(h)
	reg.removeErr = func(id int64) error {
		if id == 4 {
			return statusError(404) // already gone counts as removed
		}
		return nil
	}
	if err := newRegistrationCleanup(t, h, reg).Run(context.Background(), "pool"); err != nil {
		t.Fatal(err)
	}
	_, _, lookups, removed := reg.snapshot()
	if !slices.Equal(removed, []int64{1, 4}) {
		t.Fatalf("removed %v, want [1 4]", removed)
	}
	if want := []string{regA, regB, regD, regF}; !slices.Equal(lookups, want) {
		t.Fatalf("looked up %v, want %v; other names and live runners need no lookup", lookups, want)
	}
}

// TestRegistrationCleanupNoScaleSet: with no scale set R17 lists nothing.
func TestRegistrationCleanupNoScaleSet(t *testing.T) {
	h := newHarness(t, testPool(1, 0))
	reg := newRegistry(h)
	reg.setFound = false
	if err := newRegistrationCleanup(t, h, reg).Run(context.Background(), "pool"); err != nil {
		t.Fatal(err)
	}
	if _, lists, _, removed := reg.snapshot(); len(lists) != 0 || len(removed) != 0 {
		t.Fatal("listed or removed without a scale set")
	}
}

// runRegistrationCleanup runs R17 in a goroutine, advancing the fake clock
// one second at a time until it returns.
func runRegistrationCleanup(t *testing.T, h *harness, reg *fakeRegistry) {
	t.Helper()
	c := newRegistrationCleanup(t, h, reg)
	done := runAsync(func() error { return c.Run(context.Background(), "pool") })
	var result error
	finished := false
	advanceUntil(t, h.clock, time.Second, func() bool {
		select {
		case result = <-done:
			finished = true
		default:
		}
		return finished
	})
	if result != nil {
		t.Fatal(result)
	}
}

func offsets(start time.Time, ts []time.Time) []time.Duration {
	out := make([]time.Duration, len(ts))
	for i, t := range ts {
		out[i] = t.Sub(start)
	}
	return out
}

// TestRegistrationCleanupScaleSetRetries: a transient scale set lookup is
// retried after 1, 2 and 4 s, then R17 is skipped.
func TestRegistrationCleanupScaleSetRetries(t *testing.T) {
	h := newHarness(t, testPool(1, 0))
	reg := newRegistry(h)
	reg.setErr = func(int) error { return errTransient }
	start := h.clock.Now()
	runRegistrationCleanup(t, h, reg)
	sets, lists, _, _ := reg.snapshot()
	want := []time.Duration{0, time.Second, 3 * time.Second, 7 * time.Second}
	if got := offsets(start, sets); !slices.Equal(got, want) {
		t.Fatalf("lookups at %v, want %v", got, want)
	}
	if len(lists) != 0 {
		t.Fatal("listed after the lookup failed")
	}
}

// TestRegistrationCleanupScaleSetSkips: a permanent error, several scale
// sets with the name, an authentication error and a rate limit skip R17 at
// once.
func TestRegistrationCleanupScaleSetSkips(t *testing.T) {
	for name, err := range map[string]error{
		"permanent": errPermanent,
		"ambiguous": fmt.Errorf("get scale set: %w", retry.ErrAmbiguousScaleSet),
		"auth":      fmt.Errorf("token: %w", retry.ErrAuth),
		"rate":      &retry.RateLimitError{RetryAfter: time.Minute},
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t, testPool(1, 0))
			reg := newRegistry(h)
			reg.setErr = func(int) error { return err }
			if err := newRegistrationCleanup(t, h, reg).Run(context.Background(), "pool"); err != nil {
				t.Fatal(err)
			}
			sets, lists, _, _ := reg.snapshot()
			if len(sets) != 1 || len(lists) != 0 {
				t.Fatalf("lookups %d lists %d; want 1 and 0", len(sets), len(lists))
			}
		})
	}
}

// TestRegistrationCleanupRateLimitedBefore: before the scale set calls'
// rate limit ends, R17 makes no call.
func TestRegistrationCleanupRateLimitedBefore(t *testing.T) {
	h := newHarness(t, testPool(1, 0))
	h.limits.Record(retry.OpGetRunnerScaleSet, &retry.RateLimitError{RetryAfter: time.Minute})
	reg := newRegistry(h)
	if err := newRegistrationCleanup(t, h, reg).Run(context.Background(), "pool"); err != nil {
		t.Fatal(err)
	}
	if sets, _, _, _ := reg.snapshot(); len(sets) != 0 {
		t.Fatal("scale set looked up during its rate limit")
	}
}

// TestRegistrationCleanupListRetries: a transient list error is retried
// after 1, 2 and 4 s, then R17 is skipped; a permanent error or a rate limit
// skips it at once.
func TestRegistrationCleanupListRetries(t *testing.T) {
	h := newHarness(t, testPool(1, 0))
	reg := newRegistry(h)
	reg.listErr = func(int) error { return statusError(502) }
	start := h.clock.Now()
	runRegistrationCleanup(t, h, reg)
	_, lists, _, removed := reg.snapshot()
	want := []time.Duration{0, time.Second, 3 * time.Second, 7 * time.Second}
	if got := offsets(start, lists); !slices.Equal(got, want) || len(removed) != 0 {
		t.Fatalf("lists at %v removed %v, want %v and none", got, removed, want)
	}

	for name, err := range map[string]error{"permanent": errPermanent, "rate": &retry.RateLimitError{}} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t, testPool(1, 0))
			reg := newRegistry(h)
			reg.listErr = func(int) error { return err }
			if err := newRegistrationCleanup(t, h, reg).Run(context.Background(), "pool"); err != nil {
				t.Fatal(err)
			}
			if _, lists, _, removed := reg.snapshot(); len(lists) != 1 || len(removed) != 0 {
				t.Fatalf("lists %d removed %v; want 1 and none", len(lists), removed)
			}
		})
	}

	t.Run("recovers", func(t *testing.T) {
		h := newHarness(t, testPool(1, 0))
		reg := newRegistry(h)
		reg.listErr = func(call int) error {
			if call < 3 {
				return errTransient
			}
			return nil
		}
		runRegistrationCleanup(t, h, reg)
		if _, _, _, removed := reg.snapshot(); !slices.Equal(removed, []int64{1, 4}) {
			t.Fatalf("removed %v after the list recovered", removed)
		}
	})
}

// TestRegistrationCleanupLookupFailure: a failed GetRunnerByName skips that
// runner only; a failed RemoveRunner too.
func TestRegistrationCleanupLookupFailure(t *testing.T) {
	h := newHarness(t, testPool(1, 0))
	reg := newRegistry(h)
	reg.lookupErr = func(name string) error {
		if name == regA {
			return errTransient
		}
		return nil
	}
	if err := newRegistrationCleanup(t, h, reg).Run(context.Background(), "pool"); err != nil {
		t.Fatal(err)
	}
	if _, _, _, removed := reg.snapshot(); !slices.Equal(removed, []int64{4}) {
		t.Fatalf("removed %v, want [4]", removed)
	}

	h = newHarness(t, testPool(1, 0))
	reg = newRegistry(h)
	reg.removeErr = func(id int64) error {
		if id == 1 {
			return errPermanent
		}
		return nil
	}
	if err := newRegistrationCleanup(t, h, reg).Run(context.Background(), "pool"); err != nil {
		t.Fatal(err)
	}
	if _, _, _, removed := reg.snapshot(); !slices.Equal(removed, []int64{1, 4}) {
		t.Fatalf("removed %v, want both tried", removed)
	}
}
