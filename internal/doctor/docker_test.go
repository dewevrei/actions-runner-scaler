package doctor

import (
	"context"
	"errors"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/dewevrei/actions-runner-scaler/internal/docker"
	"github.com/dewevrei/actions-runner-scaler/internal/retry"
)

func TestDoctorDockerChecks(t *testing.T) {
	gh := newGitHubServer(t, map[string]reply{
		"/repos/o/a/actions/runners": {status: 200, body: runnersOK, header: map[string]string{"X-OAuth-Scopes": "repo"}},
		"/repos/o/b/actions/runners": {status: 200, body: runnersOK, header: map[string]string{"X-OAuth-Scopes": "repo"}},
	})
	tokenLine := "token-file = \"" + writeTokenFile(t, fixtureToken("ghp_")) + "\""
	oneTarget := writeConfig(t, tokenLine, target("o/a"))
	idleTwo := writeConfig(t, tokenLine, target("o/a", "idle = 1"), target("o/b", "idle = 1"))

	tests := []struct {
		name     string
		config   string
		docker   func(*fakeDocker)
		exposure docker.Exposure
		want     []string
	}{
		{name: "ping fails", config: oneTarget,
			docker: func(d *fakeDocker) { d.pingErr = errors.New("connect: connection refused") },
			want:   []string{"실패 Docker: /var/run/docker.sock에 연결하지 못함\n  원인: connect: connection refused\n  고치는 법: " + fixDockerSocket}},
		{name: "ping 500", config: oneTarget,
			docker: func(d *fakeDocker) {
				d.pingErr = &retry.OpError{Op: retry.OpDockerPing, Err: &docker.StatusError{Status: 500, Err: errors.New("starting")}}
			},
			want: []string{"확인 못 함 Docker: 확인하지 못함"}},
		{name: "2375 exposed", config: oneTarget, exposure: docker.Exposed,
			want: []string{"실패 Docker TCP 2375: 127.0.0.1:2375에 Docker API가 노출됨\n  원인: Docker API answers on 127.0.0.1:2375 without TLS (R79)\n  고치는 법: " + fixExposure}},
		{name: "2375 unknown", config: oneTarget, exposure: docker.ExposureUnknown,
			want: []string{"확인 못 함 Docker TCP 2375: 확인하지 못함\n  원인: could not tell whether the Docker daemon is exposed on TCP 2375"}},
		{name: "image missing", config: oneTarget,
			docker: func(d *fakeDocker) { delete(d.images, runnerImageID) },
			want: []string{"실패 이미지 runner.image: 로컬에 없음\n  원인: image " + runnerImageID + " is not present locally\n  고치는 법: " + fixImageBuild,
				"통과 이미지 buildkit.image: 로컬에 있음, arm64, " + docker.BuildKitImage + " = " + buildkitID}},
		{name: "amd64", config: oneTarget,
			docker: func(d *fakeDocker) {
				img := d.images[runnerImageID]
				img.Architecture = "amd64"
				d.images[runnerImageID] = img
			},
			want: []string{"실패 이미지 runner.image: 아키텍처가 arm64가 아님\n  원인: image " + runnerImageID + ` is "amd64", not arm64`}},
		{name: "runner image without labels", config: oneTarget,
			docker: func(d *fakeDocker) {
				img := d.images[runnerImageID]
				img.Labels = map[string]string{"org.opencontainers.image.source": "https://github.com/actions/runner"}
				d.images[runnerImageID] = img
			},
			want: []string{"확인 못 함 이미지 runner.image: 확인하지 못함\n  원인: 이 프로젝트의 image/build.sh로 만든 이미지인지 알 수 없음(라벨 없음)\n  고치는 법: " + fixImageBuild,
				"통과 이미지 buildkit.image: 로컬에 있음, arm64, " + docker.BuildKitImage + " = " + buildkitID}},
		{name: "limit 0", config: oneTarget,
			docker: func(d *fakeDocker) { d.info = docker.Capacity{NCPU: 2, MemTotal: 16 << 30} },
			want:   []string{"실패 서버 한도: 0\n  원인: server runner limit is 0: one runner reserves 3 CPUs (runner.cpus 1 + buildkit.cpus 2) and 3221225472 bytes (runner.memory 1g + buildkit.memory 2g), Docker reports 2 CPUs and 17179869184 bytes\n  고치는 법: " + fixLimitZero}},
		{name: "idle above limit", config: idleTwo,
			docker: func(d *fakeDocker) { d.info = docker.Capacity{NCPU: 3, MemTotal: 16 << 30} },
			want:   []string{"실패 서버 한도: 1\n  원인: idle of all targets (2) exceeds the server runner limit (1)\n  고치는 법: " + fixLimitIdle}},
		{name: "pass names the limit", config: idleTwo,
			want: []string{"통과 서버 한도: runner 2개", "통과 Docker: /var/run/docker.sock 응답함", "통과 Docker TCP 2375: 127.0.0.1:2375에 Docker API가 없음"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := goodDocker()
			if tt.docker != nil {
				tt.docker(d)
			}
			deps, _ := testDeps(tt.config, gh, d)
			deps.Exposure = func(context.Context) docker.Exposure { return tt.exposure }
			out, _ := output(t, deps)
			for _, want := range tt.want {
				if got := block(out, want[:strings.IndexByte(want, ':')+1]); !strings.HasPrefix(got, want) {
					t.Errorf("got:\n%s\nwant:\n%s\nall:\n%s", got, want, out)
				}
			}
		})
	}
}

// runnerTag is a runner.image value that is a reference, as init writes it.
const runnerTag = "actions-runner-scaler-runner:local"

// writeReferenceConfig writes a configuration whose runner.image is
// runnerTag and whose buildkit.image is docker.BuildKitImage.
func writeReferenceConfig(t *testing.T) string {
	t.Helper()
	path := writeConfig(t, "token-file = \""+writeTokenFile(t, fixtureToken("ghp_"))+"\"", target("o/a"))
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	s := strings.Replace(string(data), runnerImageID, runnerTag, 1)
	if err := os.WriteFile(path, []byte(s), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func platforms(ps ...string) docker.Distribution {
	var d docker.Distribution
	for _, p := range ps {
		goos, arch, _ := strings.Cut(p, "/")
		d.Platforms = append(d.Platforms, ocispec.Platform{OS: goos, Architecture: arch})
	}
	return d
}

func distributionError(status int, text string) error {
	return &retry.OpError{Op: retry.OpDistributionInspect, Err: &docker.StatusError{Status: status, Err: errors.New(text)}}
}

// referenceGitHub is the GitHub side of the reference tests: one token
// target that passes.
func referenceGitHub(t *testing.T) *githubServer {
	return newGitHubServer(t, map[string]reply{
		"/repos/o/a/actions/runners": {status: 200, body: runnersOK, header: map[string]string{"X-OAuth-Scopes": "repo"}},
	})
}

func TestDoctorImageReference(t *testing.T) {
	localID := "sha256:" + strings.Repeat("ef", 32)
	localRef := func(d *fakeDocker) {
		d.images = map[string]docker.Image{
			runnerTag:            {ID: localID, Architecture: "arm64", Labels: docker.RunnerImageLabels()},
			docker.BuildKitImage: {ID: localID, Architecture: "arm64"},
		}
	}
	notLocal := func(d *fakeDocker) { d.images = nil }
	unknown := func(key, cause string) string {
		return "확인 못 함 이미지 " + key + ": 확인하지 못함\n  원인: " + cause
	}

	tests := []struct {
		name   string
		docker func(d *fakeDocker)
		// timeout replaces the limit of the test.
		timeout time.Duration
		want    []string
		// distributions is how many registry calls the run makes.
		distributions int
	}{
		{name: "local reference", docker: localRef, want: []string{
			"통과 이미지 runner.image: 로컬에 있음, arm64, " + runnerTag + " = " + localID,
			"통과 이미지 buildkit.image: 로컬에 있음, arm64, " + docker.BuildKitImage + " = " + localID}},
		{name: "registry has linux/arm64", distributions: 2,
			docker: func(d *fakeDocker) { notLocal(d); d.dist = platforms("linux/amd64", "linux/arm64") },
			want: []string{
				unknown("runner.image", "로컬에 없어 R316의 라벨을 볼 수 없음, 스케일러가 시작 때 pull함"),
				"통과 이미지 buildkit.image: 로컬에 없음, 시작 때 pull함"}},
		{name: "linux/amd64 only", distributions: 2,
			docker: func(d *fakeDocker) { notLocal(d); d.dist = platforms("linux/amd64", "windows/arm64") },
			want: []string{
				"실패 이미지 runner.image: linux/arm64 이미지가 아님",
				"실패 이미지 buildkit.image: linux/arm64 이미지가 아님"}},
		{name: "empty platforms", distributions: 2,
			docker: func(d *fakeDocker) { notLocal(d) },
			want: []string{
				unknown("runner.image", "레지스트리가 이미지의 platform을 알려 주지 않음"),
				unknown("buildkit.image", "레지스트리가 이미지의 platform을 알려 주지 않음")}},
		{name: "401", distributions: 2,
			docker: func(d *fakeDocker) { notLocal(d); d.distErr = distributionError(401, "pull access denied") },
			want: []string{
				"실패 이미지 runner.image: 레지스트리에서 찾지 못함\n  원인: 레지스트리에 그 이미지가 없거나 읽을 권한이 없음: inspect image in registry: status 401: pull access denied",
				"실패 이미지 buildkit.image: 레지스트리에서 찾지 못함\n  원인: 레지스트리에 그 이미지가 없거나 읽을 권한이 없음: inspect image in registry: status 401: pull access denied"}},
		{name: "timeout", distributions: 2, timeout: 50 * time.Millisecond,
			docker: func(d *fakeDocker) { notLocal(d); d.hangDist = true },
			want: []string{
				unknown("runner.image", "Docker가 50ms 안에 응답하지 않음"),
				unknown("buildkit.image", "Docker가 50ms 안에 응답하지 않음")}},
		{name: "503", distributions: 2,
			docker: func(d *fakeDocker) { notLocal(d); d.distErr = distributionError(503, "unavailable") },
			want: []string{
				unknown("runner.image", "inspect image in registry: status 503: unavailable"),
				unknown("buildkit.image", "inspect image in registry: status 503: unavailable")}},
		{name: "418", distributions: 2,
			docker: func(d *fakeDocker) { notLocal(d); d.distErr = distributionError(418, "teapot") },
			want: []string{
				unknown("runner.image", "예상하지 않은 응답 코드 418"),
				unknown("buildkit.image", "예상하지 않은 응답 코드 418")}},
	}
	gh := referenceGitHub(t)
	path := writeReferenceConfig(t)
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := goodDocker()
			tt.docker(d)
			deps, _ := testDeps(path, gh, d)
			if tt.timeout != 0 {
				deps.Timeout = tt.timeout
			}
			out, _ := output(t, deps)
			for _, want := range tt.want {
				if got := block(out, want[:strings.Index(want, ":")+1]); !strings.HasPrefix(got, want) {
					t.Errorf("got:\n%s\nwant:\n%s\nall:\n%s", got, want, out)
				}
			}
			n := 0
			for _, c := range d.Calls() {
				if strings.HasPrefix(c, "distribution ") {
					n++
				}
			}
			if n != tt.distributions {
				t.Errorf("distribution calls = %d, want %d: %v", n, tt.distributions, d.Calls())
			}
		})
	}

	t.Run("missing image ID is not looked up", func(t *testing.T) {
		d := goodDocker()
		delete(d.images, runnerImageID)
		d.dist = platforms("linux/arm64")
		deps, _ := testDeps(writeConfig(t, "token-file = \""+writeTokenFile(t, fixtureToken("ghp_"))+"\"", target("o/a")), gh, d)
		out, _ := output(t, deps)
		if got := block(out, "실패 이미지 runner.image:"); !strings.Contains(got, "로컬에 없음") {
			t.Errorf("output:\n%s", out)
		}
		if slices.Contains(d.Calls(), "distribution "+runnerImageID) {
			t.Errorf("registry asked about an image ID: %v", d.Calls())
		}
	})
}

func TestDoctorOnlyGET(t *testing.T) {
	var methods []string
	probe := reflect.TypeFor[DockerProbe]()
	for i := range probe.NumMethod() {
		methods = append(methods, probe.Method(i).Name)
	}
	if want := []string{"DistributionInspect", "ImageInspect", "Info", "Ping"}; !slices.Equal(methods, want) {
		t.Errorf("DockerProbe methods = %v, want %v: doctor has no pull", methods, want)
	}

	d := goodDocker()
	d.images = nil
	d.dist = platforms("linux/arm64")
	deps, _ := testDeps(writeReferenceConfig(t), referenceGitHub(t), d)
	output(t, deps)
	want := []string{
		"ping", "info",
		"image " + runnerTag, "distribution " + runnerTag,
		"image " + docker.BuildKitImage, "distribution " + docker.BuildKitImage,
	}
	got := d.Calls()
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Errorf("Docker calls = %v, want %v", got, want)
	}
}
