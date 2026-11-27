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

// defaultSocket is where docker.DefaultHost points, as the user knows it.
var defaultSocket = strings.TrimPrefix(docker.DefaultHost, "unix://")

// Fixes of the Docker checks.
const (
	fixDockerSocket    = "Docker Desktop을 켜고 Settings > Advanced > Allow the default Docker socket to be used를 켠다"
	fixExposure        = "Docker Desktop의 Expose daemon on tcp://localhost:2375 without TLS를 끄거나, 그 포트를 연 프로그램(socat 등)을 멈춘다"
	fixImageBuild      = "image/build.sh를 실행하고, runner.image가 이미지 ID이면 출력된 runner image ID 값을 설정에 적는다"
	fixImageNoPull     = "그 참조의 이미지를 레지스트리에 올리거나 이름과 태그를 고친다. 비공개 레지스트리의 이미지는 docker pull로 미리 받는다"
	fixImageTag        = "image/build.sh를 실행한다"
	fixRegistryUnknown = "이미지 이름과 네트워크를 확인한 뒤 다시 실행한다"
	fixLimitZero       = "runner와 buildkit의 cpus, memory를 줄이거나 Docker Desktop의 CPU, 메모리를 늘린다"
	fixLimitIdle       = "targets의 idle 합을 서버 한도 이하로 줄이거나 Docker Desktop의 CPU, 메모리를 늘린다"
)

// ping is the Docker check: GET /_ping fails → 실패 (0003 SPEC R303),
// except that no answer in time or a 5xx is 확인 못 함 (R302).
func (c *checker) ping(ctx context.Context) Result {
	var err error
	c.withTimeout(ctx, func(ctx context.Context) { err = c.Docker.Ping(ctx) })
	switch {
	case err == nil:
		return Result{Outcome: Pass, Name: dockerCheck, Summary: defaultSocket + " 응답함"}
	case unansweredDocker(err):
		return unknownCause(dockerCheck, dockerCause(c, err))
	}
	return failed(dockerCheck, defaultSocket+"에 연결하지 못함", err.Error(), fixDockerSocket)
}

// unansweredDocker reports whether err is a Docker call without a usable
// answer in the sense of 0003 SPEC R302: the deadline passed or a 5xx.
func unansweredDocker(err error) bool {
	var sc retry.StatusCoder
	return errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &sc) && sc.HTTPStatus() >= 500)
}

func dockerCause(c *checker, err error) string {
	if errors.Is(err, context.DeadlineExceeded) {
		return fmt.Sprintf("Docker가 %s 안에 응답하지 않음", c.Timeout)
	}
	return err.Error()
}

// exposure is R79's check of 127.0.0.1:2375 under the same deadline.
func (c *checker) exposure(ctx context.Context) Result {
	var e docker.Exposure
	c.withTimeout(ctx, func(ctx context.Context) { e = c.Exposure(ctx) })
	switch e {
	case docker.NotExposed:
		return Result{Outcome: Pass, Name: exposureCheck, Summary: docker.ExposedAddr + "에 Docker API가 없음"}
	case docker.Exposed:
		return failed(exposureCheck, docker.ExposedAddr+"에 Docker API가 노출됨",
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
	name := "이미지 " + key
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
	c.withTimeout(ctx, func(ctx context.Context) { img, exists, err = c.Docker.ImageInspect(ctx, ref) })
	switch {
	case err != nil:
		return unknownCause(name, dockerCause(c, err))
	case !exists && config.ImageIsID(ref):
		return failed(name, "로컬에 없음", "image "+ref+" is not present locally", fixImageBuild)
	case !exists:
		return c.registryImage(ctx, name, ref)
	case img.Architecture != imageArchitecture:
		return failed(name, "아키텍처가 "+imageArchitecture+"가 아님",
			fmt.Sprintf("image %s is %q, not %s", ref, img.Architecture, imageArchitecture), fixImageBuild)
	}
	summary := "로컬에 있음, " + imageArchitecture
	if !config.ImageIsID(ref) {
		summary += ", " + ref + " = " + img.ID
	}
	return Result{Outcome: Pass, Name: name, Summary: summary}
}

// registryImage checks an image reference that is not local with one
// GET /distribution/{name}/json and no pull (0003 SPEC R323); the scaler
// pulls it at start.
func (c *checker) registryImage(ctx context.Context, name, ref string) Result {
	fix := fixImageNoPull
	if ref == docker.RunnerImageTag {
		fix = fixImageTag
	}
	var (
		dist docker.Distribution
		err  error
	)
	c.withTimeout(ctx, func(ctx context.Context) { dist, err = c.Docker.DistributionInspect(ctx, ref) })
	if err != nil {
		var sc retry.StatusCoder
		switch {
		case unansweredDocker(err):
			return unknownCause(name, dockerCause(c, err))
		case errors.As(err, &sc) && sc.HTTPStatus() == http.StatusUnauthorized:
			return failed(name, "레지스트리에서 찾지 못함",
				"레지스트리에 그 이미지가 없거나 읽을 권한이 없음: "+err.Error(), fix)
		case errors.As(err, &sc):
			r := unknownCause(name, fmt.Sprintf("예상하지 않은 응답 코드 %d: %s", sc.HTTPStatus(), err))
			r.Fix = fixRegistryUnknown
			return r
		}
		return unknownCause(name, err.Error())
	}
	switch {
	case len(dist.Platforms) == 0:
		return unknownCause(name, "레지스트리가 이미지의 platform을 알려 주지 않음")
	case !slices.ContainsFunc(dist.Platforms, func(p ocispec.Platform) bool {
		return p.OS == imageOS && p.Architecture == imageArchitecture
	}):
		return failed(name, imageOS+"/"+imageArchitecture+" 이미지가 아님",
			"registry image "+ref+" has no "+imageOS+"/"+imageArchitecture+" platform", fix)
	}
	return Result{Outcome: Pass, Name: name, Summary: "로컬에 없음, 시작 때 pull함"}
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
	c.withTimeout(ctx, func(ctx context.Context) { capacity, err = c.Docker.Info(ctx) })
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
	return Result{Outcome: Pass, Name: limitCheck, Summary: fmt.Sprintf("runner %d개", limit)}
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
