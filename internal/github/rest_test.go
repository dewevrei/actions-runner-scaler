package github

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dewevrei/actions-runner-scaler/internal/gate"
	"github.com/dewevrei/actions-runner-scaler/internal/logging"
	"github.com/dewevrei/actions-runner-scaler/internal/retry"
)

type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{now: time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// fakeREST is a GitHub REST server for the four endpoints.
type fakeREST struct {
	t     *testing.T
	srv   *httptest.Server
	clock *fakeClock
	key   *rsa.PrivateKey

	mu          sync.Mutex
	tokens      []string // installation tokens handed out
	tokenCalls  int
	apiRequests []*http.Request
	tokenReply  http.HandlerFunc // replaces the token response when set
	api         http.HandlerFunc // answers /repos/...
}

func newFakeREST(t *testing.T, clock *fakeClock) *fakeREST {
	t.Helper()
	f := &fakeREST{t: t, clock: clock, key: rsaKey(t)}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /app/installations/42/access_tokens", func(w http.ResponseWriter, r *http.Request) {
		if err := verifyJWT(&f.key.PublicKey, strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")); err != nil {
			t.Errorf("installation token request: %v", err)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		f.mu.Lock()
		f.tokenCalls++
		reply := f.tokenReply
		// No ghs_ prefix: the logging package redacts that format anyway,
		// which would hide whether the token was registered.
		token := "fake-rest-token-" + strconv.Itoa(f.tokenCalls) + "-abcdefghijklmnop"
		if reply == nil {
			f.tokens = append(f.tokens, token)
		}
		f.mu.Unlock()
		if reply != nil {
			reply(w, r)
			return
		}
		writeJSON(w, http.StatusCreated, map[string]any{
			"token":      token,
			"expires_at": clock.Now().Add(time.Hour).Format(time.RFC3339),
		})
	})
	mux.HandleFunc("/repos/", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.apiRequests = append(f.apiRequests, r)
		api := f.api
		valid := false
		for _, tok := range f.tokens {
			valid = valid || r.Header.Get("Authorization") == "Bearer "+tok
		}
		f.mu.Unlock()
		if !valid {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		api(w, r)
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeREST) set(fn func(*fakeREST)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn(f)
}

func (f *fakeREST) counts() (tokenCalls, apiCalls int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.tokenCalls, len(f.apiRequests)
}

func (f *fakeREST) lastRequest() *http.Request {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.apiRequests[len(f.apiRequests)-1]
}

// verifyJWT checks an RS256 JWT's signature and returns an error if it is
// not valid.
func verifyJWT(pub *rsa.PublicKey, token string) error {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return fmt.Errorf("JWT has %d parts", len(parts))
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return err
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	return rsa.VerifyPKCS1v15(pub, crypto.SHA256, digest[:], sig)
}

type restEnv struct {
	f       *fakeREST
	rest    *REST
	clock   *fakeClock
	secrets *logging.Secrets
	signer  *AppSigner
}

func newRESTEnv(t *testing.T) *restEnv {
	t.Helper()
	clock := newFakeClock()
	f := newFakeREST(t, clock)
	secrets := logging.NewSecrets()
	signer, err := NewAppSigner("Iv1.testclient", rsaKey(t), clock, secrets)
	if err != nil {
		t.Fatal(err)
	}
	rest, err := NewREST(RESTConfig{
		BaseURL:        f.srv.URL,
		InstallationID: 42,
		Signer:         signer,
		HTTPClient:     f.srv.Client(),
		Clock:          clock,
		Secrets:        secrets,
	})
	if err != nil {
		t.Fatal(err)
	}
	return &restEnv{f: f, rest: rest, clock: clock, secrets: secrets, signer: signer}
}

// assertClean fails when text holds an installation token or a URL query.
func (e *restEnv) assertClean(t *testing.T, text string) {
	t.Helper()
	e.f.mu.Lock()
	tokens := append([]string(nil), e.f.tokens...)
	e.f.mu.Unlock()
	for _, tok := range tokens {
		if strings.Contains(text, tok) {
			t.Errorf("text contains an installation token: %s", text)
		}
	}
	for _, q := range []string{"per_page=", "page=", "exclude_pull_requests=", "branch=", "created="} {
		if strings.Contains(text, q) {
			t.Errorf("text contains a URL query: %s", text)
		}
	}
}

func TestAppSignerToken(t *testing.T) {
	clock := newFakeClock()
	secrets := logging.NewSecrets()
	signer, err := NewAppSigner("Iv1.testclient", rsaKey(t), clock, secrets)
	if err != nil {
		t.Fatal(err)
	}
	token, err := signer.Token(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := verifyJWT(&rsaKey(t).PublicKey, token); err != nil {
		t.Fatalf("signature: %v", err)
	}
	parts := strings.Split(token, ".")
	var header map[string]string
	decodeSegment(t, parts[0], &header)
	if header["alg"] != "RS256" || header["typ"] != "JWT" {
		t.Errorf("header = %v", header)
	}
	var claims struct {
		Iat int64  `json:"iat"`
		Exp int64  `json:"exp"`
		Iss string `json:"iss"`
	}
	decodeSegment(t, parts[1], &claims)
	now := clock.Now().Unix()
	if claims.Iat != now-60 || claims.Exp != now+9*60 || claims.Iss != "Iv1.testclient" {
		t.Errorf("claims = %+v, want iat now-60, exp now+540, iss client ID", claims)
	}
	if claims.Exp-time.Now().Unix() > 10*60 && claims.Exp-now > 10*60 {
		t.Error("exp more than ten minutes ahead")
	}
	if !registered(secrets, token) {
		t.Error("JWT not registered as a secret")
	}

	// Once expired, the JWT is dropped from the set at the next signing.
	clock.Advance(10 * time.Minute)
	token2, err := signer.Token(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if registered(secrets, token) {
		t.Error("expired JWT still registered")
	}
	if !registered(secrets, token2) {
		t.Error("new JWT not registered")
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := signer.Token(ctx); !errors.Is(err, context.Canceled) {
		t.Errorf("Token with canceled ctx = %v", err)
	}
}

func decodeSegment(t *testing.T, seg string, v any) {
	t.Helper()
	data, err := base64.RawURLEncoding.DecodeString(seg)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, v); err != nil {
		t.Fatal(err)
	}
}

func TestNewAppSignerRejectsMissing(t *testing.T) {
	clock, secrets, key := newFakeClock(), logging.NewSecrets(), rsaKey(t)
	for name, args := range map[string]struct {
		id      string
		key     *rsa.PrivateKey
		clock   Clock
		secrets *logging.Secrets
	}{
		"client ID": {"", key, clock, secrets},
		"key":       {"x", nil, clock, secrets},
		"clock":     {"x", key, nil, secrets},
		"secrets":   {"x", key, clock, nil},
	} {
		if _, err := NewAppSigner(args.id, args.key, args.clock, args.secrets); err == nil {
			t.Errorf("missing %s: no error", name)
		}
	}
}

func runJSON(extra string) string {
	return `{"id":123,"workflow_id":777,"run_number":42,"created_at":"2026-11-24T11:59:30Z",` +
		`"head_branch":"main","head_repository":{"full_name":"test-org/test-repo"},` +
		`"event":"push","path":".github/workflows/deploy.yml"` + extra + `}`
}

func TestGetRun(t *testing.T) {
	base := gate.Run{
		ID: 123, WorkflowID: 777, RunNumber: 42, CreatedAt: time.Date(2026, 11, 24, 11, 59, 30, 0, time.UTC),
		HeadRepository: "test-org/test-repo", HeadBranch: "main", Event: "push", Path: ".github/workflows/deploy.yml",
	}
	with := func(shape gate.ReferencedWorkflows) gate.Run {
		r := base
		r.ReferencedWorkflows = shape
		return r
	}
	tests := []struct {
		name string
		body string
		want gate.Run
	}{
		{"referenced_workflows absent", runJSON(""), with(gate.ReferencedWorkflowsAbsent)},
		{"referenced_workflows null", runJSON(`,"referenced_workflows":null`), with(gate.ReferencedWorkflowsNull)},
		{"referenced_workflows empty", runJSON(`,"referenced_workflows":[]`), with(gate.ReferencedWorkflowsEmpty)},
		{"referenced_workflows one", runJSON(`,"referenced_workflows":[{"path":"test-org/test-repo/.github/workflows/x.yml@main","sha":"abc","ref":"refs/heads/main"}]`), with(gate.ReferencedWorkflowsNonEmpty)},
		{
			"null head branch and repository, no numbers or created_at",
			`{"id":123,"head_branch":null,"head_repository":null,"event":"push","path":"p","referenced_workflows":[]}`,
			gate.Run{ID: 123, Event: "push", Path: "p", ReferencedWorkflows: gate.ReferencedWorkflowsEmpty},
		},
		{
			"null workflow_id, run_number and created_at",
			`{"id":123,"workflow_id":null,"run_number":null,"created_at":null,"event":"push","path":"p","referenced_workflows":[]}`,
			gate.Run{ID: 123, Event: "push", Path: "p", ReferencedWorkflows: gate.ReferencedWorkflowsEmpty},
		},
		{
			"created_at with offset",
			`{"id":123,"workflow_id":6,"run_number":7,"created_at":"2026-11-24T20:59:30+09:00","referenced_workflows":[]}`,
			gate.Run{ID: 123, WorkflowID: 6, RunNumber: 7, CreatedAt: time.Date(2026, 11, 24, 11, 59, 30, 0, time.UTC), ReferencedWorkflows: gate.ReferencedWorkflowsEmpty},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			e := newRESTEnv(t)
			e.f.set(func(f *fakeREST) {
				f.api = func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("Content-Type", "application/json")
					_, _ = w.Write([]byte(tc.body))
				}
			})
			got, err := e.rest.GetRun(context.Background(), testOwner, testRepo, 123)
			if err != nil {
				t.Fatal(err)
			}
			if !equalRun(got, tc.want) {
				t.Errorf("run = %+v, want %+v", got, tc.want)
			}
			req := e.f.lastRequest()
			if req.Method != http.MethodGet || req.URL.Path != "/repos/test-org/test-repo/actions/runs/123" {
				t.Errorf("request = %s %s", req.Method, req.URL.Path)
			}
			if req.URL.Query().Get("exclude_pull_requests") != "true" {
				t.Errorf("query = %q", req.URL.RawQuery)
			}
			if req.Header.Get("Accept") != "application/vnd.github+json" || req.Header.Get("X-GitHub-Api-Version") != "2022-11-28" {
				t.Errorf("headers = %v", req.Header)
			}
		})
	}
}

// equalRun compares runs, CreatedAt by instant.
func equalRun(a, b gate.Run) bool {
	if !a.CreatedAt.Equal(b.CreatedAt) {
		return false
	}
	a.CreatedAt, b.CreatedAt = time.Time{}, time.Time{}
	return a == b
}

func TestGetRunBadValuesAreErrors(t *testing.T) {
	for name, body := range map[string]string{
		"created_at not RFC 3339": `{"id":1,"workflow_id":2,"run_number":3,"created_at":"24 Nov 2026","referenced_workflows":[]}`,
		"created_at a number":     `{"id":1,"workflow_id":2,"run_number":3,"created_at":1764000000,"referenced_workflows":[]}`,
		"run_number a string":     `{"id":1,"workflow_id":2,"run_number":"3","referenced_workflows":[]}`,
		"id a fraction":           `{"id":1.5,"referenced_workflows":[]}`,
		"id of another run":       `{"id":2,"workflow_id":2,"run_number":3,"created_at":"2026-11-24T11:59:30Z","referenced_workflows":[]}`,
		"id missing":              `{"workflow_id":2,"run_number":3,"created_at":"2026-11-24T11:59:30Z","referenced_workflows":[]}`,
		"id null":                 `{"id":null,"workflow_id":2,"run_number":3,"created_at":"2026-11-24T11:59:30Z","referenced_workflows":[]}`,
	} {
		t.Run(name, func(t *testing.T) {
			e := newRESTEnv(t)
			e.f.set(func(f *fakeREST) {
				f.api = func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(body)) }
			})
			_, err := e.rest.GetRun(context.Background(), testOwner, testRepo, 1)
			if err == nil {
				t.Fatal("accepted")
			}
			t.Logf("error: %v", err)
			assertOp(t, err, retry.OpGetRun)
			if strings.Contains(err.Error(), "24 Nov 2026") {
				t.Errorf("error quotes the body: %v", err)
			}
		})
	}
}

func TestGetRunBadReferencedWorkflowsIsError(t *testing.T) {
	e := newRESTEnv(t)
	e.f.set(func(f *fakeREST) {
		f.api = func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(runJSON(`,"referenced_workflows":{"path":"x"}`)))
		}
	})
	if _, err := e.rest.GetRun(context.Background(), testOwner, testRepo, 123); err == nil {
		t.Error("object referenced_workflows accepted")
	}
}

// deadlineDoer records the deadline of each request's context.
type deadlineDoer struct {
	next      HTTPDoer
	mu        sync.Mutex
	deadlines []time.Duration // from the request
	instants  []time.Time
}

func (d *deadlineDoer) Do(req *http.Request) (*http.Response, error) {
	if dl, ok := req.Context().Deadline(); ok {
		d.mu.Lock()
		d.deadlines = append(d.deadlines, time.Until(dl))
		d.instants = append(d.instants, dl)
		d.mu.Unlock()
	}
	return d.next.Do(req)
}

func TestGetRunHasThirtySecondDeadline(t *testing.T) {
	e := newRESTEnv(t)
	e.f.set(func(f *fakeREST) {
		f.api = func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(runJSON(`,"referenced_workflows":[]`)))
		}
	})
	doer := &deadlineDoer{next: e.f.srv.Client()}
	rest, err := NewREST(RESTConfig{
		BaseURL: e.f.srv.URL, InstallationID: 42, Signer: e.signer,
		HTTPClient: doer, Clock: e.clock, Secrets: e.secrets,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rest.GetRun(context.Background(), testOwner, testRepo, 123); err != nil {
		t.Fatal(err)
	}
	if len(doer.deadlines) != 2 {
		t.Fatalf("requests with deadline = %d, want 2 (token and run)", len(doer.deadlines))
	}
	for _, d := range doer.deadlines {
		if d <= 25*time.Second || d > 30*time.Second {
			t.Errorf("deadline in %v, want about 30s", d)
		}
	}
}

func TestInstallationTokenCachedUntilFiveMinutesBeforeExpiry(t *testing.T) {
	e := newRESTEnv(t)
	e.f.set(func(f *fakeREST) {
		f.api = func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(runJSON(`,"referenced_workflows":[]`)))
		}
	})
	ctx := context.Background()
	get := func() {
		t.Helper()
		if _, err := e.rest.GetRun(ctx, testOwner, testRepo, 123); err != nil {
			t.Fatal(err)
		}
	}
	get()
	get()
	if n, _ := e.f.counts(); n != 1 {
		t.Fatalf("token requests = %d, want 1", n)
	}
	first := e.f.tokens[0]
	if !registered(e.secrets, first) {
		t.Error("installation token not registered as a secret")
	}
	e.clock.Advance(55*time.Minute - time.Second)
	get()
	if n, _ := e.f.counts(); n != 1 {
		t.Fatalf("token requests before the margin = %d, want 1", n)
	}
	e.clock.Advance(time.Second)
	get()
	if n, _ := e.f.counts(); n != 2 {
		t.Fatalf("token requests at the margin = %d, want 2", n)
	}
	// The first token expires an hour after issue and is then forgotten.
	e.clock.Advance(time.Hour)
	get()
	if registered(e.secrets, first) {
		t.Error("expired installation token still registered")
	}
}

func TestUnauthorizedDropsCachedToken(t *testing.T) {
	e := newRESTEnv(t)
	var calls int
	e.f.set(func(f *fakeREST) {
		f.api = func(w http.ResponseWriter, r *http.Request) {
			calls++
			if calls == 1 {
				writeJSON(w, http.StatusUnauthorized, map[string]string{"message": "Bad credentials"})
				return
			}
			_, _ = w.Write([]byte(runJSON(`,"referenced_workflows":[]`)))
		}
	})
	ctx := context.Background()
	_, err := e.rest.GetRun(ctx, testOwner, testRepo, 123)
	if c := retry.Classify(ctx, retry.OpGetRun, err); c != retry.Permanent {
		t.Errorf("Classify = %v, want permanent", c)
	}
	if _, err := e.rest.GetRun(ctx, testOwner, testRepo, 123); err != nil {
		t.Fatal(err)
	}
	if n, _ := e.f.counts(); n != 2 {
		t.Errorf("token requests = %d, want 2", n)
	}
}

func TestRESTErrors(t *testing.T) {
	reset := time.Date(2026, 11, 24, 12, 30, 0, 0, time.UTC)
	tests := []struct {
		name       string
		respond    func(w http.ResponseWriter)
		wantClass  retry.Class
		retryAfter time.Duration
		reset      time.Time
		status     int
	}{
		{
			name:      "429 without headers",
			respond:   func(w http.ResponseWriter) { w.WriteHeader(http.StatusTooManyRequests) },
			wantClass: retry.RateLimited,
		},
		{
			name: "403 primary limit",
			respond: func(w http.ResponseWriter) {
				w.Header().Set("X-RateLimit-Remaining", "0")
				w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(reset.Unix(), 10))
				writeJSON(w, http.StatusForbidden, map[string]string{"message": "API rate limit exceeded for installation ID 42."})
			},
			wantClass: retry.RateLimited, reset: reset,
		},
		{
			name: "403 with Retry-After",
			respond: func(w http.ResponseWriter) {
				w.Header().Set("Retry-After", "90")
				w.Header().Set("X-RateLimit-Remaining", "4000")
				w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(reset.Unix(), 10))
				w.WriteHeader(http.StatusForbidden)
			},
			wantClass: retry.RateLimited, retryAfter: 90 * time.Second,
		},
		{
			name: "403 secondary limit message",
			respond: func(w http.ResponseWriter) {
				w.Header().Set("X-RateLimit-Remaining", "4000")
				w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(reset.Unix(), 10))
				writeJSON(w, http.StatusForbidden, map[string]string{"message": "You have exceeded a secondary rate limit."})
			},
			wantClass: retry.RateLimited,
		},
		{
			// GitHub waits for the reset whenever the quota is spent, also
			// for a secondary limit.
			name: "403 secondary limit with spent quota",
			respond: func(w http.ResponseWriter) {
				w.Header().Set("X-RateLimit-Remaining", "0")
				w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(reset.Unix(), 10))
				writeJSON(w, http.StatusForbidden, map[string]string{"message": "You have exceeded a secondary rate limit."})
			},
			wantClass: retry.RateLimited, reset: reset,
		},
		{
			name: "429 with HTTP date Retry-After",
			respond: func(w http.ResponseWriter) {
				w.Header().Set("Retry-After", time.Date(2026, 11, 24, 12, 2, 0, 0, time.UTC).Format(http.TimeFormat))
				w.WriteHeader(http.StatusTooManyRequests)
			},
			wantClass: retry.RateLimited, retryAfter: 2 * time.Minute,
		},
		{
			// No header marks it; the message alone does.
			name: "403 rate limit message with quota left",
			respond: func(w http.ResponseWriter) {
				w.Header().Set("X-RateLimit-Remaining", "4000")
				writeJSON(w, http.StatusForbidden, map[string]string{"message": "API rate limit exceeded for installation ID 42."})
			},
			wantClass: retry.RateLimited,
		},
		{
			name: "403 secondary rate-limit message",
			respond: func(w http.ResponseWriter) {
				w.Header().Set("X-RateLimit-Remaining", "4000")
				writeJSON(w, http.StatusForbidden, map[string]string{"message": "You have exceeded a secondary rate-limit."})
			},
			wantClass: retry.RateLimited,
		},
		{
			name: "429 with Retry-After past time.Duration",
			respond: func(w http.ResponseWriter) {
				w.Header().Set("Retry-After", "99999999999999")
				w.WriteHeader(http.StatusTooManyRequests)
			},
			wantClass: retry.RateLimited, retryAfter: retry.MaxRateLimitWait,
		},
		{
			name: "403 not a rate limit",
			respond: func(w http.ResponseWriter) {
				w.Header().Set("X-RateLimit-Remaining", "4000")
				writeJSON(w, http.StatusForbidden, map[string]string{"message": "Resource not accessible by integration"})
			},
			wantClass: retry.Permanent, status: 403,
		},
		{
			name: "404",
			respond: func(w http.ResponseWriter) {
				writeJSON(w, http.StatusNotFound, map[string]string{"message": "Not Found"})
			},
			wantClass: retry.Permanent, status: 404,
		},
		{
			name:      "502",
			respond:   func(w http.ResponseWriter) { w.WriteHeader(http.StatusBadGateway) },
			wantClass: retry.Transient, status: 502,
		},
		{
			name: "undecodable 200",
			respond: func(w http.ResponseWriter) {
				_, _ = w.Write([]byte(`{"head_branch":"main","path":`))
			},
			wantClass: retry.Transient,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			e := newRESTEnv(t)
			e.f.set(func(f *fakeREST) { f.api = func(w http.ResponseWriter, r *http.Request) { tc.respond(w) } })
			ctx := context.Background()
			_, err := e.rest.GetRun(ctx, testOwner, testRepo, 1)
			if err == nil {
				t.Fatal("GetRun succeeded")
			}
			t.Logf("error: %v", err)
			if c := retry.Classify(ctx, retry.OpGetRun, err); c != tc.wantClass {
				t.Errorf("Classify = %v, want %v", c, tc.wantClass)
			}
			var op *retry.OpError
			if !errors.As(err, &op) || op.Op != retry.OpGetRun {
				t.Errorf("error %v does not name %q", err, retry.OpGetRun)
			}
			var rl *retry.RateLimitError
			if errors.As(err, &rl) {
				if rl.RetryAfter != tc.retryAfter || !rl.Reset.Equal(tc.reset) {
					t.Errorf("RateLimitError = {RetryAfter %v, Reset %v}, want {%v, %v}", rl.RetryAfter, rl.Reset, tc.retryAfter, tc.reset)
				}
			}
			var se *StatusError
			gotStatus := 0
			if errors.As(err, &se) && rl == nil {
				gotStatus = se.Code
			}
			if gotStatus != tc.status {
				t.Errorf("StatusError code = %d, want %d", gotStatus, tc.status)
			}
			e.assertClean(t, err.Error())
		})
	}
}

func TestInstallationTokenErrors(t *testing.T) {
	e := newRESTEnv(t)
	e.f.set(func(f *fakeREST) {
		f.tokenReply = func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Retry-After", "120")
			w.WriteHeader(http.StatusTooManyRequests)
		}
	})
	ctx := context.Background()
	_, err := e.rest.ListRepoRunners(ctx, testOwner, testRepo)
	var rl *retry.RateLimitError
	if !errors.As(err, &rl) || rl.RetryAfter != 2*time.Minute {
		t.Fatalf("error = %v, want rate limit with RetryAfter 2m", err)
	}
	if c := retry.Classify(ctx, retry.OpListRepoRunners, err); c != retry.RateLimited {
		t.Errorf("Classify = %v", c)
	}
	if !strings.Contains(err.Error(), string(retry.OpInstallationToken)) {
		t.Errorf("error %q does not name the token call", err)
	}
	if _, api := e.f.counts(); api != 0 {
		t.Errorf("API requests = %d, want 0", api)
	}

	// A token body without a token is an error that does not echo the body.
	e.f.set(func(f *fakeREST) {
		f.tokenReply = func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"token":"ghs_partialSecretValue123","expires_at":`))
		}
	})
	_, err = e.rest.GetRun(ctx, testOwner, testRepo, 1)
	if err == nil || strings.Contains(err.Error(), "ghs_partialSecretValue123") {
		t.Errorf("error = %v", err)
	}
	if c := retry.Classify(ctx, retry.OpGetRun, err); c != retry.Transient {
		t.Errorf("Classify = %v, want transient", c)
	}
}

func TestInstallationTokenRefusalIsAuthError(t *testing.T) {
	tests := []struct {
		name     string
		respond  func(w http.ResponseWriter)
		wantAuth bool
		want     retry.Class
	}{
		{"404", func(w http.ResponseWriter) {
			writeJSON(w, http.StatusNotFound, map[string]string{"message": "Not Found"})
		}, true, retry.Permanent},
		{"401", func(w http.ResponseWriter) {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"message": "Bad credentials"})
		}, true, retry.Permanent},
		{"403 not a rate limit", func(w http.ResponseWriter) {
			w.Header().Set("X-RateLimit-Remaining", "4000")
			writeJSON(w, http.StatusForbidden, map[string]string{"message": "Forbidden"})
		}, true, retry.Permanent},
		{"429", func(w http.ResponseWriter) { w.WriteHeader(http.StatusTooManyRequests) }, false, retry.RateLimited},
		{"500", func(w http.ResponseWriter) { w.WriteHeader(http.StatusInternalServerError) }, false, retry.Transient},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			e := newRESTEnv(t)
			e.f.set(func(f *fakeREST) {
				f.tokenReply = func(w http.ResponseWriter, r *http.Request) { tc.respond(w) }
			})
			ctx := context.Background()
			_, err := e.rest.ListRepoRunners(ctx, testOwner, testRepo)
			if err == nil {
				t.Fatal("ListRepoRunners succeeded")
			}
			if got := errors.Is(err, retry.ErrAuth); got != tc.wantAuth {
				t.Errorf("errors.Is(err, ErrAuth) = %v, want %v (err %v)", got, tc.wantAuth, err)
			}
			if c := retry.Classify(ctx, retry.OpListRepoRunners, err); c != tc.want {
				t.Errorf("Classify = %v, want %v", c, tc.want)
			}
		})
	}
}

// TestInstallationTokenConcurrentCallsCreateOne starts many calls at once
// with no cached token; they share one new installation token.
func TestInstallationTokenConcurrentCallsCreateOne(t *testing.T) {
	e := newRESTEnv(t)
	release := make(chan struct{})
	e.f.set(func(f *fakeREST) {
		f.api = func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(`{"runners":[]}`))
		}
	})
	const callers = 16
	var started, wg sync.WaitGroup
	errs := make(chan error, callers)
	for range callers {
		started.Add(1)
		wg.Add(1)
		go func() {
			defer wg.Done()
			started.Done()
			<-release
			_, err := e.rest.ListRepoRunners(context.Background(), testOwner, testRepo)
			errs <- err
		}()
	}
	started.Wait()
	close(release)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if tokens, api := e.f.counts(); tokens != 1 || api != callers {
		t.Errorf("token requests = %d, API requests = %d, want 1 and %d", tokens, api, callers)
	}
}

func TestDropTokenHonorsContext(t *testing.T) {
	e := newRESTEnv(t)
	e.rest.token, e.rest.tokenExpires = "cached-token", e.clock.Now().Add(time.Hour)

	// Another caller holds the lock; a canceled context gives up and the
	// token stays.
	e.rest.tokenLock <- struct{}{}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	done := make(chan struct{})
	go func() {
		e.rest.dropToken(ctx, "cached-token")
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("dropToken did not return after its context ended")
	}
	<-e.rest.tokenLock
	if e.rest.token != "cached-token" {
		t.Errorf("token = %q, want it kept", e.rest.token)
	}

	// With the lock free, a stale token leaves the cache alone and the
	// cached one is dropped.
	e.rest.dropToken(context.Background(), "other-token")
	if e.rest.token != "cached-token" {
		t.Errorf("token = %q after dropping another token", e.rest.token)
	}
	e.rest.dropToken(context.Background(), "cached-token")
	if e.rest.token != "" || !e.rest.tokenExpires.IsZero() {
		t.Errorf("token = %q, expires %v, want dropped", e.rest.token, e.rest.tokenExpires)
	}
}

func TestParseRetryAfter(t *testing.T) {
	now := time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		value  string
		want   time.Duration
		wantOK bool
	}{
		{"", 0, false},
		{"  ", 0, false},
		{"0", 0, true},
		{"90", 90 * time.Second, true},
		{" 120 ", 2 * time.Minute, true},
		// No cap here; retry.MaxRateLimitWait caps the wait.
		{"86401", 86401 * time.Second, true},
		{"9223372036", 9223372036 * time.Second, true}, // largest that fits
		// A wait too long for a time.Duration is the longest wait, not the
		// shorter wait for a missing header.
		{"9223372037", retry.MaxRateLimitWait, true},
		{"99999999999999", retry.MaxRateLimitWait, true},
		{"99999999999999999999999", retry.MaxRateLimitWait, true}, // past int64
		{"-1", 0, false},
		{"-0", 0, false},
		{"-99999999999999999999999", 0, false},
		{"1.5", 0, false},
		{"soon", 0, false},
		{now.Add(2 * time.Minute).Format(http.TimeFormat), 2 * time.Minute, true},
		{now.Add(48 * time.Hour).Format(http.TimeFormat), 48 * time.Hour, true},
		{time.Date(9999, 1, 1, 0, 0, 0, 0, time.UTC).Format(http.TimeFormat), retry.MaxRateLimitWait, true}, // past time.Duration
		{now.Add(-time.Minute).Format(http.TimeFormat), 0, false},
		{now.Format(http.TimeFormat), 0, false},
	}
	for _, tc := range tests {
		got, ok := parseRetryAfter(tc.value, now)
		if got != tc.want || ok != tc.wantOK {
			t.Errorf("parseRetryAfter(%q) = %v, %v, want %v, %v", tc.value, got, ok, tc.want, tc.wantOK)
		}
	}
}

// TestRateLimitWaitSameOnBothPaths sends the same rate-limited response to
// the library path (RemoveRunner) and the REST path (GetRun) and checks
// that retry.NextAllowed holds the call back equally long after either.
func TestRateLimitWaitSameOnBothPaths(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	unix := func(at time.Time) string { return strconv.FormatInt(at.Unix(), 10) }
	tests := []struct {
		name    string
		status  int
		message string
		header  map[string]string
		want    time.Duration
	}{
		{"short Retry-After", 429, "", map[string]string{"Retry-After": "120"}, 2 * time.Minute},
		{"Retry-After past the cap", 429, "", map[string]string{"Retry-After": "999999999"}, retry.MaxRateLimitWait},
		// The library drops these values; the recorded header gives the
		// longest wait on both paths.
		{"Retry-After past time.Duration", 429, "", map[string]string{"Retry-After": "99999999999999"}, retry.MaxRateLimitWait},
		{"Retry-After past int64", 429, "", map[string]string{"Retry-After": "99999999999999999999999"}, retry.MaxRateLimitWait},
		{"Retry-After past time.Duration on 403", 403, "", map[string]string{"Retry-After": "99999999999999"}, retry.MaxRateLimitWait},
		{"far HTTP date", 429, "", map[string]string{"Retry-After": now.Add(30 * 24 * time.Hour).Format(http.TimeFormat)}, retry.MaxRateLimitWait},
		{"HTTP date past time.Duration", 429, "", map[string]string{"Retry-After": time.Date(9999, 1, 1, 0, 0, 0, 0, time.UTC).Format(http.TimeFormat)}, retry.MaxRateLimitWait},
		{"Retry-After past time.Duration, spent primary", 403, "API rate limit exceeded", map[string]string{
			"Retry-After": "99999999999999", "X-RateLimit-Remaining": "0", "X-RateLimit-Reset": unix(now.Add(15 * time.Minute)),
		}, retry.MaxRateLimitWait},
		{"far reset, primary", 403, "API rate limit exceeded", map[string]string{
			"X-RateLimit-Remaining": "0", "X-RateLimit-Reset": unix(now.Add(10 * 24 * time.Hour)),
		}, retry.MaxRateLimitWait},
		{"reset, primary", 403, "API rate limit exceeded", map[string]string{
			"X-RateLimit-Remaining": "0", "X-RateLimit-Reset": unix(now.Add(15 * time.Minute)),
		}, 15 * time.Minute},
		{"reset, secondary with spent quota", 403, "You have exceeded a secondary rate limit", map[string]string{
			"X-RateLimit-Remaining": "0", "X-RateLimit-Reset": unix(now.Add(10 * time.Minute)),
		}, 10 * time.Minute},
		{"short Retry-After, secondary with spent quota", 403, "You have exceeded a secondary rate limit", map[string]string{
			"Retry-After": "30", "X-RateLimit-Remaining": "0", "X-RateLimit-Reset": unix(now.Add(10 * time.Minute)),
		}, 10 * time.Minute},
		{"secondary with quota left", 403, "You have exceeded a secondary rate limit", map[string]string{
			"X-RateLimit-Remaining": "4000", "X-RateLimit-Reset": unix(now.Add(time.Hour)),
		}, retry.DefaultRateLimitWait},
		// Only the message marks these as rate limits; the library's own
		// test misses the first, the old REST test the second.
		{"rate limit message with quota left", 403, "API rate limit exceeded for installation ID 42.", map[string]string{
			"X-RateLimit-Remaining": "4000", "X-RateLimit-Reset": unix(now.Add(time.Hour)),
		}, retry.DefaultRateLimitWait},
		{"secondary rate-limit message", 403, "You have exceeded a secondary rate-limit.", map[string]string{
			"X-RateLimit-Remaining": "4000",
		}, retry.DefaultRateLimitWait},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			respond := func(w http.ResponseWriter, body map[string]string) {
				for k, v := range tc.header {
					w.Header().Set(k, v)
				}
				writeJSON(w, tc.status, body)
			}

			lib := newTestEnv(t)
			lib.gh.actions = func(w http.ResponseWriter, r *http.Request) {
				respond(w, map[string]string{"typeName": "RateLimit", "message": tc.message})
			}
			libErr := lib.client.RemoveRunner(context.Background(), 9)

			rest := newRESTEnv(t)
			rest.clock.mu.Lock()
			rest.clock.now = now
			rest.clock.mu.Unlock()
			rest.f.set(func(f *fakeREST) {
				f.api = func(w http.ResponseWriter, r *http.Request) {
					respond(w, map[string]string{"message": tc.message})
				}
			})
			_, restErr := rest.rest.GetRun(context.Background(), testOwner, testRepo, 1)

			libWait, restWait := waitFor(t, now, libErr), waitFor(t, now, restErr)
			if libWait != tc.want || restWait != tc.want {
				t.Errorf("wait: library %v, REST %v, want %v (library err %v, REST err %v)",
					libWait, restWait, tc.want, libErr, restErr)
			}
		})
	}
}

// TestRateLimit403SameOnBothPaths sends the same 403, with quota left and
// no Retry-After, to the library path (RemoveRunner) and the REST path
// (GetRun), and checks that both classify it alike: only a message that
// names a rate limit makes it one.
func TestRateLimit403SameOnBothPaths(t *testing.T) {
	tests := []struct {
		message string
		want    retry.Class
	}{
		{"You have exceeded a secondary rate limit. Please wait a few minutes before you try again.", retry.RateLimited},
		{"You have exceeded a secondary rate-limit", retry.RateLimited},
		{"API rate limit exceeded for installation ID 42.", retry.RateLimited},
		{"Rate Limit exceeded", retry.RateLimited},
		{"Resource not accessible by integration", retry.Permanent},
		{"Must have admin rights to Repository.", retry.Permanent},
		{"secondary rate exceeded", retry.RateLimited}, // only the library's pattern
		{"", retry.Permanent},
	}
	for _, tc := range tests {
		t.Run(tc.message, func(t *testing.T) {
			ctx := context.Background()
			lib := newTestEnv(t)
			lib.gh.actions = func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("X-RateLimit-Remaining", "4000")
				writeException(w, http.StatusForbidden, "Forbidden", tc.message)
			}
			libErr := lib.client.RemoveRunner(ctx, 9)

			rest := newRESTEnv(t)
			rest.f.set(func(f *fakeREST) {
				f.api = func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("X-RateLimit-Remaining", "4000")
					writeJSON(w, http.StatusForbidden, map[string]string{"message": tc.message})
				}
			})
			_, restErr := rest.rest.GetRun(ctx, testOwner, testRepo, 1)

			libClass := retry.Classify(ctx, retry.OpRemoveRunner, libErr)
			restClass := retry.Classify(ctx, retry.OpGetRun, restErr)
			if libClass != tc.want || restClass != tc.want {
				t.Errorf("class: library %v, REST %v, want %v (library err %v, REST err %v)",
					libClass, restClass, tc.want, libErr, restErr)
			}
			for _, err := range []error{libErr, restErr} {
				var sc retry.StatusCoder
				if !errors.As(err, &sc) || sc.HTTPStatus() != http.StatusForbidden {
					t.Errorf("error %v does not carry status 403", err)
				}
			}
			if tc.want == retry.RateLimited {
				if libWait, restWait := waitFor(t, time.Now(), libErr), waitFor(t, time.Now(), restErr); libWait != retry.DefaultRateLimitWait || restWait != retry.DefaultRateLimitWait {
					t.Errorf("wait: library %v, REST %v, want %v", libWait, restWait, retry.DefaultRateLimitWait)
				}
			}
		})
	}
}

func TestIsRateLimitMessage(t *testing.T) {
	for msg, want := range map[string]bool{
		"You have exceeded a secondary rate limit.":       true,
		"You have exceeded a SECONDARY RATE LIMIT":        true,
		"secondary rate-limit":                            true,
		"API rate limit exceeded for user ID 1.":          true,
		"rate limited":                                    true,
		"Resource not accessible by integration":          false,
		"Bad credentials":                                 false,
		"Although you appear to have the correct scopes.": false,
		"secondary rated":                                 false, // \b after "rate" fails
		"ratelimit":                                       false,
		"":                                                false,
	} {
		if got := isRateLimitMessage(msg); got != want {
			t.Errorf("isRateLimitMessage(%q) = %v, want %v", msg, got, want)
		}
	}
}

func TestListRepoRunnersReadsAllPages(t *testing.T) {
	e := newRESTEnv(t)
	pages := map[string]string{
		"":  `{"total_count":5,"runners":[{"id":1,"name":"pool-a","busy":true},{"id":2,"name":"pool-b","busy":false}]}`,
		"2": `{"total_count":5,"runners":[{"id":3,"name":"pool-c","busy":false},{"id":4,"name":"other","busy":true}]}`,
		"3": `{"total_count":5,"runners":[{"id":5,"name":"pool-e","busy":true}]}`,
	}
	e.f.set(func(f *fakeREST) {
		f.api = func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/repos/test-org/test-repo/actions/runners" || r.URL.Query().Get("per_page") != "100" {
				t.Errorf("request %s", r.URL)
			}
			page := r.URL.Query().Get("page")
			next := map[string]string{"": "2", "2": "3"}[page]
			link := fmt.Sprintf(`<%s/repos/test-org/test-repo/actions/runners?per_page=100&page=1>; rel="first"`, f.srv.URL)
			if next != "" {
				link = fmt.Sprintf(`<%s/repos/test-org/test-repo/actions/runners?per_page=100&page=%s>; rel="next", `, f.srv.URL, next) + link
			}
			w.Header().Set("Link", link)
			_, _ = w.Write([]byte(pages[page]))
		}
	})
	got, err := e.rest.ListRepoRunners(context.Background(), testOwner, testRepo)
	if err != nil {
		t.Fatal(err)
	}
	want := []RepoRunner{
		{1, "pool-a", true}, {2, "pool-b", false}, {3, "pool-c", false}, {4, "other", true}, {5, "pool-e", true},
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("runners = %v, want %v", got, want)
	}
	if _, api := e.f.counts(); api != 3 {
		t.Errorf("API requests = %d, want 3", api)
	}
}

func TestListRepoRunnersRejectsBadResponses(t *testing.T) {
	tests := map[string]func(srvURL string, w http.ResponseWriter){
		"next link on another host": func(_ string, w http.ResponseWriter) {
			w.Header().Set("Link", `<https://elsewhere.example/repos/x/y/actions/runners?page=2>; rel="next"`)
			_, _ = w.Write([]byte(`{"total_count":1,"runners":[{"id":1,"name":"a","busy":false}]}`))
		},
		"runner without busy": func(_ string, w http.ResponseWriter) {
			_, _ = w.Write([]byte(`{"total_count":1,"runners":[{"id":1,"name":"a"}]}`))
		},
		"endless pages": func(srvURL string, w http.ResponseWriter) {
			w.Header().Set("Link", "<"+srvURL+"/repos/test-org/test-repo/actions/runners?page=2>; rel=\"next\"")
			_, _ = w.Write([]byte(`{"total_count":0,"runners":[]}`))
		},
	}
	for name, respond := range tests {
		t.Run(name, func(t *testing.T) {
			e := newRESTEnv(t)
			e.f.set(func(f *fakeREST) {
				f.api = func(w http.ResponseWriter, r *http.Request) { respond(f.srv.URL, w) }
			})
			_, err := e.rest.ListRepoRunners(context.Background(), testOwner, testRepo)
			if err == nil {
				t.Fatal("no error")
			}
			t.Logf("error: %v", err)
			e.assertClean(t, err.Error())
		})
	}
}

func TestTransportErrorHidesQuery(t *testing.T) {
	e := newRESTEnv(t)
	e.f.set(func(f *fakeREST) {
		f.api = func(w http.ResponseWriter, r *http.Request) {
			conn, _, err := http.NewResponseController(w).Hijack()
			if err == nil {
				_ = conn.Close()
			}
		}
	})
	ctx := context.Background()
	_, err := e.rest.ListRepoRunners(ctx, testOwner, testRepo)
	if err == nil {
		t.Fatal("no error")
	}
	t.Logf("error: %v", err)
	if c := retry.Classify(ctx, retry.OpListRepoRunners, err); c != retry.Transient {
		t.Errorf("Classify = %v, want transient", c)
	}
	e.assertClean(t, err.Error())
}

func TestNewRESTRejectsBadConfig(t *testing.T) {
	e := newRESTEnv(t)
	good := RESTConfig{
		BaseURL: DefaultAPIBaseURL, InstallationID: 42, Signer: e.signer,
		HTTPClient: http.DefaultClient, Clock: e.clock, Secrets: e.secrets,
	}
	if _, err := NewREST(good); err != nil {
		t.Fatalf("good config: %v", err)
	}
	for name, mutate := range map[string]func(*RESTConfig){
		"relative base":   func(c *RESTConfig) { c.BaseURL = "/api" },
		"base with query": func(c *RESTConfig) { c.BaseURL = "https://api.github.com/?x=1" },
		"no installation": func(c *RESTConfig) { c.InstallationID = 0 },
		"no signer":       func(c *RESTConfig) { c.Signer = nil },
		"no client":       func(c *RESTConfig) { c.HTTPClient = nil },
		"no clock":        func(c *RESTConfig) { c.Clock = nil },
		"no secrets":      func(c *RESTConfig) { c.Secrets = nil },
	} {
		cfg := good
		mutate(&cfg)
		if _, err := NewREST(cfg); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
}

func TestEndpointEscapesSegments(t *testing.T) {
	e := newRESTEnv(t)
	u := e.rest.endpoint("repos", "own er", "re/po")
	if got := u.EscapedPath(); got != "/repos/own%20er/re%2Fpo" {
		t.Errorf("path = %q", got)
	}
}

func TestLinkNext(t *testing.T) {
	for _, tc := range []struct {
		values []string
		want   string
	}{
		{[]string{`<https://api.example/r?page=2>; rel="next", <https://api.example/r?page=5>; rel="last"`}, "https://api.example/r?page=2"},
		{[]string{`<https://api.example/r?page=1>; rel="prev"`, `<https://api.example/r?page=3>; rel=next`}, "https://api.example/r?page=3"},
		{[]string{`<https://api.example/r?page=1>; rel="first"`}, ""},
		{[]string{`https://api.example/r?page=2; rel="next"`}, ""},
		{nil, ""},
	} {
		if got := linkNext(tc.values); got != tc.want {
			t.Errorf("linkNext(%q) = %q, want %q", tc.values, got, tc.want)
		}
	}
}

func TestParseRepoURL(t *testing.T) {
	owner, repo, err := ParseRepoURL("https://github.com/test-org/test-repo")
	if err != nil || owner != "test-org" || repo != "test-repo" {
		t.Errorf("ParseRepoURL = %q, %q, %v", owner, repo, err)
	}
	for _, bad := range []string{
		"http://github.com/a/b", "https://github.com/a", "https://github.com/a/b/c",
		"https://github.com/a/b?x=1", "github.com/a/b", "https://github.com//b",
	} {
		if _, _, err := ParseRepoURL(bad); err == nil {
			t.Errorf("ParseRepoURL(%q) accepted", bad)
		}
	}
}

const workflowRunsPath = "/repos/test-org/test-repo/actions/workflows/777/runs"

// createdFrom is 11:00:00 UTC, given in another zone with a fraction.
var createdFrom = time.Date(2026, 11, 24, 20, 0, 0, 500_000_000, time.FixedZone("KST", 9*60*60))

// workflowRunEntry is one workflow_runs entry of workflow 777 from the
// pool's main branch.
func workflowRunEntry(id, number int64) string {
	return fmt.Sprintf(`{"id":%d,"workflow_id":777,"run_number":%d,"head_branch":"main","head_repository":{"full_name":"test-org/test-repo"},"status":"completed"}`, id, number)
}

func workflowRunsPage(total int, entries ...string) string {
	return fmt.Sprintf(`{"total_count":%d,"workflow_runs":[%s]}`, total, strings.Join(entries, ","))
}

// pagedRuns answers page "" (the first), "2", "3", ... from pages, linking
// each page to the next one as GitHub does.
func pagedRuns(t *testing.T, f *fakeREST, pages []string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != workflowRunsPath {
			t.Errorf("request path %s", r.URL.Path)
		}
		n := 1
		if p := r.URL.Query().Get("page"); p != "" {
			n, _ = strconv.Atoi(p)
		}
		if n < 1 || n > len(pages) {
			t.Errorf("page %d requested", n)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if n < len(pages) {
			q := r.URL.Query()
			q.Set("page", strconv.Itoa(n+1))
			w.Header().Set("Link", fmt.Sprintf(`<%s%s?%s>; rel="next"`, f.srv.URL, workflowRunsPath, q.Encode()))
		}
		_, _ = w.Write([]byte(pages[n-1]))
	}
}

type visitCall struct {
	page  []gate.RunSummary
	total int
}

// recorder is a visit func that records its calls and returns stop.
type recorder struct {
	calls []visitCall
	stop  bool
}

func (rec *recorder) visit(page []gate.RunSummary, total int) bool {
	rec.calls = append(rec.calls, visitCall{page, total})
	return rec.stop
}

func TestListWorkflowRunsQueryAndDecode(t *testing.T) {
	e := newRESTEnv(t)
	body := `{"total_count":3,"workflow_runs":[` +
		workflowRunEntry(900, 42) + `,` +
		`{"id":901,"workflow_id":777,"run_number":43,"head_branch":"main","head_repository":{"full_name":"fork-user/test-repo"}},` +
		`{"id":902,"workflow_id":778,"run_number":44,"head_branch":null,"head_repository":null}]}`
	e.f.set(func(f *fakeREST) {
		f.api = func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(body)) }
	})
	var rec recorder
	if err := e.rest.ListWorkflowRuns(context.Background(), testOwner, testRepo, 777, createdFrom, rec.visit); err != nil {
		t.Fatal(err)
	}
	req := e.f.lastRequest()
	if req.Method != http.MethodGet || req.URL.Path != workflowRunsPath {
		t.Errorf("request = %s %s", req.Method, req.URL.Path)
	}
	const wantQuery = "branch=main&created=%3E%3D2026-11-24T11%3A00%3A00%2B00%3A00&exclude_pull_requests=true&per_page=100"
	if req.URL.RawQuery != wantQuery {
		t.Errorf("query = %q, want %q", req.URL.RawQuery, wantQuery)
	}
	if got := req.URL.Query().Get("created"); got != ">=2026-11-24T11:00:00+00:00" {
		t.Errorf("created = %q", got)
	}
	want := []visitCall{{page: []gate.RunSummary{
		{ID: 900, WorkflowID: 777, RunNumber: 42, HeadRepository: "test-org/test-repo", HeadBranch: "main"},
		{ID: 901, WorkflowID: 777, RunNumber: 43, HeadRepository: "fork-user/test-repo", HeadBranch: "main"},
		{ID: 902, WorkflowID: 778, RunNumber: 44},
	}, total: 3}}
	if fmt.Sprint(rec.calls) != fmt.Sprint(want) {
		t.Errorf("visit calls = %+v, want %+v", rec.calls, want)
	}
}

func TestListWorkflowRunsReadsAllPages(t *testing.T) {
	e := newRESTEnv(t)
	pages := []string{
		workflowRunsPage(5, workflowRunEntry(1, 1), workflowRunEntry(2, 2)),
		workflowRunsPage(5, workflowRunEntry(3, 3), workflowRunEntry(4, 4)),
		workflowRunsPage(5, workflowRunEntry(5, 5)),
	}
	e.f.set(func(f *fakeREST) { f.api = pagedRuns(t, f, pages) })
	var rec recorder
	if err := e.rest.ListWorkflowRuns(context.Background(), testOwner, testRepo, 777, createdFrom, rec.visit); err != nil {
		t.Fatal(err)
	}
	var ids []int64
	for _, c := range rec.calls {
		if c.total != 5 {
			t.Errorf("total = %d, want 5", c.total)
		}
		for _, s := range c.page {
			ids = append(ids, s.ID)
		}
	}
	if len(rec.calls) != 3 || fmt.Sprint(ids) != "[1 2 3 4 5]" {
		t.Errorf("visit calls = %d, ids %v; want 3 calls, ids 1..5", len(rec.calls), ids)
	}
	if _, api := e.f.counts(); api != 3 {
		t.Errorf("API requests = %d, want 3", api)
	}
}

func TestListWorkflowRunsStopsWhenVisitStops(t *testing.T) {
	e := newRESTEnv(t)
	pages := []string{workflowRunsPage(2, workflowRunEntry(1, 1)), workflowRunsPage(2, workflowRunEntry(2, 2))}
	e.f.set(func(f *fakeREST) { f.api = pagedRuns(t, f, pages) })
	rec := recorder{stop: true}
	if err := e.rest.ListWorkflowRuns(context.Background(), testOwner, testRepo, 777, createdFrom, rec.visit); err != nil {
		t.Fatal(err)
	}
	if len(rec.calls) != 1 {
		t.Errorf("visit calls = %d, want 1", len(rec.calls))
	}
	if _, api := e.f.counts(); api != 1 {
		t.Errorf("API requests = %d, want 1 (no request for the next page)", api)
	}
}

// The search returns at most 1,000 results, ten pages of 100: ten pages
// are read, an eleventh is an error.
func TestListWorkflowRunsPageLimit(t *testing.T) {
	for _, tc := range []struct {
		pages   int
		wantErr bool
		reads   int
	}{
		{pages: 10, reads: 10},
		{pages: 11, wantErr: true, reads: 10},
	} {
		t.Run(strconv.Itoa(tc.pages), func(t *testing.T) {
			e := newRESTEnv(t)
			pages := make([]string, tc.pages)
			for i := range pages {
				pages[i] = workflowRunsPage(tc.pages, workflowRunEntry(int64(i+1), int64(i+1)))
			}
			e.f.set(func(f *fakeREST) { f.api = pagedRuns(t, f, pages) })
			var rec recorder
			err := e.rest.ListWorkflowRuns(context.Background(), testOwner, testRepo, 777, createdFrom, rec.visit)
			if (err != nil) != tc.wantErr {
				t.Fatalf("error = %v, wantErr %v", err, tc.wantErr)
			}
			if _, api := e.f.counts(); api != tc.reads {
				t.Errorf("API requests = %d, want %d", api, tc.reads)
			}
			if err != nil {
				t.Logf("error: %v", err)
				assertOp(t, err, retry.OpListWorkflowRuns)
				e.assertClean(t, err.Error())
			}
		})
	}
}

func TestListWorkflowRunsRejectsBadResponses(t *testing.T) {
	var otherHits int
	var mu sync.Mutex
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		otherHits++
		mu.Unlock()
		_, _ = w.Write([]byte(workflowRunsPage(1, workflowRunEntry(1, 1))))
	}))
	t.Cleanup(other.Close)

	tests := map[string]func(srvURL string, w http.ResponseWriter){
		"next link on another host": func(_ string, w http.ResponseWriter) {
			w.Header().Set("Link", "<"+other.URL+workflowRunsPath+"?page=2>; rel=\"next\"")
			_, _ = w.Write([]byte(workflowRunsPage(2, workflowRunEntry(1, 1))))
		},
		"next link with another scheme": func(srvURL string, w http.ResponseWriter) {
			w.Header().Set("Link", "<"+strings.Replace(srvURL, "http://", "https://", 1)+workflowRunsPath+"?page=2>; rel=\"next\"")
			_, _ = w.Write([]byte(workflowRunsPage(2, workflowRunEntry(1, 1))))
		},
		"entry without id": func(_ string, w http.ResponseWriter) {
			_, _ = w.Write([]byte(workflowRunsPage(1, `{"workflow_id":777,"run_number":1,"head_branch":"main"}`)))
		},
		"entry with null id": func(_ string, w http.ResponseWriter) {
			_, _ = w.Write([]byte(workflowRunsPage(1, `{"id":null,"workflow_id":777,"run_number":1,"head_branch":"main"}`)))
		},
		"entry without workflow_id": func(_ string, w http.ResponseWriter) {
			_, _ = w.Write([]byte(workflowRunsPage(1, `{"id":1,"run_number":1,"head_branch":"main"}`)))
		},
		"entry with null workflow_id": func(_ string, w http.ResponseWriter) {
			_, _ = w.Write([]byte(workflowRunsPage(1, `{"id":1,"workflow_id":null,"run_number":1,"head_branch":"main"}`)))
		},
		"entry without run_number": func(_ string, w http.ResponseWriter) {
			_, _ = w.Write([]byte(workflowRunsPage(1, `{"id":1,"workflow_id":777,"head_branch":"main"}`)))
		},
		"without total_count": func(_ string, w http.ResponseWriter) {
			_, _ = w.Write([]byte(`{"workflow_runs":[` + workflowRunEntry(1, 1) + `]}`))
		},
		"null total_count": func(_ string, w http.ResponseWriter) {
			_, _ = w.Write([]byte(`{"total_count":null,"workflow_runs":[]}`))
		},
		"negative total_count": func(_ string, w http.ResponseWriter) {
			_, _ = w.Write([]byte(workflowRunsPage(-1)))
		},
		"without workflow_runs": func(_ string, w http.ResponseWriter) {
			_, _ = w.Write([]byte(`{"total_count":0}`))
		},
		"null workflow_runs": func(_ string, w http.ResponseWriter) {
			_, _ = w.Write([]byte(`{"total_count":0,"workflow_runs":null}`))
		},
		"invalid JSON": func(_ string, w http.ResponseWriter) {
			_, _ = w.Write([]byte(`{"total_count":1,"workflow_runs":[`))
		},
	}
	for name, respond := range tests {
		t.Run(name, func(t *testing.T) {
			e := newRESTEnv(t)
			e.f.set(func(f *fakeREST) {
				f.api = func(w http.ResponseWriter, r *http.Request) { respond(f.srv.URL, w) }
			})
			var rec recorder
			ctx := context.Background()
			err := e.rest.ListWorkflowRuns(ctx, testOwner, testRepo, 777, createdFrom, rec.visit)
			if err == nil {
				t.Fatal("no error")
			}
			t.Logf("error: %v", err)
			assertOp(t, err, retry.OpListWorkflowRuns)
			e.assertClean(t, err.Error())
			if strings.Contains(name, "link") {
				// The first page was counted before its link was refused.
				if len(rec.calls) != 1 {
					t.Errorf("visit calls = %d, want 1", len(rec.calls))
				}
			} else if len(rec.calls) != 0 {
				t.Errorf("visit called for a bad page: %+v", rec.calls)
			}
		})
	}
	mu.Lock()
	defer mu.Unlock()
	if otherHits != 0 {
		t.Errorf("requests to the other host = %d, want 0", otherHits)
	}
}

func TestListWorkflowRunsErrors(t *testing.T) {
	reset := time.Date(2026, 11, 24, 12, 30, 0, 0, time.UTC)
	tests := []struct {
		name       string
		token      bool // answer the installation token request instead
		respond    func(w http.ResponseWriter)
		wantClass  retry.Class
		retryAfter time.Duration
		reset      time.Time
		status     int
	}{
		{
			name: "429 with Retry-After",
			respond: func(w http.ResponseWriter) {
				w.Header().Set("Retry-After", "90")
				w.WriteHeader(http.StatusTooManyRequests)
			},
			wantClass: retry.RateLimited, retryAfter: 90 * time.Second,
		},
		{
			name: "403 primary limit",
			respond: func(w http.ResponseWriter) {
				w.Header().Set("X-RateLimit-Remaining", "0")
				w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(reset.Unix(), 10))
				writeJSON(w, http.StatusForbidden, map[string]string{"message": "API rate limit exceeded for installation ID 42."})
			},
			wantClass: retry.RateLimited, reset: reset,
		},
		{
			name: "403 secondary limit message",
			respond: func(w http.ResponseWriter) {
				w.Header().Set("X-RateLimit-Remaining", "4000")
				writeJSON(w, http.StatusForbidden, map[string]string{"message": "You have exceeded a secondary rate limit."})
			},
			wantClass: retry.RateLimited,
		},
		{
			name: "403 not a rate limit",
			respond: func(w http.ResponseWriter) {
				writeJSON(w, http.StatusForbidden, map[string]string{"message": "Resource not accessible by integration"})
			},
			wantClass: retry.Permanent, status: 403,
		},
		{
			name: "404",
			respond: func(w http.ResponseWriter) {
				writeJSON(w, http.StatusNotFound, map[string]string{"message": "Not Found"})
			},
			wantClass: retry.Permanent, status: 404,
		},
		{
			name:      "502",
			respond:   func(w http.ResponseWriter) { w.WriteHeader(http.StatusBadGateway) },
			wantClass: retry.Transient, status: 502,
		},
		{
			name:  "installation token rate limited",
			token: true,
			respond: func(w http.ResponseWriter) {
				w.Header().Set("Retry-After", "120")
				w.WriteHeader(http.StatusTooManyRequests)
			},
			wantClass: retry.RateLimited, retryAfter: 2 * time.Minute,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			e := newRESTEnv(t)
			e.f.set(func(f *fakeREST) {
				f.api = func(w http.ResponseWriter, r *http.Request) { tc.respond(w) }
				if tc.token {
					f.tokenReply = func(w http.ResponseWriter, r *http.Request) { tc.respond(w) }
					f.api = func(w http.ResponseWriter, r *http.Request) {}
				}
			})
			ctx := context.Background()
			var rec recorder
			err := e.rest.ListWorkflowRuns(ctx, testOwner, testRepo, 777, createdFrom, rec.visit)
			if err == nil {
				t.Fatal("ListWorkflowRuns succeeded")
			}
			t.Logf("error: %v", err)
			assertOp(t, err, retry.OpListWorkflowRuns)
			if c := retry.Classify(ctx, retry.OpListWorkflowRuns, err); c != tc.wantClass {
				t.Errorf("Classify = %v, want %v", c, tc.wantClass)
			}
			var rl *retry.RateLimitError
			if errors.As(err, &rl) {
				if rl.RetryAfter != tc.retryAfter || !rl.Reset.Equal(tc.reset) {
					t.Errorf("RateLimitError = {RetryAfter %v, Reset %v}, want {%v, %v}", rl.RetryAfter, rl.Reset, tc.retryAfter, tc.reset)
				}
			} else if tc.wantClass == retry.RateLimited {
				t.Error("no RateLimitError")
			}
			var se *StatusError
			gotStatus := 0
			if errors.As(err, &se) && rl == nil {
				gotStatus = se.Code
			}
			if gotStatus != tc.status {
				t.Errorf("StatusError code = %d, want %d", gotStatus, tc.status)
			}
			// No retry: one API request, none when the token failed.
			wantCalls := 1
			if tc.token {
				wantCalls = 0
			}
			if _, api := e.f.counts(); api != wantCalls {
				t.Errorf("API requests = %d, want %d", api, wantCalls)
			}
			if len(rec.calls) != 0 {
				t.Errorf("visit calls = %d, want 0", len(rec.calls))
			}
			e.assertClean(t, err.Error())
		})
	}
}

// One 30-second deadline covers the installation token and every page.
func TestListWorkflowRunsHasThirtySecondDeadline(t *testing.T) {
	e := newRESTEnv(t)
	pages := []string{workflowRunsPage(2, workflowRunEntry(1, 1)), workflowRunsPage(2, workflowRunEntry(2, 2))}
	e.f.set(func(f *fakeREST) { f.api = pagedRuns(t, f, pages) })
	doer := &deadlineDoer{next: e.f.srv.Client()}
	rest, err := NewREST(RESTConfig{
		BaseURL: e.f.srv.URL, InstallationID: 42, Signer: e.signer,
		HTTPClient: doer, Clock: e.clock, Secrets: e.secrets,
	})
	if err != nil {
		t.Fatal(err)
	}
	var rec recorder
	if err := rest.ListWorkflowRuns(context.Background(), testOwner, testRepo, 777, createdFrom, rec.visit); err != nil {
		t.Fatal(err)
	}
	if len(doer.instants) != 3 {
		t.Fatalf("requests with deadline = %d, want 3 (token and two pages)", len(doer.instants))
	}
	for i, at := range doer.instants {
		if !at.Equal(doer.instants[0]) {
			t.Errorf("request %d deadline %v differs from the first %v", i, at, doer.instants[0])
		}
	}
	for _, d := range doer.deadlines {
		if d <= 25*time.Second || d > 30*time.Second {
			t.Errorf("deadline in %v, want about 30s", d)
		}
	}
}

func TestListWorkflowRunsRejectsBadArguments(t *testing.T) {
	visit := func([]gate.RunSummary, int) bool { return false }
	for name, call := range map[string]func(r *REST) error{
		"zero workflow ID": func(r *REST) error {
			return r.ListWorkflowRuns(context.Background(), testOwner, testRepo, 0, createdFrom, visit)
		},
		"negative workflow ID": func(r *REST) error {
			return r.ListWorkflowRuns(context.Background(), testOwner, testRepo, -1, createdFrom, visit)
		},
		"zero created-from": func(r *REST) error {
			return r.ListWorkflowRuns(context.Background(), testOwner, testRepo, 777, time.Time{}, visit)
		},
		"nil visit": func(r *REST) error {
			return r.ListWorkflowRuns(context.Background(), testOwner, testRepo, 777, createdFrom, nil)
		},
	} {
		t.Run(name, func(t *testing.T) {
			e := newRESTEnv(t)
			err := call(e.rest)
			if err == nil {
				t.Fatal("no error")
			}
			assertOp(t, err, retry.OpListWorkflowRuns)
			if tokens, api := e.f.counts(); tokens != 0 || api != 0 {
				t.Errorf("requests: token %d, API %d; want none", tokens, api)
			}
		})
	}
}

// gate.LatestScan.Add plugs in as the visitor; the scan stops the walk at
// the first newer main run and skips the fork's newer main run.
func TestListWorkflowRunsWithLatestScan(t *testing.T) {
	run := gate.Run{
		ID: 500, WorkflowID: 777, RunNumber: 50, CreatedAt: createdFrom,
		HeadRepository: "test-org/test-repo", HeadBranch: "main",
	}
	policy := gate.Policy{Owner: testOwner, Repository: testRepo}
	fork := `{"id":900,"workflow_id":777,"run_number":90,"head_branch":"main","head_repository":{"full_name":"fork-user/test-repo"}}`
	otherWorkflow := `{"id":450,"workflow_id":778,"run_number":45,"head_branch":"main","head_repository":{"full_name":"test-org/test-repo"}}`
	tests := []struct {
		name     string
		pages    []string
		requests int
		newer    int64 // ID of the newer run found, 0 for none
		other    int64 // ID of the other workflow's entry found, 0 for none
	}{
		{"fork newer run not counted", []string{
			workflowRunsPage(3, workflowRunEntry(400, 40), fork),
			workflowRunsPage(3, workflowRunEntry(500, 50)),
		}, 2, 0, 0},
		{"newer run stops the walk", []string{
			workflowRunsPage(3, workflowRunEntry(500, 50), workflowRunEntry(501, 51)),
			workflowRunsPage(3, workflowRunEntry(400, 40)),
		}, 1, 501, 0},
		{"other workflow's entry stops the walk", []string{
			workflowRunsPage(3, workflowRunEntry(500, 50), otherWorkflow),
			workflowRunsPage(3, workflowRunEntry(400, 40)),
		}, 1, 0, 450},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			e := newRESTEnv(t)
			e.f.set(func(f *fakeREST) { f.api = pagedRuns(t, f, tc.pages) })
			scan := gate.NewLatestScan(run, policy)
			if err := e.rest.ListWorkflowRuns(context.Background(), testOwner, testRepo, run.WorkflowID, run.CreatedAt.Add(-time.Hour), scan.Add); err != nil {
				t.Fatal(err)
			}
			got := scan.Result()
			if _, api := e.f.counts(); api != tc.requests {
				t.Errorf("API requests = %d, want %d", api, tc.requests)
			}
			switch {
			case tc.newer == 0 && got.Newer != nil:
				t.Errorf("Newer = %+v, want nil", *got.Newer)
			case tc.newer != 0 && (got.Newer == nil || got.Newer.ID != tc.newer):
				t.Errorf("Newer = %+v, want ID %d", got.Newer, tc.newer)
			}
			switch {
			case tc.other == 0 && got.OtherWorkflow != nil:
				t.Errorf("OtherWorkflow = %+v, want nil", *got.OtherWorkflow)
			case tc.other != 0 && (got.OtherWorkflow == nil || got.OtherWorkflow.ID != tc.other || got.OtherWorkflow.WorkflowID != 778):
				t.Errorf("OtherWorkflow = %+v, want ID %d of workflow 778", got.OtherWorkflow, tc.other)
			}
			if tc.newer == 0 && tc.other == 0 && (!got.Checked || !got.SelfSeen || got.Read != 3 || got.TotalCount != 3) {
				t.Errorf("Result() = %+v", got)
			}
		})
	}
}

func assertOp(t *testing.T, err error, want retry.Op) {
	t.Helper()
	var op *retry.OpError
	if !errors.As(err, &op) || op.Op != want {
		t.Errorf("error %v does not name %q", err, want)
	}
}

// Ten full pages whose total_count is 1,000 may be cut at the search cap:
// the walk reads them all and the result is denied.
func TestListWorkflowRunsThousandResultsDenied(t *testing.T) {
	run := gate.Run{
		ID: 1000, WorkflowID: 777, RunNumber: 1000, CreatedAt: createdFrom,
		HeadRepository: "test-org/test-repo", HeadBranch: "main",
		Event: "push", Path: ".github/workflows/deploy.yml", ReferencedWorkflows: gate.ReferencedWorkflowsEmpty,
	}
	policy := gate.Policy{Owner: testOwner, Repository: testRepo}
	pages := make([]string, 10)
	for p := range pages {
		entries := make([]string, 100)
		for i := range entries {
			n := int64(p*100 + i + 1)
			entries[i] = workflowRunEntry(n, n)
		}
		pages[p] = workflowRunsPage(1000, entries...)
	}
	e := newRESTEnv(t)
	e.f.set(func(f *fakeREST) { f.api = pagedRuns(t, f, pages) })
	scan := gate.NewLatestScan(run, policy)
	if err := e.rest.ListWorkflowRuns(context.Background(), testOwner, testRepo, run.WorkflowID, run.CreatedAt.Add(-time.Hour), scan.Add); err != nil {
		t.Fatal(err)
	}
	if _, api := e.f.counts(); api != 10 {
		t.Errorf("API requests = %d, want 10", api)
	}
	got := scan.Result()
	if got.Newer != nil || got.OtherWorkflow != nil || !got.SelfSeen || got.Read != 1000 || got.TotalCount != 1000 {
		t.Fatalf("Result() = %+v, want SelfSeen, Read 1000, TotalCount 1000, nothing newer", got)
	}
	d := gate.Evaluate(gate.Job{}, run, got, policy)
	if !slices.ContainsFunc(d.Violations, func(v gate.Violation) bool { return v.Condition == gate.LatestRun }) {
		t.Errorf("violations %+v do not include %v", d.Violations, gate.LatestRun)
	}
}

// A dropped connection is a transient error without the query string.
func TestListWorkflowRunsTransportError(t *testing.T) {
	e := newRESTEnv(t)
	e.f.set(func(f *fakeREST) {
		f.api = func(w http.ResponseWriter, r *http.Request) {
			conn, _, err := http.NewResponseController(w).Hijack()
			if err == nil {
				_ = conn.Close()
			}
		}
	})
	ctx := context.Background()
	var rec recorder
	err := e.rest.ListWorkflowRuns(ctx, testOwner, testRepo, 777, createdFrom, rec.visit)
	if err == nil {
		t.Fatal("no error")
	}
	t.Logf("error: %v", err)
	assertOp(t, err, retry.OpListWorkflowRuns)
	if c := retry.Classify(ctx, retry.OpListWorkflowRuns, err); c != retry.Transient {
		t.Errorf("Classify = %v, want transient", c)
	}
	// The request count is not checked: net/http itself may resend a GET
	// whose reused connection closed before any response.
	if len(rec.calls) != 0 {
		t.Errorf("visit calls = %d, want 0", len(rec.calls))
	}
	e.assertClean(t, err.Error())
}

// shortDeadlineDoer cuts the deadline of API requests (not the token
// request) to d, standing in for the lookup's 30 seconds running out while
// the caller's context lives on. It counts the API requests the client
// sends in apiCalls, which, unlike the server's count, does not depend on
// the request reaching the server before the deadline.
type shortDeadlineDoer struct {
	next     HTTPDoer
	d        time.Duration
	apiCalls *atomic.Int64
}

func (s shortDeadlineDoer) Do(req *http.Request) (*http.Response, error) {
	if !strings.HasPrefix(req.URL.Path, "/repos/") {
		return s.next.Do(req)
	}
	s.apiCalls.Add(1)
	ctx, cancel := context.WithTimeout(req.Context(), s.d)
	defer cancel()
	resp, err := s.next.Do(req.WithContext(ctx))
	if err != nil {
		return nil, err
	}
	// The body is read after Do returns, when ctx is cancelled; read it
	// now, within the cut deadline.
	body, readErr := io.ReadAll(resp.Body)
	resp.Body.Close()
	if readErr != nil {
		return nil, readErr
	}
	resp.Body = io.NopCloser(bytes.NewReader(body))
	return resp, nil
}

// A lookup whose deadline ends is a transient error without the query
// string; the caller's live context does not make it Canceled.
func TestListWorkflowRunsDeadlineExpiry(t *testing.T) {
	e := newRESTEnv(t)
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	e.f.set(func(f *fakeREST) {
		f.api = func(w http.ResponseWriter, r *http.Request) {
			select {
			case <-r.Context().Done():
			case <-release:
			}
		}
	})
	var apiCalls atomic.Int64
	rest, err := NewREST(RESTConfig{
		BaseURL: e.f.srv.URL, InstallationID: 42, Signer: e.signer,
		HTTPClient: shortDeadlineDoer{next: e.f.srv.Client(), d: 100 * time.Millisecond, apiCalls: &apiCalls},
		Clock:      e.clock, Secrets: e.secrets,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	var rec recorder
	err = rest.ListWorkflowRuns(ctx, testOwner, testRepo, 777, createdFrom, rec.visit)
	if err == nil {
		t.Fatal("no error")
	}
	t.Logf("error: %v", err)
	assertOp(t, err, retry.OpListWorkflowRuns)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("error %v does not hold the deadline", err)
	}
	if c := retry.Classify(ctx, retry.OpListWorkflowRuns, err); c != retry.Transient {
		t.Errorf("Classify = %v, want transient", c)
	}
	if len(rec.calls) != 0 {
		t.Errorf("visit calls = %d, want 0", len(rec.calls))
	}
	// The client counts every API request it sends, so exactly one means
	// the lookup was not retried. Under load the 100 ms deadline can end
	// before the server records the request, so the server's count is only
	// bounded.
	if n := apiCalls.Load(); n != 1 {
		t.Errorf("API requests sent = %d, want 1 (no retry)", n)
	}
	if _, api := e.f.counts(); api > 1 {
		t.Errorf("API requests received = %d, want at most 1 (no retry)", api)
	}
	e.assertClean(t, err.Error())
}
