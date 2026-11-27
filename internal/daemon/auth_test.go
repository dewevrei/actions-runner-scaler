package daemon

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dewevrei/actions-runner-scaler/internal/config"
	"github.com/dewevrei/actions-runner-scaler/internal/github"
	"github.com/dewevrei/actions-runner-scaler/internal/logging"
	"github.com/dewevrei/actions-runner-scaler/internal/retry"
)

// fixtureToken returns token n, built at run time so no token-shaped
// literal is committed. It has no GitHub token prefix, so no format rule
// of the logger hides it: a test that finds it nowhere proves it was never
// written.
func fixtureToken(n int) string {
	return fmt.Sprintf("fixture%02d", n) + strings.Repeat("Zq7x", 9)
}

// refusedCheck returns the error the real token check gives for an answer
// with status code on repository acme/a (0003 SPEC R279).
func refusedCheck(t *testing.T, code int) error {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_, _ = io.WriteString(w, `{"message":"refused"}`)
	}))
	defer srv.Close()
	rest, err := github.NewTokenREST(github.TokenRESTConfig{
		BaseURL: srv.URL, Token: fixtureToken(99), HTTPClient: srv.Client(),
		Clock: newFakeClock(), Secrets: logging.NewSecrets(),
	})
	if err != nil {
		t.Fatal(err)
	}
	target, err := github.ParseTargetURL("https://github.com/acme/a")
	if err != nil {
		t.Fatal(err)
	}
	_, err = rest.CheckRepositoryAccess(context.Background(), target)
	if err == nil {
		t.Fatalf("token check with status %d succeeded", code)
	}
	return err
}

// assertNoToken fails when token appears in the daemon's logs or status.
func (h *harness) assertNoToken(token string) {
	h.t.Helper()
	if strings.Contains(h.logs.String(), token) {
		h.t.Error("a log line holds the token")
	}
	if strings.Contains(fmt.Sprintf("%+v", h.report()), token) {
		h.t.Error("the status report holds the token")
	}
}

// targetStatus returns the status line of the target with url.
func (h *harness) targetStatus(url string) string {
	h.t.Helper()
	for _, tg := range h.report().Targets {
		if tg.URL == url {
			return tg.LastError
		}
	}
	h.t.Fatalf("no status line for %s", url)
	return ""
}

// TestTokenTargetStart proves 0003 SPEC R278 and R273: a token target makes
// one token check and no installation lookup, then its clients, the
// cleanup, the scale set and the session; the App target beside it looks
// its installation up as before.
func TestTokenTargetStart(t *testing.T) {
	h := newHarness(t)
	fakes := h.useTargets("a", "b")
	token := fixtureToken(1)
	h.useTokens(token, "")
	h.start()
	fakes[0].polled(t, 0)
	fakes[1].polled(t, 0)

	log := h.calls.snapshot()
	last := -1
	for _, want := range []string{
		"github.CheckRepositoryAccess a", "github.TokenTarget a", "a.FindScaleSet 1 pool",
		"a.GetOrCreateScaleSet 1 pool", "a.SetSystemInfo", "a.CreateSession 7 host",
	} {
		i := indexOf(log, want)
		if i < 0 || i < last {
			t.Fatalf("call %q at %d after %d; calls:\n%s", want, i, last, strings.Join(log, "\n"))
		}
		last = i
	}
	for call, want := range map[string]int{
		"github.CheckRepositoryAccess a": 1, "github.Installation a": 0, "github.Target a": 0,
		"github.Installation b": 1, "github.Target b 99": 1, "github.CheckRepositoryAccess b": 0, "github.TokenTarget b": 0,
	} {
		if n := h.calls.count(call); n != want {
			t.Errorf("%s: %d calls, want %d", call, n, want)
		}
	}
	trs := h.targetRuns()
	if trs[0].inst.Load() != h.d.restWaits.get(tokenKey(token)) {
		t.Error("a is not bound to its token's REST waits")
	}
	if trs[1].inst.Load() != h.d.restWaits.get(installationKey(testInstallationID)) {
		t.Error("b is not bound to its installation's REST waits")
	}
	h.assertNoToken(token)
	h.exit()
}

// TestTokenTargetRefused proves 0003 SPEC R279 and R172: a 401, 403 or 404
// answer to the token check puts only that target in the target error wait
// with R279's sentence alone as its last error; the App target runs.
func TestTokenTargetRefused(t *testing.T) {
	const cannot = "personal access token for repository acme/a cannot manage its runners (%d): a classic token needs the repo scope and admin access, a fine-grained token needs Administration: Read and write on this repository"
	tests := []struct {
		code int
		want string
	}{
		{401, "personal access token for repository acme/a was refused (401): it is invalid, expired or revoked"},
		{403, fmt.Sprintf(cannot, 403)},
		{404, fmt.Sprintf(cannot, 404)},
	}
	for _, tt := range tests {
		t.Run(fmt.Sprint(tt.code), func(t *testing.T) {
			h := newHarness(t)
			fakes := h.useTargets("a", "b")
			token := fixtureToken(1)
			h.useTokens(token, "")
			refused := refusedCheck(t, tt.code)
			fakes[0].accessHook = func(context.Context, int) error { return refused }
			h.start()
			fakes[1].polled(t, 0)
			trs := h.targetRuns()
			h.waitTargetState(trs[0], targetErrorWait)
			h.waitTargetState(trs[1], targetRunning)
			if last, _ := trs[0].rec.Last(); last != tt.want {
				t.Errorf("a's last error = %q, want %q", last, tt.want)
			}
			if got := h.targetStatus("https://github.com/acme/a"); got != tt.want {
				t.Errorf("a's status line error = %q, want %q", got, tt.want)
			}
			if n := fakes[0].tokenChecks(); n != 1 {
				t.Errorf("a's token checks = %d, want 1 (a refusal is not retried with R24)", n)
			}
			if n := h.calls.count("github.TokenTarget a"); n != 0 {
				t.Errorf("a's clients were made %d times", n)
			}
			// The log keeps the operation and status the last error leaves
			// out (0003 PLAN M30).
			if want := `"err":"` + string(retry.OpCheckTokenAccess) + ": HTTP " + fmt.Sprint(tt.code); !strings.Contains(h.logs.String(), want) {
				t.Errorf("no log line holds %q", want)
			}
			h.assertNoToken(token)
			h.exit()
		})
	}
}

// TestTokenNeverWrittenRealClients proves 0003 SPEC R277 with the real
// GitHub side: a token target's check answered by a server that echoes the
// Authorization header in its error message leaves the token in neither
// the logs, the target's last error nor the status report.
func TestTokenNeverWrittenRealClients(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = fmt.Fprintf(w, `{"message":"echo %s"}`, r.Header.Get("Authorization"))
	}))
	defer srv.Close()
	h := newHarness(t)
	h.useTargets("a")
	token := fixtureToken(1)
	h.useTokens(token)
	h.newGitHubFn = func() (GitHub, error) {
		return NewGitHub(GitHubConfig{
			Config: h.cfg, Credentials: h.creds, Clock: h.clock, Secrets: h.secrets,
			HTTPClient: srv.Client(), BaseURL: srv.URL,
		})
	}
	h.start()
	tr := h.targetRuns()[0]
	waitFor(t, "the echoed error recorded", func() bool {
		last, _ := tr.rec.Last()
		return strings.Contains(last, "echo Bearer ")
	})
	last, _ := tr.rec.Last()
	if strings.Contains(last, token) {
		t.Error("the last error holds the token")
	}
	// The server got the token and echoed it; only redaction hid it.
	if !strings.Contains(last, "echo Bearer "+logging.Placeholder) {
		t.Errorf("last error = %q, want the echoed token redacted", last)
	}
	h.assertNoToken(token)
	h.signal()
	if err := h.result(); err != nil {
		t.Fatal(err)
	}
}

// TestTokenClientsFailOnlyThatTarget proves 0002 SPEC R172 for a token
// target: when its clients cannot be made, only that target enters the
// target error wait, without a token check; the App target runs.
func TestTokenClientsFailOnlyThatTarget(t *testing.T) {
	h := newHarness(t)
	fakes := h.useTargets("a", "b")
	h.useTokens(fixtureToken(1), "")
	fakes[0].sourceErr = errors.New("cannot make the clients")
	h.start()
	fakes[1].polled(t, 0)
	trs := h.targetRuns()
	h.waitTargetState(trs[0], targetErrorWait)
	if last, _ := trs[0].rec.Last(); last != "github clients: cannot make the clients" {
		t.Errorf("a's last error = %q", last)
	}
	if n := fakes[0].tokenChecks(); n != 0 {
		t.Errorf("a's token checks = %d, want 0", n)
	}
	h.exit()
}

// TestTokenTargetTransientRetried proves 0003 SPEC R279 and R24: a 502 on
// the first start's token check is retried after R24's backoff, and the
// target then starts.
func TestTokenTargetTransientRetried(t *testing.T) {
	h := newHarness(t)
	fakes := h.useTargets("a")
	h.useTokens(fixtureToken(1))
	fakes[0].accessHook = func(_ context.Context, n int) error {
		if n == 1 {
			return opErr(retry.OpCheckTokenAccess, statusError(502))
		}
		return nil
	}
	h.start()
	h.clock.waitTimersAt(t, testEpoch.Add(time.Second), 1)
	if n := fakes[0].tokenChecks(); n != 1 {
		t.Fatalf("token checks before the backoff ended = %d, want 1", n)
	}
	h.clock.Advance(time.Second)
	fakes[0].polled(t, 0)
	if n := fakes[0].tokenChecks(); n != 2 {
		t.Errorf("token checks = %d, want 2", n)
	}
	h.exit()
}

// TestTokenCheckWaitIsTheTokens proves 0003 SPEC R280 and 0002 SPEC R189: a
// 429 on the token check is recorded in the token's waits, not in the
// installation lookup's shared wait, so the App target's lookup is made
// at once; the token target checks again when the 429's wait ended.
func TestTokenCheckWaitIsTheTokens(t *testing.T) {
	h := newHarness(t)
	fakes := h.useTargets("a", "b")
	token := fixtureToken(1)
	h.useTokens(token, "")
	const wait = 10 * time.Minute
	fakes[0].accessHook = func(_ context.Context, n int) error {
		if n == 1 {
			return rateLimited(retry.OpCheckTokenAccess, wait)
		}
		return nil
	}
	h.start()
	tokenWaits := h.d.restWaits.get(tokenKey(token))
	waitFor(t, "a's rate-limited token check", func() bool { return !tokenWaits.Allowed(retry.OpCheckTokenAccess) })
	if got, want := tokenWaits.At(retry.OpCheckTokenAccess), testEpoch.Add(wait); !got.Equal(want) {
		t.Errorf("token wait = %v, want %v", got, want)
	}
	for _, op := range []retry.Op{retry.OpGetInstallation, retry.OpCheckTokenAccess} {
		if !h.d.limits.Allowed(op) {
			t.Errorf("the shared installation lookup wait holds %s", op)
		}
	}
	fakes[1].polled(t, 0)
	if !h.clock.Now().Equal(testEpoch) {
		t.Fatalf("the clock moved to %v before b ran", h.clock.Now())
	}
	if trs := h.targetRuns(); trs[0].limitsOf(retry.OpCheckTokenAccess).At(retry.OpCheckTokenAccess) != tokenWaits.At(retry.OpCheckTokenAccess) {
		t.Error("the target error wait would not read the token's wait for the token check")
	}
	h.clock.waitTimersAt(t, testEpoch.Add(wait), 1)
	h.clock.Advance(wait)
	fakes[0].polled(t, 0)
	if n := fakes[0].tokenChecks(); n != 2 {
		t.Errorf("token checks = %d, want 2", n)
	}
	h.exit()
}

// credentialWaitRun starts targets a and b with the given tokens (empty:
// the App), both with a scale set; a's start-up R241 runner list answers
// 429 with Retry-After: 90 s. b's credential check is held until a's
// cleanup returned (a's scale set call). It returns the targets once both
// poll, and the number of b's runner lists.
func credentialWaitRun(t *testing.T, tokens [2]string) (*harness, []*targetRun, int) {
	t.Helper()
	h := newHarness(t)
	fakes := h.useTargets("a", "b")
	h.useTokens(tokens[0], tokens[1])
	fakes[0].hasSet, fakes[1].hasSet = true, true
	fakes[0].listErr = func(int) error { return rateLimited(retry.OpListRepoRunners, 90*time.Second) }
	aCleaned := make(chan struct{})
	fakes[0].setErr = func(n int) error {
		if n == 1 {
			close(aCleaned)
		}
		return nil
	}
	hold := func(ctx context.Context, n int) error {
		if n == 1 {
			select {
			case <-aCleaned:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		return nil
	}
	fakes[1].instHook, fakes[1].accessHook = hold, hold
	h.start()
	fakes[0].polled(t, 0)
	fakes[1].polled(t, 0)
	if n := h.calls.count("a.ListRunners"); n != 1 {
		t.Fatalf("a listed its runners %d times, want 1", n)
	}
	return h, h.targetRuns(), h.calls.count("b.ListRunners")
}

// TestCredentialWaitsSameToken proves 0003 SPEC R280: a REST rate limit on
// target A's cleanup holds back target B with an equal token, whose cleanup
// skips its runner list; both are bound to one entry.
func TestCredentialWaitsSameToken(t *testing.T) {
	token := fixtureToken(1)
	// An equal value built apart, so only the value can make them share.
	same := strings.Clone(token)
	h, trs, lists := credentialWaitRun(t, [2]string{token, same})
	if lists != 0 {
		t.Errorf("b listed its runners %d times during a's rate limit, want 0", lists)
	}
	bl := trs[1].rateLimits()
	want := testEpoch.Add(90 * time.Second)
	for _, op := range []retry.Op{retry.OpListRepoRunners, retry.OpFindRepoRunner} {
		if bl.Allowed(op) || !bl.At(op).Equal(want) {
			t.Errorf("b's %s: allowed %v, at %v; want delayed until %v", op, bl.Allowed(op), bl.At(op), want)
		}
	}
	if trs[0].inst.Load() == nil || trs[0].inst.Load() != trs[1].inst.Load() {
		t.Error("targets of one token have different REST waits")
	}
	if !trs[1].limits.Allowed(retry.OpListRepoRunners) {
		t.Error("a's rate limit went to b's own waits, not the token's")
	}
	h.assertNoToken(token)
	h.exit()
}

// TestCredentialWaitsSeparate proves 0003 SPEC R280: a REST rate limit of
// target A's credential leaves target B of another token, or of the App,
// allowed; B's cleanup lists its runners.
func TestCredentialWaitsSeparate(t *testing.T) {
	tests := []struct {
		name   string
		tokens [2]string
	}{
		{"two tokens", [2]string{fixtureToken(1), fixtureToken(2)}},
		{"token beside App", [2]string{fixtureToken(1), ""}},
		{"App beside token", [2]string{"", fixtureToken(2)}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h, trs, lists := credentialWaitRun(t, tt.tokens)
			if lists != 1 {
				t.Errorf("b listed its runners %d times, want 1", lists)
			}
			if trs[0].rateLimits().Allowed(retry.OpListRepoRunners) {
				t.Fatal("a's rate limit was not recorded")
			}
			bl := trs[1].rateLimits()
			for _, op := range []retry.Op{retry.OpListRepoRunners, retry.OpFindRepoRunner} {
				if !bl.Allowed(op) {
					t.Errorf("b's %s delayed by another credential's rate limit", op)
				}
			}
			if trs[0].inst.Load() == trs[1].inst.Load() {
				t.Error("targets of different credentials share REST waits")
			}
			h.exit()
		})
	}
}

// TestCredentialWaitsConcurrent proves 0003 SPEC R280 under -race: 8
// goroutines get the waits of two tokens and two installations at once;
// equal keys give equal pointers, the four keys four entries.
func TestCredentialWaitsConcurrent(t *testing.T) {
	w, err := newCredentialWaits(newFakeClock())
	if err != nil {
		t.Fatal(err)
	}
	t1, t2 := fixtureToken(1), fixtureToken(2)
	const n = 8
	got := make([][4]*retry.NextAllowed, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() {
			got[i] = [4]*retry.NextAllowed{
				w.get(tokenKey(strings.Clone(t1))), w.get(tokenKey(t2)),
				w.get(installationKey(1)), w.get(installationKey(2)),
			}
		})
	}
	wg.Wait()
	for i, g := range got {
		if g != got[0] {
			t.Fatalf("goroutine %d got other waits for the same keys", i)
		}
	}
	seen := map[*retry.NextAllowed]bool{}
	for _, p := range got[0] {
		seen[p] = true
	}
	if len(seen) != 4 {
		t.Errorf("four keys share %d entries, want 4", len(seen))
	}
}

// TestRetryTokenTargetRunsAccessCheck proves 0003 SPEC R279 and 0002 SPEC
// R193: each retry of a token target's error wait makes the token check,
// never an installation lookup; a refused retry shows R279's sentence
// alone, and the next retry that passes runs the target again.
func TestRetryTokenTargetRunsAccessCheck(t *testing.T) {
	h := newHarness(t)
	fakes := h.useTargets("a")
	token := fixtureToken(1)
	h.useTokens(token)
	h.start()
	sa := fakes[0].polled(t, 0)
	tr := h.targetRuns()[0]

	refused := refusedCheck(t, 401)
	fakes[0].setAccessHook(func(_ context.Context, n int) error {
		if n == 2 {
			return refused
		}
		return nil
	})
	sa.send(t, nil, permanentPoll())
	at := h.waitTokenRetry(tr, fakes[0], 1, 30*time.Second)
	h.clock.Advance(at.Sub(h.clock.Now()))
	at = h.waitTokenRetry(tr, fakes[0], 2, time.Minute)
	const want = "personal access token for repository acme/a was refused (401): it is invalid, expired or revoked"
	if last, _ := tr.rec.Last(); last != want {
		t.Errorf("last error after the refused retry = %q, want %q", last, want)
	}
	h.clock.Advance(at.Sub(h.clock.Now()))
	fakes[0].polled(t, 1)
	h.waitTargetState(tr, targetRunning)
	if n := fakes[0].tokenChecks(); n != 3 {
		t.Errorf("token checks = %d, want 3", n)
	}
	if n := h.calls.count("github.Installation"); n != 0 {
		t.Errorf("installation lookups = %d, want 0", n)
	}
	h.assertNoToken(token)
	h.exit()
}

// waitTokenRetry waits until tr's next retry is due in after from now,
// after n token checks of gh, and returns its time.
func (h *harness) waitTokenRetry(tr *targetRun, gh *fakeGitHub, n int, after time.Duration) time.Time {
	h.t.Helper()
	waitFor(h.t, "a retry in "+after.String(), func() bool {
		at := tr.retryAt()
		return gh.tokenChecks() == n && !at.IsZero() && at.Sub(h.clock.Now()) == after
	})
	return tr.retryAt()
}

// TestCredentialKeyPrintsNoToken proves 0003 SPEC R277 and R280: no fmt
// verb prints a token from its key, a target's limits, a token target's
// credential or the real GitHub side.
func TestCredentialKeyPrintsNoToken(t *testing.T) {
	token := fixtureToken(1)
	key := tokenKey(token)
	w, err := newCredentialWaits(newFakeClock())
	if err != nil {
		t.Fatal(err)
	}
	own, err := retry.NewNextAllowed(newFakeClock())
	if err != nil {
		t.Fatal(err)
	}
	target, err := github.ParseTargetURL("https://github.com/acme/a")
	if err != nil {
		t.Fatal(err)
	}
	cfg := testConfig()
	cfg.GitHub = config.GitHub{TokenFile: "/token"}
	g, err := NewGitHub(GitHubConfig{
		Config:      cfg,
		Credentials: config.Credentials{DefaultToken: token, TargetTokens: []string{""}},
		Clock:       newFakeClock(),
		Secrets:     logging.NewSecrets(),
	})
	if err != nil {
		t.Fatal(err)
	}
	auth, err := g.Auth(target, 0)
	if err != nil {
		t.Fatal(err)
	}
	for name, v := range map[string]any{
		"credentialKey": key,
		"targetLimits":  pinnedLimits(own, w.get(key)),
		"tokenAuth":     auth,
		"GitHub":        g,
	} {
		for _, verb := range []string{"%v", "%+v", "%#v", "%s", "%x", "%q"} {
			if strings.Contains(fmt.Sprintf(verb, v), token) {
				t.Errorf("%s printed with %s shows the token", name, verb)
			}
		}
	}
}

// TestNewGitHubTokenOnly proves 0003 SPEC R272, R274 and R278: credentials
// without an App key make no App signer and serve a token target, whose
// check sends the token as a Bearer credential and returns the token's
// key; a target with no credential at all is refused.
func TestNewGitHubTokenOnly(t *testing.T) {
	token := fixtureToken(1)
	var (
		mu   sync.Mutex
		auth []string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		auth = append(auth, r.Method+" "+r.URL.Path+" "+r.Header.Get("Authorization"))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"total_count":0,"runners":[]}`)
	}))
	defer srv.Close()
	cfg := testConfig()
	cfg.GitHub = config.GitHub{TokenFile: "/token"}
	secrets := logging.NewSecrets()
	g, err := NewGitHub(GitHubConfig{
		Config:      cfg,
		Credentials: config.Credentials{DefaultToken: token, TargetTokens: []string{""}},
		Clock:       newFakeClock(),
		Secrets:     secrets,
		HTTPClient:  srv.Client(),
		BaseURL:     srv.URL,
	})
	if err != nil {
		t.Fatalf("NewGitHub = %v", err)
	}
	if rg := g.(*realGitHub); slices.ContainsFunc(rg.apps, func(a *appClients) bool { return a != nil }) {
		t.Error("an App signer was made without an App key")
	}
	if strings.Contains(secrets.Redact(token), token) {
		t.Error("the token is not registered in Secrets")
	}
	target, err := github.ParseTargetURL(testTargetURL)
	if err != nil {
		t.Fatal(err)
	}
	a, err := g.Auth(target, 0)
	if err != nil {
		t.Fatalf("Auth = %v", err)
	}
	if _, ok := a.(*tokenAuth); !ok {
		t.Fatalf("Auth = %T, want *tokenAuth", a)
	}
	clock := newFakeClock()
	waits, err := newCredentialWaits(clock)
	if err != nil {
		t.Fatal(err)
	}
	direct := func(ctx context.Context, _ callScope, _ string, _ retry.Op, fn func(context.Context) error) error {
		return fn(ctx)
	}
	sc := callScope{fail: func(where string, err error) error { return err }}
	key, err := a.check(context.Background(), direct, sc, authWaits{credentials: waits})
	if err != nil {
		t.Fatalf("check = %v", err)
	}
	if key != tokenKey(token) {
		t.Error("check returned another key than the token's")
	}
	mu.Lock()
	got := strings.Join(auth, "\n")
	mu.Unlock()
	if want := "GET /repos/acme/widget/actions/runners Bearer " + token; got != want {
		t.Errorf("requests = %q, want one token check with the token as Bearer", got)
	}
	if gh, err := a.clients(key); err != nil || gh == nil {
		t.Errorf("clients = %v, %v", gh, err)
	}

	cfg.Targets = append(cfg.Targets, cfg.Targets[0])
	cfg.Targets[1].URL = "https://github.com/acme/other"
	if _, err := NewGitHub(GitHubConfig{
		Config:      cfg,
		Credentials: config.Credentials{TargetTokens: []string{token, ""}},
		Clock:       newFakeClock(),
		Secrets:     logging.NewSecrets(),
	}); err == nil || strings.Contains(err.Error(), token) {
		t.Errorf("NewGitHub of a target without credential = %v, want an error without the token", err)
	}
}

// appServer is a fake REST API. It answers each repository's installation
// lookup with the ID in ids, recording the App JWT's issuer, and every
// runner list with an empty one, recording the token.
type appServer struct {
	srv *httptest.Server

	mu      sync.Mutex
	issuers map[string]string
	tokens  map[string]string
	mints   []string
}

func newAppServer(t *testing.T, ids map[string]int64) *appServer {
	t.Helper()
	a := &appServer{issuers: map[string]string{}, tokens: map[string]string{}}
	a.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		a.mu.Lock()
		defer a.mu.Unlock()
		bearer := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		path := strings.TrimPrefix(r.URL.Path, "/repos/")
		w.Header().Set("Content-Type", "application/json")
		if repo, ok := strings.CutSuffix(path, "/actions/runners"); ok {
			a.tokens[repo] = bearer
			_, _ = io.WriteString(w, `{"total_count":0,"runners":[]}`)
			return
		}
		if inst, ok := strings.CutPrefix(r.URL.Path, "/app/installations/"); ok && r.Method == http.MethodPost {
			id := strings.TrimSuffix(inst, "/access_tokens")
			a.mints = append(a.mints, id+" "+jwtIssuer(t, bearer))
			w.WriteHeader(http.StatusCreated)
			_, _ = fmt.Fprintf(w, `{"token":"inst-%s","expires_at":%q}`, id, testEpoch.Add(time.Hour).Format(time.RFC3339))
			return
		}
		repo, ok := strings.CutSuffix(path, "/installation")
		id, known := ids[repo]
		if !ok || !known {
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"message":"Not Found"}`)
			return
		}
		a.issuers[repo] = jwtIssuer(t, bearer)
		_, _ = fmt.Fprintf(w, `{"id":%d,"permissions":{"administration":"write"}}`, id)
	}))
	t.Cleanup(a.srv.Close)
	return a
}

// seen returns the issuer of each installation lookup and the token of each
// runner list, by repository.
func (a *appServer) seen() (issuers, tokens map[string]string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return maps.Clone(a.issuers), maps.Clone(a.tokens)
}

// minted returns each installation token request as "<installation ID>
// <issuer of its JWT>", in order.
func (a *appServer) minted() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return slices.Clone(a.mints)
}

// jwtIssuer returns the iss claim of an App JWT.
func jwtIssuer(t *testing.T, jwt string) string {
	t.Helper()
	parts := strings.Split(jwt, ".")
	if len(parts) != 3 {
		t.Error("the installation lookup does not carry a JWT")
		return ""
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Errorf("JWT payload: %v", err)
		return ""
	}
	var claims struct {
		Issuer string `json:"iss"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		t.Errorf("JWT claims: %v", err)
	}
	return claims.Issuer
}

func newAppKey(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 1024)
	if err != nil {
		t.Fatal(err)
	}
	return key
}

// appTargets returns a configuration without [github] credential whose
// targets are repos, each with the App client ID clientIDs holds for it.
func appTargets(repos []string, clientIDs map[string]string) *config.Config {
	cfg := testConfig()
	cfg.GitHub = config.GitHub{}
	cfg.Targets = nil
	for _, repo := range repos {
		tc := config.DefaultTarget()
		tc.URL = "https://github.com/" + repo
		if id := clientIDs[repo]; id != "" {
			tc.AppClientID, tc.AppPrivateKeyFile = id, "/"+id+".pem"
		}
		cfg.Targets = append(cfg.Targets, tc)
	}
	return cfg
}

// checkAll runs the credential check of every target of cfg on g, with no
// wait in the way, makes its clients and returns the checks' keys.
func checkAll(t *testing.T, g GitHub, cfg *config.Config) ([]targetAuth, []credentialKey, []TargetGitHub) {
	t.Helper()
	waits, err := newCredentialWaits(newFakeClock())
	if err != nil {
		t.Fatal(err)
	}
	direct := func(ctx context.Context, _ callScope, _ string, _ retry.Op, fn func(context.Context) error) error {
		return fn(ctx)
	}
	sc := callScope{fail: func(where string, err error) error { return err }}
	auths := make([]targetAuth, len(cfg.Targets))
	keys := make([]credentialKey, len(cfg.Targets))
	clients := make([]TargetGitHub, len(cfg.Targets))
	for i, tc := range cfg.Targets {
		target, err := github.ParseTargetURL(tc.URL)
		if err != nil {
			t.Fatal(err)
		}
		if auths[i], err = g.Auth(target, i); err != nil {
			t.Fatalf("Auth %d = %v", i, err)
		}
		if keys[i], err = auths[i].check(context.Background(), direct, sc, authWaits{credentials: waits}); err != nil {
			t.Fatalf("check %d = %v", i, err)
		}
		if clients[i], err = auths[i].clients(keys[i]); err != nil || clients[i] == nil {
			t.Fatalf("clients %d = %v, %v", i, clients[i], err)
		}
	}
	return auths, keys, clients
}

// TestNewGitHubTargetAppBesideDefaultToken proves that a target with its
// own App uses that App, with its own JWT and installation lookup, while
// the target without a credential of its own uses the default token and
// looks nothing up.
func TestNewGitHubTargetAppBesideDefaultToken(t *testing.T) {
	token := fixtureToken(1)
	srv := newAppServer(t, map[string]int64{"acme/own": 31})
	cfg := appTargets([]string{"acme/widget", "acme/own"}, map[string]string{"acme/own": "own-app"})
	cfg.GitHub = config.GitHub{TokenFile: "/token"}
	g, err := NewGitHub(GitHubConfig{
		Config: cfg,
		Credentials: config.Credentials{
			DefaultToken:  token,
			TargetTokens:  []string{"", ""},
			TargetAppKeys: []*rsa.PrivateKey{nil, newAppKey(t)},
		},
		Clock:      newFakeClock(),
		Secrets:    logging.NewSecrets(),
		HTTPClient: srv.srv.Client(),
		BaseURL:    srv.srv.URL,
	})
	if err != nil {
		t.Fatalf("NewGitHub = %v", err)
	}
	auths, keys, _ := checkAll(t, g, cfg)
	if _, ok := auths[0].(*tokenAuth); !ok || keys[0] != tokenKey(token) {
		t.Errorf("target 0 = %T, want the default token's", auths[0])
	}
	if _, ok := auths[1].(appAuth); !ok || keys[1] != installationKey(31) {
		t.Errorf("target 1 = %T, want its own App's installation 31", auths[1])
	}
	issuers, tokens := srv.seen()
	if want := map[string]string{"acme/own": "own-app"}; !maps.Equal(issuers, want) {
		t.Errorf("installation lookups by issuer = %v, want %v", issuers, want)
	}
	if len(tokens) != 1 || tokens["acme/widget"] != token {
		t.Errorf("token checks on %d repositories, want one on acme/widget with the default token", len(tokens))
	}
}

// TestNewGitHubTargetAppsWithoutDefault proves that targets with their own
// Apps run with no [github] credential: each lookup carries its own App's
// JWT, and targets share REST waits exactly when they share an
// installation (0002 SPEC R266).
func TestNewGitHubTargetAppsWithoutDefault(t *testing.T) {
	srv := newAppServer(t, map[string]int64{"acme/a": 11, "acme/b": 22, "acme/c": 11})
	cfg := appTargets([]string{"acme/a", "acme/b", "acme/c"},
		map[string]string{"acme/a": "app-one", "acme/b": "app-two", "acme/c": "app-one"})
	one := newAppKey(t)
	g, err := NewGitHub(GitHubConfig{
		Config:      cfg,
		Credentials: config.Credentials{TargetTokens: []string{"", "", ""}, TargetAppKeys: []*rsa.PrivateKey{one, newAppKey(t), one}},
		Clock:       newFakeClock(),
		Secrets:     logging.NewSecrets(),
		HTTPClient:  srv.srv.Client(),
		BaseURL:     srv.srv.URL,
	})
	if err != nil {
		t.Fatalf("NewGitHub = %v", err)
	}
	_, keys, clients := checkAll(t, g, cfg)
	if keys[0] != installationKey(11) || keys[2] != keys[0] {
		t.Error("a and c are not both on installation 11's waits")
	}
	if keys[1] != installationKey(22) {
		t.Error("b is not on installation 22's waits")
	}
	issuers, tokens := srv.seen()
	want := map[string]string{"acme/a": "app-one", "acme/b": "app-two", "acme/c": "app-one"}
	if !maps.Equal(issuers, want) {
		t.Errorf("installation lookups by issuer = %v, want %v", issuers, want)
	}
	if len(tokens) != 0 {
		t.Errorf("token checks on %d repositories, want none", len(tokens))
	}

	// One REST call per target through its clients: each installation token
	// is bought with the JWT of the target's own App, once per installation
	// of that App, and each call carries its installation's token.
	for i, gh := range clients {
		if _, err := gh.ListRunners(context.Background()); err != nil {
			t.Fatalf("ListRunners %d = %v", i, err)
		}
	}
	if got, want := srv.minted(), []string{"11 app-one", "22 app-two"}; !slices.Equal(got, want) {
		t.Errorf("installation tokens bought (installation, issuer) = %q, want %q", got, want)
	}
	_, tokens = srv.seen()
	if want := map[string]string{"acme/a": "inst-11", "acme/b": "inst-22", "acme/c": "inst-11"}; !maps.Equal(tokens, want) {
		t.Errorf("runner lists by token = %v, want %v", tokens, want)
	}
	if g := g.(*realGitHub); g.apps[0] != g.apps[2] || g.apps[0] == g.apps[1] {
		t.Error("a and c do not share app-one's clients, or b shares them")
	}
}

// TestNewGitHubCredentialOrder proves the order in which a target picks its
// credential: its own token file, its own App, the default token file, the
// default App; and that the targets on the default App share its source.
func TestNewGitHubCredentialOrder(t *testing.T) {
	token, own := fixtureToken(1), fixtureToken(2)
	cfg := appTargets([]string{"acme/a", "acme/b", "acme/c", "acme/d"}, map[string]string{"acme/b": "own-app"})
	cfg.Targets[0].TokenFile = "/own-token"
	keys := []*rsa.PrivateKey{nil, newAppKey(t), nil, nil}
	newGitHub := func(creds config.Credentials) *realGitHub {
		t.Helper()
		g, err := NewGitHub(GitHubConfig{Config: cfg, Credentials: creds, Clock: newFakeClock(), Secrets: logging.NewSecrets()})
		if err != nil {
			t.Fatalf("NewGitHub = %v", err)
		}
		return g.(*realGitHub)
	}

	cfg.GitHub = config.GitHub{AppClientID: "default-app", AppPrivateKeyFile: "/key.pem"}
	g := newGitHub(config.Credentials{AppKey: newAppKey(t), TargetTokens: []string{own, "", "", ""}, TargetAppKeys: keys})
	if g.tokens[0] == nil || g.keys[0] != tokenKey(own) {
		t.Error("a: want its own token")
	}
	if g.apps[1] == nil || g.apps[1] == g.apps[2] {
		t.Error("b: want its own App, apart from the default")
	}
	if g.apps[2] == nil || g.apps[2] != g.apps[3] {
		t.Error("c and d: want the one default App")
	}

	cfg.GitHub = config.GitHub{TokenFile: "/token"}
	g = newGitHub(config.Credentials{DefaultToken: token, TargetTokens: []string{own, "", "", ""}, TargetAppKeys: keys})
	if g.apps[1] == nil || g.tokens[1] != nil {
		t.Error("b: want its own App over the default token")
	}
	for _, i := range []int{2, 3} {
		if g.tokens[i] == nil || g.keys[i] != tokenKey(token) {
			t.Errorf("target %d: want the default token", i)
		}
	}

	if _, err := NewGitHub(GitHubConfig{
		Config:      cfg,
		Credentials: config.Credentials{TargetAppKeys: keys[:2]},
		Clock:       newFakeClock(),
		Secrets:     logging.NewSecrets(),
	}); err == nil || !strings.Contains(err.Error(), "do not match") {
		t.Errorf("NewGitHub with misaligned App keys = %v, want the alignment error", err)
	}

	// A target naming its own App never falls back to the default.
	if _, err := NewGitHub(GitHubConfig{
		Config:      cfg,
		Credentials: config.Credentials{DefaultToken: token, TargetTokens: []string{own, "", "", ""}},
		Clock:       newFakeClock(),
		Secrets:     logging.NewSecrets(),
	}); err == nil || !strings.Contains(err.Error(), "targets[1]: the secret of targets.app-client-id") {
		t.Errorf("NewGitHub with an own App and no key = %v, want the targets[1] error", err)
	}
}

// TestNewGitHubTargetAppPrintsNoKey proves that no fmt verb prints a
// target's App key through the GitHub side NewGitHub returns.
func TestNewGitHubTargetAppPrintsNoKey(t *testing.T) {
	key := newAppKey(t)
	cfg := appTargets([]string{"acme/a"}, map[string]string{"acme/a": "own-app"})
	g, err := NewGitHub(GitHubConfig{
		Config:      cfg,
		Credentials: config.Credentials{TargetAppKeys: []*rsa.PrivateKey{key}},
		Clock:       newFakeClock(),
		Secrets:     logging.NewSecrets(),
	})
	if err != nil {
		t.Fatalf("NewGitHub = %v", err)
	}
	target, err := github.ParseTargetURL("https://github.com/acme/a")
	if err != nil {
		t.Fatal(err)
	}
	auth, err := g.Auth(target, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, verb := range []string{"%v", "%+v", "%#v"} {
		if s := fmt.Sprintf(verb, []any{*g.(*realGitHub), auth}); strings.Contains(s, key.D.String()) || strings.Contains(s, key.Primes[0].String()) {
			t.Errorf("printed with %s, the GitHub side shows the key", verb)
		}
	}
}

// TestAppInstallationWaits proves that the installation-lookup wait is one
// per App (0002 SPEC R189, per App): the default App's is the server's, a
// target's own App that is the default App shares it and the default App's
// clients, another App gets its own, and targets on one App share it.
func TestAppInstallationWaits(t *testing.T) {
	defaultKey, otherKey := newAppKey(t), newAppKey(t)
	// The same key read from two files.
	sameDefault := &rsa.PrivateKey{PublicKey: defaultKey.PublicKey, D: defaultKey.D, Primes: defaultKey.Primes}
	cfg := appTargets([]string{"acme/a", "acme/b", "acme/c", "acme/d"},
		map[string]string{"acme/b": "default-app", "acme/c": "other-app", "acme/d": "other-app"})
	cfg.GitHub = config.GitHub{AppClientID: "default-app", AppPrivateKeyFile: "/key.pem"}
	g, err := NewGitHub(GitHubConfig{
		Config: cfg,
		Credentials: config.Credentials{
			AppKey:        defaultKey,
			TargetAppKeys: []*rsa.PrivateKey{nil, sameDefault, otherKey, otherKey},
		},
		Clock:   newFakeClock(),
		Secrets: logging.NewSecrets(),
	})
	if err != nil {
		t.Fatalf("NewGitHub = %v", err)
	}
	rg := g.(*realGitHub)
	if rg.apps[0] != rg.apps[1] || rg.apps[2] != rg.apps[3] || rg.apps[0] == rg.apps[2] {
		t.Error("want one clients value for the default App (a, b) and one for other-app (c, d)")
	}

	server, err := retry.NewNextAllowed(newFakeClock())
	if err != nil {
		t.Fatal(err)
	}
	creds, err := newCredentialWaits(newFakeClock())
	if err != nil {
		t.Fatal(err)
	}
	w := authWaits{installation: server, credentials: creds}
	waits := make([]*retry.NextAllowed, len(cfg.Targets))
	for i, tc := range cfg.Targets {
		target, err := github.ParseTargetURL(tc.URL)
		if err != nil {
			t.Fatal(err)
		}
		auth, err := g.Auth(target, i)
		if err != nil {
			t.Fatal(err)
		}
		op, limits := auth.checkWait(w)
		if op != retry.OpGetInstallation {
			t.Errorf("target %d: op = %v, want the installation lookup", i, op)
		}
		waits[i] = limits
	}
	if waits[0] != server || waits[1] != server {
		t.Error("the default App's targets do not use the server's wait")
	}
	if waits[2] == server || waits[2] != waits[3] {
		t.Error("other-app's targets do not share a wait of their own")
	}
	if waits[2] == creds.get(tokenKey("other-app")) || waits[2] == creds.get(installationKey(1)) {
		t.Error("an App's wait is a token's or an installation's")
	}
}
