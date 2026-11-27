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
		return "the token expires at " + v + " (or has already expired); make a new token, replace the file and restart the scaler"
	}
	day := 24 * time.Hour
	tests := []struct {
		name        string
		access      github.Access
		wantSuffix  string
		wantWarning string
	}{
		{"absent", github.Access{}, "", ""},
		{"MST layout, far", github.Access{ExpirationPresent: true, Expiration: "2027-01-01 00:00:00 UTC"}, ", token expiry 2027-01-01 00:00:00 UTC", ""},
		{"offset layout, near", github.Access{ExpirationPresent: true, Expiration: "2026-11-27 09:00:00 +0900"}, ", token expiry 2026-11-27 09:00:00 +0900", warn("2026-11-27 09:00:00 +0900")},
		{"8 days ahead", github.Access{ExpirationPresent: true, Expiration: at(8 * day)}, ", token expiry " + at(8*day), ""},
		{"7 days ahead", github.Access{ExpirationPresent: true, Expiration: at(7 * day)}, ", token expiry " + at(7*day), warn(at(7 * day))},
		{"1 hour ahead", github.Access{ExpirationPresent: true, Expiration: at(time.Hour)}, ", token expiry " + at(time.Hour), warn(at(time.Hour))},
		{"past", github.Access{ExpirationPresent: true, Expiration: at(-day)}, ", token expiry " + at(-day), warn(at(-day))},
		{"slash", github.Access{ExpirationPresent: true, Expiration: "2026/11/27 00:00:00 UTC"}, ", token expiry unreadable", ""},
		{"41 characters", github.Access{ExpirationPresent: true, Expiration: strings.Repeat("1", 41)}, ", token expiry unreadable", ""},
		{"40 characters, unparsable", github.Access{ExpirationPresent: true, Expiration: strings.Repeat("1", 40)}, ", token expiry " + strings.Repeat("1", 40), ""},
		{"empty", github.Access{ExpirationPresent: true}, ", token expiry unreadable", ""},
		{"line break", github.Access{ExpirationPresent: true, Expiration: "2026-11-27\n00:00:00 UTC"}, ", token expiry unreadable", ""},
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
		// Config, the token file, GitHub, Docker, 2375, two images, the limit.
		want, wantCounts, wantFailed := "PASS GitHub o/a: classic token, repo scope, token expiry "+near, "PASS 8, FAIL 0, UNKNOWN 0", 0
		if scopes != "repo" {
			want, wantCounts, wantFailed = "FAIL GitHub o/a: no repo scope, token expiry "+near, "PASS 7, FAIL 1, UNKNOWN 0", 1
		}
		if line(out, want) == "" || !strings.Contains(out, "  warning: the token expires at "+near) || failed != wantFailed {
			t.Errorf("scopes %q: failed = %d\n%s", scopes, failed, out)
		}
		if !strings.HasSuffix(out, "\n"+wantCounts+"\n") {
			t.Errorf("scopes %q: count line, want %q:\n%s", scopes, wantCounts, out)
		}
	}
}
