package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/dewevrei/actions-runner-scaler/internal/docker"
	"github.com/dewevrei/actions-runner-scaler/internal/gate"
	"github.com/dewevrei/actions-runner-scaler/internal/retry"
	"github.com/dewevrei/actions-runner-scaler/internal/scaler"
	"github.com/dewevrei/actions-runner-scaler/internal/status"
)

func blockUntilDone(ctx context.Context) error {
	<-ctx.Done()
	return ctx.Err()
}

func countID(ids []int64, id int64) int {
	n := 0
	for _, v := range ids {
		if v == id {
			n++
		}
	}
	return n
}

// The signal drain keeps the session and MaxRunners, calls no AcquireJobs
// (0002 SPEC R220 replaces R89), starts nothing, waits for busy and starting
// runners, then exits 0 (R20, R90, R91, R92).
func TestSignalDrain(t *testing.T) {
	h := newHarness(t)
	s1 := h.running()
	h.seedRunner("pool-00000001", 1, scaler.StateBusy)
	h.seedRunner("pool-00000002", 2, scaler.StateStarting)
	h.signal()
	h.waitState(status.Draining)

	s1.send(t, availableMsg(1, 11), nil)
	s1.send(t, statsMsg(2, 2), nil)
	waitFor(t, "acknowledged", func() bool { _, _, d, _ := s1.snapshot(); return len(d) == 2 })
	if n := h.calls.count("github.GenerateJITConfig"); n != 0 {
		t.Fatalf("a runner started during the drain (%d JIT calls)", n)
	}
	if n := h.calls.count("session1.AcquireJobs"); n != 0 {
		t.Fatalf("AcquireJobs called %d times during the signal drain (0002 SPEC R220)", n)
	}

	h.endRunner("pool-00000001")
	h.clock.Advance(5 * time.Second)
	if h.returned() {
		t.Fatal("drain ended while a runner was starting")
	}
	h.endRunner("pool-00000002")
	for !h.returned() {
		h.clock.Advance(5 * time.Second)
		waitFor(t, "a poll timer", func() bool { return h.returned() || h.clock.hasTimerAt(h.clock.Now().Add(5*time.Second)) })
	}
	if err := h.result(); err != nil {
		t.Fatalf("Run = %v", err)
	}
	_, caps, _, _ := s1.snapshot()
	for _, c := range caps {
		if c != h.cfg.Targets[0].MaxRunners {
			t.Fatalf("maxCapacity %v, want %d throughout (0002 R116)", caps, h.cfg.Targets[0].MaxRunners)
		}
	}
}

// TestListenerMaxCapacityOneTarget proves 0002 SPEC R116: the listener sends
// the target's max-runners as maxCapacity with every poll, also while
// draining.
func TestListenerMaxCapacityOneTarget(t *testing.T) {
	h := newHarness(t)
	h.cfg.Targets[0].MaxRunners = 3
	s1 := h.running()
	waitFor(t, "a poll", func() bool { polls, _, _, _ := s1.snapshot(); return polls > 0 })
	h.signal()
	h.waitState(status.Draining)
	for !h.returned() {
		h.clock.Advance(5 * time.Second)
		waitFor(t, "a poll timer", func() bool { return h.returned() || h.clock.hasTimerAt(h.clock.Now().Add(5*time.Second)) })
	}
	if err := h.result(); err != nil {
		t.Fatalf("Run = %v", err)
	}
	_, caps, _, _ := s1.snapshot()
	if len(caps) == 0 {
		t.Fatal("no poll was made")
	}
	for _, c := range caps {
		if c != 3 {
			t.Fatalf("maxCapacity %v, want 3 throughout", caps)
		}
	}
}

// A second signal finalizes at once: the busy runner's resources and
// registration are removed once (R21).
func TestSecondSignalFinalizesAtOnce(t *testing.T) {
	h := newHarness(t)
	h.running()
	h.seedRunner("pool-00000001", 1, scaler.StateBusy)
	h.signal()
	h.waitState(status.Draining)
	h.signal()
	if err := h.result(); err != nil {
		t.Fatalf("Run = %v", err)
	}
	if n := h.calls.count("docker.RemoveResources pool-00000001"); n != 1 {
		t.Errorf("RemoveResources = %d, want 1", n)
	}
	if got := h.gh.removedIDs(); !slices.Equal(got, []int64{1}) {
		t.Errorf("RemoveRunner = %v, want [1]", got)
	}
}

// A permanent error during the signal drain is logged only; the drain ends
// with exit 0, not the error wait (R100).
func TestSignalDrainPermanentErrorLoggedOnly(t *testing.T) {
	h := newHarness(t)
	s1 := h.running()
	h.seedRunner("pool-00000001", 1, scaler.StateBusy)
	h.signal()
	h.waitState(status.Draining)
	s1.send(t, nil, opErr(retry.OpGetMessage, statusError(403)))
	waitFor(t, "R100 log", func() bool { return strings.Contains(h.logs.String(), "R100") })
	h.endRunner("pool-00000001")
	for !h.returned() {
		h.clock.Advance(5 * time.Second)
		waitFor(t, "a poll timer", func() bool { return h.returned() || h.clock.hasTimerAt(h.clock.Now().Add(5*time.Second)) })
	}
	if err := h.result(); err != nil {
		t.Fatal(err)
	}
	if h.report().State == status.ErrorWait {
		t.Fatal("error wait after a signal drain")
	}
	if n := h.calls.count("github.CreateSession"); n != 1 {
		t.Fatalf("new session during the signal drain (%d)", n)
	}
}

// Without a session, a runner whose JobStarted never came is denied 180 s
// after it became busy (R81, R92); it then no longer holds the drain, and
// finalize removes what the deny path left.
func TestSignalDrainRunnerWithoutJobStartedRemovedAfter180s(t *testing.T) {
	h := newHarness(t)
	s1 := h.running()
	h.seedRunner("pool-00000001", 1, scaler.StateBusy)
	h.signal()
	h.waitState(status.Draining)
	s1.send(t, nil, opErr(retry.OpGetMessage, statusError(503)))
	waitFor(t, "session closed", func() bool { _, _, _, c := s1.snapshot(); return c == 1 })
	h.clock.Advance(179 * time.Second)
	if h.returned() || h.calls.count("docker.Remove") != 0 {
		t.Fatalf("runner removed or drain ended before 180 s: %v", h.calls.snapshot())
	}
	h.clock.Advance(time.Second)
	for !h.returned() {
		h.clock.Advance(5 * time.Second)
		waitFor(t, "a poll timer", func() bool { return h.returned() || h.clock.hasTimerAt(h.clock.Now().Add(5*time.Second)) })
	}
	if err := h.result(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(h.logs.String(), "R81") {
		t.Error("deny by the job check timer not logged")
	}
	if n := h.calls.count("docker.RemoveResources pool-00000001"); n != 1 {
		t.Errorf("RemoveResources = %d, want 1", n)
	}
}

// The error drain keeps a live Run, waits for busy and done runners, removes
// idle ones by the R53 rules, then finalizes and enters the error wait,
// after which nothing calls GitHub or Docker (R53, R54, R55, R47).
func TestErrorDrain(t *testing.T) {
	h := newHarness(t)
	s1 := h.running()
	h.seedRunner("pool-00000001", 1, scaler.StateBusy)
	h.seedRunner("pool-00000002", 2, scaler.StateDone)
	h.seedRunner("pool-00000005", 5, scaler.StateIdle)
	h.seedRunner("pool-00000006", 6, scaler.StateIdle)
	h.gh.removeErr = func(_ context.Context, id int64) error {
		switch id {
		case 5:
			return opErr(retry.OpRemoveRunner, retry.ErrJobStillRunning)
		case 6:
			return opErr(retry.OpRemoveRunner, statusError(500))
		}
		return nil
	}
	h.d.fatal(opErr(retry.OpAcquireJobs, statusError(403)))
	h.waitState(status.Draining)
	waitFor(t, "idle runners handled", func() bool {
		info, ok := h.d.table.Get("pool-00000005")
		return ok && info.State == scaler.StateBusy && h.calls.count("docker.RemoveContainer pool-00000006") == 1
	})
	if n := h.calls.count("docker.RemoveResources"); n != 0 {
		t.Fatalf("RemoveResources during the drain = %d, want container only (R53)", n)
	}
	// The session is kept while Run lives.
	s1.send(t, nil, nil)
	waitFor(t, "second poll", func() bool { p, _, _, _ := s1.snapshot(); return p >= 2 })

	for _, name := range []string{"pool-00000001", "pool-00000002"} {
		h.endRunner(name)
		h.clock.Advance(5 * time.Second)
		if h.report().State == status.ErrorWait {
			t.Fatalf("error wait while %s's peers were busy", name)
		}
	}
	h.endRunner("pool-00000005")
	for h.report().State != status.ErrorWait {
		h.clock.Advance(5 * time.Second)
		waitFor(t, "a poll timer or the error wait", func() bool {
			return h.report().State == status.ErrorWait || h.clock.hasTimerAt(h.clock.Now().Add(5*time.Second))
		})
	}
	h.status.mu.Lock()
	statusDown := h.status.shutdowns
	h.status.mu.Unlock()
	if statusDown != 0 {
		t.Fatal("status socket stopped before the error wait (R45)")
	}
	// Finalize removed the abandoned row of runner 6.
	if n := h.calls.count("docker.RemoveResources pool-00000006"); n != 1 {
		t.Errorf("finalize RemoveResources for the abandoned row = %d, want 1", n)
	}
	h.assertNoCallsFor(10 * time.Minute)
	h.signal()
	if err := h.result(); err != nil {
		t.Fatal(err)
	}
}

// A signal during the error drain finalizes and exits 0 (R21).
func TestSignalDuringErrorDrainExits0(t *testing.T) {
	h := newHarness(t)
	h.running()
	h.seedRunner("pool-00000001", 1, scaler.StateBusy)
	h.d.fatal(errors.New("permanent"))
	h.waitState(status.Draining)
	h.signal()
	if err := h.result(); err != nil {
		t.Fatal(err)
	}
	if h.report().State == status.ErrorWait {
		t.Fatal("error wait after a signal")
	}
	if n := h.calls.count("docker.RemoveResources pool-00000001"); n != 1 {
		t.Errorf("RemoveResources = %d, want 1", n)
	}
}

// A start path slow during the error drain finishes its docker undo before
// the error wait; after it nothing calls Docker (R47, PLAN M5-8 stage 4).
func TestErrorDrainWaitsForSlowStartPath(t *testing.T) {
	h := newHarness(t)
	s1 := h.running()
	release := make(chan struct{})
	h.docker.startErr = func(ctx context.Context, name string) error {
		<-ctx.Done()
		<-release // the docker undo, after cancellation
		return opErr(retry.OpContainerStart, ctx.Err())
	}
	s1.send(t, statsMsg(1, 1), nil)
	waitFor(t, "start", func() bool { return len(h.docker.startedNames()) == 1 })
	h.d.fatal(errors.New("permanent"))
	h.waitState(status.Draining)
	// The drain cannot finish a pass while Scale runs; it times out.
	h.clock.advanceTo(t, testEpoch.Add(h.cfg.Shutdown.DrainTimeout))
	t0 := h.clock.Now()
	h.clock.waitTimersAt(t, t0.Add(finalizeRunShare), finalizeTimersAtA)
	h.clock.advanceTo(t, t0.Add(finalizeRunShare))
	h.clock.waitTimersAt(t, t0.Add(finalizeRunShare+finalizeWaitShare), 1)
	h.clock.Advance(finalizeWaitShare - time.Second)
	if h.report().State == status.ErrorWait {
		t.Fatal("error wait while the start path was undoing")
	}
	close(release)
	h.waitState(status.ErrorWait)
	if got := h.gh.removedIDs(); countID(got, 100) != 1 {
		t.Errorf("RemoveRunner = %v, want runner 100 once", got)
	}
	h.assertNoCallsFor(10 * time.Minute)
	h.signal()
	if err := h.result(); err != nil {
		t.Fatal(err)
	}
}

// Every stage gets its own share: with every call hanging and Run never
// returning, stage 5's calls and the session close still happen with their
// full bounds, in parallel, and finalize ends 55 s after it began.
func TestFinalizeStageBudgets(t *testing.T) {
	h := newHarness(t)
	s1 := h.running()
	stuck := make(chan struct{})
	defer close(stuck)
	s1.mu.Lock()
	s1.stuck = stuck
	s1.mu.Unlock()
	s1.send(t, nil, nil)
	waitFor(t, "stuck poll", func() bool { p, _, _, _ := s1.snapshot(); return p == 2 })

	h.seedRunner("pool-00000001", 1, scaler.StateBusy)
	h.seedRunner("pool-00000002", 2, scaler.StateBusy)
	h.docker.removeErr = func(ctx context.Context, _ string) error {
		return opErr(retry.OpContainerRemove, blockUntilDone(ctx))
	}
	h.gh.removeErr = func(ctx context.Context, _ int64) error {
		return opErr(retry.OpRemoveRunner, blockUntilDone(ctx))
	}
	s1.closeFn = blockUntilDone
	h.jobs.shutdownFn = blockUntilDone

	h.signal()
	h.waitState(status.Draining)
	t0 := h.clock.Now()
	h.signal()
	waitFor(t, "finalize", func() bool { return h.d.table.Finalizing() })

	h.clock.waitTimersAt(t, t0.Add(finalizeRunShare), finalizeTimersAtA)
	h.clock.advanceTo(t, t0.Add(15*time.Second)) // stage 3: Run never returns
	h.clock.advanceTo(t, t0.Add(25*time.Second)) // stage 4
	waitFor(t, "parallel RemoveResources", func() bool { return h.calls.count("docker.RemoveResources") == 2 })
	h.clock.waitTimersAt(t, t0.Add(45*time.Second), 1) // stage 5's share
	h.clock.advanceTo(t, t0.Add(35*time.Second))
	waitFor(t, "parallel RemoveRunner", func() bool { return len(h.gh.removedIDs()) == 2 })
	h.clock.advanceTo(t, t0.Add(45*time.Second))
	waitFor(t, "session close", func() bool { return h.calls.count("session1.Close") == 1 })
	h.clock.advanceTo(t, t0.Add(53*time.Second))
	h.clock.advanceTo(t, t0.Add(55*time.Second))
	if err := h.result(); err != nil {
		t.Fatal(err)
	}
	if took := h.clock.Now().Sub(t0); took > 55*time.Second {
		t.Fatalf("finalize took %v, want at most 55 s", took)
	}
	near := func(what string, got []time.Duration, want time.Duration) {
		t.Helper()
		if len(got) == 0 {
			t.Errorf("%s: no call", what)
		}
		for _, d := range got {
			if d <= want-time.Second || d > want {
				t.Errorf("%s deadline %v, want %v", what, d, want)
			}
		}
	}
	near("RemoveResources", h.docker.removeLefts(), finalizeCallShare)
	near("RemoveRunner", h.gh.removeLefts(), finalizeCallShare)
	near("Close", s1.closeLeft, finalizeCloseShare)
	near("Shutdown", h.jobs.shutdownLeft, finalizeShutdownShare)
	// The status socket stops in stage 7 too, within the same share, so the
	// exit stays within 55 s.
	near("status Shutdown", h.status.shutdownLeft, finalizeShutdownShare)
	if n := h.status.shutdowns; n != 1 {
		t.Errorf("status shutdowns = %d, want 1", n)
	}
}

// Finalize tries RemoveRunner once for each pending registration, and once
// only for an ID that is also a row's; logs never show the JIT config (R31).
func TestFinalizePendingUnregistrations(t *testing.T) {
	h := newHarness(t)
	s1 := h.running()
	var removes int
	h.docker.startErr = func(context.Context, string) error {
		return opErr(retry.OpContainerStart, statusError(500))
	}
	h.docker.removeErr = func(context.Context, string) error {
		removes++
		if removes == 1 {
			return nil
		}
		return opErr(retry.OpContainerRemove, statusError(400))
	}
	h.gh.removeErr = func(context.Context, int64) error { return opErr(retry.OpRemoveRunner, statusError(503)) }
	s1.send(t, statsMsg(1, 1), nil)
	waitFor(t, "first start cleaned", func() bool { return countID(h.gh.removedIDs(), 100) == 1 })
	s1.send(t, statsMsg(2, 1), nil)
	waitFor(t, "second start cleaned", func() bool { return countID(h.gh.removedIDs(), 101) == 1 })
	if got := h.targetRuns()[0].scaler.PendingUnregistrations(); len(got) != 2 {
		t.Fatalf("pending = %v, want 2", got)
	}

	h.signal()
	if err := h.result(); err != nil {
		t.Fatal(err)
	}
	ids := h.gh.removedIDs()
	if countID(ids, 100) != 2 || countID(ids, 101) != 2 {
		t.Fatalf("RemoveRunner = %v, want each ID once more in finalize", ids)
	}
	if strings.Contains(h.logs.String(), "jit-secret") {
		t.Fatal("a JIT config reached the log")
	}
}

// Before RemoveRunner's allowed time finalize does not call it.
func TestFinalizeRespectsRemoveRunnerRateLimit(t *testing.T) {
	h := newHarness(t)
	h.running()
	h.seedRunner("pool-00000001", 1, scaler.StateBusy)
	h.targetRuns()[0].limits.Record(retry.OpRemoveRunner, &retry.RateLimitError{RetryAfter: time.Hour})
	h.signal()
	h.waitState(status.Draining)
	h.signal()
	if err := h.result(); err != nil {
		t.Fatal(err)
	}
	if n := h.calls.count("docker.RemoveResources pool-00000001"); n != 1 {
		t.Errorf("RemoveResources = %d, want 1", n)
	}
	if got := h.gh.removedIDs(); len(got) != 0 {
		t.Errorf("RemoveRunner = %v, want none before the allowed time", got)
	}
}

// A runner whose deny-path container removal keeps failing, and an abandoned
// row, are removed once by finalize; the deny retries stop.
func TestFinalizeRemovesDenyRetryAndAbandonedRows(t *testing.T) {
	h := newHarness(t)
	h.running()
	h.seedRunner("pool-00000001", 1, scaler.StateBusy)
	h.seedRunner("pool-00000003", 3, scaler.StateIdle)
	h.d.table.BeginExit("pool-00000003")
	h.d.table.AbandonExit("pool-00000003", scaler.AbandonPermanent)
	h.docker.containerErr = opErr(retry.OpContainerRemove, statusError(500))
	h.signal()
	h.waitState(status.Draining)
	h.clock.Advance(100 * time.Second)
	// A later busy runner keeps the drain going while the deny path retries.
	h.seedRunner("pool-00000002", 2, scaler.StateBusy)
	waits := h.calls.count("docker.Wait pool-00000001")
	h.clock.Advance(80 * time.Second)
	// The container keeps running, so the deny path removes it once its
	// 30 s stop grace ends. The deny path sets the grace timer in its own
	// goroutine, before it calls Wait, so a new Wait call shows the timer
	// is set. Waiting for any timer at the grace end would not do: the
	// reconciler's 30 s tick is due at that same instant, and moving the
	// clock on it alone would push the grace timer 30 s later.
	waitFor(t, "the deny path's wait", func() bool { return h.calls.count("docker.Wait pool-00000001") > waits })
	h.clock.Advance(30 * time.Second)
	waitFor(t, "deny path", func() bool { return h.calls.count("docker.RemoveContainer pool-00000001") >= 1 })
	// The removal runs in the deny path's own goroutine, so its retry timer
	// may not be set yet.
	h.clock.advanceTo(t, h.clock.Now().Add(2*time.Second))
	waitFor(t, "deny retry", func() bool { return h.calls.count("docker.RemoveContainer pool-00000001") >= 2 })
	h.signal()
	if err := h.result(); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"pool-00000001", "pool-00000003"} {
		if n := h.calls.count("docker.RemoveResources " + name); n != 1 {
			t.Errorf("RemoveResources %s = %d, want 1", name, n)
		}
	}
	if got := h.gh.removedIDs(); !slices.Contains(got, 1) || !slices.Contains(got, 3) {
		t.Errorf("RemoveRunner = %v, want 1 and 3", got)
	}
	before := h.calls.count("docker.RemoveContainer")
	h.clock.Advance(time.Minute)
	if after := h.calls.count("docker.RemoveContainer"); after != before {
		t.Errorf("deny retries went on after finalize (%d → %d)", before, after)
	}
}

// A signal drain that reaches drain-timeout finalizes and exits 0 (R92).
func TestSignalDrainTimeout(t *testing.T) {
	h := newHarness(t)
	h.running()
	h.seedRunner("pool-00000001", 1, scaler.StateBusy)
	// Allowed, so the 180 s timer leaves it running.
	h.d.table.Allow("pool-00000001")
	h.signal()
	h.waitState(status.Draining)
	h.clock.advanceTo(t, testEpoch.Add(h.cfg.Shutdown.DrainTimeout))
	if err := h.result(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(h.logs.String(), "drain timed out") {
		t.Error("timeout not logged")
	}
	if n := h.calls.count("docker.RemoveResources pool-00000001"); n != 1 {
		t.Errorf("RemoveResources = %d, want 1", n)
	}
}

// Scale set the error drain (before calling Fatal) and then a signal came:
// that is a signal during an error drain, so finalize and exit 0 (R21).
func TestSignalAfterScaleSetErrorDrain(t *testing.T) {
	h := newHarness(t)
	h.running()
	h.seedRunner("pool-00000001", 1, scaler.StateBusy)
	h.d.table.SetDraining(scaler.DrainError)
	h.signal()
	if err := h.result(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(h.logs.String(), "signal during the error drain; finalizing") {
		t.Error("branch not taken")
	}
	if h.report().State == status.ErrorWait {
		t.Fatal("error wait after a signal")
	}
	if n := h.calls.count("docker.RemoveResources pool-00000001"); n != 1 {
		t.Errorf("RemoveResources = %d, want 1", n)
	}
}

// An error drain that begins after Run returned makes no new session; idle
// runners are removed without messages and the busy one is waited for
// (R54, R53).
func TestErrorDrainAfterRunReturned(t *testing.T) {
	h := newHarness(t)
	s1 := h.running()
	h.seedRunner("pool-00000001", 1, scaler.StateBusy)
	h.seedRunner("pool-00000003", 3, scaler.StateIdle)
	s1.send(t, nil, opErr(retry.OpGetMessage, statusError(503)))
	waitFor(t, "session closed", func() bool { _, _, _, c := s1.snapshot(); return c == 1 })
	h.d.fatal(errors.New("permanent"))
	h.waitState(status.Draining)
	waitFor(t, "idle runner removed", func() bool {
		_, ok := h.d.table.Get("pool-00000003")
		return !ok
	})
	if got := h.gh.removedIDs(); !slices.Equal(got, []int64{3}) {
		t.Fatalf("RemoveRunner = %v, want [3]", got)
	}
	h.clock.Advance(time.Minute)
	if n := h.calls.count("github.CreateSession"); n != 1 {
		t.Fatalf("sessions = %d, want no new one", n)
	}
	h.endRunner("pool-00000001")
	for h.report().State != status.ErrorWait {
		h.clock.Advance(5 * time.Second)
		waitFor(t, "a poll timer or the error wait", func() bool {
			return h.report().State == status.ErrorWait || h.clock.hasTimerAt(h.clock.Now().Add(5*time.Second))
		})
	}
	h.assertNoCallsFor(10 * time.Minute)
	h.signal()
	if err := h.result(); err != nil {
		t.Fatal(err)
	}
}

// Event file bodies: a pull request from the run's own repository and one
// from a fork.
const (
	sameRepoEvent = `{"repository":{"id":7},"pull_request":{"head":{"repo":{"id":7}},"base":{"repo":{"id":7}}}}`
	forkEvent     = `{"repository":{"id":7},"pull_request":{"head":{"repo":{"id":8,"full_name":"fork/repo"}},"base":{"repo":{"id":7}}}}`
)

// giveToken gives a seeded runner a job check token and returns it.
func (h *harness) giveToken(name string) string {
	h.t.Helper()
	tok, err := gate.NewTokenGenerator(nil).New()
	if err != nil {
		h.t.Fatal(err)
	}
	if !h.d.table.SetToken(name, tok) {
		h.t.Fatalf("SetToken(%s) = false", name)
	}
	return tok.Reveal()
}

// postEvent sends the hook's event report through a job check server over
// the authenticator the daemon gave NewJobServer.
func (h *harness) postEvent(name, token string, body io.Reader) *httptest.ResponseRecorder {
	h.t.Helper()
	h.mu.Lock()
	auth := h.jobAuth
	h.mu.Unlock()
	srv, err := gate.NewServer("127.0.0.1:0", auth, h.clock, nil)
	if err != nil {
		h.t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, gate.EventPath, body)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("X-Runner-Name", name)
	req.Header.Set("X-Event-Name", "pull_request")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	return rec
}

func (h *harness) assertVerdict(rec *httptest.ResponseRecorder, name string, want scaler.Verdict) {
	h.t.Helper()
	if rec.Code != http.StatusOK || rec.Body.String() != `{"verdict":"`+want.String()+`"}` {
		h.t.Errorf("%s: got %d %q, want 200 %s", name, rec.Code, rec.Body.String(), want)
	}
	if info, _ := h.d.table.Get(name); info.Verdict != want {
		h.t.Errorf("%s: table verdict %v, want %v", name, info.Verdict, want)
	}
}

// TestEventJudgedWhileSessionDown: while session creation fails in the R25
// backoff, and during a signal drain without a session, an event report
// still gets its verdict (0002 SPEC R133).
func TestEventJudgedWhileSessionDown(t *testing.T) {
	t.Run("session backoff", func(t *testing.T) {
		h := newHarness(t)
		s1 := h.running()
		h.gh.mu.Lock()
		h.gh.createErr = func(int) error { return opErr(retry.OpSessionCreate, statusError(503)) }
		h.gh.mu.Unlock()
		s1.send(t, nil, opErr(retry.OpGetMessage, statusError(503)))
		waitFor(t, "session closed", func() bool { _, _, _, c := s1.snapshot(); return c == 1 })
		h.clock.advanceTo(t, testEpoch.Add(time.Second))
		waitFor(t, "a failed session create", func() bool { return h.calls.count("github.CreateSession") >= 2 })

		h.seedRunner("pool-00000001", 1, scaler.StateIdle)
		h.seedRunner("pool-00000002", 2, scaler.StateIdle)
		allowTok, denyTok := h.giveToken("pool-00000001"), h.giveToken("pool-00000002")
		h.assertVerdict(h.postEvent("pool-00000001", allowTok, strings.NewReader(sameRepoEvent)), "pool-00000001", scaler.VerdictAllow)
		h.assertVerdict(h.postEvent("pool-00000002", denyTok, strings.NewReader(forkEvent)), "pool-00000002", scaler.VerdictDeny)
		if r := h.report(); r.State != status.Running {
			t.Errorf("state = %s, want running", r.State)
		}
	})
	t.Run("signal drain without a session", func(t *testing.T) {
		h := newHarness(t)
		s1 := h.running()
		// The runner took a job whose JobStarted never came, so the table
		// still lists it idle. The drain's RemoveRunner then gets
		// JobStillRunningError and keeps it as busy (R91), or the report
		// marks it busy first (0002 SPEC R138); either way its token stays.
		// Without that the drain removes the idle runner and revokes its
		// token, often before the report.
		h.gh.mu.Lock()
		h.gh.removeErr = func(context.Context, int64) error {
			return opErr(retry.OpRemoveRunner, retry.ErrJobStillRunning)
		}
		h.gh.mu.Unlock()
		h.seedRunner("pool-00000001", 1, scaler.StateIdle)
		tok := h.giveToken("pool-00000001")
		h.signal()
		h.waitState(status.Draining)
		s1.send(t, nil, opErr(retry.OpGetMessage, statusError(503)))
		waitFor(t, "session closed", func() bool { _, _, _, c := s1.snapshot(); return c == 1 })

		h.assertVerdict(h.postEvent("pool-00000001", tok, strings.NewReader(sameRepoEvent)), "pool-00000001", scaler.VerdictAllow)
		if n := h.calls.count("github.CreateSession"); n != 1 {
			t.Errorf("sessions = %d, want no new one during the signal drain", n)
		}
	})
}

// TestEventMakesNoGitHubCalls: from the event report to its verdict nothing
// calls GitHub, for an allow and for a deny (0002 SPEC R123).
func TestEventMakesNoGitHubCalls(t *testing.T) {
	h := newHarness(t)
	h.running()
	h.seedRunner("pool-00000001", 1, scaler.StateIdle)
	h.seedRunner("pool-00000002", 2, scaler.StateIdle)
	allowTok, denyTok := h.giveToken("pool-00000001"), h.giveToken("pool-00000002")
	before := h.calls.count("github.")

	h.assertVerdict(h.postEvent("pool-00000001", allowTok, strings.NewReader(sameRepoEvent)), "pool-00000001", scaler.VerdictAllow)
	// The runner container runs, so the deny path waits for it on the
	// fake clock and removes nothing yet.
	h.assertVerdict(h.postEvent("pool-00000002", denyTok, strings.NewReader(forkEvent)), "pool-00000002", scaler.VerdictDeny)

	if n := h.calls.count("github.") - before; n != 0 {
		t.Errorf("GitHub calls from report to verdict = %d, want 0: %v", n, h.calls.snapshot())
	}
	if !strings.Contains(h.logs.String(), `"head_repo":"fork/repo"`) {
		t.Errorf("deny log lacks head_repo:\n%s", h.logs)
	}
}

// cutBody is a request body that ends before its declared length.
type cutBody struct{}

func (cutBody) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }

// TestEventFirstPostArmsJobCheckTimer: the first authenticated POST marks
// the runner busy and arms the 180 s timer even when its body is cut short
// and nothing is recorded; the full report then allows and stops the timer
// (0002 SPEC R137, R138).
func TestEventFirstPostArmsJobCheckTimer(t *testing.T) {
	h := newHarness(t)
	h.running()
	h.seedRunner("pool-00000001", 1, scaler.StateIdle)
	tok := h.giveToken("pool-00000001")
	busyAt := h.clock.Now()
	if h.clock.hasTimerAt(busyAt.Add(180 * time.Second)) {
		t.Fatal("timer armed before the report")
	}

	func() {
		defer func() {
			if r := recover(); r != http.ErrAbortHandler {
				t.Errorf("cut body: panic %v, want http.ErrAbortHandler", r)
			}
		}()
		h.postEvent("pool-00000001", tok, cutBody{})
	}()
	info, _ := h.d.table.Get("pool-00000001")
	if info.State != scaler.StateBusy || !info.BusyAt.Equal(busyAt) || info.Verdict != scaler.VerdictPending {
		t.Fatalf("row after a cut report = %v busy at %v verdict %v; want busy now, pending", info.State, info.BusyAt, info.Verdict)
	}
	if !h.clock.hasTimerAt(busyAt.Add(180 * time.Second)) {
		t.Fatal("no job check timer at busy + 180 s")
	}

	h.clock.Advance(time.Second)
	h.assertVerdict(h.postEvent("pool-00000001", tok, strings.NewReader(sameRepoEvent)), "pool-00000001", scaler.VerdictAllow)
	if h.clock.hasTimerAt(busyAt.Add(180 * time.Second)) {
		t.Error("job check timer still armed after the allow")
	}
}

// printed runs status.Print on the served report, as the status command
// does, and returns its lines and Print's error.
func (h *harness) printed() ([]string, error) {
	h.t.Helper()
	var b bytes.Buffer
	err := status.Print(&b, h.report())
	return strings.Split(strings.TrimSuffix(b.String(), "\n"), "\n"), err
}

// targetLine is a target's line as 0002 SPEC R180 writes it, without the
// error and retry parts.
func targetLine(url, word string, busy, idle, waiting, max, failures int) string {
	return fmt.Sprintf("%s: %s, 실행 중 %d, 대기 %d, 대기 job %d, 최대 %d, 연속 시작 실패 %d",
		url, word, busy, idle, waiting, max, failures)
}

func rfc3339(at time.Time) string { return at.Format(time.RFC3339) }

// finishDrain drives the clock until Run returned after a signal and checks
// that every target goroutine stopped.
func (h *harness) finishDrain() {
	h.t.Helper()
	h.driveUntil("Run returned", h.returned)
	if err := h.result(); err != nil {
		h.t.Fatalf("Run = %v, want nil", err)
	}
	h.assertTargetsStopped()
}

// TestStatusTargetStates proves 0002 SPEC R180: each target state has its
// word and line, an error drain or wait line carries the target's last
// error, and an error wait line carries the next retry. A target stopped in
// its start phase is shown as running (0002 PLAN M15).
func TestStatusTargetStates(t *testing.T) {
	t.Run("starting and running", func(t *testing.T) {
		h := newHarness(t)
		fakes := h.useTargets("a", "b")
		fakes[0].setErr = func(int) error { return opErr(retry.OpGetOrCreateScaleSet, statusError(503)) }
		h.start()
		fakes[1].polled(t, 0)
		h.waitState(status.Running)
		trs := h.targetRuns()
		h.waitTargetState(trs[0], targetStarting)
		lines, err := h.printed()
		if err != nil {
			t.Fatalf("Print = %v", err)
		}
		if lines[0] != "상태: 정상" || lines[1] != targetLine("https://github.com/acme/a", "시작 중", 0, 0, 0, 2, 0) ||
			lines[2] != targetLine("https://github.com/acme/b", "정상", 0, 0, 0, 2, 0) {
			t.Errorf("lines = %q", lines)
		}
		if r := h.report(); r.Targets[0].State != status.TargetStarting || r.Targets[0].NextRetryAt != nil {
			t.Errorf("a = %+v, want starting without a retry time", r.Targets[0])
		}
		h.exit()
	})

	t.Run("error wait", func(t *testing.T) {
		h := newHarness(t)
		fakes := h.useTargets("a", "b")
		fakes[0].instErr = func(int) error { return notInstalled("a") }
		h.start()
		fakes[1].polled(t, 0)
		trs := h.targetRuns()
		h.waitTargetState(trs[0], targetErrorWait)
		retryAt := h.waitRetry(trs[0], fakes[0], 1, 30*time.Second)
		lines, err := h.printed()
		if err != nil {
			t.Fatalf("Print = %v", err)
		}
		r := h.report()
		a := r.Targets[0]
		if !strings.Contains(a.LastError, "GitHub App is not installed on repository acme/a") || a.LastErrorAt.IsZero() {
			t.Fatalf("a = %+v, want the last error and its time", a)
		}
		want := targetLine("https://github.com/acme/a", "오류 대기", 0, 0, 0, 2, 0) +
			fmt.Sprintf(", 오류: %s (%s), 다음 시도 %s", a.LastError, rfc3339(a.LastErrorAt), rfc3339(retryAt))
		if lines[0] != "상태: 정상" || lines[1] != want {
			t.Errorf("lines = %q, want %q after the first", lines, want)
		}
		h.exit()
	})

	t.Run("error drain", func(t *testing.T) {
		h := newHarness(t)
		fakes := h.useTargets("a", "b")
		h.cfg.Shutdown.DrainTimeout = time.Minute
		h.start()
		sa := fakes[0].polled(t, 0)
		fakes[1].polled(t, 0)
		trs := h.targetRuns()
		h.seedRunnerOf(trs[0], "pool-000000a1", 1, scaler.StateBusy)
		sa.send(t, nil, permanentPoll())
		waitFor(t, "a's error drain and its error", func() bool {
			return trs[0].currentState() == targetErrorDrain && h.report().Targets[0].LastError != ""
		})
		lines, err := h.printed()
		if err != nil {
			t.Fatalf("Print = %v", err)
		}
		a := h.report().Targets[0]
		if !strings.Contains(a.LastError, "target error drain") || a.NextRetryAt != nil {
			t.Fatalf("a = %+v, want the drain error and no retry time", a)
		}
		want := targetLine("https://github.com/acme/a", "오류 drain 중", 1, 0, 0, 2, 0) +
			fmt.Sprintf(", 오류: %s (%s)", a.LastError, rfc3339(a.LastErrorAt))
		if lines[0] != "상태: 정상" || lines[1] != want {
			t.Errorf("lines = %q, want %q after the first", lines, want)
		}
		h.exit()
	})

	t.Run("stopped in the start phase", func(t *testing.T) {
		h := newHarness(t)
		fakes := h.useTargets("a", "b")
		fakes[0].createErr = func(int) error { return opErr(retry.OpSessionCreate, statusError(503)) }
		h.start()
		fakes[1].polled(t, 0)
		h.waitState(status.Running)
		trs := h.targetRuns()
		waitFor(t, "a's session backoff", func() bool { return h.calls.count("a.CreateSession") == 1 })
		h.signal()
		h.waitState(status.Draining)
		h.waitTargetState(trs[0], targetStopped)
		lines, err := h.printed()
		if err != nil {
			t.Fatalf("Print = %v", err)
		}
		if lines[0] != "상태: drain 중" || lines[1] != targetLine("https://github.com/acme/a", "정상", 0, 0, 0, 2, 0) {
			t.Errorf("lines = %q, want the stopped target shown as 정상", lines)
		}
		h.finishDrain()
	})
}

// TestTargetStateStatusMapping proves 0002 PLAN M15: a stopped target is
// shown as running while the server is running or draining (R197 must not
// read it) and as starting while the server is starting (R58, R198); the
// other target states do not depend on the server state.
func TestTargetStateStatusMapping(t *testing.T) {
	servers := []status.State{status.Starting, status.Running, status.Draining, status.ErrorWait}
	for _, server := range servers {
		want := status.TargetRunning
		if server == status.Starting {
			want = status.TargetStarting
		}
		if got := targetStopped.statusState(server); got != want {
			t.Errorf("stopped with server %s = %s, want %s", server, got, want)
		}
		for st, want := range map[targetState]status.TargetState{
			targetStarting:   status.TargetStarting,
			targetRunning:    status.TargetRunning,
			targetErrorDrain: status.TargetErrorDraining,
			targetErrorWait:  status.TargetErrorWait,
		} {
			if got := st.statusState(server); got != want {
				t.Errorf("%s with server %s = %s, want %s", st, server, got, want)
			}
		}
	}
}

// TestStatusStoppedTargetBeforeFirstSession proves 0002 PLAN M15: a signal
// before any first session stops the start phase of every target while the
// server state is still starting, and status shows them as starting.
func TestStatusStoppedTargetBeforeFirstSession(t *testing.T) {
	h := newHarness(t)
	fakes := h.useTargets("a", "b")
	for _, f := range fakes {
		f.setErr = func(int) error { return opErr(retry.OpGetOrCreateScaleSet, statusError(503)) }
	}
	h.start()
	trs := h.targetRuns()
	for _, tr := range trs {
		h.waitTargetState(tr, targetStarting)
	}
	h.signal()
	if err := h.result(); err != nil {
		t.Fatalf("Run = %v, want nil", err)
	}
	if h.report().State != status.Starting {
		t.Fatalf("server state = %s, want starting", h.report().State)
	}
	for _, tr := range trs {
		if s := tr.currentState(); s != targetStopped {
			t.Fatalf("%s state = %s, want stopped", tr.t.Key(), s)
		}
		if got := tr.TargetStatus().State; got != status.TargetStarting {
			t.Errorf("%s status = %s, want starting", tr.t.Key(), got)
		}
	}
	h.assertTargetsStopped()
}

// TestStatusCounts proves the 0001 counting rules in a target's line (0002
// SPEC R180, 0002 PLAN M15): busy is the busy and done runners, idle is the
// starting and idle ones, and a runner whose removal began is left out.
func TestStatusCounts(t *testing.T) {
	h := newHarness(t)
	fakes := h.useTargets("a", "b")
	h.start()
	fakes[0].polled(t, 0)
	fakes[1].polled(t, 0)
	trs := h.targetRuns()
	h.seedRunnerOf(trs[0], "pool-000000a1", 1, scaler.StateStarting)
	h.seedRunnerOf(trs[0], "pool-000000a2", 2, scaler.StateIdle)
	h.seedRunnerOf(trs[0], "pool-000000a3", 3, scaler.StateIdle)
	h.seedRunnerOf(trs[0], "pool-000000a4", 4, scaler.StateBusy)
	h.seedRunnerOf(trs[0], "pool-000000a5", 5, scaler.StateDone)
	h.seedRunnerOf(trs[0], "pool-000000a6", 6, scaler.StateDone)
	h.seedRunnerOf(trs[0], "pool-000000a7", 7, scaler.StateIdle)
	h.seedRunnerOf(trs[1], "pool-000000b1", 8, scaler.StateBusy)
	if !h.d.table.BeginExit("pool-000000a7") {
		t.Fatal("BeginExit refused")
	}
	if c := h.d.table.Counts(trs[0].t.Key()); c.Exiting != 1 {
		t.Fatalf("counts = %+v, want one exiting runner", c)
	}
	r := h.report()
	if a := r.Targets[0]; a.Busy != 1+2 || a.Idle != 1+2 {
		t.Errorf("a = busy %d idle %d, want busy + done = 3 and starting + idle = 3 (exiting left out)", a.Busy, a.Idle)
	}
	if b := r.Targets[1]; b.Busy != 1 || b.Idle != 0 {
		t.Errorf("b = busy %d idle %d, want busy 1 (another target's rows are not counted)", b.Busy, b.Idle)
	}
	h.exit()
}

// TestStatusServerLimitAndMaxRunners proves 0002 SPEC R152 and R205:
// serverLimit is in the JSON, each target line and JSON object shows that
// target's max-runners, and the 전체 line sums the target lines and ends
// with the server limit.
func TestStatusServerLimitAndMaxRunners(t *testing.T) {
	h := newHarness(t)
	h.docker.info = docker.Capacity{NCPU: 10, MemTotal: 7936 << 20} // server limit 2
	fakes := h.useTargets("a", "b")
	h.cfg.Targets[0].MaxRunners = 3
	h.cfg.Targets[1].MaxRunners = 1
	h.start()
	fakes[0].polled(t, 0)
	fakes[1].polled(t, 0)
	trs := h.targetRuns()
	h.seedRunnerOf(trs[0], "pool-000000a1", 1, scaler.StateBusy)
	h.seedRunnerOf(trs[1], "pool-000000b1", 2, scaler.StateStarting)

	raw, err := json.Marshal(h.report())
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		ServerLimit *int `json:"serverLimit"`
		Targets     []struct {
			MaxRunners int `json:"maxRunners"`
		} `json:"targets"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if got.ServerLimit == nil || *got.ServerLimit != 2 || len(got.Targets) != 2 ||
		got.Targets[0].MaxRunners != 3 || got.Targets[1].MaxRunners != 1 {
		t.Errorf("JSON = %s, want serverLimit 2 and maxRunners 3 and 1", raw)
	}
	lines, err := h.printed()
	if err != nil {
		t.Fatalf("Print = %v", err)
	}
	if lines[1] != targetLine("https://github.com/acme/a", "정상", 1, 0, 0, 3, 0) ||
		lines[2] != targetLine("https://github.com/acme/b", "정상", 0, 1, 0, 1, 0) ||
		lines[3] != "전체: 실행 중 1, 대기 1, 최대 2" {
		t.Errorf("lines = %q", lines)
	}
	h.exit()
}

// TestStatusWaitingJobs proves 0002 SPEC R222: a job that finds the server
// full is a waiting job of its target, in the line and in the JSON, and
// the other target shows none.
func TestStatusWaitingJobs(t *testing.T) {
	h := newHarness(t)
	h.docker.info = docker.Capacity{NCPU: 10, MemTotal: 7936 << 20} // server limit 2
	fakes := h.useTargets("a", "b")
	h.start()
	sa := fakes[0].polled(t, 0)
	fakes[1].polled(t, 0)
	trs := h.targetRuns()
	h.seedRunnerOf(trs[1], "pool-000000b1", 1, scaler.StateBusy)
	h.seedRunnerOf(trs[1], "pool-000000b2", 2, scaler.StateBusy)

	sa.send(t, availableMsg(1, 11), nil)
	waitFor(t, "a's waiting job", func() bool { return h.report().Targets[0].WaitingJobs == 1 })
	lines, err := h.printed()
	if err != nil {
		t.Fatalf("Print = %v", err)
	}
	if lines[1] != targetLine("https://github.com/acme/a", "정상", 0, 0, 1, 2, 0) ||
		lines[2] != targetLine("https://github.com/acme/b", "정상", 2, 0, 0, 2, 0) {
		t.Errorf("lines = %q", lines)
	}
	raw, err := json.Marshal(h.report())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"waitingJobs":1`) {
		t.Errorf("JSON = %s, want waitingJobs 1", raw)
	}
	h.exit()
}

// TestStatusStartingShowsTargets proves 0002 SPEC R198: before any first
// session, status prints the target lines under 상태: 시작 중, without the
// 전체 line.
func TestStatusStartingShowsTargets(t *testing.T) {
	h := newHarness(t)
	fakes := h.useTargets("a", "b")
	for _, f := range fakes {
		f.setErr = func(int) error { return opErr(retry.OpGetOrCreateScaleSet, statusError(503)) }
	}
	h.start()
	trs := h.targetRuns()
	for _, tr := range trs {
		h.waitTargetState(tr, targetStarting)
	}
	lines, err := h.printed()
	if err != nil {
		t.Fatalf("Print = %v", err)
	}
	if r := h.report(); r.State != status.Starting {
		t.Fatalf("state = %s, want starting", r.State)
	}
	if len(lines) != 4 || lines[0] != "상태: 시작 중" ||
		lines[1] != targetLine("https://github.com/acme/a", "시작 중", 0, 0, 0, 2, 0) ||
		lines[2] != targetLine("https://github.com/acme/b", "시작 중", 0, 0, 0, 2, 0) ||
		!strings.HasPrefix(lines[3], "마지막 오류: ") {
		t.Errorf("lines = %q, want 상태, two target lines and 마지막 오류", lines)
	}
	// A signal before the first session ends Run at once (R57).
	h.signal()
	if err := h.result(); err != nil {
		t.Fatalf("Run = %v, want nil", err)
	}
	h.assertTargetsStopped()
}

// TestStatusNoRunningTarget proves 0002 SPEC R197: with every target in the
// target error wait the scaler is running, status prints 상태: 정상 target
// 없음 and fails with ErrNoRunningTarget, which makes the command exit 1,
// and the targets go on retrying.
func TestStatusNoRunningTarget(t *testing.T) {
	h := newHarness(t)
	fakes := h.useTargets("a", "b")
	for _, f := range fakes {
		f.instErr = func(int) error { return notInstalled(f.prefix) }
	}
	h.start()
	trs := h.targetRuns()
	for _, tr := range trs {
		h.waitTargetState(tr, targetErrorWait)
	}
	h.waitState(status.Running)
	lines, err := h.printed()
	if !errors.Is(err, status.ErrNoRunningTarget) || !status.Reported(err) {
		t.Fatalf("Print = %v, want ErrNoRunningTarget, a reported error (exit 1)", err)
	}
	if lines[0] != "상태: 정상 target 없음" || len(lines) != 5 || !strings.HasPrefix(lines[3], "전체: ") {
		t.Errorf("lines = %q, want the first line, two target lines, 전체 and 마지막 오류", lines)
	}
	for i, f := range fakes {
		h.waitRetry(trs[i], f, 1, 30*time.Second)
	}
	h.clock.Advance(30 * time.Second)
	for _, f := range fakes {
		waitFor(t, f.prefix+"'s retry", func() bool { return f.installations() == 2 })
	}
	h.exit()
}

// TestStatusOneTargetNotInstalled proves 0002 SPEC R257: with the App not
// installed on A's repository, A's line is 오류 대기 with the R237 text,
// B's line is 정상, the state is 정상, and B's job gets a runner.
func TestStatusOneTargetNotInstalled(t *testing.T) {
	h := newHarness(t)
	fakes := h.useTargets("a", "b")
	fakes[0].instErr = func(int) error { return notInstalled("a") }
	h.start()
	sb := fakes[1].polled(t, 0)
	trs := h.targetRuns()
	h.waitTargetState(trs[0], targetErrorWait)
	h.waitRetry(trs[0], fakes[0], 1, 30*time.Second)
	lines, err := h.printed()
	if err != nil {
		t.Fatalf("Print = %v, want exit 0", err)
	}
	if lines[0] != "상태: 정상" ||
		!strings.HasPrefix(lines[1], "https://github.com/acme/a: 오류 대기, ") ||
		!strings.Contains(lines[1], "GitHub App is not installed on repository acme/a") ||
		lines[2] != targetLine("https://github.com/acme/b", "정상", 0, 0, 0, 2, 0) {
		t.Errorf("lines = %q", lines)
	}
	h.gotRunner(fakes[1], sb, 1)
	h.exit()
}
