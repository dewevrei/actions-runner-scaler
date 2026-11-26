package github

import (
	"context"
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

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

// fakeREST is a GitHub REST server for the installation token and the
// /repos/ and /orgs/ endpoints.
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
	api         http.HandlerFunc // answers /repos/... and /orgs/...
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
	apiHandler := func(w http.ResponseWriter, r *http.Request) {
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
	}
	mux.HandleFunc("/repos/", apiHandler)
	mux.HandleFunc("/orgs/", apiHandler)
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

// runnerJSON is a runner list page holding pool-a.
const runnerJSON = `{"total_count":1,"runners":[{"id":1,"name":"pool-a","busy":false}]}`

// findRunner is the REST call the tests of shared REST behavior make
// (token cache, errors, rate limits).
func findRunner(ctx context.Context, r *REST) error {
	_, _, err := r.FindRunner(ctx, testTarget, "pool-a")
	return err
}

func TestInstallationTokenCachedUntilFiveMinutesBeforeExpiry(t *testing.T) {
	e := newRESTEnv(t)
	e.f.set(func(f *fakeREST) {
		f.api = func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(runnerJSON))
		}
	})
	ctx := context.Background()
	get := func() {
		t.Helper()
		if err := findRunner(ctx, e.rest); err != nil {
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
			_, _ = w.Write([]byte(runnerJSON))
		}
	})
	ctx := context.Background()
	err := findRunner(ctx, e.rest)
	if c := retry.Classify(ctx, retry.OpFindOrgRunner, err); c != retry.Permanent {
		t.Errorf("Classify = %v, want permanent", c)
	}
	if err := findRunner(ctx, e.rest); err != nil {
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
			err := findRunner(ctx, e.rest)
			if err == nil {
				t.Fatal("FindRunner succeeded")
			}
			t.Logf("error: %v", err)
			if c := retry.Classify(ctx, retry.OpFindOrgRunner, err); c != tc.wantClass {
				t.Errorf("Classify = %v, want %v", c, tc.wantClass)
			}
			var op *retry.OpError
			if !errors.As(err, &op) || op.Op != retry.OpFindOrgRunner {
				t.Errorf("error %v does not name %q", err, retry.OpFindOrgRunner)
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
	err = findRunner(ctx, e.rest)
	if err == nil || strings.Contains(err.Error(), "ghs_partialSecretValue123") {
		t.Errorf("error = %v", err)
	}
	if c := retry.Classify(ctx, retry.OpFindOrgRunner, err); c != retry.Transient {
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
// the library path (RemoveRunner) and the REST path (FindRunner) and checks
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
			restErr := findRunner(context.Background(), rest.rest)

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
// (FindRunner), and checks that both classify it alike: only a message that
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
			restErr := findRunner(ctx, rest.rest)

			libClass := retry.Classify(ctx, retry.OpRemoveRunner, libErr)
			restClass := retry.Classify(ctx, retry.OpFindOrgRunner, restErr)
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

func assertOp(t *testing.T, err error, want retry.Op) {
	t.Helper()
	var op *retry.OpError
	if !errors.As(err, &op) || op.Op != want {
		t.Errorf("error %v does not name %q", err, want)
	}
}

const orgRunnersPath = "/orgs/test-org/actions/runners"

// pagedOrgRunners answers page "" (the first), "2", ... from pages and links
// each page to the next. It records the queries it saw.
func pagedOrgRunners(t *testing.T, f *fakeREST, pages []string, queries *[]string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != orgRunnersPath {
			t.Errorf("request path %s", r.URL.Path)
		}
		*queries = append(*queries, r.URL.RawQuery)
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
			w.Header().Set("Link", fmt.Sprintf(`<%s%s?%s>; rel="next"`, f.srv.URL, orgRunnersPath, q.Encode()))
		}
		_, _ = w.Write([]byte(pages[n-1]))
	}
}

// TestListRunnersAllPages proves 0002 SPEC R148.
func TestListRunnersAllPages(t *testing.T) {
	e := newRESTEnv(t)
	pages := []string{
		`{"total_count":5,"runners":[{"id":1,"name":"pool-a","busy":true},{"id":2,"name":"pool-b","busy":false}]}`,
		`{"total_count":5,"runners":[{"id":3,"name":"pool-c","busy":false},{"id":4,"name":"other","busy":true}]}`,
		`{"total_count":5,"runners":[{"id":5,"name":"pool-e","busy":true}]}`,
	}
	var queries []string
	e.f.set(func(f *fakeREST) { f.api = pagedOrgRunners(t, f, pages, &queries) })
	got, err := e.rest.ListRunners(context.Background(), testTarget)
	if err != nil {
		t.Fatal(err)
	}
	want := []Runner{
		{ID: 1, Name: "pool-a", Busy: true}, {ID: 2, Name: "pool-b"}, {ID: 3, Name: "pool-c"},
		{ID: 4, Name: "other", Busy: true}, {ID: 5, Name: "pool-e", Busy: true},
	}
	if !slices.Equal(got, want) {
		t.Errorf("runners = %v, want %v", got, want)
	}
	if len(queries) != 3 || queries[0] != "per_page=100" {
		t.Errorf("queries = %q, want 3 requests, the first per_page=100", queries)
	}
}

func TestListRunnersEmpty(t *testing.T) {
	e := newRESTEnv(t)
	var queries []string
	e.f.set(func(f *fakeREST) { f.api = pagedOrgRunners(t, f, []string{`{"total_count":0,"runners":[]}`}, &queries) })
	got, err := e.rest.ListRunners(context.Background(), testTarget)
	if err != nil || len(got) != 0 {
		t.Errorf("ListRunners = %v, %v, want none", got, err)
	}
}

func TestListRunnersRejectsBadResponses(t *testing.T) {
	tests := map[string]func(srvURL string, w http.ResponseWriter){
		"next link on another host": func(_ string, w http.ResponseWriter) {
			w.Header().Set("Link", `<https://elsewhere.example/orgs/x/actions/runners?page=2>; rel="next"`)
			_, _ = w.Write([]byte(`{"total_count":1,"runners":[{"id":1,"name":"a","busy":false}]}`))
		},
		"runner without busy": func(_ string, w http.ResponseWriter) {
			_, _ = w.Write([]byte(`{"total_count":1,"runners":[{"id":1,"name":"a"}]}`))
		},
		"runner without name": func(_ string, w http.ResponseWriter) {
			_, _ = w.Write([]byte(`{"total_count":1,"runners":[{"id":1,"busy":true}]}`))
		},
		"not json": func(_ string, w http.ResponseWriter) { _, _ = w.Write([]byte(`oops`)) },
		"endless pages": func(srvURL string, w http.ResponseWriter) {
			w.Header().Set("Link", "<"+srvURL+orgRunnersPath+"?page=2>; rel=\"next\"")
			_, _ = w.Write([]byte(`{"total_count":0,"runners":[]}`))
		},
	}
	for name, respond := range tests {
		t.Run(name, func(t *testing.T) {
			e := newRESTEnv(t)
			e.f.set(func(f *fakeREST) {
				f.api = func(w http.ResponseWriter, r *http.Request) { respond(f.srv.URL, w) }
			})
			_, err := e.rest.ListRunners(context.Background(), testTarget)
			if err == nil {
				t.Fatal("no error")
			}
			assertOp(t, err, retry.OpListOrgRunners)
			e.assertClean(t, err.Error())
		})
	}
}

func TestListRunnersRejectsNonOrganizationTarget(t *testing.T) {
	e := newRESTEnv(t)
	_, err := e.rest.ListRunners(context.Background(), Target{})
	if err == nil {
		t.Fatal("no error")
	}
	assertOp(t, err, retry.OpListOrgRunners)
	if _, api := e.f.counts(); api != 0 {
		t.Errorf("API requests = %d, want 0", api)
	}
}

// TestFindRunnerExactName proves 0002 SPEC R149.
func TestFindRunnerExactName(t *testing.T) {
	e := newRESTEnv(t)
	pages := []string{
		`{"total_count":3,"runners":[{"id":1,"name":"pool-a-extra","busy":true},{"id":2,"name":"POOL-A","busy":false}]}`,
		`{"total_count":3,"runners":[{"id":3,"name":"pool-a","busy":true}]}`,
	}
	var queries []string
	e.f.set(func(f *fakeREST) { f.api = pagedOrgRunners(t, f, pages, &queries) })
	ctx := context.Background()

	got, ok, err := e.rest.FindRunner(ctx, testTarget, "pool-a")
	if err != nil || !ok {
		t.Fatalf("FindRunner = %+v, %v, %v", got, ok, err)
	}
	if got != (Runner{ID: 3, Name: "pool-a", Busy: true}) {
		t.Errorf("runner = %+v", got)
	}
	if len(queries) != 2 || queries[0] != "name=pool-a&per_page=100" {
		t.Errorf("queries = %q, want the name and per_page=100 on every page", queries)
	}

	// Only entries with other names: not found.
	queries = nil
	got, ok, err = e.rest.FindRunner(ctx, testTarget, "pool-b")
	if err != nil || ok || got != (Runner{}) {
		t.Errorf("FindRunner(pool-b) = %+v, %v, %v, want zero, false, nil", got, ok, err)
	}
}

func TestFindRunnerNameIsEscaped(t *testing.T) {
	e := newRESTEnv(t)
	var queries []string
	e.f.set(func(f *fakeREST) {
		f.api = pagedOrgRunners(t, f, []string{`{"total_count":0,"runners":[]}`}, &queries)
	})
	if _, _, err := e.rest.FindRunner(context.Background(), testTarget, "a&b=c d"); err != nil {
		t.Fatal(err)
	}
	if len(queries) != 1 || queries[0] != "name=a%26b%3Dc+d&per_page=100" {
		t.Errorf("queries = %q", queries)
	}
}

func TestFindRunnerErrors(t *testing.T) {
	ctx := context.Background()
	e := newRESTEnv(t)
	if _, _, err := e.rest.FindRunner(ctx, testTarget, ""); err == nil {
		t.Error("empty name: no error")
	}
	if _, _, err := e.rest.FindRunner(ctx, Target{}, "x"); err == nil {
		t.Error("zero target: no error")
	}
	e.f.set(func(f *fakeREST) {
		f.api = func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusInternalServerError) }
	})
	_, ok, err := e.rest.FindRunner(ctx, testTarget, "x")
	if err == nil || ok {
		t.Fatalf("FindRunner = %v, %v, want an error", ok, err)
	}
	assertOp(t, err, retry.OpFindOrgRunner)
	if c := retry.Classify(ctx, retry.OpFindOrgRunner, err); c != retry.Transient {
		t.Errorf("Classify = %v, want transient", c)
	}
	e.f.set(func(f *fakeREST) {
		f.api = func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNotFound) }
	})
	_, _, err = e.rest.FindRunner(ctx, testTarget, "x")
	if c := retry.Classify(ctx, retry.OpFindOrgRunner, err); c != retry.Permanent {
		t.Errorf("404: Classify = %v, want permanent", c)
	}
}
