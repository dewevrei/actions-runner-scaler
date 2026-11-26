package main

import (
	"context"
	"crypto/rsa"
	"errors"
	"fmt"
	"io"
	"log/slog"
	mathrand "math/rand/v2"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/actions/scaleset"

	"github.com/dewevrei/actions-runner-scaler/internal/config"
	"github.com/dewevrei/actions-runner-scaler/internal/daemon"
	"github.com/dewevrei/actions-runner-scaler/internal/docker"
	"github.com/dewevrei/actions-runner-scaler/internal/github"
	"github.com/dewevrei/actions-runner-scaler/internal/retry"
	"github.com/dewevrei/actions-runner-scaler/internal/scaler"
)

const (
	// deleteCallTimeout bounds one call of the command, as the scaler's
	// own calls are bounded. A list of the repository's runners counts as
	// one call.
	deleteCallTimeout = 30 * time.Second
	// deleteCallRetries is how many times a transient or rate-limited call
	// is repeated, as R241 does for its lookups.
	deleteCallRetries = 3
)

// waitTimeLayout formats the time a wait ends.
const waitTimeLayout = "2006-01-02 15:04:05"

// Messages printed on stdout, the way status prints its outcome.
const (
	// msgRateLimited is printed with the time the wait for a rate limit
	// ends, in waitTimeLayout.
	msgRateLimited = "%s까지 대기 (속도 제한)"
	msgLocked      = "다른 프로세스가 실행 중이거나 오류 대기 중이다(잠금이 잡혀 있음). 아무것도 지우지 않음"
	// msgContainersLeft follows the names of the runners whose containers
	// are alive (PLAN M6-1).
	msgContainersLeft = "남은 runner 컨테이너를 지운 뒤 다시 실행"
)

// msgCredentialArg refuses an argument holding a credential without quoting
// it (0003 SPEC R314, R277).
const msgCredentialArg = "delete-scaleset: value contains a credential; set the repository URL only"

// deleteGitHub is the App side of delete-scaleset: the installation lookup
// and the target's clients.
type deleteGitHub interface {
	// Installation looks up the App's installation on t (0002 SPEC R236).
	Installation(ctx context.Context, t github.Target) (github.Installation, error)
	// Target makes the clients of t for installationID.
	Target(t github.Target, installationID int64) (scaleSetAPI, error)
}

// deleteTokenGitHub is the token side of delete-scaleset for one token
// target: 0003 SPEC R279's check and the target's clients, both made with
// its token.
type deleteTokenGitHub interface {
	// CheckRepositoryAccess makes R279's call with the token.
	CheckRepositoryAccess(ctx context.Context, t github.Target) (github.Access, error)
	// Target makes the clients of t with the token.
	Target(t github.Target) (scaleSetAPI, error)
}

// scaleSetAPI is the GitHub side of one target that delete-scaleset uses.
// *github.ScaleSetClient provides all but ListRunners, which githubTarget
// binds to the target's repository.
type scaleSetAPI interface {
	// GetScaleSetInGroup returns the scale set called name in runner group
	// groupID, nil when there is none, and an error marked
	// retry.ErrAmbiguousScaleSet when several share the name.
	GetScaleSetInGroup(ctx context.Context, groupID int, name string) (*scaleset.RunnerScaleSet, error)
	DeleteScaleSet(ctx context.Context, id int) error
	// ListRunners lists every runner of the target's repository (0002 SPEC
	// R241).
	ListRunners(ctx context.Context) ([]github.Runner, error)
	// GetRunnerByName returns nil when no runner has the name.
	GetRunnerByName(ctx context.Context, name string) (*github.Runner, error)
	RemoveRunner(ctx context.Context, id int64) error
}

// containerLister lists the scaler's Docker resources (*docker.Client).
type containerLister interface {
	List(ctx context.Context) ([]docker.Resource, error)
}

var _ containerLister = (*docker.Client)(nil)

// deleteScaleSetDeps is what delete-scaleset takes from the machine, so
// tests run it on fakes.
type deleteScaleSetDeps struct {
	// Lock takes the single-run lock (R78) without waiting. It returns
	// daemon.ErrLocked when another process holds it.
	Lock func() (unlock func(), err error)
	// ConfigPath is the configuration file's path, named in the error for a
	// URL that is not in it (0002 SPEC R269).
	ConfigPath string
	// LoadConfig reads and validates the configuration and its credentials.
	LoadConfig func() (*config.Config, config.Credentials, error)
	// GitHub makes the App side of the command. It is called only for an
	// App target.
	GitHub func(cfg *config.Config, creds config.Credentials) (deleteGitHub, error)
	// Token makes the token side for a token target's token (0003 SPEC
	// R281).
	Token func(token string) (deleteTokenGitHub, error)
	// Docker connects to Docker. It is called only when a target runner is
	// registered, so a missing Docker does not stop a deletion with no
	// registrations. The command closes the result if it is an io.Closer.
	Docker func() (containerLister, error)
	// Clock and Jitter pace the retries of transient errors.
	Clock  retry.Clock
	Jitter func() float64
}

func deleteScaleSetCmd(ctx context.Context, e env, args []string) error {
	fs := newFlagSet("delete-scaleset", e)
	configPath := fs.String("config", defaultConfigPath(), "path to the TOML config file")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return badUsage("usage: delete-scaleset [-config path] <repository URL>")
	}
	deps, err := newDeleteScaleSetDeps(e, *configPath)
	if err != nil {
		return err
	}
	return runDeleteScaleSet(ctx, e, deps, fs.Arg(0))
}

// newDeleteScaleSetDeps wires the real lock, configuration, GitHub and
// Docker.
func newDeleteScaleSetDeps(e env, configPath string) (deleteScaleSetDeps, error) {
	lockPath, err := daemon.DefaultLockPath()
	if err != nil {
		return deleteScaleSetDeps{}, err
	}
	clock := scaler.SystemClock()
	return deleteScaleSetDeps{
		ConfigPath: configPath,
		Lock:       func() (func(), error) { return daemon.LockFile(lockPath) },
		LoadConfig: func() (*config.Config, config.Credentials, error) { return loadConfig(configPath, e.secrets) },
		GitHub: func(cfg *config.Config, creds config.Credentials) (deleteGitHub, error) {
			return newGitHubApp(e, clock, cfg, creds.AppKey)
		},
		Token: func(token string) (deleteTokenGitHub, error) {
			return newGitHubToken(e, clock, github.DefaultAPIBaseURL, &http.Client{Transport: github.NewTransport()}, token)
		},
		Docker: func() (containerLister, error) {
			engine, err := docker.NewEngine(docker.DefaultHost)
			if err != nil {
				return nil, fmt.Errorf("docker client: %w", err)
			}
			return engineLister{Client: docker.New(engine), engine: engine}, nil
		},
		Clock:  clock,
		Jitter: mathrand.Float64,
	}, nil
}

// engineLister is a docker.Client that also closes the Engine client it was
// made from.
type engineLister struct {
	*docker.Client
	engine io.Closer
}

func (l engineLister) Close() error { return l.engine.Close() }

// githubApp is deleteGitHub over the real App.
type githubApp struct {
	e      env
	app    *github.App
	signer *github.AppSigner
}

var _ deleteGitHub = githubApp{}

func (g githubApp) Installation(ctx context.Context, t github.Target) (github.Installation, error) {
	return g.app.Installation(ctx, t)
}

// Target makes the REST and scale set clients of t, sharing the App's one
// signer.
func (g githubApp) Target(t github.Target, installationID int64) (scaleSetAPI, error) {
	rest, err := g.app.REST(installationID)
	if err != nil {
		return nil, err
	}
	ss, err := github.NewScaleSetClient(github.ScaleSetConfig{
		ConfigURL:      t.ConfigURL(),
		InstallationID: installationID,
		Signer:         g.signer,
		Logger:         g.e.logger.With("target", t.Key(), "component", "scaleset"),
		Secrets:        g.e.secrets,
		SystemInfo:     scaleset.SystemInfo{System: systemName, Version: version, Subsystem: "delete-scaleset"},
	})
	if err != nil {
		return nil, err
	}
	return githubTarget{ScaleSetClient: ss, rest: rest, target: t}, nil
}

// githubTarget is scaleSetAPI over the real clients of one target.
type githubTarget struct {
	*github.ScaleSetClient
	rest   *github.REST
	target github.Target
}

var _ scaleSetAPI = githubTarget{}

func (g githubTarget) ListRunners(ctx context.Context) ([]github.Runner, error) {
	return g.rest.ListRunners(ctx, g.target)
}

// githubToken is deleteTokenGitHub over the real clients of one token.
type githubToken struct {
	rest *github.REST
	// newScaleSet makes a scale set client with the token, which lives only
	// in this closure so that no fmt verb prints it.
	newScaleSet func(t github.Target) (*github.ScaleSetClient, error)
}

var _ deleteTokenGitHub = githubToken{}

// newGitHubToken makes the token's REST client; the scale set client waits
// for Target, after the check.
func newGitHubToken(e env, clock github.Clock, baseURL string, httpClient github.HTTPDoer, token string) (deleteTokenGitHub, error) {
	rest, err := github.NewTokenREST(github.TokenRESTConfig{
		BaseURL:    baseURL,
		Token:      token,
		HTTPClient: httpClient,
		Clock:      clock,
		Secrets:    e.secrets,
	})
	if err != nil {
		return nil, err
	}
	return githubToken{
		rest: rest,
		newScaleSet: func(t github.Target) (*github.ScaleSetClient, error) {
			return github.NewScaleSetClient(github.ScaleSetConfig{
				ConfigURL:  t.ConfigURL(),
				Token:      token,
				Logger:     e.logger.With("target", t.Key(), "component", "scaleset"),
				Secrets:    e.secrets,
				SystemInfo: scaleset.SystemInfo{System: systemName, Version: version, Subsystem: "delete-scaleset"},
			})
		},
	}, nil
}

func (g githubToken) CheckRepositoryAccess(ctx context.Context, t github.Target) (github.Access, error) {
	return g.rest.CheckRepositoryAccess(ctx, t)
}

func (g githubToken) Target(t github.Target) (scaleSetAPI, error) {
	ss, err := g.newScaleSet(t)
	if err != nil {
		return nil, err
	}
	return githubTarget{ScaleSetClient: ss, rest: g.rest, target: t}, nil
}

// newGitHubApp makes the App with its one signer.
//
// daemon.NewGitHub does the same wiring, but its TargetGitHub has no
// DeleteScaleSet, so delete-scaleset wires its own.
func newGitHubApp(e env, clock scaler.Clock, cfg *config.Config, key *rsa.PrivateKey) (deleteGitHub, error) {
	signer, err := github.NewAppSigner(cfg.GitHub.AppClientID, key, clock, e.secrets)
	if err != nil {
		return nil, err
	}
	app, err := github.NewApp(github.AppConfig{
		BaseURL:    github.DefaultAPIBaseURL,
		Signer:     signer,
		HTTPClient: &http.Client{Transport: github.NewTransport()},
		Clock:      clock,
		Secrets:    e.secrets,
	})
	if err != nil {
		return nil, err
	}
	return githubApp{e: e, app: app, signer: signer}, nil
}

// runDeleteScaleSet deletes the scale set of the configured repository whose
// URL is targetURL and its runner registrations (0002 SPEC R268). It deletes
// nothing and returns an error when the lock is held (0001 R49), the
// configuration is invalid, the argument holds a credential (0003 SPEC
// R314), the repository is not in it, the installation or its permission
// cannot be found (R269) or GitHub refuses a token target's token (0003 SPEC
// R281), several scale sets share the name,
// or a registered runner still has a live container; the outcome of a held
// lock, duplicates and a live container is then already on stdout. When
// there is no scale set it says so and returns nil (R246).
func runDeleteScaleSet(ctx context.Context, e env, d deleteScaleSetDeps, targetURL string) error {
	unlock, err := d.Lock()
	if errors.Is(err, daemon.ErrLocked) {
		fmt.Fprintln(e.stdout, msgLocked)
		return reportedError{err: err}
	}
	if err != nil {
		return err
	}
	defer unlock()

	cfg, creds, err := d.LoadConfig()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	target, index, err := findTarget(cfg, d.ConfigPath, targetURL)
	if err != nil {
		return err
	}
	del := &scaleSetDeleter{
		label:     cfg.Scaling.Label,
		newDocker: d.Docker,
		out:       e.stdout,
		log:       e.logger.With("target", target.Key()),
		clock:     d.Clock,
		jitter:    d.Jitter,
	}
	if token := targetToken(creds, index); token != "" {
		err = del.connectToken(ctx, d.Token, token, target)
	} else {
		err = del.connectApp(ctx, d.GitHub, cfg, creds, target)
	}
	if err != nil {
		return err
	}
	return del.run(ctx)
}

// targetToken returns the token of the configured target at index: its own
// token-file's, else github.token-file's; empty means the App (0003 SPEC
// R272, R273).
func targetToken(creds config.Credentials, index int) string {
	if index < len(creds.TargetTokens) && creds.TargetTokens[index] != "" {
		return creds.TargetTokens[index]
	}
	return creds.DefaultToken
}

// findTarget returns the configured target whose repository equals that of
// arg, ignoring case (0002 SPEC R233's comparison), and its index in
// cfg.Targets. An argument holding a credential is refused first, without
// quoting it (0003 SPEC R314). An argument that is not a repository URL, an
// organization URL included, and a repository that is not configured get
// the R269 error, which names the configuration file and its targets. It
// makes no GitHub call.
func findTarget(cfg *config.Config, configPath, arg string) (github.Target, int, error) {
	if config.ContainsCredential(arg) {
		return github.Target{}, 0, errors.New(msgCredentialArg)
	}
	want, err := github.ParseTargetURL(arg)
	if err == nil {
		for i, tc := range cfg.Targets {
			if t, err := github.ParseTargetURL(tc.URL); err == nil && t.Key() == want.Key() {
				return t, i, nil
			}
		}
	}
	urls := make([]string, len(cfg.Targets))
	for i, tc := range cfg.Targets {
		urls[i] = tc.URL
	}
	return github.Target{}, 0, fmt.Errorf("target %q is not in the config %s (targets: %s); delete-scaleset deletes only a repository listed in [[targets]]",
		arg, configPath, strings.Join(urls, ", "))
}

// scaleSetDeleter is one delete-scaleset run after the lock, the
// configuration and the target are settled.
type scaleSetDeleter struct {
	// label is the scale set name (scaling.label).
	label string
	// gh is set by connectApp or connectToken.
	gh        scaleSetAPI
	newDocker func() (containerLister, error)
	out       io.Writer
	log       *slog.Logger
	clock     retry.Clock
	jitter    func() float64
}

// connectApp makes the App side, looks the installation of target up and
// checks its permission (0002 SPEC R236, R238). Any failure ends the
// command before anything is deleted (R269). Transient errors keep the
// command's retry.
func (s *scaleSetDeleter) connectApp(ctx context.Context, newApp func(*config.Config, config.Credentials) (deleteGitHub, error),
	cfg *config.Config, creds config.Credentials, target github.Target) error {
	app, err := newApp(cfg, creds)
	if err != nil {
		return fmt.Errorf("github client: %w", err)
	}
	var inst github.Installation
	err = s.call(ctx, retry.OpGetInstallation, func(ctx context.Context) (err error) {
		inst, err = app.Installation(ctx, target)
		return err
	})
	if err != nil {
		return fmt.Errorf("look up the installation on %s: %w", target, err)
	}
	if err := github.CheckPermission(inst, target); err != nil {
		return err
	}
	s.gh, err = app.Target(target, inst.ID)
	if err != nil {
		return fmt.Errorf("github client: %w", err)
	}
	return nil
}

// connectToken makes the token side and 0003 SPEC R279's call with the
// token of a token target in place of the installation lookup and
// permission check (R281), then the target's clients with that token. Any
// failure ends the command before anything is deleted; a refusal's error is
// R279's sentence alone. Transient errors keep the command's retry.
func (s *scaleSetDeleter) connectToken(ctx context.Context, newToken func(string) (deleteTokenGitHub, error),
	token string, target github.Target) error {
	tg, err := newToken(token)
	if err != nil {
		return fmt.Errorf("github client: %w", err)
	}
	err = s.call(ctx, retry.OpCheckTokenAccess, func(ctx context.Context) error {
		// The answer's scopes and expiration are doctor's (0003 SPEC R301,
		// R317).
		_, err := tg.CheckRepositoryAccess(ctx, target)
		return err
	})
	if r, ok := asTokenRefusal(err); ok {
		return r
	}
	if err != nil {
		return fmt.Errorf("check the token's access to %s: %w", target, err)
	}
	s.gh, err = tg.Target(target)
	if err != nil {
		return fmt.Errorf("github client: %w", err)
	}
	return nil
}

// tokenRefusal is a token GitHub refused at 0003 SPEC R279's call. Its text
// is R279's sentence alone (0003 PLAN M30); Unwrap keeps the operation and
// the status.
type tokenRefusal struct {
	text string
	err  error
}

func (r *tokenRefusal) Error() string { return r.text }
func (r *tokenRefusal) Unwrap() error { return r.err }

// asTokenRefusal returns err as a tokenRefusal when it is R279's 401, 403 or
// 404, which CheckRepositoryAccess marks with retry.ErrTargetSetup and whose
// StatusError Message is R279's sentence.
func asTokenRefusal(err error) (*tokenRefusal, bool) {
	var se *github.StatusError
	if !errors.Is(err, retry.ErrTargetSetup) || !errors.As(err, &se) || se.Message == "" {
		return nil, false
	}
	return &tokenRefusal{text: se.Message, err: err}, true
}

// run deletes the target's scale set, which is in the default runner group
// (0002 SPEC R239), and says so when there is none (R246). Nothing is deleted
// before the scale set is found unique and the label's runners are known to
// have no live container.
func (s *scaleSetDeleter) run(ctx context.Context) error {
	var set *scaleset.RunnerScaleSet
	err := s.call(ctx, retry.OpGetRunnerScaleSet, func(ctx context.Context) (err error) {
		set, err = s.gh.GetScaleSetInGroup(ctx, github.DefaultRunnerGroupID, s.label)
		return err
	})
	switch {
	case errors.Is(err, retry.ErrAmbiguousScaleSet):
		fmt.Fprintf(s.out, "같은 이름의 scale set이 여럿이다: %s. 하나만 남긴 뒤 다시 실행. 아무것도 지우지 않음\n", s.label)
		return reportedError{err: err}
	case err != nil:
		return fmt.Errorf("find scale set %q: %w", s.label, err)
	case set == nil:
		fmt.Fprintf(s.out, "scale set 없음: %s. 지울 것이 없음\n", s.label)
		return nil
	}

	members, err := s.members(ctx, set.ID)
	if err != nil {
		return err
	}
	live, err := s.liveContainers(ctx, members)
	if err != nil {
		return err
	}
	if len(live) > 0 {
		for _, name := range live {
			fmt.Fprintf(s.out, "컨테이너가 남은 runner: %s\n", name)
		}
		fmt.Fprintln(s.out, msgContainersLeft)
		return reportedError{err: errors.New("runner containers are still alive")}
	}

	if err := s.removeRunners(ctx, members); err != nil {
		return err
	}
	attempts := 0
	err = s.call(ctx, retry.OpDeleteScaleSet, func(ctx context.Context) error {
		attempts++
		return s.gh.DeleteScaleSet(ctx, set.ID)
	})
	// A retry can find the scale set gone because an earlier attempt
	// succeeded and only its response was lost. The lookup below confirms
	// it; a 404 on the first attempt is not explained so and fails.
	if attempts > 1 && isNotFound(err) {
		s.log.Info("scale set is gone after a retry; confirming", "id", set.ID, "err", err)
		err = nil
	}
	if err != nil {
		return fmt.Errorf("delete scale set %q (id %d): %w", s.label, set.ID, err)
	}
	var left *scaleset.RunnerScaleSet
	err = s.call(ctx, retry.OpGetRunnerScaleSet, func(ctx context.Context) (err error) {
		left, err = s.gh.GetScaleSetInGroup(ctx, github.DefaultRunnerGroupID, s.label)
		return err
	})
	switch {
	case err != nil:
		return fmt.Errorf("confirm scale set %q is deleted: %w", s.label, err)
	case left != nil:
		return fmt.Errorf("scale set %q (id %d) still exists after the delete", s.label, left.ID)
	}
	fmt.Fprintf(s.out, "scale set 삭제됨: %s (ID %d), runner 등록 %d개 제거\n", s.label, set.ID, len(members))
	return nil
}

// members returns the registrations that belong to the target's scale set,
// found the R241 way: runners of the repository named like the label's
// runners whose scale set is setID, by name. Other runners of the
// repository, and registrations of another scale set that happen to have
// such a name, are left out. A failed lookup ends the run, since the membership is then
// unknown and nothing has been deleted yet.
func (s *scaleSetDeleter) members(ctx context.Context, setID int) ([]github.Runner, error) {
	var listed []github.Runner
	err := s.call(ctx, retry.OpListRepoRunners, func(ctx context.Context) (err error) {
		listed, err = s.gh.ListRunners(ctx)
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("list repository runners: %w", err)
	}
	var members []github.Runner
	for _, rr := range listed {
		if !scaler.RunnerName(s.label, rr.Name) {
			continue
		}
		var reg *github.Runner
		err := s.call(ctx, retry.OpGetRunnerByName, func(ctx context.Context) (err error) {
			reg, err = s.gh.GetRunnerByName(ctx, rr.Name)
			return err
		})
		if err != nil {
			return nil, fmt.Errorf("look up runner %q: %w", rr.Name, err)
		}
		if reg != nil && reg.ScaleSetID == setID {
			members = append(members, *reg)
		}
	}
	slices.SortFunc(members, func(a, b github.Runner) int { return strings.Compare(a.Name, b.Name) })
	return members, nil
}

// liveContainers returns the names of the members that have a live runner
// container. As in R241, a container is live unless it has ended.
func (s *scaleSetDeleter) liveContainers(ctx context.Context, members []github.Runner) ([]string, error) {
	if len(members) == 0 {
		return nil, nil
	}
	dk, err := s.newDocker()
	if err != nil {
		return nil, err
	}
	if c, ok := dk.(io.Closer); ok {
		defer func() {
			if err := c.Close(); err != nil {
				s.log.Warn("close docker client", "err", err)
			}
		}()
	}
	var res []docker.Resource
	err = s.call(ctx, retry.OpContainerList, func(ctx context.Context) (err error) {
		res, err = dk.List(ctx)
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("list runner containers: %w", err)
	}
	alive := make(map[string]bool)
	for _, r := range res {
		if r.Kind == docker.KindRunner && r.State != "exited" && r.State != "dead" {
			alive[r.RunnerName] = true
		}
	}
	var live []string
	for _, m := range members {
		if alive[m.Name] {
			live = append(live, m.Name)
		}
	}
	return live, nil
}

// removeRunners removes every member's registration. A registration that is
// gone already counts as removed. A failure does not stop the others, but
// the scale set is then not deleted, and the failures are returned together.
func (s *scaleSetDeleter) removeRunners(ctx context.Context, members []github.Runner) error {
	var errs []error
	for _, m := range members {
		err := s.call(ctx, retry.OpRemoveRunner, func(ctx context.Context) error {
			return s.gh.RemoveRunner(ctx, m.ID)
		})
		if err != nil && retry.Classify(ctx, retry.OpRemoveRunner, err) != retry.Gone {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			errs = append(errs, fmt.Errorf("remove runner %q (id %d): %w", m.Name, m.ID, err))
			continue
		}
		s.log.Info("runner registration removed", "runner", m.Name, "runnerId", m.ID)
	}
	return errors.Join(errs...)
}

// call runs fn under deleteCallTimeout. A transient or rate-limited error is
// repeated up to deleteCallRetries times after the backoff, which waits out
// a rate limit's hint (R52) and says on stdout until when; any other error
// is returned as it is.
func (s *scaleSetDeleter) call(ctx context.Context, op retry.Op, fn func(context.Context) error) error {
	limited := false
	clock := announceClock{Clock: s.clock, announce: func(until time.Time) {
		if limited {
			fmt.Fprintf(s.out, msgRateLimited+"\n", until.Format(waitTimeLayout))
		}
	}}
	backoff, err := retry.NewDefaultBackoff(clock, s.jitter)
	if err != nil {
		return err
	}
	for attempt := 0; ; attempt++ {
		callCtx, cancel := context.WithTimeout(ctx, deleteCallTimeout)
		err := fn(callCtx)
		cancel()
		if err == nil {
			return nil
		}
		class := retry.Classify(ctx, op, err)
		if class == retry.Canceled {
			return ctx.Err()
		}
		if (class != retry.Transient && class != retry.RateLimited) || attempt == deleteCallRetries {
			return err
		}
		s.log.Warn("call failed; retrying", "op", op, "class", class, "attempt", attempt+1, "err", err)
		limited = class == retry.RateLimited
		if err := backoff.Wait(ctx, err); err != nil {
			return err
		}
	}
}

// announceClock tells announce when each wait ends, as the backoff starts
// it, so the wait the backoff picked for a rate limit's hint is the one
// reported.
type announceClock struct {
	retry.Clock
	announce func(until time.Time)
}

func (c announceClock) After(d time.Duration) <-chan time.Time {
	c.announce(c.Now().Add(d))
	return c.Clock.After(d)
}

// isNotFound reports whether err carries an HTTP 404.
func isNotFound(err error) bool {
	var sc retry.StatusCoder
	return errors.As(err, &sc) && sc.HTTPStatus() == http.StatusNotFound
}
