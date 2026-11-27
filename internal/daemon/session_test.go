package daemon

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/actions/scaleset"

	"github.com/dewevrei/actions-runner-scaler/internal/retry"
	"github.com/dewevrei/actions-runner-scaler/internal/scaler"
	"github.com/dewevrei/actions-runner-scaler/internal/status"
)

func statsMsg(id, assigned int) *scaleset.RunnerScaleSetMessage {
	return &scaleset.RunnerScaleSetMessage{
		MessageID:  id,
		Statistics: &scaleset.RunnerScaleSetStatistic{TotalAssignedJobs: assigned},
	}
}

func availableMsg(id int, reqIDs ...int64) *scaleset.RunnerScaleSetMessage {
	m := statsMsg(id, 0)
	for _, r := range reqIDs {
		m.JobAvailableMessages = append(m.JobAvailableMessages, &scaleset.JobAvailable{
			JobMessageBase: scaleset.JobMessageBase{RunnerRequestID: r},
		})
	}
	return m
}

// waitSessions waits until n sessions were made and the last one polled.
func (h *harness) waitSessions(n int) *fakeSession {
	h.t.Helper()
	waitFor(h.t, "session polled", func() bool {
		s := h.gh.session(n - 1)
		if s == nil {
			return false
		}
		polls, _, _, _ := s.snapshot()
		return polls > 0
	})
	return h.gh.session(n - 1)
}

// A transient end of Run and a lost session both close the session and make
// a new one after the backoff (R25, R99).
func TestSessionReconnects(t *testing.T) {
	for _, tt := range []struct {
		name string
		err  error
	}{
		{"transient", opErr(retry.OpGetMessage, statusError(503))},
		{"session lost", opErr(retry.OpGetMessage, statusError(404))},
		{"delete message lost", opErr(retry.OpDeleteMessage, statusError(404))},
	} {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t)
			s1 := h.running()
			if tt.name == "delete message lost" {
				s1.deleteFn = func(context.Context, int) error { return tt.err }
				s1.send(t, statsMsg(1, 0), nil)
			} else {
				s1.send(t, nil, tt.err)
			}
			waitFor(t, "session closed", func() bool { _, _, _, c := s1.snapshot(); return c == 1 })
			if left := s1.closeLeft[0]; left <= 9*time.Second || left > sessionCloseTimeout {
				t.Errorf("close deadline %v, want %v", left, sessionCloseTimeout)
			}
			if n := h.calls.count("github.CreateSession"); n != 1 {
				t.Fatalf("new session before the backoff (%d sessions)", n)
			}
			h.clock.advanceTo(t, testEpoch.Add(time.Second))
			h.waitSessions(2)
			if r := h.report(); r.State != status.Running {
				t.Errorf("state = %s, want running", r.State)
			}
		})
	}
}

// A permanent session error starts the target error drain, and with
// nothing to wait for, the target's cleanup and the target error wait
// follow; the scaler keeps running and the target makes no call before its
// first retry (0002 SPEC R171, R173~R175).
func TestSessionPermanentErrorStartsTargetErrorDrain(t *testing.T) {
	for _, tt := range []struct {
		name string
		run  func(h *harness, s1 *fakeSession)
	}{
		{"auth refused inside GetMessage", func(h *harness, s1 *fakeSession) {
			s1.send(h.t, nil, opErr(retry.OpGetMessage, retry.ErrAuth))
		}},
		{"session create 404", func(h *harness, s1 *fakeSession) {
			h.gh.createErr = func(n int) error {
				if n > 1 {
					return opErr(retry.OpSessionCreate, statusError(404))
				}
				return nil
			}
			s1.send(h.t, nil, opErr(retry.OpGetMessage, statusError(503)))
			h.clock.advanceTo(h.t, testEpoch.Add(time.Second))
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t)
			s1 := h.running()
			tt.run(h, s1)
			tr := h.targetRuns()[0]
			h.waitTargetState(tr, targetErrorWait)
			if r := h.report(); r.State != status.Running || !strings.HasPrefix(r.LastError, "target error drain") {
				t.Errorf("state %s, lastError = %q", r.State, r.LastError)
			}
			if _, _, _, closed := s1.snapshot(); closed != 1 {
				t.Errorf("session closed %d times, want 1", closed)
			}
			h.assertNoCallsBeforeRetry(tr)
			h.signal()
			if err := h.result(); err != nil {
				t.Fatalf("Run = %v", err)
			}
		})
	}
}

// A rate-limited AcquireJobs ends the listener: the message is not
// acknowledged, the session is closed, and the new session waits for the
// allowed time (R52). The message comes again on the new session and is
// acquired there.
func TestAcquireJobsRateLimitDelaysNewSession(t *testing.T) {
	h := newHarness(t)
	s1 := h.running()
	s1.acquireErr = func(int) error {
		return opErr(retry.OpAcquireJobs, &retry.RateLimitError{RetryAfter: 90 * time.Second})
	}
	s1.send(t, availableMsg(1, 11), nil)
	waitFor(t, "session closed", func() bool { _, _, _, closed := s1.snapshot(); return closed == 1 })
	if _, _, d, _ := s1.snapshot(); len(d) != 0 {
		t.Fatalf("acknowledged %v, want the message left for the next session", d)
	}
	// The session backoff takes the rate limit's wait, which is longer.
	at := testEpoch.Add(90 * time.Second)
	waitFor(t, "rate limit wait", func() bool { return h.clock.hasTimerAt(at) })
	h.clock.Advance(90*time.Second - time.Nanosecond)
	if n := h.calls.count("github.CreateSession"); n != 1 {
		t.Fatalf("new session before AcquireJobs was allowed (%d)", n)
	}
	h.clock.Advance(time.Nanosecond)
	s2 := h.waitSessions(2)
	s2.send(t, availableMsg(1, 11), nil) // the message again
	waitFor(t, "acquired on the new session", func() bool { return h.calls.count("session2.AcquireJobs") == 1 })
	waitFor(t, "acknowledged on the new session", func() bool { _, _, d, _ := s2.snapshot(); return len(d) == 1 })
}

// A failed close is only logged (R99); the loop goes on.
func TestSessionCloseFailureLogged(t *testing.T) {
	h := newHarness(t)
	s1 := h.running()
	s1.closeFn = func(context.Context) error { return errors.New("close refused") }
	s1.send(t, nil, opErr(retry.OpGetMessage, statusError(500)))
	h.clock.advanceTo(t, testEpoch.Add(time.Second))
	h.waitSessions(2)
	if !strings.Contains(h.logs.String(), "R99") {
		t.Error("close failure not logged")
	}
}

// A signal during the session backoff starts the signal drain and no new
// session is made (R20, R88).
func TestSignalDuringSessionBackoff(t *testing.T) {
	h := newHarness(t)
	s1 := h.running()
	s1.send(t, nil, opErr(retry.OpGetMessage, statusError(503)))
	waitFor(t, "backoff", func() bool { return h.clock.hasTimerAt(testEpoch.Add(time.Second)) })
	h.signal()
	if err := h.result(); err != nil {
		t.Fatalf("Run = %v", err)
	}
	h.clock.Advance(time.Minute)
	if n := h.calls.count("github.CreateSession"); n != 1 {
		t.Fatalf("sessions = %d, want no new one", n)
	}
}

// Fatal is idempotent: many calls start one drain (M5-5).
func TestFatalIdempotent(t *testing.T) {
	h := newHarness(t)
	h.running()
	h.seedRunner("pool-00000001", 1, scaler.StateBusy)
	for range 3 {
		h.d.fatal(errors.New("permanent"))
	}
	h.waitState(status.Draining)
	if n := strings.Count(h.logs.String(), "error drain started"); n != 1 {
		t.Fatalf("error drain started %d times", n)
	}
	h.signal()
	if err := h.result(); err != nil {
		t.Fatal(err)
	}
}

// A permanent AcquireJobs error starts one target error drain (0002 SPEC
// R171); the message is acknowledged, the session is kept while the drain
// works (R54, 0002 SPEC R173), and the target's later messages get no
// AcquireJobs (0002 SPEC R220).
func TestAcquireJobsPermanentRepeated(t *testing.T) {
	h := newHarness(t)
	s1 := h.running()
	h.seedRunner("pool-00000001", 1, scaler.StateBusy)
	s1.acquireErr = func(int) error { return opErr(retry.OpAcquireJobs, statusError(403)) }
	s1.send(t, availableMsg(1, 11), nil)
	h.waitTargetState(h.targetRuns()[0], targetErrorDrain)
	s1.send(t, availableMsg(2, 12), nil)
	waitFor(t, "both messages acknowledged", func() bool { _, _, d, _ := s1.snapshot(); return len(d) == 2 })
	if n := h.calls.count("session1.AcquireJobs"); n != 1 {
		t.Fatalf("AcquireJobs %d, want none after the target error drain began", n)
	}
	if _, _, _, closed := s1.snapshot(); closed != 0 {
		t.Fatalf("session closed %d times during the target error drain, want 0", closed)
	}
	if n := strings.Count(h.logs.String(), "target error drain started"); n != 1 {
		t.Fatalf("target error drain started %d times", n)
	}
	if r := h.report(); r.State != status.Running {
		t.Errorf("state = %s, want running (0002 SPEC R171)", r.State)
	}
	// The signal drain waits for the busy runner (0002 SPEC R179); the
	// second signal stops waiting (R21).
	h.signal()
	h.waitState(status.Draining)
	h.signal()
	if err := h.result(); err != nil {
		t.Fatal(err)
	}
}

// pollSession is a fake session whose GetMessage runs fn.
type pollSession struct {
	*fakeSession
	fn func(ctx context.Context) (*scaleset.RunnerScaleSetMessage, error)
}

func (s pollSession) GetMessage(ctx context.Context, _, _ int) (*scaleset.RunnerScaleSetMessage, error) {
	return s.fn(ctx)
}

// newPollClient returns the session wrapper over fn on clock, counting
// the polls it reports as answered in polled.
func newPollClient(clock scaler.Clock, polled *atomic.Int32,
	fn func(ctx context.Context) (*scaleset.RunnerScaleSetMessage, error)) *sessionClient {
	return newSessionClient(pollSession{fakeSession: newFakeSession(&calls{}, 1), fn: fn},
		scaler.NewTable(scaler.TableConfig{}), clock, discardLog(), context.Background(), func() { polled.Add(1) })
}

// heldPoll is a poll the server holds: it waits for the end of ctx and
// returns the adapter's error.
func heldPoll(ctx context.Context) (*scaleset.RunnerScaleSetMessage, error) {
	<-ctx.Done()
	return nil, opErr(retry.OpGetMessage, ctx.Err())
}

type pollResult struct {
	msg *scaleset.RunnerScaleSetMessage
	err error
}

// getMessageAsync calls c.GetMessage on its own goroutine.
func getMessageAsync(ctx context.Context, c *sessionClient) <-chan pollResult {
	done := make(chan pollResult, 1)
	go func() {
		msg, err := c.GetMessage(ctx, 0, 2)
		done <- pollResult{msg, err}
	}()
	return done
}

func waitPoll(t *testing.T, done <-chan pollResult) pollResult {
	t.Helper()
	select {
	case r := <-done:
		return r
	case <-time.After(failsafe):
		t.Fatal("GetMessage did not return")
		return pollResult{}
	}
}

// A poll ended by its parent, cancelled or past the parent's own deadline,
// returns its error and is not reported as answered.
func TestGetMessageParentEndedIsError(t *testing.T) {
	t.Run("cancelled", func(t *testing.T) {
		var polled atomic.Int32
		var started atomic.Bool
		c := newPollClient(newFakeClock(), &polled, func(ctx context.Context) (*scaleset.RunnerScaleSetMessage, error) {
			started.Store(true)
			return heldPoll(ctx)
		})
		parent, cancel := context.WithCancel(context.Background())
		done := getMessageAsync(parent, c)
		waitFor(t, "poll started", started.Load)
		cancel()
		r := waitPoll(t, done)
		if !errors.Is(r.err, context.Canceled) {
			t.Fatalf("GetMessage = %v, %v, want the cancellation", r.msg, r.err)
		}
		if n := polled.Load(); n != 0 {
			t.Fatalf("polled %d times, want 0", n)
		}
	})
	t.Run("parent deadline", func(t *testing.T) {
		var polled atomic.Int32
		c := newPollClient(newFakeClock(), &polled, heldPoll)
		parent, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
		defer cancel()
		r := waitPoll(t, getMessageAsync(parent, c))
		if !errors.Is(r.err, context.DeadlineExceeded) {
			t.Fatalf("GetMessage = %v, %v, want the parent's deadline", r.msg, r.err)
		}
		if n := polled.Load(); n != 0 {
			t.Fatalf("polled %d times, want 0", n)
		}
	})
}

// Any other error comes back unchanged, a deadline error from inside the
// call while the poll's own bound has not passed included.
func TestGetMessageOtherErrorReturned(t *testing.T) {
	for _, tt := range []struct {
		name string
		err  error
	}{
		{"server error", opErr(retry.OpGetMessage, statusError(503))},
		{"inner deadline before the bound", opErr(retry.OpGetMessage, fmt.Errorf("attempt: %w", context.DeadlineExceeded))},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var polled atomic.Int32
			c := newPollClient(newFakeClock(), &polled, func(context.Context) (*scaleset.RunnerScaleSetMessage, error) {
				return nil, tt.err
			})
			msg, err := c.GetMessage(context.Background(), 0, 2)
			if err != tt.err || msg != nil {
				t.Fatalf("GetMessage = %v, %v, want %v", msg, err, tt.err)
			}
			if n := polled.Load(); n != 0 {
				t.Fatalf("polled %d times, want 0", n)
			}
		})
	}
}

// The session wrapper bounds DeleteMessage at 30 s (R47).
func TestDeleteMessageDeadline(t *testing.T) {
	clock := newFakeClock()
	s := newFakeSession(&calls{}, 1)
	c := newSessionClient(s, scaler.NewTable(scaler.TableConfig{}), clock, discardLog(), context.Background(), nil)
	if err := c.DeleteMessage(context.WithoutCancel(context.Background()), 5); err != nil {
		t.Fatal(err)
	}
	if left := s.deleteLeft[0]; left <= 29*time.Second || left > deleteMessageTimeout {
		t.Fatalf("deadline %v, want %v", left, deleteMessageTimeout)
	}
	// The bound follows the injected clock too.
	s.deleteFn = func(ctx context.Context, int2 int) error { <-ctx.Done(); return ctx.Err() }
	done := make(chan error, 1)
	go func() { done <- c.DeleteMessage(context.Background(), 6) }()
	clock.advanceTo(t, testEpoch.Add(deleteMessageTimeout))
	if err := <-done; !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("hung DeleteMessage = %v, want it ended", err)
	}
}

// Once finalize began the wrapper makes no call (R47).
func TestDeleteMessageSkippedWhileFinalizing(t *testing.T) {
	s := newFakeSession(&calls{}, 1)
	tb := scaler.NewTable(scaler.TableConfig{})
	tb.BeginFinalize(testEpoch.Add(time.Minute))
	c := newSessionClient(s, tb, newFakeClock(), discardLog(), context.Background(), nil)
	if err := c.DeleteMessage(context.Background(), 5); !errors.Is(err, scaler.ErrFinalizing) {
		t.Fatalf("DeleteMessage = %v, want ErrFinalizing", err)
	}
	if _, _, d, _ := s.snapshot(); len(d) != 0 {
		t.Fatalf("DeleteMessage reached the session: %v", d)
	}
}

func TestDeleteMessageEndsAtDeadlineA(t *testing.T) {
	s := newFakeSession(&calls{}, 1)
	s.deleteFn = func(ctx context.Context, _ int) error { <-ctx.Done(); return ctx.Err() }
	a, cancelA := context.WithCancel(context.Background())
	c := newSessionClient(s, scaler.NewTable(scaler.TableConfig{}), newFakeClock(), discardLog(), a, nil)
	done := make(chan error, 1)
	go func() { done <- c.DeleteMessage(context.Background(), 5) }()
	waitFor(t, "delete called", func() bool { _, _, d, _ := s.snapshot(); return len(d) == 1 })
	cancelA()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("DeleteMessage = nil after deadline A")
		}
	case <-time.After(failsafe):
		t.Fatal("DeleteMessage did not end at deadline A")
	}
}

// A DeleteMessage that hangs when finalize begins ends at deadline A; the
// error wait follows only after Run returned, and then nothing calls GitHub
// (R47). No new session is made.
func TestHungDeleteMessageEndsAtDeadlineA(t *testing.T) {
	h := newHarness(t)
	s1 := h.running()
	s1.deleteFn = func(ctx context.Context, _ int) error { <-ctx.Done(); return ctx.Err() }
	s1.send(t, statsMsg(1, 0), nil)
	waitFor(t, "delete called", func() bool { _, _, d, _ := s1.snapshot(); return len(d) == 1 })

	h.d.fatal(errors.New("permanent"))
	a := testEpoch.Add(finalizeRunShare)
	h.clock.waitTimersAt(t, a, finalizeTimersAtA)
	h.clock.Advance(a.Sub(h.clock.Now()) - time.Nanosecond)
	if h.report().State == status.ErrorWait {
		t.Fatal("error wait before Run returned")
	}
	h.clock.Advance(time.Nanosecond)
	h.waitState(status.ErrorWait)
	select {
	case <-h.d.runDone:
	default:
		t.Fatal("error wait while Run had not returned")
	}
	if n := h.calls.count("github.CreateSession"); n != 1 {
		t.Fatalf("sessions = %d, want 1", n)
	}
	if n := h.calls.count("session1.Close"); n != 1 {
		t.Fatalf("session closes = %d, want 1 (finalize stage 6)", n)
	}
	h.assertNoCallsFor(10 * time.Minute)
	h.signal()
	if err := h.result(); err != nil {
		t.Fatal(err)
	}
}

func TestFailedOp(t *testing.T) {
	if got := failedOp(errors.New("x")); got != retry.OpGetMessage {
		t.Errorf("failedOp(plain) = %v", got)
	}
	err := errors.Join(errors.New("failed to scale"), opErr(retry.OpAcquireJobs, opErr(retry.OpSessionRefresh, statusError(404))))
	if got := failedOp(err); got != retry.OpAcquireJobs {
		t.Errorf("failedOp = %v, want the outermost op", got)
	}
}

// When the session's allowed time is later than AcquireJobs', the new
// session waits for the session's (R52).
func TestSessionRateLimitLaterThanAcquireJobs(t *testing.T) {
	h := newHarness(t)
	s1 := h.running()
	h.targetRuns()[0].limits.Record(retry.OpAcquireJobs, &retry.RateLimitError{RetryAfter: 90 * time.Second})
	h.targetRuns()[0].limits.Record(retry.OpSessionCreate, &retry.RateLimitError{RetryAfter: 200 * time.Second})
	s1.send(t, nil, opErr(retry.OpGetMessage, statusError(503)))
	at := testEpoch.Add(200 * time.Second)
	h.clock.advanceTo(t, testEpoch.Add(time.Second)) // the backoff
	waitFor(t, "session rate limit wait", func() bool { return h.clock.hasTimerAt(at) })
	h.clock.Advance(at.Sub(h.clock.Now()) - time.Nanosecond)
	if n := h.calls.count("github.CreateSession"); n != 1 {
		t.Fatalf("new session before the session's allowed time (%d)", n)
	}
	h.clock.Advance(time.Nanosecond)
	h.waitSessions(2)
}

// Scale ends with ErrFinalizing when finalize began before the listener's
// context is cancelled: the loop stops without classifying, closing or
// making a session, and the message is not acknowledged (R47).
func TestScaleErrFinalizingStopsLoop(t *testing.T) {
	h := newHarness(t)
	s1 := h.running()
	h.targetRuns()[0].scaler.BeginFinalize(h.clock.Now().Add(time.Minute))
	s1.send(t, statsMsg(1, 0), nil)
	select {
	case <-h.d.runDone:
	case <-time.After(failsafe):
		t.Fatal("session loop still running")
	}
	if h.d.listenCtx.Err() != nil {
		t.Fatal("listenCtx was cancelled; the test needs the gap before it")
	}
	if _, _, d, c := s1.snapshot(); len(d) != 0 || c != 0 {
		t.Fatalf("deleted %v, closes %d; want none", d, c)
	}
	if n := h.calls.count("github.CreateSession"); n != 1 {
		t.Fatalf("sessions = %d", n)
	}
	if !strings.Contains(h.logs.String(), "listener stopped for finalize") {
		t.Error("stop not logged")
	}
	h.signal()
	if err := h.result(); err != nil {
		t.Fatal(err)
	}
	if n := h.calls.count("session1.Close"); n != 1 {
		t.Fatalf("closes = %d, want 1 by finalize", n)
	}
}
