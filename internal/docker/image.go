package docker

import (
	"context"

	specs "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/dewevrei/actions-runner-scaler/internal/retry"
)

const (
	// RunnerImageTag is the tag image/build.sh gives the runner image
	// (RUNNER_TAG there). init reads the image ID from it (0003 SPEC R286).
	RunnerImageTag = "actions-runner-scaler-runner:local"

	// BuildKitImage is the BuildKit image image/build.sh pulls
	// (BUILDKIT_IMAGE there); a test keeps the two equal.
	BuildKitImage = "moby/buildkit:v0.35.1-rootless@sha256:7d02292556e66feea6d2d89b7ec143503b57e4f59a4efc6b7ece8547e4d3e5be"
)

// The values of the labels image/build.sh puts on the runner image (0003
// SPEC R316).
const (
	RunnerImageSource = "https://github.com/dewevrei/actions-runner-scaler"
	RunnerImageTitle  = "actions-runner-scaler-runner"
)

// RunnerImageLabels returns the labels image/build.sh puts on the runner
// image (0003 SPEC R316), under the OCI pre-defined annotation keys. Both
// values must match for an image to count as built by this project's
// script. They are a mark, not proof of origin: anyone can set a label.
func RunnerImageLabels() map[string]string {
	return map[string]string{
		specs.AnnotationSource: RunnerImageSource,
		specs.AnnotationTitle:  RunnerImageTitle,
	}
}

// HasRunnerImageLabels reports whether labels hold both runner image labels
// with exactly their values.
func HasRunnerImageLabels(labels map[string]string) bool {
	src, ok := labels[specs.AnnotationSource]
	if !ok || src != RunnerImageSource {
		return false
	}
	title, ok := labels[specs.AnnotationTitle]
	return ok && title == RunnerImageTitle
}

// Image is the part of a local image's inspection that the scaler reads.
type Image struct {
	// ID is the image ID, such as "sha256:...".
	ID string
	// Architecture is the CPU architecture the image runs on, such as
	// "arm64".
	Architecture string
	// Labels are the image configuration's labels; nil when there are none.
	Labels map[string]string
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
	img := Image{ID: res.ID, Architecture: res.Architecture}
	if res.Config != nil {
		img.Labels = res.Config.Labels
	}
	return img, true, nil
}
