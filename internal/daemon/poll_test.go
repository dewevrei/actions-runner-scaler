package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/actions/scaleset"

	"github.com/dewevrei/actions-runner-scaler/internal/retry"
	"github.com/dewevrei/actions-runner-scaler/internal/status"
)

// discardLog returns a logger that drops everything.
func discardLog() *slog.Logger { return slog.New(slog.DiscardHandler) }

// capturedLog returns a JSON logger and the buffer it writes to.
func capturedLog() (*slog.Logger, *syncBuffer) {
	buf := &syncBuffer{}
	return slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})), buf
}

// logLines decodes the JSON log lines whose msg is msg.
func logLines(t *testing.T, buf *syncBuffer, msg string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if line == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("log line %q: %v", line, err)
		}
		if m["msg"] == msg {
			out = append(out, m)
		}
	}
	return out
}

// newLoggedPollClient is newPollClient with a log the test can read.
func newLoggedPollClient(clock *fakeClock, log *slog.Logger,
	fn func(ctx context.Context) (*scaleset.RunnerScaleSetMessage, error)) *sessionClient {
	var polled atomic.Int32
	c := newPollClient(clock, &polled, fn)
	c.log = log
	return c
}

// A poll that ends before its deadline passes its message through and is
// reported as answered; the call runs under a deadline of
// getMessageAnswerDeadline (PLAN M5-5, 0002 SPEC R153).
func TestGetMessageReturnsMessage(t *testing.T) {
	var polled atomic.Int32
	var left time.Duration
	c := newPollClient(newFakeClock(), &polled, func(ctx context.Context) (*scaleset.RunnerScaleSetMessage, error) {
		if d, ok := ctx.Deadline(); ok {
			left = time.Until(d)
		}
		return statsMsg(3, 1), nil
	})
	msg, err := c.GetMessage(context.Background(), 0, 2)
	if err != nil || msg == nil || msg.MessageID != 3 {
		t.Fatalf("GetMessage = %v, %v, want message 3", msg, err)
	}
	if left <= getMessageAnswerDeadline-time.Second || left > getMessageAnswerDeadline {
		t.Fatalf("poll deadline in %v, want %v", left, getMessageAnswerDeadline)
	}
	if n := polled.Load(); n != 1 {
		t.Fatalf("polled %d times, want 1", n)
	}
}

// A poll the server never answers ends at getMessageAnswerDeadline, not
// earlier: a transient get-message error that wraps
// context.DeadlineExceeded, with the WARN line (0002 SPEC R153).
func TestPollDeadlineIsTransient(t *testing.T) {
	clock := newFakeClock()
	log, buf := capturedLog()
	c := newLoggedPollClient(clock, log, heldPoll)
	start := clock.Now()
	done := getMessageAsync(context.Background(), c)
	deadline := start.Add(getMessageAnswerDeadline)
	clock.waitTimersAt(t, deadline, 1)
	clock.Advance(getMessageAnswerDeadline - time.Nanosecond)
	select {
	case r := <-done:
		t.Fatalf("poll ended before the deadline: %v, %v", r.msg, r.err)
	default:
	}
	clock.advanceTo(t, deadline)
	r := waitPoll(t, done)
	if r.msg != nil || r.err == nil {
		t.Fatalf("GetMessage = %v, %v, want an error", r.msg, r.err)
	}
	var oe *retry.OpError
	if !errors.As(r.err, &oe) || oe.Op != retry.OpGetMessage {
		t.Fatalf("error %v is not a get message OpError", r.err)
	}
	if !errors.Is(r.err, context.DeadlineExceeded) {
		t.Errorf("error %v does not wrap context.DeadlineExceeded", r.err)
	}
	if class := retry.Classify(context.Background(), retry.OpGetMessage, r.err); class != retry.Transient {
		t.Errorf("class = %v, want transient", class)
	}
	lines := logLines(t, buf, "message poll got no answer")
	if len(lines) != 1 || lines[0]["level"] != "WARN" {
		t.Fatalf("no-answer lines = %v, want one WARN", lines)
	}
	if got, want := lines[0]["deadline"], float64(getMessageAnswerDeadline); got != want {
		t.Errorf("deadline = %v, want %v", got, want)
	}
}

// A poll the parent ended is not a missing answer.
func TestPollParentEndedIsNotNoAnswer(t *testing.T) {
	clock := newFakeClock()
	log, buf := capturedLog()
	c := newLoggedPollClient(clock, log, heldPoll)
	ctx, cancel := context.WithCancel(context.Background())
	done := getMessageAsync(ctx, c)
	clock.waitTimersAt(t, clock.Now().Add(getMessageAnswerDeadline), 1)
	cancel()
	r := waitPoll(t, done)
	if !errors.Is(r.err, context.Canceled) {
		t.Fatalf("GetMessage error = %v, want context.Canceled", r.err)
	}
	if n := len(logLines(t, buf, "message poll got no answer")); n != 0 {
		t.Fatalf("%d no-answer lines for a cancelled poll", n)
	}
}

// With a healthy daemon, a poll the fake server never answers ends at
// getMessageAnswerDeadline, and no poll ends before: the WARN line, a new session after the 1 s
// backoff, and the error as the last error (0002 SPEC R153).
func TestPollDeadline(t *testing.T) {
	h := newHarness(t)
	s1 := h.running()
	deadline := h.clock.Now().Add(getMessageAnswerDeadline)
	h.clock.waitTimersAt(t, deadline, 1)
	h.clock.Advance(getMessageAnswerDeadline - time.Nanosecond)
	if polls, _, _, _ := s1.snapshot(); polls != 1 {
		t.Fatalf("%d polls before the deadline, want 1", polls)
	}
	if h.calls.count("github.CreateSession") != 1 {
		t.Fatal("new session before the poll's deadline")
	}
	h.clock.advanceTo(t, deadline)
	waitFor(t, "no-answer line", func() bool { return strings.Contains(h.logs.String(), "message poll got no answer") })
	waitFor(t, "session closed", func() bool { _, _, _, c := s1.snapshot(); return c == 1 })
	if n := h.calls.count("github.CreateSession"); n != 1 {
		t.Fatalf("new session before the backoff (%d sessions)", n)
	}
	waitFor(t, "last error", func() bool { return strings.HasPrefix(h.report().LastError, "message session") })
	if r := h.report(); !strings.Contains(r.LastError, "no answer") {
		t.Errorf("lastError = %q, want the no-answer error", r.LastError)
	}
	h.clock.advanceTo(t, deadline.Add(time.Second))
	h.waitSessions(2)
	if r := h.report(); r.State != status.Running {
		t.Errorf("state = %s, want running", r.State)
	}
}

// A rate-limited GetMessage logs one WARN line with the status and the time
// the call may be made again (0002 SPEC R154).
func TestRateLimitedPollLogged(t *testing.T) {
	limited := opErr(retry.OpGetMessage, &retry.RateLimitError{
		RetryAfter: 90 * time.Second,
		Err:        statusError(429),
	})
	clock := newFakeClock()
	log, buf := capturedLog()
	c := newLoggedPollClient(clock, log, func(context.Context) (*scaleset.RunnerScaleSetMessage, error) {
		return nil, limited
	})
	now := clock.Now()
	if _, err := c.GetMessage(context.Background(), 0, 2); err != limited {
		t.Fatalf("GetMessage error = %v, want %v unchanged", err, limited)
	}
	lines := logLines(t, buf, "message poll rate limited")
	if len(lines) != 1 || lines[0]["level"] != "WARN" {
		t.Fatalf("rate limit lines = %v, want one WARN", lines)
	}
	if got := lines[0]["status"]; got != float64(429) {
		t.Errorf("status = %v, want 429", got)
	}
	want := now.Add(90 * time.Second).Format(time.RFC3339Nano)
	if got := lines[0]["retryAt"]; got != want {
		t.Errorf("retryAt = %v, want %v", got, want)
	}
}

// Other errors, and answers, log no rate limit line; a rate limit with no
// status in the error logs status 0.
func TestRateLimitedPollLogOnlyForRateLimit(t *testing.T) {
	for _, tt := range []struct {
		name string
		err  error
		want int
	}{
		{"no error", nil, 0},
		{"server error", opErr(retry.OpGetMessage, statusError(503)), 0},
		{"plain error", errors.New("boom"), 0},
		{"bare 429", opErr(retry.OpGetMessage, statusError(429)), 1},
		{"rate limit without status", opErr(retry.OpGetMessage, &retry.RateLimitError{}), 1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			log, buf := capturedLog()
			c := newLoggedPollClient(newFakeClock(), log, func(context.Context) (*scaleset.RunnerScaleSetMessage, error) {
				return nil, tt.err
			})
			_, _ = c.GetMessage(context.Background(), 0, 2)
			lines := logLines(t, buf, "message poll rate limited")
			if len(lines) != tt.want {
				t.Fatalf("%d rate limit lines, want %d", len(lines), tt.want)
			}
			if tt.name == "rate limit without status" && lines[0]["status"] != float64(0) {
				t.Errorf("status = %v, want 0", lines[0]["status"])
			}
		})
	}
}

// A rate limit that arrives as the poll's deadline passes is a rate
// limit, not a missing answer: Classify reads it first (0002 SPEC R153,
// R154).
func TestPollRateLimitBeatsDeadline(t *testing.T) {
	clock := newFakeClock()
	log, buf := capturedLog()
	c := newLoggedPollClient(clock, log, func(ctx context.Context) (*scaleset.RunnerScaleSetMessage, error) {
		<-ctx.Done()
		return nil, opErr(retry.OpGetMessage, &retry.RateLimitError{
			RetryAfter: time.Minute,
			Err:        errors.Join(statusError(429), ctx.Err()),
		})
	})
	start := clock.Now()
	done := getMessageAsync(context.Background(), c)
	clock.advanceTo(t, start.Add(getMessageAnswerDeadline))
	r := waitPoll(t, done)
	if class := retry.Classify(context.Background(), retry.OpGetMessage, r.err); class != retry.RateLimited {
		t.Fatalf("class = %v, want rate limited", class)
	}
	if n := len(logLines(t, buf, "message poll got no answer")); n != 0 {
		t.Errorf("%d no-answer lines for a rate-limited poll", n)
	}
	if n := len(logLines(t, buf, "message poll rate limited")); n != 1 {
		t.Errorf("%d rate limit lines, want 1", n)
	}
}
