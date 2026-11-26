package doctor

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"github.com/dewevrei/actions-runner-scaler/internal/config"
	"github.com/dewevrei/actions-runner-scaler/internal/github"
	"github.com/dewevrei/actions-runner-scaler/internal/logging"
	"github.com/dewevrei/actions-runner-scaler/internal/retry"
)

// fineGrainedPrefix starts a fine-grained personal access token (0003 SPEC
// Design 자격).
const fineGrainedPrefix = "github_pat_"

// GitHubConfig configures the real GitHub clients.
type GitHubConfig struct {
	BaseURL    string
	HTTPClient github.HTTPDoer
	Clock      github.Clock
	// Secrets receives every token and App JWT before any request.
	Secrets *logging.Secrets
}

// NewGitHubClients returns GitHubClients over the github package. Its
// clients send GET requests only and never buy an installation token
// (0003 SPEC R299).
func NewGitHubClients(cfg GitHubConfig) GitHubClients {
	return githubClients{cfg: cfg}
}

type githubClients struct {
	cfg GitHubConfig
}

func (g githubClients) App(clientID, keyPath string) (AppProbe, error) {
	key, err := config.ReadAppPrivateKey(keyPath)
	if err != nil {
		return nil, err
	}
	signer, err := github.NewAppSigner(clientID, key, g.cfg.Clock, g.cfg.Secrets)
	if err != nil {
		return nil, err
	}
	return github.NewApp(github.AppConfig{
		BaseURL:    g.cfg.BaseURL,
		Signer:     signer,
		HTTPClient: g.cfg.HTTPClient,
		Clock:      g.cfg.Clock,
		Secrets:    g.cfg.Secrets,
	})
}

func (g githubClients) Token(path string) (TokenClient, error) {
	token, err := config.ReadTokenFile(path)
	if err != nil {
		return TokenClient{}, err
	}
	if g.cfg.Secrets == nil {
		return TokenClient{}, errors.New("doctor: nil secrets")
	}
	g.cfg.Secrets.Register(token)
	rest, err := github.NewTokenREST(github.TokenRESTConfig{
		BaseURL:    g.cfg.BaseURL,
		Token:      token,
		HTTPClient: g.cfg.HTTPClient,
		Clock:      g.cfg.Clock,
		Secrets:    g.cfg.Secrets,
	})
	if err != nil {
		return TokenClient{}, err
	}
	return TokenClient{Probe: rest, FineGrained: strings.HasPrefix(token, fineGrainedPrefix)}, nil
}

// targetPlan is how one target is checked: blocked by earlier checks, or
// with an App or a token file's client.
type targetPlan struct {
	name     string
	target   github.Target
	blockers []string
	// tokenKey is the token file key of a token target; empty for the App.
	tokenKey string
}

// targets makes the GitHub line of every target (0003 SPEC R300, R301). The
// clients are made one after another, so each file is read once, and the
// calls of different targets then run at the same time.
func (c *checker) targets(ctx context.Context, in *config.Inspection, files fileChecks) []Result {
	if in == nil {
		return []Result{blocked(githubCheck, []string{configCheck})}
	}
	plans := make([]targetPlan, len(in.Config.Targets))
	for i := range plans {
		plans[i] = planTarget(in, files, i)
	}

	var (
		app       AppProbe
		appErr    error
		appMade   bool
		tokens    = make(map[string]TokenClient)
		tokenErrs = make(map[string]error)
	)
	for _, p := range plans {
		switch {
		case len(p.blockers) > 0:
		case p.tokenKey == "" && !appMade:
			g := in.Config.GitHub
			app, appErr = c.GitHub.App(strings.TrimSpace(g.AppClientID), g.AppPrivateKeyFile)
			appMade = true
		case p.tokenKey != "":
			if _, ok := tokens[p.tokenKey]; ok || tokenErrs[p.tokenKey] != nil {
				continue
			}
			tc, err := c.GitHub.Token(tokenPath(in.Config, p.tokenKey))
			if err != nil {
				tokenErrs[p.tokenKey] = err
				continue
			}
			tokens[p.tokenKey] = tc
		}
	}

	return parallel(len(plans), func(i int) Result {
		p := plans[i]
		switch {
		case len(p.blockers) > 0:
			return blocked(p.name, p.blockers)
		case p.tokenKey == "" && appErr != nil:
			return clientFailed(p.name, appErr)
		case p.tokenKey == "":
			return c.appTarget(ctx, p.name, app, p.target)
		case tokenErrs[p.tokenKey] != nil:
			return clientFailed(p.name, tokenErrs[p.tokenKey])
		}
		return c.tokenTarget(ctx, p.name, tokens[p.tokenKey], p.target)
	})
}

// planTarget names target i and finds its credential (0003 SPEC R272,
// R273) and the earlier checks it depends on. A url refused by validation
// is never shown; the line is named by index (0003 SPEC R314).
func planTarget(in *config.Inspection, files fileChecks, i int) targetPlan {
	tc := in.Config.Targets[i]
	p := targetPlan{name: fmt.Sprintf("%s targets[%d]", githubCheck, i)}
	urlKey := fmt.Sprintf("targets[%d].url", i)
	if t, err := github.ParseTargetURL(tc.URL); err == nil && !in.Failed(urlKey) {
		p.name = githubCheck + " " + t.FullName()
		p.target = t
	} else {
		p.blockers = append(p.blockers, configCheck)
	}

	g := in.Config.GitHub
	var cred []string
	switch own := targetTokenKey(i); {
	case tc.TokenFile != "" || in.Failed(own):
		p.tokenKey = own
		cred = files.blockers(own)
	case in.Failed("github"):
		// No default credential, or both kinds at once (R272, R274).
		cred = []string{configCheck}
	case g.TokenFile != "" || in.Failed(defaultTokenKey):
		p.tokenKey = defaultTokenKey
		cred = files.blockers(defaultTokenKey)
	default:
		if in.Failed("github.app-client-id") || strings.TrimSpace(g.AppClientID) == "" {
			cred = append(cred, configCheck)
		}
		cred = append(cred, files.blockers(appKeyKey)...)
	}
	for _, b := range cred {
		if !slices.Contains(p.blockers, b) {
			p.blockers = append(p.blockers, b)
		}
	}
	return p
}

// tokenPath returns the path the token file key holds.
func tokenPath(cfg *config.Config, key string) string {
	if key == defaultTokenKey {
		return cfg.GitHub.TokenFile
	}
	for i, t := range cfg.Targets {
		if targetTokenKey(i) == key {
			return t.TokenFile
		}
	}
	return ""
}

// clientFailed is the line of a target whose client could not be made,
// such as a file changed since its check.
func clientFailed(name string, err error) Result {
	return Result{
		Outcome: Unknown,
		Name:    name,
		Summary: noAnswerSummary,
		Causes:  []string{"GitHub 클라이언트를 만들지 못함: " + err.Error()},
		Fix:     fixAgain,
	}
}

// Fixes of the GitHub refusals.
const (
	fixAppInstall    = "GitHub에서 App을 이 저장소에 설치한다(App 설정의 Install App)"
	fixAppPermission = "App 설정의 Repository permissions에서 Administration을 Read and write로 바꾸고, 설치에서 새 권한을 승인한다"
	fixAppJWT        = "app-client-id와 키 파일이 같은 App의 것인지, 그 키가 App에서 지워지지 않았는지 확인한다"
	fixTokenRefused  = "토큰이 만료되거나 취소되지 않았는지 확인하고, 그렇다면 새 토큰을 만들어 파일을 바꾼다"
	fixTokenAccess   = "classic 토큰에는 repo scope와 이 저장소의 admin 권한을, fine-grained 토큰에는 이 저장소의 Administration: Read and write를 준다"
	fixFineGrained   = "fine-grained 토큰에 이 저장소의 Repository permission Administration: Read and write를 준다"
	fixClassicScope  = "classic 토큰에 repo scope를 준다"
	fixScopesUnknown = "GitHub 토큰 설정에서 이 토큰에 repo scope가 있는지 직접 확인한다"
	fixCodeUnknown   = "GitHub 상태를 확인하고 잠시 뒤 다시 실행한다"
)

// appTarget is R300: the App JWT installation lookup, once.
func (c *checker) appTarget(ctx context.Context, name string, app AppProbe, t github.Target) Result {
	var (
		inst github.Installation
		err  error
	)
	c.withTimeout(ctx, func(ctx context.Context) { inst, err = app.Installation(ctx, t) })
	if err != nil {
		if r, ok := c.unanswered(name, err); ok {
			return r
		}
		var se *github.StatusError
		errors.As(err, &se)
		switch se.Code {
		case http.StatusNotFound:
			return failed(name, "App이 설치되지 않음", se.Message, fixAppInstall)
		case http.StatusUnauthorized:
			return failed(name, "App 인증 거부", "GitHub가 App의 JWT를 받지 않음(401)", fixAppJWT)
		}
		return unknownCode(name, se.Code)
	}
	if err := github.CheckPermission(inst, t); err != nil {
		return failed(name, "App 권한 부족", err.Error(), fixAppPermission)
	}
	perm, level := t.Permission()
	return Result{Outcome: Pass, Name: name, Summary: fmt.Sprintf("App 설치 %d, %s: %s", inst.ID, perm, level)}
}

// tokenTarget is R301: the token check, then the scope or the policies
// call, each once. R317's expiry goes on the line whatever its outcome.
func (c *checker) tokenTarget(ctx context.Context, name string, tc TokenClient, t github.Target) Result {
	var (
		access github.Access
		err    error
	)
	c.withTimeout(ctx, func(ctx context.Context) { access, err = tc.Probe.CheckRepositoryAccess(ctx, t) })
	if err != nil {
		if r, ok := c.unanswered(name, err); ok {
			return r
		}
		var se *github.StatusError
		errors.As(err, &se)
		switch se.Code {
		case http.StatusUnauthorized:
			return failed(name, "토큰 거부(401)", se.Message, fixTokenRefused)
		case http.StatusForbidden, http.StatusNotFound:
			return failed(name, fmt.Sprintf("토큰 권한 부족(%d)", se.Code), se.Message, fixTokenAccess)
		}
		return unknownCode(name, se.Code)
	}

	var r Result
	if tc.FineGrained {
		r = c.policies(ctx, name, tc.Probe, t)
	} else {
		r = classicScopes(name, access)
	}
	suffix, warning := expiration(access, c.started)
	r.Summary += suffix
	if warning != "" {
		r.Warnings = append(r.Warnings, warning)
	}
	return r
}

// policies checks a fine-grained token's Administration write through the
// Actions policies list, which needs exactly that (0003 SPEC R301).
func (c *checker) policies(ctx context.Context, name string, probe TokenProbe, t github.Target) Result {
	var (
		code int
		err  error
	)
	c.withTimeout(ctx, func(ctx context.Context) { code, err = probe.ActionsPoliciesStatus(ctx, t) })
	if err != nil {
		if r, ok := c.unanswered(name, err); ok {
			return r
		}
		return unknownCause(name, err.Error())
	}
	switch {
	case code == http.StatusOK:
		return Result{Outcome: Pass, Name: name, Summary: "fine-grained 토큰, Administration: write"}
	case code == http.StatusForbidden:
		return failed(name, "토큰 권한 부족(403)", "토큰에 이 저장소의 Administration 쓰기 권한이 없음", fixFineGrained)
	case code >= 500:
		return unanswered5xx(name, code)
	}
	return unknownCode(name, code)
}

// classicScopes checks the scopes GitHub listed for a classic token; only
// an item exactly "repo" passes (0003 SPEC R301).
func classicScopes(name string, a github.Access) Result {
	switch {
	case !a.ScopesPresent:
		return Result{
			Outcome: Unknown, Name: name, Summary: noAnswerSummary,
			Causes: []string{"응답에 X-OAuth-Scopes 헤더가 없어 토큰의 scope를 알 수 없음"},
			Fix:    fixScopesUnknown,
		}
	case slices.Contains(a.Scopes, "repo"):
		return Result{Outcome: Pass, Name: name, Summary: "classic 토큰, repo scope"}
	}
	return failed(name, "repo scope 없음", "토큰의 scope에 repo가 없음", fixClassicScope)
}

// unanswered returns the 확인 못 함 line of a call without a usable answer:
// a timeout, a transport error, a 5xx or a rate limit, the last with R52's
// time (0003 SPEC R302). ok is false for any other HTTP status.
func (c *checker) unanswered(name string, err error) (Result, bool) {
	var rl *retry.RateLimitError
	if errors.As(err, &rl) {
		now := c.Now()
		at := now.Add(retry.RateLimitWait(err, now))
		return unknownCause(name, fmt.Sprintf("GitHub 속도 제한: %s 뒤에 다시 실행할 수 있음", at.Format(timeLayout))), true
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return unknownCause(name, fmt.Sprintf("%s 안에 응답이 없음", c.Timeout)), true
	}
	var se *github.StatusError
	if !errors.As(err, &se) {
		return unknownCause(name, err.Error()), true
	}
	if se.Code >= 500 {
		return unanswered5xx(name, se.Code), true
	}
	return Result{}, false
}

// timeLayout shows a time to the user.
const timeLayout = "2006-01-02 15:04:05 MST"

func unanswered5xx(name string, code int) Result {
	return unknownCause(name, fmt.Sprintf("GitHub가 %d로 응답함", code))
}

func unknownCode(name string, code int) Result {
	r := unknownCause(name, fmt.Sprintf("예상하지 않은 응답 코드 %d", code))
	r.Fix = fixCodeUnknown
	return r
}

func unknownCause(name, cause string) Result {
	return Result{Outcome: Unknown, Name: name, Summary: noAnswerSummary, Causes: []string{cause}, Fix: fixAgain}
}

func failed(name, summary, cause, fix string) Result {
	return Result{Outcome: Fail, Name: name, Summary: summary, Causes: []string{cause}, Fix: fix}
}
