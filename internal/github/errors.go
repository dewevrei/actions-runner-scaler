package github

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/actions/scaleset"

	"github.com/dewevrei/actions-runner-scaler/internal/logging"
	"github.com/dewevrei/actions-runner-scaler/internal/retry"
)

// StatusError reports a GitHub response with an unexpected HTTP status that
// is not a rate limit. It implements retry.StatusCoder.
type StatusError struct {
	// Code is the HTTP status code.
	Code int
	// Message is GitHub's error message or the library's error text, with
	// secrets and URL queries removed. It may be empty.
	Message string
	// Err holds only the context errors (context.Canceled,
	// context.DeadlineExceeded) of the underlying error, so that errors.Is
	// still finds them; nil when there are none. The rest of the underlying
	// error is dropped: its text can quote the response body, which only
	// Message has been cleaned of (see keptCause).
	Err error
}

// Error returns the status and the redacted message.
func (e *StatusError) Error() string {
	s := "HTTP " + strconv.Itoa(e.Code)
	if text := http.StatusText(e.Code); text != "" {
		s += " " + text
	}
	if e.Message != "" {
		s += ": " + e.Message
	}
	return s
}

// HTTPStatus returns Code.
func (e *StatusError) HTTPStatus() int { return e.Code }

// Unwrap returns the underlying error.
func (e *StatusError) Unwrap() error { return e.Err }

// safeError carries a redacted message for an error whose own text may hold
// secrets. Of the original it keeps only what keptCause returns, so no part
// of the error chain repeats the unredacted text.
type safeError struct {
	msg   string
	cause error
}

// keptCause returns the part of err that the redacted errors of this
// package keep in their chain: the context errors, which classification
// and callers test with errors.Is (retry.Classify reads
// context.DeadlineExceeded as transient). The original error is dropped
// because its text can quote a response body, such as a JIT config, and
// redaction only covers registered secrets. Nothing else is lost: the
// library's sentinels are already turned into a status or a retry sentinel
// by convertError, and an error without a status classifies the same with
// or without its cause. It returns nil when err holds no context error.
func keptCause(err error) error {
	var kept []error
	for _, target := range []error{context.Canceled, context.DeadlineExceeded} {
		if errors.Is(err, target) {
			kept = append(kept, target)
		}
	}
	switch len(kept) {
	case 0:
		return nil
	case 1:
		return kept[0]
	}
	return errors.Join(kept...)
}

func (e *safeError) Error() string { return e.msg }
func (e *safeError) Unwrap() error { return e.cause }

// sentinelError marks a library error with one of the retry sentinels while
// printing as the wrapped error.
type sentinelError struct {
	sentinel error
	err      error
}

func (e *sentinelError) Error() string   { return e.err.Error() }
func (e *sentinelError) Unwrap() []error { return []error{e.sentinel, e.err} }

// statusSlot holds what the recording transport saw on the last round trip
// of one library call. The adapter puts a new slot in the call's context;
// the recording transport writes it. The whole record is one value, so a
// reader never sees the status of one response with the headers of another.
type statusSlot struct {
	last atomic.Pointer[lastFailure]
}

// failure returns what the slot recorded; the zero lastFailure when the
// last round trip succeeded or failed in transport.
func (s *statusSlot) failure() lastFailure {
	if f := s.last.Load(); f != nil {
		return *f
	}
	return lastFailure{}
}

// lastFailure is the failed response, if any, that ended a library call.
type lastFailure struct {
	// status is the response's status, or 0 when the last round trip
	// succeeded or failed in transport.
	status int
	// authExchange reports that the response answered an authentication
	// exchange (see isAuthExchange).
	authExchange bool
	// quotaSpent reports X-RateLimit-Remaining: 0 on the response.
	quotaSpent bool
	// reset is the response's X-RateLimit-Reset when quotaSpent is set and
	// the header holds a positive epoch second; zero otherwise.
	reset time.Time
	// retryAfter is the response's Retry-After header as sent. It is read
	// with parseRetryAfter against at, on both call paths, so a value the
	// library cannot use (one too large for a time.Duration) still gives
	// the same wait as on the REST path.
	retryAfter string
	// at is when the response arrived.
	at time.Time
	// rateLimitMessage reports a 403 whose message names a rate limit
	// (isRateLimitMessage). It is only set for 403.
	rateLimitMessage bool
}

// newFailure returns what resp says about a rate limit, read at at, with
// message as the response's message (responseMessage). Both call paths
// build their rate limit errors from it, so they read a response alike.
func newFailure(resp *http.Response, at time.Time, message string) lastFailure {
	f := lastFailure{status: resp.StatusCode, retryAfter: resp.Header.Get("Retry-After"), at: at}
	f.quotaSpent, f.reset = spentQuota(resp.Header)
	if resp.StatusCode == http.StatusForbidden {
		f.rateLimitMessage = isRateLimitMessage(message)
	}
	return f
}

// isRateLimit reports whether the response was a rate limit. Following
// GitHub's "Rate limits for the REST API", a 429 always is, and a 403 is
// when it has Retry-After, X-RateLimit-Remaining 0 (a spent primary limit;
// GitHub then always sends 0), or a message that names a rate limit (a
// secondary limit: "an error message that indicates that you exceeded a
// secondary rate limit"). These are the conditions scaleset v0.5.0 uses
// (errors.go rateLimitFromResponse), with the message test widened by
// isRateLimitMessage.
func (f lastFailure) isRateLimit() bool {
	switch f.status {
	case http.StatusTooManyRequests:
		return true
	case http.StatusForbidden:
		return f.retryAfter != "" || f.quotaSpent || f.rateLimitMessage
	}
	return false
}

// rateLimitError returns the rate limit error for f with cause as its
// underlying error. Reset is kept only when the remaining quota is 0, as
// GitHub says to wait for X-RateLimit-Reset only then, for primary and
// secondary limits alike.
func (f lastFailure) rateLimitError(cause error) *retry.RateLimitError {
	rl := &retry.RateLimitError{Err: cause}
	if f.quotaSpent {
		rl.Reset = f.reset
	}
	if wait, ok := parseRetryAfter(f.retryAfter, f.at); ok {
		rl.RetryAfter = wait
	}
	return rl
}

// secondaryRateLimitPattern is the message test of scaleset v0.5.0
// (errors.go secondaryRateLimitPattern).
var secondaryRateLimitPattern = regexp.MustCompile(`(?i)\bsecondary rate\b`)

// isRateLimitMessage reports whether a 403's message names a rate limit:
// it contains "rate limit" in any case, or matches the library's
// `\bsecondary rate\b`. GitHub documents only that a secondary limit's
// message "indicates that you exceeded a secondary rate limit", not its
// exact text, so the test is the union of the two earlier ones: it accepts
// every message either path accepted before ("secondary rate-limit"
// matches only the library's pattern, "API rate limit exceeded" only the
// substring). A permanent 403 whose message names a rate limit is not
// known; were there one, it would wait and retry instead of stopping,
// which an operator sees in the logs, while a missed rate limit would stop
// the scaler for a condition that clears by itself.
func isRateLimitMessage(msg string) bool {
	return strings.Contains(strings.ToLower(msg), "rate limit") || secondaryRateLimitPattern.MatchString(msg)
}

// responseMessage returns an error response's message the way scaleset
// v0.5.0 reads it (errors.go newRequestResponseError): the whole body when
// it is plain text, otherwise the "message" field of a JSON body, which
// both GitHub's REST API and the Actions service send.
func responseMessage(h http.Header, body []byte) string {
	if strings.Contains(h.Get("Content-Type"), "text/plain") {
		return string(body)
	}
	var parsed struct {
		Message string `json:"message"`
	}
	if json.Unmarshal(body, &parsed) != nil {
		return ""
	}
	return parsed.Message
}

type statusSlotKey struct{}

// withStatusSlot returns a context carrying a new, empty slot.
func withStatusSlot(ctx context.Context) (context.Context, *statusSlot) {
	slot := new(statusSlot)
	return context.WithValue(ctx, statusSlotKey{}, slot), slot
}

func statusSlotFrom(ctx context.Context) *statusSlot {
	slot, _ := ctx.Value(statusSlotKey{}).(*statusSlot)
	return slot
}

// statusRecorder is an http.RoundTripper that records, in the request
// context's slot, each response of 400 or more: its status, whether it
// answered an authentication exchange, and whether it spent the rate limit
// quota and when that resets. It clears the slot before every round trip,
// so when the last attempt of a call fails in transport the slot is empty
// and the failure reads as a transport error.
type statusRecorder struct {
	next http.RoundTripper
	// now returns the time a response arrives; nil means time.Now.
	now func() time.Time
}

func (r *statusRecorder) clock() time.Time {
	if r.now != nil {
		return r.now()
	}
	return time.Now()
}

// RoundTrip sends req through the wrapped transport and records the status.
func (r *statusRecorder) RoundTrip(req *http.Request) (*http.Response, error) {
	slot := statusSlotFrom(req.Context())
	if slot != nil {
		slot.last.Store(nil)
	}
	resp, err := r.next.RoundTrip(req)
	if slot != nil && err == nil && resp != nil && resp.StatusCode >= 400 {
		var message string
		if resp.StatusCode == http.StatusForbidden {
			message = peekMessage(resp)
		}
		f := newFailure(resp, r.clock(), message)
		f.authExchange = isAuthExchange(req)
		slot.last.Store(&f)
	}
	return resp, err
}

// peekMessage returns the message of resp's body (responseMessage) and
// leaves the body readable from the start for the library. It reads at
// most maxErrorBodyBytes; the rest stays unread behind what was read.
func peekMessage(resp *http.Response) string {
	if resp.Body == nil || resp.Body == http.NoBody {
		return ""
	}
	// A read error is left for the library to meet when it reads on.
	data, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBodyBytes))
	resp.Body = &peekedBody{Reader: io.MultiReader(bytes.NewReader(data), resp.Body), Closer: resp.Body}
	return responseMessage(resp.Header, data)
}

// peekedBody replays the bytes peekMessage read, then the rest of the
// original body, which it closes.
type peekedBody struct {
	io.Reader
	io.Closer
}

// spentQuota reports whether h says the rate limit quota is spent
// (X-RateLimit-Remaining: 0) and, if so, the X-RateLimit-Reset time, or the
// zero time when that header is missing or not a positive epoch second.
func spentQuota(h http.Header) (bool, time.Time) {
	if h.Get("X-RateLimit-Remaining") != "0" {
		return false, time.Time{}
	}
	sec, err := strconv.ParseInt(h.Get("X-RateLimit-Reset"), 10, 64)
	if err != nil || sec <= 0 {
		return true, time.Time{}
	}
	return true, time.Unix(sec, 0)
}

// isAuthExchange reports whether req is one of the requests scaleset v0.5.0
// sends, inside any Actions service call, to renew its admin token
// (client.go updateTokenIfNeeded, getRunnerRegistrationToken,
// fetchAccessToken, getActionsServiceAdminConnection): the installation
// token, the registration token (repository, organization or enterprise),
// and the admin connection. The paths are matched by suffix because GitHub
// Enterprise Server puts the API under /api/v3.
func isAuthExchange(req *http.Request) bool {
	if req.Method != http.MethodPost {
		return false
	}
	path := req.URL.Path
	if strings.HasSuffix(path, "/actions/runners/registration-token") ||
		strings.HasSuffix(path, "/actions/runner-registration") {
		return true
	}
	rest, ok := strings.CutSuffix(path, "/access_tokens")
	if !ok {
		return false
	}
	i := strings.LastIndex(rest, "/")
	return i >= 0 && strings.HasSuffix(rest[:i], "/app/installations")
}

// libraryStatus matches the status the library writes into its error text,
// as in `failed(status="404 Not Found")`.
var libraryStatus = regexp.MustCompile(`status="(\d{3})`)

// statusOfText returns the status written in the library's error text.
func statusOfText(text string) (int, bool) {
	m := libraryStatus.FindStringSubmatch(text)
	if m == nil {
		return 0, false
	}
	code, err := strconv.Atoi(m[1])
	if err != nil {
		return 0, false
	}
	return code, true
}

// convertError rewrites an error from the scaleset library into the values
// the retry package classifies, wrapped in a retry.OpError for op. last is
// what the recording transport saw on the call's last round trip.
//
// The status is taken from, in order:
//  1. the library's sentinels (400, 401, 404, 409) and its rate limit error;
//     runner sentinels also become the retry sentinels,
//  2. the recorded status,
//  3. the status in the library's error text, used only when 1 and 2 give
//     none.
//
// Only a status of 400 or more counts. A response below 400 that the library
// failed to decode yields an error without a status, which classifies as
// transient. Its text omits the library's message, which can quote the
// response body, such as a JIT config. The one exception is the library's
// report of several scale sets with one name, which is marked
// retry.ErrAmbiguousScaleSet instead; see multipleScaleSets. A missing
// runner group is marked retry.ErrTargetSetup; see noRunnerGroup.
//
// A 403 that the library did not report as a rate limit but that is one by
// lastFailure.isRateLimit (its message names a rate limit in a way the
// library's narrower test misses) becomes a rate limit too, so both call
// paths treat a 403 alike.
//
// When the call ended on a 4xx from an authentication exchange, the error
// is marked retry.ErrAuth instead of a runner sentinel: the library renews
// its admin token inside the call (client.go newActionsServiceRequest), so
// that 4xx, a 404 included, is about the App's access and not about the
// call's target. A 5xx or a transport error there stays unmarked and
// transient, since the same exchange may succeed later.
func convertError(op retry.Op, err error, last lastFailure, redact logging.Redactor) error {
	if err == nil {
		return nil
	}
	text := err.Error()

	var rl *scaleset.RateLimitError
	if errors.As(err, &rl) {
		return &retry.OpError{Op: op, Err: rateLimitFromLibrary(rl, err, last, redact)}
	}
	if last.isRateLimit() && sentinelStatus(err) == 0 {
		cause := &StatusError{Code: last.status, Message: shortMessage(redact, text), Err: keptCause(err)}
		return &retry.OpError{Op: op, Err: last.rateLimitError(cause)}
	}

	var sentinel error
	switch {
	case errors.Is(err, scaleset.RunnerNotFoundError):
		sentinel = retry.ErrRunnerGone
	case errors.Is(err, scaleset.JobStillRunningError):
		sentinel = retry.ErrJobStillRunning
	case errors.Is(err, scaleset.RunnerExistsError):
		sentinel = retry.ErrRunnerExists
	}

	status := sentinelStatus(err)
	if status == 0 && last.status >= 400 {
		status = last.status
	}
	textStatus, inText := statusOfText(text)
	if status == 0 && inText && textStatus >= 400 {
		status = textStatus
	}
	if last.authExchange && status >= 400 && status < 500 {
		sentinel = retry.ErrAuth
	}

	var out error
	switch {
	case status == 0 && multipleScaleSets(text):
		out = &safeError{
			msg:   "server answered " + strconv.Itoa(textStatus) + " with several scale sets of the name: " + shortMessage(redact, text),
			cause: keptCause(err),
		}
		sentinel = retry.ErrAmbiguousScaleSet
	case status == 0 && noRunnerGroup(text):
		out = &safeError{
			msg:   "server answered " + strconv.Itoa(textStatus) + " without the runner group: " + shortMessage(redact, text),
			cause: keptCause(err),
		}
		sentinel = retry.ErrTargetSetup
	case status != 0:
		out = &StatusError{Code: status, Message: shortMessage(redact, text), Err: keptCause(err)}
	case inText:
		// A response arrived but could not be used; its body may be secret.
		out = &safeError{
			msg:   "unusable response with status " + strconv.Itoa(textStatus) + " (details withheld)",
			cause: keptCause(err),
		}
	default:
		out = &safeError{msg: shortMessage(redact, text), cause: keptCause(err)}
	}
	if sentinel != nil {
		out = &sentinelError{sentinel: sentinel, err: out}
	}
	return &retry.OpError{Op: op, Err: out}
}

// multipleScaleSetsText is how scaleset v0.5.0 reports, with the 200
// response's status in the text, that GetRunnerScaleSet found more than one
// scale set with the name (client.go GetRunnerScaleSet).
const multipleScaleSetsText = "multiple runner scale sets found with name"

// multipleScaleSets reports whether text is the library's report of several
// scale sets with one name. Only a person can fix that state (R41), so the
// error is marked retry.ErrAmbiguousScaleSet, which classifies as permanent
// on its own. It is given no status: the server sent 200, and a made-up
// error status would follow whatever a call site does with that status.
// The message keeps the status the server sent.
func multipleScaleSets(text string) bool {
	return strings.Contains(text, multipleScaleSetsText)
}

// noRunnerGroupText is how scaleset v0.5.0 reports, with the 200 response's
// status in the text, that GetRunnerGroupByName found no group of the name
// (client.go GetRunnerGroupByName).
const noRunnerGroupText = "no runner group found with name"

// noRunnerGroup reports whether text is the library's report of a missing
// runner group. Only a person can create the group, so the error is marked
// retry.ErrTargetSetup; like multipleScaleSets it has no status (0002 SPEC
// R111).
func noRunnerGroup(text string) bool {
	return strings.Contains(text, noRunnerGroupText)
}

// sentinelStatus returns the status that a library sentinel stands for.
func sentinelStatus(err error) int {
	switch {
	case errors.Is(err, scaleset.BadRequestError):
		return http.StatusBadRequest
	case errors.Is(err, scaleset.UnauthorizedError):
		return http.StatusUnauthorized
	case errors.Is(err, scaleset.NotFoundError):
		return http.StatusNotFound
	case errors.Is(err, scaleset.ConflictError):
		return http.StatusConflict
	}
	return 0
}

// rateLimitFromLibrary converts the library's rate limit error. last is
// the recorded response, which is the rate-limited one, since RetryConfig
// stops the library from retrying rate limits.
//
// GitHub sends X-RateLimit-Reset on every response, and its documentation
// says to wait for it whenever X-RateLimit-Remaining is 0, for primary and
// secondary limits alike ("Rate limits for the REST API", "Exceeding the
// rate limit"). The library sets Kind to secondary when the message names a
// secondary limit even if the quota is spent, and then turns the reset into
// RetryAfter only when Retry-After is absent (scaleset v0.5.0 errors.go
// rateLimitFromResponse). So Reset is kept when the recorded response spent
// the quota, or when the library's kind is primary, which it gives only for
// a spent quota; the retry package then waits for the later of the two.
//
// RetryAfter is read again from the recorded Retry-After header with
// parseRetryAfter, as on the REST path, when the header is usable there.
// The library drops a value too large for a time.Duration, which would
// shorten the wait to the 60-second default; parseRetryAfter gives
// retry.MaxRateLimitWait for it instead. When the header is not usable
// there, the library's value stays, which for a spent primary limit is the
// time until the reset.
func rateLimitFromLibrary(rl *scaleset.RateLimitError, err error, last lastFailure, redact logging.Redactor) *retry.RateLimitError {
	// The library's own text starts with "rate limited", which the retry
	// error already says, so only the request part is kept.
	var msg string
	if inner := rl.Unwrap(); inner != nil {
		msg = shortMessage(redact, inner.Error())
	}
	out := &retry.RateLimitError{
		RetryAfter: rl.RetryAfter,
		Err:        &StatusError{Code: rl.StatusCode, Message: msg, Err: keptCause(err)},
	}
	if last.status == rl.StatusCode {
		if wait, ok := parseRetryAfter(last.retryAfter, last.at); ok {
			out.RetryAfter = wait
		}
	}
	switch {
	case last.quotaSpent && last.status == rl.StatusCode:
		out.Reset = last.reset
	case rl.Kind == scaleset.RateLimitPrimary:
		out.Reset = rl.Reset
	}
	return out
}

// shortMessage redacts msg, folds its white space, and cuts it to
// maxMessageRunes, so a long response body, such as an HTML error page,
// cannot fill the logs. Redaction comes first so the cut cannot split a
// secret and leave its start unredacted.
func shortMessage(redact logging.Redactor, msg string) string {
	msg = strings.Join(strings.Fields(redact.Redact(msg)), " ")
	if runes := []rune(msg); len(runes) > maxMessageRunes {
		msg = string(runes[:maxMessageRunes]) + "…"
	}
	return msg
}
