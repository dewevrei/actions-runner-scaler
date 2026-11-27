package daemon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/actions/scaleset"
	"github.com/actions/scaleset/listener"

	"github.com/dewevrei/actions-runner-scaler/internal/retry"
	"github.com/dewevrei/actions-runner-scaler/internal/scaler"
)

// deleteMessageTimeout bounds one DeleteMessage call (PLAN M5-5). The
// listener calls it with context.WithoutCancel and the github session adds
// no deadline, so without this bound an ack could outlive finalize (R47).
const deleteMessageTimeout = 30 * time.Second

// getMessageAnswerDeadline bounds one GetMessage poll (0002 SPEC R153). It
// is scaleset's own per-attempt poll timeout, scaleset.MessagePollTimeout
// (2 minutes, v0.5.0 and a7e9c70, which leaves room over the service's hold
// of about 50 s for an empty poll; actions-runner-controller sets no
// timeout of its own). The library applies that value to each attempt and
// retries a failed one with a new window, which could keep a service that
// connects but never answers waiting for over 10 minutes; this bound covers
// the whole call. Detection takes this bound, within R162's 6 minutes 30 s.
const getMessageAnswerDeadline = scaleset.MessagePollTimeout

// sessionClient is the listener.Client the daemon gives listener.Run
// (PLAN M5-5).
//
// DeleteMessage (R47): once finalize has begun it makes no call and returns
// scaler.ErrFinalizing, so Run ends without acknowledging; otherwise the
// call gets deleteMessageTimeout and also ends at finalize deadline A.
//
// GetMessage gives each poll getMessageAnswerDeadline; see GetMessage.
//
// AcquireJobs and Session pass through.
type sessionClient struct {
	inner Session
	table *scaler.Table
	clock scaler.Clock
	log   *slog.Logger
	// deadlineA ends at finalize deadline A.
	deadlineA context.Context
	// polled is called after each poll the server answered, with a message
	// or without one.
	polled func()
}

// newSessionClient returns the wrapper for one session.
func newSessionClient(inner Session, table *scaler.Table, clock scaler.Clock, log *slog.Logger, deadlineA context.Context, polled func()) *sessionClient {
	return &sessionClient{
		inner:     inner,
		table:     table,
		clock:     clock,
		log:       log,
		deadlineA: deadlineA,
		polled:    polled,
	}
}

// GetMessage polls for the next message under getMessageAnswerDeadline.
// The deadline covers the whole call: scaleset's retries of a failed
// attempt and its session refresh run under the same context, and the
// library neither retries nor waits once that context is done (scaleset
// v0.5.0 DefaultShouldRetry and commonClient.send).
//
// When only that deadline ended the call, the service did not answer and
// the poll is a transient error wrapping context.DeadlineExceeded (0002
// SPEC R153), so Run ends and the session loop makes a new session (R25).
// A rate limit that arrived as the deadline passed is still a rate limit
// (retry.Classify reads it first). A deadline error while the poll's own
// context is live, or with ctx ended, is a real failure and is returned
// unchanged.
//
// A GetMessage that ends rate limited is logged once (0002 SPEC R154).
func (c *sessionClient) GetMessage(ctx context.Context, lastMessageID, maxCapacity int) (*scaleset.RunnerScaleSetMessage, error) {
	pollCtx, cancel := withTimeout(ctx, c.clock, getMessageAnswerDeadline)
	defer cancel()
	msg, err := c.inner.GetMessage(pollCtx, lastMessageID, maxCapacity)
	if err != nil && pollBoundPassed(ctx, pollCtx, err) && !c.rateLimited(ctx, err) {
		c.log.Warn("message poll got no answer", "deadline", getMessageAnswerDeadline)
		return nil, &retry.OpError{Op: retry.OpGetMessage, Err: fmt.Errorf(
			"message poll got no answer within %s: %w", getMessageAnswerDeadline, err)}
	}
	if err == nil && c.polled != nil {
		c.polled()
	}
	c.logRateLimited(ctx, err)
	return msg, err
}

// rateLimited reports whether err ended a GetMessage call as a rate limit.
func (c *sessionClient) rateLimited(ctx context.Context, err error) bool {
	return err != nil && retry.Classify(ctx, retry.OpGetMessage, err) == retry.RateLimited
}

// logRateLimited writes the WARN line of 0002 SPEC R154 when err is a rate
// limit: the response status (0 when err carries none) and when the call
// may be made again, by the same wait Backoff.Wait uses.
func (c *sessionClient) logRateLimited(ctx context.Context, err error) {
	if !c.rateLimited(ctx, err) {
		return
	}
	var status int
	var sc retry.StatusCoder
	if errors.As(err, &sc) {
		status = sc.HTTPStatus()
	}
	now := c.clock.Now()
	c.log.Warn("message poll rate limited", "status", status,
		"retryAt", now.Add(retry.RateLimitWait(err, now)))
}

// pollBoundPassed reports whether err ended a poll only because the poll's
// own bound passed: err is a deadline error, pollCtx ran out of its time,
// and parent is still live. A deadline error while pollCtx is live (some
// other bound inside the call), or with parent ended (cancellation or the
// parent's own deadline), is a real failure.
func pollBoundPassed(parent, pollCtx context.Context, err error) bool {
	return errors.Is(err, context.DeadlineExceeded) &&
		errors.Is(pollCtx.Err(), context.DeadlineExceeded) &&
		parent.Err() == nil
}

func (c *sessionClient) DeleteMessage(ctx context.Context, messageID int) error {
	if c.table.Finalizing() {
		return fmt.Errorf("message %d not acknowledged: %w", messageID, scaler.ErrFinalizing)
	}
	callCtx, cancel := withTimeout(ctx, c.clock, deleteMessageTimeout)
	defer cancel()
	stop := context.AfterFunc(c.deadlineA, cancel)
	defer stop()
	return c.inner.DeleteMessage(callCtx, messageID)
}

func (c *sessionClient) AcquireJobs(ctx context.Context, requestIDs []int64) ([]int64, error) {
	return c.inner.AcquireJobs(ctx, requestIDs)
}

func (c *sessionClient) Session() scaleset.RunnerScaleSetSession { return c.inner.Session() }

var _ listener.Client = (*sessionClient)(nil)
