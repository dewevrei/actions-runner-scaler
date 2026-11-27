package github

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/actions/scaleset"

	"github.com/dewevrei/actions-runner-scaler/internal/logging"
	"github.com/dewevrei/actions-runner-scaler/internal/retry"
)

// Fake secret values. Tests check that none of them shows up in error texts
// or logs.
const (
	fakeInstallationToken = "ghs_fakeInstallationToken0123456789"
	fakeRegistrationToken = "fake-registration-token-0123456789"
	fakeQueueToken        = "fake-queue-token-0123456789"
	fakeQueueToken2       = "fake-queue-token-refreshed-0123456789"
	fakeJIT               = "ZmFrZS1qaXQtY29uZmlnLXNlY3JldC0wMTIzNDU2Nzg5"
)

const (
	testOwner      = "acme"
	testRepo       = "widget"
	testScaleSetID = 7
	testSessionID  = "11111111-2222-3333-4444-555555555555"
)

var (
	keyOnce sync.Once
	testKey *rsa.PrivateKey
)

// rsaKey returns a test key generated once per test binary.
func rsaKey(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	keyOnce.Do(func() {
		k, err := rsa.GenerateKey(rand.Reader, 2048)
		if err != nil {
			panic(err)
		}
		testKey = k
	})
	return testKey
}

// fakeAdminToken is an unsigned JWT whose exp the library reads to decide
// when to refresh. It expires after ttl.
func fakeAdminToken(ttl time.Duration) string {
	enc := base64.RawURLEncoding.EncodeToString
	header := enc([]byte(`{"alg":"RS256","typ":"JWT"}`))
	claims := enc(fmt.Appendf(nil, `{"exp":%d}`, time.Now().Add(ttl).Unix()))
	return header + "." + claims + "." + enc([]byte("sig"))
}

// fakeGitHub serves the token exchange the library performs and passes the
// Actions service calls under /svc/ and /queue to actions.
type fakeGitHub struct {
	t   *testing.T
	srv *httptest.Server

	mu       sync.Mutex
	requests []string // "METHOD path" in order
	jwts     []string // App JWTs presented for installation tokens

	// bearer is the token the registration token request must carry:
	// the installation token, or a token target's personal access token.
	bearer string

	// installation, when set, answers the installation token request.
	installation http.HandlerFunc
	// registrationToken, when set, answers the registration token request.
	registrationToken http.HandlerFunc
	// registration, when set, answers the admin connection request.
	registration http.HandlerFunc
	// shortAdminToken makes the admin token expire within the library's
	// one-minute refresh margin, so every Actions service call renews it.
	shortAdminToken bool
	// actions answers every request under /svc/ and /queue.
	actions http.HandlerFunc
}

// handler returns the field h points to, read under the lock.
func (f *fakeGitHub) handler(h *http.HandlerFunc) http.HandlerFunc {
	f.mu.Lock()
	defer f.mu.Unlock()
	return *h
}

func newFakeGitHub(t *testing.T) *fakeGitHub {
	t.Helper()
	f := &fakeGitHub{t: t, bearer: fakeInstallationToken}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v3/app/installations/{id}/access_tokens", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.jwts = append(f.jwts, strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
		f.mu.Unlock()
		if h := f.handler(&f.installation); h != nil {
			h(w, r)
			return
		}
		writeJSON(w, http.StatusCreated, map[string]any{
			"token":      fakeInstallationToken,
			"expires_at": time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
		})
	})
	mux.HandleFunc("POST /api/v3/repos/{owner}/{repo}/actions/runners/registration-token", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		bearer := f.bearer
		f.mu.Unlock()
		if r.Header.Get("Authorization") != "Bearer "+bearer {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if h := f.handler(&f.registrationToken); h != nil {
			h(w, r)
			return
		}
		writeJSON(w, http.StatusCreated, map[string]any{
			"token":      fakeRegistrationToken,
			"expires_at": time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
		})
	})
	mux.HandleFunc("POST /api/v3/actions/runner-registration", func(w http.ResponseWriter, r *http.Request) {
		if h := f.handler(&f.registration); h != nil {
			h(w, r)
			return
		}
		f.mu.Lock()
		ttl := time.Hour
		if f.shortAdminToken {
			ttl = 30 * time.Second
		}
		f.mu.Unlock()
		writeJSON(w, http.StatusOK, map[string]any{"url": f.srv.URL + "/svc/", "token": fakeAdminToken(ttl)})
	})
	actions := func(w http.ResponseWriter, r *http.Request) {
		if f.actions == nil {
			http.NotFound(w, r)
			return
		}
		f.actions(w, r)
	}
	mux.HandleFunc("/svc/", actions)
	mux.HandleFunc("/queue", actions)
	mux.HandleFunc("/queue/", actions)

	f.srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.requests = append(f.requests, r.Method+" "+r.URL.Path)
		f.mu.Unlock()
		mux.ServeHTTP(w, r)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

// count returns how many requests matched method and a path suffix.
func (f *fakeGitHub) count(method, pathSuffix string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, r := range f.requests {
		m, p, _ := strings.Cut(r, " ")
		if m == method && strings.HasSuffix(p, pathSuffix) {
			n++
		}
	}
	return n
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// writeException writes an Actions service error body.
func writeException(w http.ResponseWriter, status int, typeName, message string) {
	writeJSON(w, status, map[string]string{"typeName": typeName, "message": message})
}

// testEnv is a ScaleSetClient against a fakeGitHub with its logs captured.
type testEnv struct {
	gh      *fakeGitHub
	client  *ScaleSetClient
	secrets *logging.Secrets
	logs    *syncBuffer
}

type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// fastRetry keeps the library's policy but with millisecond waits.
func fastRetry() scaleset.RetryConfig {
	cfg := RetryConfig()
	cfg.WaitMin = time.Millisecond
	cfg.WaitMax = 5 * time.Millisecond
	return cfg
}

func newTestEnv(t *testing.T) *testEnv {
	t.Helper()
	return newTestEnvTransport(t, nil)
}

// newTestEnvTransport is newTestEnv with the base transport from
// transport, which gets the fake server's own client transport; nil uses
// that transport as it is.
func newTestEnvTransport(t *testing.T, transport func(base *http.Transport) http.RoundTripper) *testEnv {
	t.Helper()
	return newTestEnvWith(t, transport, "")
}

// newTokenTestEnv is newTestEnv for a token target: the client holds token
// instead of the App signer, and the fake server's registration token
// request accepts only that token.
func newTokenTestEnv(t *testing.T, token string) *testEnv {
	t.Helper()
	return newTestEnvWith(t, nil, token)
}

func newTestEnvWith(t *testing.T, transport func(base *http.Transport) http.RoundTripper, token string) *testEnv {
	t.Helper()
	gh := newFakeGitHub(t)
	var tr http.RoundTripper = gh.srv.Client().Transport
	if transport != nil {
		tr = transport(gh.srv.Client().Transport.(*http.Transport))
	}
	secrets := logging.NewSecrets()
	logs := &syncBuffer{}
	logger := slog.New(logging.NewRedactingHandler(
		slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}), secrets))
	cfg := ScaleSetConfig{
		ConfigURL: gh.srv.URL + "/" + testOwner + "/" + testRepo,
		Transport: tr,
		Logger:    logger,
		Secrets:   secrets,
	}
	if token != "" {
		gh.bearer = token
		cfg.Token = token
	} else {
		signer, err := NewAppSigner("Iv1.testclient", rsaKey(t), retry.SystemClock(), secrets)
		if err != nil {
			t.Fatal(err)
		}
		cfg.Signer, cfg.InstallationID = signer, 42
	}
	client, err := newScaleSetClient(cfg, fastRetry())
	if err != nil {
		t.Fatal(err)
	}
	return &testEnv{gh: gh, client: client, secrets: secrets, logs: logs}
}

// assertNoSecrets fails when text holds any fake secret, a JWT made by the
// signer, or a URL query.
func (e *testEnv) assertNoSecrets(t *testing.T, what, text string) {
	t.Helper()
	secrets := []string{fakeInstallationToken, fakeRegistrationToken, fakeQueueToken, fakeQueueToken2, fakeJIT, "api-version="}
	e.gh.mu.Lock()
	secrets = append(secrets, e.gh.jwts...)
	e.gh.mu.Unlock()
	for _, s := range secrets {
		if s != "" && strings.Contains(text, s) {
			t.Errorf("%s contains %q: %s", what, s, text)
		}
	}
}

func TestNewTransportSetsHTTP2Ping(t *testing.T) {
	tr := NewTransport()
	if tr.HTTP2 == nil {
		t.Fatal("HTTP2 config not set")
	}
	if tr.HTTP2.SendPingTimeout != 30*time.Second || tr.HTTP2.PingTimeout != 10*time.Second {
		t.Errorf("HTTP2 = %+v, want SendPingTimeout 30s, PingTimeout 10s", *tr.HTTP2)
	}
	if !tr.ForceAttemptHTTP2 {
		t.Error("ForceAttemptHTTP2 = false")
	}
}

func TestRetryConfigOnlyChangesRateLimitAndBackoff(t *testing.T) {
	got, def := RetryConfig(), scaleset.DefaultRetryConfig()
	if got.Max != def.Max || got.WaitMin != def.WaitMin || got.WaitMax != def.WaitMax ||
		got.MaxElapsed != def.MaxElapsed || got.ShouldRetry != nil {
		t.Errorf("RetryConfig differs from the default beyond OnRateLimit and Backoff: %+v", got)
	}
	if got.Backoff == nil {
		t.Error("Backoff is nil, want the capped default")
	}
	for _, status := range []int{http.StatusTooManyRequests, http.StatusForbidden} {
		wait, retryIt := got.OnRateLimit(context.Background(), &scaleset.RateLimitError{StatusCode: status})
		if wait != 0 || retryIt {
			t.Errorf("OnRateLimit(%d) = (%v, %v), want (0, false)", status, wait, retryIt)
		}
	}
}

// retryAfterResponse returns a response with status and Retry-After value.
func retryAfterResponse(status int, value string) *http.Response {
	h := make(http.Header)
	h.Set("Retry-After", value)
	return &http.Response{StatusCode: status, Header: h}
}

func TestCapBackoff(t *testing.T) {
	const waitMin, waitMax = time.Second, 30 * time.Second
	capped := RetryConfig().Backoff
	tests := []struct {
		name string
		resp *http.Response
		max  time.Duration
	}{
		{"503 with a day of Retry-After", retryAfterResponse(http.StatusServiceUnavailable, "86400"), waitMax},
		{"503 with a far HTTP date", retryAfterResponse(http.StatusServiceUnavailable, time.Now().Add(48*time.Hour).UTC().Format(http.TimeFormat)), waitMax},
		{"502 without hints", &http.Response{StatusCode: http.StatusBadGateway, Header: http.Header{}}, waitMax},
		{"transport error", nil, waitMax},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			for attempt := range 8 {
				if got := capped(waitMin, waitMax, attempt, tc.resp); got <= 0 || got > tc.max {
					t.Errorf("attempt %d: wait = %v, want in (0, %v]", attempt, got, tc.max)
				}
			}
		})
	}
	// A short Retry-After is still followed, as the library does.
	if got := capped(waitMin, waitMax, 0, retryAfterResponse(http.StatusServiceUnavailable, "5")); got != 5*time.Second {
		t.Errorf("Retry-After 5 wait = %v, want 5s", got)
	}
	// A configured Backoff is wrapped, not replaced.
	custom := capBackoff(func(time.Duration, time.Duration, int, *http.Response) time.Duration { return time.Hour })
	if got := custom(waitMin, waitMax, 0, nil); got != waitMax {
		t.Errorf("custom backoff wait = %v, want %v", got, waitMax)
	}
}

// TestServiceUnavailableRetryAfterIsCapped checks that a 503 with a huge
// Retry-After is retried by the library within WaitMax. Without the cap the
// first wait would outlast the call's deadline and the library would give
// up after one request; without a deadline it would wait a day.
func TestServiceUnavailableRetryAfterIsCapped(t *testing.T) {
	e := newTestEnv(t)
	e.gh.actions = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "86400")
		w.WriteHeader(http.StatusServiceUnavailable)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	start := time.Now()
	_, err := e.client.GenerateJIT(ctx, testScaleSetID, "pool-0a1b2c3d")
	if err == nil {
		t.Fatal("GenerateJIT succeeded")
	}
	if elapsed := time.Since(start); elapsed > 30*time.Second {
		t.Errorf("call took %v, want the capped waits", elapsed)
	}
	want := scaleset.DefaultRetryMax + 1
	if n := e.gh.count(http.MethodPost, "/generatejitconfig"); n != want {
		t.Errorf("generatejitconfig requests = %d, want %d (retried within the cap)", n, want)
	}
	var rl *retry.RateLimitError
	if errors.As(err, &rl) {
		t.Errorf("503 became a rate limit: %v", err)
	}
	if c := retry.Classify(context.Background(), retry.OpGenerateJIT, err); c != retry.Transient {
		t.Errorf("Classify = %v, want transient", c)
	}
}

// TestRateLimitBodyCutByCallDeadline checks that a 429 whose body is still
// being read when the call's own deadline ends stays a rate limit with its
// Retry-After, so the caller waits as R52 says instead of retrying sooner.
func TestRateLimitBodyCutByCallDeadline(t *testing.T) {
	e := newTestEnv(t)
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	e.gh.actions = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Retry-After", "90")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"message":`))
		w.(http.Flusher).Flush()
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}
	// A real deadline, so the error holds context.DeadlineExceeded. 2 s
	// leaves room for the authentication steps before the 429 under -race
	// and load; the handler then holds the body until the deadline ends.
	callCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, err := e.client.GenerateJIT(callCtx, testScaleSetID, "pool-0a1b2c3d")
	if err == nil {
		t.Fatal("GenerateJIT succeeded")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("error %v does not hold the call deadline", err)
	}
	var rl *retry.RateLimitError
	if !errors.As(err, &rl) || rl.RetryAfter != 90*time.Second {
		t.Fatalf("error = %v, want RateLimitError with RetryAfter 90s", err)
	}
	if c := retry.Classify(context.Background(), retry.OpGenerateJIT, err); c != retry.RateLimited {
		t.Errorf("Classify = %v, want rate limited", c)
	}
	if n := e.gh.count(http.MethodPost, "/generatejitconfig"); n != 1 {
		t.Errorf("generatejitconfig requests = %d, want 1", n)
	}
}

// TestAdminConnectionRateLimitMessageRetriedByLibrary pins the accepted
// behavior described at RetryConfig: a 403 at the admin connection whose
// message names a rate limit, without headers the library reads, is
// retried by the library as a plain 403 and still ends as a rate limit.
func TestAdminConnectionRateLimitMessageRetriedByLibrary(t *testing.T) {
	e := newTestEnv(t)
	e.gh.registration = func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusForbidden, map[string]string{"message": "API rate limit exceeded for installation."})
	}
	e.gh.actions = func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
	}
	ctx := context.Background()
	_, err := e.client.GenerateJIT(ctx, testScaleSetID, "pool-0a1b2c3d")
	if err == nil {
		t.Fatal("GenerateJIT succeeded")
	}
	want := scaleset.DefaultRetryMax + 1
	if n := e.gh.count(http.MethodPost, "/actions/runner-registration"); n != want {
		t.Errorf("admin connection requests = %d, want %d", n, want)
	}
	if c := retry.Classify(ctx, retry.OpGenerateJIT, err); c != retry.RateLimited {
		t.Errorf("Classify = %v, want rate limited (err %v)", c, err)
	}
	if errors.Is(err, retry.ErrAuth) {
		t.Errorf("error %v is marked ErrAuth", err)
	}
	e.assertNoSecrets(t, "error", err.Error())
}

func TestGenerateJITRateLimitedWithoutRetryAfterIsNotRetried(t *testing.T) {
	e := newTestEnv(t)
	e.gh.actions = func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	}
	ctx := context.Background()
	_, err := e.client.GenerateJIT(ctx, testScaleSetID, "pool-0a1b2c3d")
	if err == nil {
		t.Fatal("GenerateJIT succeeded")
	}
	if n := e.gh.count(http.MethodPost, "/generatejitconfig"); n != 1 {
		t.Errorf("generatejitconfig requests = %d, want 1 (no library retry)", n)
	}
	var rl *retry.RateLimitError
	if !errors.As(err, &rl) {
		t.Fatalf("error %v is not a retry.RateLimitError", err)
	}
	if rl.RetryAfter != 0 || !rl.Reset.IsZero() {
		t.Errorf("RateLimitError = %+v, want no hints", rl)
	}
	if c := retry.Classify(ctx, retry.OpGenerateJIT, err); c != retry.RateLimited {
		t.Errorf("Classify = %v, want rate limited", c)
	}
	var op *retry.OpError
	if !errors.As(err, &op) || op.Op != retry.OpGenerateJIT {
		t.Errorf("error %v does not name %q", err, retry.OpGenerateJIT)
	}
	e.assertNoSecrets(t, "error", err.Error())
	e.assertNoSecrets(t, "logs", e.logs.String())
}

func TestGenerateJITRateLimitWithRetryAfter(t *testing.T) {
	e := newTestEnv(t)
	e.gh.actions = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "90")
		w.WriteHeader(http.StatusTooManyRequests)
	}
	_, err := e.client.GenerateJIT(context.Background(), testScaleSetID, "pool-0a1b2c3d")
	var rl *retry.RateLimitError
	if !errors.As(err, &rl) || rl.RetryAfter != 90*time.Second {
		t.Fatalf("error = %v, want RateLimitError with RetryAfter 90s", err)
	}
	if n := e.gh.count(http.MethodPost, "/generatejitconfig"); n != 1 {
		t.Errorf("generatejitconfig requests = %d, want 1", n)
	}
}

func TestGenerateJITServerErrorIsRetriedByLibrary(t *testing.T) {
	e := newTestEnv(t)
	var calls int
	var mu sync.Mutex
	e.gh.actions = func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls++
		n := calls
		mu.Unlock()
		if n == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"runner":           map[string]any{"id": 5, "name": "pool-0a1b2c3d", "runnerScaleSetId": testScaleSetID},
			"encodedJITConfig": fakeJIT,
		})
	}
	jit, err := e.client.GenerateJIT(context.Background(), testScaleSetID, "pool-0a1b2c3d")
	if err != nil {
		t.Fatal(err)
	}
	if jit.RunnerID != 5 || jit.RunnerName != "pool-0a1b2c3d" || jit.Encoded != fakeJIT {
		t.Errorf("JIT = %+v", jit)
	}
	if n := e.gh.count(http.MethodPost, "/generatejitconfig"); n != 2 {
		t.Errorf("generatejitconfig requests = %d, want 2", n)
	}
}

func TestJITConfigHidesEncoded(t *testing.T) {
	jit := JITConfig{RunnerID: 5, RunnerName: "pool-0a1b2c3d", Encoded: fakeJIT}
	var buf bytes.Buffer
	slog.New(slog.NewJSONHandler(&buf, nil)).Info("started", "jit", jit)
	for _, text := range []string{jit.String(), fmt.Sprint(jit), fmt.Sprintf("%v", jit), buf.String()} {
		if strings.Contains(text, fakeJIT) {
			t.Errorf("JIT config shows Encoded: %s", text)
		}
	}
	if !strings.Contains(buf.String(), "pool-0a1b2c3d") {
		t.Errorf("log lacks the runner name: %s", buf.String())
	}
}

func TestGetOrCreateCreatesWithDisableUpdate(t *testing.T) {
	e := newTestEnv(t)
	var created scaleset.RunnerScaleSet
	e.gh.actions = func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			if got := r.URL.Query().Get("runnerGroupId"); got != "1" {
				t.Errorf("runnerGroupId = %q, want 1", got)
			}
			if got := r.URL.Query().Get("name"); got != "linux-arm64-deploy" {
				t.Errorf("name = %q", got)
			}
			writeJSON(w, http.StatusOK, map[string]any{"count": 0, "value": []any{}})
		case http.MethodPost:
			if err := json.NewDecoder(r.Body).Decode(&created); err != nil {
				t.Errorf("decode create body: %v", err)
			}
			created.ID = testScaleSetID
			writeJSON(w, http.StatusOK, created)
		}
	}
	set, err := e.client.GetOrCreateScaleSetInGroup(context.Background(), DefaultRunnerGroupID, "linux-arm64-deploy")
	if err != nil {
		t.Fatal(err)
	}
	if set.ID != testScaleSetID {
		t.Errorf("ID = %d", set.ID)
	}
	if !created.RunnerSetting.DisableUpdate {
		t.Error("create request has DisableUpdate false")
	}
	if created.RunnerGroupID != 1 || created.Name != "linux-arm64-deploy" {
		t.Errorf("create request = %+v", created)
	}
	// Labels were left empty; the library fills in the name as the only label.
	if len(created.Labels) != 1 || created.Labels[0].Name != "linux-arm64-deploy" {
		t.Errorf("labels = %+v, want only the name", created.Labels)
	}
	if strings.Contains(e.logs.String(), "level=WARN") {
		t.Errorf("unexpected warning: %s", e.logs.String())
	}
}

// An existing scale set is reused. One with DisableUpdate unset is updated
// to set it (R22, R80), and the answer is checked; when the update fails or
// does not take effect, the set is still returned and a warning is logged.
func TestGetOrCreateReusesExisting(t *testing.T) {
	for _, tc := range []struct {
		name          string
		disableUpdate bool
		// patch handles the update request; nil fails the test on one.
		patch      func(w http.ResponseWriter, body scaleset.RunnerScaleSet)
		wantPatch  int
		wantWarn   bool
		wantSetOff bool // the returned set still has DisableUpdate false
	}{
		{name: "disable update true", disableUpdate: true},
		{
			name: "disable update false is updated",
			patch: func(w http.ResponseWriter, body scaleset.RunnerScaleSet) {
				body.ID = testScaleSetID
				writeJSON(w, http.StatusOK, body)
			},
			wantPatch: 1,
		},
		{
			name: "update fails",
			patch: func(w http.ResponseWriter, _ scaleset.RunnerScaleSet) {
				writeException(w, http.StatusForbidden, "AccessDeniedException", "no")
			},
			wantPatch:  1,
			wantWarn:   true,
			wantSetOff: true,
		},
		{
			name: "update accepted but ignored",
			patch: func(w http.ResponseWriter, _ scaleset.RunnerScaleSet) {
				writeJSON(w, http.StatusOK, map[string]any{"id": testScaleSetID, "RunnerSetting": map[string]any{"disableUpdate": false}})
			},
			wantPatch:  1,
			wantWarn:   true,
			wantSetOff: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newTestEnv(t)
			var patched scaleset.RunnerScaleSet
			e.gh.actions = func(w http.ResponseWriter, r *http.Request) {
				switch r.Method {
				case http.MethodGet:
					writeJSON(w, http.StatusOK, map[string]any{"count": 1, "value": []any{map[string]any{
						"id": testScaleSetID, "name": "linux-arm64-deploy",
						"RunnerSetting": map[string]any{"disableUpdate": tc.disableUpdate},
					}}})
				case http.MethodPatch:
					if tc.patch == nil {
						t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
						return
					}
					if err := json.NewDecoder(r.Body).Decode(&patched); err != nil {
						t.Errorf("decode update body: %v", err)
					}
					tc.patch(w, patched)
				default:
					t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
				}
			}
			set, err := e.client.GetOrCreateScaleSetInGroup(context.Background(), DefaultRunnerGroupID, "linux-arm64-deploy")
			if err != nil {
				t.Fatal(err)
			}
			if set.ID != testScaleSetID {
				t.Errorf("ID = %d", set.ID)
			}
			if n := e.gh.count(http.MethodPost, "/runnerscalesets"); n != 0 {
				t.Errorf("create requests = %d, want 0", n)
			}
			wantPath := fmt.Sprintf("/runnerscalesets/%d", testScaleSetID)
			if n := e.gh.count(http.MethodPatch, wantPath); n != tc.wantPatch {
				t.Errorf("update requests to %s = %d, want %d", wantPath, n, tc.wantPatch)
			}
			if tc.wantPatch > 0 && !patched.RunnerSetting.DisableUpdate {
				t.Error("update request has DisableUpdate false")
			}
			if tc.wantPatch > 0 && (patched.Name != "" || patched.RunnerGroupID != 0 || len(patched.Labels) != 0) {
				t.Errorf("update request carries more than the setting: %+v", patched)
			}
			if got := set.RunnerSetting.DisableUpdate; got == tc.wantSetOff {
				t.Errorf("returned set DisableUpdate = %v", got)
			}
			warned := strings.Contains(e.logs.String(), "level=WARN")
			if warned != tc.wantWarn {
				t.Errorf("warned = %v, want %v; logs: %s", warned, tc.wantWarn, e.logs.String())
			}
			e.assertNoSecrets(t, "logs", e.logs.String())
		})
	}
}

func TestGetScaleSetAbsentIsNil(t *testing.T) {
	e := newTestEnv(t)
	e.gh.actions = func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"count": 0, "value": []any{}})
	}
	set, err := e.client.GetScaleSetInGroup(context.Background(), DefaultRunnerGroupID, "linux-arm64-deploy")
	if err != nil || set != nil {
		t.Errorf("GetScaleSet = %v, %v, want nil, nil", set, err)
	}
}

// TestSeveralScaleSetsWithNameArePermanent covers the library's report of
// several scale sets with one name, sent with the 200 response's status.
func TestSeveralScaleSetsWithNameArePermanent(t *testing.T) {
	e := newTestEnv(t)
	e.gh.actions = func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
		writeJSON(w, http.StatusOK, map[string]any{"count": 2, "value": []any{
			map[string]any{"id": testScaleSetID, "name": "linux-arm64-deploy"},
			map[string]any{"id": testScaleSetID + 1, "name": "linux-arm64-deploy"},
		}})
	}
	ctx := context.Background()
	for _, tc := range []struct {
		op   retry.Op
		call func() error
	}{
		{retry.OpGetRunnerScaleSet, func() error {
			_, err := e.client.GetScaleSetInGroup(ctx, DefaultRunnerGroupID, "linux-arm64-deploy")
			return err
		}},
		{retry.OpGetOrCreateScaleSet, func() error {
			_, err := e.client.GetOrCreateScaleSetInGroup(ctx, DefaultRunnerGroupID, "linux-arm64-deploy")
			return err
		}},
	} {
		err := tc.call()
		if err == nil {
			t.Fatalf("%s succeeded", tc.op)
		}
		t.Logf("error: %v", err)
		if c := retry.Classify(ctx, tc.op, err); c != retry.Permanent {
			t.Errorf("%s: Classify = %v, want permanent", tc.op, c)
		}
		if !errors.Is(err, retry.ErrAmbiguousScaleSet) {
			t.Errorf("%s: error %v is not ErrAmbiguousScaleSet", tc.op, err)
		}
		if !strings.Contains(err.Error(), "200") {
			t.Errorf("%s: error %q does not keep the server's status", tc.op, err)
		}
		e.assertNoSecrets(t, "error", err.Error())
	}
	if n := e.gh.count(http.MethodPost, "/runnerscalesets"); n != 0 {
		t.Errorf("create requests = %d, want 0", n)
	}
}

func TestGetRunnerByName(t *testing.T) {
	e := newTestEnv(t)
	e.gh.actions = func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("agentName") == "missing" {
			writeJSON(w, http.StatusOK, map[string]any{"count": 0, "value": []any{}})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"count": 1, "value": []any{
			map[string]any{"id": 9, "name": "pool-0a1b2c3d", "runnerScaleSetId": testScaleSetID},
		}})
	}
	got, err := e.client.GetRunnerByName(context.Background(), "pool-0a1b2c3d")
	if err != nil {
		t.Fatal(err)
	}
	if *got != (Runner{ID: 9, Name: "pool-0a1b2c3d", ScaleSetID: testScaleSetID}) {
		t.Errorf("runner = %+v", *got)
	}
	got, err = e.client.GetRunnerByName(context.Background(), "missing")
	if err != nil || got != nil {
		t.Errorf("missing runner = %v, %v, want nil, nil", got, err)
	}
}

func TestNewScaleSetClientRejectsBadConfig(t *testing.T) {
	secrets := logging.NewSecrets()
	signer, err := NewAppSigner("Iv1.testclient", rsaKey(t), retry.SystemClock(), secrets)
	if err != nil {
		t.Fatal(err)
	}
	good := ScaleSetConfig{ConfigURL: "https://github.com/acme/widget", InstallationID: 1, Signer: signer, Secrets: secrets}
	if _, err := NewScaleSetClient(good); err != nil {
		t.Fatalf("good config: %v", err)
	}
	for name, mutate := range map[string]func(*ScaleSetConfig){
		"no signer":       func(c *ScaleSetConfig) { c.Signer = nil },
		"no secrets":      func(c *ScaleSetConfig) { c.Secrets = nil },
		"no installation": func(c *ScaleSetConfig) { c.InstallationID = 0 },
	} {
		cfg := good
		mutate(&cfg)
		if _, err := NewScaleSetClient(cfg); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
}

// TestScaleSetClientURLForms proves 0002 SPEC R231: the config URL is the
// repository URL; the first version's organization URL is no longer
// accepted.
func TestScaleSetClientURLForms(t *testing.T) {
	secrets := logging.NewSecrets()
	signer, err := NewAppSigner("Iv1.testclient", rsaKey(t), retry.SystemClock(), secrets)
	if err != nil {
		t.Fatal(err)
	}
	for url, ok := range map[string]bool{
		"https://github.com/acme/widget":     true,
		"https://github.com/acme":            false,
		"https://github.com/acme/widget/":    false,
		"https://github.com/acme/widget.git": false,
		"https://github.com/enterprises/x":   false,
		"https://github.com/":                false,
		"https://github.com/a/b/c":           false,
		"https://github.com/acme/widget?x=1": false,
		"https://github.com/acme/widget#x":   false,
		"http://github.com/acme/widget":      false,
		"example":                            false,
		"":                                   false,
	} {
		_, err := NewScaleSetClient(ScaleSetConfig{ConfigURL: url, InstallationID: 1, Signer: signer, Secrets: secrets})
		if (err == nil) != ok {
			t.Errorf("ConfigURL %q: error = %v, want accepted = %v", url, err, ok)
		}
	}
}

// groupListResponse is the Actions service's answer to a runner group query.
func groupListResponse(w http.ResponseWriter, groups ...map[string]any) {
	if groups == nil {
		groups = []map[string]any{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"count": len(groups), "value": groups})
}

// TestScaleSetClientUsesRepositoryRegistration proves 0002 SPEC R239, R240:
// with a repository config URL the library asks the repository's
// registration token endpoint, registers with that URL, and the scale set is
// looked up and created in runner group 1.
func TestScaleSetClientUsesRepositoryRegistration(t *testing.T) {
	e := newTestEnv(t)
	var registeredURL string
	e.gh.registration = func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			URL string `json:"url"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode admin connection body: %v", err)
		}
		registeredURL = body.URL
		writeJSON(w, http.StatusOK, map[string]any{"url": e.gh.srv.URL + "/svc/", "token": fakeAdminToken(time.Hour)})
	}
	var queried string
	var created scaleset.RunnerScaleSet
	e.gh.actions = func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			queried = r.URL.Query().Get("runnerGroupId")
			groupListResponse(w)
		case http.MethodPost:
			if err := json.NewDecoder(r.Body).Decode(&created); err != nil {
				t.Errorf("decode create body: %v", err)
			}
			created.ID = testScaleSetID
			writeJSON(w, http.StatusOK, created)
		}
	}
	if _, err := e.client.GetOrCreateScaleSetInGroup(context.Background(), DefaultRunnerGroupID, "linux-arm64-deploy"); err != nil {
		t.Fatal(err)
	}
	const tokenPath = "/api/v3/repos/" + testOwner + "/" + testRepo + "/actions/runners/registration-token"
	if n := e.gh.count(http.MethodPost, tokenPath); n != 1 {
		e.gh.mu.Lock()
		t.Errorf("registration token requests at %s = %d, want 1; requests %v", tokenPath, n, e.gh.requests)
		e.gh.mu.Unlock()
	}
	if want := e.gh.srv.URL + "/" + testOwner + "/" + testRepo; registeredURL != want {
		t.Errorf("admin connection url = %q, want %q", registeredURL, want)
	}
	if queried != "1" || created.RunnerGroupID != 1 {
		t.Errorf("get runnerGroupId = %q, create runner group %d, want 1 for both", queried, created.RunnerGroupID)
	}
}

// TestScaleSetInGroup proves 0002 SPEC R239: get and create use the given
// runner group.
func TestScaleSetInGroup(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name    string
		groupID int
		call    func(c *ScaleSetClient) (*scaleset.RunnerScaleSet, error)
	}{
		{"in group", 5, func(c *ScaleSetClient) (*scaleset.RunnerScaleSet, error) {
			return c.GetOrCreateScaleSetInGroup(ctx, 5, "linux-arm64-deploy")
		}},
		{"default group", DefaultRunnerGroupID, func(c *ScaleSetClient) (*scaleset.RunnerScaleSet, error) {
			return c.GetOrCreateScaleSetInGroup(ctx, DefaultRunnerGroupID, "linux-arm64-deploy")
		}},
	} {
		t.Run("create "+tc.name, func(t *testing.T) {
			e := newTestEnv(t)
			var created scaleset.RunnerScaleSet
			var queried string
			e.gh.actions = func(w http.ResponseWriter, r *http.Request) {
				switch r.Method {
				case http.MethodGet:
					queried = r.URL.Query().Get("runnerGroupId")
					groupListResponse(w)
				case http.MethodPost:
					if err := json.NewDecoder(r.Body).Decode(&created); err != nil {
						t.Errorf("decode create body: %v", err)
					}
					created.ID = testScaleSetID
					writeJSON(w, http.StatusOK, created)
				}
			}
			if _, err := tc.call(e.client); err != nil {
				t.Fatal(err)
			}
			if want := fmt.Sprint(tc.groupID); queried != want || created.RunnerGroupID != tc.groupID {
				t.Errorf("get group %q, create group %d, want %s", queried, created.RunnerGroupID, want)
			}
		})
	}

	for _, tc := range []struct {
		name    string
		groupID int
		call    func(c *ScaleSetClient) (*scaleset.RunnerScaleSet, error)
	}{
		{"in group", 5, func(c *ScaleSetClient) (*scaleset.RunnerScaleSet, error) {
			return c.GetScaleSetInGroup(ctx, 5, "linux-arm64-deploy")
		}},
		{"default group", DefaultRunnerGroupID, func(c *ScaleSetClient) (*scaleset.RunnerScaleSet, error) {
			return c.GetScaleSetInGroup(ctx, DefaultRunnerGroupID, "linux-arm64-deploy")
		}},
	} {
		t.Run("get "+tc.name, func(t *testing.T) {
			e := newTestEnv(t)
			var queried string
			e.gh.actions = func(w http.ResponseWriter, r *http.Request) {
				queried = r.URL.Query().Get("runnerGroupId")
				groupListResponse(w, map[string]any{"id": testScaleSetID, "name": "linux-arm64-deploy"})
			}
			set, err := tc.call(e.client)
			if err != nil || set == nil || set.ID != testScaleSetID {
				t.Fatalf("GetScaleSet = %+v, %v", set, err)
			}
			if want := fmt.Sprint(tc.groupID); queried != want {
				t.Errorf("runnerGroupId = %q, want %s", queried, want)
			}
		})
	}
}

// TestScaleSetClientToken proves 0003 SPEC R278: with Token the library
// asks the repository's registration token with "Bearer <token>" (the fake
// server refuses any other) and requests no App installation token; Token
// beside the App credential, and no credential at all, are refused.
func TestScaleSetClientToken(t *testing.T) {
	token := fixtureToken(t, "ghp_")
	e := newTokenTestEnv(t, token)
	e.gh.actions = func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			groupListResponse(w)
		case http.MethodPost:
			var created scaleset.RunnerScaleSet
			if err := json.NewDecoder(r.Body).Decode(&created); err != nil {
				t.Errorf("decode create body: %v", err)
			}
			created.ID = testScaleSetID
			writeJSON(w, http.StatusOK, created)
		}
	}
	set, err := e.client.GetOrCreateScaleSetInGroup(context.Background(), DefaultRunnerGroupID, "linux-arm64-deploy")
	if err != nil {
		assertNoToken(t, "error", err.Error(), token)
		t.Fatal("GetOrCreateScaleSetInGroup failed")
	}
	if set.ID != testScaleSetID {
		t.Errorf("scale set ID = %d, want %d", set.ID, testScaleSetID)
	}
	const tokenPath = "/api/v3/repos/" + testOwner + "/" + testRepo + "/actions/runners/registration-token"
	if n := e.gh.count(http.MethodPost, tokenPath); n != 1 {
		t.Errorf("registration token requests = %d, want 1", n)
	}
	if n := e.gh.count(http.MethodPost, "/access_tokens"); n != 0 {
		t.Errorf("installation token requests = %d, want 0", n)
	}
	assertNoToken(t, "logs", e.logs.String(), token)

	secrets := logging.NewSecrets()
	signer, err := NewAppSigner("Iv1.testclient", rsaKey(t), retry.SystemClock(), secrets)
	if err != nil {
		t.Fatal(err)
	}
	good := ScaleSetConfig{ConfigURL: "https://github.com/acme/widget", Token: token, Secrets: secrets}
	if _, err := NewScaleSetClient(good); err != nil {
		t.Fatalf("token config: %v", err)
	}
	for name, mutate := range map[string]func(*ScaleSetConfig){
		"token and signer":       func(c *ScaleSetConfig) { c.Signer, c.InstallationID = signer, 42 },
		"token and signer only":  func(c *ScaleSetConfig) { c.Signer = signer },
		"token and installation": func(c *ScaleSetConfig) { c.InstallationID = 42 },
		"neither":                func(c *ScaleSetConfig) { c.Token = "" },
		"no secrets":             func(c *ScaleSetConfig) { c.Secrets = nil },
	} {
		cfg := good
		mutate(&cfg)
		_, err := NewScaleSetClient(cfg)
		if err == nil {
			t.Errorf("%s: no error", name)
			continue
		}
		assertNoToken(t, name+" error", err.Error(), token)
	}
}
