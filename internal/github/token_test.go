package github

import (
	"context"
	"crypto/rand"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dewevrei/actions-runner-scaler/internal/logging"
	"github.com/dewevrei/actions-runner-scaler/internal/retry"
)

// fixtureToken returns prefix followed by 26 random characters, built at
// run time so that no token-shaped literal is committed (0003 PLAN Risks).
func fixtureToken(t *testing.T, prefix string) string {
	t.Helper()
	return prefix + rand.Text()
}

// assertNoToken fails when text holds token; the failure shows text with
// the token replaced, so the test output never holds it either.
func assertNoToken(t *testing.T, what, text, token string) {
	t.Helper()
	if strings.Contains(text, token) {
		t.Errorf("%s contains the token: %s", what, strings.ReplaceAll(text, token, "<fixture token>"))
	}
}

// seenRequest is what tokenServer records of one request.
type seenRequest struct {
	method, path, query string
	// bearer reports whether Authorization was "Bearer <the token>".
	bearer bool
}

// tokenServer is a GitHub REST server for clients made by NewTokenREST.
// It answers every request with reply and records it.
type tokenServer struct {
	srv   *httptest.Server
	token string

	mu       sync.Mutex
	requests []seenRequest
	reply    http.HandlerFunc
}

func newTokenServer(t *testing.T, token string) *tokenServer {
	t.Helper()
	s := &tokenServer{token: token}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.requests = append(s.requests, seenRequest{
			method: r.Method, path: r.URL.Path, query: r.URL.RawQuery,
			bearer: r.Header.Get("Authorization") == "Bearer "+s.token,
		})
		reply := s.reply
		s.mu.Unlock()
		if reply == nil {
			http.NotFound(w, r)
			return
		}
		reply(w, r)
	}))
	t.Cleanup(s.srv.Close)
	return s
}

func (s *tokenServer) set(reply http.HandlerFunc) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reply = reply
}

func (s *tokenServer) seen() []seenRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]seenRequest(nil), s.requests...)
}

// tokenEnv is a REST client made by NewTokenREST against a tokenServer.
type tokenEnv struct {
	srv     *tokenServer
	rest    *REST
	token   string
	secrets *logging.Secrets
}

func newTokenEnv(t *testing.T, prefix string) *tokenEnv {
	t.Helper()
	token := fixtureToken(t, prefix)
	srv := newTokenServer(t, token)
	secrets := logging.NewSecrets()
	rest, err := NewTokenREST(TokenRESTConfig{
		BaseURL:    srv.srv.URL,
		Token:      token,
		HTTPClient: srv.srv.Client(),
		Clock:      newFakeClock(),
		Secrets:    secrets,
	})
	if err != nil {
		t.Fatal(err)
	}
	return &tokenEnv{srv: srv, rest: rest, token: token, secrets: secrets}
}

// TestTokenRESTAuthorization proves 0003 SPEC R278: every REST call of a
// token client carries "Bearer <token>", none asks for an installation
// token, and a 401 makes no token request either.
func TestTokenRESTAuthorization(t *testing.T) {
	e := newTokenEnv(t, "ghp_")
	e.srv.set(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/actions/runners") {
			_, _ = w.Write([]byte(runnerJSON))
			return
		}
		_, _ = w.Write([]byte(`{}`))
	})
	ctx := context.Background()
	if _, err := e.rest.ListRunners(ctx, testTarget); err != nil {
		t.Fatalf("ListRunners: %v", err)
	}
	if _, found, err := e.rest.FindRunner(ctx, testTarget, "pool-a"); err != nil || !found {
		t.Fatalf("FindRunner = %v, %v", found, err)
	}
	if _, err := e.rest.CheckRepositoryAccess(ctx, testTarget); err != nil {
		t.Fatalf("CheckRepositoryAccess: %v", err)
	}
	if code, err := e.rest.ActionsPoliciesStatus(ctx, testTarget); err != nil || code != http.StatusOK {
		t.Fatalf("ActionsPoliciesStatus = %d, %v", code, err)
	}

	e.srv.set(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"message": "Bad credentials"})
	})
	for range 2 {
		err := findRunner(ctx, e.rest)
		if c := retry.Classify(ctx, retry.OpFindRepoRunner, err); c != retry.Permanent {
			t.Errorf("Classify(401) = %v, want permanent", c)
		}
	}

	seen := e.srv.seen()
	if len(seen) != 6 {
		t.Errorf("requests = %d, want 6", len(seen))
	}
	for _, r := range seen {
		if !r.bearer {
			t.Errorf("%s %s without the token as bearer", r.method, r.path)
		}
		if r.method != http.MethodGet || !strings.HasPrefix(r.path, "/repos/") {
			t.Errorf("unexpected request %s %s", r.method, r.path)
		}
	}
}

// TestCheckRepositoryAccess proves 0003 SPEC R279 and the header reading
// R301 and R317 rely on.
func TestCheckRepositoryAccess(t *testing.T) {
	const (
		refused = "personal access token for repository acme/widget was refused (401): it is invalid, expired or revoked"
		cannot  = "personal access token for repository acme/widget cannot manage its runners (%d): a classic token needs the repo scope and admin access, a fine-grained token needs Administration: Read and write on this repository"
		accepts = "; GitHub says the endpoint accepts "
		sso     = "; an organization requires SAML SSO authorization of this token"
		expires = "2026-12-24 12:00:00 UTC"
	)
	cannotManage := func(code int) string { return strings.Replace(cannot, "%d", strconv.Itoa(code), 1) }
	ssoValue := "required; url=https://github.com/orgs/acme/sso?authorization_request=abc"
	longest := strings.Repeat("a", 200)
	tests := []struct {
		name    string
		status  int
		headers map[string]string
		access  Access // for 200
		message string // the StatusError's Message when setup is set
		setup   bool   // permanent through ErrTargetSetup
		class   retry.Class
	}{
		{
			name: "200 classic", status: 200,
			headers: map[string]string{"X-OAuth-Scopes": "repo, workflow ,read:org", "GitHub-Authentication-Token-Expiration": expires},
			access:  Access{Status: 200, Scopes: []string{"repo", "workflow", "read:org"}, ScopesPresent: true, Expiration: expires, ExpirationPresent: true},
		},
		{
			name: "200 fine-grained", status: 200,
			headers: map[string]string{"GitHub-Authentication-Token-Expiration": expires},
			access:  Access{Status: 200, Expiration: expires, ExpirationPresent: true},
		},
		{name: "200 without headers", status: 200, access: Access{Status: 200}},
		{
			name: "200 classic without scopes", status: 200,
			headers: map[string]string{"X-OAuth-Scopes": ""},
			access:  Access{Status: 200, ScopesPresent: true},
		},
		{
			name: "200 public_repo only", status: 200,
			headers: map[string]string{"X-OAuth-Scopes": "public_repo,repo:status"},
			access:  Access{Status: 200, Scopes: []string{"public_repo", "repo:status"}, ScopesPresent: true},
		},
		{name: "401", status: 401, message: refused, setup: true, class: retry.Permanent},
		{name: "403", status: 403, message: cannotManage(403), setup: true, class: retry.Permanent},
		{name: "404", status: 404, message: cannotManage(404), setup: true, class: retry.Permanent},
		{
			name: "403 accepted permissions", status: 403,
			headers: map[string]string{"X-Accepted-GitHub-Permissions": "administration=write"},
			message: cannotManage(403) + accepts + "administration=write", setup: true, class: retry.Permanent,
		},
		{
			name: "404 accepted combinations", status: 404,
			headers: map[string]string{"X-Accepted-GitHub-Permissions": "administration=write; actions=read,metadata=read"},
			message: cannotManage(404) + accepts + "administration=write; actions=read,metadata=read", setup: true, class: retry.Permanent,
		},
		{
			name: "403 accepted 200 characters", status: 403,
			headers: map[string]string{"X-Accepted-GitHub-Permissions": longest},
			message: cannotManage(403) + accepts + longest, setup: true, class: retry.Permanent,
		},
		{
			name: "403 accepted with upper case", status: 403,
			headers: map[string]string{"X-Accepted-GitHub-Permissions": "Administration=write"},
			message: cannotManage(403), setup: true, class: retry.Permanent,
		},
		{
			name: "403 accepted longer than 200", status: 403,
			headers: map[string]string{"X-Accepted-GitHub-Permissions": longest + "a"},
			message: cannotManage(403), setup: true, class: retry.Permanent,
		},
		{
			name: "403 SSO", status: 403,
			headers: map[string]string{"X-GitHub-SSO": ssoValue},
			message: cannotManage(403) + sso, setup: true, class: retry.Permanent,
		},
		{
			name: "403 accepted and SSO", status: 403,
			headers: map[string]string{"X-Accepted-GitHub-Permissions": "administration=write", "X-GitHub-SSO": ssoValue},
			message: cannotManage(403) + accepts + "administration=write" + sso, setup: true, class: retry.Permanent,
		},
		{
			name: "401 ignores hints", status: 401,
			headers: map[string]string{"X-Accepted-GitHub-Permissions": "administration=write", "X-GitHub-SSO": ssoValue},
			message: refused, setup: true, class: retry.Permanent,
		},
		{
			name: "403 rate limit", status: 403,
			headers: map[string]string{"X-RateLimit-Remaining": "0", "X-RateLimit-Reset": "1795000000"},
			class:   retry.RateLimited,
		},
		{name: "429", status: 429, headers: map[string]string{"Retry-After": "30"}, class: retry.RateLimited},
		{name: "502", status: 502, class: retry.Transient},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newTokenEnv(t, "github_pat_")
			e.srv.set(func(w http.ResponseWriter, r *http.Request) {
				for k, v := range tt.headers {
					w.Header().Set(k, v)
				}
				if tt.status == http.StatusOK {
					_, _ = w.Write([]byte(runnerJSON))
					return
				}
				// GitHub never echoes a token; the body does here to
				// prove that no error quotes it.
				writeJSON(w, tt.status, map[string]string{"message": "Bad credentials " + e.token})
			})
			ctx := context.Background()
			access, err := e.rest.CheckRepositoryAccess(ctx, testTarget)

			seen := e.srv.seen()
			if len(seen) != 1 || seen[0].path != "/repos/acme/widget/actions/runners" || seen[0].query != "per_page=1" {
				t.Errorf("requests = %+v, want one GET of /repos/acme/widget/actions/runners?per_page=1", seen)
			}
			if tt.status == http.StatusOK {
				if err != nil {
					t.Fatalf("error = %v", err)
				}
				if !reflect.DeepEqual(access, tt.access) {
					t.Errorf("access = %+v, want %+v", access, tt.access)
				}
				return
			}
			if err == nil {
				t.Fatal("no error")
			}
			assertNoToken(t, "error", err.Error(), e.token)
			if strings.Contains(err.Error(), "authorization_request") || strings.Contains(err.Error(), "orgs/acme") {
				t.Errorf("error shows the X-GitHub-SSO value: %v", err)
			}
			assertOp(t, err, retry.OpCheckTokenAccess)
			if c := retry.Classify(ctx, retry.OpCheckTokenAccess, err); c != tt.class {
				t.Errorf("Classify = %v, want %v", c, tt.class)
			}
			if got := errors.Is(err, retry.ErrTargetSetup); got != tt.setup {
				t.Errorf("errors.Is(ErrTargetSetup) = %v, want %v", got, tt.setup)
			}
			var se *StatusError
			if !errors.As(err, &se) || se.Code != tt.status {
				t.Fatalf("status of %v = %+v, want %d", err, se, tt.status)
			}
			if tt.setup && se.Message != tt.message {
				t.Errorf("message = %q\nwant      %q", se.Message, tt.message)
			}
		})
	}
}

// TestCheckRepositoryAccessRefusalExpiration proves that a refusal still
// returns the status and the expiry header R317 shows, and that rate
// limits and 5xx return an empty Access.
func TestCheckRepositoryAccessRefusalExpiration(t *testing.T) {
	const expires = "2026-11-27 00:00:00 UTC"
	tests := []struct {
		name   string
		status int
		expiry bool
		want   Access
	}{
		{"403 with expiry", 403, true, Access{Status: 403, Expiration: expires, ExpirationPresent: true}},
		{"404 with expiry", 404, true, Access{Status: 404, Expiration: expires, ExpirationPresent: true}},
		{"401 with expiry", 401, true, Access{Status: 401, Expiration: expires, ExpirationPresent: true}},
		{"403 without expiry", 403, false, Access{Status: 403}},
		{"429 with expiry", 429, true, Access{}},
		{"500 with expiry", 500, true, Access{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newTokenEnv(t, "ghp_")
			e.srv.set(func(w http.ResponseWriter, r *http.Request) {
				if tt.expiry {
					w.Header().Set("GitHub-Authentication-Token-Expiration", expires)
				}
				writeJSON(w, tt.status, map[string]string{"message": "no"})
			})
			access, err := e.rest.CheckRepositoryAccess(context.Background(), testTarget)
			if err == nil {
				t.Fatal("no error")
			}
			if !reflect.DeepEqual(access, tt.want) {
				t.Errorf("access = %+v, want %+v", access, tt.want)
			}
			if strings.Contains(err.Error(), expires) {
				t.Errorf("error shows the expiry: %v", err)
			}
		})
	}
}

// TestActionsPoliciesStatus proves 0003 SPEC R301 and R302: one GET of the
// policies endpoint whose status is returned as it is; only a rate limit
// or a transport error is an error.
func TestActionsPoliciesStatus(t *testing.T) {
	tests := []struct {
		name      string
		status    int
		headers   map[string]string
		rateLimit bool
	}{
		{name: "200", status: 200},
		{name: "403", status: 403},
		{name: "404", status: 404},
		{name: "401", status: 401},
		{name: "500", status: 500},
		{name: "429", status: 429, headers: map[string]string{"Retry-After": "120"}, rateLimit: true},
		{name: "403 rate limit", status: 403, headers: map[string]string{"X-RateLimit-Remaining": "0", "X-RateLimit-Reset": "1795000000"}, rateLimit: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newTokenEnv(t, "github_pat_")
			e.srv.set(func(w http.ResponseWriter, r *http.Request) {
				for k, v := range tt.headers {
					w.Header().Set(k, v)
				}
				writeJSON(w, tt.status, map[string]string{"message": "answer"})
			})
			code, err := e.rest.ActionsPoliciesStatus(context.Background(), testTarget)
			if code != tt.status {
				t.Errorf("status = %d, want %d", code, tt.status)
			}
			seen := e.srv.seen()
			if len(seen) != 1 || seen[0].path != "/repos/acme/widget/actions/policies" || seen[0].query != "per_page=1" || seen[0].method != http.MethodGet {
				t.Errorf("requests = %+v, want one GET of /repos/acme/widget/actions/policies?per_page=1", seen)
			}
			if !tt.rateLimit {
				if err != nil {
					t.Errorf("error = %v, want none", err)
				}
				return
			}
			var rl *retry.RateLimitError
			if !errors.As(err, &rl) {
				t.Fatalf("error = %v, want a rate limit", err)
			}
			if rl.RetryAfter == 0 && rl.Reset.IsZero() {
				t.Errorf("rate limit %+v carries no time", rl)
			}
			assertOp(t, err, retry.OpGetActionsPolicies)
		})
	}

	t.Run("transport error", func(t *testing.T) {
		e := newTokenEnv(t, "github_pat_")
		e.srv.srv.Close()
		code, err := e.rest.ActionsPoliciesStatus(context.Background(), testTarget)
		if err == nil || code != 0 {
			t.Fatalf("status = %d, error = %v, want 0 and an error", code, err)
		}
		assertOp(t, err, retry.OpGetActionsPolicies)
		assertNoToken(t, "error", err.Error(), e.token)
	})
}

func TestNewTokenRESTRejectsBadConfig(t *testing.T) {
	token := fixtureToken(t, "ghp_")
	good := TokenRESTConfig{
		BaseURL: DefaultAPIBaseURL, Token: token,
		HTTPClient: http.DefaultClient, Clock: newFakeClock(), Secrets: logging.NewSecrets(),
	}
	if _, err := NewTokenREST(good); err != nil {
		t.Fatalf("good config: %v", err)
	}
	for name, mutate := range map[string]func(*TokenRESTConfig){
		"no token":      func(c *TokenRESTConfig) { c.Token = "" },
		"no secrets":    func(c *TokenRESTConfig) { c.Secrets = nil },
		"no client":     func(c *TokenRESTConfig) { c.HTTPClient = nil },
		"no clock":      func(c *TokenRESTConfig) { c.Clock = nil },
		"relative base": func(c *TokenRESTConfig) { c.BaseURL = "/api" },
	} {
		cfg := good
		mutate(&cfg)
		_, err := NewTokenREST(cfg)
		if err == nil {
			t.Errorf("%s: no error", name)
			continue
		}
		assertNoToken(t, name+" error", err.Error(), token)
	}
}

// TestTokenRegisteredAsSecret proves 0003 SPEC R277: both constructors
// register the token before any request, and errors of failing requests do
// not contain it. The fixture has no GitHub prefix, so only the
// registration can hide it.
func TestTokenRegisteredAsSecret(t *testing.T) {
	ctx := context.Background()

	t.Run("REST", func(t *testing.T) {
		e := newTokenEnv(t, "fixture-")
		if got := e.secrets.Redact("token " + e.token); strings.Contains(got, e.token) {
			t.Fatal("token not registered after NewTokenREST")
		}
		e.srv.set(func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"message": "failed for " + e.token})
		})
		_, err := e.rest.ListRunners(ctx, testTarget)
		if err == nil {
			t.Fatal("ListRunners: no error")
		}
		assertNoToken(t, "ListRunners error", err.Error(), e.token)
		_, err = e.rest.CheckRepositoryAccess(ctx, testTarget)
		if err == nil {
			t.Fatal("CheckRepositoryAccess: no error")
		}
		assertNoToken(t, "CheckRepositoryAccess error", err.Error(), e.token)
	})

	t.Run("scale set", func(t *testing.T) {
		secrets := logging.NewSecrets()
		token := fixtureToken(t, "fixture-")
		if _, err := NewScaleSetClient(ScaleSetConfig{ConfigURL: "https://github.com/acme/widget", Token: token, Secrets: secrets}); err != nil {
			t.Fatal(err)
		}
		if got := secrets.Redact("token " + token); strings.Contains(got, token) {
			t.Fatal("token not registered after NewScaleSetClient")
		}

		e := newTokenTestEnv(t, token)
		e.gh.registrationToken = func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, http.StatusForbidden, map[string]string{"message": "Resource not accessible by " + token})
		}
		callCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		_, err := e.client.GetOrCreateScaleSetInGroup(callCtx, DefaultRunnerGroupID, "linux-arm64-deploy")
		if err == nil {
			t.Fatal("GetOrCreateScaleSetInGroup: no error")
		}
		assertNoToken(t, "error", err.Error(), token)
		assertNoToken(t, "logs", e.logs.String(), token)
		if c := retry.Classify(ctx, retry.OpGetOrCreateScaleSet, err); c != retry.Permanent {
			t.Errorf("Classify(403 at the registration token) = %v, want permanent", c)
		}
	})
}
