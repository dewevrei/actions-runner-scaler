package doctor

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
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
		{"write", reply{status: 200, body: installationWrite}, "PASS GitHub o/a: App installation 42, administration: write"},
		{"read", reply{status: 200, body: `{"id":42,"permissions":{"administration":"read"}}`},
			"FAIL GitHub o/a: App permission too low\n  cause: GitHub App installation on repository o/a lacks administration: write\n  fix: " + fixAppPermission},
		{"404", reply{status: 404, body: `{"message":"Not Found"}`},
			"FAIL GitHub o/a: App not installed\n  cause: GitHub App is not installed on repository o/a\n  fix: " + fixAppInstall},
		{"401", reply{status: 401, body: `{"message":"Bad credentials"}`},
			"FAIL GitHub o/a: App authentication refused\n  cause: GitHub did not accept the App's JWT (401)\n  fix: " + fixAppJWT},
		{"500", reply{status: 500, body: `{"message":"oops"}`},
			"UNKNOWN GitHub o/a: could not check\n  cause: GitHub answered 500\n  fix: " + fixAgain},
		{"418", reply{status: 418, body: `{"message":"teapot"}`},
			"UNKNOWN GitHub o/a: could not check\n  cause: unexpected response code 418\n  fix: " + fixCodeUnknown},
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
			"FAIL GitHub o/a: token refused (401)\n  cause: personal access token for repository o/a was refused (401): it is invalid, expired or revoked\n  fix: " + fixTokenRefused, nil},
		{"403", "ghp_", map[string]reply{runners: {status: 403, body: `{"message":"Must have admin rights"}`}},
			"FAIL GitHub o/a: token permission too low (403)\n  cause: " + refused403 + "\n  fix: " + fixTokenAccess, nil},
		{"404", "github_pat_", map[string]reply{runners: {status: 404, body: `{"message":"Not Found"}`}},
			"FAIL GitHub o/a: token permission too low (404)\n  cause: " + strings.ReplaceAll(refused403, "403", "404") + "\n  fix: " + fixTokenAccess, nil},
		{"fine-grained policies 200", "github_pat_", map[string]reply{runners: {status: 200, body: runnersOK}, policies: {status: 200, body: `{}`}},
			"PASS GitHub o/a: fine-grained token, Administration: write", []string{"GET " + runners + "?per_page=1", "GET " + policies + "?per_page=1"}},
		{"fine-grained policies 403", "github_pat_", map[string]reply{runners: {status: 200, body: runnersOK}, policies: {status: 403, body: `{}`}},
			"FAIL GitHub o/a: token permission too low (403)\n  cause: the token has no Administration write permission on this repository\n  fix: " + fixFineGrained, nil},
		{"fine-grained policies 404", "github_pat_", map[string]reply{runners: {status: 200, body: runnersOK}, policies: {status: 404, body: `{}`}},
			"UNKNOWN GitHub o/a: could not check\n  cause: unexpected response code 404\n  fix: " + fixCodeUnknown, nil},
		{"classic repo", "ghp_", map[string]reply{runners: scopes("repo")},
			"PASS GitHub o/a: classic token, repo scope", []string{"GET " + runners + "?per_page=1"}},
		{"classic narrower scopes", "ghp_", map[string]reply{runners: scopes("public_repo, repo:status")},
			"FAIL GitHub o/a: no repo scope\n  cause: the token's scopes do not include repo\n  fix: " + fixClassicScope, nil},
		{"classic spaces trimmed", "ghp_", map[string]reply{runners: scopes(" workflow,  repo ")},
			"PASS GitHub o/a: classic token, repo scope", nil},
		{"classic no header", "ghp_", map[string]reply{runners: {status: 200, body: runnersOK}},
			"UNKNOWN GitHub o/a: could not check\n  cause: the response has no X-OAuth-Scopes header, so the token's scopes are unknown\n  fix: " + fixScopesUnknown, nil},
		{"418", "github_pat_", map[string]reply{runners: {status: 418, body: `{}`}},
			"UNKNOWN GitHub o/a: could not check\n  cause: unexpected response code 418\n  fix: " + fixCodeUnknown, []string{"GET " + runners + "?per_page=1"}},
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
			if got := block(out, "UNKNOWN "+name+":"); !strings.Contains(got, "cause: no answer within 200ms") {
				t.Errorf("%s:\n%s", name, out)
			}
		}
		if got := block(out, "UNKNOWN Docker:"); !strings.Contains(got, "cause: Docker did not answer within 200ms") {
			t.Errorf("Docker:\n%s", out)
		}
		if got := block(out, "UNKNOWN Docker TCP 2375:"); !strings.Contains(got, "could not tell whether the Docker daemon is exposed") {
			t.Errorf("exposure:\n%s", out)
		}
		if got := block(out, "UNKNOWN Server limit:"); !strings.Contains(got, "an earlier check did not pass: Docker") {
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
			want := "  cause: GitHub rate limit: run doctor again after " + tt.want
			if got := block(out, "UNKNOWN GitHub o/a:"); !strings.Contains(got, want) {
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
		if got := block(out, "UNKNOWN GitHub o/a:"); !strings.Contains(got, "run doctor again after 2026-11-26 00:00:30 UTC") {
			t.Errorf("output:\n%s", out)
		}
	})
}

// R317 also holds for a refused token: the FAIL line shows the expiry and
// the warning, and the counts stay those of the refusal.
func TestDoctorTokenRefusalExpiration(t *testing.T) {
	near := testNow.Add(time.Hour).Format("2006-01-02 15:04:05 MST")
	far := testNow.Add(30 * 24 * time.Hour).Format("2006-01-02 15:04:05 MST")
	refusedNear := "personal access token for repository o/a cannot manage its runners (403): a classic token needs the repo scope and admin access, a fine-grained token needs Administration: Read and write on this repository"
	tests := []struct {
		name       string
		reply      reply
		want       string
		wantFailed int
	}{
		{"403 near", reply{status: 403, body: `{}`, header: map[string]string{"GitHub-Authentication-Token-Expiration": near}},
			"FAIL GitHub o/a: token permission too low (403), token expiry " + near + "\n  cause: " + refusedNear + "\n  fix: " + fixTokenAccess +
				"\n  warning: the token expires at " + near + ", or already has; make a new token, replace the file and restart the scaler", 1},
		{"401 far", reply{status: 401, body: `{}`, header: map[string]string{"GitHub-Authentication-Token-Expiration": far}},
			"FAIL GitHub o/a: token refused (401), token expiry " + far + "\n  cause: personal access token for repository o/a was refused (401): it is invalid, expired or revoked\n  fix: " + fixTokenRefused, 1},
		{"404 unreadable", reply{status: 404, body: `{}`, header: map[string]string{"GitHub-Authentication-Token-Expiration": "2026/11/26"}},
			"FAIL GitHub o/a: token permission too low (404), token expiry unreadable", 1},
		{"429 not a refusal", reply{status: 429, body: `{}`, header: map[string]string{"GitHub-Authentication-Token-Expiration": near}},
			"UNKNOWN GitHub o/a: could not check\n  cause: GitHub rate limit: run doctor again after 2026-11-26 00:01:00 UTC\n  fix: " + fixAgain, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := writeConfig(t, "token-file = \""+writeTokenFile(t, fixtureToken("ghp_"))+"\"", target("o/a"))
			gh := newGitHubServer(t, map[string]reply{"/repos/o/a/actions/runners": tt.reply})
			deps, _ := testDeps(path, gh, goodDocker())
			out, failed := output(t, deps)
			if got := block(out, tt.want[:strings.IndexByte(tt.want, ':')+1]); !strings.HasPrefix(got, tt.want) || (strings.Contains(tt.want, "warning") != strings.Contains(got, "warning")) {
				t.Errorf("got:\n%s\nwant:\n%s", got, tt.want)
			}
			if failed != tt.wantFailed {
				t.Errorf("failed = %d, want %d", failed, tt.wantFailed)
			}
		})
	}
}

// ownAppTarget is a [[targets]] entry for repo with its own App.
func ownAppTarget(repo, clientID, keyPath string) string {
	return target(repo, fmt.Sprintf("app-client-id = %q\napp-private-key-file = %q", clientID, keyPath))
}

// issuers returns the iss claim of the App JWT of each installation lookup
// gh received, by request path.
func (g *githubServer) issuers(t *testing.T) map[string]string {
	t.Helper()
	g.mu.Lock()
	defer g.mu.Unlock()
	out := make(map[string]string)
	for i, req := range g.requests {
		path, ok := strings.CutPrefix(req, "GET ")
		if !ok || !strings.HasSuffix(path, "/installation") {
			continue
		}
		parts := strings.Split(strings.TrimPrefix(g.auth[i], "Bearer "), ".")
		if len(parts) != 3 {
			t.Errorf("%s was not made with an App JWT", path)
			continue
		}
		payload, err := base64.RawURLEncoding.DecodeString(parts[1])
		var claims struct {
			Issuer string `json:"iss"`
		}
		if err != nil || json.Unmarshal(payload, &claims) != nil {
			t.Errorf("%s: unreadable JWT claims", path)
			continue
		}
		out[path] = claims.Issuer
	}
	return out
}

// TestDoctorTargetApp proves that a target with its own App gets the App
// checks with that App: its key file has its own line, the installation is
// looked up with its own JWT, and administration: write is checked, beside
// a default token and with no [github] credential at all.
func TestDoctorTargetApp(t *testing.T) {
	ownKey := writeAppKey(t, 0o600)
	tokenPath := writeTokenFile(t, fixtureToken("ghp_"))
	tests := []struct {
		name    string
		config  string
		replies map[string]reply
		want    []string
		issuers map[string]string
	}{
		{name: "beside a default token",
			config: writeConfig(t, fmt.Sprintf("token-file = %q", tokenPath), target("o/a"), ownAppTarget("o/b", "Iv1.own", ownKey)),
			replies: map[string]reply{
				"/repos/o/a/actions/runners": {status: 200, body: runnersOK, header: map[string]string{"X-OAuth-Scopes": "repo"}},
				"/repos/o/b/installation":    {status: 200, body: installationWrite},
			},
			want: []string{
				"PASS Token file github.token-file: read",
				"PASS App key file targets[1].app-private-key-file: read",
				"PASS GitHub o/a: classic token, repo scope",
				"PASS GitHub o/b: App installation 42, administration: write",
			},
			issuers: map[string]string{"/repos/o/b/installation": "Iv1.own"}},
		{name: "without [github]",
			config: writeConfig(t, "", ownAppTarget("o/a", "Iv1.one", ownKey), ownAppTarget("o/b", "Iv1.two", writeAppKey(t, 0o600))),
			replies: map[string]reply{
				"/repos/o/a/installation": {status: 200, body: installationWrite},
				"/repos/o/b/installation": {status: 200, body: `{"id":43,"permissions":{"administration":"read"}}`},
			},
			want: []string{
				"PASS Config: no problems",
				"PASS App key file targets[0].app-private-key-file: read",
				"PASS GitHub o/a: App installation 42, administration: write",
				"FAIL GitHub o/b: App permission too low\n  cause: GitHub App installation on repository o/b lacks administration: write",
			},
			issuers: map[string]string{"/repos/o/a/installation": "Iv1.one", "/repos/o/b/installation": "Iv1.two"}},
		{name: "beside the default App",
			config: writeConfig(t, appLines(writeAppKey(t, 0o600)), target("o/a"), ownAppTarget("o/b", "Iv1.own", ownKey)),
			replies: map[string]reply{
				"/repos/o/a/installation": {status: 200, body: installationWrite},
				"/repos/o/b/installation": {status: 200, body: installationWrite},
			},
			want: []string{
				"PASS App key file: github.app-private-key-file read",
				"PASS App key file targets[1].app-private-key-file: read",
				"PASS GitHub o/a: App installation 42",
				"PASS GitHub o/b: App installation 42",
			},
			issuers: map[string]string{"/repos/o/a/installation": "Iv1.test", "/repos/o/b/installation": "Iv1.own"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gh := newGitHubServer(t, tt.replies)
			deps, _ := testDeps(tt.config, gh, goodDocker())
			out, _ := output(t, deps)
			for _, want := range tt.want {
				if got := block(out, want[:strings.IndexByte(want, ':')+1]); !strings.HasPrefix(got, want) {
					t.Errorf("got:\n%s\nwant:\n%s\nall:\n%s", got, want, out)
				}
			}
			if got := gh.issuers(t); !maps.Equal(got, tt.issuers) {
				t.Errorf("installation lookups by issuer = %v, want %v", got, tt.issuers)
			}
			if n := len(gh.Requests()); n != len(tt.replies) {
				t.Errorf("requests = %v, want each check once", gh.Requests())
			}
		})
	}
}

// TestDoctorTargetCredentialBlocked proves that a target whose own
// credential has a problem is blocked by it and never checked with the
// default credential or anything else.
func TestDoctorTargetCredentialBlocked(t *testing.T) {
	ownKey := writeAppKey(t, 0o600)
	tokenPath := writeTokenFile(t, fixtureToken("ghp_"))
	tests := []struct {
		name   string
		target string
		want   string
	}{
		{"both kinds", target("o/b", fmt.Sprintf("app-client-id = \"Iv1.own\"\napp-private-key-file = %q\ntoken-file = %q", ownKey, tokenPath)),
			"UNKNOWN GitHub o/b: not checked\n  cause: an earlier check did not pass: Config"},
		{"client ID only", target("o/b", `app-client-id = "Iv1.own"`),
			"UNKNOWN GitHub o/b: not checked\n  cause: an earlier check did not pass: App key file targets[1].app-private-key-file"},
		{"key file only", target("o/b", fmt.Sprintf("app-private-key-file = %q", ownKey)),
			"UNKNOWN GitHub o/b: not checked\n  cause: an earlier check did not pass: Config"},
		{"wide key file", ownAppTarget("o/b", "Iv1.own", writeAppKey(t, 0o644)),
			"UNKNOWN GitHub o/b: not checked\n  cause: an earlier check did not pass: App key file targets[1].app-private-key-file"},
		{"client ID of the wrong type", target("o/b", fmt.Sprintf("app-client-id = 5\napp-private-key-file = %q", ownKey)),
			"UNKNOWN GitHub o/b: not checked\n  cause: an earlier check did not pass: Config"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := writeConfig(t, appLines(writeAppKey(t, 0o600)), target("o/a"), tt.target)
			gh := newGitHubServer(t, map[string]reply{"/repos/o/a/installation": {status: 200, body: installationWrite}})
			deps, _ := testDeps(path, gh, goodDocker())
			out, _ := output(t, deps)
			if got := block(out, "UNKNOWN GitHub o/b:"); !strings.HasPrefix(got, tt.want) {
				t.Errorf("got:\n%s\nwant:\n%s\nall:\n%s", got, tt.want, out)
			}
			if got := gh.Requests(); !slices.Equal(got, []string{"GET /repos/o/a/installation"}) {
				t.Errorf("requests = %v, want only o/a's lookup", got)
			}
			if strings.Contains(out, ownKey) {
				t.Error("the output holds the key file path")
			}
		})
	}
}
