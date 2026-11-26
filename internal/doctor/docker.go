package doctor

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/dewevrei/actions-runner-scaler/internal/config"
	"github.com/dewevrei/actions-runner-scaler/internal/docker"
	"github.com/dewevrei/actions-runner-scaler/internal/retry"
)

// imageArchitecture is the only platform the runners run on (AGENTS.md:
// Docker Desktop on Apple Silicon).
const imageArchitecture = "arm64"

// defaultSocket is where docker.DefaultHost points, as the user knows it.
var defaultSocket = strings.TrimPrefix(docker.DefaultHost, "unix://")

// Fixes of the Docker checks.
const (
	fixDockerSocket = "Docker Desktop을 켜고 Settings > Advanced > Allow the default Docker socket to be used를 켠다"
	fixExposure     = "Docker Desktop의 Expose daemon on tcp://localhost:2375 without TLS를 끄거나, 그 포트를 연 프로그램(socat 등)을 멈춘다"
	fixImageBuild   = "image/build.sh를 실행하고 그 출력의 값을 설정에 적는다"
	fixLimitZero    = "runner와 buildkit의 cpus, memory를 줄이거나 Docker Desktop의 CPU, 메모리를 늘린다"
	fixLimitIdle    = "targets의 idle 합을 서버 한도 이하로 줄이거나 Docker Desktop의 CPU, 메모리를 늘린다"
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
		c.image(ctx, in, ping, "runner.image", runner, true),
		c.image(ctx, in, ping, "buildkit.image", buildkit, false),
	}
}

func (c *checker) image(ctx context.Context, in *config.Inspection, ping Result, key, ref string, labelled bool) Result {
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
	case !exists:
		return failed(name, "로컬에 없음", "image "+ref+" is not present locally", fixImageBuild)
	case img.Architecture != imageArchitecture:
		return failed(name, "아키텍처가 "+imageArchitecture+"가 아님",
			fmt.Sprintf("image %s is %q, not %s", ref, img.Architecture, imageArchitecture), fixImageBuild)
	case labelled && !docker.HasRunnerImageLabels(img.Labels):
		return Result{
			Outcome: Unknown, Name: name, Summary: noAnswerSummary,
			Causes: []string{"이 프로젝트의 image/build.sh로 만든 이미지인지 알 수 없음(라벨 없음)"},
			Fix:    fixImageBuild,
		}
	}
	return Result{Outcome: Pass, Name: name, Summary: "로컬에 있음, " + imageArchitecture}
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
