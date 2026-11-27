package doctor

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/dewevrei/actions-runner-scaler/internal/config"
	"github.com/dewevrei/actions-runner-scaler/internal/docker"
	"github.com/dewevrei/actions-runner-scaler/internal/retry"
)

// The only platform the runners run on (AGENTS.md: Docker Desktop on Apple
// Silicon).
const (
	imageOS           = "linux"
	imageArchitecture = "arm64"
)

// Fixes of the Docker checks.
const (
	fixDockerSocket     = "start Docker Desktop and turn on Settings > Advanced > Allow the default Docker socket to be used"
	fixDockerHostSocket = "start Docker and check that the socket path in docker.host is the socket Docker opens (docker context inspect)"
	fixDockerTCP        = "check that Docker listens at that address and that the host and port in docker.host are right"
	fixExposure         = "turn off Expose daemon on tcp://localhost:2375 without TLS in Docker Desktop, or stop the program that opened that port (such as socat)"
	fixImageBuild       = "run image/build.sh and, if runner.image is an image ID, write the runner image ID it prints in the config"
	fixImageNoPull      = "push the image of that reference to the registry or fix its name and tag; pull an image of a private registry with docker pull first"
	fixImageTag         = "run image/build.sh"
	fixRegistryUnknown  = "check the image name and the network, then run doctor again"
	fixLimitZero        = "lower cpus and memory of runner and buildkit, or raise the CPU and memory of Docker Desktop"
	fixLimitIdle        = "lower the sum of idle in targets to at most the server limit, or raise the CPU and memory of Docker Desktop"
)

// ping is the Docker check on the host docker.host names: GET /_ping fails
// → FAIL (0003 SPEC R303), except that no answer in time or a 5xx is
// UNKNOWN (R302). A refused docker.host leaves it to the Config check.
func (c *checker) ping(ctx context.Context) Result {
	if c.host == "" {
		return blocked(dockerCheck, []string{configCheck})
	}
	shown, fix := shownHost(c.host)
	var err error
	c.withTimeout(ctx, func(ctx context.Context) { err = c.docker.Ping(ctx) })
	switch {
	case err == nil:
		return Result{Outcome: Pass, Name: dockerCheck, Summary: shown + " answered"}
	case unansweredDocker(err):
		return unknownCause(dockerCheck, dockerCause(c, err))
	}
	return failed(dockerCheck, "could not connect to "+shown, err.Error(), fix)
}

// shownHost returns how the Docker line names host, a valid docker.host
// value, and the fix of a host that does not answer: the socket path of a
// unix:// URL, which holds nothing secret, and a tcp:// URL as written,
// which validation keeps free of a user name or password.
func shownHost(host string) (shown, fix string) {
	switch path, ok := strings.CutPrefix(host, "unix://"); {
	case host == config.DefaultDockerHost:
		return path, fixDockerSocket
	case ok:
		return path, fixDockerHostSocket
	}
	return host, fixDockerTCP
}

// unansweredDocker reports whether err is a Docker call without a usable
// answer in the sense of 0003 SPEC R302: the deadline passed or a 5xx.
func unansweredDocker(err error) bool {
	var sc retry.StatusCoder
	return errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &sc) && sc.HTTPStatus() >= 500)
}

func dockerCause(c *checker, err error) string {
	if errors.Is(err, context.DeadlineExceeded) {
		return fmt.Sprintf("Docker did not answer within %s", c.Timeout)
	}
	return err.Error()
}

// exposure is R79's check of 127.0.0.1:2375 under the same deadline.
func (c *checker) exposure(ctx context.Context) Result {
	var e docker.Exposure
	c.withTimeout(ctx, func(ctx context.Context) { e = c.Exposure(ctx) })
	switch e {
	case docker.NotExposed:
		return Result{Outcome: Pass, Name: exposureCheck, Summary: "no Docker API on " + docker.ExposedAddr}
	case docker.Exposed:
		return failed(exposureCheck, "Docker API exposed on "+docker.ExposedAddr,
			"Docker API answers on "+docker.ExposedAddr+" without TLS (R79)", fixExposure)
	}
	return unknownCause(exposureCheck, "could not tell whether the Docker daemon is exposed on TCP 2375")
}

// images checks runner.image and buildkit.image (0003 SPEC R303).
func (c *checker) images(ctx context.Context, in *config.Inspection, ping Result) []Result {
	var runner, buildkit string
	if in != nil {
		runner, buildkit = in.Config.Runner.Image, in.Config.BuildKit.Image
	}
	return []Result{
		c.image(ctx, in, ping, "runner.image", runner),
		c.image(ctx, in, ping, "buildkit.image", buildkit),
	}
}

func (c *checker) image(ctx context.Context, in *config.Inspection, ping Result, key, ref string) Result {
	name := "Image " + key
	var by []string
	if in == nil || in.Failed(key) {
		by = append(by, configCheck)
	}
	if ping.Outcome != Pass {
		by = append(by, dockerCheck)
	}
	if len(by) > 0 {
		return blocked(name, by)
	}

	var (
		img    docker.Image
		exists bool
		err    error
	)
	c.withTimeout(ctx, func(ctx context.Context) { img, exists, err = c.docker.ImageInspect(ctx, ref) })
	switch {
	case err != nil:
		return unknownCause(name, dockerCause(c, err))
	case !exists && config.ImageIsID(ref):
		return failed(name, "not present locally", "image "+ref+" is not present locally", fixImageBuild)
	case !exists:
		return c.registryImage(ctx, name, key, ref)
	case img.Architecture != imageArchitecture:
		return failed(name, "architecture is not "+imageArchitecture,
			fmt.Sprintf("image %s is %q, not %s", ref, img.Architecture, imageArchitecture), fixImageBuild)
	}
	summary := "present locally, " + imageArchitecture
	if !config.ImageIsID(ref) {
		summary += ", " + ref + " = " + img.ID
	}
	return Result{Outcome: Pass, Name: name, Summary: summary}
}

// registryImage checks an image reference that is not local with one
// GET /distribution/{name}/json and no pull (0003 SPEC R323); the scaler
// pulls it at start.
func (c *checker) registryImage(ctx context.Context, name, key, ref string) Result {
	fix := fixImageNoPull
	if key == "runner.image" && ref == docker.RunnerImageTag {
		fix = fixImageTag
	}
	var (
		dist docker.Distribution
		err  error
	)
	c.withTimeout(ctx, func(ctx context.Context) { dist, err = c.docker.DistributionInspect(ctx, ref) })
	if err != nil {
		var sc retry.StatusCoder
		switch {
		case unansweredDocker(err):
			return unknownCause(name, dockerCause(c, err))
		case errors.As(err, &sc) && sc.HTTPStatus() == http.StatusUnauthorized:
			return failed(name, "not found in the registry",
				"the registry has no such image or denies reading it: "+err.Error(), fix)
		case errors.As(err, &sc):
			r := unknownCause(name, fmt.Sprintf("unexpected response code %d: %s", sc.HTTPStatus(), err))
			r.Fix = fixRegistryUnknown
			return r
		}
		return unknownCause(name, err.Error())
	}
	switch {
	case len(dist.Platforms) == 0:
		return unknownCause(name, "the registry did not report the image's platforms")
	case !slices.ContainsFunc(dist.Platforms, func(p ocispec.Platform) bool {
		return p.OS == imageOS && p.Architecture == imageArchitecture
	}):
		return failed(name, "not a "+imageOS+"/"+imageArchitecture+" image",
			"registry image "+ref+" has no "+imageOS+"/"+imageArchitecture+" platform", fix)
	}
	return Result{Outcome: Pass, Name: name, Summary: "not present locally, pulled at start"}
}

// serverLimit is 0002 SPEC R201, R202 and R167 on one /info answer.
func (c *checker) serverLimit(ctx context.Context, in *config.Inspection, ping Result) Result {
	var by []string
	if in == nil || resourcesFailed(in) {
		by = append(by, configCheck)
	}
	if ping.Outcome != Pass {
		by = append(by, dockerCheck)
	}
	if len(by) > 0 {
		return blocked(limitCheck, by)
	}

	var (
		capacity docker.Capacity
		err      error
	)
	c.withTimeout(ctx, func(ctx context.Context) { capacity, err = c.docker.Info(ctx) })
	if err != nil {
		return unknownCause(limitCheck, dockerCause(c, err))
	}
	cfg := in.Config
	share := docker.RunnerShare(docker.ContainerLimits(cfg.Runner), docker.ContainerLimits(cfg.BuildKit))
	limit := docker.ServerLimit(capacity, share)
	if limit == 0 {
		return failed(limitCheck, "0", fmt.Sprintf(
			"server runner limit is 0: one runner reserves %s CPUs (runner.cpus %s + buildkit.cpus %s) and %d bytes (runner.memory %s + buildkit.memory %s), Docker reports %d CPUs and %d bytes",
			formatCPUs(float64(share.NanoCPUs)/1e9), formatCPUs(cfg.Runner.CPUs), formatCPUs(cfg.BuildKit.CPUs),
			share.Memory, cfg.Runner.Memory, cfg.BuildKit.Memory, capacity.NCPU, capacity.MemTotal), fixLimitZero)
	}
	idle := 0
	for _, t := range cfg.Targets {
		idle += t.Idle
	}
	if idle > limit {
		return failed(limitCheck, strconv.Itoa(limit),
			fmt.Sprintf("idle of all targets (%d) exceeds the server runner limit (%d)", idle, limit), fixLimitIdle)
	}
	return Result{Outcome: Pass, Name: limitCheck, Summary: count(limit, "runner")}
}

// resourcesFailed reports whether a key the server limit reads failed
// validation or decoding.
func resourcesFailed(in *config.Inspection) bool {
	keys := []string{"runner.cpus", "runner.memory", "buildkit.cpus", "buildkit.memory"}
	for i := range in.Config.Targets {
		keys = append(keys, fmt.Sprintf("targets[%d].idle", i))
	}
	for _, k := range keys {
		if in.Failed(k) {
			return true
		}
	}
	return false
}

// formatCPUs prints a CPU count without trailing zeros.
func formatCPUs(v float64) string { return strconv.FormatFloat(v, 'f', -1, 64) }
