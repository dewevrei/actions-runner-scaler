package scaler

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	mathrand "math/rand/v2"
	"slices"
	"time"

	"github.com/dewevrei/actions-runner-scaler/internal/retry"
)

// maxStartupRetries is how many times R17 retries a transient failure of
// its scale set lookup or of a list (SPEC Design retry table, start-up
// cleanup).
const maxStartupRetries = 3

// startupRetryDelay returns the wait before R17's retry n, counted from 0:
// 1, 2 and 4 s.
func startupRetryDelay(n int) time.Duration { return time.Second << n }

// ResourceCleanupConfig holds a ResourceCleanup's dependencies.
type ResourceCleanupConfig struct {
	Resources ResourceLister
	Remover   ResourceIDRemover
	Clock     Clock
	// Logger is optional; nil discards logs.
	Logger *slog.Logger
	// Jitter returns values in [0, 1) for the backoff; nil means
	// math/rand/v2.Float64.
	Jitter func() float64
}

// ResourceCleanup is R16: at start-up it removes every container and
// network with the scaler's managed-by label, left by an earlier run. It
// needs neither the configuration nor GitHub, so it runs before either is
// checked.
//
// Unlike reconciliation and R17, it is not limited to one pool's runner
// names. SPEC R16 removes every managed-by resource, and it runs before the
// configuration is read and validated, so the pool names are not known yet
// (SPEC Design, start order). The start lock (R78) keeps a second scaler
// process from running beside this one, so every managed-by resource is
// one an earlier run left, of whatever pool.
type ResourceCleanup struct {
	resources ResourceLister
	remover   ResourceIDRemover
	clock     Clock
	log       *slog.Logger
	jitter    func() float64
}

// NewResourceCleanup returns a ResourceCleanup. Resources, Remover and
// Clock are required.
func NewResourceCleanup(cfg ResourceCleanupConfig) (*ResourceCleanup, error) {
	if cfg.Resources == nil || cfg.Remover == nil || cfg.Clock == nil {
		return nil, errors.New("scaler: resource cleanup needs Resources, Remover and Clock")
	}
	c := &ResourceCleanup{
		resources: cfg.Resources,
		remover:   cfg.Remover,
		clock:     cfg.Clock,
		log:       cfg.Logger,
		jitter:    cfg.Jitter,
	}
	if c.log == nil {
		c.log = slog.New(slog.DiscardHandler)
	}
	if c.jitter == nil {
		c.jitter = mathrand.Float64
	}
	return c, nil
}

// Run lists the resources, retrying a transient list error with the 1 to
// 60 s backoff until it succeeds (R24), and removes each one by its ID:
// every container first, then every network, since Docker does not remove
// a network a container is still attached to. Removing by ID takes exactly
// what the list found, the resources without a runner-name label among
// them. A permanent list error is logged and the start goes on (R16); a
// failed removal is logged and the next resource is tried. Run returns an
// error only when ctx ends (a signal during start-up, R57).
func (c *ResourceCleanup) Run(ctx context.Context) error {
	backoff, err := retry.NewDefaultBackoff(c.clock, c.jitter)
	if err != nil {
		return fmt.Errorf("scaler: %w", err)
	}
	var res []Resource
	for {
		callCtx, cancel := context.WithTimeout(ctx, dockerQueryTimeout)
		res, err = c.resources.List(callCtx)
		cancel()
		if err == nil {
			break
		}
		err = withOp(retry.OpContainerList, err)
		switch class := retry.Classify(ctx, retry.OpContainerList, err); class {
		case retry.Canceled:
			return ctx.Err()
		case retry.Permanent:
			c.log.Error("listing leftover runner resources failed permanently; not removed (R16)", "err", err)
			return nil
		default:
			c.log.Warn("listing leftover runner resources failed; retrying", "class", class, "err", err)
			if err := backoff.Wait(ctx, err); err != nil {
				return err
			}
		}
	}

	// Containers before networks; by name, then ID, to keep the order
	// stable.
	slices.SortFunc(res, func(a, b Resource) int {
		if an, bn := a.isNetwork(), b.isNetwork(); an != bn {
			if an {
				return 1
			}
			return -1
		}
		if n := cmp.Compare(a.Name, b.Name); n != 0 {
			return n
		}
		return cmp.Compare(a.ID, b.ID)
	})
	removed := 0
	for _, rs := range res {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		op := rs.removalOp()
		callCtx, cancel := context.WithTimeout(ctx, cleanupTimeout)
		err := c.remover.RemoveByID(callCtx, rs.removalKind(), rs.ID)
		cancel()
		if err != nil && retry.Classify(ctx, op, err) != retry.Gone {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			c.log.Warn("removing leftover runner resource failed; skipped (R16)",
				"kind", rs.Kind, "name", rs.Name, "id", rs.ID, "runner", rs.RunnerName,
				"err", withOp(op, err))
			continue
		}
		removed++
	}
	c.log.Info("leftover runner resources removed (R16)", "removed", removed, "found", len(res))
	return nil
}

// ScaleSetFinder looks a scale set up by name without creating it
// (github.ScaleSetClient.GetScaleSet).
type ScaleSetFinder interface {
	// FindScaleSet returns the ID of the scale set called name; found is
	// false when there is none.
	FindScaleSet(ctx context.Context, name string) (id int, found bool, err error)
}

// RegisteredRunner is a runner registration as GetRunnerByName reports it.
type RegisteredRunner struct {
	ID         int64
	Name       string
	ScaleSetID int
}

// RunnerRegistry looks up and removes runner registrations
// (github.ScaleSetClient GetRunnerByName and RemoveRunner).
type RunnerRegistry interface {
	// GetRunnerByName returns the runner registered as name; found is false
	// when there is none.
	GetRunnerByName(ctx context.Context, name string) (runner RegisteredRunner, found bool, err error)
	// RemoveRunner removes registration runnerID.
	RemoveRunner(ctx context.Context, runnerID int64) error
}

// RegistrationCleanupConfig holds a RegistrationCleanup's dependencies; all
// but Logger are required.
type RegistrationCleanupConfig struct {
	ScaleSets ScaleSetFinder
	Runners   RepoRunnerLister
	Registry  RunnerRegistry
	// Resources is checked for a live runner container before a
	// registration is removed.
	Resources ResourceLister
	Limits    RateLimits
	Clock     Clock
	// Logger is optional; nil discards logs.
	Logger *slog.Logger
}

// RegistrationCleanup is R17: at start-up it removes the pool's runner
// registrations left by an earlier run. It never blocks the start.
type RegistrationCleanup struct {
	scaleSets ScaleSetFinder
	runners   RepoRunnerLister
	registry  RunnerRegistry
	resources ResourceLister
	limits    RateLimits
	clock     Clock
	log       *slog.Logger
}

// NewRegistrationCleanup returns a RegistrationCleanup.
func NewRegistrationCleanup(cfg RegistrationCleanupConfig) (*RegistrationCleanup, error) {
	if cfg.ScaleSets == nil || cfg.Runners == nil || cfg.Registry == nil ||
		cfg.Resources == nil || cfg.Limits == nil || cfg.Clock == nil {
		return nil, errors.New("scaler: registration cleanup needs ScaleSets, Runners, Registry, Resources, Limits and Clock")
	}
	c := &RegistrationCleanup{
		scaleSets: cfg.ScaleSets,
		runners:   cfg.Runners,
		registry:  cfg.Registry,
		resources: cfg.Resources,
		limits:    cfg.Limits,
		clock:     cfg.Clock,
		log:       cfg.Logger,
	}
	if c.log == nil {
		c.log = slog.New(slog.DiscardHandler)
	}
	return c, nil
}

// errSkip ends R17 early; the reason is logged already.
var errSkip = errors.New("registration cleanup skipped")

// Run removes the registrations of pool (the scale set name) that an
// earlier run left (R17):
//
//   - The scale set's ID is looked up only, never created; with no scale set
//     there is nothing to remove.
//   - The lookup and the two lists (repository runners, Docker resources)
//     retry a transient error after 1, 2 and 4 s; a permanent error (several
//     scale sets with the name and authentication errors among them), a rate
//     limit, or a fourth failure skips R17. Before a call whose rate limit
//     has not ended, R17 is skipped without calling (PLAN M1-4).
//   - Each repository runner named "<pool>-" and 8 hex characters, the
//     names the start path gives (poolRunnerName), is looked up with
//     GetRunnerByName; it is removed only if its RunnerScaleSetID is the
//     pool's and no live runner container has its name. A failed lookup or
//     removal skips that runner only.
//
// Run returns an error only when ctx ends.
func (c *RegistrationCleanup) Run(ctx context.Context, pool string) error {
	log := c.log.With("pool", pool)
	err := c.run(ctx, log, pool)
	if errors.Is(err, errSkip) {
		return nil
	}
	return err
}

func (c *RegistrationCleanup) run(ctx context.Context, log *slog.Logger, pool string) error {
	var (
		setID int
		found bool
	)
	err := c.retrying(ctx, log, retry.OpGetRunnerScaleSet, func(ctx context.Context) error {
		var err error
		setID, found, err = c.scaleSets.FindScaleSet(ctx, pool)
		return err
	})
	if err != nil {
		return err
	}
	if !found {
		log.Info("no scale set yet; no registration to remove (R17)")
		return nil
	}

	var runners []RepoRunner
	err = c.retrying(ctx, log, retry.OpListRepoRunners, func(ctx context.Context) error {
		var err error
		runners, err = c.runners.ListRepoRunners(ctx)
		return err
	})
	if err != nil {
		return err
	}
	// Only names the start path gives this pool's runners: a pool whose
	// name starts with "<pool>-" has runners of its own with that prefix.
	runners = slices.DeleteFunc(runners, func(rr RepoRunner) bool { return !poolRunnerName(pool, rr.Name) })
	if len(runners) == 0 {
		log.Info("no registration to remove (R17)")
		return nil
	}

	var res []Resource
	err = c.retrying(ctx, log, retry.OpContainerList, func(ctx context.Context) error {
		var err error
		res, err = c.resources.List(ctx)
		return err
	})
	if err != nil {
		return err
	}
	alive := make(map[string]bool)
	for _, rs := range res {
		if rs.Kind == ResourceRunner && !rs.stopped() {
			alive[rs.RunnerName] = true
		}
	}

	removed := 0
	for _, rr := range runners {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if c.removeOne(ctx, log.With("runner", rr.Name), rr, setID, alive) {
			removed++
		}
	}
	log.Info("stale registrations removed (R17)", "removed", removed, "candidates", len(runners))
	return nil
}

// removeOne removes one candidate registration if it belongs to scale set
// setID and has no live container. It reports whether it removed it.
func (c *RegistrationCleanup) removeOne(ctx context.Context, log *slog.Logger, rr RepoRunner, setID int, alive map[string]bool) bool {
	if alive[rr.Name] {
		log.Info("runner container alive; registration kept (R17)")
		return false
	}
	if !c.limits.Allowed(retry.OpGetRunnerByName) {
		log.Info("runner lookup rate limited; skipped (R17)", "until", c.limits.At(retry.OpGetRunnerByName))
		return false
	}
	callCtx, cancel := context.WithTimeout(ctx, callTimeout)
	reg, found, err := c.registry.GetRunnerByName(callCtx, rr.Name)
	cancel()
	if err != nil {
		c.limits.Record(retry.OpGetRunnerByName, err)
		log.Warn("runner lookup failed; skipped (R17)", "err", withOp(retry.OpGetRunnerByName, err))
		return false
	}
	if !found {
		log.Debug("runner no longer registered")
		return false
	}
	if reg.ScaleSetID != setID {
		log.Info("runner belongs to another scale set; kept (R17)", "scaleSetId", reg.ScaleSetID)
		return false
	}
	if !c.limits.Allowed(retry.OpRemoveRunner) {
		log.Info("RemoveRunner rate limited; skipped (R17)", "until", c.limits.At(retry.OpRemoveRunner))
		return false
	}
	callCtx, cancel = context.WithTimeout(ctx, callTimeout)
	err = c.registry.RemoveRunner(callCtx, reg.ID)
	cancel()
	if err != nil {
		c.limits.Record(retry.OpRemoveRunner, err)
		if retry.Classify(ctx, retry.OpRemoveRunner, err) != retry.Gone {
			log.Warn("removing stale registration failed; skipped (R17)", "runnerId", reg.ID,
				"err", withOp(retry.OpRemoveRunner, err))
			return false
		}
	}
	log.Info("stale registration removed (R17)", "runnerId", reg.ID)
	return true
}

// retrying calls fn with a callTimeout context. A transient error is
// retried up to maxStartupRetries times, each after startupRetryDelay; any
// other failure, the last transient one, or a rate limit not yet over
// before the call ends R17 with errSkip, logged. A Docker call (op not rate
// limited) is never held back.
func (c *RegistrationCleanup) retrying(ctx context.Context, log *slog.Logger, op retry.Op, fn func(context.Context) error) error {
	for attempt := 0; ; attempt++ {
		if !c.limits.Allowed(op) {
			log.Info("rate limited; registration cleanup skipped (R17)", "op", op, "until", c.limits.At(op))
			return errSkip
		}
		callCtx, cancel := context.WithTimeout(ctx, callTimeout)
		err := fn(callCtx)
		cancel()
		if err == nil {
			return nil
		}
		c.limits.Record(op, err)
		err = withOp(op, err)
		class := retry.Classify(ctx, op, err)
		switch {
		case class == retry.Canceled:
			return ctx.Err()
		case class != retry.Transient:
			log.Warn("registration cleanup skipped (R17)", "class", class, "err", err)
			return errSkip
		case attempt == maxStartupRetries:
			log.Warn("registration cleanup skipped after retries (R17)", "retries", attempt, "err", err)
			return errSkip
		}
		wait := startupRetryDelay(attempt)
		log.Info("registration cleanup call failed; retrying", "class", class, "in", wait, "err", err)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-c.clock.After(wait):
		}
	}
}
