package github

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"

	"github.com/dewevrei/actions-runner-scaler/internal/logging"
	"github.com/dewevrei/actions-runner-scaler/internal/retry"
)

// TokenRESTConfig configures a REST client that authenticates with a
// personal access token (0003 SPEC R278).
type TokenRESTConfig struct {
	// BaseURL is the API root, such as DefaultAPIBaseURL.
	BaseURL string
	// Token is the personal access token every request carries.
	Token string
	// HTTPClient sends the requests.
	HTTPClient HTTPDoer
	// Clock reads the time of rate limit answers.
	Clock Clock
	// Secrets receives Token before anything else, so no error or log line
	// can show it (0003 SPEC R277).
	Secrets *logging.Secrets
}

// NewTokenREST returns a REST client that sends cfg.Token as
// "Authorization: Bearer <token>" on every request. It buys no
// installation token and keeps its token after a 401, since it has no other
// to try.
func NewTokenREST(cfg TokenRESTConfig) (*REST, error) {
	switch {
	case cfg.Secrets == nil:
		return nil, errors.New("github: nil secrets")
	case cfg.Token == "":
		return nil, errors.New("github: empty personal access token")
	}
	cfg.Secrets.Register(cfg.Token)
	switch {
	case cfg.HTTPClient == nil:
		return nil, errors.New("github: nil HTTP client")
	case cfg.Clock == nil:
		return nil, errors.New("github: nil clock")
	}
	api, err := newAPIClient(cfg.BaseURL, cfg.HTTPClient, cfg.Clock, cfg.Secrets)
	if err != nil {
		return nil, err
	}
	return &REST{apiClient: api, auth: &tokenAuth{token: cfg.Token}}, nil
}

// tokenAuth is the authorizer of a personal access token. It is used by
// pointer so that printing a REST shows an address, not the token.
type tokenAuth struct {
	token string
}

func (a *tokenAuth) credential(context.Context) (string, error) { return a.token, nil }

func (a *tokenAuth) refused(context.Context, string) {}

// Access is what GitHub's answer to the token check says about the token
// (0003 SPEC R279, R301, R317): all of it for a 200, only Status and the
// expiration for a refusal.
type Access struct {
	// Status is the HTTP status: 200, or the 401, 403 or 404 of a refusal.
	Status int
	// Scopes are the items of X-OAuth-Scopes, split on "," and trimmed,
	// empty items left out. GitHub sends the header for classic tokens.
	Scopes []string
	// ScopesPresent is true when X-OAuth-Scopes was sent, even with an
	// empty value. Present without an item exactly "repo" means 실패 in
	// 0003 SPEC R301; only an absent header means 확인 못 함.
	ScopesPresent bool
	// Expiration is GitHub-Authentication-Token-Expiration as sent,
	// unfiltered. Callers must not log it; doctor applies 0003 SPEC R317's
	// character filter before showing it.
	Expiration string
	// ExpirationPresent reports whether the expiration header was sent.
	ExpirationPresent bool
}

// Texts of the token check's refusals (0003 SPEC R279).
const (
	tokenRefusedFormat       = "personal access token for repository %s was refused (401): it is invalid, expired or revoked"
	tokenCannotManageFormat  = "personal access token for repository %s cannot manage its runners (%d): a classic token needs the repo scope and admin access, a fine-grained token needs Administration: Read and write on this repository"
	tokenAcceptsHintFormat   = "; GitHub says the endpoint accepts %s"
	tokenSSOHint             = "; an organization requires SAML SSO authorization of this token"
	acceptedPermissionsLimit = 200
)

// acceptedPermissions is the only shape of X-Accepted-GitHub-Permissions
// that is quoted in an error: GitHub's permission names and levels with
// their separators (0003 SPEC R279).
var acceptedPermissions = regexp.MustCompile(fmt.Sprintf(`^[a-z_=,; ]{1,%d}$`, acceptedPermissionsLimit))

// CheckRepositoryAccess asks GET /repos/{owner}/{repo}/actions/runners
// with per_page=1, the token target's start-up check (0003 SPEC R279), and
// returns what the 200 answer's headers say about the token.
//
// A 401, a 403 that is not a rate limit, and a 404 return R279's text as
// the Message of a *StatusError that keeps the status, wrapped with
// retry.ErrTargetSetup. The 403 and 404 texts end with the endpoint's
// accepted permissions when X-Accepted-GitHub-Permissions has the safe
// shape, and with an SSO sentence when X-GitHub-SSO is present; that
// header's value, which names the organization, is never shown. Callers
// take that Message (through errors.As) as the target's last error and as
// doctor's cause, so the text is R279's sentence alone; err.Error() keeps
// the operation and status prefix for logs. A refusal also returns an
// Access with its Status and the expiration header, since R317 shows the
// expiry on a refused token's line too. Rate limits, 5xx and transport
// errors are returned as the other REST calls return them, with an empty
// Access, and stay transient (R24). The texts name a personal access
// token, so the call is meant for clients made by NewTokenREST.
func (r *REST) CheckRepositoryAccess(ctx context.Context, t Target) (Access, error) {
	const op = retry.OpCheckTokenAccess
	if !t.valid() {
		return Access{}, &retry.OpError{Op: op, Err: errors.New("target is not a repository")}
	}
	u := r.endpoint(strings.Split(t.RunnersPath(), "/")...)
	u.RawQuery = "per_page=1"
	resp, err := r.do(ctx, u)
	if err != nil {
		return Access{}, &retry.OpError{Op: op, Err: err}
	}
	defer resp.Body.Close()
	access := Access{Status: resp.StatusCode}
	access.Expiration, access.ExpirationPresent = expirationHeader(resp.Header)
	if resp.StatusCode != http.StatusOK {
		err := r.accessError(resp, t)
		if !errors.Is(err, retry.ErrTargetSetup) {
			access = Access{}
		}
		return access, &retry.OpError{Op: op, Err: err}
	}
	drain(resp.Body)

	if values := resp.Header.Values("X-OAuth-Scopes"); len(values) > 0 {
		access.ScopesPresent = true
		for _, item := range strings.Split(strings.Join(values, ","), ",") {
			if item = strings.TrimSpace(item); item != "" {
				access.Scopes = append(access.Scopes, item)
			}
		}
	}
	return access, nil
}

// expirationHeader returns the first GitHub-Authentication-Token-Expiration
// value, unfiltered, and whether the header was sent.
func expirationHeader(h http.Header) (string, bool) {
	values := h.Values("GitHub-Authentication-Token-Expiration")
	if len(values) == 0 {
		return "", false
	}
	return values[0], true
}

// accessError converts a failed answer to the token check. Only a 401, 403
// and 404 that are not rate limits get R279's text and ErrTargetSetup.
func (r *REST) accessError(resp *http.Response, t Target) error {
	err := r.responseError(resp)
	se, ok := err.(*StatusError)
	if !ok {
		return err
	}
	switch se.Code {
	case http.StatusUnauthorized:
		se.Message = fmt.Sprintf(tokenRefusedFormat, t.FullName())
	case http.StatusForbidden, http.StatusNotFound:
		se.Message = fmt.Sprintf(tokenCannotManageFormat, t.FullName(), se.Code) + r.refusalHints(resp.Header)
	default:
		return err
	}
	return &sentinelError{sentinel: retry.ErrTargetSetup, err: se}
}

// refusalHints returns the endings R279 adds to a 403 or 404 text. The
// accepted permissions pass through the registered secrets as well, so a
// registered value can never be quoted even if it had the safe shape.
func (r *REST) refusalHints(h http.Header) string {
	var hints string
	if accepted := h.Get("X-Accepted-GitHub-Permissions"); acceptedPermissions.MatchString(accepted) {
		hints += fmt.Sprintf(tokenAcceptsHintFormat, r.secrets.Redact(accepted))
	}
	if len(h.Values("X-GitHub-SSO")) > 0 {
		hints += tokenSSOHint
	}
	return hints
}

// ActionsPoliciesStatus asks GET /repos/{owner}/{repo}/actions/policies
// with per_page=1 once and returns the answer's status, whatever it is
// (0003 SPEC R301). Only a transport error, the end of ctx, or a rate limit
// (a *retry.RateLimitError, from which R52's time is read) is an error; a
// rate limit also returns its status. It does not retry (0003 SPEC R302).
func (r *REST) ActionsPoliciesStatus(ctx context.Context, t Target) (int, error) {
	const op = retry.OpGetActionsPolicies
	if !t.valid() {
		return 0, &retry.OpError{Op: op, Err: errors.New("target is not a repository")}
	}
	u := r.endpoint("repos", t.Owner, t.Repo, "actions", "policies")
	u.RawQuery = "per_page=1"
	resp, err := r.do(ctx, u)
	if err != nil {
		return 0, &retry.OpError{Op: op, Err: err}
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		drain(resp.Body)
		return resp.StatusCode, nil
	}
	var rl *retry.RateLimitError
	if err := r.responseError(resp); errors.As(err, &rl) {
		return resp.StatusCode, &retry.OpError{Op: op, Err: err}
	}
	return resp.StatusCode, nil
}

// drain reads up to maxErrorBodyBytes of an unused body, so the connection
// can be reused.
func drain(body io.Reader) {
	_, _ = io.Copy(io.Discard, io.LimitReader(body, maxErrorBodyBytes))
}
