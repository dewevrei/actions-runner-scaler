package github

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/actions/scaleset"

	"github.com/dewevrei/actions-runner-scaler/internal/logging"
	"github.com/dewevrei/actions-runner-scaler/internal/retry"
)

// TestRemoveRunnerErrors drives the library against responses and checks
// the converted error's status, sentinel and class.
func TestRemoveRunnerErrors(t *testing.T) {
	tests := []struct {
		name       string
		respond    func(w http.ResponseWriter)
		wantStatus int // 0: no status
		sentinel   error
		wantClass  retry.Class
	}{
		{
			name:       "runner not found",
			respond:    func(w http.ResponseWriter) { writeException(w, 404, "AgentNotFoundException", "missing") },
			wantStatus: 404, sentinel: retry.ErrRunnerGone, wantClass: retry.Gone,
		},
		{
			name:       "job still running as 409",
			respond:    func(w http.ResponseWriter) { writeException(w, 409, "JobStillRunningException", "busy") },
			wantStatus: 409, sentinel: retry.ErrJobStillRunning, wantClass: retry.Transient,
		},
		{
			name:       "job still running as 400",
			respond:    func(w http.ResponseWriter) { writeException(w, 400, "JobStillRunningException", "busy") },
			wantStatus: 400, sentinel: retry.ErrJobStillRunning, wantClass: retry.Transient,
		},
		{
			name:       "runner exists as 409",
			respond:    func(w http.ResponseWriter) { writeException(w, 409, "AgentExistsException", "exists") },
			wantStatus: 409, sentinel: retry.ErrRunnerExists, wantClass: retry.Permanent,
		},
		{
			name:       "plain 404",
			respond:    func(w http.ResponseWriter) { writeException(w, 404, "SomethingElse", "nope") },
			wantStatus: 404, wantClass: retry.Gone,
		},
		{
			name:       "unauthorized",
			respond:    func(w http.ResponseWriter) { writeException(w, 401, "Unauthorized", "no") },
			wantStatus: 401, wantClass: retry.Permanent,
		},
		{
			// The library leaves 403 unwrapped; the recorded status decides.
			name:       "forbidden without rate limit",
			respond:    func(w http.ResponseWriter) { writeException(w, 403, "AccessDenied", "denied") },
			wantStatus: 403, wantClass: retry.Permanent,
		},
		{
			// The library's own test misses this message; the recorded
			// message makes it a rate limit, as on the REST path.
			name: "forbidden with rate limit message",
			respond: func(w http.ResponseWriter) {
				w.Header().Set("X-RateLimit-Remaining", "4000")
				writeException(w, 403, "RateLimit", "API rate limit exceeded for installation ID 42.")
			},
			wantStatus: 403, wantClass: retry.RateLimited,
		},
		{
			name:       "unprocessable",
			respond:    func(w http.ResponseWriter) { writeException(w, 422, "Invalid", "bad") },
			wantStatus: 422, wantClass: retry.Permanent,
		},
		{
			name: "server error after retries",
			respond: func(w http.ResponseWriter) {
				w.WriteHeader(http.StatusInternalServerError)
			},
			wantStatus: 500, wantClass: retry.Transient,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			e := newTestEnv(t)
			e.gh.actions = func(w http.ResponseWriter, r *http.Request) { tc.respond(w) }
			ctx := context.Background()
			err := e.client.RemoveRunner(ctx, 9)
			if err == nil {
				t.Fatal("RemoveRunner succeeded")
			}
			t.Logf("error: %v", err)
			var op *retry.OpError
			if !errors.As(err, &op) || op.Op != retry.OpRemoveRunner {
				t.Errorf("error %v does not name %q", err, retry.OpRemoveRunner)
			}
			var sc retry.StatusCoder
			gotStatus := 0
			if errors.As(err, &sc) {
				gotStatus = sc.HTTPStatus()
			}
			if gotStatus != tc.wantStatus {
				t.Errorf("status = %d, want %d (err %v)", gotStatus, tc.wantStatus, err)
			}
			if tc.sentinel != nil && !errors.Is(err, tc.sentinel) {
				t.Errorf("error %v is not %v", err, tc.sentinel)
			}
			if c := retry.Classify(ctx, retry.OpRemoveRunner, err); c != tc.wantClass {
				t.Errorf("Classify = %v, want %v", c, tc.wantClass)
			}
			e.assertNoSecrets(t, "error", err.Error())
		})
	}
}

func TestPrimaryRateLimit403KeepsReset(t *testing.T) {
	e := newTestEnv(t)
	reset := time.Now().Add(10 * time.Minute).Truncate(time.Second)
	e.gh.actions = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-RateLimit-Remaining", "0")
		w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(reset.Unix(), 10))
		writeException(w, http.StatusForbidden, "RateLimit", "API rate limit exceeded")
	}
	err := e.client.RemoveRunner(context.Background(), 9)
	var rl *retry.RateLimitError
	if !errors.As(err, &rl) {
		t.Fatalf("error %v is not a RateLimitError", err)
	}
	if !rl.Reset.Equal(reset) {
		t.Errorf("Reset = %v, want %v", rl.Reset, reset)
	}
	if rl.RetryAfter <= 0 || rl.RetryAfter > 10*time.Minute {
		t.Errorf("RetryAfter = %v, want up to the reset", rl.RetryAfter)
	}
	if n := e.gh.count(http.MethodDelete, "/agents/9"); n != 1 {
		t.Errorf("requests = %d, want 1", n)
	}
}

func TestSecondaryRateLimitDropsReset(t *testing.T) {
	e := newTestEnv(t)
	e.gh.actions = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-RateLimit-Remaining", "4000")
		w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(time.Now().Add(time.Hour).Unix(), 10))
		writeException(w, http.StatusForbidden, "RateLimit", "You have exceeded a secondary rate limit")
	}
	err := e.client.RemoveRunner(context.Background(), 9)
	var rl *retry.RateLimitError
	if !errors.As(err, &rl) {
		t.Fatalf("error %v is not a RateLimitError", err)
	}
	if !rl.Reset.IsZero() || rl.RetryAfter != 0 {
		t.Errorf("RateLimitError = %+v, want no hints for a secondary limit with quota left", rl)
	}
}

// TestSecondaryRateLimitWithSpentQuotaKeepsReset sends a secondary limit
// message with X-RateLimit-Remaining 0. GitHub says to wait for
// X-RateLimit-Reset whenever the remaining quota is 0, so Reset must survive
// although the library calls the limit secondary, and the wait must reach
// the reset even when Retry-After is shorter.
func TestSecondaryRateLimitWithSpentQuotaKeepsReset(t *testing.T) {
	for _, retryAfter := range []string{"", "30"} {
		t.Run("Retry-After "+strconv.Quote(retryAfter), func(t *testing.T) {
			e := newTestEnv(t)
			reset := time.Now().Add(10 * time.Minute).Truncate(time.Second)
			e.gh.actions = func(w http.ResponseWriter, r *http.Request) {
				if retryAfter != "" {
					w.Header().Set("Retry-After", retryAfter)
				}
				w.Header().Set("X-RateLimit-Remaining", "0")
				w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(reset.Unix(), 10))
				writeException(w, http.StatusForbidden, "RateLimit", "You have exceeded a secondary rate limit")
			}
			ctx := context.Background()
			start := time.Now()
			err := e.client.RemoveRunner(ctx, 9)
			var rl *retry.RateLimitError
			if !errors.As(err, &rl) {
				t.Fatalf("error %v is not a RateLimitError", err)
			}
			if !rl.Reset.Equal(reset) {
				t.Errorf("Reset = %v, want %v", rl.Reset, reset)
			}
			if c := retry.Classify(ctx, retry.OpRemoveRunner, err); c != retry.RateLimited {
				t.Errorf("Classify = %v, want rate limited", c)
			}
			if got, want := waitFor(t, start, err), reset.Sub(start); got != want {
				t.Errorf("wait = %v, want %v (until the reset)", got, want)
			}
			if n := e.gh.count(http.MethodDelete, "/agents/9"); n != 1 {
				t.Errorf("requests = %d, want 1", n)
			}
		})
	}
}

// fixedClock is a retry.Clock stopped at one time.
type fixedClock struct{ now time.Time }

func (c fixedClock) Now() time.Time                       { return c.now }
func (c fixedClock) After(time.Duration) <-chan time.Time { return nil }

// waitFor returns how long retry.NextAllowed holds a call back after err,
// measured from now.
func waitFor(t *testing.T, now time.Time, err error) time.Duration {
	t.Helper()
	n, nerr := retry.NewNextAllowed(fixedClock{now: now})
	if nerr != nil {
		t.Fatal(nerr)
	}
	// The op only picks the kind of wait; the error decides its length.
	if !n.Record(retry.OpRemoveRunner, err) {
		t.Fatalf("error %v is not a rate limit", err)
	}
	return n.At(retry.OpRemoveRunner).Sub(now)
}

// TestLibraryErrorMessageIsShort makes the server answer with a large HTML
// page, which the library quotes whole in its error text.
func TestLibraryErrorMessageIsShort(t *testing.T) {
	e := newTestEnv(t)
	page := "<html><body>" + strings.Repeat("<p>Unprocessable entity, see the docs.</p>\n", 2000) + "</body></html>"
	e.gh.actions = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = w.Write([]byte(page))
	}
	ctx := context.Background()
	err := e.client.RemoveRunner(ctx, 9)
	var se *StatusError
	if !errors.As(err, &se) {
		t.Fatalf("error %v is not a StatusError", err)
	}
	if se.Code != http.StatusUnprocessableEntity {
		t.Errorf("Code = %d, want 422", se.Code)
	}
	if n := utf8.RuneCountInString(se.Message); n > maxMessageRunes+1 {
		t.Errorf("Message has %d runes, want at most %d", n, maxMessageRunes+1)
	}
	if !strings.HasSuffix(se.Message, "…") {
		t.Errorf("Message %q does not end with an ellipsis", se.Message)
	}
	if n := len(err.Error()); n > 2*maxMessageRunes*utf8.UTFMax {
		t.Errorf("error text has %d bytes", n)
	}
	if c := retry.Classify(ctx, retry.OpRemoveRunner, err); c != retry.Permanent {
		t.Errorf("Classify = %v, want permanent", c)
	}
}

func TestShortMessageRedactsBeforeCutting(t *testing.T) {
	secrets := logging.NewSecrets()
	secret := "secret-value-" + strings.Repeat("s", 40)
	secrets.Register(secret)
	// The secret straddles the cut: only redacting first hides its start.
	msg := strings.Repeat("a", maxMessageRunes-10) + " " + secret
	got := shortMessage(secrets, msg)
	if strings.Contains(got, "secret-value") {
		t.Errorf("shortMessage left part of the secret: %q", got)
	}
	if got := shortMessage(secrets, "  two\n\tlines  "); got != "two lines" {
		t.Errorf("shortMessage folded white space to %q", got)
	}
	if got := shortMessage(secrets, ""); got != "" {
		t.Errorf("shortMessage(\"\") = %q", got)
	}
}

func TestDecodeErrorOn200IsTransientAndWithholdsBody(t *testing.T) {
	e := newTestEnv(t)
	e.gh.actions = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		// Truncated JSON that still carries the secret.
		_, _ = w.Write([]byte(`{"runner":{"id":5},"encodedJITConfig":"` + fakeJIT + `"`))
	}
	ctx := context.Background()
	_, err := e.client.GenerateJIT(ctx, testScaleSetID, "pool-0a1b2c3d")
	if err == nil {
		t.Fatal("GenerateJIT succeeded")
	}
	var sc retry.StatusCoder
	if errors.As(err, &sc) {
		t.Errorf("error has status %d, want none", sc.HTTPStatus())
	}
	if c := retry.Classify(ctx, retry.OpGenerateJIT, err); c != retry.Transient {
		t.Errorf("Classify = %v, want transient", c)
	}
	t.Logf("error: %v", err)
	if !strings.Contains(err.Error(), "withheld") {
		t.Errorf("error %q does not say details are withheld", err)
	}
	e.assertNoSecrets(t, "error", err.Error())
	e.assertNoSecrets(t, "logs", e.logs.String())
}

// TestTransportErrorAfter403LeavesSlotEmpty makes the admin connection
// request, which the library retries on 403, fail with 403 and then with a
// dropped connection. The recorded 403 must not survive the last attempt.
func TestTransportErrorAfter403LeavesSlotEmpty(t *testing.T) {
	e := newTestEnv(t)
	var calls atomic.Int32
	e.gh.registration = func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			writeJSON(w, http.StatusForbidden, map[string]string{"message": "not yet"})
			return
		}
		conn, _, err := http.NewResponseController(w).Hijack()
		if err != nil {
			t.Errorf("hijack: %v", err)
			return
		}
		_ = conn.Close()
	}
	ctx := context.Background()
	err := e.client.RemoveRunner(ctx, 9)
	if err == nil {
		t.Fatal("RemoveRunner succeeded")
	}
	t.Logf("error: %v", err)
	if calls.Load() < 2 {
		t.Fatalf("admin connection attempts = %d, want at least 2", calls.Load())
	}
	var sc retry.StatusCoder
	if errors.As(err, &sc) {
		t.Errorf("error has status %d, want none (transport error): %v", sc.HTTPStatus(), err)
	}
	if c := retry.Classify(ctx, retry.OpRemoveRunner, err); c != retry.Transient {
		t.Errorf("Classify = %v, want transient", c)
	}
	e.assertNoSecrets(t, "error", err.Error())
}

func TestConvertErrorStatusOrder(t *testing.T) {
	secrets := logging.NewSecrets()
	textErr := errors.New(`request GET https://example.invalid/x?token=abc failed(status="422 Unprocessable Entity"): bad`)
	tests := []struct {
		name     string
		err      error
		recorded int
		want     int
	}{
		{"text only when nothing recorded", textErr, 0, 422},
		{"recorded wins over text", textErr, 403, 403},
		{"sentinel wins over recorded", errors.Join(scaleset.NotFoundError, textErr), 403, 404},
		{"recorded below 400 ignored", textErr, 200, 422},
		{"text below 400 ignored", errors.New(`failed(status="200 OK"): decode`), 0, 0},
		{"no status at all", errors.New("dial tcp: connection refused"), 0, 0},
		{"several scale sets on 200", errors.New(`request GET https://example.invalid/x?token=abc failed(status="200 OK"): multiple runner scale sets found with name "pool": unknown error`), 0, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := convertError(retry.OpRemoveRunner, tc.err, lastFailure{status: tc.recorded}, secrets)
			got := 0
			var sc retry.StatusCoder
			if errors.As(err, &sc) {
				got = sc.HTTPStatus()
			}
			if got != tc.want {
				t.Errorf("status = %d, want %d", got, tc.want)
			}
			if strings.Contains(err.Error(), "token=abc") {
				t.Errorf("error shows the URL query: %v", err)
			}
		})
	}
	if convertError(retry.OpRemoveRunner, nil, lastFailure{status: 500}, secrets) != nil {
		t.Error("nil error converted to non-nil")
	}
}

// TestConvertErrorAmbiguousScaleSet checks that several scale sets with one
// name are marked with their own sentinel and carry no status, so they stay
// permanent at every call site, even one that reads 409 as transient.
func TestConvertErrorAmbiguousScaleSet(t *testing.T) {
	secrets := logging.NewSecrets()
	libErr := errors.New(`request GET https://example.invalid/x?token=abc failed(status="200 OK"): multiple runner scale sets found with name "pool": unknown error`)
	for _, op := range []retry.Op{retry.OpGetRunnerScaleSet, retry.OpGetOrCreateScaleSet, retry.OpSessionCreate} {
		err := convertError(op, libErr, lastFailure{}, secrets)
		if !errors.Is(err, retry.ErrAmbiguousScaleSet) {
			t.Errorf("%s: error %v is not ErrAmbiguousScaleSet", op, err)
		}
		var sc retry.StatusCoder
		if errors.As(err, &sc) {
			t.Errorf("%s: error has status %d, want none", op, sc.HTTPStatus())
		}
		if c := retry.Classify(context.Background(), op, err); c != retry.Permanent {
			t.Errorf("%s: Classify = %v, want permanent", op, c)
		}
		if !strings.Contains(err.Error(), "server answered 200") {
			t.Errorf("%s: error %q does not keep the server's status", op, err)
		}
		if strings.Contains(err.Error(), "token=abc") {
			t.Errorf("%s: error shows the URL query: %v", op, err)
		}
	}
	// Any other error is not marked.
	err := convertError(retry.OpGetRunnerScaleSet, errors.New(`failed(status="409 Conflict"): conflict`), lastFailure{}, secrets)
	if errors.Is(err, retry.ErrAmbiguousScaleSet) {
		t.Errorf("409 error %v is marked ErrAmbiguousScaleSet", err)
	}
}

func TestConvertErrorKeepsContextErrors(t *testing.T) {
	secrets := logging.NewSecrets()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := convertError(retry.OpGetMessage, context.Canceled, lastFailure{}, secrets)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("error %v lost context.Canceled", err)
	}
	if c := retry.Classify(ctx, retry.OpGetMessage, err); c != retry.Canceled {
		t.Errorf("Classify = %v, want canceled", c)
	}
	err = convertError(retry.OpGetMessage, context.DeadlineExceeded, lastFailure{}, secrets)
	if c := retry.Classify(context.Background(), retry.OpGetMessage, err); c != retry.Transient {
		t.Errorf("Classify deadline = %v, want transient", c)
	}
}

func TestStatusRecorderClearsEachRoundTrip(t *testing.T) {
	responses := []func() (*http.Response, error){
		func() (*http.Response, error) { return &http.Response{StatusCode: 403, Body: http.NoBody}, nil },
		func() (*http.Response, error) { return nil, errors.New("connection reset") },
		func() (*http.Response, error) { return &http.Response{StatusCode: 502, Body: http.NoBody}, nil },
		func() (*http.Response, error) { return &http.Response{StatusCode: 200, Body: http.NoBody}, nil },
	}
	want := []int32{403, 0, 502, 0}
	var i int
	rec := &statusRecorder{next: roundTripFunc(func(*http.Request) (*http.Response, error) {
		r := responses[i]
		i++
		return r()
	})}
	ctx, slot := withStatusSlot(context.Background())
	for n := range responses {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://example.invalid/", nil)
		resp, err := rec.RoundTrip(req)
		if err == nil {
			_ = resp.Body.Close()
		}
		if got := int32(slot.failure().status); got != want[n] {
			t.Errorf("after response %d slot = %d, want %d", n, got, want[n])
		}
	}
	// A request without a slot passes through.
	i = 0
	req, _ := http.NewRequest(http.MethodGet, "http://example.invalid/", nil)
	if resp, err := rec.RoundTrip(req); err == nil {
		_ = resp.Body.Close()
	}
}

func TestStatusRecorderMarksAuthExchange(t *testing.T) {
	steps := []struct {
		method, path string
		status       int
		wantAuth     bool
	}{
		{http.MethodPost, "/api/v3/app/installations/42/access_tokens", 404, true},
		{http.MethodDelete, "/svc/_apis/runtime/runnerscalesets/7/agents/9", 404, false},
		{http.MethodPost, "/repos/o/r/actions/runners/registration-token", 403, true},
		{http.MethodPost, "/api/v3/repos/o/r/actions/runners/registration-token", 404, true}, // GHES
		{http.MethodPost, "/actions/runner-registration", 200, false},                        // success clears
		{http.MethodPost, "/orgs/o/actions/runners/registration-token", 401, true},
		{http.MethodPost, "/enterprises/e/actions/runners/registration-token", 422, true},
		{http.MethodPost, "/api/v3/actions/runner-registration", 401, true},
		{http.MethodPost, "/app/installations/42/access_tokens", 500, true},
		{http.MethodGet, "/app/installations/42/access_tokens", 404, false},
		{http.MethodPost, "/svc/app/installations", 404, false},
		{http.MethodPost, "/access_tokens", 404, false},
	}
	var i int
	rec := &statusRecorder{next: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: steps[i].status, Body: http.NoBody}, nil
	})}
	ctx, slot := withStatusSlot(context.Background())
	for i = range steps {
		st := steps[i]
		req, _ := http.NewRequestWithContext(ctx, st.method, "https://example.invalid"+st.path, nil)
		resp, err := rec.RoundTrip(req)
		if err == nil {
			_ = resp.Body.Close()
		}
		if got := slot.failure().authExchange; got != st.wantAuth {
			t.Errorf("%s %s %d: auth = %v, want %v", st.method, st.path, st.status, got, st.wantAuth)
		}
	}
	// A transport error after an auth exchange failure clears the mark.
	rec.next = roundTripFunc(func(*http.Request) (*http.Response, error) { return nil, errors.New("reset") })
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, "https://example.invalid/actions/runner-registration", nil)
	if _, err := rec.RoundTrip(req); err == nil {
		t.Fatal("round trip succeeded")
	}
	if got := slot.failure(); got != (lastFailure{}) {
		t.Errorf("after transport error slot = %+v, want empty", got)
	}
}

// TestAuthExchangeFailureIsNotGone makes the token exchange the library runs
// inside RemoveRunner fail. Its 404 is about the App's access, so the call
// must classify as permanent, not as a removed runner (R50, R51).
func TestAuthExchangeFailureIsNotGone(t *testing.T) {
	notFound := func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusNotFound, map[string]string{"message": "Not Found"})
	}
	tests := []struct {
		name      string
		set       func(*fakeGitHub)
		wantClass retry.Class
		wantAuth  bool
		// retried, when set, is a POST path suffix the library must have
		// requested more than once.
		retried string
	}{
		{"installation token 404", func(f *fakeGitHub) { f.installation = notFound }, retry.Permanent, true, ""},
		{"registration token 404", func(f *fakeGitHub) { f.registrationToken = notFound }, retry.Permanent, true, ""},
		{"admin connection 404", func(f *fakeGitHub) { f.registration = notFound }, retry.Permanent, true, ""},
		{"installation token 401", func(f *fakeGitHub) {
			f.installation = func(w http.ResponseWriter, r *http.Request) {
				writeJSON(w, http.StatusUnauthorized, map[string]string{"message": "Bad credentials"})
			}
		}, retry.Permanent, true, ""},
		{"installation token 422", func(f *fakeGitHub) {
			f.installation = func(w http.ResponseWriter, r *http.Request) {
				writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"message": "Validation Failed"})
			}
		}, retry.Permanent, true, ""},
		// The library retries 401 and 403 on the admin connection request
		// alone (scaleset v0.5.0 client.go
		// getActionsServiceAdminConnectionRequest); the last 403 is still
		// an authentication refusal.
		{"admin connection 403 repeated", func(f *fakeGitHub) {
			f.registration = func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("X-RateLimit-Remaining", "4000")
				writeJSON(w, http.StatusForbidden, map[string]string{"message": "Forbidden"})
			}
		}, retry.Permanent, true, "/actions/runner-registration"},
		{"registration token 502", func(f *fakeGitHub) {
			f.registrationToken = func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusBadGateway) }
		}, retry.Transient, false, ""},
		{"installation token rate limited", func(f *fakeGitHub) {
			f.installation = func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Retry-After", "60")
				w.WriteHeader(http.StatusTooManyRequests)
			}
		}, retry.RateLimited, false, ""},
		// A rate limit inside the exchange is the call's transient error,
		// also when only the message marks it.
		{"registration token 403 rate limit message", func(f *fakeGitHub) {
			f.registrationToken = func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("X-RateLimit-Remaining", "4000")
				writeJSON(w, http.StatusForbidden, map[string]string{"message": "API rate limit exceeded for installation ID 42."})
			}
		}, retry.RateLimited, false, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			e := newTestEnv(t)
			tc.set(e.gh)
			e.gh.actions = func(w http.ResponseWriter, r *http.Request) {
				t.Errorf("unexpected Actions service request %s %s", r.Method, r.URL.Path)
				w.WriteHeader(http.StatusNoContent)
			}
			ctx := context.Background()
			err := e.client.RemoveRunner(ctx, 9)
			if err == nil {
				t.Fatal("RemoveRunner succeeded")
			}
			t.Logf("error: %v", err)
			if got := errors.Is(err, retry.ErrAuth); got != tc.wantAuth {
				t.Errorf("errors.Is(err, ErrAuth) = %v, want %v", got, tc.wantAuth)
			}
			if errors.Is(err, retry.ErrRunnerGone) {
				t.Error("error is ErrRunnerGone")
			}
			if c := retry.Classify(ctx, retry.OpRemoveRunner, err); c != tc.wantClass {
				t.Errorf("Classify = %v, want %v", c, tc.wantClass)
			}
			if tc.retried != "" {
				if n := e.gh.count(http.MethodPost, tc.retried); n < 2 {
					t.Errorf("POST %s requests = %d, want more than one", tc.retried, n)
				}
			}
			e.assertNoSecrets(t, "error", err.Error())
		})
	}
}

// TestAuthExchangeFailureInsideSessionCall checks that a refused token
// exchange during the session refresh inside GetMessage is permanent, not a
// lost session.
func TestAuthExchangeFailureInsideSessionCall(t *testing.T) {
	s := newFakeSession(t)
	// Every Actions service call renews the admin token first.
	s.e.gh.mu.Lock()
	s.e.gh.shortAdminToken = true
	s.e.gh.mu.Unlock()
	ctx := context.Background()
	sess, err := s.e.client.CreateSession(ctx, testScaleSetID, "host")
	if err != nil {
		t.Fatal(err)
	}
	// The queue token is rejected, so GetMessage refreshes the session,
	// whose admin token renewal is refused.
	s.set(func(s *fakeSession) { s.token = "rotated-away" })
	s.e.gh.mu.Lock()
	s.e.gh.registrationToken = func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusNotFound, map[string]string{"message": "Not Found"})
	}
	s.e.gh.mu.Unlock()
	_, err = sess.GetMessage(ctx, 0, 2)
	if err == nil {
		t.Fatal("GetMessage succeeded")
	}
	if !errors.Is(err, retry.ErrAuth) {
		t.Errorf("error %v is not ErrAuth", err)
	}
	if c := retry.Classify(ctx, retry.OpGetMessage, err); c != retry.Permanent {
		t.Errorf("Classify = %v, want permanent (err %v)", c, err)
	}
}

func TestStatusRecorderRecordsSpentQuota(t *testing.T) {
	reset := time.Unix(1_800_000_000, 0)
	tests := []struct {
		name      string
		status    int
		header    map[string]string
		wantSpent bool
		wantReset time.Time
	}{
		{"spent with reset", 403, map[string]string{"X-RateLimit-Remaining": "0", "X-RateLimit-Reset": "1800000000"}, true, reset},
		{"spent without reset", 429, map[string]string{"X-RateLimit-Remaining": "0"}, true, time.Time{}},
		{"spent with bad reset", 403, map[string]string{"X-RateLimit-Remaining": "0", "X-RateLimit-Reset": "soon"}, true, time.Time{}},
		{"spent with zero reset", 403, map[string]string{"X-RateLimit-Remaining": "0", "X-RateLimit-Reset": "0"}, true, time.Time{}},
		{"quota left", 403, map[string]string{"X-RateLimit-Remaining": "12", "X-RateLimit-Reset": "1800000000"}, false, time.Time{}},
		{"no headers", 429, nil, false, time.Time{}},
		{"success is not recorded", 200, map[string]string{"X-RateLimit-Remaining": "0", "X-RateLimit-Reset": "1800000000"}, false, time.Time{}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rec := &statusRecorder{next: roundTripFunc(func(*http.Request) (*http.Response, error) {
				h := make(http.Header)
				for k, v := range tc.header {
					h.Set(k, v)
				}
				return &http.Response{StatusCode: tc.status, Header: h, Body: http.NoBody}, nil
			})}
			ctx, slot := withStatusSlot(context.Background())
			req, _ := http.NewRequestWithContext(ctx, http.MethodDelete, "https://example.invalid/agents/9", nil)
			resp, err := rec.RoundTrip(req)
			if err != nil {
				t.Fatal(err)
			}
			_ = resp.Body.Close()
			got := slot.failure()
			if got.quotaSpent != tc.wantSpent || !got.reset.Equal(tc.wantReset) {
				t.Errorf("slot = %+v, want quotaSpent %v, reset %v", got, tc.wantSpent, tc.wantReset)
			}
		})
	}
}

func TestStatusSlotsAreIndependent(t *testing.T) {
	rec := &statusRecorder{next: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		code, _ := strconv.Atoi(r.URL.Query().Get("code"))
		return &http.Response{StatusCode: code, Body: http.NoBody}, nil
	})}
	var wg sync.WaitGroup
	for code := 400; code < 450; code++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, slot := withStatusSlot(context.Background())
			req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://example.invalid/?code="+strconv.Itoa(code), nil)
			resp, err := rec.RoundTrip(req)
			if err == nil {
				_ = resp.Body.Close()
			}
			if got := slot.failure().status; got != code {
				t.Errorf("slot = %d, want %d", got, code)
			}
		}()
	}
	wg.Wait()
}

func TestStatusErrorText(t *testing.T) {
	err := &StatusError{Code: 403, Message: "Resource not accessible by integration"}
	if got, want := err.Error(), "HTTP 403 Forbidden: Resource not accessible by integration"; got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
	if err.HTTPStatus() != 403 {
		t.Errorf("HTTPStatus = %d", err.HTTPStatus())
	}
	if got := (&StatusError{Code: 599}).Error(); got != "HTTP 599" {
		t.Errorf("Error() = %q", got)
	}
}

func TestStatusOfText(t *testing.T) {
	for text, want := range map[string]int{
		`failed(status="404 Not Found")`:  404,
		`(status="503 Service")`:          503,
		`status=404`:                      0,
		`no status here`:                  0,
		`status="abc"`:                    0,
		"status=\"429 x\" status=\"500\"": 429,
	} {
		got, _ := statusOfText(text)
		if got != want {
			t.Errorf("statusOfText(%q) = %d, want %d", text, got, want)
		}
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// chainTexts returns the text of err and of every error in its chain.
func chainTexts(err error) []string {
	var texts []string
	var walk func(error)
	walk = func(e error) {
		if e == nil {
			return
		}
		texts = append(texts, e.Error())
		switch u := e.(type) {
		case interface{ Unwrap() error }:
			walk(u.Unwrap())
		case interface{ Unwrap() []error }:
			for _, inner := range u.Unwrap() {
				walk(inner)
			}
		}
	}
	walk(err)
	return texts
}

// TestRedactedErrorChainHoldsNoBody checks that no error in a converted
// error's chain repeats the response body that the top-level text leaves
// out: the withheld body of an unusable 200, and the part of a long error
// body past the cut.
func TestRedactedErrorChainHoldsNoBody(t *testing.T) {
	const marker = "BODY-TAIL-MARKER"
	tests := []struct {
		name    string
		respond func(w http.ResponseWriter)
		absent  []string
	}{
		{"unusable 200", func(w http.ResponseWriter) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"runner":{"id":5},"encodedJITConfig":"` + fakeJIT + `","x":"` + marker + `"`))
		}, []string{fakeJIT, marker}},
		{"long 422 body", func(w http.ResponseWriter) {
			w.Header().Set("Content-Type", "text/plain")
			w.WriteHeader(http.StatusUnprocessableEntity)
			_, _ = w.Write([]byte(strings.Repeat("filler ", 200) + marker))
		}, []string{marker}},
		{"long 403 rate limit body", func(w http.ResponseWriter) {
			w.Header().Set("Content-Type", "text/plain")
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte("API rate limit exceeded. " + strings.Repeat("filler ", 200) + marker))
		}, []string{marker}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			e := newTestEnv(t)
			e.gh.actions = func(w http.ResponseWriter, r *http.Request) { tc.respond(w) }
			ctx := context.Background()
			_, err := e.client.GenerateJIT(ctx, testScaleSetID, "pool-0a1b2c3d")
			if err == nil {
				t.Fatal("GenerateJIT succeeded")
			}
			texts := chainTexts(err)
			if len(texts) < 2 {
				t.Fatalf("chain of %v has %d errors", err, len(texts))
			}
			for _, text := range texts {
				for _, s := range tc.absent {
					if strings.Contains(text, s) {
						t.Errorf("error in chain holds %q: %s", s, text)
					}
				}
			}
		})
	}
}

// TestRedactedErrorKeepsContextErrors checks that keptCause leaves the
// context errors reachable and nothing else.
func TestRedactedErrorKeepsContextErrors(t *testing.T) {
	secrets := logging.NewSecrets()
	body := errors.New(`failed(status="422 Unprocessable Entity"): secret body`)
	for _, tc := range []struct {
		name string
		err  error
		is   []error
	}{
		{"status with deadline", errors.Join(body, context.DeadlineExceeded), []error{context.DeadlineExceeded}},
		{"no status with cancel", errors.Join(errors.New("read: secret body"), context.Canceled), []error{context.Canceled}},
		{"status only", body, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := convertError(retry.OpRemoveRunner, tc.err, lastFailure{}, secrets)
			for _, target := range tc.is {
				if !errors.Is(err, target) {
					t.Errorf("error %v lost %v", err, target)
				}
			}
			if errors.Is(err, body) {
				t.Error("original error is still in the chain")
			}
		})
	}
}

// TestStatusRecorderKeepsPeekedBody checks that the recorder reads a 403's
// message for the rate limit test and leaves the whole body for the
// library.
func TestStatusRecorderKeepsPeekedBody(t *testing.T) {
	tests := []struct {
		name, contentType, body string
		wantRateLimit           bool
	}{
		{"JSON rate limit", "application/json", `{"message":"You have exceeded a secondary rate limit"}`, true},
		{"JSON other", "application/json", `{"message":"Resource not accessible by integration"}`, false},
		{"plain text rate limit", "text/plain; charset=utf-8", "API rate limit exceeded", true},
		{"HTML", "text/html", "<p>rate limit</p>", false},
		{"longer than the peek", "application/json", `{"message":"rate limit","pad":"` + strings.Repeat("x", maxErrorBodyBytes) + `"}`, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			at := time.Unix(1_800_000_000, 0)
			rec := &statusRecorder{
				next: roundTripFunc(func(*http.Request) (*http.Response, error) {
					h := http.Header{"Content-Type": {tc.contentType}, "Retry-After": {"7"}}
					return &http.Response{StatusCode: 403, Header: h, Body: io.NopCloser(strings.NewReader(tc.body))}, nil
				}),
				now: func() time.Time { return at },
			}
			ctx, slot := withStatusSlot(context.Background())
			req, _ := http.NewRequestWithContext(ctx, http.MethodDelete, "https://example.invalid/agents/9", nil)
			resp, err := rec.RoundTrip(req)
			if err != nil {
				t.Fatal(err)
			}
			got, err := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if err != nil || string(got) != tc.body {
				t.Errorf("body after peek = %d bytes (err %v), want the %d bytes sent", len(got), err, len(tc.body))
			}
			f := slot.failure()
			if f.rateLimitMessage != tc.wantRateLimit {
				t.Errorf("rateLimitMessage = %v, want %v", f.rateLimitMessage, tc.wantRateLimit)
			}
			if f.retryAfter != "7" || !f.at.Equal(at) {
				t.Errorf("slot = %+v, want retryAfter 7 at %v", f, at)
			}
		})
	}
}
