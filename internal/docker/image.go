package docker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/distribution/reference"
	"github.com/moby/moby/api/types/jsonstream"
	"github.com/moby/moby/client"
	specs "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/dewevrei/actions-runner-scaler/internal/retry"
)

const (
	// RunnerImageTag is the tag image/build.sh gives the runner image
	// (RUNNER_TAG there). init writes it as runner.image when the image
	// exists locally (0003 SPEC R286, R322).
	RunnerImageTag = "actions-runner-scaler-runner:local"

	// BuildKitImage is the BuildKit image image/build.sh pulls
	// (BUILDKIT_IMAGE there); a test keeps the two equal.
	BuildKitImage = "moby/buildkit:v0.35.1-rootless@sha256:7d02292556e66feea6d2d89b7ec143503b57e4f59a4efc6b7ece8547e4d3e5be"
)

// Image is the part of a local image's inspection that the scaler reads.
type Image struct {
	// ID is the image ID, such as "sha256:...".
	ID string
	// Architecture is the CPU architecture the image runs on, such as
	// "arm64".
	Architecture string
}

// ImageInspect returns the local image ref with one GET /images/{name}/json
// call. It never pulls. An image that is not present returns false and a nil
// error; any other failure is a *retry.OpError for retry.OpImageInspect.
func (c *Client) ImageInspect(ctx context.Context, ref string) (Image, bool, error) {
	ctx = track(ctx)
	res, err := c.engine.ImageInspect(ctx, ref)
	if err != nil {
		if IsNotFound(err) {
			return Image{}, false, nil
		}
		return Image{}, false, opError(ctx, retry.OpImageInspect, err)
	}
	return Image{ID: res.ID, Architecture: res.Architecture}, true, nil
}

// ImagePull pulls the image reference ref with one POST /images/create call
// and reads the response stream to its end (0003 SPEC R320). It sends no
// registry credential, so it pulls only what needs none, and sets neither
// All nor a platform. The daemon's progress is read and dropped.
//
// A transport error or a 5xx answer to the request is a *retry.OpError for
// retry.OpImagePull that keeps its status; retry.Classify calls it
// Transient. Every other failure (a 4xx answer, an error message inside the
// stream, a stream that is not valid JSON, the deadline of ctx, a ref that
// is not an image reference) is such an error wrapping retry.ErrPullFailed
// with Docker's text kept by %v, so Classify calls it Permanent and no
// context.DeadlineExceeded stays in its chain. An error while reading the
// stream (a dropped connection, an unexpected end) keeps no sentinel, like
// a transport error of the request. A canceled ctx keeps context.Canceled
// in the chain, so Classify reads it as Canceled.
func (c *Client) ImagePull(ctx context.Context, ref string) error {
	// The Engine client parses ref before its request and returns an error
	// with no status, which would read as a transport error.
	if _, err := reference.ParseNormalizedNamed(ref); err != nil {
		return pullFailed(err)
	}
	ctx = track(ctx)
	resp, err := c.engine.ImagePull(ctx, ref, client.ImagePullOptions{})
	if err != nil {
		return requestPullError(convert(ctx, err))
	}
	defer resp.Close()
	if err := resp.Wait(ctx); err != nil {
		return streamPullError(err)
	}
	return nil
}

// streamPullError returns the error of a pull whose response stream failed
// with err, the error of Wait. A message that Docker put in the stream, a
// stream that is not valid JSON and the pull's deadline are failures that
// R320 sends to the error wait. A canceled context and a read error (a
// dropped connection, an unexpected end of the stream) are not: the first
// is Canceled, the second a transport error, which 0001 R24 retries.
func streamPullError(err error) error {
	var (
		message *jsonstream.Error
		syntax  *json.SyntaxError
		typed   *json.UnmarshalTypeError
	)
	switch {
	case errors.Is(err, context.Canceled):
		return &retry.OpError{Op: retry.OpImagePull, Err: err}
	case errors.Is(err, context.DeadlineExceeded),
		errors.As(err, &message), errors.As(err, &syntax), errors.As(err, &typed):
		return pullFailed(err)
	}
	return &retry.OpError{Op: retry.OpImagePull, Err: err}
}

// requestPullError returns the error of a pull whose request failed with
// err, which has passed through convert.
func requestPullError(err error) error {
	if errors.Is(err, context.Canceled) {
		return &retry.OpError{Op: retry.OpImagePull, Err: err}
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return pullFailed(err)
	}
	var se *StatusError
	if errors.As(err, &se) && se.Status < http.StatusInternalServerError {
		return pullFailed(err)
	}
	// A 5xx answer, or a transport error that has no status.
	return &retry.OpError{Op: retry.OpImagePull, Err: err}
}

// pullFailed returns the error of a pull that failed for a reason that
// R320 sends to the error wait.
func pullFailed(err error) error {
	return &retry.OpError{Op: retry.OpImagePull, Err: fmt.Errorf("%w: %v", retry.ErrPullFailed, err)}
}

// Distribution is the part of a registry lookup of an image that the scaler
// reads.
type Distribution struct {
	// Platforms are the platforms of the image's manifest; empty when the
	// registry gave none.
	Platforms []specs.Platform
}

// DistributionInspect asks the registry, through the daemon, about the image
// reference ref with one GET /distribution/{name}/json call (0003 SPEC
// R323). It sends no registry credential and pulls nothing. A failure is a
// *retry.OpError for retry.OpDistributionInspect that keeps the response
// status.
func (c *Client) DistributionInspect(ctx context.Context, ref string) (Distribution, error) {
	ctx = track(ctx)
	res, err := c.engine.DistributionInspect(ctx, ref, client.DistributionInspectOptions{})
	if err != nil {
		return Distribution{}, opError(ctx, retry.OpDistributionInspect, err)
	}
	return Distribution{Platforms: res.Platforms}, nil
}
