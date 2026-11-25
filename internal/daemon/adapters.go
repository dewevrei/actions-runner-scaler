package daemon

import (
	"context"
	"crypto/rsa"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptrace"
	"sync"
	"sync/atomic"
	"time"

	"github.com/actions/scaleset"
	"github.com/actions/scaleset/listener"

	"github.com/dewevrei/actions-runner-scaler/internal/config"
	"github.com/dewevrei/actions-runner-scaler/internal/docker"
	"github.com/dewevrei/actions-runner-scaler/internal/gate"
	"github.com/dewevrei/actions-runner-scaler/internal/github"
	"github.com/dewevrei/actions-runner-scaler/internal/logging"
	"github.com/dewevrei/actions-runner-scaler/internal/retry"
	"github.com/dewevrei/actions-runner-scaler/internal/scaler"
)

// deleteMessageTimeout bounds one DeleteMessage call (PLAN M5-5). The
// listener calls it with context.WithoutCancel and the github session adds
// no deadline, so without this bound an ack could outlive finalize (R47).
const deleteMessageTimeout = 30 * time.Second

// getMessagePollTimeout bounds one GetMessage poll (PLAN M5-5, SPEC Areas
// of concern 4). The service holds an empty long poll for about 50 s and
// does not return a JobStarted that becomes available during that hold
// until the next poll (actions/scaleset issue #122), so JobStarted could
// reach the scaler about 50 s late. Ending each poll after 10 s and polling
// again bounds that delay by about 10 s at about six polls a minute.
const getMessagePollTimeout = 10 * time.Second

// getMessageFullPollAfter is how long the server may go without answering
// a poll before the next poll is sent without getMessagePollTimeout
// (PLAN M5-5). A cut poll cannot tell an idle server from a dead one; the
// uncut poll can, at the cost of up to about 50 s of JobStarted delay once
// in this period, well within R81's 180 s.
const getMessageFullPollAfter = 5 * time.Minute

// withTimeout returns a context that ends after d both on the wall clock,
// so Deadline reports it, and on clock, so a fake clock in tests can end
// it. With the system clock the two coincide. Either way a context that
// runs out reports context.DeadlineExceeded, from Err, from context.Cause
// and in every context derived from it, as context.WithTimeout does, so
// retry.Classify reads the failed call as one that hit its own deadline
// (transient). If parent ended first, Err is parent's error. As the context
// contract asks, Err stays nil until Done is closed.
func withTimeout(parent context.Context, clock scaler.Clock, d time.Duration) (context.Context, context.CancelFunc) {
	inner, cancelCause := context.WithCancelCause(parent)
	timed, cancelTimed := context.WithTimeout(inner, d)
	c := &clockTimeoutCtx{Context: timed, done: make(chan struct{})}
	if timed.Err() != nil {
		c.closeDone()
	}
	context.AfterFunc(timed, c.closeDone)
	t := clock.AfterFunc(d, func() {
		c.mu.Lock()
		if timed.Err() == nil {
			c.expired = true
		}
		c.mu.Unlock()
		cancelCause(context.DeadlineExceeded)
		c.closeDone()
	})
	return c, func() {
		t.Stop()
		cancelTimed()
		cancelCause(context.Canceled)
		c.closeDone()
	}
}

// clockTimeoutCtx is the context withTimeout returns. When the clock's timer
// ended it, Err reports context.DeadlineExceeded instead of the Canceled
// the inner cancellation gives; context.Cause reports DeadlineExceeded
// through the inner WithCancelCause.
//
// Done is its own channel, closed once the inner context is done. The
// context package then does not take the inner cancelCtx as a derived
// context's parent (it checks that the Done channels match) and propagates
// Err instead, so derived contexts report DeadlineExceeded too. Done closes
// at once when the clock timer fires, cancel is called, or the parent had
// ended already; after a later end of the parent or the wall deadline it
// closes from a context.AfterFunc goroutine, a moment after the inner
// context ended. Err reports nothing until Done is closed, and Done closes
// only after the inner context ended, so Err is non-nil exactly once Done
// is closed.
type clockTimeoutCtx struct {
	context.Context
	done     chan struct{}
	doneOnce sync.Once

	mu      sync.Mutex
	expired bool
}

func (c *clockTimeoutCtx) closeDone() { c.doneOnce.Do(func() { close(c.done) }) }

func (c *clockTimeoutCtx) Done() <-chan struct{} { return c.done }

func (c *clockTimeoutCtx) Err() error {
	select {
	case <-c.done:
	default:
		return nil
	}
	err := c.Context.Err()
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.expired {
		return context.DeadlineExceeded
	}
	return err
}

// Docker is the Docker side the daemon uses (*docker.Client).
type Docker interface {
	Ping(ctx context.Context) error
	ImageExists(ctx context.Context, ref string) (bool, error)
	MemTotal(ctx context.Context) (int64, error)
	List(ctx context.Context) ([]docker.Resource, error)
	RemoveByID(ctx context.Context, kind, id string) error
	StartRunner(ctx context.Context, p docker.Params, in docker.Stdin) error
	RemoveResources(ctx context.Context, name string) error
	RemoveContainer(ctx context.Context, name string) error
	Wait(ctx context.Context, name string) (int64, error)
	Processes(ctx context.Context, name string) ([]string, error)
}

var _ Docker = (*docker.Client)(nil)

// dockerAdapter adapts Docker to the scaler's interfaces: Containers,
// ContainerWaiter, ResourceLister, ResourceIDRemover and ProcessLister.
type dockerAdapter struct {
	d Docker
	// params holds everything of docker.Params but the runner name.
	params docker.Params
}

var (
	_ scaler.Containers        = dockerAdapter{}
	_ scaler.ContainerWaiter   = dockerAdapter{}
	_ scaler.ResourceLister    = dockerAdapter{}
	_ scaler.ResourceIDRemover = dockerAdapter{}
	_ scaler.ProcessLister     = dockerAdapter{}
)

// StartRunner starts runner name's resources and writes jit and token to
// its stdin (R36, R63).
func (a dockerAdapter) StartRunner(ctx context.Context, name string, jit scaler.JITConfig, token gate.Token) error {
	p := a.params
	p.Name = name
	return a.d.StartRunner(ctx, p, docker.NewStdin(jit.Reveal(), token.Reveal()))
}

func (a dockerAdapter) RemoveResources(ctx context.Context, name string) error {
	return a.d.RemoveResources(ctx, name)
}

func (a dockerAdapter) RemoveContainer(ctx context.Context, name string) error {
	return a.d.RemoveContainer(ctx, name)
}

func (a dockerAdapter) Wait(ctx context.Context, name string) (int64, error) {
	return a.d.Wait(ctx, name)
}

func (a dockerAdapter) Processes(ctx context.Context, name string) ([]string, error) {
	return a.d.Processes(ctx, name)
}

// List converts the docker resources, kind labels included.
func (a dockerAdapter) List(ctx context.Context) ([]scaler.Resource, error) {
	res, err := a.d.List(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]scaler.Resource, len(res))
	for i, r := range res {
		out[i] = scaler.Resource{
			Kind:       resourceKind(r.Kind),
			ID:         r.ID,
			Name:       r.Name,
			RunnerName: r.RunnerName,
			State:      r.State,
		}
	}
	return out, nil
}

// RemoveByID removes one listed resource by its ID.
func (a dockerAdapter) RemoveByID(ctx context.Context, kind scaler.ResourceKind, id string) error {
	return a.d.RemoveByID(ctx, dockerKind(kind), id)
}

// resourceKind converts a docker kind label to a scaler.ResourceKind. A
// missing or unknown label stays as it is, which the scaler treats as
// unknown (it then tells a network by its empty State).
func resourceKind(kind string) scaler.ResourceKind {
	switch kind {
	case docker.KindRunner:
		return scaler.ResourceRunner
	case docker.KindBuildKit:
		return scaler.ResourceBuildKit
	case docker.KindNetwork:
		return scaler.ResourceNetwork
	default:
		return scaler.ResourceKind(kind)
	}
}

// dockerKind converts a scaler.ResourceKind to the kind RemoveByID takes.
// Every kind but a network removes a container, so an unknown kind becomes
// the empty kind, which docker removes as a container.
func dockerKind(kind scaler.ResourceKind) string {
	switch kind {
	case scaler.ResourceNetwork:
		return docker.KindNetwork
	case scaler.ResourceRunner:
		return docker.KindRunner
	case scaler.ResourceBuildKit:
		return docker.KindBuildKit
	default:
		return ""
	}
}

// Session is one message session: the listener's client plus Close
// (*github.Session).
type Session interface {
	listener.Client
	Close(ctx context.Context) error
}

var _ Session = (*github.Session)(nil)

// PoolGitHub is the GitHub side of one pool. Errors carry retry's ops,
// statuses, sentinels and rate limits (the github package converts them).
type PoolGitHub interface {
	// FindScaleSet looks the scale set up without creating it (R17).
	scaler.ScaleSetFinder
	// ListRepoRunners lists the repository's runners (R17, R103).
	scaler.RepoRunnerLister
	// GetRunnerByName and RemoveRunner (R17, finalize).
	scaler.RunnerRegistry
	// GetRun and ListWorkflowRuns (the job check, R9).
	scaler.RunLookup
	// GetOrCreateScaleSet returns the ID of the pool's scale set, creating
	// it when absent (R22, R80).
	GetOrCreateScaleSet(ctx context.Context, name string) (id int, err error)
	// SetSystemInfo sets what later requests and sessions report.
	SetSystemInfo(info scaleset.SystemInfo)
	// GenerateJITConfig registers runner name in scale set scaleSetID.
	GenerateJITConfig(ctx context.Context, scaleSetID int, name string) (scaler.JITRunner, error)
	// CreateSession opens a message session for owner.
	CreateSession(ctx context.Context, scaleSetID int, owner string) (Session, error)
}

// GitHub makes the GitHub side of each pool. Every pool shares one App
// signer and one REST client.
type GitHub interface {
	Pool(p config.Pool) (PoolGitHub, error)
}

// GitHubConfig holds what NewGitHub needs. All fields are required except
// Logger and HTTPClient.
type GitHubConfig struct {
	Config *config.Config
	Key    *rsa.PrivateKey
	Clock  github.Clock
	// Secrets is the process's one set: it receives the App JWTs,
	// installation tokens and message session tokens, and the logger
	// redacts with it (R31).
	Secrets    *logging.Secrets
	Logger     *slog.Logger
	SystemInfo scaleset.SystemInfo
	// HTTPClient sends the REST requests; nil means a client on
	// github.NewTransport.
	HTTPClient github.HTTPDoer
	// BaseURL is the REST API root; empty means github.DefaultAPIBaseURL.
	BaseURL string
}

// NewGitHub returns the real GitHub side: one AppSigner shared by the REST
// client and every pool's scale set client (PLAN M5-3 step 10, Decisions).
func NewGitHub(cfg GitHubConfig) (GitHub, error) {
	if cfg.Config == nil {
		return nil, errors.New("daemon: nil config")
	}
	signer, err := github.NewAppSigner(cfg.Config.GitHub.AppClientID, cfg.Key, cfg.Clock, cfg.Secrets)
	if err != nil {
		return nil, err
	}
	httpClient := cfg.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{Transport: github.NewTransport()}
	}
	base := cfg.BaseURL
	if base == "" {
		base = github.DefaultAPIBaseURL
	}
	rest, err := github.NewREST(github.RESTConfig{
		BaseURL:        base,
		InstallationID: cfg.Config.GitHub.AppInstallationID,
		Signer:         signer,
		HTTPClient:     httpClient,
		Clock:          cfg.Clock,
		Secrets:        cfg.Secrets,
	})
	if err != nil {
		return nil, err
	}
	log := cfg.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &realGitHub{cfg: cfg, signer: signer, rest: rest, log: log}, nil
}

type realGitHub struct {
	cfg    GitHubConfig
	signer *github.AppSigner
	rest   *github.REST
	log    *slog.Logger
}

func (g *realGitHub) Pool(p config.Pool) (PoolGitHub, error) {
	owner, repo, err := github.ParseRepoURL(p.RepoURL)
	if err != nil {
		return nil, err
	}
	ss, err := github.NewScaleSetClient(github.ScaleSetConfig{
		RepoURL:        p.RepoURL,
		InstallationID: g.cfg.Config.GitHub.AppInstallationID,
		Signer:         g.signer,
		Logger:         g.log.With("pool", p.Name, "component", "scaleset"),
		Secrets:        g.cfg.Secrets,
		SystemInfo:     g.cfg.SystemInfo,
	})
	if err != nil {
		return nil, err
	}
	return newPoolGitHub(ss, g.rest, owner, repo), nil
}

// scaleSetClient is the part of *github.ScaleSetClient a pool uses.
type scaleSetClient interface {
	GetOrCreateScaleSet(ctx context.Context, name string) (*scaleset.RunnerScaleSet, error)
	GetScaleSet(ctx context.Context, name string) (*scaleset.RunnerScaleSet, error)
	SetSystemInfo(info scaleset.SystemInfo)
	GenerateJIT(ctx context.Context, scaleSetID int, name string) (github.JITConfig, error)
	GetRunnerByName(ctx context.Context, name string) (*github.Runner, error)
	RemoveRunner(ctx context.Context, id int64) error
	CreateSession(ctx context.Context, scaleSetID int, owner string) (*github.Session, error)
}

// restClient is the part of *github.REST a pool uses.
type restClient interface {
	GetRun(ctx context.Context, owner, repo string, runID int64) (gate.Run, error)
	ListWorkflowRuns(ctx context.Context, owner, repo string, workflowID int64, createdFrom time.Time,
		visit func(page []gate.RunSummary, totalCount int) (stop bool)) error
	ListRepoRunners(ctx context.Context, owner, repo string) ([]github.RepoRunner, error)
}

var (
	_ scaleSetClient = (*github.ScaleSetClient)(nil)
	_ restClient     = (*github.REST)(nil)
)

// poolGitHub adapts the github clients to PoolGitHub for one repository.
type poolGitHub struct {
	ss          scaleSetClient
	rest        restClient
	owner, repo string
}

func newPoolGitHub(ss scaleSetClient, rest restClient, owner, repo string) *poolGitHub {
	return &poolGitHub{ss: ss, rest: rest, owner: owner, repo: repo}
}

var _ PoolGitHub = (*poolGitHub)(nil)

func (g *poolGitHub) FindScaleSet(ctx context.Context, name string) (int, bool, error) {
	set, err := g.ss.GetScaleSet(ctx, name)
	if err != nil || set == nil {
		return 0, false, err
	}
	return set.ID, true, nil
}

func (g *poolGitHub) GetOrCreateScaleSet(ctx context.Context, name string) (int, error) {
	set, err := g.ss.GetOrCreateScaleSet(ctx, name)
	if err != nil {
		return 0, err
	}
	return set.ID, nil
}

func (g *poolGitHub) SetSystemInfo(info scaleset.SystemInfo) { g.ss.SetSystemInfo(info) }

func (g *poolGitHub) GenerateJITConfig(ctx context.Context, scaleSetID int, name string) (scaler.JITRunner, error) {
	jit, err := g.ss.GenerateJIT(ctx, scaleSetID, name)
	if err != nil {
		return scaler.JITRunner{}, err
	}
	return scaler.JITRunner{ID: jit.RunnerID, Config: scaler.NewJITConfig(jit.Encoded)}, nil
}

func (g *poolGitHub) GetRunnerByName(ctx context.Context, name string) (scaler.RegisteredRunner, bool, error) {
	r, err := g.ss.GetRunnerByName(ctx, name)
	if err != nil || r == nil {
		return scaler.RegisteredRunner{}, false, err
	}
	return scaler.RegisteredRunner{ID: r.ID, Name: r.Name, ScaleSetID: r.ScaleSetID}, true, nil
}

func (g *poolGitHub) RemoveRunner(ctx context.Context, id int64) error {
	return g.ss.RemoveRunner(ctx, id)
}

func (g *poolGitHub) CreateSession(ctx context.Context, scaleSetID int, owner string) (Session, error) {
	s, err := g.ss.CreateSession(ctx, scaleSetID, owner)
	if err != nil {
		return nil, err
	}
	if s == nil {
		return nil, errors.New("github: no session")
	}
	return s, nil
}

func (g *poolGitHub) ListRepoRunners(ctx context.Context) ([]scaler.RepoRunner, error) {
	rs, err := g.rest.ListRepoRunners(ctx, g.owner, g.repo)
	if err != nil {
		return nil, err
	}
	out := make([]scaler.RepoRunner, len(rs))
	for i, r := range rs {
		out[i] = scaler.RepoRunner{ID: r.ID, Name: r.Name, Busy: r.Busy}
	}
	return out, nil
}

// GetRun reads the run of the pool's repository. The judge passes the
// policy's owner and repository, which bind already checked against the
// pool's repo-url.
func (g *poolGitHub) GetRun(ctx context.Context, owner, repo string, runID int64) (gate.Run, error) {
	return g.rest.GetRun(ctx, owner, repo, runID)
}

func (g *poolGitHub) ListWorkflowRuns(ctx context.Context, owner, repo string, workflowID int64, createdFrom time.Time,
	visit func(page []gate.RunSummary, totalCount int) (stop bool)) error {
	return g.rest.ListWorkflowRuns(ctx, owner, repo, workflowID, createdFrom, visit)
}

// boundScaleSet is scaler.ScaleSetAPI for the pool's scale set.
type boundScaleSet struct {
	gh PoolGitHub
	id int
}

var _ scaler.ScaleSetAPI = boundScaleSet{}

func (b boundScaleSet) GenerateJITConfig(ctx context.Context, name string) (scaler.JITRunner, error) {
	return b.gh.GenerateJITConfig(ctx, b.id, name)
}

func (b boundScaleSet) RemoveRunner(ctx context.Context, id int64) error {
	return b.gh.RemoveRunner(ctx, id)
}

// tableAuth is gate.Authenticator over the runner table.
type tableAuth struct{ t *scaler.Table }

var _ gate.Authenticator = tableAuth{}

// Authenticate checks the token against the table (R14, R40) and converts
// the verdict.
func (a tableAuth) Authenticate(name, presented string, at time.Time) (gate.Verdict, bool) {
	v, ok := a.t.Authenticate(name, presented, at)
	if !ok {
		return gate.VerdictPending, false
	}
	return gateVerdict(v), true
}

// gateVerdict converts a table verdict to what the server reports. An
// unknown value becomes pending, the only answer that never lets a step run
// (R10); allow is given only for VerdictAllow.
func gateVerdict(v scaler.Verdict) gate.Verdict {
	switch v {
	case scaler.VerdictAllow:
		return gate.VerdictAllow
	case scaler.VerdictDeny:
		return gate.VerdictDeny
	case scaler.VerdictPending:
		return gate.VerdictPending
	default:
		return gate.VerdictPending
	}
}

// sessionClient is the listener.Client the daemon gives listener.Run
// (PLAN M5-5).
//
// DeleteMessage (R47): once finalize has begun it makes no call and returns
// scaler.ErrFinalizing, so Run ends without acknowledging; otherwise the
// call gets deleteMessageTimeout and also ends at finalize deadline A.
//
// GetMessage cuts a poll at getMessagePollTimeout, except that once the
// server has not answered for getMessageFullPollAfter it sends one poll
// without the cut; see GetMessage.
//
// AcquireJobs and Session pass through.
type sessionClient struct {
	inner Session
	table *scaler.Table
	clock scaler.Clock
	// deadlineA ends at finalize deadline A.
	deadlineA context.Context
	// polled is called after each poll the server answered, with a message
	// or without one; a cut poll is not reported.
	polled func()

	mu sync.Mutex
	// lastAnswer is when a GetMessage call last ended by itself, before any
	// cut: with a message, without one, or with an error. It starts at the
	// wrapper's creation.
	lastAnswer time.Time
}

// newSessionClient returns the wrapper for one session. Its server-answer
// clock starts now, so a session the server never answers gets its first
// uncut poll getMessageFullPollAfter after it began.
func newSessionClient(inner Session, table *scaler.Table, clock scaler.Clock, deadlineA context.Context, polled func()) *sessionClient {
	return &sessionClient{
		inner:      inner,
		table:      table,
		clock:      clock,
		deadlineA:  deadlineA,
		polled:     polled,
		lastAnswer: clock.Now(),
	}
}

// GetMessage polls for the next message.
//
// A poll is cut at getMessagePollTimeout (SPEC Areas of concern 4). The
// cut covers the whole call: scaleset's retries of a failed attempt and its
// session refresh run under the same context, and the library neither
// retries nor waits once that context is done (scaleset v0.5.0
// DefaultShouldRetry and commonClient.send). It can return earlier with the
// attempt's own error, since the library skips a retry wait that would
// reach the deadline (waitOutlasts); that error ends Run as before.
//
// A cut poll whose last request was written to a connection is an empty
// poll: nil, nil, which Run hands to Scale as it does an empty long poll.
// One whose last request never got onto a connection (a hung DNS lookup,
// dial, or TLS handshake) is a transient error, so Run ends and the session
// loop makes a new session (R25).
//
// A healthy idle server holds a poll for about 50 s, so a cut poll says
// nothing about whether the server is there. Once no call has ended by
// itself for getMessageFullPollAfter, the next poll is sent without the
// cut, under ctx and the library's own timeouts and retries alone: a dead
// service then shows up as an error, as it did before the cut existed.
func (c *sessionClient) GetMessage(ctx context.Context, lastMessageID, maxCapacity int) (*scaleset.RunnerScaleSetMessage, error) {
	if c.fullPollDue() {
		msg, err := c.inner.GetMessage(ctx, lastMessageID, maxCapacity)
		c.answered(err)
		return msg, err
	}

	var trace pollTrace
	pollCtx, cancel := withTimeout(httptrace.WithClientTrace(ctx, trace.clientTrace()), c.clock, getMessagePollTimeout)
	defer cancel()
	msg, err := c.inner.GetMessage(pollCtx, lastMessageID, maxCapacity)
	if err == nil || !pollBoundPassed(ctx, pollCtx, err) {
		c.answered(err)
		return msg, err
	}
	if !trace.written() {
		return nil, &retry.OpError{Op: retry.OpGetMessage, Err: fmt.Errorf(
			"message poll got no connection within %s: %w", getMessagePollTimeout, err)}
	}
	return nil, nil
}

// fullPollDue reports whether the server has not answered for
// getMessageFullPollAfter.
func (c *sessionClient) fullPollDue() bool {
	now := c.clock.Now()
	c.mu.Lock()
	defer c.mu.Unlock()
	return now.Sub(c.lastAnswer) >= getMessageFullPollAfter
}

// answered records a call that ended by itself and reports a successful
// one to polled.
func (c *sessionClient) answered(err error) {
	now := c.clock.Now()
	c.mu.Lock()
	c.lastAnswer = now
	c.mu.Unlock()
	if err == nil && c.polled != nil {
		c.polled()
	}
}

// pollBoundPassed reports whether err ended a poll only because the poll's
// own bound passed: err is a deadline error, pollCtx ran out of its time,
// and parent is still live. A deadline error while pollCtx is live (some
// other bound inside the call), or with parent ended (cancellation or the
// parent's own deadline), is a real failure.
func pollBoundPassed(parent, pollCtx context.Context, err error) bool {
	return errors.Is(err, context.DeadlineExceeded) &&
		errors.Is(pollCtx.Err(), context.DeadlineExceeded) &&
		parent.Err() == nil
}

// pollTrace tracks whether the latest request of a poll was written to a
// connection. The library sends each attempt, and a session refresh, with
// the poll's context, so net/http reports every request's connection to
// the trace. GetConn starts a request; WroteRequest without an error means
// it reached a connection. HTTP/2 may report WroteRequest after RoundTrip
// returned, so a late report from one attempt could in principle land
// after the next attempt's GetConn; the window is far shorter than the
// library's retry waits, and the uncut poll after getMessageFullPollAfter
// still catches a poll misread this way.
type pollTrace struct {
	wrote atomic.Bool
}

func (p *pollTrace) clientTrace() *httptrace.ClientTrace {
	return &httptrace.ClientTrace{
		GetConn: func(string) { p.wrote.Store(false) },
		WroteRequest: func(info httptrace.WroteRequestInfo) {
			p.wrote.Store(info.Err == nil)
		},
	}
}

// written reports whether the latest request was written to a connection.
func (p *pollTrace) written() bool { return p.wrote.Load() }

func (c *sessionClient) DeleteMessage(ctx context.Context, messageID int) error {
	if c.table.Finalizing() {
		return fmt.Errorf("message %d not acknowledged: %w", messageID, scaler.ErrFinalizing)
	}
	callCtx, cancel := withTimeout(ctx, c.clock, deleteMessageTimeout)
	defer cancel()
	stop := context.AfterFunc(c.deadlineA, cancel)
	defer stop()
	return c.inner.DeleteMessage(callCtx, messageID)
}

func (c *sessionClient) AcquireJobs(ctx context.Context, requestIDs []int64) ([]int64, error) {
	return c.inner.AcquireJobs(ctx, requestIDs)
}

func (c *sessionClient) Session() scaleset.RunnerScaleSetSession { return c.inner.Session() }

var _ listener.Client = (*sessionClient)(nil)
