package doctor

import (
	"strings"
	"testing"
	"time"

	"github.com/dewevrei/actions-runner-scaler/internal/github"
)

func TestExpirationSuffix(t *testing.T) {
	at := func(d time.Duration) string { return testNow.Add(d).Format("2006-01-02 15:04:05 MST") }
	warn := func(v string) string {
		return "토큰이 " + v + "에 만료됨. 새 토큰을 만들어 파일을 바꾸고 스케일러를 다시 시작한다"
	}
	day := 24 * time.Hour
	tests := []struct {
		name        string
		access      github.Access
		wantSuffix  string
		wantWarning string
	}{
		{"absent", github.Access{}, "", ""},
		{"MST layout, far", github.Access{ExpirationPresent: true, Expiration: "2027-01-01 00:00:00 UTC"}, ", 토큰 만료 2027-01-01 00:00:00 UTC", ""},
		{"offset layout, near", github.Access{ExpirationPresent: true, Expiration: "2026-11-27 09:00:00 +0900"}, ", 토큰 만료 2026-11-27 09:00:00 +0900", warn("2026-11-27 09:00:00 +0900")},
		{"8 days ahead", github.Access{ExpirationPresent: true, Expiration: at(8 * day)}, ", 토큰 만료 " + at(8*day), ""},
		{"7 days ahead", github.Access{ExpirationPresent: true, Expiration: at(7 * day)}, ", 토큰 만료 " + at(7*day), warn(at(7 * day))},
		{"1 hour ahead", github.Access{ExpirationPresent: true, Expiration: at(time.Hour)}, ", 토큰 만료 " + at(time.Hour), warn(at(time.Hour))},
		{"past", github.Access{ExpirationPresent: true, Expiration: at(-day)}, ", 토큰 만료 " + at(-day), warn(at(-day))},
		{"slash", github.Access{ExpirationPresent: true, Expiration: "2026/11/27 00:00:00 UTC"}, ", 토큰 만료 값을 읽을 수 없음", ""},
		{"41 characters", github.Access{ExpirationPresent: true, Expiration: strings.Repeat("1", 41)}, ", 토큰 만료 값을 읽을 수 없음", ""},
		{"40 characters, unparsable", github.Access{ExpirationPresent: true, Expiration: strings.Repeat("1", 40)}, ", 토큰 만료 " + strings.Repeat("1", 40), ""},
		{"empty", github.Access{ExpirationPresent: true}, ", 토큰 만료 값을 읽을 수 없음", ""},
		{"line break", github.Access{ExpirationPresent: true, Expiration: "2026-11-27\n00:00:00 UTC"}, ", 토큰 만료 값을 읽을 수 없음", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			suffix, warning := expiration(tt.access, testNow)
			if suffix != tt.wantSuffix || warning != tt.wantWarning {
				t.Errorf("expiration = %q, %q; want %q, %q", suffix, warning, tt.wantSuffix, tt.wantWarning)
			}
		})
	}
}

// The warning changes neither the outcome nor the counts (R317).
func TestExpirationKeepsOutcome(t *testing.T) {
	near := testNow.Add(time.Hour).Format("2006-01-02 15:04:05 MST")
	for _, scopes := range []string{"repo", "public_repo"} {
		path := writeConfig(t, "token-file = \""+writeTokenFile(t, fixtureToken("ghp_"))+"\"", target("o/a"))
		gh := newGitHubServer(t, map[string]reply{"/repos/o/a/actions/runners": {status: 200, body: runnersOK, header: map[string]string{
			"X-OAuth-Scopes": scopes, "GitHub-Authentication-Token-Expiration": near,
		}}})
		deps, _ := testDeps(path, gh, goodDocker())
		out, failed := output(t, deps)
		// 설정, the token file, GitHub, Docker, 2375, two images, the limit.
		want, wantCounts, wantFailed := "통과 GitHub o/a: classic 토큰, repo scope, 토큰 만료 "+near, "통과 8, 실패 0, 확인 못 함 0", 0
		if scopes != "repo" {
			want, wantCounts, wantFailed = "실패 GitHub o/a: repo scope 없음, 토큰 만료 "+near, "통과 7, 실패 1, 확인 못 함 0", 1
		}
		if line(out, want) == "" || !strings.Contains(out, "  경고: 토큰이 "+near+"에 만료됨") || failed != wantFailed {
			t.Errorf("scopes %q: failed = %d\n%s", scopes, failed, out)
		}
		if !strings.HasSuffix(out, "\n"+wantCounts+"\n") {
			t.Errorf("scopes %q: count line, want %q:\n%s", scopes, wantCounts, out)
		}
	}
}
