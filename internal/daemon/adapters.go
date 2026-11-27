package daemon

import (
	"context"
	"crypto/rsa"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/actions/scaleset"
	"github.com/actions/scaleset/listener"

	"github.com/dewevrei/actions-runner-scaler/internal/config"
	"github.com/dewevrei/actions-runner-scaler/internal/docker"
	"github.com/dewevrei/actions-runner-scaler/internal/gate"
	"github.com/dewevrei/actions-runner-scaler/internal/github"
	"github.com/dewevrei/actions-runner-scaler/internal/logging"
	"github.com/dewevrei/actions-runner-scaler/internal/scaler"
)

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
	ImageInspect(ctx context.Context, ref string) (docker.Image, bool, error)
	ImagePull(ctx context.Context, ref string) error
	Info(ctx context.Context) (docker.Capacity, error)
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
	// params holds everything of docker.Params but the runner name and the
	// image IDs, which StartRunner gets for each runner.
	params docker.Params
}

var (
	_ scaler.Containers        = dockerAdapter{}
	_ scaler.ContainerWaiter   = dockerAdapter{}
	_ scaler.ResourceLister    = dockerAdapter{}
	_ scaler.ResourceIDRemover = dockerAdapter{}
	_ scaler.ProcessLister     = dockerAdapter{}
)

// StartRunner starts runner name's resources from the image IDs looked up
// for it and writes jit and token to its stdin (R36, R63, 0003 SPEC R319).
func (a dockerAdapter) StartRunner(ctx context.Context, name string, images scaler.ImageIDs, jit scaler.JITConfig, token gate.Token) error {
	p := a.params
	p.Name = name
	p.RunnerImage = images.Runner
	p.BuildKitImage = images.BuildKit
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

// TargetGitHub is the GitHub side of one target. Errors carry retry's ops,
// statuses, sentinels and rate limits (the github package converts them).
type TargetGitHub interface {
	// FindScaleSet looks the scale set up without creating it (0002 SPEC
	// R241).
	scaler.ScaleSetFinder
	// ListRunners lists the repository's runners (0002 SPEC R241).
	scaler.RunnerLister
	// FindRunner looks one runner of the repository up by name (0002 SPEC
	// R242).
	scaler.RunnerFinder
	// GetRunnerByName and RemoveRunner (0002 SPEC R241, finalize).
	scaler.RunnerRegistry
	// GetOrCreateScaleSet returns the ID of the target's scale set in runner
	// group groupID, creating it when absent (0002 SPEC R239: the daemon
	// passes github.DefaultRunnerGroupID).
	GetOrCreateScaleSet(ctx context.Context, groupID int, name string) (id int, err error)
	// SetSystemInfo sets what later requests and sessions report.
	SetSystemInfo(info scaleset.SystemInfo)
	// GenerateJITConfig registers runner name in scale set scaleSetID.
	GenerateJITConfig(ctx context.Context, scaleSetID int, name string) (scaler.JITRunner, error)
	// CreateSession opens a message session for owner.
	CreateSession(ctx context.Context, scaleSetID int, owner string) (Session, error)
}

// GitHub makes the GitHub side of each target: its credential, which makes
// its clients (0003 SPEC R273).
type GitHub interface {
	// Auth returns the credential of t, the target at index in the
	// configuration's targets.
	Auth(t github.Target, index int) (targetAuth, error)
}

// GitHubConfig holds what NewGitHub needs. All fields are required except
// Logger, HTTPClient and BaseURL.
type GitHubConfig struct {
	Config *config.Config
	// Credentials holds the App keys, the default one and each target's,
	// and the tokens (config.LoadCredentials). NewGitHub keeps none of it:
	// the signers and the token clients hold what they need.
	Credentials config.Credentials
	Clock       github.Clock
	// Secrets is the process's one set: it receives the tokens, App JWTs,
	// installation tokens and message session tokens, and the logger
	// redacts with it (R31, 0003 SPEC R277).
	Secrets    *logging.Secrets
	Logger     *slog.Logger
	SystemInfo scaleset.SystemInfo
	// HTTPClient sends the REST requests; nil means a client on
	// github.NewTransport.
	HTTPClient github.HTTPDoer
	// BaseURL is the REST API root; empty means github.DefaultAPIBaseURL.
	BaseURL string
}

// NewGitHub returns the real GitHub side. Each target uses the credential
// config.Credentials.Target picks: the first it has of its own token file,
// its own App, the default token file and the default App (0003 SPEC R272,
// R273). Targets on one App, by client ID and key, share one appClients:
// its signer, its installation lookups and one REST client per
// installation, whether they name it as the default App or as their own.
// A token target gets its own REST and scale set clients with its token
// (R278). NewGitHub registers every token in Secrets before it makes a
// client.
func NewGitHub(cfg GitHubConfig) (GitHub, error) {
	switch {
	case cfg.Config == nil:
		return nil, errors.New("daemon: nil config")
	case cfg.Secrets == nil:
		return nil, errors.New("daemon: nil secrets")
	}
	targets := cfg.Config.Targets
	creds := make([]config.TargetCredential, len(targets))
	for i := range targets {
		tc, err := cfg.Credentials.Target(cfg.Config, i)
		if err != nil {
			return nil, fmt.Errorf("daemon: %w", err)
		}
		cfg.Secrets.Register(tc.Token)
		creds[i] = tc
	}

	c := clientsConfig{
		clock: cfg.Clock, secrets: cfg.Secrets, systemInfo: cfg.SystemInfo,
		base: cfg.BaseURL, http: cfg.HTTPClient, log: cfg.Logger,
		defaultClientID: cfg.Config.GitHub.AppClientID,
	}
	if c.http == nil {
		c.http = &http.Client{Transport: github.NewTransport()}
	}
	if c.base == "" {
		c.base = github.DefaultAPIBaseURL
	}
	if c.log == nil {
		c.log = slog.New(slog.DiscardHandler)
	}
	g := &realGitHub{
		apps:   make([]*appClients, len(targets)),
		tokens: make([]func() (tokenSource, error), len(targets)),
		keys:   make([]credentialKey, len(targets)),
	}
	var apps []*appClients
	for i, tc := range targets {
		cred := creds[i]
		if cred.Token == "" {
			app, err := sharedApp(&apps, c, cred.AppClientID, cred.AppKey)
			if err != nil {
				return nil, fmt.Errorf("daemon: targets[%d]: %w", i, err)
			}
			g.apps[i] = app
			continue
		}
		t, err := github.ParseTargetURL(tc.URL)
		if err != nil {
			return nil, fmt.Errorf("targets[%d].url: %w", i, err)
		}
		token := cred.Token
		g.tokens[i] = func() (tokenSource, error) {
			return newTokenClients(c, token, t)
		}
		g.keys[i] = tokenKey(token)
	}
	return g, nil
}

// sharedApp returns the appClients in apps for the App with clientID and
// key, making it and adding it to apps when there is none. Keys compare by
// value, so two files holding one key name one App.
func sharedApp(apps *[]*appClients, c clientsConfig, clientID string, key *rsa.PrivateKey) (*appClients, error) {
	for _, a := range *apps {
		if a.clientID == clientID && a.public.Equal(&key.PublicKey) {
			return a, nil
		}
	}
	a, err := newAppClients(c, clientID, key)
	if err != nil {
		return nil, err
	}
	*apps = append(*apps, a)
	return a, nil
}

// clientsConfig is what the clients take from GitHubConfig, with its
// defaults applied.
type clientsConfig struct {
	clock      github.Clock
	secrets    *logging.Secrets
	systemInfo scaleset.SystemInfo
	base       string
	http       github.HTTPDoer
	log        *slog.Logger
	// defaultClientID is github.app-client-id: the App whose installation
	// lookups keep the server's wait.
	defaultClientID string
}

// realGitHub holds each target's App side or what makes its token clients.
// The tokens live only in those functions' closures, which no fmt verb
// prints, and in the clients they make.
type realGitHub struct {
	// apps, tokens and keys are index-aligned with the configured targets.
	// apps holds an App target's shared App and nil for a token target;
	// tokens and keys hold nil and the zero key for an App target.
	apps   []*appClients
	tokens []func() (tokenSource, error)
	keys   []credentialKey
}

func (g *realGitHub) Auth(t github.Target, index int) (targetAuth, error) {
	if index < 0 || index >= len(g.tokens) {
		return nil, fmt.Errorf("daemon: no configured target %d", index)
	}
	if newSource := g.tokens[index]; newSource != nil {
		return &tokenAuth{t: t, key: g.keys[index], newSource: newSource}, nil
	}
	app := g.apps[index]
	if app == nil {
		return nil, fmt.Errorf("daemon: targets[%d] has no credential", index)
	}
	return appAuth{src: app, t: t, app: app.waitID}, nil
}

// appClients is the appSource of one real App: one signer, the App's
// installation lookups and one REST client per installation, shared by
// every target that uses the App. It is safe for concurrent use.
type appClients struct {
	clientID string
	// public identifies the key; the private key stays in the signer.
	public *rsa.PublicKey
	// waitID is the client ID whose installation-lookup wait the App's
	// targets use; empty for the default App's, the server's.
	waitID     string
	signer     *github.AppSigner
	app        *github.App
	log        *slog.Logger
	secrets    *logging.Secrets
	systemInfo scaleset.SystemInfo

	mu sync.Mutex
	// rest holds the REST client of each installation, made on first use,
	// so the targets of one installation share its installation tokens.
	rest map[int64]*github.REST
}

var _ appSource = (*appClients)(nil)

func newAppClients(cfg clientsConfig, clientID string, key *rsa.PrivateKey) (*appClients, error) {
	if key == nil {
		return nil, errors.New("nil App private key")
	}
	signer, err := github.NewAppSigner(clientID, key, cfg.clock, cfg.secrets)
	if err != nil {
		return nil, err
	}
	app, err := github.NewApp(github.AppConfig{
		BaseURL:    cfg.base,
		Signer:     signer,
		HTTPClient: cfg.http,
		Clock:      cfg.clock,
		Secrets:    cfg.secrets,
	})
	if err != nil {
		return nil, err
	}
	a := &appClients{
		clientID:   clientID,
		public:     &key.PublicKey,
		waitID:     clientID,
		signer:     signer,
		app:        app,
		log:        cfg.log,
		secrets:    cfg.secrets,
		systemInfo: cfg.systemInfo,
		rest:       make(map[int64]*github.REST),
	}
	if clientID == cfg.defaultClientID {
		a.waitID = ""
	}
	return a, nil
}

func (a *appClients) Installation(ctx context.Context, t github.Target) (github.Installation, error) {
	return a.app.Installation(ctx, t)
}

func (a *appClients) Target(t github.Target, installationID int64) (TargetGitHub, error) {
	rest, err := a.installationREST(installationID)
	if err != nil {
		return nil, err
	}
	ss, err := github.NewScaleSetClient(github.ScaleSetConfig{
		ConfigURL:      t.ConfigURL(),
		InstallationID: installationID,
		Signer:         a.signer,
		Logger:         a.log.With("target", t.Key(), "component", "scaleset"),
		Secrets:        a.secrets,
		SystemInfo:     a.systemInfo,
	})
	if err != nil {
		return nil, err
	}
	return newTargetGitHub(ss, rest, t), nil
}

// installationREST returns the App's REST client for installationID.
func (a *appClients) installationREST(installationID int64) (*github.REST, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if rest := a.rest[installationID]; rest != nil {
		return rest, nil
	}
	rest, err := a.app.REST(installationID)
	if err != nil {
		return nil, err
	}
	a.rest[installationID] = rest
	return rest, nil
}

// tokenClients is the tokenSource of one token target: its REST and scale
// set clients, made with its token at each start, as an App target's are.
type tokenClients struct {
	rest *github.REST
	ss   *github.ScaleSetClient
}

var _ tokenSource = (*tokenClients)(nil)

func newTokenClients(cfg clientsConfig, token string, t github.Target) (tokenSource, error) {
	rest, err := github.NewTokenREST(github.TokenRESTConfig{
		BaseURL:    cfg.base,
		Token:      token,
		HTTPClient: cfg.http,
		Clock:      cfg.clock,
		Secrets:    cfg.secrets,
	})
	if err != nil {
		return nil, err
	}
	ss, err := github.NewScaleSetClient(github.ScaleSetConfig{
		ConfigURL:  t.ConfigURL(),
		Token:      token,
		Logger:     cfg.log.With("target", t.Key(), "component", "scaleset"),
		Secrets:    cfg.secrets,
		SystemInfo: cfg.systemInfo,
	})
	if err != nil {
		return nil, err
	}
	return &tokenClients{rest: rest, ss: ss}, nil
}

func (c *tokenClients) CheckRepositoryAccess(ctx context.Context, t github.Target) (github.Access, error) {
	return c.rest.CheckRepositoryAccess(ctx, t)
}

func (c *tokenClients) TokenTarget(t github.Target) (TargetGitHub, error) {
	return newTargetGitHub(c.ss, c.rest, t), nil
}

// scaleSetClient is the part of *github.ScaleSetClient a target uses.
type scaleSetClient interface {
	GetOrCreateScaleSetInGroup(ctx context.Context, groupID int, name string) (*scaleset.RunnerScaleSet, error)
	GetScaleSetInGroup(ctx context.Context, groupID int, name string) (*scaleset.RunnerScaleSet, error)
	SetSystemInfo(info scaleset.SystemInfo)
	GenerateJIT(ctx context.Context, scaleSetID int, name string) (github.JITConfig, error)
	GetRunnerByName(ctx context.Context, name string) (*github.Runner, error)
	RemoveRunner(ctx context.Context, id int64) error
	CreateSession(ctx context.Context, scaleSetID int, owner string) (*github.Session, error)
}

// restClient is the part of *github.REST a target uses.
type restClient interface {
	ListRunners(ctx context.Context, t github.Target) ([]github.Runner, error)
	FindRunner(ctx context.Context, t github.Target, name string) (github.Runner, bool, error)
}

var (
	_ scaleSetClient = (*github.ScaleSetClient)(nil)
	_ restClient     = (*github.REST)(nil)
)

// targetGitHub adapts the github clients to TargetGitHub for one target.
type targetGitHub struct {
	ss     scaleSetClient
	rest   restClient
	target github.Target
}

func newTargetGitHub(ss scaleSetClient, rest restClient, t github.Target) *targetGitHub {
	return &targetGitHub{ss: ss, rest: rest, target: t}
}

var _ TargetGitHub = (*targetGitHub)(nil)

func (g *targetGitHub) FindScaleSet(ctx context.Context, groupID int, name string) (int, bool, error) {
	set, err := g.ss.GetScaleSetInGroup(ctx, groupID, name)
	if err != nil || set == nil {
		return 0, false, err
	}
	return set.ID, true, nil
}

func (g *targetGitHub) GetOrCreateScaleSet(ctx context.Context, groupID int, name string) (int, error) {
	set, err := g.ss.GetOrCreateScaleSetInGroup(ctx, groupID, name)
	if err != nil {
		return 0, err
	}
	return set.ID, nil
}

func (g *targetGitHub) SetSystemInfo(info scaleset.SystemInfo) { g.ss.SetSystemInfo(info) }

func (g *targetGitHub) GenerateJITConfig(ctx context.Context, scaleSetID int, name string) (scaler.JITRunner, error) {
	jit, err := g.ss.GenerateJIT(ctx, scaleSetID, name)
	if err != nil {
		return scaler.JITRunner{}, err
	}
	return scaler.JITRunner{ID: jit.RunnerID, Config: scaler.NewJITConfig(jit.Encoded)}, nil
}

func (g *targetGitHub) GetRunnerByName(ctx context.Context, name string) (scaler.RegisteredRunner, bool, error) {
	r, err := g.ss.GetRunnerByName(ctx, name)
	if err != nil || r == nil {
		return scaler.RegisteredRunner{}, false, err
	}
	return scaler.RegisteredRunner{ID: r.ID, Name: r.Name, ScaleSetID: r.ScaleSetID}, true, nil
}

func (g *targetGitHub) RemoveRunner(ctx context.Context, id int64) error {
	return g.ss.RemoveRunner(ctx, id)
}

func (g *targetGitHub) CreateSession(ctx context.Context, scaleSetID int, owner string) (Session, error) {
	s, err := g.ss.CreateSession(ctx, scaleSetID, owner)
	if err != nil {
		return nil, err
	}
	if s == nil {
		return nil, errors.New("github: no session")
	}
	return s, nil
}

func (g *targetGitHub) ListRunners(ctx context.Context) ([]scaler.ListedRunner, error) {
	rs, err := g.rest.ListRunners(ctx, g.target)
	if err != nil {
		return nil, err
	}
	out := make([]scaler.ListedRunner, len(rs))
	for i, r := range rs {
		out[i] = scaler.ListedRunner{ID: r.ID, Name: r.Name, Busy: r.Busy}
	}
	return out, nil
}

func (g *targetGitHub) FindRunner(ctx context.Context, name string) (scaler.ListedRunner, bool, error) {
	r, found, err := g.rest.FindRunner(ctx, g.target, name)
	if err != nil || !found {
		return scaler.ListedRunner{}, false, err
	}
	return scaler.ListedRunner{ID: r.ID, Name: r.Name, Busy: r.Busy}, true, nil
}

// boundScaleSet is scaler.ScaleSetAPI for one scale set of a target.
type boundScaleSet struct {
	gh TargetGitHub
	id int
}

var _ scaler.ScaleSetAPI = boundScaleSet{}

func (b boundScaleSet) GenerateJITConfig(ctx context.Context, name string) (scaler.JITRunner, error) {
	return b.gh.GenerateJITConfig(ctx, b.id, name)
}

func (b boundScaleSet) RemoveRunner(ctx context.Context, id int64) error {
	return b.gh.RemoveRunner(ctx, id)
}

// jobCheck is the job check server's gate.Authenticator over the runner
// table and every target's judge (0002 PLAN M8-6). It finds the row by
// runner name and passes the report to the judge of the row's target
// (0002 PLAN M12-7). Neither calls GitHub, and Record does not wait for
// Docker, so a report is judged whatever the sessions' state (0002 SPEC
// R123, R133).
type jobCheck struct {
	t *scaler.Table
	// judges holds each target's judge by target key.
	judges map[string]*scaler.JobJudge
}

var _ gate.Authenticator = jobCheck{}

// Authenticate checks the token against the table (R14, R40) and converts
// the verdict.
func (a jobCheck) Authenticate(name, presented string, at time.Time) (gate.Verdict, bool) {
	v, ok := a.t.Authenticate(name, presented, at)
	if !ok {
		return gate.VerdictPending, false
	}
	return gateVerdict(v), true
}

// Finalizing reports whether finalize has begun, or the end cleanup of the
// error drain of the runner's target (0002 SPEC R131, 0002 PLAN M13-2), so
// another target's first report is still judged.
func (a jobCheck) Finalizing(name string) bool { return a.t.FinalizingRunner(name) }

// Record records the verdict through the judge of the runner's target
// (0002 SPEC R131). A runner no longer in the table stays pending, the
// answer that never lets a step run (R10).
func (a jobCheck) Record(name, event string, d gate.Decision) gate.Verdict {
	info, ok := a.t.Get(name)
	if !ok {
		return gate.VerdictPending
	}
	j := a.judges[info.Target]
	if j == nil {
		return gate.VerdictPending
	}
	return gateVerdict(j.Record(name, event, d))
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
