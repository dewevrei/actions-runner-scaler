package daemon

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/dewevrei/actions-runner-scaler/internal/github"
	"github.com/dewevrei/actions-runner-scaler/internal/retry"
	"github.com/dewevrei/actions-runner-scaler/internal/scaler"
	"github.com/dewevrei/actions-runner-scaler/internal/status"
)

// assertTargetsStopped checks that every target's goroutine returned by the
// time Run returned (0002 PLAN M12: no goroutine is left).
func (h *harness) assertTargetsStopped() {
	h.t.Helper()
	if h.d.runDone == nil {
		return
	}
	select {
	case <-h.d.runDone:
	case <-time.After(failsafe):
		h.t.Fatal("a target goroutine still runs after Run returned")
	}
}

// exit sends a signal, waits for Run's nil result and checks that every
// target goroutine stopped.
func (h *harness) exit() {
	h.t.Helper()
	h.signal()
	// Draining is set after the drain began, so no start phase goes on.
	h.waitState(status.Draining)
	for !h.returned() {
		h.clock.Advance(5 * time.Second)
		waitFor(h.t, "a drain poll or the end of Run", func() bool {
			return h.returned() || h.clock.hasTimerAt(h.clock.Now().Add(5*time.Second))
		})
	}
	if err := h.result(); err != nil {
		h.t.Fatalf("Run = %v, want nil", err)
	}
	h.assertTargetsStopped()
}

// gotRunner sends a message with one assigned job to sess and waits for
// gh's JIT call, the start of a runner of that target.
func (h *harness) gotRunner(gh *fakeGitHub, sess *fakeSession, id int) {
	h.t.Helper()
	before := h.calls.count(gh.prefix + ".GenerateJITConfig")
	sess.send(h.t, statsMsg(id, 1), nil)
	waitFor(h.t, gh.prefix+" runner start", func() bool {
		return h.calls.count(gh.prefix+".GenerateJITConfig") > before
	})
}

// TestThreeTargetsServed proves 0002 SPEC R105: three targets each get
// their own installation, clients, scale set, session and listener, and the
// status lists them in config order.
func TestThreeTargetsServed(t *testing.T) {
	h := newHarness(t)
	fakes := h.useTargets("a", "b", "c")
	for i := range h.cfg.Targets {
		h.cfg.Targets[i].MaxRunners = i + 1
	}
	h.start()
	h.waitState(status.Running)
	for i, f := range fakes {
		s := f.polled(t, 0)
		if n := h.calls.count(fmt.Sprintf("%s.CreateSession %d host", f.prefix, 7+i)); n != 1 {
			t.Errorf("%s: sessions on its scale set = %d, want 1", f.prefix, n)
		}
		if n := h.calls.count(fmt.Sprintf("github.Target %s 99", f.prefix)); n != 1 {
			t.Errorf("%s: clients made %d times, want 1", f.prefix, n)
		}
		if _, caps, _, _ := s.snapshot(); caps[0] != i+1 {
			t.Errorf("%s: maxCapacity %v, want %d", f.prefix, caps, i+1)
		}
	}
	r := h.report()
	var urls []string
	for _, tg := range r.Targets {
		urls = append(urls, tg.URL)
	}
	if want := []string{"https://github.com/acme/a", "https://github.com/acme/b", "https://github.com/acme/c"}; !slices.Equal(urls, want) {
		t.Errorf("status targets = %v, want %v", urls, want)
	}
	for _, tr := range h.targetRuns() {
		if s := tr.currentState(); s != targetRunning {
			t.Errorf("%s: state %s, want running", tr.t.Key(), s)
		}
	}
	h.exit()
}

// TestListenerMaxCapacityPerTarget proves 0002 SPEC R116: each target's
// listener sends that target's max-runners with every poll, also during a
// drain.
func TestListenerMaxCapacityPerTarget(t *testing.T) {
	h := newHarness(t)
	fakes := h.useTargets("a", "b")
	h.cfg.Targets[0].MaxRunners, h.cfg.Targets[1].MaxRunners = 2, 3
	h.start()
	sa, sb := fakes[0].polled(t, 0), fakes[1].polled(t, 0)
	trs := h.targetRuns()
	// A busy runner keeps the signal drain going.
	h.seedRunnerOf(trs[0], "pool-000000a1", 1, scaler.StateBusy)
	h.signal()
	h.waitState(status.Draining)
	for _, s := range []*fakeSession{sa, sb} {
		s.send(t, statsMsg(1, 0), nil)
		waitFor(t, s.name+" polled during the drain", func() bool { polls, _, _, _ := s.snapshot(); return polls >= 2 })
	}
	h.endRunner("pool-000000a1")
	for !h.returned() {
		h.clock.Advance(5 * time.Second)
		waitFor(t, "a drain poll", func() bool { return h.returned() || h.clock.hasTimerAt(h.clock.Now().Add(5*time.Second)) })
	}
	if err := h.result(); err != nil {
		t.Fatalf("Run = %v", err)
	}
	for _, c := range []struct {
		s    *fakeSession
		want int
	}{{sa, 2}, {sb, 3}} {
		_, caps, _, _ := c.s.snapshot()
		for _, got := range caps {
			if got != c.want {
				t.Fatalf("%s: maxCapacity %v, want %d throughout", c.s.name, caps, c.want)
			}
		}
	}
	h.assertTargetsStopped()
}

// TestRateLimitPerTarget proves 0002 SPEC R188: a rate limit on target A's
// JIT call does not delay target B's JIT call.
func TestRateLimitPerTarget(t *testing.T) {
	h := newHarness(t)
	fakes := h.useTargets("a", "b")
	fakes[0].jitErr = opErr(retry.OpGenerateJIT, &retry.RateLimitError{RetryAfter: time.Hour})
	h.start()
	sa, sb := fakes[0].polled(t, 0), fakes[1].polled(t, 0)
	trs := h.targetRuns()

	h.gotRunner(fakes[0], sa, 1)
	waitFor(t, "a's JIT rate limit", func() bool { return !trs[0].limits.Allowed(retry.OpGenerateJIT) })
	h.gotRunner(fakes[1], sb, 1)
	if !trs[1].limits.Allowed(retry.OpGenerateJIT) {
		t.Error("a's rate limit was recorded for b")
	}
	waitFor(t, "b's runner", func() bool { return h.d.table.Counts("acme/b").Live() == 1 })
	if n := h.d.table.Counts("acme/a").All(); n != 0 {
		t.Errorf("a has %d runners after its failed start", n)
	}
	h.exit()
}

// TestInstallationWaitShared proves 0002 SPEC R189: a rate-limited
// installation lookup of target A delays target B's lookup, since both use
// the App JWT.
func TestInstallationWaitShared(t *testing.T) {
	h := newHarness(t)
	fakes := h.useTargets("a", "b")
	const wait = 10 * time.Minute
	// a's rate limit answers only after b's first lookup was made, so b's
	// first call never waits for it.
	bLooked := make(chan struct{})
	fakes[0].instHook = func(ctx context.Context, n int) error {
		if n == 1 {
			select {
			case <-bLooked:
			case <-ctx.Done():
				return ctx.Err()
			}
			return opErr(retry.OpGetInstallation, &retry.RateLimitError{RetryAfter: wait})
		}
		return nil
	}
	fakes[1].instHook = func(_ context.Context, n int) error {
		if n == 1 {
			close(bLooked)
			return opErr(retry.OpGetInstallation, statusError(503))
		}
		return nil
	}
	h.start()
	waitFor(t, "both first lookups and a's rate limit", func() bool {
		return h.calls.count("github.Installation a") == 1 && h.calls.count("github.Installation b") == 1 &&
			!h.d.limits.Allowed(retry.OpGetInstallation)
	})
	// a waits for its rate limit; b's 1 s backoff ends, and its retry then
	// waits for a's rate limit too.
	h.clock.waitTimersAt(t, testEpoch.Add(wait), 1)
	h.clock.advanceTo(t, testEpoch.Add(time.Second))
	h.clock.waitTimersAt(t, testEpoch.Add(wait), 2)
	h.clock.Advance(wait - time.Second - time.Nanosecond)
	if n := h.calls.count("github.Installation b"); n != 1 {
		t.Fatalf("b looked up again before a's rate limit ended (%d lookups)", n)
	}
	h.clock.Advance(time.Nanosecond)
	fakes[0].polled(t, 0)
	fakes[1].polled(t, 0)
	h.exit()
}

// TestTargetStartupPermanentErrorOnlyThatTarget proves 0002 SPEC R237,
// R172 and R182: target A's missing installation (404) puts only A in the
// target error wait with the SPEC text as A's last error; B makes its
// session and gets runners.
func TestTargetStartupPermanentErrorOnlyThatTarget(t *testing.T) {
	tests := []struct {
		name  string
		setup func(h *harness, a *fakeGitHub)
		want  string
	}{
		{"not installed", func(h *harness, a *fakeGitHub) {
			a.instErr = func(int) error {
				return opErr(retry.OpGetInstallation, &github.StatusError{Code: 404, Message: "GitHub App is not installed on repository acme/a"})
			}
		}, "GitHub App is not installed on repository acme/a"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t)
			fakes := h.useTargets("a", "b")
			tt.setup(h, fakes[0])
			h.start()
			sb := fakes[1].polled(t, 0)
			trs := h.targetRuns()
			h.waitTargetState(trs[0], targetErrorWait)
			h.waitState(status.Running)
			if last, _ := trs[0].rec.Last(); !strings.Contains(last, tt.want) {
				t.Errorf("a's lastError = %q, want %q in it", last, tt.want)
			}
			if last, _ := trs[1].rec.Last(); last != "" {
				t.Errorf("b's lastError = %q, want none", last)
			}
			if s := trs[1].currentState(); s != targetRunning {
				t.Errorf("b's state = %s, want running", s)
			}
			h.gotRunner(fakes[1], sb, 1)
			if fakes[0].sessionCount() != 0 || h.calls.count("a.GetOrCreateScaleSet") != 0 {
				t.Errorf("a went on after its permanent error: %v", h.calls.snapshot())
			}
			h.exit()
		})
	}
}

// TestTargetMissingPermissionOnlyThatTarget proves 0002 SPEC R238: target
// A's installation without administration: write puts only A in the
// target error wait, naming the permission and the repository.
func TestTargetMissingPermissionOnlyThatTarget(t *testing.T) {
	h := newHarness(t)
	fakes := h.useTargets("a", "b")
	fakes[0].inst.Permissions = map[string]string{"administration": "read"}
	h.start()
	sb := fakes[1].polled(t, 0)
	trs := h.targetRuns()
	h.waitTargetState(trs[0], targetErrorWait)
	last, _ := trs[0].rec.Last()
	if !strings.Contains(last, "administration: write") || !strings.Contains(last, "repository acme/a") {
		t.Errorf("a's lastError = %q", last)
	}
	if h.calls.count("github.Target a") != 0 {
		t.Errorf("a's clients were made: %v", h.calls.snapshot())
	}
	h.gotRunner(fakes[1], sb, 1)
	h.exit()
}

// TestTargetBackoffDoesNotDelayOthers proves 0002 SPEC R177: while target A
// waits in its R24 backoff, target B makes its first session and starts a
// runner without the clock moving.
func TestTargetBackoffDoesNotDelayOthers(t *testing.T) {
	h := newHarness(t)
	fakes := h.useTargets("a", "b")
	fakes[0].setErr = func(int) error { return opErr(retry.OpGetOrCreateScaleSet, statusError(503)) }
	h.start()
	waitFor(t, "a's R24 backoff", func() bool {
		return h.calls.count("a.GetOrCreateScaleSet") == 1 && h.clock.hasTimerAt(testEpoch.Add(time.Second))
	})
	sb := fakes[1].polled(t, 0)
	h.gotRunner(fakes[1], sb, 1)
	if !h.clock.Now().Equal(testEpoch) {
		t.Fatalf("the clock moved to %v", h.clock.Now())
	}
	trs := h.targetRuns()
	if s := trs[0].currentState(); s != targetStarting {
		t.Errorf("a's state = %s, want starting", s)
	}
	if n := h.calls.count("a.GetOrCreateScaleSet"); n != 1 {
		t.Errorf("a's scale set calls = %d, want 1 before its backoff ends", n)
	}
	h.exit()
}

// TestFirstSessionOfAnyTarget proves 0002 SPEC R178: the scaler is running
// once target B made its first session while target A still starts; A
// joins after its backoff.
func TestFirstSessionOfAnyTarget(t *testing.T) {
	h := newHarness(t)
	fakes := h.useTargets("a", "b")
	fakes[0].createErr = func(n int) error {
		if n == 1 {
			return opErr(retry.OpSessionCreate, statusError(503))
		}
		return nil
	}
	h.start()
	waitFor(t, "a's session backoff", func() bool {
		return h.calls.count("a.CreateSession") == 1 && h.clock.hasTimerAt(testEpoch.Add(time.Second))
	})
	fakes[1].polled(t, 0)
	h.waitState(status.Running)
	trs := h.targetRuns()
	if s := trs[0].currentState(); s != targetStarting {
		t.Errorf("a's state = %s, want starting", s)
	}
	h.clock.advanceTo(t, testEpoch.Add(time.Second))
	fakes[0].polled(t, 0)
	h.waitTargetState(trs[0], targetRunning)
	h.exit()
}

// TestAllTargetsFailBeforeFirstSession proves 0002 SPEC R195: when every
// target enters the target error wait before any first session, start-up
// counts as finished and the scaler is running, not in the error wait.
func TestAllTargetsFailBeforeFirstSession(t *testing.T) {
	h := newHarness(t)
	fakes := h.useTargets("a", "b")
	fakes[0].instErr = func(int) error {
		return opErr(retry.OpGetInstallation, &github.StatusError{Code: 404, Message: "GitHub App is not installed on repository acme/a"})
	}
	fakes[1].setErr = func(int) error { return opErr(retry.OpGetOrCreateScaleSet, retry.ErrAuth) }
	h.start()
	trs := h.targetRuns()
	for _, tr := range trs {
		h.waitTargetState(tr, targetErrorWait)
	}
	h.waitState(status.Running)
	if !strings.Contains(h.logs.String(), "every target is in the target error wait") {
		t.Error("R195 not logged")
	}
	if fakes[0].sessionCount()+fakes[1].sessionCount() != 0 {
		t.Errorf("a session was made: %v", h.calls.snapshot())
	}
	h.exit()
}

// TestSignalStopsTargetsStillStarting proves 0002 SPEC R196: a signal after
// target B's first session stops target A's start phase; A makes no session
// and has no runner.
func TestSignalStopsTargetsStillStarting(t *testing.T) {
	h := newHarness(t)
	fakes := h.useTargets("a", "b")
	fakes[0].createErr = func(int) error { return opErr(retry.OpSessionCreate, statusError(503)) }
	h.start()
	fakes[1].polled(t, 0)
	h.waitState(status.Running)
	waitFor(t, "a's session backoff", func() bool {
		return h.calls.count("a.CreateSession") == 1 && h.clock.hasTimerAt(testEpoch.Add(time.Second))
	})
	h.exit()
	h.clock.Advance(10 * time.Minute)
	if n := h.calls.count("a.CreateSession"); n != 1 {
		t.Errorf("a tried %d sessions, want 1: its start phase did not stop", n)
	}
	if fakes[0].sessionCount() != 0 || h.d.table.Counts("acme/a").All() != 0 {
		t.Error("a has a session or a runner")
	}
	if s := h.targetRuns()[0].currentState(); s != targetStopped {
		t.Errorf("a's state = %s, want stopped", s)
	}
}

// TestEventRoutedByTarget proves 0002 PLAN M12-7: the job check server
// passes a report to the judge of the row's target.
func TestEventRoutedByTarget(t *testing.T) {
	h := newHarness(t)
	fakes := h.useTargets("a", "b")
	h.start()
	fakes[0].polled(t, 0)
	fakes[1].polled(t, 0)
	trs := h.targetRuns()
	h.seedRunnerOf(trs[0], "pool-000000a1", 1, scaler.StateIdle)
	h.seedRunnerOf(trs[1], "pool-000000b1", 2, scaler.StateIdle)
	aTok, bTok := h.giveToken("pool-000000a1"), h.giveToken("pool-000000b1")
	h.assertVerdict(h.postEvent("pool-000000b1", bTok, strings.NewReader(sameRepoEvent)), "pool-000000b1", scaler.VerdictAllow)
	h.assertVerdict(h.postEvent("pool-000000a1", aTok, strings.NewReader(forkEvent)), "pool-000000a1", scaler.VerdictDeny)
	if strings.Contains(h.logs.String(), "not of this target") {
		t.Errorf("a report reached another target's judge:\n%s", h.logs)
	}
	h.signal()
	h.signal()
	if err := h.result(); err != nil {
		t.Fatal(err)
	}
	h.assertTargetsStopped()
}

// TestSignalDrainTwoTargets: during a signal drain every target's
// scale-down runs, so target B's idle runner is removed through B's
// repository while target A's busy runner is still waited for; the drain
// then ends when A's runner ends, before the drain timeout (0002 PLAN M11-5,
// M12-7).
func TestSignalDrainTwoTargets(t *testing.T) {
	h := newHarness(t)
	fakes := h.useTargets("a", "b")
	h.start()
	fakes[0].polled(t, 0)
	fakes[1].polled(t, 0)
	trs := h.targetRuns()
	h.seedRunnerOf(trs[0], "pool-000000a1", 1, scaler.StateBusy)
	h.seedRunnerOf(trs[1], "pool-000000b1", 2, scaler.StateIdle)
	h.signal()
	h.waitState(status.Draining)
	waitFor(t, "b's idle runner removed during the drain", func() bool { return h.calls.count("b.RemoveRunner 2") == 1 })
	if h.returned() {
		t.Fatal("the drain ended while a's runner was busy")
	}
	h.endRunner("pool-000000a1")
	for !h.returned() {
		h.clock.Advance(5 * time.Second)
		waitFor(t, "a drain poll", func() bool { return h.returned() || h.clock.hasTimerAt(h.clock.Now().Add(5*time.Second)) })
	}
	if err := h.result(); err != nil {
		t.Fatal(err)
	}
	if h.clock.Now().Sub(testEpoch) >= h.cfg.Shutdown.DrainTimeout {
		t.Errorf("the drain ran %v, up to its timeout", h.clock.Now().Sub(testEpoch))
	}
	if got := fakes[1].removedIDs(); !slices.Equal(got, []int64{2}) {
		t.Errorf("b's RemoveRunner = %v, want [2] once", got)
	}
	if got := fakes[0].removedIDs(); len(got) != 0 {
		t.Errorf("a's RemoveRunner = %v, want none", got)
	}
	h.assertTargetsStopped()
}

// TestFinalizeRemovesEachTargetsRunners: finalize removes the rows left of
// every target and unregisters each through its own repository, sharing
// one budget (0002 PLAN M12-7).
func TestFinalizeRemovesEachTargetsRunners(t *testing.T) {
	h := newHarness(t)
	fakes := h.useTargets("a", "b")
	h.start()
	fakes[0].polled(t, 0)
	fakes[1].polled(t, 0)
	trs := h.targetRuns()
	h.seedRunnerOf(trs[0], "pool-000000a1", 1, scaler.StateBusy)
	h.seedRunnerOf(trs[1], "pool-000000b1", 1, scaler.StateBusy)
	h.signal()
	h.waitState(status.Draining)
	h.signal()
	if err := h.result(); err != nil {
		t.Fatal(err)
	}
	for _, f := range fakes {
		if got := f.removedIDs(); !slices.Equal(got, []int64{1}) {
			t.Errorf("%s: RemoveRunner = %v, want [1]", f.prefix, got)
		}
		if _, _, _, closed := f.session(0).snapshot(); closed != 1 {
			t.Errorf("%s: session closed %d times, want 1", f.prefix, closed)
		}
	}
	for _, name := range []string{"pool-000000a1", "pool-000000b1"} {
		if n := h.calls.count("docker.RemoveResources " + name); n != 1 {
			t.Errorf("RemoveResources %s = %d, want 1", name, n)
		}
	}
	h.assertTargetsStopped()
}

// TestJobServerListenFailureBeforeTargets proves 0002 SPEC R176 for the job
// check server listen: its failure is the server's error wait, and no target
// makes a GitHub call or a session.
func TestJobServerListenFailureBeforeTargets(t *testing.T) {
	h := newHarness(t)
	h.useTargets("a", "b")
	h.jobs.listenErr = errors.New("address in use")
	h.start()
	h.waitState(status.ErrorWait)
	if r := h.report(); !strings.Contains(r.LastError, "address in use") {
		t.Errorf("lastError = %q", r.LastError)
	}
	log := h.calls.snapshot()
	if indexOf(log, "jobs.Listen") < 0 {
		t.Fatalf("the job check server did not listen: %v", log)
	}
	for _, prefix := range []string{"github.", "a.", "b.", "a-session", "b-session"} {
		if i := indexOf(log, prefix); i >= 0 {
			t.Errorf("call %q after the listen failure", log[i])
		}
	}
	h.assertNoCallsFor(10 * time.Minute)
	h.signal()
	if err := h.result(); err != nil {
		t.Fatal(err)
	}
}
