// Package github talks to GitHub: the runner scale set API through
// github.com/actions/scaleset v0.5.0, and four REST endpoints directly. It
// authenticates as a GitHub App installation or with a personal access
// token (0003 SPEC R278).
//
// Every error it returns is a *retry.OpError naming the call, whose cause is
// one the retry package classifies: a retry sentinel, a
// *retry.RateLimitError, a *StatusError, or an error without a status.
// Error texts carry no tokens, JWTs, JIT configs, or URL queries (R31;
// 0003 SPEC R277).
// The same holds for every error in their chain: the original error of the
// library or the HTTP client, whose text can quote a response body, is not
// kept; only the context errors it holds are (see keptCause).
package github

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/actions/scaleset"

	"github.com/dewevrei/actions-runner-scaler/internal/logging"
	"github.com/dewevrei/actions-runner-scaler/internal/retry"
)

// DefaultRunnerGroupID is the default runner group, the runner group of every
// repository scale set (0002 SPEC R239). GitHub does not document the ID; it
// is the value ARC uses.
const DefaultRunnerGroupID = 1

// HTTP/2 health check of the transport. Without it a dead HTTP/2 connection
// can stall the library for about 25 minutes (actions/scaleset issue #131,
// fixed only after v0.5.0). Drop this once a scaleset release includes the
// upstream fix.
const (
	http2SendPingTimeout = 30 * time.Second
	http2PingTimeout     = 10 * time.Second
)

// NewTransport returns the library's default transport with an HTTP/2 ping
// after 30 seconds without frames and a 10 second wait for the reply.
func NewTransport() *http.Transport {
	t := scaleset.DefaultTransport()
	t.HTTP2 = &http.HTTP2Config{
		SendPingTimeout: http2SendPingTimeout,
		PingTimeout:     http2PingTimeout,
	}
	return t
}

// RetryConfig returns the library's default retry policy with rate-limited
// responses never retried, so that the scaler alone paces them (R52).
// Transport errors and 5xx keep the library's retries.
//
// Each of those waits is capped at WaitMax (see capBackoff). The library
// asks Backoff only for responses it does not take as rate limits, since
// OnRateLimit declines every one it does, so the cap never shortens an R52
// wait: a 429 is always a rate limit to the library, and so is a 403 with
// Retry-After, X-RateLimit-Remaining 0, or a "secondary rate" message
// (scaleset v0.5.0 common_client.go send, errors.go rateLimitFromResponse).
//
// One case still reaches the library's retries: the admin connection
// request retries 401 and 403 (client.go getActionsServiceAdminConnection-
// Request, retryOnStatus), so a 403 whose message names a rate limit only
// in a way the library's test misses, such as "API rate limit exceeded"
// without the headers, is sent up to Max (4) more times. 401 and 403 are
// not rate limits to the library, so DefaultBackoff waits 1, 2, 4, 8 s,
// each plus up to half as jitter: at most about 22.5 s in all, with the
// request time on top (each attempt bounded by the client timeout and the
// call's ctx). This is accepted: it costs at most four extra requests on
// one authentication exchange, and the error
// that ends the call is still read as a rate limit (convertError,
// lastFailure.isRateLimit), so the scaler then waits as R52 says. Stopping
// it would mean replacing the library's ShouldRetry for that one request,
// which the library applies after ours and so cannot be overridden.
func RetryConfig() scaleset.RetryConfig {
	cfg := scaleset.DefaultRetryConfig()
	cfg.OnRateLimit = func(context.Context, *scaleset.RateLimitError) (time.Duration, bool) {
		return 0, false
	}
	cfg.Backoff = capBackoff(cfg.Backoff)
	return cfg
}

// backoffFunc is the type of scaleset.RetryConfig.Backoff.
type backoffFunc = func(waitMin, waitMax time.Duration, attempt int, resp *http.Response) time.Duration

// capBackoff returns base, or the library's DefaultBackoff when base is nil,
// with every wait cut to waitMax.
//
// DefaultBackoff follows the Retry-After of a 503 in full, without
// waitMax (scaleset v0.5.0 http_client.go DefaultBackoff), and the default
// policy has no MaxElapsed. A call whose context has no deadline, such as
// the message poll, would then stall for as long as a broken header says,
// a day for "Retry-After: 86400". With the cap a 503 is retried at the
// library's own longest backoff step, and once the library's retries are
// spent the error is transient and the call site's backoff takes over
// (R24, R25). Retry-After on a 503 is a server estimate, not a rate limit
// that R52 forbids retrying before, so retrying sooner breaks no rule.
func capBackoff(base backoffFunc) backoffFunc {
	if base == nil {
		base = scaleset.DefaultBackoff
	}
	return func(waitMin, waitMax time.Duration, attempt int, resp *http.Response) time.Duration {
		return min(base(waitMin, waitMax, attempt, resp), waitMax)
	}
}

// ScaleSetConfig configures a ScaleSetClient for one target. It sets
// exactly one credential: Token, or Signer with InstallationID.
type ScaleSetConfig struct {
	// ConfigURL is the target's repository URL,
	// https://github.com/<owner>/<repo> (Target.ConfigURL; 0002 SPEC R231,
	// R239).
	ConfigURL string
	// Token is the personal access token of a token target (0003 SPEC
	// R278). The library sends it only to ask for registration tokens.
	Token string
	// InstallationID is the App installation on that repository (0002 SPEC
	// R236).
	InstallationID int64
	// Signer makes the App JWTs the library exchanges for tokens.
	Signer scaleset.JWTProvider
	// Transport is the base transport. Nil uses NewTransport.
	Transport http.RoundTripper
	// Logger receives the library's logs and this package's warnings. It
	// should redact secrets. Nil discards.
	Logger *slog.Logger
	// Secrets receives message session tokens and redacts error texts.
	Secrets *logging.Secrets
	// SystemInfo is sent in the User-Agent of every request.
	SystemInfo scaleset.SystemInfo
}

// ScaleSetClient wraps the library client for one target's scale set. Each
// method names its call with a retry.Op and converts the library's errors.
// It is safe for concurrent use.
type ScaleSetClient struct {
	client  *scaleset.Client
	logger  *slog.Logger
	secrets *logging.Secrets
}

// NewScaleSetClient returns a client that authenticates with cfg.Token, or
// as the App through cfg.Signer.
func NewScaleSetClient(cfg ScaleSetConfig) (*ScaleSetClient, error) {
	// Only here, so tests of newScaleSetClient can point at a local server.
	if _, err := ParseTargetURL(cfg.ConfigURL); err != nil {
		return nil, err
	}
	return newScaleSetClient(cfg, RetryConfig())
}

// newScaleSetClient lets tests shorten the retry waits and use a server URL.
func newScaleSetClient(cfg ScaleSetConfig, retryCfg scaleset.RetryConfig) (*ScaleSetClient, error) {
	switch {
	case cfg.Secrets == nil:
		return nil, errors.New("github: nil secrets")
	case cfg.Token != "" && (cfg.Signer != nil || cfg.InstallationID != 0):
		return nil, errors.New("github: set either a personal access token or an App signer and installation ID, not both")
	case cfg.Token != "":
		// Before the library holds it, so no error or log line can show
		// it (0003 SPEC R277).
		cfg.Secrets.Register(cfg.Token)
	case cfg.Signer == nil:
		return nil, errors.New("github: nil App signer")
	case cfg.InstallationID <= 0:
		return nil, errors.New("github: installation ID must be positive")
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	transport := cfg.Transport
	if transport == nil {
		transport = NewTransport()
	}
	httpClient := &http.Client{
		Transport: &statusRecorder{next: transport},
		Timeout:   scaleset.DefaultTimeout,
	}
	options := []scaleset.HTTPOption{
		scaleset.WithHTTPClient(httpClient),
		scaleset.WithRetry(retryCfg),
		scaleset.WithLogger(logger),
	}
	var client *scaleset.Client
	var err error
	if cfg.Token != "" {
		// scaleset v0.5.0 sends the token as "Bearer <token>" to the
		// registration token endpoint only (client.go 222-239, 883-909).
		client, err = scaleset.NewClientWithPersonalAccessToken(
			scaleset.NewClientWithPersonalAccessTokenConfig{
				GitHubConfigURL:     cfg.ConfigURL,
				PersonalAccessToken: cfg.Token,
				SystemInfo:          cfg.SystemInfo,
			},
			options...,
		)
	} else {
		client, err = scaleset.NewClientWithJWTProvider(
			scaleset.ClientWithJWTProviderConfig{
				GitHubConfigURL: cfg.ConfigURL,
				InstallationID:  cfg.InstallationID,
				SystemInfo:      cfg.SystemInfo,
			},
			cfg.Signer,
			options...,
		)
	}
	if err != nil {
		return nil, fmt.Errorf("github: new scale set client: %s", cfg.Secrets.Redact(err.Error()))
	}
	return &ScaleSetClient{client: client, logger: logger, secrets: cfg.Secrets}, nil
}

// call runs fn with a fresh status slot and converts its error for op.
func call(ctx context.Context, op retry.Op, redact logging.Redactor, fn func(context.Context) error) error {
	ctx, slot := withStatusSlot(ctx)
	err := fn(ctx)
	return convertError(op, err, slot.failure(), redact)
}

// GetOrCreateScaleSetInGroup returns the scale set called name in runner
// group groupID, creating it there when absent with DisableUpdate set (R22,
// R80; 0002 SPEC R239: the scaler passes DefaultRunnerGroupID).
// The request leaves Labels empty; the library then fills it on the client
// side with the name as the only label, of type "System" (scaleset v0.5.0
// client.go ensureLabels). An existing scale set with DisableUpdate unset is
// updated to set it; if that fails, the set is kept and a warning is logged.
//
// Several scale sets with the name yield an error that classifies as
// permanent; see multipleScaleSets.
func (c *ScaleSetClient) GetOrCreateScaleSetInGroup(ctx context.Context, groupID int, name string) (*scaleset.RunnerScaleSet, error) {
	var set *scaleset.RunnerScaleSet
	err := call(ctx, retry.OpGetOrCreateScaleSet, c.secrets, func(ctx context.Context) error {
		existing, err := c.client.GetRunnerScaleSet(ctx, groupID, name)
		if err != nil {
			return err
		}
		if existing != nil {
			set = existing
			return nil
		}
		set, err = c.client.CreateRunnerScaleSet(ctx, &scaleset.RunnerScaleSet{
			Name:          name,
			RunnerGroupID: groupID,
			RunnerSetting: scaleset.RunnerSetting{DisableUpdate: true},
		})
		return err
	})
	if err != nil {
		return nil, err
	}
	if set == nil {
		return nil, &retry.OpError{Op: retry.OpGetOrCreateScaleSet, Err: errors.New("empty scale set response")}
	}
	if !set.RunnerSetting.DisableUpdate {
		c.disableUpdate(ctx, set)
	}
	return set, nil
}

// disableUpdate sets DisableUpdate on the existing scale set set, which a
// set created without it (or by another tool) lacks (R22, R80). The request
// carries only the setting, as actions-runner-controller and runpool send
// their partial updates. The answer is checked: the endpoint is a preview,
// and a 200 that ignored the field would otherwise read as done. A failure
// only logs a warning; the scale set is usable either way.
func (c *ScaleSetClient) disableUpdate(ctx context.Context, set *scaleset.RunnerScaleSet) {
	var updated *scaleset.RunnerScaleSet
	err := call(ctx, retry.OpGetOrCreateScaleSet, c.secrets, func(ctx context.Context) error {
		var err error
		updated, err = c.client.UpdateRunnerScaleSet(ctx, set.ID, &scaleset.RunnerScaleSet{
			RunnerSetting: scaleset.RunnerSetting{DisableUpdate: true},
		})
		return err
	})
	switch {
	case err != nil:
		c.logger.Warn("scale set lets runners update themselves; disabling updates failed",
			"scaleSet", set.Name, "scaleSetID", set.ID, "err", err)
	case updated == nil || !updated.RunnerSetting.DisableUpdate:
		c.logger.Warn("scale set lets runners update themselves; the update was accepted but did not take effect",
			"scaleSet", set.Name, "scaleSetID", set.ID)
	default:
		set.RunnerSetting.DisableUpdate = true
		c.logger.Info("scale set updates for runners disabled", "scaleSet", set.Name, "scaleSetID", set.ID)
	}
}

// GetScaleSetInGroup returns the scale set called name in runner group
// groupID, or nil when there is none. Several scale sets with the name yield
// an error that classifies as permanent.
func (c *ScaleSetClient) GetScaleSetInGroup(ctx context.Context, groupID int, name string) (*scaleset.RunnerScaleSet, error) {
	var set *scaleset.RunnerScaleSet
	err := call(ctx, retry.OpGetRunnerScaleSet, c.secrets, func(ctx context.Context) error {
		var err error
		set, err = c.client.GetRunnerScaleSet(ctx, groupID, name)
		return err
	})
	if err != nil {
		return nil, err
	}
	return set, nil
}

// DeleteScaleSet deletes the scale set with id.
func (c *ScaleSetClient) DeleteScaleSet(ctx context.Context, id int) error {
	return call(ctx, retry.OpDeleteScaleSet, c.secrets, func(ctx context.Context) error {
		return c.client.DeleteRunnerScaleSet(ctx, id)
	})
}

// SetSystemInfo sets the information sent with later requests and copied
// into sessions created afterwards. In v0.5.0 it makes no request.
func (c *ScaleSetClient) SetSystemInfo(info scaleset.SystemInfo) {
	c.client.SetSystemInfo(info)
}

// JITConfig is a just-in-time runner registration. Its String and LogValue
// hide Encoded, which lets anyone holding it run as the runner.
type JITConfig struct {
	// RunnerID is the registered runner's ID.
	RunnerID int64
	// RunnerName is the registered runner's name.
	RunnerName string
	// Encoded is the base64 config the runner starts with. The caller
	// must register it with the secrets set for as long as it keeps it.
	Encoded string
}

// String describes the config without Encoded.
func (j JITConfig) String() string {
	return fmt.Sprintf("JIT config for runner %q (id %d)", j.RunnerName, j.RunnerID)
}

// LogValue logs the config without Encoded.
func (j JITConfig) LogValue() slog.Value {
	return slog.GroupValue(slog.Int64("runnerID", j.RunnerID), slog.String("runnerName", j.RunnerName))
}

// GenerateJIT registers a runner called name in scale set scaleSetID and
// returns its JIT config.
//
// The caller must register the returned Encoded with its logging.Secrets
// before using or logging anything that may contain it, and unregister it
// once the runner is gone; GenerateJIT does not, because only the caller
// knows how long the config lives (R31).
func (c *ScaleSetClient) GenerateJIT(ctx context.Context, scaleSetID int, name string) (JITConfig, error) {
	var cfg *scaleset.RunnerScaleSetJitRunnerConfig
	err := call(ctx, retry.OpGenerateJIT, c.secrets, func(ctx context.Context) error {
		var err error
		cfg, err = c.client.GenerateJitRunnerConfig(ctx, &scaleset.RunnerScaleSetJitRunnerSetting{Name: name}, scaleSetID)
		return err
	})
	if err != nil {
		return JITConfig{}, err
	}
	if cfg == nil || cfg.Runner == nil || cfg.EncodedJITConfig == "" {
		return JITConfig{}, &retry.OpError{Op: retry.OpGenerateJIT, Err: errors.New("incomplete JIT config response")}
	}
	return JITConfig{RunnerID: int64(cfg.Runner.ID), RunnerName: cfg.Runner.Name, Encoded: cfg.EncodedJITConfig}, nil
}

// Runner is a registered runner. GetRunnerByName sets ScaleSetID and the
// REST runner lists set Busy; each is zero where the source gives none.
type Runner struct {
	ID         int64
	Name       string
	ScaleSetID int
	Busy       bool
}

// GetRunnerByName returns the runner registered as name, or nil when there
// is none.
func (c *ScaleSetClient) GetRunnerByName(ctx context.Context, name string) (*Runner, error) {
	var ref *scaleset.RunnerReference
	err := call(ctx, retry.OpGetRunnerByName, c.secrets, func(ctx context.Context) error {
		var err error
		ref, err = c.client.GetRunnerByName(ctx, name)
		return err
	})
	if err != nil || ref == nil {
		return nil, err
	}
	return &Runner{ID: int64(ref.ID), Name: ref.Name, ScaleSetID: ref.RunnerScaleSetID}, nil
}

// RemoveRunner removes the runner registration with id. A runner that no
// longer exists yields an error that classifies as retry.Gone.
func (c *ScaleSetClient) RemoveRunner(ctx context.Context, id int64) error {
	return call(ctx, retry.OpRemoveRunner, c.secrets, func(ctx context.Context) error {
		return c.client.RemoveRunner(ctx, id)
	})
}

// CreateSession opens a message session on scale set scaleSetID for owner,
// typically the host name. A 409 means a previous session is still open.
func (c *ScaleSetClient) CreateSession(ctx context.Context, scaleSetID int, owner string) (*Session, error) {
	var inner *scaleset.MessageSessionClient
	err := call(ctx, retry.OpSessionCreate, c.secrets, func(ctx context.Context) error {
		var err error
		inner, err = c.client.MessageSessionClient(ctx, scaleSetID, owner)
		return err
	})
	if err != nil {
		return nil, err
	}
	s := &Session{inner: inner, secrets: c.secrets}
	s.trackToken()
	return s, nil
}
