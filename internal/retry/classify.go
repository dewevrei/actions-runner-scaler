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
//
// The first rule that matches decides:
//  1. ctx has ended: Canceled.
//  2. A rate limit (RateLimitError, or a bare 429): RateLimited.
//  3. The per-call deadline (context.DeadlineExceeded): Transient.
//  4. ErrAuth or ErrAmbiguousScaleSet: Permanent.
//  5. ErrRunnerGone, or a 404 at a runner or Docker removal: Gone.
//  6. ErrJobStillRunning: Transient; ErrRunnerExists: Permanent.
//  7. A 409 at session creation, or a 409 or 403 at a Docker removal:
//     Transient.
//  8. A 404 at a message session call: SessionLost.
//  9. Any other 4xx: Permanent.
//  10. Anything else (transport errors, 5xx, no status): Transient.
func Classify(ctx context.Context, op Op, err error) Class {
	if err == nil {
		return 0
	}
	if ctx.Err() != nil {
		return Canceled
	}
	// A rate-limited request was not processed, so no other rule can read
	// meaning into it. It comes before the per-call deadline: a call whose
	// deadline ends while it reads a rate-limited response carries both,
	// and the server's answer still forbids an early retry (R52).
	if _, ok := rateLimitOf(err); ok {
		return RateLimited
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return Transient
	}

	// A refused authentication exchange stops the call before its own
	// request; its 404 must not read as Gone or SessionLost (R51).
	if errors.Is(err, ErrAuth) {
		return Permanent
	}
	// Several scale sets with one name stay so until a person removes one
	// (R41). The sentinel keeps this case apart from any status rule.
	if errors.Is(err, ErrAmbiguousScaleSet) {
		return Permanent
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
