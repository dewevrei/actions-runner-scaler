package scaler

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/dewevrei/actions-runner-scaler/internal/gate"
	"github.com/dewevrei/actions-runner-scaler/internal/retry"
)

const (
	// runLookupTimeout bounds each of the job check's two lookups, the
	// installation token and every page included (SPEC Design retry rules:
	// 30 s each). The github package sets the same bound; the judge sets it
	// too so that any RunLookup is bounded.
	runLookupTimeout = callTimeout
	// latestRunWindow is how far before the run's created_at the latest run
	// lookup starts (SPEC Design judgment 8).
	latestRunWindow = time.Hour
	// denyRetryInterval is how often the owner of a deny path tries again to
	// remove a runner container whose removal failed (PLAN M4-2).
	denyRetryInterval = 2 * time.Second
	// denyStopGrace is how long the deny path waits, after recording the
	// deny, for the runner container to stop on its own before removing it.
	// On the deny the hook kills the job's Worker; the runner then reports
	// the job failed and exits within seconds.
	denyStopGrace = 30 * time.Second
)

// RunLookup is the GitHub REST calls of the job check (github.REST). Neither
// call retries, and the judge denies the job on any error (R65).
type RunLookup interface {
	// GetRun returns workflow run runID of owner/repo.
	GetRun(ctx context.Context, owner, repo string, runID int64) (gate.Run, error)
	// ListWorkflowRuns hands visit, page by page, the main runs of workflow
	// workflowID created at or after createdFrom, and stops when visit
	// returns true.
	ListWorkflowRuns(ctx context.Context, owner, repo string, workflowID int64, createdFrom time.Time,
		visit func(page []gate.RunSummary, totalCount int) (stop bool)) error
}

// JudgeConfig holds a JobJudge's dependencies; both are required.
type JudgeConfig struct {
	// Runs looks up workflow runs in the pool's repository.
	Runs RunLookup
	// Policy is the pool's rule (R9). Its repository is also the one the
	// lookups read.
	Policy gate.Policy
}

// JobJudge is the Judge: it checks every job a runner starts against the R9
// conditions and removes the runner of a job it denies.
//
// Start runs one goroutine per runner: GetRun, then the first nine
// conditions (gate.EvaluateRun), and only when they pass and the run has a
// workflow_id and created_at, the latest run lookup from an hour before the
// run's created_at (gate.LatestScan), then all ten conditions
// (gate.Evaluate). A failed lookup, whether transient, permanent or rate
// limited, denies that job alone: it is not retried, nothing waits for a
// rate limit, and no drain starts (R52, R65, R98). An allow records the
// verdict and stops the runner's job check timer.
//
// The deny path, shared by a denied job, Reject (R102) and the job check
// timer (R81), runs for the one caller that wins the runner's exit. It
// records the deny at once, so the hook reads it and kills the job's
// Worker: no step runs any more, and the runner reports the job failed and
// exits. When the verdict was not pending, as for a Reject after an
// allow, the hook cannot see the deny and the wait below is skipped. In a
// new goroutine it waits for the runner container to stop, at most
// denyStopGrace on the Clock (a container that is gone counts as stopped,
// and after a failed wait it waits out the grace), then removes the runner
// container with a 10-second bound, revokes the token once the container is
// confirmed gone (not found included), and removes the BuildKit container,
// the network and the registration by the removal rules of watch and
// reconcile (Scaler.retire). While the container removal fails, the owner
// tries again every 2 seconds whatever the error, logging and recording
// each new error once.
//
// New binds a JobJudge to the Scaler it is given to, which installs the
// job check timers in that Scaler's Table; it must not be used before.
// Stop cancels the checks in progress, ends the waits for denied runners'
// containers, and stops the deny retries and the timers; finalize calls it
// after Scaler.BeginFinalize (M5-8), and every step also checks the table's
// finalize mark itself. Wait waits for all of it to end.
//
// It is safe for concurrent use.
type JobJudge struct {
	runs   RunLookup
	policy gate.Policy
	busy   *BusyTimers

	// ctx ends every lookup and every wait for a denied runner's
	// container; Stop cancels it.
	ctx    context.Context
	cancel context.CancelFunc
	// work counts the judge goroutines, the deny paths Reject began, the
	// waits for denied runners' containers and the pending deny retries.
	work inFlight

	// s, log and retries are set once by bind, before any use.
	s   *Scaler
	log *slog.Logger
	// retries holds the timers of the deny retries.
	retries *deferredWork

	mu      sync.Mutex
	bound   bool
	stopped bool
}

// judgeBinder is implemented by a Judge that New binds to the Scaler.
type judgeBinder interface {
	bind(s *Scaler) error
}

// NewJobJudge returns a JobJudge to give to New as Config.Judge.
func NewJobJudge(cfg JudgeConfig) (*JobJudge, error) {
	if cfg.Runs == nil {
		return nil, errors.New("scaler: judge missing Runs")
	}
	if cfg.Policy.Owner == "" || cfg.Policy.Repository == "" {
		return nil, errors.New("scaler: judge policy names no repository")
	}
	p := cfg.Policy
	p.AllowedWorkflows = slices.Clone(p.AllowedWorkflows)
	p.AllowedEvents = slices.Clone(p.AllowedEvents)
	ctx, cancel := context.WithCancel(context.Background())
	j := &JobJudge{runs: cfg.Runs, policy: p, ctx: ctx, cancel: cancel}
	j.busy = &BusyTimers{j: j, timers: make(map[string]Timer)}
	return j, nil
}

// bind gives the judge the Scaler whose table, clock, logger, error recorder
// and removal rules it uses, and installs the job check timers in the
// Scaler's table, so R81 cannot be left unwired. New calls it once. It fails
// when the judge or the table is bound already, or when the pool's repo-url
// is empty, malformed, or names a repository other than the policy's.
func (j *JobJudge) bind(s *Scaler) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.bound {
		return errors.New("judge already belongs to another Scaler")
	}
	url := s.pool.RepoURL
	owner, repo, ok := repoFromURL(url)
	if !ok || !strings.EqualFold(owner, j.policy.Owner) || !strings.EqualFold(repo, j.policy.Repository) {
		return fmt.Errorf("judge policy repository %s/%s is not the pool's repo-url %q",
			j.policy.Owner, j.policy.Repository, url)
	}
	// The hook reads these once installed, so they are set before it is
	// published and rolled back if it cannot be.
	j.s = s
	j.log = s.log.With("component", "judge")
	j.retries = newDeferredWork(s.clock, &j.work)
	if err := s.table.setJobCheckTimer(j.busy.arm); err != nil {
		j.s, j.log, j.retries = nil, nil, nil
		return err
	}
	j.bound = true
	return nil
}

// repoFromURL splits a repo-url, https://github.com/<owner>/<repo> as
// config validates it, into owner and repository.
func repoFromURL(url string) (owner, repo string, ok bool) {
	rest, ok := strings.CutPrefix(url, "https://github.com/")
	if !ok {
		return "", "", false
	}
	owner, repo, ok = strings.Cut(rest, "/")
	if !ok || owner == "" || repo == "" || strings.Contains(repo, "/") {
		return "", "", false
	}
	return owner, repo, true
}

// BusyTimers returns the judge's job check timers (R81), which bind
// installed in the table. Stop and Wait cover them too.
func (j *JobJudge) BusyTimers() *BusyTimers { return j.busy }

// Start begins checking job on runner name in a new goroutine and returns
// at once (Judge). It does nothing once finalize has begun or Stop was
// called.
func (j *JobJudge) Start(name string, job Job) {
	if j.s == nil {
		return
	}
	log := j.log.With("runner", name, "jobId", job.ID, "workflowRunId", job.WorkflowRunID)
	if j.s.table.Finalizing() {
		log.Info("finalize began; job not judged")
		return
	}
	if !j.begin() {
		log.Info("judge stopped; job not judged")
		return
	}
	go func() {
		defer j.work.end()
		j.judge(log, name, job)
	}()
}

// Reject removes runner name through the deny path (Judge, R102). The
// runner's exit is decided before it returns, by Table.BeginExit, so a
// second call for the same runner does nothing; the removal runs in a new
// goroutine. It does nothing once finalize has begun or Stop was called.
func (j *JobJudge) Reject(name, reason string) {
	if j.s == nil {
		return
	}
	log := j.log.With("runner", name)
	if j.s.table.Finalizing() {
		log.Info("finalize began; runner not rejected", "reason", reason)
		return
	}
	if !j.begin() {
		log.Info("judge stopped; runner not rejected", "reason", reason)
		return
	}
	if !j.s.table.BeginExit(name) {
		j.work.end()
		log.Debug("runner removal already begun; reject ignored", "reason", reason)
		return
	}
	go func() {
		defer j.work.end()
		j.removeDenied(log, name, reason)
	}()
}

// Stop cancels the lookups in progress and the waits for denied runners'
// containers, stops the pending deny retries and the job check timers, and
// refuses new checks. A deny path in progress finishes its current call and
// leaves the runner to finalize.
//
// Precondition: finalize has begun (Scaler.BeginFinalize), as in M5-8,
// which then removes every row left. Without finalize, a row whose pending
// deny retry Stop cancelled stays exiting with its deny recorded, so its
// hook has killed the Worker or every runner process and no step runs (R10),
// until the next start's R16; a row a deny path itself stopped on is
// revoked and abandoned for reconciliation (denial.leave).
func (j *JobJudge) Stop() {
	j.mu.Lock()
	j.stopped = true
	retries := j.retries
	j.mu.Unlock()
	j.cancel()
	j.busy.Stop()
	if retries != nil {
		retries.stop()
	}
}

// Wait waits until every timer callback, check, deny path and pending deny
// retry has ended, or ctx ends. Before Stop a pending timer or retry can keep
// it waiting for minutes.
func (j *JobJudge) Wait(ctx context.Context) error {
	// A timer callback may schedule a deny retry, so the timers go first.
	if err := j.busy.Wait(ctx); err != nil {
		return err
	}
	return j.work.wait(ctx)
}

// begin counts one unit of work unless Stop was called.
func (j *JobJudge) begin() bool {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.stopped {
		return false
	}
	j.work.begin()
	return true
}

// halted reports whether Stop was called or finalize has begun; the judge
// then begins no Docker or GitHub call.
func (j *JobJudge) halted() bool {
	return j.ctx.Err() != nil || j.s.table.Finalizing()
}

// judge is one runner's check.
func (j *JobJudge) judge(log *slog.Logger, name string, job Job) {
	allow, reason := j.decide(log, job)
	if j.halted() {
		log.Info("judging stopped; no verdict")
		return
	}
	if allow {
		j.allow(log, name)
		return
	}
	j.deny(log, name, reason, j.s.table.BeginExit)
}

// decide makes the lookups and applies the policy. It reports the verdict
// and, for a deny, why.
func (j *JobJudge) decide(log *slog.Logger, job Job) (allow bool, reason string) {
	p := j.policy
	if j.halted() {
		return false, "judging stopped"
	}
	ctx, cancel := context.WithTimeout(j.ctx, runLookupTimeout)
	run, err := j.runs.GetRun(ctx, p.Owner, p.Repository, job.WorkflowRunID)
	cancel()
	if err != nil {
		return false, j.lookupFailed(log, retry.OpGetRun, err)
	}
	if d := gate.EvaluateRun(job.Gate, run, p); !d.Allow {
		return false, d.Reason()
	}
	if run.WorkflowID <= 0 || run.CreatedAt.IsZero() {
		// The latest run lookup needs both; LatestRun fails without them,
		// and Evaluate says why without a lookup.
		return false, gate.Evaluate(job.Gate, run, gate.Latest{}, p).Reason()
	}
	if j.halted() {
		return false, "judging stopped"
	}
	scan := gate.NewLatestScan(run, p)
	ctx, cancel = context.WithTimeout(j.ctx, runLookupTimeout)
	err = j.runs.ListWorkflowRuns(ctx, p.Owner, p.Repository, run.WorkflowID,
		run.CreatedAt.Add(-latestRunWindow), scan.Add)
	cancel()
	if err != nil {
		// Denied on the error alone: what the pages read so far showed
		// proves nothing (LatestScan.Result).
		return false, j.lookupFailed(log, retry.OpListWorkflowRuns, err)
	}
	d := gate.Evaluate(job.Gate, run, scan.Result(), p)
	return d.Allow, d.Reason()
}

// lookupFailed logs and records a failed lookup and returns the deny reason.
// The failure is never retried and records no rate limit (R52, R65).
func (j *JobJudge) lookupFailed(log *slog.Logger, op retry.Op, err error) string {
	err = withOp(op, err)
	if j.halted() {
		return "judging stopped"
	}
	class := retry.Classify(j.ctx, op, err)
	log.Warn("job check lookup failed; denying the job without retry", "class", class, "err", err)
	j.s.errs.Record("job check lookup", err)
	return fmt.Sprintf("lookup failed (%s): %v", class, err)
}

// allow records the allow verdict and stops the runner's job check timer.
// Table.Allow refuses when the runner's removal began meanwhile, as when the
// timer fired first.
func (j *JobJudge) allow(log *slog.Logger, name string) {
	if !j.s.table.Allow(name) {
		info, _ := j.s.table.Get(name)
		log.Info("job passed the check but the allow was not recorded",
			"state", info.State, "verdict", info.Verdict)
		return
	}
	j.busy.cancel(name)
	log.Info("job allowed")
}

// deny begins the deny path for runner name if begin, a BeginExit variant,
// wins the runner's exit. It records the deny before it returns and does
// not wait for the runner container.
func (j *JobJudge) deny(log *slog.Logger, name, reason string, begin func(string) bool) {
	if j.halted() {
		log.Info("judge stopped or finalize began; runner left to finalize", "reason", reason)
		return
	}
	if !begin(name) {
		log.Debug("runner removal already begun or job allowed; deny path not started", "reason", reason)
		return
	}
	j.removeDenied(log, name, reason)
}

// removeDenied runs the deny path for runner name, whose exit the caller
// won. It records the deny at once, so the hook kills the job's Worker,
// and leaves the wait for the container and its removal to a new goroutine.
// When the verdict was not pending the hook cannot see the deny, so the
// container is removed without waiting for it to stop.
func (j *JobJudge) removeDenied(log *slog.Logger, name, reason string) {
	j.busy.cancel(name)
	info, _ := j.s.table.Get(name)
	d := &denial{j: j, name: name, id: info.ID, log: log}
	if j.s.table.Deny(name) {
		log.Info("job denied; waiting for the runner to stop", "reason", reason)
		d.awaitStop()
		return
	}
	info, _ = j.s.table.Get(name)
	log.Info("job denied but the verdict was not pending, deny not recorded; removing the runner container",
		"reason", reason, "verdict", info.Verdict)
	d.start(d.attempt)
}

// denial is one runner's deny path after its owner won the runner's exit.
type denial struct {
	j    *JobJudge
	name string
	// id is the GitHub runner ID; zero when none is known.
	id  int64
	log *slog.Logger
	// errs logs and records each new container removal error once.
	errs    errorOnce
	retries int
}

// start runs f in a new goroutine that the judge's work counts, so Wait
// waits for it. Once Stop was called it runs nothing and leaves the runner
// to finalize.
func (d *denial) start(f func()) {
	j := d.j
	if !j.begin() {
		d.leave("judge stopped")
		return
	}
	go func() {
		defer j.work.end()
		f()
	}()
}

// awaitStop waits, in a new goroutine, for the runner container to stop
// after the hook killed its Worker, at most denyStopGrace on the Clock,
// then removes it, unless the judge stopped or finalize began. Stop ends
// the wait at once, since the wait's context derives from the judge's;
// finalize without Stop is seen when the wait ends.
func (d *denial) awaitStop() {
	d.start(func() {
		j := d.j
		began := j.s.clock.Now()
		ctx, cancel := context.WithCancel(j.ctx)
		defer cancel()
		grace := j.s.clock.AfterFunc(denyStopGrace, cancel)
		err := d.waitStopped(ctx)
		grace.Stop()
		if j.halted() {
			d.leave("judge stopped or finalize began")
			return
		}
		switch {
		case err == nil:
			d.log.Info("denied runner's container stopped; removing it", "elapsed", j.s.clock.Now().Sub(began))
		case isNotFound(err):
			d.log.Info("denied runner's container gone; removing the rest", "elapsed", j.s.clock.Now().Sub(began))
		default:
			// Only the grace ends ctx without halting the judge.
			d.log.Info("runner did not stop within grace; removing its container", "grace", denyStopGrace)
		}
		d.attempt()
	})
}

// waitStopped waits until the runner container stops or ctx ends. It
// returns nil once the container stopped, the not-found error when the
// container is gone, and otherwise ctx's error. A wait that fails another
// way is logged, and ctx is then waited for, so only the grace or Stop
// ends the wait.
func (d *denial) waitStopped(ctx context.Context) error {
	_, err := d.j.s.watch.waiter.Wait(ctx, d.name)
	if err == nil || isNotFound(err) || ctx.Err() != nil {
		return err
	}
	err = withOp(retry.OpContainerWait, err)
	d.log.Warn("waiting for the denied runner's container failed; removing it when the grace ends",
		"class", retry.Classify(context.Background(), retry.OpContainerWait, err), "err", err)
	<-ctx.Done()
	return ctx.Err()
}

// attempt removes the runner container once. On success it goes on; on
// failure it tries again in 2 seconds.
func (d *denial) attempt() {
	j, s := d.j, d.j.s
	if j.halted() {
		d.leave("judge stopped or finalize began")
		return
	}
	ctx, cancel, ok := s.cleanupContext(context.Background(), removeContainerTimeout)
	if !ok {
		d.leave("finalize budget spent")
		return
	}
	err := s.containers.RemoveContainer(ctx, d.name)
	cancel()
	if err != nil {
		err = withOp(retry.OpContainerRemove, err)
		if class := retry.Classify(context.Background(), retry.OpContainerRemove, err); class != retry.Gone {
			d.failed(class, err)
			return
		}
	}
	d.removed()
}

// failed handles a container removal that was not confirmed: the token
// stays valid, so the hook still reads the deny and no step runs (R10), and
// the owner tries again whatever the error. Neither watch nor reconcile
// takes the runner over, since it is exiting.
func (d *denial) failed(class retry.Class, err error) {
	d.retries++
	d.errs.report(d.log, d.j.s.errs, "deny remove runner container",
		"removing the denied runner's container failed; retrying", class, err)
	if !d.j.retries.after(denyRetryInterval, d.attempt) {
		d.leave("judge stopped")
	}
}

// removed revokes the token once the runner container is gone and removes
// the rest of the runner (R11, R38).
func (d *denial) removed() {
	j, s := d.j, d.j.s
	if d.retries > 0 {
		d.errs.reset(d.log, "denied runner's container removed after retries")
	}
	d.log.Info("denied runner's container removed")
	s.table.RevokeToken(d.name)
	if j.halted() {
		d.leave("judge stopped or finalize began")
		return
	}
	s.retire(d.log, d.name, d.id, unregDenied)
}

// leave stops the deny path and leaves the exiting row to finalize, which
// removes every row left (M5-8). It revokes the token first: a hook that
// has not read the deny yet then gets 401 and keeps waiting (R66), and the
// row meets Scaler.retire's precondition, so it is also safe when Stop came
// without finalize and reconciliation claims the row (ClaimAbandoned).
func (d *denial) leave(why string) {
	s := d.j.s
	s.table.RevokeToken(d.name)
	s.table.AbandonExit(d.name, AbandonFinalize)
	d.log.Info("deny path stopped; runner left to finalize", "why", why)
}
