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
	"regexp"
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
	// is repeated, as R17 does for its lookups.
	deleteCallRetries = 3
)

// Messages printed on stdout, the way status prints its outcome.
const (
	msgLocked = "다른 스케일러가 실행 중이거나 오류 대기 중이다(잠금이 잡혀 있음). 아무것도 지우지 않음"
	// msgContainersLeft follows the names of the runners whose containers
	// are alive (PLAN M6-1).
	msgContainersLeft = "남은 runner 컨테이너를 지운 뒤 다시 실행"
)

// scaleSetAPI is the GitHub side of one pool that delete-scaleset uses.
// *github.ScaleSetClient provides all but ListRepoRunners, which githubPool
// binds to the pool's repository.
type scaleSetAPI interface {
	// GetScaleSet returns the pool's scale set, nil when there is none, and
	// an error marked retry.ErrAmbiguousScaleSet when several share the name.
	GetScaleSet(ctx context.Context, name string) (*scaleset.RunnerScaleSet, error)
	DeleteScaleSet(ctx context.Context, id int) error
	// ListRepoRunners lists every runner of the pool's repository.
	ListRepoRunners(ctx context.Context) ([]github.RepoRunner, error)
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
	// LoadConfig reads and validates the configuration and the App key.
	LoadConfig func() (*config.Config, *rsa.PrivateKey, error)
	// GitHub makes the GitHub side of pool.
	GitHub func(cfg *config.Config, key *rsa.PrivateKey, pool config.Pool) (scaleSetAPI, error)
	// Docker connects to Docker. It is called only when a pool runner is
	// registered, so a missing Docker does not stop a deletion with no
	// registrations.
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
		return badUsage("usage: delete-scaleset [-config path] <pool name>")
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
		Lock:       func() (func(), error) { return daemon.LockFile(lockPath) },
		LoadConfig: func() (*config.Config, *rsa.PrivateKey, error) { return loadConfig(configPath) },
		GitHub: func(cfg *config.Config, key *rsa.PrivateKey, pool config.Pool) (scaleSetAPI, error) {
			return newGitHubPool(e, clock, cfg, key, pool)
		},
		Docker: func() (containerLister, error) {
			engine, err := docker.NewEngine(docker.DefaultHost)
			if err != nil {
				return nil, fmt.Errorf("docker client: %w", err)
			}
			return docker.New(engine), nil
		},
		Clock:  clock,
		Jitter: mathrand.Float64,
	}, nil
}

// githubPool is scaleSetAPI over the real clients of one pool.
type githubPool struct {
	*github.ScaleSetClient
	rest        *github.REST
	owner, repo string
}

var _ scaleSetAPI = githubPool{}

func (g githubPool) ListRepoRunners(ctx context.Context) ([]github.RepoRunner, error) {
	return g.rest.ListRepoRunners(ctx, g.owner, g.repo)
}

// newGitHubPool makes the scale set and REST clients of pool, sharing one
// App signer.
//
// daemon.NewGitHub does the same wiring, but its PoolGitHub has no
// DeleteScaleSet, so delete-scaleset wires its own.
func newGitHubPool(e env, clock scaler.Clock, cfg *config.Config, key *rsa.PrivateKey, pool config.Pool) (scaleSetAPI, error) {
	owner, repo, err := github.ParseRepoURL(pool.RepoURL)
	if err != nil {
		return nil, err
	}
	signer, err := github.NewAppSigner(cfg.GitHub.AppClientID, key, clock, e.secrets)
	if err != nil {
		return nil, err
	}
	rest, err := github.NewREST(github.RESTConfig{
		BaseURL:        github.DefaultAPIBaseURL,
		InstallationID: cfg.GitHub.AppInstallationID,
		Signer:         signer,
		HTTPClient:     &http.Client{Transport: github.NewTransport()},
		Clock:          clock,
		Secrets:        e.secrets,
	})
	if err != nil {
		return nil, err
	}
	ss, err := github.NewScaleSetClient(github.ScaleSetConfig{
		RepoURL:        pool.RepoURL,
		InstallationID: cfg.GitHub.AppInstallationID,
		Signer:         signer,
		Logger:         e.logger.With("pool", pool.Name, "component", "scaleset"),
		Secrets:        e.secrets,
		SystemInfo:     scaleset.SystemInfo{System: systemName, Version: version, Subsystem: "delete-scaleset"},
	})
	if err != nil {
		return nil, err
	}
	return githubPool{ScaleSetClient: ss, rest: rest, owner: owner, repo: repo}, nil
}

// runDeleteScaleSet deletes the scale set of the configured pool poolName
// and its runner registrations (R23). It deletes nothing and returns a
// reportedError when the lock is held (R49), several scale sets share the
// name, or a registered runner of the pool still has a live container; the
// outcome is then already on stdout.
func runDeleteScaleSet(ctx context.Context, e env, d deleteScaleSetDeps, poolName string) error {
	unlock, err := d.Lock()
	if errors.Is(err, daemon.ErrLocked) {
		fmt.Fprintln(e.stdout, msgLocked)
		return reportedError{err: err}
	}
	if err != nil {
		return err
	}
	defer unlock()

	cfg, key, err := d.LoadConfig()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	pool, err := findPool(cfg, poolName)
	if err != nil {
		return err
	}
	gh, err := d.GitHub(cfg, key, pool)
	if err != nil {
		return fmt.Errorf("github client: %w", err)
	}
	del := &scaleSetDeleter{
		pool:      pool.Name,
		gh:        gh,
		newDocker: d.Docker,
		out:       e.stdout,
		log:       e.logger.With("pool", pool.Name),
		clock:     d.Clock,
		jitter:    d.Jitter,
		isRunner:  poolRunnerNames(pool.Name),
	}
	return del.run(ctx)
}

// findPool returns the configured pool called name.
func findPool(cfg *config.Config, name string) (config.Pool, error) {
	names := make([]string, 0, len(cfg.Pools))
	for _, p := range cfg.Pools {
		if p.Name == name {
			return p, nil
		}
		names = append(names, p.Name)
	}
	return config.Pool{}, fmt.Errorf("pool %q is not in the config (pools: %s)", name, strings.Join(names, ", "))
}

// poolRunnerNames returns a matcher of the names the scaler gives the
// runners of pool: "<pool>-" and 8 lowercase hex characters (R17).
//
// It repeats scaler.poolRunnerName, which is not exported.
func poolRunnerNames(pool string) func(name string) bool {
	re := regexp.MustCompile("^" + regexp.QuoteMeta(pool) + "-[0-9a-f]{8}$")
	return re.MatchString
}

// scaleSetDeleter is one delete-scaleset run after the lock, the
// configuration and the pool are settled.
type scaleSetDeleter struct {
	pool      string
	gh        scaleSetAPI
	newDocker func() (containerLister, error)
	out       io.Writer
	log       *slog.Logger
	clock     retry.Clock
	jitter    func() float64
	// isRunner tells the names of the pool's runners from others.
	isRunner func(name string) bool
}

// run deletes the pool's scale set. Nothing is deleted before the scale set
// is found unique and the pool's runners are known to have no live
// container.
func (s *scaleSetDeleter) run(ctx context.Context) error {
	var set *scaleset.RunnerScaleSet
	err := s.call(ctx, retry.OpGetRunnerScaleSet, func(ctx context.Context) (err error) {
		set, err = s.gh.GetScaleSet(ctx, s.pool)
		return err
	})
	switch {
	case errors.Is(err, retry.ErrAmbiguousScaleSet):
		fmt.Fprintf(s.out, "같은 이름의 scale set이 여럿이다: %s. 하나만 남긴 뒤 다시 실행. 아무것도 지우지 않음\n", s.pool)
		return reportedError{err: err}
	case err != nil:
		return fmt.Errorf("find scale set %q: %w", s.pool, err)
	case set == nil:
		fmt.Fprintf(s.out, "scale set 없음: %s. 지울 것이 없음\n", s.pool)
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
	err = s.call(ctx, retry.OpDeleteScaleSet, func(ctx context.Context) error {
		return s.gh.DeleteScaleSet(ctx, set.ID)
	})
	if err != nil {
		return fmt.Errorf("delete scale set %q (id %d): %w", s.pool, set.ID, err)
	}
	var left *scaleset.RunnerScaleSet
	err = s.call(ctx, retry.OpGetRunnerScaleSet, func(ctx context.Context) (err error) {
		left, err = s.gh.GetScaleSet(ctx, s.pool)
		return err
	})
	switch {
	case err != nil:
		return fmt.Errorf("confirm scale set %q is deleted: %w", s.pool, err)
	case left != nil:
		return fmt.Errorf("scale set %q (id %d) still exists after the delete", s.pool, left.ID)
	}
	fmt.Fprintf(s.out, "scale set 삭제됨: %s (ID %d), runner 등록 %d개 제거\n", s.pool, set.ID, len(members))
	return nil
}

// members returns the registrations that belong to the pool's scale set,
// found the R17 way: runners of the repository named like the pool's
// runners whose scale set is setID, by name. Another pool's runners, and
// registrations of another scale set that happen to have such a name, are
// left out. A failed lookup ends the run, since the membership is then
// unknown and nothing has been deleted yet.
func (s *scaleSetDeleter) members(ctx context.Context, setID int) ([]github.Runner, error) {
	var listed []github.RepoRunner
	err := s.call(ctx, retry.OpListRepoRunners, func(ctx context.Context) (err error) {
		listed, err = s.gh.ListRepoRunners(ctx)
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("list repository runners: %w", err)
	}
	var members []github.Runner
	for _, rr := range listed {
		if !s.isRunner(rr.Name) {
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
// container. As in R17, a container is live unless it has ended.
func (s *scaleSetDeleter) liveContainers(ctx context.Context, members []github.Runner) ([]string, error) {
	if len(members) == 0 {
		return nil, nil
	}
	dk, err := s.newDocker()
	if err != nil {
		return nil, err
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
// a rate limit's hint (R52); any other error is returned as it is.
func (s *scaleSetDeleter) call(ctx context.Context, op retry.Op, fn func(context.Context) error) error {
	backoff, err := retry.NewDefaultBackoff(s.clock, s.jitter)
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
		if err := backoff.Wait(ctx, err); err != nil {
			return err
		}
	}
}
