package docker

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"sync/atomic"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/moby/moby/client"

	"github.com/dewevrei/actions-runner-scaler/internal/retry"
)

// StatusError is a Docker Engine API error response. It reports the
// response status through HTTPStatus so that retry.Classify can read it.
//
// The Engine API client does not keep the status of an error response in
// the error it returns; it keeps an errdefs class chosen from the status
// (client@v0.6.2 errors.go httpErrorFromStatusCode, errdefs/pkg@v0.3.0
// errhttp/http.go ToNative). A status without a class of its own, such as
// 405, 413 or 422, gets only the "unknown" class, and a failed attach
// upgrade (client@v0.6.2 hijack.go 103~106) gets no class at all.
//
// So the status comes from the response itself where possible: an Engine
// made by NewEngine records the status of each error response in a slot
// that the Client puts into the context of each call (statusHooks). Without
// a recorded status, the errdefs class is mapped back to the status it
// came from; an error with neither is not converted.
type StatusError struct {
	Status int
	Err    error
}

func (e *StatusError) Error() string {
	return "status " + strconv.Itoa(e.Status) + ": " + e.Err.Error()
}

// HTTPStatus implements retry.StatusCoder.
func (e *StatusError) HTTPStatus() int { return e.Status }

// Unwrap returns the client error, so the errdefs checks still work.
func (e *StatusError) Unwrap() error { return e.Err }

// IsNotFound reports whether err means the object (container, network,
// image) does not exist.
func IsNotFound(err error) bool {
	return cerrdefs.IsNotFound(err)
}

// statusSlot holds the status of the last error response one Engine call
// got, or 0. Each call gets its own slot through its context, so calls
// running at the same time never see each other's status.
type statusSlot struct{ code atomic.Int32 }

type statusSlotKey struct{}

// track returns ctx with a new, empty status slot for one Engine call.
func track(ctx context.Context) context.Context {
	return context.WithValue(ctx, statusSlotKey{}, new(statusSlot))
}

func slotOf(ctx context.Context) *statusSlot {
	s, _ := ctx.Value(statusSlotKey{}).(*statusSlot)
	return s
}

// statusHooks returns the client options that fill the status slot of the
// call a request belongs to. The client runs the hooks for plain requests
// (client@v0.6.2 client.go 252~258) and for the attach upgrade (hijack.go
// 90~96), and a response hook sees every response the daemon sends
// (client_hook.go 45~59).
//
// The request hook empties the slot before each request of the call (an
// API version ping, then the request itself), so a status from an earlier
// request never sticks to a later transport error. The response hook keeps
// only statuses of 400 and above: those are the responses the client turns
// into errors (request.go 221~229) or, for the upgrade, anything but 101
// (hijack.go 103~106).
func statusHooks() []client.Opt {
	return []client.Opt{
		client.WithHTTPRequestHook(func(req *http.Request) error {
			if s := slotOf(req.Context()); s != nil {
				s.code.Store(0)
			}
			return nil
		}),
		client.WithHTTPResponseHook(func(resp *http.Response) {
			if resp.Request == nil || resp.StatusCode < http.StatusBadRequest {
				return
			}
			if s := slotOf(resp.Request.Context()); s != nil {
				s.code.Store(int32(resp.StatusCode))
			}
		}),
	}
}

// opError converts err from the Engine API call made with ctx (see track)
// and records op. It returns nil for a nil err.
func opError(ctx context.Context, op retry.Op, err error) error {
	if err == nil {
		return nil
	}
	return &retry.OpError{Op: op, Err: convert(ctx, err)}
}

// convert attaches the response status to an Engine API error: the status
// recorded for the call, else the one its errdefs class stands for.
// Context errors and transport errors are returned unchanged.
func convert(ctx context.Context, err error) error {
	var se *StatusError
	if errors.As(err, &se) {
		return err
	}
	if s := slotOf(ctx); s != nil {
		if status := int(s.code.Load()); status != 0 {
			return &StatusError{Status: status, Err: err}
		}
	}
	if status := statusOf(err); status != 0 {
		return &StatusError{Status: status, Err: err}
	}
	return err
}

// statusOf returns the status an errdefs class stands for, or 0 when err
// has no class that comes from a single status.
func statusOf(err error) int {
	switch {
	case cerrdefs.IsNotFound(err):
		return http.StatusNotFound
	case cerrdefs.IsConflict(err):
		return http.StatusConflict
	case cerrdefs.IsPermissionDenied(err):
		return http.StatusForbidden
	case cerrdefs.IsUnauthorized(err):
		return http.StatusUnauthorized
	case cerrdefs.IsFailedPrecondition(err):
		return http.StatusPreconditionFailed
	case cerrdefs.IsResourceExhausted(err):
		return http.StatusTooManyRequests
	case cerrdefs.IsInvalidArgument(err):
		return http.StatusBadRequest
	case cerrdefs.IsNotImplemented(err):
		return http.StatusNotImplemented
	case cerrdefs.IsUnavailable(err):
		return http.StatusServiceUnavailable
	case cerrdefs.IsInternal(err):
		return http.StatusInternalServerError
	}
	return 0
}
