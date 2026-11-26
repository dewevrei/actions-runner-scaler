package daemon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http/httptrace"
	"sync"
	"sync/atomic"
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

// getMessagePollTimeout bounds one GetMessage poll (PLAN M5-5, SPEC Areas
// of concern 4). The service holds an empty long poll for about 50 s and
// does not return a JobStarted that becomes available during that hold
// until the next poll (actions/scaleset issue #122), so JobStarted could
// reach the scaler about 50 s late. Ending each poll after 10 s and polling
// again bounds that delay by about 10 s at about six polls a minute.
const getMessagePollTimeout = 10 * time.Second

// getMessageFullPollAfter is how long the server may go without answering
// a poll before the next poll is sent without getMessagePollTimeout
// (PLAN M5-5). A cut poll cannot tell an idle server from a dead one; the
// uncut poll can, at the cost of up to about 50 s of JobStarted delay once
// in this period, well within R81's 180 s.
const getMessageFullPollAfter = 5 * time.Minute

// getMessageAnswerDeadline bounds the uncut poll sent after
// getMessageFullPollAfter (0002 SPEC R153). A healthy service holds an
// empty poll for about 50.3 s; scaleset's own 2-minute attempt timeout and
// retries could otherwise keep a service that connects but never answers
// waiting for over 10 minutes. Detection takes 5 minutes to 5 minutes 10 s
// plus this bound, within R162's 6 minutes 30 s.
const getMessageAnswerDeadline = 70 * time.Second

// sessionClient is the listener.Client the daemon gives listener.Run
// (PLAN M5-5).
//
// DeleteMessage (R47): once finalize has begun it makes no call and returns
// scaler.ErrFinalizing, so Run ends without acknowledging; otherwise the
// call gets deleteMessageTimeout and also ends at finalize deadline A.
//
// GetMessage cuts a poll at getMessagePollTimeout, except that once the
// server has not answered for getMessageFullPollAfter it sends one poll
// without the cut; see GetMessage.
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
	// or without one; a cut poll is not reported.
	polled func()

	mu sync.Mutex
	// lastAnswer is when a GetMessage call last ended by itself, before any
	// cut: with a message, without one, or with an error. It starts at the
	// wrapper's creation.
	lastAnswer time.Time
}

// newSessionClient returns the wrapper for one session. Its server-answer
// clock starts now, so a session the server never answers gets its first
// uncut poll getMessageFullPollAfter after it began.
func newSessionClient(inner Session, table *scaler.Table, clock scaler.Clock, log *slog.Logger, deadlineA context.Context, polled func()) *sessionClient {
	return &sessionClient{
		inner:      inner,
		table:      table,
		clock:      clock,
		log:        log,
		deadlineA:  deadlineA,
		polled:     polled,
		lastAnswer: clock.Now(),
	}
}

// GetMessage polls for the next message.
//
// A poll is cut at getMessagePollTimeout (SPEC Areas of concern 4). The
// cut covers the whole call: scaleset's retries of a failed attempt and its
// session refresh run under the same context, and the library neither
// retries nor waits once that context is done (scaleset v0.5.0
// DefaultShouldRetry and commonClient.send). It can return earlier with the
// attempt's own error, since the library skips a retry wait that would
// reach the deadline (waitOutlasts); that error ends Run as before.
//
// A cut poll whose last request was written to a connection is an empty
// poll: nil, nil, which Run hands to Scale as it does an empty long poll.
// One whose last request never got onto a connection (a hung DNS lookup,
// dial, or TLS handshake) is a transient error, so Run ends and the session
// loop makes a new session (R25).
//
// A healthy idle server holds a poll for about 50 s, so a cut poll says
// nothing about whether the server is there. Once no call has ended by
// itself for getMessageFullPollAfter, the next poll is sent without the
// cut, under ctx and getMessageAnswerDeadline: a service that does not
// answer then shows up as a transient error (0002 SPEC R153), and Run ends
// so the session loop makes a new session (R25).
//
// A GetMessage that ends rate limited, cut or not, is logged once (0002
// SPEC R154); a rate limit that arrived as the cut passed is still a rate
// limit, as in uncutPoll.
func (c *sessionClient) GetMessage(ctx context.Context, lastMessageID, maxCapacity int) (*scaleset.RunnerScaleSetMessage, error) {
	if c.fullPollDue() {
		return c.uncutPoll(ctx, lastMessageID, maxCapacity)
	}

	var trace pollTrace
	pollCtx, cancel := withTimeout(httptrace.WithClientTrace(ctx, trace.clientTrace()), c.clock, getMessagePollTimeout)
	defer cancel()
	msg, err := c.inner.GetMessage(pollCtx, lastMessageID, maxCapacity)
	if err == nil || !pollBoundPassed(ctx, pollCtx, err) || c.rateLimited(ctx, err) {
		c.answered(err)
		c.logRateLimited(ctx, err)
		return msg, err
	}
	if !trace.written() {
		return nil, &retry.OpError{Op: retry.OpGetMessage, Err: fmt.Errorf(
			"message poll got no connection within %s: %w", getMessagePollTimeout, err)}
	}
	return nil, nil
}

// uncutPoll sends the poll after getMessageFullPollAfter without
// getMessagePollTimeout. It runs under getMessageAnswerDeadline; when only
// that deadline ended it, the server did not answer and the poll is a
// transient error wrapping context.DeadlineExceeded (0002 SPEC R153). The
// deadline does not count as an answer. A rate limit that arrived as the
// deadline passed is still a rate limit (retry.Classify reads it first).
func (c *sessionClient) uncutPoll(ctx context.Context, lastMessageID, maxCapacity int) (*scaleset.RunnerScaleSetMessage, error) {
	pollCtx, cancel := withTimeout(ctx, c.clock, getMessageAnswerDeadline)
	defer cancel()
	msg, err := c.inner.GetMessage(pollCtx, lastMessageID, maxCapacity)
	if err != nil && pollBoundPassed(ctx, pollCtx, err) && !c.rateLimited(ctx, err) {
		c.mu.Lock()
		silent := c.clock.Now().Sub(c.lastAnswer)
		c.mu.Unlock()
		c.log.Warn("message poll got no answer", "elapsed", silent, "deadline", getMessageAnswerDeadline)
		return nil, &retry.OpError{Op: retry.OpGetMessage, Err: fmt.Errorf(
			"message poll got no answer within %s: %w", getMessageAnswerDeadline, err)}
	}
	c.answered(err)
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

// fullPollDue reports whether the server has not answered for
// getMessageFullPollAfter.
func (c *sessionClient) fullPollDue() bool {
	now := c.clock.Now()
	c.mu.Lock()
	defer c.mu.Unlock()
	return now.Sub(c.lastAnswer) >= getMessageFullPollAfter
}

// answered records a call that ended by itself and reports a successful
// one to polled.
func (c *sessionClient) answered(err error) {
	now := c.clock.Now()
	c.mu.Lock()
	c.lastAnswer = now
	c.mu.Unlock()
	if err == nil && c.polled != nil {
		c.polled()
	}
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

// pollTrace tracks whether the latest request of a poll was written to a
// connection. The library sends each attempt, and a session refresh, with
// the poll's context, so net/http reports every request's connection to
// the trace. GetConn starts a request; WroteRequest without an error means
// it reached a connection. HTTP/2 may report WroteRequest after RoundTrip
// returned, so a late report from one attempt could in principle land
// after the next attempt's GetConn; the window is far shorter than the
// library's retry waits, and the uncut poll after getMessageFullPollAfter
// still catches a poll misread this way.
type pollTrace struct {
	wrote atomic.Bool
}

func (p *pollTrace) clientTrace() *httptrace.ClientTrace {
	return &httptrace.ClientTrace{
		GetConn: func(string) { p.wrote.Store(false) },
		WroteRequest: func(info httptrace.WroteRequestInfo) {
			p.wrote.Store(info.Err == nil)
		},
	}
}

// written reports whether the latest request was written to a connection.
func (p *pollTrace) written() bool { return p.wrote.Load() }

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
