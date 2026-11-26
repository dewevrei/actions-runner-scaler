package scaler

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/dewevrei/actions-runner-scaler/internal/retry"
)

func TestStartRunnerName(t *testing.T) {
	h := newHarness(t, testTarget(2, 0))
	h.mustScale(statsMsg(1, 2))
	names := h.docker.startedNames()
	re := regexp.MustCompile(`^pool-[0-9a-f]{8}$`)
	if len(names) != 2 || names[0] == names[1] {
		t.Fatalf("names %v, want two distinct", names)
	}
	for _, n := range names {
		if !re.MatchString(n) {
			t.Fatalf("name %q does not match <pool>-<8 hex>", n)
		}
	}
	if got := h.gh.jitNames; !slices.Equal(got, names) {
		t.Fatalf("JIT names %v, container names %v", got, names)
	}
	for i, n := range names {
		info, _ := h.table.Get(n)
		if info.ID != int64(100+i) || info.State != StateIdle {
			t.Fatalf("row %+v, want id %d idle", info, 100+i)
		}
	}
}

func TestStartRunnerSuccessPath(t *testing.T) {
	h := newHarness(t, testTarget(1, 0))
	var watched []string
	h.s.watcher = watcherFunc(func(name string) { watched = append(watched, name) })
	var stateDuringStart State
	h.docker.duringStart = func(_ context.Context, name string) { stateDuringStart = h.state(name) }
	h.mustScale(statsMsg(1, 1))

	name := h.docker.startedNames()[0]
	if stateDuringStart != StateStarting {
		t.Fatalf("state during container start %v, want starting", stateDuringStart)
	}
	if !slices.Equal(watched, []string{name}) {
		t.Fatalf("watched %v, want %s (M3-8)", watched, name)
	}
	// The stdin lines are the JIT config and a token the table accepts.
	lines := strings.Split(h.docker.stdin[0], "\n")
	if lines[0] != "jit-secret-"+name || len(lines[1]) != 43 {
		t.Fatal("stdin does not carry the JIT config and a 43-character token")
	}
	if v, ok := h.table.Authenticate(name, lines[1], testEpoch); !ok || v != VerdictPending {
		t.Fatal("table does not accept the token given to the container")
	}
	// The JIT config is registered for the start path only; the token
	// until the table revokes it (R31).
	if len(h.secrets.seen) != 2 {
		t.Fatalf("registered %d secrets, want JIT and token", len(h.secrets.seen))
	}
	if h.secrets.counts[lines[0]] != 0 || h.secrets.counts[lines[1]] != 1 {
		t.Fatal("want the JIT config unregistered and the token registered after the start path")
	}
	h.table.BeginExit(name)
	h.table.MarkRemoved(name)
	if h.secrets.live() != 0 {
		t.Fatal("token still registered after removal")
	}
	// R31: neither value appears in the logs.
	logs := h.logs.String()
	if strings.Contains(logs, lines[0]) || strings.Contains(logs, lines[1]) {
		t.Fatal("JIT config or token in logs")
	}
	if !strings.Contains(logs, "runner started") {
		t.Fatal("no start log")
	}
}

type watcherFunc func(string)

func (f watcherFunc) Watch(name string) { f(name) }

func TestStartJITFailureSkipsRemoveRunner(t *testing.T) {
	h := newHarness(t, testTarget(1, 0))
	h.gh.setJITErr(func(string) error { return errTransient })
	h.mustScale(statsMsg(1, 1))
	if n := len(h.gh.removedIDs()); n != 0 {
		t.Fatalf("RemoveRunner calls %d, want 0 after a JIT failure", n)
	}
	if n := len(h.docker.startedNames()) + len(h.docker.removedNames()); n != 0 {
		t.Fatalf("Docker calls %d, want 0", n)
	}
	if list := h.table.List(); len(list) != 0 {
		t.Fatalf("rows %+v, want none", list)
	}
	if h.table.Counts(testKey).All() != 0 {
		t.Fatal("failed start still counted")
	}
}

func TestStartFailureAfterJITCleansUp(t *testing.T) {
	for _, tt := range []struct {
		name string
		set  func(h *harness)
	}{
		{"token", func(h *harness) { h.tokens.err = errors.New("no randomness") }},
		{"network or BuildKit or runner container", func(h *harness) {
			h.docker.setStartErr(func(string) error { return errTransient })
		}},
		{"permanent container error", func(h *harness) {
			h.docker.setStartErr(func(string) error { return statusError(400) })
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t, testTarget(1, 0))
			tt.set(h)
			h.mustScale(statsMsg(1, 1))
			name := h.gh.jitNames[0]
			if got := h.docker.removedNames(); !slices.Equal(got, []string{name}) {
				t.Fatalf("resources removed %v, want [%s] (R18)", got, name)
			}
			if got := h.gh.removedIDs(); !slices.Equal(got, []int64{100}) {
				t.Fatalf("RemoveRunner ids %v, want [100] (R18)", got)
			}
			if !h.gone(name) || h.table.StartStatus(testKey).Failures != 1 {
				t.Fatal("failed start not removed and counted")
			}
			if h.secrets.live() != 0 {
				t.Fatal("secrets left registered")
			}
		})
	}
}

func TestStartJITRateLimited(t *testing.T) {
	h := newHarness(t, testTarget(1, 0))
	h.gh.setJITErr(func(string) error { return errRateLimited() })
	h.mustScale(statsMsg(1, 1))
	h.gh.setJITErr(nil)
	if h.gh.jitCalls() != 1 {
		t.Fatal("no JIT call")
	}
	for _, d := range []time.Duration{time.Second, 30 * time.Second, 28 * time.Second} {
		h.clock.Advance(d)
		h.mustScale(nil)
	}
	if n := h.gh.jitCalls(); n != 1 {
		t.Fatalf("JIT calls %d within 59s, want 1 (R52)", n)
	}
	if !strings.Contains(h.logs.String(), "JIT rate limit") {
		t.Fatal("no log for the rate-limited start (R87)")
	}
	h.clock.Advance(time.Second)
	h.mustScale(nil)
	if n := h.gh.jitCalls(); n != 2 {
		t.Fatalf("JIT calls %d at 60s, want 2", n)
	}
}

func TestStartFinalize(t *testing.T) {
	t.Run("before JIT", func(t *testing.T) {
		h := newHarness(t, testTarget(1, 0))
		h.finalize()
		h.scaleFinalizing(statsMsg(1, 1))
		if h.gh.jitCalls() != 0 {
			t.Fatal("JIT called while finalizing")
		}
	})
	t.Run("during JIT", func(t *testing.T) {
		h := newHarness(t, testTarget(1, 0))
		h.gh.duringJIT = func(string) { h.finalize() }
		h.scaleFinalizing(statsMsg(1, 1))
		if n := len(h.docker.startedNames()); n != 0 {
			t.Fatalf("StartRunner calls %d after finalize, want 0", n)
		}
		if got := h.gh.removedIDs(); !slices.Equal(got, []int64{100}) {
			t.Fatalf("RemoveRunner ids %v, want [100]", got)
		}
		if h.table.StartStatus(testKey).Failures != 0 {
			t.Fatal("finalize counted as a start failure")
		}
	})
	t.Run("during container start", func(t *testing.T) {
		h := newHarness(t, testTarget(2, 0))
		var watched int
		h.s.watcher = watcherFunc(func(string) { watched++ })
		h.docker.duringStart = func(context.Context, string) { h.finalize() }
		h.scaleFinalizing(statsMsg(1, 2))
		if watched != 0 {
			t.Fatal("watching a runner that started after finalize began")
		}
		name := h.gh.jitNames[0]
		if got := h.docker.startedNames(); !slices.Equal(got, []string{name}) {
			t.Fatalf("started %v, want only %s", got, name)
		}
		if got := h.docker.removedNames(); !slices.Equal(got, []string{name}) {
			t.Fatalf("removed %v, want the containers made after finalize began", got)
		}
		if !h.gone(name) || len(h.gh.removedIDs()) != 1 {
			t.Fatal("start path did not clean up its runner")
		}
		if h.gh.jitCalls() != 1 {
			t.Fatal("second start began while finalizing")
		}
	})
}

func TestStartRemovalBegunElsewhere(t *testing.T) {
	h := newHarness(t, testTarget(1, 0))
	// A deny path wins BeginExit while the containers are starting.
	h.docker.duringStart = func(_ context.Context, name string) { h.table.BeginExit(name) }
	var watched int
	h.s.watcher = watcherFunc(func(string) { watched++ })
	h.mustScale(statsMsg(1, 1))
	name := h.gh.jitNames[0]
	if watched != 0 {
		t.Fatal("watching a runner whose removal began")
	}
	if got := h.docker.removedNames(); !slices.Equal(got, []string{name}) {
		t.Fatalf("removed %v, want the start path's own cleanup", got)
	}
	if h.table.StartStatus(testKey).Failures != 0 {
		t.Fatal("aborted start counted as failure")
	}
}

func TestStartCleanupRemoveRunnerRateLimited(t *testing.T) {
	h := newHarness(t, testTarget(1, 0))
	h.docker.setStartErr(func(string) error { return errTransient })
	// RemoveRunner was rate limited just before.
	if !h.limits.Record(retry.OpRemoveRunner, errRateLimited()) {
		t.Fatal("rate limit not recorded")
	}
	h.mustScale(statsMsg(1, 1))
	if n := len(h.gh.removedIDs()); n != 0 {
		t.Fatalf("RemoveRunner calls %d before the allowed time, want 0", n)
	}
	if h.clock.pending() != 1 {
		t.Fatal("no deferred RemoveRunner")
	}
	h.clock.Advance(59 * time.Second)
	if n := len(h.gh.removedIDs()); n != 0 {
		t.Fatal("RemoveRunner ran early")
	}
	h.clock.Advance(time.Second)
	if got := h.gh.removedIDs(); !slices.Equal(got, []int64{100}) {
		t.Fatalf("RemoveRunner ids %v at the allowed time, want [100]", got)
	}

	t.Run("skipped after finalize", func(t *testing.T) {
		h := newHarness(t, testTarget(1, 0))
		h.docker.setStartErr(func(string) error { return errTransient })
		h.limits.Record(retry.OpRemoveRunner, errRateLimited())
		h.mustScale(statsMsg(1, 1))
		h.finalize()
		h.clock.Advance(time.Minute)
		if n := len(h.gh.removedIDs()); n != 0 {
			t.Fatalf("RemoveRunner calls %d after finalize, want 0", n)
		}
	})
}

func TestStartCleanupFailuresAreRecorded(t *testing.T) {
	h := newHarness(t, testTarget(1, 0))
	h.docker.setStartErr(func(string) error { return errTransient })
	h.docker.setRemoveErr(func(string) error { return statusError(400) })
	h.gh.setRemoveErr(func(int64) error { return errPermanent })
	h.mustScale(statsMsg(1, 1))
	name := h.gh.jitNames[0]
	if h.state(name) != StateExiting {
		t.Fatalf("state %v, want exiting while resources may remain", h.state(name))
	}
	// RemoveRunner follows the resource removal even when it failed (R18).
	if got := h.gh.removedIDs(); !slices.Equal(got, []int64{100}) {
		t.Fatalf("RemoveRunner ids %v, want [100]", got)
	}
	if h.errs.count() != 2 {
		t.Fatalf("recorded %d errors, want 2", h.errs.count())
	}
	if h.fatalCount() != 0 {
		t.Fatal("cleanup failure reported fatal (R98)")
	}
	// Permanent failures are not retried.
	h.clock.Advance(time.Hour)
	if n := len(h.docker.removedNames()); n != 1 || h.clock.pending() != 0 {
		t.Fatalf("RemoveResources calls %d, pending timers %d; want 1 and 0", n, h.clock.pending())
	}
}

func TestWaitInFlight(t *testing.T) {
	h := newHarness(t, testTarget(1, 0))
	inStart := make(chan struct{})
	release := make(chan struct{})
	h.docker.duringStart = func(context.Context, string) {
		close(inStart)
		<-release
	}
	scaled := make(chan error, 1)
	go func() { scaled <- h.scale(statsMsg(1, 1)) }()
	<-inStart

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := h.s.WaitInFlight(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("WaitInFlight with a start in flight = %v, want canceled", err)
	}
	waited := make(chan error, 1)
	go func() { waited <- h.s.WaitInFlight(context.Background()) }()
	close(release)
	if err := <-waited; err != nil {
		t.Fatal(err)
	}
	if err := <-scaled; err != nil {
		t.Fatal(err)
	}
	if err := h.s.WaitInFlight(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestJITConfigHidden(t *testing.T) {
	c := NewJITConfig("secret-jit")
	type holder struct{ c JITConfig }
	for _, s := range []string{
		fmt.Sprint(c), fmt.Sprintf("%v %s %#v %+v", c, c, c, c),
		fmt.Sprintf("%+v %#v", holder{c}, holder{c}), slog.AnyValue(c).Resolve().String(),
	} {
		if strings.Contains(s, "secret-jit") {
			t.Fatalf("JIT config printed: %s", s)
		}
	}
	if c.Reveal() != "secret-jit" || (JITConfig{}).Reveal() != "" {
		t.Fatal("Reveal")
	}
}

// TestStartFailureRedacted: the stored start error hides the JIT config and
// the token, which are still registered when it is stored (R31).
func TestStartFailureRedacted(t *testing.T) {
	h := newHarness(t, testTarget(1, 0))
	var jit, token string
	h.docker.setStartErr(func(name string) error {
		lines := strings.Split(h.docker.stdin[0], "\n")
		jit, token = lines[0], lines[1]
		return fmt.Errorf("attach failed writing %q and %q", jit, token)
	})
	h.mustScale(statsMsg(1, 1))
	st := h.table.StartStatus(testKey)
	if st.LastError == nil {
		t.Fatal("no last error")
	}
	msg := st.LastError.Error()
	if jit == "" || token == "" || strings.Contains(msg, jit) || strings.Contains(msg, token) {
		t.Fatalf("last error %q shows a secret", msg)
	}
	if !strings.Contains(msg, "<redacted>") || !strings.Contains(msg, string(retry.OpContainerStart)) {
		t.Fatalf("last error %q, want the call and redacted values", msg)
	}
	if h.secrets.live() != 0 {
		t.Fatal("secrets left registered after the start path")
	}
}

// TestStartCleanupLostBeginExit: when another caller (the deny path) won
// BeginExit, the start path still removes what it made, but a failure does
// not mark the row abandoned, so reconciliation cannot claim it while its
// owner works on it.
func TestStartCleanupLostBeginExit(t *testing.T) {
	for _, tt := range []struct {
		name string
		err  error
	}{
		{"permanent", statusError(400)},
		{"transient until given up", errTransient},
	} {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t, testTarget(1, 0))
			h.docker.duringStart = func(_ context.Context, name string) { h.table.BeginExit(name) }
			h.docker.setRemoveErr(func(string) error { return tt.err })
			h.mustScale(statsMsg(1, 1))
			h.clock.Advance(time.Hour)
			name := h.gh.jitNames[0]
			if n := len(h.docker.removedNames()); n == 0 {
				t.Fatal("start path did not remove what it made")
			}
			info, _ := h.table.Get(name)
			if info.State != StateExiting || info.Abandoned {
				t.Fatalf("row %+v, want exiting and not abandoned by the start path", info)
			}
			if h.table.ClaimAbandoned(name) {
				t.Fatal("row claimable while its owner removes it")
			}
		})
	}
}

func TestRunnerName(t *testing.T) {
	for name, want := range map[string]bool{
		"pool-0123abcd":     true,
		"pool-00000000":     true,
		"pool-0123abc":      false,
		"pool-0123abcde":    false,
		"pool-0123ABCD":     false,
		"pool-0123abcg":     false,
		"pool-x-0123abcd":   false,
		"other-0123abcd":    false,
		"xpool-0123abcd":    false,
		"pool0123abcd":      false,
		"pool-0123abcd-net": false,
		"pool-":             false,
		"":                  false,
		"pool.-0123abcd":    false,
		"pool-0123abcd\n":   false,
	} {
		if got := RunnerName("pool", name); got != want {
			t.Errorf("RunnerName(pool, %q) = %v, want %v", name, got, want)
		}
	}
	// The label is taken literally, not as a pattern.
	if RunnerName("p.ol", "pool-0123abcd") || !RunnerName("p.ol", "p.ol-0123abcd") {
		t.Error("label read as a pattern")
	}
	// Every name newName gives matches.
	h := newHarness(t, testTarget(1, 0))
	for range 100 {
		name, err := h.s.newName()
		if err != nil {
			t.Fatal(err)
		}
		if !RunnerName("pool", name) {
			t.Fatalf("newName gave %q, which does not match", name)
		}
	}
}

// TestRunnerCreationLookupBeforeJIT: the images are looked up before the
// JIT request, and their IDs go to StartRunner; an image being pulled makes
// no JIT request, no row and no container, and is no failure (0003 SPEC
// R319).
func TestRunnerCreationLookupBeforeJIT(t *testing.T) {
	h := newHarness(t, testTarget(1, 0))
	order := &callLog{}
	h.images.order = order
	h.gh.duringJIT = func(string) { order.add("jit") }
	h.docker.duringStart = func(context.Context, string) { order.add("start") }
	ids := ImageIDs{Runner: "sha256:r1", BuildKit: "sha256:b1"}
	h.images.set(ids, nil)
	h.mustScale(statsMsg(1, 1))
	if got := order.snapshot(); !slices.Equal(got, []string{"images", "jit", "start"}) {
		t.Fatalf("calls %v, want images, JIT, StartRunner", got)
	}
	if got := h.docker.images; !slices.Equal(got, []ImageIDs{ids}) {
		t.Fatalf("StartRunner images %v, want %v", got, ids)
	}

	p := newHarness(t, testTarget(1, 0))
	p.images.set(ImageIDs{}, func(int) error { return ErrImagePending })
	p.mustScale(statsMsg(1, 1))
	if p.images.callCount() == 0 {
		t.Fatal("no image lookup")
	}
	if n, rows := p.gh.jitCalls(), p.table.List(); n != 0 || len(rows) != 0 || len(p.docker.startedNames()) != 0 {
		t.Fatalf("JIT calls %d, rows %+v, containers %v; want none while the image is pulled", n, rows, p.docker.startedNames())
	}
	if st := p.table.StartStatus(testKey); st.Failures != 0 || st.LastError != nil {
		t.Fatalf("start status %+v, want no failure (0003 SPEC R319)", st)
	}
	// No row came and went, so nothing kicked a placement, which would
	// loop until the pull ends.
	if n := len(p.placer.kick); n != 0 {
		t.Fatalf("%d placement kicks pending, want none", n)
	}
}

// TestRunnerCreationImageFailure: a failed lookup and a missing image ID are
// counted start failures with their text and the start backoff, before any
// JIT request, network or container (0003 SPEC R319).
func TestRunnerCreationImageFailure(t *testing.T) {
	for _, tt := range []struct {
		name string
		err  error
		text string
	}{
		{"lookup 500", &retry.OpError{Op: retry.OpImageInspect, Err: statusError(500)}, string(retry.OpImageInspect)},
		{"missing ID", fmt.Errorf("image sha256:%s is not present locally", strings.Repeat("a", 64)), "is not present locally"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t, testTarget(1, 0))
			h.images.set(ImageIDs{}, func(int) error { return tt.err })
			for i := range startBackoffAfter {
				h.mustScale(statsMsg(i+1, 1))
			}
			if n := h.gh.jitCalls(); n != 0 {
				t.Fatalf("JIT calls %d, want 0", n)
			}
			if got := h.docker.startedNames(); len(got) != 0 || len(h.docker.removedNames()) != 0 {
				t.Fatalf("containers started %v, removed %v; want none", got, h.docker.removedNames())
			}
			if rows := h.table.List(); len(rows) != 0 {
				t.Fatalf("rows %+v, want none", rows)
			}
			st := h.table.StartStatus(testKey)
			if st.Failures != startBackoffAfter || st.LastError == nil || !strings.Contains(st.LastError.Error(), tt.text) {
				t.Fatalf("start status %+v, want %d failures with %q", st, startBackoffAfter, tt.text)
			}
			if !st.NextStartAt.After(h.clock.Now()) {
				t.Fatalf("next start %v, want the start backoff (R61)", st.NextStartAt)
			}
			if n := len(h.gh.removedIDs()); n != 0 {
				t.Fatalf("RemoveRunner calls %d, want 0: nothing was registered", n)
			}
		})
	}
}
