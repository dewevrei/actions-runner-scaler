package doctor

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// allPass returns a configuration with an App target, a classic token
// target and a fine-grained token target, and a server where each passes.
func allPass(t *testing.T) (string, *githubServer) {
	t.Helper()
	path := writeConfig(t, appLines(writeAppKey(t, 0o600)),
		target("o/a"),
		tokenTarget("o/b", writeTokenFile(t, fixtureToken("ghp_"))),
		tokenTarget("o/c", writeTokenFile(t, fixtureToken("github_pat_"))),
	)
	gh := newGitHubServer(t, map[string]reply{
		"/repos/o/a/installation": {status: 200, body: installationWrite},
		"/repos/o/b/actions/runners": {status: 200, body: `{"total_count":0,"runners":[]}`, header: map[string]string{
			"X-OAuth-Scopes":                         "repo, workflow",
			"GitHub-Authentication-Token-Expiration": "2026-12-31 00:00:00 UTC",
		}},
		"/repos/o/c/actions/runners": {status: 200, body: `{"total_count":0,"runners":[]}`, header: map[string]string{
			"GitHub-Authentication-Token-Expiration": "2026-12-01 10:00:00 +0900",
		}},
		"/repos/o/c/actions/policies": {status: 200, body: `{}`},
	})
	return path, gh
}

func TestDoctorOrderAndFormat(t *testing.T) {
	path, gh := allPass(t)
	deps, _ := testDeps(path, gh, goodDocker())
	got, failed := output(t, deps)
	want := `통과 설정: 문제 없음
통과 App 키 파일: github.app-private-key-file 읽음
통과 토큰 파일 targets[1].token-file: 읽음
통과 토큰 파일 targets[2].token-file: 읽음
통과 GitHub o/a: App 설치 42, administration: write
통과 GitHub o/b: classic 토큰, repo scope, 토큰 만료 2026-12-31 00:00:00 UTC
통과 GitHub o/c: fine-grained 토큰, Administration: write, 토큰 만료 2026-12-01 10:00:00 +0900
  경고: 토큰이 2026-12-01 10:00:00 +0900에 만료됨. 새 토큰을 만들어 파일을 바꾸고 스케일러를 다시 시작한다
통과 Docker: /var/run/docker.sock 응답함
통과 Docker TCP 2375: 127.0.0.1:2375에 Docker API가 없음
통과 이미지 runner.image: 로컬에 있음, arm64
통과 이미지 buildkit.image: 로컬에 있음, arm64
통과 서버 한도: runner 2개
통과 12, 실패 0, 확인 못 함 0
`
	if got != want {
		t.Errorf("output:\n%s\nwant:\n%s", got, want)
	}
	if failed != 0 {
		t.Errorf("failed = %d, want 0", failed)
	}
}

// line returns the line of out that starts with prefix, or "".
func line(out, prefix string) string {
	for l := range strings.SplitSeq(out, "\n") {
		if strings.HasPrefix(l, prefix) {
			return l
		}
	}
	return ""
}

// block returns the line of out that starts with prefix and the indented
// lines under it.
func block(out, prefix string) string {
	var b []string
	for l := range strings.SplitSeq(out, "\n") {
		switch {
		case strings.HasPrefix(l, prefix):
			b = append(b, l)
		case len(b) > 0 && strings.HasPrefix(l, "  "):
			b = append(b, l)
		case len(b) > 0:
			return strings.Join(b, "\n")
		}
	}
	return strings.Join(b, "\n")
}

// insertBefore rewrites the file at path with text inserted before the
// first occurrence of marker.
func insertBefore(t *testing.T, path, marker, text string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	s := strings.Replace(string(data), marker, text+marker, 1)
	if err := os.WriteFile(path, []byte(s), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestDoctorDependentUnknown(t *testing.T) {
	t.Run("docker ping fails", func(t *testing.T) {
		path, gh := allPass(t)
		d := goodDocker()
		d.pingErr = errors.New("connect: no such file or directory")
		deps, _ := testDeps(path, gh, d)
		out, _ := output(t, deps)
		for _, name := range []string{"이미지 runner.image", "이미지 buildkit.image", "서버 한도"} {
			if got := block(out, "확인 못 함 "+name+":"); !strings.Contains(got, "원인: 앞 검사가 통과하지 못함: Docker") {
				t.Errorf("%s:\n%s", name, got)
			}
		}
		if line(out, "실패 Docker:") == "" || line(out, "통과 Docker TCP 2375:") == "" {
			t.Errorf("Docker lines:\n%s", out)
		}
		if got := d.Calls(); len(got) != 1 || got[0] != "ping" {
			t.Errorf("docker calls = %v, want only ping", got)
		}
		if n := len(gh.Requests()); n != 4 {
			t.Errorf("GitHub requests = %d, want 4: every target is still checked", n)
		}
	})

	t.Run("App key file fails", func(t *testing.T) {
		path := writeConfig(t, appLines(writeAppKey(t, 0o644)),
			target("o/a"), tokenTarget("o/b", writeTokenFile(t, fixtureToken("ghp_"))))
		gh := newGitHubServer(t, map[string]reply{
			"/repos/o/b/actions/runners": {status: 200, header: map[string]string{"X-OAuth-Scopes": "repo"}},
		})
		deps, secrets := testDeps(path, gh, goodDocker())
		fake := &fakeGitHub{inner: gh.clients(secrets)}
		deps.GitHub = fake
		out, _ := output(t, deps)
		if got := block(out, "확인 못 함 GitHub o/a:"); !strings.Contains(got, "앞 검사가 통과하지 못함: App 키 파일") {
			t.Errorf("App target:\n%s", out)
		}
		if line(out, "실패 App 키 파일:") == "" || line(out, "통과 GitHub o/b:") == "" {
			t.Errorf("output:\n%s", out)
		}
		if got := strings.Join(fake.made, ","); got != "token" {
			t.Errorf("clients made = %q, want only the token's", got)
		}
	})

	t.Run("token file fails", func(t *testing.T) {
		bad := writeSecret(t, "token", []byte(fixtureToken("ghp_")+"\n"), 0o644)
		path := writeConfig(t, appLines(writeAppKey(t, 0o600)), target("o/a"), tokenTarget("o/b", bad))
		gh := newGitHubServer(t, map[string]reply{"/repos/o/a/installation": {status: 200, body: installationWrite}})
		deps, _ := testDeps(path, gh, goodDocker())
		out, _ := output(t, deps)
		if got := block(out, "확인 못 함 GitHub o/b:"); !strings.Contains(got, "앞 검사가 통과하지 못함: 토큰 파일 targets[1].token-file") {
			t.Errorf("token target:\n%s", out)
		}
		if line(out, "실패 토큰 파일 targets[1].token-file:") == "" || line(out, "통과 GitHub o/a:") == "" {
			t.Errorf("output:\n%s", out)
		}
	})

	t.Run("url refused", func(t *testing.T) {
		url := "https://" + fixtureToken("ghp_") + "@github.com/o/a"
		path := writeConfig(t, appLines(writeAppKey(t, 0o600)), "url = \""+url+"\"")
		gh := newGitHubServer(t, nil)
		deps, _ := testDeps(path, gh, goodDocker())
		out, _ := output(t, deps)
		if got := block(out, "확인 못 함 GitHub targets[0]:"); !strings.Contains(got, "앞 검사가 통과하지 못함: 설정") {
			t.Errorf("output:\n%s", out)
		}
		if strings.Contains(out, url) || len(gh.Requests()) != 0 {
			t.Errorf("url shown or GitHub called %v:\n%s", gh.Requests(), out)
		}
	})

	t.Run("config unreadable", func(t *testing.T) {
		gh := newGitHubServer(t, nil)
		deps, _ := testDeps(filepath.Join(t.TempDir(), "missing.toml"), gh, goodDocker())
		out, failed := output(t, deps)
		for _, prefix := range []string{
			"실패 설정:", "확인 못 함 App 키 파일:", "확인 못 함 토큰 파일:", "확인 못 함 GitHub:",
			"통과 Docker:", "통과 Docker TCP 2375:",
		} {
			if line(out, prefix) == "" {
				t.Errorf("no line %q:\n%s", prefix, out)
			}
		}
		for _, name := range []string{"이미지 runner.image", "이미지 buildkit.image", "서버 한도"} {
			if got := block(out, "확인 못 함 "+name+":"); !strings.Contains(got, "앞 검사가 통과하지 못함: 설정") {
				t.Errorf("%s:\n%s", name, got)
			}
		}
		if failed != 1 {
			t.Errorf("failed = %d, want 1", failed)
		}
	})

	t.Run("resource key invalid", func(t *testing.T) {
		path := writeConfig(t, appLines(writeAppKey(t, 0o600)), target("o/a"))
		insertBefore(t, path, "image = \"sha256:", "cpus = 0\n")
		gh := newGitHubServer(t, map[string]reply{"/repos/o/a/installation": {status: 200, body: installationWrite}})
		deps, _ := testDeps(path, gh, goodDocker())
		out, _ := output(t, deps)
		if got := block(out, "확인 못 함 서버 한도:"); !strings.Contains(got, "앞 검사가 통과하지 못함: 설정") {
			t.Errorf("output:\n%s", out)
		}
		if line(out, "통과 이미지 runner.image:") == "" || line(out, "통과 GitHub o/a:") == "" {
			t.Errorf("other checks did not run:\n%s", out)
		}
	})
}

func TestDoctorSummaryAndExit(t *testing.T) {
	path := writeConfig(t, appLines(writeAppKey(t, 0o600)), target("o/a"), target("o/b"))
	gh := newGitHubServer(t, map[string]reply{
		"/repos/o/a/installation": {status: 404, body: `{"message":"Not Found"}`},
		"/repos/o/b/installation": {status: 503, body: `{"message":"unavailable"}`},
	})
	d := goodDocker()
	unlabelled := d.images[runnerImageID]
	unlabelled.Labels = nil
	d.images[runnerImageID] = unlabelled
	deps, _ := testDeps(path, gh, d)
	out, failed := output(t, deps)

	counts := map[string]int{}
	lines := strings.Split(strings.TrimSuffix(out, "\n"), "\n")
	for _, l := range lines[:len(lines)-1] {
		for _, word := range []string{"통과 ", "실패 ", "확인 못 함 "} {
			if strings.HasPrefix(l, word) {
				counts[word]++
			}
		}
	}
	want := fmt.Sprintf("통과 %d, 실패 %d, 확인 못 함 %d", counts["통과 "], counts["실패 "], counts["확인 못 함 "])
	if last := lines[len(lines)-1]; last != want {
		t.Errorf("last line = %q, want %q", last, want)
	}
	if failed != 1 || counts["실패 "] != 1 || counts["확인 못 함 "] != 2 {
		t.Errorf("failed = %d, counts = %v:\n%s", failed, counts, out)
	}

	// 확인 못 함 alone is not a failure (R298).
	var buf bytes.Buffer
	rep := Report{
		Results: []Result{{Outcome: Unknown, Name: "x", Summary: "y", Causes: []string{"c"}, Fix: "f"}},
		redact:  func(s string) string { return s },
	}
	if n, err := Print(&buf, rep); n != 0 || err != nil {
		t.Errorf("Print of 확인 못 함 only = %d, %v; want 0, nil", n, err)
	}
	if got := buf.String(); got != "확인 못 함 x: y\n  원인: c\n  고치는 법: f\n통과 0, 실패 0, 확인 못 함 1\n" {
		t.Errorf("Print = %q", got)
	}
}

func TestDoctorConfigLines(t *testing.T) {
	keyPath := writeAppKey(t, 0o644)
	tokenPath := writeSecret(t, "token", []byte(fixtureToken("ghp_")+"\n"), 0o644)
	path := writeConfig(t, appLines(keyPath),
		tokenTarget("o/a", tokenPath),
		target("o/b", "token-file = \""+fixtureToken("github_pat_")+"\""),
	)
	insertBefore(t, path, "[runner]", "[scaling]\nlabel = \"Bad Label\"\n\n")
	gh := newGitHubServer(t, nil)
	deps, _ := testDeps(path, gh, goodDocker())
	out, _ := output(t, deps)

	cfg := block(out, "실패 설정:")
	if !strings.Contains(cfg, "  원인: scaling.label: must be") || strings.Contains(cfg, "token-file") || strings.Contains(cfg, "app-private-key-file") {
		t.Errorf("설정 line:\n%s", cfg)
	}
	for _, prefix := range []string{
		"실패 App 키 파일: github.app-private-key-file 읽지 못함",
		"실패 토큰 파일 targets[0].token-file: 읽지 못함",
		"실패 토큰 파일 targets[1].token-file: 읽지 못함",
	} {
		if line(out, prefix) == "" {
			t.Errorf("no line %q:\n%s", prefix, out)
		}
	}
	if got := block(out, "실패 토큰 파일 targets[1].token-file:"); !strings.Contains(got, "value looks like a token") {
		t.Errorf("R276 line:\n%s", got)
	}
	for _, p := range []string{keyPath, tokenPath, filepath.Dir(keyPath), filepath.Dir(tokenPath)} {
		if strings.Contains(out, p) {
			t.Errorf("output shows path %q:\n%s", p, out)
		}
	}
	if len(gh.Requests()) != 0 {
		t.Errorf("GitHub called: %v", gh.Requests())
	}
}

func TestDoctorNoSecretOutput(t *testing.T) {
	classic := fixtureToken("ghp_")
	fine := fixtureToken("github_pat_")
	other := fixtureToken("gho_")
	path := writeConfig(t, "token-file = \""+writeTokenFile(t, classic)+"\"",
		target("o/a"),
		tokenTarget("o/b", writeTokenFile(t, fine)),
		"url = \"https://x:"+other+"@github.com/o/c\"",
		target("o/d", "token-file = \""+other+"\""),
	)
	// The server quotes the Authorization header in its error messages.
	echo := reply{status: 403, echoAuth: true, header: map[string]string{"X-Accepted-GitHub-Permissions": "administration=write"}}
	gh := newGitHubServer(t, map[string]reply{
		"/repos/o/a/actions/runners": echo,
		"/repos/o/b/actions/runners": echo,
	})
	deps, _ := testDeps(path, gh, goodDocker())
	out, _ := output(t, deps)
	for _, token := range []string{classic, fine, other} {
		if strings.Contains(out, token) {
			t.Errorf("output holds a fixture token:\n%s", out)
		}
	}
	if n := len(gh.Requests()); n != 2 {
		t.Errorf("GitHub requests = %v, want the two token targets", gh.Requests())
	}
	if line(out, "실패 GitHub o/a:") == "" || line(out, "실패 GitHub o/b:") == "" {
		t.Errorf("output:\n%s", out)
	}
}

// Run refuses Deps without the redaction of the token set, or without a
// client, before any check; Print refuses a Report Run did not make.
func TestDoctorRequiredDeps(t *testing.T) {
	path, gh := allPass(t)
	tests := []struct {
		name  string
		unset func(*Deps)
	}{
		{"Redact", func(d *Deps) { d.Redact = nil }},
		{"GitHub", func(d *Deps) { d.GitHub = nil }},
		{"Docker", func(d *Deps) { d.Docker = nil }},
		{"Exposure", func(d *Deps) { d.Exposure = nil }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := goodDocker()
			deps, _ := testDeps(path, gh, d)
			tt.unset(&deps)
			if _, err := Run(context.Background(), deps); err == nil || !strings.Contains(err.Error(), tt.name) {
				t.Errorf("Run error = %v, want one naming %s", err, tt.name)
			}
			if len(d.Calls()) != 0 || len(gh.Requests()) != 0 {
				t.Errorf("checks ran: docker %v, GitHub %v", d.Calls(), gh.Requests())
			}
		})
	}
	var buf bytes.Buffer
	if _, err := Print(&buf, Report{Results: []Result{{Name: "x"}}}); err == nil || buf.Len() != 0 {
		t.Errorf("Print of a hand-made Report = %v, wrote %q", err, buf.String())
	}
}
