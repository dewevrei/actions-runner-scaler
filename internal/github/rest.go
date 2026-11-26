package github

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/dewevrei/actions-runner-scaler/internal/logging"
	"github.com/dewevrei/actions-runner-scaler/internal/retry"
)

// DefaultAPIBaseURL is the REST API root of github.com.
const DefaultAPIBaseURL = "https://api.github.com"

const (
	// apiVersion pins the REST API version sent with every request.
	apiVersion = "2022-11-28"
	userAgent  = "actions-runner-scaler"

	// jwtBackdate sets iat in the past against clock drift, and jwtLifetime
	// keeps exp under the ten minutes GitHub allows from now (GitHub docs,
	// "Generating a JSON Web Token (JWT) for a GitHub App").
	jwtBackdate = 60 * time.Second
	jwtLifetime = 9 * time.Minute

	// tokenRefreshMargin is how long before expiry a cached installation
	// token is replaced.
	tokenRefreshMargin = 5 * time.Minute

	// runnersPerPage is the largest page the runner list allows.
	runnersPerPage = 100
	// maxRunnerPages stops a Link header loop.
	maxRunnerPages = 100

	maxBodyBytes      = 8 << 20
	maxErrorBodyBytes = 64 << 10
	maxMessageRunes   = 300
)

// Clock is the time source for token expiry and JWT claims.
type Clock interface {
	Now() time.Time
}

// HTTPDoer sends HTTP requests. *http.Client implements it.
type HTTPDoer interface {
	Do(req *http.Request) (*http.Response, error)
}

// expiringSecrets registers values with a logging.Secrets and unregisters
// each one once it has expired, so the set does not grow without bound.
// It is safe for concurrent use.
type expiringSecrets struct {
	secrets *logging.Secrets

	mu      sync.Mutex
	entries []expiringSecret
}

type expiringSecret struct {
	value   string
	expires time.Time
}

// add registers value until expires and drops the entries expired at now.
func (s *expiringSecrets) add(value string, expires, now time.Time) {
	s.secrets.Register(value)
	s.mu.Lock()
	defer s.mu.Unlock()
	kept := s.entries[:0]
	for _, e := range s.entries {
		if now.After(e.expires) {
			s.secrets.Unregister(e.value)
			continue
		}
		kept = append(kept, e)
	}
	s.entries = append(kept, expiringSecret{value: value, expires: expires})
}

// AppSigner signs GitHub App JWTs (RS256). Every JWT it makes is registered
// with the secrets set until it expires. It implements scaleset.JWTProvider
// and is safe for concurrent use.
type AppSigner struct {
	clientID string
	key      *rsa.PrivateKey
	clock    Clock
	issued   *expiringSecrets
}

// NewAppSigner returns a signer for the App with clientID (the client ID or
// App ID, used as iss) and its private key.
func NewAppSigner(clientID string, key *rsa.PrivateKey, clock Clock, secrets *logging.Secrets) (*AppSigner, error) {
	switch {
	case clientID == "":
		return nil, errors.New("github: empty App client ID")
	case key == nil:
		return nil, errors.New("github: nil App private key")
	case clock == nil:
		return nil, errors.New("github: nil clock")
	case secrets == nil:
		return nil, errors.New("github: nil secrets")
	}
	return &AppSigner{
		clientID: clientID,
		key:      key,
		clock:    clock,
		issued:   &expiringSecrets{secrets: secrets},
	}, nil
}

// jwtHeader is the fixed, encoded JOSE header of every App JWT.
var jwtHeader = base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256","typ":"JWT"}`))

// Token returns a new App JWT with iat one minute in the past and exp nine
// minutes ahead.
func (s *AppSigner) Token(ctx context.Context) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", fmt.Errorf("sign App JWT: %w", err)
	}
	now := s.clock.Now()
	exp := now.Add(jwtLifetime)
	claims, err := json.Marshal(struct {
		IssuedAt  int64  `json:"iat"`
		ExpiresAt int64  `json:"exp"`
		Issuer    string `json:"iss"`
	}{
		IssuedAt:  now.Add(-jwtBackdate).Unix(),
		ExpiresAt: exp.Unix(),
		Issuer:    s.clientID,
	})
	if err != nil {
		return "", fmt.Errorf("sign App JWT: %w", err)
	}
	signingInput := jwtHeader + "." + base64.RawURLEncoding.EncodeToString(claims)
	digest := sha256.Sum256([]byte(signingInput))
	sig, err := rsa.SignPKCS1v15(rand.Reader, s.key, crypto.SHA256, digest[:])
	if err != nil {
		// The error text from crypto/rsa carries no key material.
		return "", fmt.Errorf("sign App JWT: %w", err)
	}
	token := signingInput + "." + base64.RawURLEncoding.EncodeToString(sig)
	s.issued.add(token, exp, now)
	return token, nil
}

// RESTConfig configures a REST client.
type RESTConfig struct {
	// BaseURL is the API root, such as DefaultAPIBaseURL.
	BaseURL string
	// InstallationID is the App installation whose token the client uses.
	InstallationID int64
	// Signer makes the App JWT that buys installation tokens.
	Signer *AppSigner
	// HTTPClient sends the requests.
	HTTPClient HTTPDoer
	// Clock decides when a cached installation token is replaced.
	Clock Clock
	// Secrets receives every installation token.
	Secrets *logging.Secrets
}

// REST calls the GitHub REST endpoints the scaler needs directly:
// installation tokens and an organization's runners. It is safe for
// concurrent use.
type REST struct {
	apiClient
	installationID int64
	signer         *AppSigner
	tokens         *expiringSecrets

	// tokenLock serializes token creation. It is a channel so that a
	// waiting caller still honors its context.
	tokenLock chan struct{}
	// token and tokenExpires are guarded by tokenLock.
	token        string
	tokenExpires time.Time
}

// NewREST returns a REST client.
func NewREST(cfg RESTConfig) (*REST, error) {
	switch {
	case cfg.InstallationID <= 0:
		return nil, errors.New("github: installation ID must be positive")
	case cfg.Signer == nil:
		return nil, errors.New("github: nil App signer")
	case cfg.HTTPClient == nil:
		return nil, errors.New("github: nil HTTP client")
	case cfg.Clock == nil:
		return nil, errors.New("github: nil clock")
	case cfg.Secrets == nil:
		return nil, errors.New("github: nil secrets")
	}
	api, err := newAPIClient(cfg.BaseURL, cfg.HTTPClient, cfg.Clock, cfg.Secrets)
	if err != nil {
		return nil, err
	}
	return &REST{
		apiClient:      api,
		installationID: cfg.InstallationID,
		signer:         cfg.Signer,
		tokens:         &expiringSecrets{secrets: cfg.Secrets},
		tokenLock:      make(chan struct{}, 1),
	}, nil
}

// apiClient holds what a REST request needs besides its authentication; REST
// and App share it (0002 PLAN M1).
type apiClient struct {
	base    *url.URL
	http    HTTPDoer
	clock   Clock
	secrets *logging.Secrets
}

// newAPIClient checks the base URL and returns the shared request parts.
func newAPIClient(baseURL string, httpClient HTTPDoer, clock Clock, secrets *logging.Secrets) (apiClient, error) {
	base, err := url.Parse(strings.TrimRight(baseURL, "/"))
	if err != nil || (base.Scheme != "https" && base.Scheme != "http") || base.Host == "" ||
		base.RawQuery != "" || base.Fragment != "" || base.User != nil {
		return apiClient{}, errors.New("github: base URL must be an absolute http(s) URL without query")
	}
	return apiClient{base: base, http: httpClient, clock: clock, secrets: secrets}, nil
}

// ListRunners returns every self-hosted runner of the organization t, reading
// all pages by following the Link header (0002 SPEC R148). The caller's
// context bounds the whole list.
func (r *REST) ListRunners(ctx context.Context, t Target) ([]Runner, error) {
	return r.readRunners(ctx, retry.OpListOrgRunners, t, "")
}

// FindRunner looks for the runner called name in the organization t (0002
// SPEC R149). It sends the name as a query and still compares the names of
// the entries it reads, because the endpoint's name matching is not
// documented; it reports false when no entry has exactly that name.
func (r *REST) FindRunner(ctx context.Context, t Target, name string) (Runner, bool, error) {
	if name == "" {
		return Runner{}, false, &retry.OpError{Op: retry.OpFindOrgRunner, Err: errors.New("runner name is empty")}
	}
	runners, err := r.readRunners(ctx, retry.OpFindOrgRunner, t, name)
	if err != nil {
		return Runner{}, false, err
	}
	for _, rn := range runners {
		if rn.Name == name {
			return rn, true, nil
		}
	}
	return Runner{}, false, nil
}

// readRunners reads the pages of the organization's runner list under op,
// filtered by name when it is not empty.
func (r *REST) readRunners(ctx context.Context, op retry.Op, t Target, name string) ([]Runner, error) {
	if !t.valid() {
		return nil, &retry.OpError{Op: op, Err: errors.New("target is not an organization")}
	}
	u := r.endpoint(strings.Split(t.RunnersPath(), "/")...)
	q := url.Values{}
	if name != "" {
		q.Set("name", name)
	}
	q.Set("per_page", strconv.Itoa(runnersPerPage))
	u.RawQuery = q.Encode()

	var runners []Runner
	err := r.walkPages(ctx, u, maxRunnerPages, func(body []byte) (bool, error) {
		page, err := decodeRunners(body)
		runners = append(runners, page...)
		return false, err
	})
	if err != nil {
		return nil, &retry.OpError{Op: op, Err: err}
	}
	return runners, nil
}

// decodeRunners reads one page of the runner list. name and busy are
// required by the schema; a runner without them cannot be judged, so the
// page is rejected rather than guessed.
func decodeRunners(body []byte) ([]Runner, error) {
	var resp struct {
		Runners []struct {
			ID   int64   `json:"id"`
			Name *string `json:"name"`
			Busy *bool   `json:"busy"`
		} `json:"runners"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, decodeError(err)
	}
	runners := make([]Runner, 0, len(resp.Runners))
	for _, rn := range resp.Runners {
		if rn.Name == nil || rn.Busy == nil {
			return nil, errors.New("decode response: runner without name or busy")
		}
		runners = append(runners, Runner{ID: rn.ID, Name: *rn.Name, Busy: *rn.Busy})
	}
	return runners, nil
}

// walkPages GETs u and then each rel="next" page, handing every body to
// visit. It ends after the last page, when visit returns true or an error,
// or with an error when a next page would exceed maxPages. Errors are not
// wrapped; the caller adds its Op.
func (r *REST) walkPages(ctx context.Context, u *url.URL, maxPages int, visit func(body []byte) (stop bool, err error)) error {
	for page := 1; u != nil; page++ {
		if page > maxPages {
			return fmt.Errorf("more than %d pages", maxPages)
		}
		body, header, err := r.get(ctx, u)
		if err != nil {
			return err
		}
		stop, err := visit(body)
		if err != nil || stop {
			return err
		}
		if u, err = r.nextPage(header); err != nil {
			return err
		}
	}
	return nil
}

// nextPage returns the rel="next" URL of the Link header, or nil on the
// last page. The URL must stay on the API host, since the request carries
// the installation token.
func (r *REST) nextPage(header http.Header) (*url.URL, error) {
	next := linkNext(header.Values("Link"))
	if next == "" {
		return nil, nil
	}
	u, err := url.Parse(next)
	if err != nil || u.Scheme != r.base.Scheme || u.Host != r.base.Host || u.User != nil {
		return nil, errors.New("next page link is not on the API host")
	}
	return u, nil
}

// linkNext returns the target of the rel="next" link in Link header values
// (RFC 8288), or "".
func linkNext(values []string) string {
	for _, value := range values {
		for _, link := range strings.Split(value, ",") {
			target, params, ok := strings.Cut(strings.TrimSpace(link), ";")
			target = strings.TrimSpace(target)
			if !ok || !strings.HasPrefix(target, "<") || !strings.HasSuffix(target, ">") {
				continue
			}
			for _, param := range strings.Split(params, ";") {
				name, val, ok := strings.Cut(strings.TrimSpace(param), "=")
				if !ok || !strings.EqualFold(strings.TrimSpace(name), "rel") {
					continue
				}
				for _, rel := range strings.Fields(strings.Trim(strings.TrimSpace(val), `"`)) {
					if strings.EqualFold(rel, "next") {
						return target[1 : len(target)-1]
					}
				}
			}
		}
	}
	return ""
}

// endpoint returns the API URL for the escaped path segments.
func (r *apiClient) endpoint(segments ...string) *url.URL {
	escaped := make([]string, len(segments))
	for i, s := range segments {
		escaped[i] = url.PathEscape(s)
	}
	u := *r.base
	u.Path = strings.TrimRight(r.base.Path, "/") + "/" + strings.Join(segments, "/")
	u.RawPath = strings.TrimRight(r.base.EscapedPath(), "/") + "/" + strings.Join(escaped, "/")
	return &u
}

// get sends an authenticated GET and returns the body of a 200 response.
func (r *REST) get(ctx context.Context, u *url.URL) ([]byte, http.Header, error) {
	token, err := r.installationToken(ctx)
	if err != nil {
		return nil, nil, err
	}
	req, err := r.newRequest(ctx, http.MethodGet, u, "Bearer "+token)
	if err != nil {
		return nil, nil, err
	}
	resp, err := r.send(req)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		if resp.StatusCode == http.StatusUnauthorized {
			r.dropToken(ctx, token)
		}
		return nil, nil, r.responseError(resp)
	}
	body, err := readBody(resp.Body, maxBodyBytes)
	if err != nil {
		return nil, nil, err
	}
	return body, resp.Header, nil
}

// installationToken returns a cached installation token, creating a new
// one when none is cached or the cached one expires within five minutes.
func (r *REST) installationToken(ctx context.Context) (string, error) {
	select {
	case r.tokenLock <- struct{}{}:
	case <-ctx.Done():
		return "", ctx.Err()
	}
	defer func() { <-r.tokenLock }()

	if r.token != "" && r.clock.Now().Before(r.tokenExpires.Add(-tokenRefreshMargin)) {
		return r.token, nil
	}
	token, expires, err := r.createInstallationToken(ctx)
	if err != nil {
		return "", &retry.OpError{Op: retry.OpInstallationToken, Err: markAuthRefusal(err)}
	}
	r.token, r.tokenExpires = token, expires
	return token, nil
}

// dropToken forgets token if it is still the cached one, so the next call
// creates a new one. Like installationToken it waits for the lock only
// while ctx lasts; when ctx ends first the token is kept, and the next call
// that gets a 401 with it drops it.
func (r *REST) dropToken(ctx context.Context, token string) {
	select {
	case r.tokenLock <- struct{}{}:
	case <-ctx.Done():
		return
	}
	defer func() { <-r.tokenLock }()
	if r.token == token {
		r.token, r.tokenExpires = "", time.Time{}
	}
}

// markAuthRefusal marks a 4xx answer to the installation token request,
// other than a rate limit, as retry.ErrAuth: the App's access was refused,
// whatever call needed the token (R51). Rate limits, 5xx and transport
// errors are left as they are.
func markAuthRefusal(err error) error {
	var se *StatusError
	if errors.As(err, &se) && se.Code >= 400 && se.Code < 500 {
		var rl *retry.RateLimitError
		if !errors.As(err, &rl) {
			return &sentinelError{sentinel: retry.ErrAuth, err: err}
		}
	}
	return err
}

// createInstallationToken calls "Create an installation access token for
// an app" (POST /app/installations/{id}/access_tokens, 201).
func (r *REST) createInstallationToken(ctx context.Context) (string, time.Time, error) {
	jwt, err := r.signer.Token(ctx)
	if err != nil {
		return "", time.Time{}, err
	}
	u := r.endpoint("app", "installations", strconv.FormatInt(r.installationID, 10), "access_tokens")
	req, err := r.newRequest(ctx, http.MethodPost, u, "Bearer "+jwt)
	if err != nil {
		return "", time.Time{}, err
	}
	resp, err := r.send(req)
	if err != nil {
		return "", time.Time{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		return "", time.Time{}, r.responseError(resp)
	}
	body, err := readBody(resp.Body, maxErrorBodyBytes)
	if err != nil {
		return "", time.Time{}, err
	}
	var created struct {
		Token     string    `json:"token"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	if err := json.Unmarshal(body, &created); err != nil {
		// The body holds the token; the decoder's text is not used.
		return "", time.Time{}, errors.New("decode installation token response")
	}
	if created.Token == "" || created.ExpiresAt.IsZero() {
		return "", time.Time{}, errors.New("installation token response without token or expires_at")
	}
	r.tokens.add(created.Token, created.ExpiresAt, r.clock.Now())
	return created.Token, created.ExpiresAt, nil
}

func (r *apiClient) newRequest(ctx context.Context, method string, u *url.URL, authorization string) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, u.String(), nil)
	if err != nil {
		return nil, &safeError{msg: r.secrets.Redact(err.Error()), cause: keptCause(err)}
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", apiVersion)
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Authorization", authorization)
	return req, nil
}

// send performs req and rewrites a transport error so its text carries no
// URL query.
func (r *apiClient) send(req *http.Request) (*http.Response, error) {
	resp, err := r.http.Do(req)
	if err != nil {
		return nil, &safeError{msg: r.secrets.Redact(err.Error()), cause: keptCause(err)}
	}
	return resp, nil
}

// responseError converts a failed response into a retry.RateLimitError or a
// *StatusError. A response is a rate limit by lastFailure.isRateLimit, and
// its wait hints are read by lastFailure.rateLimitError, the same rules the
// library path uses (convertError, rateLimitFromLibrary).
func (r *apiClient) responseError(resp *http.Response) error {
	body, _ := readBody(resp.Body, maxErrorBodyBytes)
	message := responseMessage(resp.Header, body)
	statusErr := &StatusError{Code: resp.StatusCode, Message: shortMessage(r.secrets, message)}
	f := newFailure(resp, r.clock.Now(), message)
	if !f.isRateLimit() {
		return statusErr
	}
	return f.rateLimitError(statusErr)
}

// parseRetryAfter reads Retry-After as delay-seconds or an HTTP date (RFC
// 9110 10.2.3), measured from now. Both call paths use it: the REST path on
// the response, the library path on the header the recording transport
// kept (rateLimitFromLibrary).
//
// It reports false for an empty or malformed value, a negative number, and
// a date not after now. A number or date whose wait does not fit in a
// time.Duration reports retry.MaxRateLimitWait: it asks for a longer wait
// than any other, so it must not fall back to the shorter wait for a
// missing header. A wait that fits is not capped; the retry package caps
// it at retry.MaxRateLimitWait.
//
// It differs from scaleset v0.5.0's parser (http_client.go parseRetryAfter,
// durationUntil), which reports false for a value too large for a
// time.Duration, in two more ways: it trims surrounding white space, and a
// date equal to now reports false where the library reports a zero wait.
// The retry package treats a zero wait as no hint, so that difference does
// not change any wait.
func parseRetryAfter(value string, now time.Time) (time.Duration, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0, false
	}
	if sec, err := strconv.ParseInt(value, 10, 64); err == nil || errors.Is(err, strconv.ErrRange) {
		switch {
		case strings.HasPrefix(value, "-"):
			return 0, false
		case err != nil || sec > int64(math.MaxInt64/int64(time.Second)):
			return retry.MaxRateLimitWait, true
		}
		return time.Duration(sec) * time.Second, true
	}
	when, err := http.ParseTime(value)
	if err != nil {
		return 0, false
	}
	// Time.Sub saturates; a wait that does not lead back to when did not
	// fit in a time.Duration.
	wait := when.Sub(now)
	switch {
	case wait <= 0:
		return 0, false
	case !now.Add(wait).Equal(when):
		return retry.MaxRateLimitWait, true
	}
	return wait, true
}

// readBody reads at most limit bytes of body.
func readBody(body io.Reader, limit int64) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(body, limit+1))
	if err != nil {
		return nil, &safeError{msg: "read response body: " + err.Error(), cause: keptCause(err)}
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("response body larger than %d bytes", limit)
	}
	return data, nil
}

// decodeError describes a JSON decode failure without quoting the body.
func decodeError(err error) error {
	var syntax *json.SyntaxError
	var typ *json.UnmarshalTypeError
	switch {
	case errors.As(err, &syntax):
		return fmt.Errorf("decode response: invalid JSON at offset %d", syntax.Offset)
	case errors.As(err, &typ):
		return fmt.Errorf("decode response: field %q has JSON type %s", typ.Field, typ.Value)
	}
	return errors.New("decode response: invalid JSON")
}
