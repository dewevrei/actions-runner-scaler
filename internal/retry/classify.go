// Package retry classifies call errors and paces retries.
//
// It only decides what an error means; each call site decides how to react
// (R97), so the package does not import the github or docker packages.
package retry

import (
	"context"
	"errors"
	"net/http"
	"strconv"
)

// Class is the meaning of a failed call. The zero Class is not a valid class.
type Class int

// Classes returned by Classify.
const (
	// Canceled means the caller's context ended.
	Canceled Class = iota + 1
	// Transient means the same call may succeed later.
	Transient
	// Gone means the target is already gone; removals treat it as success.
	Gone
	// RateLimited means the call must not be repeated before the limit allows.
	RateLimited
	// SessionLost means the message session no longer exists (R25).
	SessionLost
	// Permanent means retrying cannot help until a person fixes the cause (R41).
	Permanent
)

func (c Class) String() string {
	switch c {
	case Canceled:
		return "canceled"
	case Transient:
		return "transient"
	case Gone:
		return "gone"
	case RateLimited:
		return "rate limited"
	case SessionLost:
		return "session lost"
	case Permanent:
		return "permanent"
	default:
		return "Class(" + strconv.Itoa(int(c)) + ")"
	}
}

// Classify reports the class of err returned by the call op made under ctx.
// ctx is the caller's context, not the per-call one, so a per-call deadline
// is told apart from shutdown. err must not be nil; a nil err yields the zero
// Class.
//
// Only op decides the call-site rules; the Op of an OpError in err's chain
// is ignored, because the caller knows which call it is reacting to and a
// wrapped error may come from a different, inner call.
func Classify(ctx context.Context, op Op, err error) Class {
	if err == nil {
		return 0
	}
	if ctx.Err() != nil {
		return Canceled
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return Transient
	}

	status, hasStatus := statusOf(err)
	is := func(code int) bool { return hasStatus && status == code }

	if errors.Is(err, ErrRunnerGone) ||
		(op == OpRemoveRunner && is(http.StatusNotFound)) ||
		(isDockerRemove(op) && is(http.StatusNotFound)) {
		return Gone
	}

	// The library wraps these sentinels in a 409 conflict as well, so they
	// must be checked before any status rule.
	if errors.Is(err, ErrJobStillRunning) {
		return Transient
	}
	if errors.Is(err, ErrRunnerExists) {
		return Permanent
	}

	if _, ok := rateLimitOf(err); ok {
		return RateLimited
	}
	// A session conflict clears when the previous session expires.
	if op == OpSessionCreate && is(http.StatusConflict) {
		return Transient
	}
	// Docker reports "removal already in progress" as 409 and a network that
	// still has endpoints as 403; both clear on their own (R50).
	if isDockerRemove(op) && (is(http.StatusConflict) || is(http.StatusForbidden)) {
		return Transient
	}

	if isSessionCall(op) && is(http.StatusNotFound) {
		return SessionLost
	}

	if hasStatus && status >= 400 && status < 500 {
		return Permanent
	}
	// Transport errors, 5xx, and failures without a status may clear later.
	return Transient
}

// rateLimitOf returns the rate limit err reports. A bare 429 that no one
// converted counts as a limit without hints, so it still waits (R52).
func rateLimitOf(err error) (*RateLimitError, bool) {
	var rl *RateLimitError
	if errors.As(err, &rl) {
		return rl, true
	}
	if status, ok := statusOf(err); ok && status == http.StatusTooManyRequests {
		return &RateLimitError{Err: err}, true
	}
	return nil, false
}

func statusOf(err error) (int, bool) {
	var sc StatusCoder
	if errors.As(err, &sc) {
		return sc.HTTPStatus(), true
	}
	return 0, false
}

func isDockerRemove(op Op) bool {
	switch op {
	case OpContainerRemove, OpNetworkRemove, OpVolumeRemove:
		return true
	}
	return false
}

// isSessionCall reports the calls whose 404 means the session expired (R25).
// Session creation is excluded: its 404 means the scale set is missing.
func isSessionCall(op Op) bool {
	switch op {
	case OpGetMessage, OpDeleteMessage, OpAcquireJobs, OpSessionRefresh:
		return true
	}
	return false
}
