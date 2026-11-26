package daemon

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dewevrei/actions-runner-scaler/internal/retry"
	"github.com/dewevrei/actions-runner-scaler/internal/scaler"
	"github.com/dewevrei/actions-runner-scaler/internal/status"
)

// The image references of the resolver tests.
const (
	runnerRef = "actions-runner-scaler-runner:local"
	bkRef     = "moby/buildkit:latest"
)

// resolverEnv is an imageResolver on a fake Docker and a fake clock. Every
// end of a background pull sends on ended, the onPullDone the tests see.
type resolverEnv struct {
	r      *imageResolver
	docker *fakeDocker
	clock  *fakeClock
	logs   *syncBuffer
	cancel context.CancelFunc
	ended  chan struct{}
	kicks  atomic.Int32
}

func newResolverEnv(t *testing.T, runner, buildKit string) *resolverEnv {
	t.Helper()
	log, logs := capturedLog()
	ctx, cancel := context.WithCancel(context.Background())
	e := &resolverEnv{docker: newFakeDocker(&calls{}), clock: newFakeClock(), logs: logs, cancel: cancel,
		ended: make(chan struct{}, 64)}
	r, err := newImageResolver(imageResolverConfig{Docker: e.docker, Clock: e.clock, Logger: log,
		Jitter: func() float64 { return 0 }, Context: ctx, Runner: runner, BuildKit: buildKit})
	if err != nil {
		t.Fatal(err)
	}
	r.setOnPullDone(func() {
		e.kicks.Add(1)
		e.ended <- struct{}{}
	})
	e.r = r
	t.Cleanup(func() {
		cancel()
		wctx, wcancel := context.WithTimeout(context.Background(), failsafe)
		defer wcancel()
		if err := r.wait(wctx); err != nil {
			t.Errorf("pulls still running: %v", err)
		}
	})
	return e
}

// pullEnded waits for the end of one background pull.
func (e *resolverEnv) pullEnded(t *testing.T) {
	t.Helper()
	select {
	case <-e.ended:
	case <-time.After(failsafe):
		t.Fatal("no background pull ended")
	}
}

// imageLines returns the "image" lines of key as (id, pulled) pairs.
func imageLines(t *testing.T, logs *syncBuffer, key string) []string {
	t.Helper()
	var out []string
	for _, m := range logLines(t, logs, "image") {
		if m["key"] == key {
			out = append(out, fmt.Sprintf("%v %v", m["id"], m["pulled"]))
		}
	}
	return out
}

// TestImageLogLines proves 0003 SPEC R320 and R321: start-up logs "pulling
// image" before each pull and one "image" line per key; a runner creation
// logs an "image" line only when the ID changed; a background pull logs
// "pulling image" and an "image" line with pulled true, also when it brought
// back the same ID.
func TestImageLogLines(t *testing.T) {
	t.Run("start-up and runner creation", func(t *testing.T) {
		h := newHarness(t)
		h.cfg.Runner.Image = runnerRef
		h.docker.ids[runnerRef] = "sha256:i1"
		h.docker.missing[testBuildKitImage] = true
		h.docker.pullIDs[testBuildKitImage] = "sha256:b1"
		s := h.running()
		pulling := logLines(t, h.logs, "pulling image")
		if len(pulling) != 1 || pulling[0]["key"] != "buildkit.image" || pulling[0]["ref"] != testBuildKitImage {
			t.Fatalf("pulling image lines %v, want one for buildkit.image", pulling)
		}
		lines := logLines(t, h.logs, "image")
		if len(lines) != 2 {
			t.Fatalf("image lines %v, want one per key", lines)
		}
		for i, want := range []map[string]any{
			{"key": "runner.image", "ref": runnerRef, "id": "sha256:i1", "pulled": false},
			{"key": "buildkit.image", "ref": testBuildKitImage, "id": "sha256:b1", "pulled": true},
		} {
			for k, v := range want {
				if lines[i][k] != v {
					t.Errorf("image line %d: %s = %v, want %v", i, k, lines[i][k], v)
				}
			}
		}
		if strings.Index(h.logs.String(), `"msg":"pulling image"`) > strings.Index(h.logs.String(), `"pulled":true`) {
			t.Error("pulling image after the image line")
		}

		// A runner creation with the same IDs logs nothing.
		s.send(t, statsMsg(1, 1), nil)
		waitFor(t, "first runner", func() bool { return len(h.docker.startParams()) == 1 })
		if n := len(logLines(t, h.logs, "image")); n != 2 {
			t.Fatalf("%d image lines after an unchanged lookup, want 2", n)
		}
		// The tag moved: the next creation logs the new ID once.
		h.docker.setImage(runnerRef, "sha256:i2")
		s.send(t, statsMsg(2, 2), nil)
		waitFor(t, "second runner", func() bool { return len(h.docker.startParams()) == 2 })
		if got := imageLines(t, h.logs, "runner.image"); len(got) != 2 || got[1] != "sha256:i2 false" {
			t.Fatalf("runner.image lines %v, want the new ID once", got)
		}
		h.signal()
		if err := h.result(); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("background pull of the same ID", func(t *testing.T) {
		e := newResolverEnv(t, testRunnerImage, bkRef)
		e.docker.ids[bkRef] = "sha256:b1"
		if _, err := e.r.ResolveImages(context.Background()); err != nil {
			t.Fatal(err)
		}
		e.docker.setImage(bkRef, "")
		e.docker.pullIDs[bkRef] = "sha256:b1"
		if _, err := e.r.ResolveImages(context.Background()); !errors.Is(err, scaler.ErrImagePending) {
			t.Fatalf("ResolveImages = %v, want ErrImagePending", err)
		}
		e.pullEnded(t)
		if p := logLines(t, e.logs, "pulling image"); len(p) != 1 || p[0]["key"] != "buildkit.image" || p[0]["ref"] != bkRef {
			t.Fatalf("pulling image lines %v", p)
		}
		if got := imageLines(t, e.logs, "buildkit.image"); len(got) != 2 || got[1] != "sha256:b1 true" {
			t.Fatalf("buildkit.image lines %v, want the pulled line after the first", got)
		}
		ids, err := e.r.ResolveImages(context.Background())
		if err != nil || ids.BuildKit != "sha256:b1" {
			t.Fatalf("ResolveImages = %+v, %v", ids, err)
		}
		if got := imageLines(t, e.logs, "buildkit.image"); len(got) != 2 {
			t.Fatalf("buildkit.image lines %v after an unchanged lookup, want 2", got)
		}
	})
}

// TestRunnerCreationUsesCurrentID proves 0003 SPEC R319: the tag moves
// from I1 to I2 between two runner starts; the second runner is made from
// I2 and the first runner's containers are left alone.
func TestRunnerCreationUsesCurrentID(t *testing.T) {
	h := newHarness(t)
	h.cfg.Runner.Image = runnerRef
	h.docker.ids[runnerRef] = "sha256:i1"
	s := h.running()
	s.send(t, statsMsg(1, 1), nil)
	waitFor(t, "first runner", func() bool { return len(h.docker.startParams()) == 1 })
	h.docker.setImage(runnerRef, "sha256:i2")
	s.send(t, statsMsg(2, 2), nil)
	waitFor(t, "second runner", func() bool { return len(h.docker.startParams()) == 2 })
	p := h.docker.startParams()
	if p[0].RunnerImage != "sha256:i1" || p[1].RunnerImage != "sha256:i2" {
		t.Fatalf("runner images %q then %q, want sha256:i1 then sha256:i2", p[0].RunnerImage, p[1].RunnerImage)
	}
	if n := h.calls.count("docker.RemoveResources"); n != 0 {
		t.Fatalf("%d removals; the first runner's containers must stay", n)
	}
	if n, _ := h.docker.pullCount(runnerRef); n != 0 {
		t.Fatalf("%d pulls of a present reference", n)
	}
	h.signal()
	if err := h.result(); err != nil {
		t.Fatal(err)
	}
}

// TestBackgroundPullDoesNotBlockPlacement proves 0003 SPEC R319 and 0002
// SPEC R212: while a background pull blocks, the target that needs the image
// gets no runner and no counted failure, another target's Scale and its
// placement return, and no second pull of the reference starts; the end of
// the pull kicks a placement that starts the runners with the pulled ID.
func TestBackgroundPullDoesNotBlockPlacement(t *testing.T) {
	h := newHarness(t)
	fakes := h.useTargets("a", "b")
	h.cfg.Runner.Image = runnerRef
	h.docker.ids[runnerRef] = "sha256:old"
	h.start()
	sa, sb := fakes[0].polled(t, 0), fakes[1].polled(t, 0)
	trs := h.targetRuns()

	// The image went missing after start-up.
	began := make(chan string, 4)
	release := make(chan error)
	h.docker.setImage(runnerRef, "")
	h.docker.pullIDs[runnerRef] = "sha256:new"
	h.docker.setPull(blockingPull(began, release))

	sa.send(t, statsMsg(1, 1), nil)
	select {
	case <-began:
	case <-time.After(failsafe):
		t.Fatal("no background pull")
	}
	waitFor(t, "a's message acknowledged", func() bool { _, _, d, _ := sa.snapshot(); return len(d) == 1 })
	// b's Scale and its placement return while the pull blocks.
	sb.send(t, statsMsg(1, 1), nil)
	waitFor(t, "b's message acknowledged", func() bool { _, _, d, _ := sb.snapshot(); return len(d) == 1 })
	if n, most := h.docker.pullCount(runnerRef); n != 1 || most != 1 {
		t.Fatalf("pulls %d, at once %d; want 1 and 1", n, most)
	}
	if n := h.calls.count("a.GenerateJITConfig") + h.calls.count("b.GenerateJITConfig"); n != 0 {
		t.Fatalf("%d JIT requests while the image is pulled", n)
	}
	for _, tr := range trs {
		if st := h.d.table.StartStatus(tr.t.Key()); st.Failures != 0 {
			t.Fatalf("%s start status %+v, want no failure", tr.t.Key(), st)
		}
	}
	if len(h.d.table.List()) != 0 {
		t.Fatalf("rows %+v while the image is pulled", h.d.table.List())
	}

	// No new message: the pull's end kicks the placement.
	release <- nil
	waitFor(t, "both runners", func() bool { return len(h.docker.startParams()) == 2 })
	for _, p := range h.docker.startParams() {
		if p.RunnerImage != "sha256:new" {
			t.Fatalf("runner image %q, want the pulled ID", p.RunnerImage)
		}
	}
	if n, _ := h.docker.pullCount(runnerRef); n != 1 {
		t.Fatalf("pulls %d, want 1", n)
	}
	h.signal()
	if err := h.result(); err != nil {
		t.Fatal(err)
	}
}

// TestBackgroundPullFailureBackoff proves 0003 SPEC R319, R320 and 0001
// R24: a failed pull logs "image pull failed" and kicks one placement; until
// the backoff ends each start is a failure with the R320 text and no pull
// starts; the gap doubles from 1 s and stops at 60 s; a success resets it.
func TestBackgroundPullFailureBackoff(t *testing.T) {
	e := newResolverEnv(t, testRunnerImage, bkRef)
	e.docker.setImage(bkRef, "")
	var fail atomic.Bool
	fail.Store(true)
	e.docker.setPull(func(context.Context, string, int) error {
		if fail.Load() {
			return pullFailedErr(statusError(404))
		}
		return nil
	})
	ctx := context.Background()

	pullFails := func(t *testing.T) {
		t.Helper()
		if _, err := e.r.ResolveImages(ctx); !errors.Is(err, scaler.ErrImagePending) {
			t.Fatalf("ResolveImages = %v, want a new pull", err)
		}
		e.pullEnded(t)
	}
	pullFails(t)
	if n := len(logLines(t, e.logs, "image pull failed")); n != 1 {
		t.Fatalf("%d image pull failed lines, want 1", n)
	}
	if n := e.kicks.Load(); n != 1 {
		t.Fatalf("%d kicks, want 1", n)
	}
	for i, gap := range []time.Duration{1, 2, 4, 8, 16, 32, 60, 60} {
		gap *= time.Second
		e.clock.Advance(gap - time.Millisecond)
		_, err := e.r.ResolveImages(ctx)
		var oe *retry.OpError
		if err == nil || errors.Is(err, scaler.ErrImagePending) || !errors.As(err, &oe) || oe.Op != retry.OpImagePull ||
			!strings.Contains(err.Error(), "image "+bkRef+" is not present locally and pulling it failed: ") {
			t.Fatalf("failure %d, %v in: ResolveImages = %v, want the R320 failure", i+1, gap-time.Millisecond, err)
		}
		if n, _ := e.docker.pullCount(bkRef); n != i+1 {
			t.Fatalf("pulls %d within the backoff, want %d", n, i+1)
		}
		e.clock.Advance(time.Millisecond)
		pullFails(t)
	}
	if n := e.kicks.Load(); n != 9 {
		t.Fatalf("%d kicks, want one per pull (9)", n)
	}

	// A success resets the backoff: the next failure waits 1 s again.
	e.clock.Advance(time.Minute)
	fail.Store(false)
	if _, err := e.r.ResolveImages(ctx); !errors.Is(err, scaler.ErrImagePending) {
		t.Fatalf("ResolveImages = %v, want a new pull", err)
	}
	e.pullEnded(t)
	if ids, err := e.r.ResolveImages(ctx); err != nil || ids.BuildKit == "" {
		t.Fatalf("ResolveImages after the pull = %+v, %v", ids, err)
	}
	e.docker.setImage(bkRef, "")
	fail.Store(true)
	pullFails(t)
	e.clock.Advance(time.Second)
	pullFails(t)
}

// TestBackgroundPullStopsAtFinalize proves 0003 SPEC R319: canceling the
// server context ends a running pull, wait returns, and the pull's end
// records no failure and calls no onPullDone. In the daemon, finalize does
// both, and Run returns.
func TestBackgroundPullStopsAtFinalize(t *testing.T) {
	t.Run("resolver", func(t *testing.T) {
		e := newResolverEnv(t, testRunnerImage, bkRef)
		e.docker.setImage(bkRef, "")
		began := make(chan string, 1)
		e.docker.setPull(blockingPull(began, nil))
		if _, err := e.r.ResolveImages(context.Background()); !errors.Is(err, scaler.ErrImagePending) {
			t.Fatalf("ResolveImages = %v", err)
		}
		<-began
		e.cancel()
		ctx, cancel := context.WithTimeout(context.Background(), failsafe)
		defer cancel()
		if err := e.r.wait(ctx); err != nil {
			t.Fatalf("wait = %v", err)
		}
		if n := e.kicks.Load(); n != 0 {
			t.Fatalf("%d onPullDone calls after the cancel", n)
		}
		if n := len(logLines(t, e.logs, "image pull failed")); n != 0 {
			t.Fatalf("%d failures recorded after the cancel", n)
		}
		e.r.mu.Lock()
		st := *e.r.pulls[bkRef]
		e.r.mu.Unlock()
		if st.lastErr != nil || !st.next.IsZero() || st.running {
			t.Fatalf("pull state %+v, want nothing recorded", st)
		}
		// No pull starts once stopped.
		if _, err := e.r.ResolveImages(context.Background()); !errors.Is(err, scaler.ErrImagePending) {
			t.Fatalf("ResolveImages after the stop = %v", err)
		}
		if n, _ := e.docker.pullCount(bkRef); n != 1 {
			t.Fatalf("pulls %d, want 1", n)
		}
	})

	t.Run("daemon", func(t *testing.T) {
		h := newHarness(t)
		h.cfg.Runner.Image = runnerRef
		s := h.running()
		began := make(chan string, 1)
		h.docker.setImage(runnerRef, "")
		h.docker.setPull(blockingPull(began, nil))
		s.send(t, statsMsg(1, 1), nil)
		<-began
		h.signal()
		// Nothing is busy: the drain ends and finalize cancels the pull and
		// waits for it; the clock does not move, so no share runs out.
		waitFor(t, "Run returned", h.returned)
		if err := h.result(); err != nil {
			t.Fatalf("Run = %v", err)
		}
		if n := len(logLines(t, h.logs, "image pull failed")); n != 0 {
			t.Fatalf("%d failures recorded after finalize", n)
		}
		if strings.Contains(h.logs.String(), `"what":"image pulls"`) {
			t.Fatal("finalize gave up waiting for the pull")
		}
		if h.report().State == status.ErrorWait {
			t.Fatal("error wait")
		}
	})
}

// TestImageResolverConcurrent proves 0003 SPEC R319 and R321 under -race:
// 8 goroutines resolve while the pulls of both references end; at most one
// pull of a reference runs at a time, one pull per reference each time it
// goes missing, and each new ID gets one "image" line of each kind.
func TestImageResolverConcurrent(t *testing.T) {
	e := newResolverEnv(t, runnerRef, bkRef)
	const rounds, workers = 5, 8
	for round := 1; round <= rounds; round++ {
		for _, ref := range []string{runnerRef, bkRef} {
			e.docker.setImage(ref, "")
			e.docker.mu.Lock()
			e.docker.pullIDs[ref] = fmt.Sprintf("sha256:%s-%d", ref, round)
			e.docker.mu.Unlock()
		}
		want := scaler.ImageIDs{Runner: fmt.Sprintf("sha256:%s-%d", runnerRef, round),
			BuildKit: fmt.Sprintf("sha256:%s-%d", bkRef, round)}
		var wg sync.WaitGroup
		for range workers {
			wg.Go(func() {
				deadline := time.Now().Add(failsafe)
				for time.Now().Before(deadline) {
					ids, err := e.r.ResolveImages(context.Background())
					switch {
					case err == nil:
						if ids != want {
							t.Errorf("round %d: ids %+v, want %+v", round, ids, want)
						}
						return
					case !errors.Is(err, scaler.ErrImagePending):
						t.Errorf("round %d: ResolveImages = %v", round, err)
						return
					}
					runtime.Gosched()
				}
				t.Errorf("round %d: images never resolved", round)
			})
		}
		wg.Wait()
		for _, ref := range []string{runnerRef, bkRef} {
			if n, most := e.docker.pullCount(ref); n != round || most != 1 {
				t.Fatalf("round %d: %s pulled %d times, %d at once; want %d and 1", round, ref, n, most, round)
			}
		}
	}
	for _, key := range []string{"runner.image", "buildkit.image"} {
		seen := map[string]int{}
		for _, l := range imageLines(t, e.logs, key) {
			seen[l]++
		}
		for round := 1; round <= rounds; round++ {
			ref := map[string]string{"runner.image": runnerRef, "buildkit.image": bkRef}[key]
			id := fmt.Sprintf("sha256:%s-%d", ref, round)
			if seen[id+" true"] != 1 || seen[id+" false"] > 1 {
				t.Errorf("%s %s: lines %v, want one pulled line and at most one other", key, id, seen)
			}
		}
	}
}
