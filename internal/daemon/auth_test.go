package daemon

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
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
			h.assertNoToken(token)
			h.exit()
		})
	}
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
	if rg := g.(*realGitHub); rg.app != nil {
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
	if _, ok := a.(tokenAuth); !ok {
		t.Fatalf("Auth = %T, want tokenAuth", a)
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
