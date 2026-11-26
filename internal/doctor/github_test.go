package doctor

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/dewevrei/actions-runner-scaler/internal/docker"
)

const runnersOK = `{"total_count":0,"runners":[]}`

func TestDoctorAppCheck(t *testing.T) {
	keyPath := writeAppKey(t, 0o600)
	tests := []struct {
		name  string
		reply reply
		want  string
	}{
		{"write", reply{status: 200, body: installationWrite}, "통과 GitHub o/a: App 설치 42, administration: write"},
		{"read", reply{status: 200, body: `{"id":42,"permissions":{"administration":"read"}}`},
			"실패 GitHub o/a: App 권한 부족\n  원인: GitHub App installation on repository o/a lacks administration: write\n  고치는 법: " + fixAppPermission},
		{"404", reply{status: 404, body: `{"message":"Not Found"}`},
			"실패 GitHub o/a: App이 설치되지 않음\n  원인: GitHub App is not installed on repository o/a\n  고치는 법: " + fixAppInstall},
		{"401", reply{status: 401, body: `{"message":"Bad credentials"}`},
			"실패 GitHub o/a: App 인증 거부\n  원인: GitHub가 App의 JWT를 받지 않음(401)\n  고치는 법: " + fixAppJWT},
		{"500", reply{status: 500, body: `{"message":"oops"}`},
			"확인 못 함 GitHub o/a: 확인하지 못함\n  원인: GitHub가 500로 응답함\n  고치는 법: " + fixAgain},
		{"418", reply{status: 418, body: `{"message":"teapot"}`},
			"확인 못 함 GitHub o/a: 확인하지 못함\n  원인: 예상하지 않은 응답 코드 418\n  고치는 법: " + fixCodeUnknown},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := writeConfig(t, appLines(keyPath), target("o/a"))
			gh := newGitHubServer(t, map[string]reply{"/repos/o/a/installation": tt.reply})
			deps, _ := testDeps(path, gh, goodDocker())
			out, _ := output(t, deps)
			if got := block(out, tt.want[:strings.IndexByte(tt.want, ':')+1]); got != tt.want {
				t.Errorf("got:\n%s\nwant:\n%s", got, tt.want)
			}
			if got := gh.Requests(); len(got) != 1 || got[0] != "GET /repos/o/a/installation" {
				t.Errorf("requests = %v", got)
			}
			if auth := gh.auth[0]; !strings.HasPrefix(auth, "Bearer ey") {
				t.Errorf("request not made with the App JWT")
			}
		})
	}
}

func TestDoctorTokenCheck(t *testing.T) {
	const (
		runners  = "/repos/o/a/actions/runners"
		policies = "/repos/o/a/actions/policies"
	)
	scopes := func(v string) reply {
		return reply{status: 200, body: runnersOK, header: map[string]string{"X-OAuth-Scopes": v}}
	}
	refused403 := "personal access token for repository o/a cannot manage its runners (403): a classic token needs the repo scope and admin access, a fine-grained token needs Administration: Read and write on this repository"
	tests := []struct {
		name     string
		prefix   string
		replies  map[string]reply
		want     string
		requests []string
	}{
		{"401", "ghp_", map[string]reply{runners: {status: 401, body: `{"message":"Bad credentials"}`}},
			"실패 GitHub o/a: 토큰 거부(401)\n  원인: personal access token for repository o/a was refused (401): it is invalid, expired or revoked\n  고치는 법: " + fixTokenRefused, nil},
		{"403", "ghp_", map[string]reply{runners: {status: 403, body: `{"message":"Must have admin rights"}`}},
			"실패 GitHub o/a: 토큰 권한 부족(403)\n  원인: " + refused403 + "\n  고치는 법: " + fixTokenAccess, nil},
		{"404", "github_pat_", map[string]reply{runners: {status: 404, body: `{"message":"Not Found"}`}},
			"실패 GitHub o/a: 토큰 권한 부족(404)\n  원인: " + strings.ReplaceAll(refused403, "403", "404") + "\n  고치는 법: " + fixTokenAccess, nil},
		{"fine-grained policies 200", "github_pat_", map[string]reply{runners: {status: 200, body: runnersOK}, policies: {status: 200, body: `{}`}},
			"통과 GitHub o/a: fine-grained 토큰, Administration: write", []string{"GET " + runners + "?per_page=1", "GET " + policies + "?per_page=1"}},
		{"fine-grained policies 403", "github_pat_", map[string]reply{runners: {status: 200, body: runnersOK}, policies: {status: 403, body: `{}`}},
			"실패 GitHub o/a: 토큰 권한 부족(403)\n  원인: 토큰에 이 저장소의 Administration 쓰기 권한이 없음\n  고치는 법: " + fixFineGrained, nil},
		{"fine-grained policies 404", "github_pat_", map[string]reply{runners: {status: 200, body: runnersOK}, policies: {status: 404, body: `{}`}},
			"확인 못 함 GitHub o/a: 확인하지 못함\n  원인: 예상하지 않은 응답 코드 404\n  고치는 법: " + fixCodeUnknown, nil},
		{"classic repo", "ghp_", map[string]reply{runners: scopes("repo")},
			"통과 GitHub o/a: classic 토큰, repo scope", []string{"GET " + runners + "?per_page=1"}},
		{"classic narrower scopes", "ghp_", map[string]reply{runners: scopes("public_repo, repo:status")},
			"실패 GitHub o/a: repo scope 없음\n  원인: 토큰의 scope에 repo가 없음\n  고치는 법: " + fixClassicScope, nil},
		{"classic spaces trimmed", "ghp_", map[string]reply{runners: scopes(" workflow,  repo ")},
			"통과 GitHub o/a: classic 토큰, repo scope", nil},
		{"classic no header", "ghp_", map[string]reply{runners: {status: 200, body: runnersOK}},
			"확인 못 함 GitHub o/a: 확인하지 못함\n  원인: 응답에 X-OAuth-Scopes 헤더가 없어 토큰의 scope를 알 수 없음\n  고치는 법: " + fixScopesUnknown, nil},
		{"418", "github_pat_", map[string]reply{runners: {status: 418, body: `{}`}},
			"확인 못 함 GitHub o/a: 확인하지 못함\n  원인: 예상하지 않은 응답 코드 418\n  고치는 법: " + fixCodeUnknown, []string{"GET " + runners + "?per_page=1"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := writeConfig(t, "token-file = \""+writeTokenFile(t, fixtureToken(tt.prefix))+"\"", target("o/a"))
			gh := newGitHubServer(t, tt.replies)
			deps, _ := testDeps(path, gh, goodDocker())
			out, _ := output(t, deps)
			if got := block(out, tt.want[:strings.IndexByte(tt.want, ':')+1]); got != tt.want {
				t.Errorf("got:\n%s\nwant:\n%s\nall:\n%s", got, tt.want, out)
			}
			if tt.requests != nil && strings.Join(gh.Requests(), "\n") != strings.Join(tt.requests, "\n") {
				t.Errorf("requests = %v, want %v", gh.Requests(), tt.requests)
			}
		})
	}
}

func TestDoctorTimeoutsAndRateLimit(t *testing.T) {
	t.Run("no answer", func(t *testing.T) {
		path := writeConfig(t, appLines(writeAppKey(t, 0o600)),
			target("o/a"), tokenTarget("o/b", writeTokenFile(t, fixtureToken("github_pat_"))))
		gh := newGitHubServer(t, map[string]reply{
			"/repos/o/a/installation":    {hang: true},
			"/repos/o/b/actions/runners": {hang: true},
		})
		d := goodDocker()
		d.hang = true
		deps, _ := testDeps(path, gh, d)
		deps.Timeout = 200 * time.Millisecond
		deps.Exposure = func(ctx context.Context) docker.Exposure {
			<-ctx.Done()
			return docker.ExposureUnknown
		}
		start := time.Now()
		out, _ := output(t, deps)
		if elapsed := time.Since(start); elapsed > 5*time.Second {
			t.Errorf("run took %s", elapsed)
		}
		for _, name := range []string{"GitHub o/a", "GitHub o/b"} {
			if got := block(out, "확인 못 함 "+name+":"); !strings.Contains(got, "원인: 200ms 안에 응답이 없음") {
				t.Errorf("%s:\n%s", name, out)
			}
		}
		if got := block(out, "확인 못 함 Docker:"); !strings.Contains(got, "원인: Docker가 200ms 안에 응답하지 않음") {
			t.Errorf("Docker:\n%s", out)
		}
		if got := block(out, "확인 못 함 Docker TCP 2375:"); !strings.Contains(got, "could not tell whether the Docker daemon is exposed") {
			t.Errorf("exposure:\n%s", out)
		}
		if got := block(out, "확인 못 함 서버 한도:"); !strings.Contains(got, "앞 검사가 통과하지 못함: Docker") {
			t.Errorf("server limit:\n%s", out)
		}
		if got := gh.Requests(); len(got) != 2 {
			t.Errorf("requests = %v, want one per check", got)
		}
		if got := d.Calls(); len(got) != 1 {
			t.Errorf("docker calls = %v, want one ping", got)
		}
	})

	reset := strconv.FormatInt(testNow.Add(time.Hour).Unix(), 10)
	tests := []struct {
		name    string
		prefix  string
		replies map[string]reply
		want    string
	}{
		{"429 Retry-After", "ghp_",
			map[string]reply{"/repos/o/a/actions/runners": {status: 429, header: map[string]string{"Retry-After": "120"}, body: `{}`}},
			"2026-11-26 00:02:00 UTC"},
		{"403 reset", "ghp_",
			map[string]reply{"/repos/o/a/actions/runners": {status: 403, header: map[string]string{"X-RateLimit-Remaining": "0", "X-RateLimit-Reset": reset}, body: `{"message":"API rate limit exceeded"}`}},
			"2026-11-26 01:00:00 UTC"},
		{"policies 429", "github_pat_",
			map[string]reply{
				"/repos/o/a/actions/runners":  {status: 200, body: runnersOK},
				"/repos/o/a/actions/policies": {status: 429, body: `{}`},
			},
			"2026-11-26 00:01:00 UTC"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := writeConfig(t, "token-file = \""+writeTokenFile(t, fixtureToken(tt.prefix))+"\"", target("o/a"))
			gh := newGitHubServer(t, tt.replies)
			deps, _ := testDeps(path, gh, goodDocker())
			out, _ := output(t, deps)
			want := "  원인: GitHub 속도 제한: " + tt.want + " 뒤에 다시 실행할 수 있음"
			if got := block(out, "확인 못 함 GitHub o/a:"); !strings.Contains(got, want) {
				t.Errorf("got:\n%s\nwant line %q", out, want)
			}
			if n := len(gh.Requests()); n != len(tt.replies) {
				t.Errorf("requests = %v, want each call once", gh.Requests())
			}
		})
	}

	t.Run("App 429", func(t *testing.T) {
		path := writeConfig(t, appLines(writeAppKey(t, 0o600)), target("o/a"))
		gh := newGitHubServer(t, map[string]reply{
			"/repos/o/a/installation": {status: 429, header: map[string]string{"Retry-After": "30"}, body: `{}`},
		})
		deps, _ := testDeps(path, gh, goodDocker())
		out, _ := output(t, deps)
		if got := block(out, "확인 못 함 GitHub o/a:"); !strings.Contains(got, "2026-11-26 00:00:30 UTC 뒤에") {
			t.Errorf("output:\n%s", out)
		}
	})
}
