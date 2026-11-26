package daemon

import (
	"context"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/actions/scaleset"

	"github.com/dewevrei/actions-runner-scaler/internal/docker"
	"github.com/dewevrei/actions-runner-scaler/internal/github"
	"github.com/dewevrei/actions-runner-scaler/internal/retry"
	"github.com/dewevrei/actions-runner-scaler/internal/scaler"
	"github.com/dewevrei/actions-runner-scaler/internal/status"
)

// notInstalled is the installation lookup's answer for an organization
// without the App (0002 SPEC R109).
func notInstalled(org string) error {
	return opErr(retry.OpGetInstallation, &github.StatusError{Code: 404, Message: "GitHub App is not installed on organization " + org})
}

// permanentPoll ends a listener with a permanent GetMessage error.
func permanentPoll() error { return opErr(retry.OpGetMessage, retry.ErrAuth) }

// driveUntil moves the clock in drain-poll steps until cond holds; between
// steps it waits for cond or the next drain poll timer.
func (h *harness) driveUntil(what string, cond func() bool) {
	h.t.Helper()
	for !cond() {
		h.clock.Advance(5 * time.Second)
		waitFor(h.t, what, func() bool { return cond() || h.clock.hasTimerAt(h.clock.Now().Add(5*time.Second)) })
	}
}

// waitRetry waits until tr's next retry is due in after from now, after n
// installation lookups of gh, and returns its time.
func (h *harness) waitRetry(tr *targetRun, gh *fakeGitHub, n int, after time.Duration) time.Time {
	h.t.Helper()
	waitFor(h.t, "a retry in "+after.String(), func() bool {
		at := tr.retryAt()
		return gh.installations() == n && !at.IsZero() && at.Sub(h.clock.Now()) == after
	})
	return tr.retryAt()
}

// seedAllowed seeds a busy runner of tr with an allow verdict, so R81 never
// removes it and reconciliation does not look it up.
func (h *harness) seedAllowed(tr *targetRun, name string, id int64) {
	h.t.Helper()
	h.seedRunnerOf(tr, name, id, scaler.StateBusy)
	if !h.d.table.Allow(name) {
		h.t.Fatalf("Allow(%s) = false", name)
	}
}

// targetCalls counts the calls made for organization org: its clients,
// its sessions and its installation lookups.
func (h *harness) targetCalls(org string) int {
	return h.calls.count(org+".") + h.calls.count(org+"-session") +
		h.calls.count("github.Installation "+org) + h.calls.count("github.Target "+org)
}

// TestTargetPermanentErrorDrainsOnlyThatTarget proves 0002 SPEC R171, R173
// and R183: a permanent GetMessage error of target A while B's busy runner
// runs puts A through its error drain into the target error wait; B's
// runner is not removed and ends, and B's next job then gets a runner.
func TestTargetPermanentErrorDrainsOnlyThatTarget(t *testing.T) {
	h := newHarness(t)
	fakes := h.useTargets("a", "b")
	h.start()
	sa, sb := fakes[0].polled(t, 0), fakes[1].polled(t, 0)
	trs := h.targetRuns()
	h.seedRunnerOf(trs[1], "pool-000000b1", 1, scaler.StateBusy)

	sa.send(t, nil, permanentPoll())
	h.waitTargetState(trs[0], targetErrorWait)
	if r := h.report(); r.State != status.Running {
		t.Errorf("state = %s, want running", r.State)
	}
	if last, _ := trs[0].rec.Last(); !strings.Contains(last, "target error drain") {
		t.Errorf("a's lastError = %q", last)
	}
	if _, _, _, closed := sa.snapshot(); closed != 1 {
		t.Errorf("a's session closed %d times, want 1", closed)
	}
	if s := trs[1].currentState(); s != targetRunning {
		t.Errorf("b's state = %s, want running", s)
	}
	if info, ok := h.d.table.Get("pool-000000b1"); !ok || info.State != scaler.StateBusy {
		t.Fatalf("b's runner = %+v, %v; want busy and kept", info, ok)
	}
	if n := h.calls.count("b.RemoveRunner") + h.calls.count("docker.RemoveResources pool-000000b1"); n != 0 {
		t.Fatalf("b's runner was removed: %v", h.calls.snapshot())
	}
	h.endRunner("pool-000000b1")
	h.gotRunner(fakes[1], sb, 1)
	waitFor(t, "b's new runner", func() bool { return h.d.table.Counts("b").Live() == 1 })
	h.exit()
}

// TestTargetErrorDrainRules proves 0002 SPEC R173: during A's error drain
// its idle runner is removed, its busy runner is waited for until
// shutdown.drain-timeout, and then removed with one RemoveRunner; B keeps
// running.
func TestTargetErrorDrainRules(t *testing.T) {
	h := newHarness(t)
	fakes := h.useTargets("a", "b")
	h.cfg.Shutdown.DrainTimeout = time.Minute
	h.start()
	sa, sb := fakes[0].polled(t, 0), fakes[1].polled(t, 0)
	trs := h.targetRuns()
	h.seedRunnerOf(trs[0], "pool-000000a1", 1, scaler.StateIdle)
	h.seedRunnerOf(trs[0], "pool-000000a2", 2, scaler.StateBusy)
	t0 := h.clock.Now()

	sa.send(t, nil, permanentPoll())
	waitFor(t, "a's idle runner removed", func() bool { _, ok := h.d.table.Get("pool-000000a1"); return !ok })
	if got := fakes[0].removedIDs(); !slices.Equal(got, []int64{1}) {
		t.Fatalf("a's RemoveRunner = %v, want [1] for the idle runner", got)
	}
	if s := trs[0].currentState(); s != targetErrorDrain {
		t.Fatalf("a's state = %s, want error-drain while its runner is busy", s)
	}
	h.driveUntil("a's error wait", func() bool { return trs[0].currentState() == targetErrorWait })
	if took := h.clock.Now().Sub(t0); took < time.Minute {
		t.Errorf("a's drain ended after %v, before drain-timeout", took)
	}
	if got := fakes[0].removedIDs(); !slices.Equal(got, []int64{1, 2}) {
		t.Errorf("a's RemoveRunner = %v, want [1 2]: the busy runner once", got)
	}
	if n := h.calls.count("docker.RemoveResources pool-000000a2"); n != 1 {
		t.Errorf("RemoveResources of the busy runner = %d, want 1", n)
	}
	if n := h.calls.count("b.RemoveRunner"); n != 0 {
		t.Errorf("b's RemoveRunner = %d", n)
	}
	if _, _, _, closed := sb.snapshot(); closed != 0 || trs[1].currentState() != targetRunning {
		t.Errorf("b's session closed %d times, state %s", closed, trs[1].currentState())
	}
	h.exit()
}

// TestTargetFinalizePendingOnlyThatTarget proves 0002 SPEC R131 and R173:
// during A's drain-end cleanup a first event report of A's runner gets
// pending with nothing recorded, and one of B's runner gets its verdict.
func TestTargetFinalizePendingOnlyThatTarget(t *testing.T) {
	h := newHarness(t)
	fakes := h.useTargets("a", "b")
	h.cfg.Shutdown.DrainTimeout = time.Minute
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	h.docker.removeErr = func(ctx context.Context, name string) error {
		if name != "pool-000000a1" {
			return nil
		}
		once.Do(func() { close(entered) })
		select {
		case <-release:
		case <-ctx.Done():
		}
		return nil
	}
	h.start()
	sa := fakes[0].polled(t, 0)
	fakes[1].polled(t, 0)
	trs := h.targetRuns()
	h.seedRunnerOf(trs[0], "pool-000000a1", 1, scaler.StateBusy)
	h.seedRunnerOf(trs[1], "pool-000000b1", 2, scaler.StateBusy)
	aTok, bTok := h.giveToken("pool-000000a1"), h.giveToken("pool-000000b1")

	sa.send(t, nil, permanentPoll())
	h.driveUntil("a's cleanup", func() bool {
		select {
		case <-entered:
			return true
		default:
			return false
		}
	})
	rec := h.postEvent("pool-000000a1", aTok, strings.NewReader(sameRepoEvent))
	if rec.Code != 200 || rec.Body.String() != `{"verdict":"pending"}` {
		t.Errorf("a's report: %d %q, want 200 pending", rec.Code, rec.Body.String())
	}
	if info, _ := h.d.table.Get("pool-000000a1"); info.Verdict != scaler.VerdictPending {
		t.Errorf("a's verdict %v recorded during its cleanup", info.Verdict)
	}
	h.assertVerdict(h.postEvent("pool-000000b1", bTok, strings.NewReader(sameRepoEvent)), "pool-000000b1", scaler.VerdictAllow)
	close(release)
	h.waitTargetState(trs[0], targetErrorWait)
	h.endRunner("pool-000000b1")
	h.exit()
}

// TestTargetRetryIntervals proves 0002 SPEC R174: the retries of the target
// error wait come 30 s after entering it, then 60 s, 120 s, … after each
// failure up to 30 minutes, with no limit on their number, and no call is
// made between them.
func TestTargetRetryIntervals(t *testing.T) {
	h := newHarness(t)
	h.gh.instErr = func(int) error { return notInstalled("owner") }
	h.start()
	tr := h.targetRuns()[0]
	h.waitTargetState(tr, targetErrorWait)
	want := []time.Duration{
		30 * time.Second, time.Minute, 2 * time.Minute, 4 * time.Minute, 8 * time.Minute, 16 * time.Minute,
		30 * time.Minute, 30 * time.Minute, 30 * time.Minute, 30 * time.Minute,
	}
	for i, interval := range want {
		h.waitRetry(tr, h.gh, i+1, interval)
		before := len(h.calls.snapshot())
		h.clock.Advance(interval - time.Nanosecond)
		for _, c := range h.calls.snapshot()[before:] {
			if !strings.HasPrefix(c, "docker.") {
				t.Fatalf("retry %d: call %q before the retry", i+1, c)
			}
		}
		h.clock.Advance(time.Nanosecond)
	}
	waitFor(t, "the last retry", func() bool { return h.gh.installations() == len(want)+1 })
	if s := tr.currentState(); s != targetErrorWait {
		t.Errorf("state = %s, want error-wait", s)
	}
	h.signal()
	if err := h.result(); err != nil {
		t.Fatal(err)
	}
}

// TestTargetRetryRateLimited proves 0002 SPEC R174: after a rate-limited
// retry the next one comes at the later of the R52 time and the interval's
// time.
func TestTargetRetryRateLimited(t *testing.T) {
	h := newHarness(t)
	h.gh.instErr = func(n int) error {
		switch n {
		case 2:
			return opErr(retry.OpGetInstallation, &retry.RateLimitError{RetryAfter: 10 * time.Minute})
		case 3:
			return opErr(retry.OpGetInstallation, &retry.RateLimitError{RetryAfter: 10 * time.Second})
		default:
			return notInstalled("owner")
		}
	}
	h.start()
	tr := h.targetRuns()[0]
	h.waitTargetState(tr, targetErrorWait)
	at := h.waitRetry(tr, h.gh, 1, 30*time.Second)
	h.clock.Advance(at.Sub(h.clock.Now()))
	// The R52 time, 10 minutes, is later than the 60 s interval.
	at = h.waitRetry(tr, h.gh, 2, 10*time.Minute)
	h.clock.Advance(at.Sub(h.clock.Now()))
	// The 120 s interval is later than the R52 time, 10 s.
	h.waitRetry(tr, h.gh, 3, 2*time.Minute)
	h.signal()
	if err := h.result(); err != nil {
		t.Fatal(err)
	}
}

// TestTargetRetryNoCallsOutside proves 0002 SPEC R174 and R199: while A
// retries, nothing is called for A between its retries, each retry looks
// the installation up once, and B's calls, session and runners stay as
// they were.
func TestTargetRetryNoCallsOutside(t *testing.T) {
	h := newHarness(t)
	fakes := h.useTargets("a", "b")
	h.start()
	sa, sb := fakes[0].polled(t, 0), fakes[1].polled(t, 0)
	trs := h.targetRuns()
	h.seedAllowed(trs[1], "pool-000000b1", 1)
	fakes[0].setInstErr(func(int) error { return notInstalled("a") })
	sa.send(t, nil, permanentPoll())
	h.waitTargetState(trs[0], targetErrorWait)
	bCalls, bRows := h.calls.count("b."), h.d.table.Counts("b")

	// Three retries take 3.5 minutes, less than the 5 minutes after which
	// B's unanswered polls would end its session (0002 SPEC R153).
	interval := 30 * time.Second
	for i := range 3 {
		at := h.waitRetry(trs[0], fakes[0], i+1, interval)
		aCalls := h.targetCalls("a")
		h.clock.Advance(at.Sub(h.clock.Now()) - time.Nanosecond)
		if n := h.targetCalls("a"); n != aCalls {
			t.Fatalf("retry %d: %d calls for a before it", i+1, n-aCalls)
		}
		h.clock.Advance(time.Nanosecond)
		waitFor(t, "a's retry", func() bool { return fakes[0].installations() == i+2 })
		interval *= 2
	}
	if n := h.calls.count("b."); n != bCalls {
		t.Errorf("b's calls changed during a's retries: %d, want %d", n, bCalls)
	}
	if c := h.d.table.Counts("b"); c != bRows {
		t.Errorf("b's runners changed during a's retries: %+v, want %+v", c, bRows)
	}
	if _, _, _, closed := sb.snapshot(); closed != 0 || fakes[1].sessionCount() != 1 {
		t.Errorf("b's session closed %d times, %d sessions", closed, fakes[1].sessionCount())
	}
	h.signal()
	h.waitState(status.Draining)
	h.endRunner("pool-000000b1")
	h.driveUntil("Run's end", h.returned)
	if err := h.result(); err != nil {
		t.Fatal(err)
	}
}

// TestRetryRunsStartStepsWithoutR24 proves 0002 SPEC R193: a retry runs the
// start phase from the installation lookup on; R148's failure does not end
// it, and a transient error ends it as a failed retry without R24's
// backoff, recorded as the target's last error.
func TestRetryRunsStartStepsWithoutR24(t *testing.T) {
	h := newHarness(t)
	h.gh.instErr = func(n int) error {
		if n == 1 {
			return notInstalled("owner")
		}
		return nil
	}
	h.gh.findSetErr = opErr(retry.OpGetRunnerScaleSet, retry.ErrAuth)
	h.gh.setErr = func(int) error { return opErr(retry.OpGetOrCreateScaleSet, statusError(503)) }
	h.start()
	tr := h.targetRuns()[0]
	at := h.waitRetry(tr, h.gh, 1, 30*time.Second)
	h.clock.Advance(at.Sub(h.clock.Now()))
	h.waitRetry(tr, h.gh, 2, time.Minute)
	log := h.calls.snapshot()
	order := []string{"github.Installation owner", "github.Target owner", "github.RunnerGroupID", "github.FindScaleSet", "github.GetOrCreateScaleSet"}
	last := -1
	for _, step := range order {
		i := slices.IndexFunc(log, func(c string) bool { return strings.HasPrefix(c, step) })
		if i <= last {
			t.Fatalf("step %q out of order or missing: %v", step, log)
		}
		last = i
	}
	h.clock.Advance(time.Minute - time.Nanosecond)
	if n := h.calls.count("github.GetOrCreateScaleSet"); n != 1 {
		t.Fatalf("scale set calls = %d before the next retry, want 1 (no R24 backoff)", n)
	}
	if last, _ := tr.rec.Last(); !strings.Contains(last, "503") {
		t.Errorf("lastError = %q, want the retry's 503", last)
	}
	if h.gh.sessionCount() != 0 {
		t.Error("a session was made")
	}
	h.signal()
	if err := h.result(); err != nil {
		t.Fatal(err)
	}
}

// TestRetrySuccessReturnsToRunning proves 0002 SPEC R194 and R199: once A's
// installation lookup succeeds again, A runs at that retry without a
// restart, its marks are cleared and its watches resume, its job gets a
// runner, and its next target error wait starts again at 30 s.
func TestRetrySuccessReturnsToRunning(t *testing.T) {
	h := newHarness(t)
	fakes := h.useTargets("a", "b")
	h.start()
	sa := fakes[0].polled(t, 0)
	fakes[1].polled(t, 0)
	trs := h.targetRuns()
	fakes[0].setInstErr(func(n int) error {
		if n == 2 {
			return notInstalled("a")
		}
		return nil
	})
	sa.send(t, nil, permanentPoll())
	at := h.waitRetry(trs[0], fakes[0], 1, 30*time.Second)
	h.clock.Advance(at.Sub(h.clock.Now()))
	at = h.waitRetry(trs[0], fakes[0], 2, time.Minute)
	h.clock.Advance(at.Sub(h.clock.Now()))

	sa2 := fakes[0].polled(t, 1)
	h.waitTargetState(trs[0], targetRunning)
	if h.d.table.TargetDraining("a") || h.d.table.TargetFinalizing("a") || !trs[0].retryAt().IsZero() {
		t.Fatal("a's marks or retry time left after it runs again")
	}
	h.gotRunner(fakes[0], sa2, 1)
	waitFor(t, "a's new runner watched", func() bool {
		for _, name := range h.docker.startedNames() {
			if h.calls.count("docker.Wait "+name) > 0 {
				return true
			}
		}
		return false
	})

	// A's judge works again: a report of A's runner is judged.
	h.seedRunnerOf(trs[0], "pool-000000a9", 9, scaler.StateIdle)
	tok := h.giveToken("pool-000000a9")
	h.assertVerdict(h.postEvent("pool-000000a9", tok, strings.NewReader(sameRepoEvent)), "pool-000000a9", scaler.VerdictAllow)
	// The report made it busy; it ends before A's next drain.
	h.endRunner("pool-000000a9")

	sa2.send(t, nil, permanentPoll())
	h.waitTargetState(trs[0], targetErrorWait)
	h.waitRetry(trs[0], fakes[0], 3, 30*time.Second)
	h.exit()
}

// TestAllTargetsErrorWaitNoServerErrorWait proves 0002 SPEC R175: with every
// target in the target error wait the scaler stays running, not in the
// error wait, and each target retries.
func TestAllTargetsErrorWaitNoServerErrorWait(t *testing.T) {
	h := newHarness(t)
	fakes := h.useTargets("a", "b")
	h.start()
	trs := h.targetRuns()
	sessions := []*fakeSession{fakes[0].polled(t, 0), fakes[1].polled(t, 0)}
	for i, f := range fakes {
		f.setInstErr(func(int) error { return notInstalled(f.prefix) })
		sessions[i].send(t, nil, permanentPoll())
		h.waitTargetState(trs[i], targetErrorWait)
	}
	if r := h.report(); r.State != status.Running {
		t.Fatalf("state = %s, want running", r.State)
	}
	for i, f := range fakes {
		h.waitRetry(trs[i], f, 1, 30*time.Second)
	}
	h.clock.Advance(30 * time.Second)
	for _, f := range fakes {
		waitFor(t, f.prefix+"'s retry", func() bool { return f.installations() == 2 })
	}
	if r := h.report(); r.State != status.Running {
		t.Fatalf("state = %s after the retries, want running", r.State)
	}
	h.exit()
}

// TestTargetDrainStopsOnlyItsWatches proves 0002 SPEC R190: at the end of
// A's error drain A's watches and pending removal retries stop and its rows
// are no longer reconciled, while reconciliation and watches go on for B.
func TestTargetDrainStopsOnlyItsWatches(t *testing.T) {
	h := newHarness(t)
	fakes := h.useTargets("a", "b")
	h.start()
	sa, sb := fakes[0].polled(t, 0), fakes[1].polled(t, 0)
	trs := h.targetRuns()
	h.gotRunner(fakes[0], sa, 1)
	h.gotRunner(fakes[1], sb, 1)
	var aName, bName string
	waitFor(t, "both runners watched", func() bool {
		names := h.docker.startedNames()
		if len(names) != 2 {
			return false
		}
		for _, n := range names {
			if h.calls.count("docker.Wait "+n) == 0 {
				return false
			}
			if info, _ := h.d.table.Get(n); info.Target == "a" {
				aName = n
			} else {
				bName = n
			}
		}
		return aName != "" && bName != ""
	})
	h.docker.mu.Lock()
	h.docker.removeErr = func(_ context.Context, name string) error {
		if name == aName {
			return statusError(500)
		}
		return nil
	}
	h.docker.mu.Unlock()
	fakes[0].setInstErr(func(int) error { return notInstalled("a") })

	sa.send(t, nil, permanentPoll())
	h.waitTargetState(trs[0], targetErrorWait)
	ended := func(w *scaler.RunnerWatcher) bool {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		return w.Wait(ctx) == nil
	}
	if !ended(trs[0].scaler.Watcher()) {
		t.Error("a's watch still runs in its target error wait")
	}
	if ended(trs[1].scaler.Watcher()) {
		t.Error("b's watch stopped")
	}
	aRemovals := h.calls.count("docker.RemoveResources " + aName)

	// B's container disappears without its watcher noticing; the next
	// reconciliation removes B's runner.
	h.docker.dropRunner(bName)
	h.clock.Advance(30 * time.Second)
	waitFor(t, "b's runner reconciled", func() bool { _, ok := h.d.table.Get(bName); return !ok })
	for range 10 {
		rounds := h.calls.count("docker.List")
		h.clock.Advance(30 * time.Second)
		waitFor(t, "a reconciliation round", func() bool { return h.calls.count("docker.List") > rounds })
	}
	if n := h.calls.count("docker.RemoveResources " + aName); n != aRemovals {
		t.Errorf("a's runner removal retried or reconciled in its error wait: %d calls, want %d", n, aRemovals)
	}
	if _, ok := h.d.table.Get(aName); !ok {
		t.Error("a's row left the table without a successful removal")
	}
	h.exit()
}

// TestTargetErrorWaitStopsJobChecks proves 0002 SPEC R174 and R190 for the
// judge: a busy runner of A whose removal failed in A's cleanup stays in
// the table, and its job check timer (R81) does not fire a deny path in
// A's error wait.
func TestTargetErrorWaitStopsJobChecks(t *testing.T) {
	h := newHarness(t)
	fakes := h.useTargets("a", "b")
	h.cfg.Shutdown.DrainTimeout = time.Minute
	h.docker.removeErr = func(_ context.Context, name string) error {
		if name == "pool-000000a1" {
			return statusError(500)
		}
		return nil
	}
	h.start()
	sa := fakes[0].polled(t, 0)
	fakes[1].polled(t, 0)
	trs := h.targetRuns()
	h.seedRunnerOf(trs[0], "pool-000000a1", 1, scaler.StateBusy)
	fakes[0].setInstErr(func(int) error { return notInstalled("a") })
	sa.send(t, nil, permanentPoll())
	h.driveUntil("a's error wait", func() bool { return trs[0].currentState() == targetErrorWait })
	removes, containers := len(fakes[0].removedIDs()), h.calls.count("docker.RemoveContainer")
	h.clock.Advance(5 * time.Minute)
	for range 100 {
		runtime.Gosched()
	}
	if n := len(fakes[0].removedIDs()); n != removes {
		t.Errorf("a's RemoveRunner calls in its error wait: %d, want %d", n, removes)
	}
	if n := h.calls.count("docker.RemoveContainer"); n != containers {
		t.Errorf("a deny path removed a container in a's error wait (%d calls)", n-containers)
	}
	if _, ok := h.d.table.Get("pool-000000a1"); !ok {
		t.Error("a's leftover row left the table")
	}
	h.exit()
}

// TestSignalDuringTargetErrorDrainWaitsBusy proves 0002 SPEC R179: a signal
// during A's error drain starts the signal drain, which waits for A's busy
// runner past A's own drain timeout; A's drain does not go on to its
// cleanup.
func TestSignalDuringTargetErrorDrainWaitsBusy(t *testing.T) {
	h := newHarness(t)
	fakes := h.useTargets("a", "b")
	h.start()
	sa := fakes[0].polled(t, 0)
	fakes[1].polled(t, 0)
	trs := h.targetRuns()
	h.seedAllowed(trs[0], "pool-000000a1", 1)
	t0 := h.clock.Now()
	sa.send(t, nil, permanentPoll())
	h.waitTargetState(trs[0], targetErrorDrain)
	h.clock.Advance(5 * time.Minute)

	h.signal()
	h.waitState(status.Draining)
	waitFor(t, "a's drain stopped", func() bool { return strings.Contains(h.logs.String(), "target error drain stopped") })
	h.clock.Advance(t0.Add(h.cfg.Shutdown.DrainTimeout + time.Minute).Sub(h.clock.Now()))
	if h.returned() {
		t.Fatal("Run returned while a's runner was busy")
	}
	if _, ok := h.d.table.Get("pool-000000a1"); !ok {
		t.Fatal("a's busy runner was removed")
	}
	h.endRunner("pool-000000a1")
	h.driveUntil("Run's end", h.returned)
	if err := h.result(); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(h.logs.String(), "target cleanup started") {
		t.Error("a's drain went on to its cleanup after the signal")
	}
	if s := trs[0].currentState(); s != targetErrorDrain {
		t.Errorf("a's state = %s, want error-drain", s)
	}
	h.assertTargetsStopped()
}

// TestSignalStopsRetries proves 0002 SPEC R192: once the signal drain
// begins no retry is made, and a retry in progress cancels its call and
// makes no session.
func TestSignalStopsRetries(t *testing.T) {
	t.Run("waiting", func(t *testing.T) {
		h := newHarness(t)
		fakes := h.useTargets("a", "b")
		fakes[0].instErr = func(int) error { return notInstalled("a") }
		h.start()
		trs := h.targetRuns()
		fakes[1].polled(t, 0)
		h.seedAllowed(trs[1], "pool-000000b1", 1)
		h.waitRetry(trs[0], fakes[0], 1, 30*time.Second)
		h.signal()
		h.waitState(status.Draining)
		h.clock.Advance(5 * time.Minute)
		if n := fakes[0].installations(); n != 1 {
			t.Errorf("a's installation lookups = %d during the signal drain, want 1", n)
		}
		h.endRunner("pool-000000b1")
		h.driveUntil("Run's end", h.returned)
		if err := h.result(); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("in progress", func(t *testing.T) {
		h := newHarness(t)
		fakes := h.useTargets("a", "b")
		inRetry := make(chan struct{})
		fakes[0].instHook = func(ctx context.Context, n int) error {
			switch n {
			case 1:
				return notInstalled("a")
			case 2:
				close(inRetry)
				<-ctx.Done()
				return opErr(retry.OpGetInstallation, ctx.Err())
			}
			return nil
		}
		h.start()
		trs := h.targetRuns()
		fakes[1].polled(t, 0)
		at := h.waitRetry(trs[0], fakes[0], 1, 30*time.Second)
		h.clock.Advance(at.Sub(h.clock.Now()))
		<-inRetry
		h.signal()
		if err := h.result(); err != nil {
			t.Fatal(err)
		}
		h.assertTargetsStopped()
		h.clock.Advance(time.Hour)
		if fakes[0].sessionCount() != 0 || h.calls.count("github.Target a") != 0 || fakes[0].installations() != 2 {
			t.Errorf("a went on after the signal: %v", h.calls.snapshot())
		}
	})
}

// TestSignalDrainTargetPermanentErrorLogsOnly proves 0002 SPEC R191: during
// the signal drain a permanent error of A's call is logged and recorded as
// A's last error only; A does not enter its error drain.
func TestSignalDrainTargetPermanentErrorLogsOnly(t *testing.T) {
	h := newHarness(t)
	fakes := h.useTargets("a", "b")
	h.start()
	sa := fakes[0].polled(t, 0)
	fakes[1].polled(t, 0)
	trs := h.targetRuns()
	h.seedAllowed(trs[1], "pool-000000b1", 1)
	h.signal()
	h.waitState(status.Draining)
	sa.send(t, nil, permanentPoll())
	waitFor(t, "R191 log", func() bool { return strings.Contains(h.logs.String(), "0002 SPEC R191") })
	if s := trs[0].currentState(); s != targetRunning {
		t.Errorf("a's state = %s, want running", s)
	}
	if last, _ := trs[0].rec.Last(); !strings.Contains(last, retry.ErrAuth.Error()) {
		t.Errorf("a's lastError = %q", last)
	}
	if strings.Contains(h.logs.String(), "target error drain started") || h.d.table.TargetDraining("a") {
		t.Error("a entered its error drain during the signal drain")
	}
	h.endRunner("pool-000000b1")
	h.driveUntil("Run's end", h.returned)
	if err := h.result(); err != nil {
		t.Fatal(err)
	}
}

// TestPlacePermanentAcquireDuringOtherScale proves 0002 SPEC R171, R210 and
// R214 on the real daemon drain path: a placement kicked by a row of B
// leaving the table gets a permanent AcquireJobs error for A's entry while
// B's Scale waits for the Placer's lock. Nothing deadlocks, A goes through
// its target error drain into the target error wait with its entries
// dropped, and B's Scale returns and B's job gets a runner.
func TestPlacePermanentAcquireDuringOtherScale(t *testing.T) {
	h := newHarness(t)
	h.docker.info = docker.Capacity{NCPU: 10, MemTotal: 7936 << 20} // server limit 2
	fakes := h.useTargets("a", "b")
	h.start()
	sa, sb := fakes[0].polled(t, 0), fakes[1].polled(t, 0)
	trs := h.targetRuns()
	h.seedRunnerOf(trs[1], "pool-000000b1", 1, scaler.StateBusy)
	h.seedRunnerOf(trs[1], "pool-000000b2", 2, scaler.StateBusy)

	// A's job waits: the server is full.
	sa.send(t, availableMsg(1, 11), nil)
	waitFor(t, "a's message acknowledged", func() bool { _, _, d, _ := sa.snapshot(); return len(d) == 1 })
	if n := h.calls.count("a-session1.AcquireJobs"); n != 0 {
		t.Fatalf("AcquireJobs %d on a full server", n)
	}
	inAcquire, release := make(chan struct{}), make(chan struct{})
	sa.mu.Lock()
	sa.acquireErr = func(int) error {
		close(inAcquire)
		<-release
		return opErr(retry.OpAcquireJobs, statusError(403))
	}
	sa.mu.Unlock()

	h.endRunner("pool-000000b1") // kicks a placement (0002 SPEC R170)
	select {
	case <-inAcquire:
	case <-time.After(failsafe):
		t.Fatal("no AcquireJobs for a's entry after b's row left")
	}
	// B's message reaches its Scale, which waits for the Placer's lock
	// before its table steps (0002 PLAN M14-1).
	msgB := availableMsg(1, 21)
	msgB.JobCompletedMessages = append(msgB.JobCompletedMessages, &scaleset.JobCompleted{
		RunnerName: "pool-0000ffff", JobMessageBase: scaleset.JobMessageBase{RunnerRequestID: 99, JobID: "other"},
	})
	sb.send(t, msgB, nil)
	waitFor(t, "b's Scale waiting for the Placer's lock", waitingInScaled)
	if strings.Contains(h.logs.String(), `"msg":"job completed for a runner not in the table"`) {
		t.Fatal("b's table step ran while the placement held the Placer's lock")
	}
	close(release)

	h.driveUntil("a in the target error wait", func() bool { return trs[0].currentState() == targetErrorWait })
	waitFor(t, "b's Scale returned and acknowledged", func() bool { _, _, d, _ := sb.snapshot(); return len(d) == 1 })
	if !strings.Contains(h.logs.String(), `"msg":"job completed for a runner not in the table"`) {
		t.Fatal("b's table step did not run after the placement released the Placer's lock")
	}
	waitFor(t, "b's job got a runner", func() bool { return h.calls.count("b.GenerateJITConfig") == 1 })
	if n := h.d.placer.WaitingJobs("a"); n != 0 {
		t.Fatalf("a's waiting jobs %d, want its entries dropped (0002 SPEC R210)", n)
	}
	if last, _ := trs[0].rec.Last(); !strings.Contains(last, "target error drain") {
		t.Errorf("a's lastError = %q", last)
	}
	if s := trs[1].currentState(); s != targetRunning {
		t.Errorf("b's state = %s, want running", s)
	}
	h.exit()
}

// waitingInScaled reports whether a goroutine waits for the Placer's lock in
// Placer.scaled, read from the stacks of all goroutines.
func waitingInScaled() bool {
	buf := make([]byte, 1<<20)
	for {
		n := runtime.Stack(buf, true)
		if n < len(buf) {
			buf = buf[:n]
			break
		}
		buf = make([]byte, 2*len(buf))
	}
	for _, g := range strings.Split(string(buf), "\n\n") {
		if strings.Contains(g, "scaler.(*Placer).scaled") && strings.Contains(g, "sync.(*Mutex).Lock") {
			return true
		}
	}
	return false
}

// TestServeDrainMarkBeforeFatal: the placement sets a target's drain mark
// before it calls fatal (0002 PLAN M14-3.4). When the session loop sees the
// mark and ends in between, serve begins the target error drain itself, so
// the target reaches the target error wait instead of being left in
// error-drain with nobody running the drain; the late fatal only records the
// error.
func TestServeDrainMarkBeforeFatal(t *testing.T) {
	h := newHarness(t)
	s1 := h.running()
	tr := h.targetRuns()[0]
	h.d.table.SetTargetDraining(tr.t.Key())
	s1.send(t, nil, opErr(retry.OpGetMessage, statusError(503)))
	// serve begins the drain without waiting for fatal.
	waitFor(t, "the target error drain", func() bool { return tr.currentState() != targetRunning })
	err := opErr(retry.OpAcquireJobs, statusError(403))
	tr.fatal(err)
	h.driveUntil("the target error wait", func() bool { return tr.currentState() == targetErrorWait })
	if last, _ := tr.rec.Last(); !strings.Contains(last, "target error drain") {
		t.Errorf("lastError = %q, want the late fatal's error", last)
	}
	waitFor(t, "a retry scheduled", func() bool { return !tr.retryAt().IsZero() })
	h.exit()
}

// TestFatalNotRunningSetsNoMark: a late fatal for a target that is not
// running, here in its target error wait, sets no drain mark, so a retry
// that returns it to running leaves it no goal of 0.
func TestFatalNotRunningSetsNoMark(t *testing.T) {
	h := newHarness(t)
	fakes := h.useTargets("a")
	fakes[0].setInstErr(func(int) error { return notInstalled("a") })
	h.start()
	tr := h.targetRuns()[0]
	h.waitTargetState(tr, targetErrorWait)
	tr.fatal(opErr(retry.OpAcquireJobs, statusError(403)))
	if h.d.table.TargetDraining("a") || tr.currentState() != targetErrorWait {
		t.Fatalf("drain mark %v, state %s; want no mark and the wait kept", h.d.table.TargetDraining("a"), tr.currentState())
	}
	fakes[0].setInstErr(nil)
	at := h.waitRetry(tr, fakes[0], 1, 30*time.Second)
	h.clock.Advance(at.Sub(h.clock.Now()))
	h.waitTargetState(tr, targetRunning)
	if h.d.table.TargetDraining("a") {
		t.Fatal("the target runs with its drain mark set")
	}
	h.exit()
}
