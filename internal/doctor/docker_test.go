package doctor

import (
	"context"
	"errors"
	"strings"
	"testing"

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
			docker: func(d *fakeDocker) { delete(d.images, docker.BuildKitImage) },
			want: []string{"실패 이미지 buildkit.image: 로컬에 없음\n  원인: image " + docker.BuildKitImage + " is not present locally\n  고치는 법: " + fixImageBuild,
				"통과 이미지 runner.image: 로컬에 있음, arm64"}},
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
				"통과 이미지 buildkit.image: 로컬에 있음, arm64"}},
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
