package scaler

import (
	"context"
	"errors"
	"log/slog"
	"path"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/dewevrei/actions-runner-scaler/internal/retry"
)

// Reconciliation timing.
const (
	// reconcileInterval is R42's period. The ticks are fixed: a round that
	// runs long makes the next tick come sooner, and a tick missed entirely
	// is skipped, as time.Ticker does.
	reconcileInterval = 30 * time.Second
	// findRunnerTimeout bounds one runner lookup of R149 (0002 PLAN M9-8).
	findRunnerTimeout = 30 * time.Second
	// dockerQueryTimeout bounds one Docker list or process list. Not from
	// SPEC: chosen so a round's queries stay well within reconcileInterval.
	dockerQueryTimeout = 10 * time.Second
)

// ResourceKind says what a Resource is.
type ResourceKind string

// Resource kinds; the values match the docker package's "kind" label.
const (
	ResourceRunner   ResourceKind = "runner"
	ResourceBuildKit ResourceKind = "buildkit"
	ResourceNetwork  ResourceKind = "network"
)

// Resource is a container or network carrying the scaler's managed-by label
// (docker.Resource).
type Resource struct {
	Kind ResourceKind
	ID   string
	// Name has no leading slash.
	Name string
	// RunnerName is the runner-name label: the runner the resource belongs
	// to.
	RunnerName string
	// State is the container state as Docker reports it ("created",
	// "running", "paused", "restarting", "removing", "exited", "dead"); it
	// is empty for a network.
	State string
}

// running reports whether the container runs.
func (r *Resource) running() bool { return r != nil && r.State == "running" }

// stopped reports whether the container has ended.
func (r *Resource) stopped() bool { return r != nil && (r.State == "exited" || r.State == "dead") }

// isNetwork reports whether the resource is a network. A resource whose
// kind label is missing or unknown is told by its State, which is empty for
// a network only (docker.Resource.State).
func (r *Resource) isNetwork() bool {
	switch r.Kind {
	case ResourceNetwork:
		return true
	case ResourceRunner, ResourceBuildKit:
		return false
	}
	return r.State == ""
}

// removalKind is the kind ResourceIDRemover.RemoveByID is called with:
// ResourceNetwork for every network, the resource's own kind otherwise.
func (r *Resource) removalKind() ResourceKind {
	if r.isNetwork() {
		return ResourceNetwork
	}
	return r.Kind
}

// removalOp is the call retry.Classify reads a removal error of the
// resource as.
func (r *Resource) removalOp() retry.Op {
	if r.isNetwork() {
		return retry.OpNetworkRemove
	}
	return retry.OpContainerRemove
}

// ResourceLister lists every container (running or not) and network with
// the scaler's managed-by label (docker.Client.List), whatever target it
// belongs to.
type ResourceLister interface {
	List(ctx context.Context) ([]Resource, error)
}

// ResourceIDRemover removes one listed resource by its Docker ID: kind
// ResourceNetwork removes network id, and every other kind, an empty or
// unknown one included, force-removes container id with its anonymous
// volumes (SPEC Design: removal uses RemoveVolumes). A resource already
// gone may count as success or fail with status 404 (retry.StatusCoder);
// both count as removed. Removing by ID takes exactly the resource that was
// listed, also one without the runner-name label, and lets a caller tell
// which of a runner's resources failed (R16, R42).
type ResourceIDRemover interface {
	RemoveByID(ctx context.Context, kind ResourceKind, id string) error
}

// ProcessLister lists the command lines of the processes in a runner
// container (docker.Client.Processes, the top API).
type ProcessLister interface {
	Processes(ctx context.Context, name string) ([]string, error)
}

// ListedRunner is one entry of the organization's self-hosted runner list.
type ListedRunner struct {
	ID   int64
	Name string
	Busy bool
}

// RunnerLister lists every self-hosted runner of the target's organization,
// all pages (github.REST.ListRunners with the target bound). The caller's
// context bounds the whole list.
type RunnerLister interface {
	ListRunners(ctx context.Context) ([]ListedRunner, error)
}

// RunnerFinder looks up one runner of the target's organization by name
// (github.REST.FindRunner with the target bound); found is false when no
// runner has exactly that name (0002 SPEC R149).
type RunnerFinder interface {
	FindRunner(ctx context.Context, name string) (runner ListedRunner, found bool, err error)
}

// ReconcilerConfig holds a Reconciler's dependencies; all are required.
type ReconcilerConfig struct {
	// Scaler supplies the table, the clock, the removal rules, the rate
	// limits, the logger and the error recorder.
	Scaler    *Scaler
	Resources ResourceLister
	// Remover removes the resources of runners not in the table, one by one
	// (R42).
	Remover   ResourceIDRemover
	Processes ProcessLister
	Finder    RunnerFinder
}

// Reconciler is the 30-second reconciliation of one target (R42). Each round:
//
//  1. From the table alone, begins R101's removal of every done runner whose
//     5 minutes have passed and whose removal has not begun (the backup of
//     the per-runner timer), and claims abandoned rows (Table.ClaimAbandoned)
//     to remove them again. A row abandoned on a permanent error is never
//     claimed (R42).
//  2. Starts R149 in its own goroutine, at most one at a time, when a runner
//     that is neither done nor allowed has not been marked busy: it looks
//     the runners up by name one after another.
//  3. Lists the Docker resources and matches them with the table by the
//     runner-name label: a row with no runner container, or a stopped one,
//     is handled as ended (except starting rows, which belong to their start
//     path); an idle runner whose BuildKit container is gone or stopped is
//     removed by the scale-down rules (R67); a running runner without an
//     allow verdict that has a Runner.Worker process is marked busy (R83);
//     resources of a runner not in the table are removed one by one, by ID,
//     except one whose removal failed with a permanent error (R42).
//
// Only the target's own resources are touched. ResourceLister returns every
// managed-by resource on the host, those of other targets included, so a
// resource whose runner is not in the table is removed only when its
// runner-name has the form the start path gives this target's runners,
// "<label>-" and 8 hex characters (RunnerName); others are skipped. The
// table holds this target's runners only, so its rows need no such check.
//
// Removals follow the retry rules of watch and reconcile and never start a
// drain (R98). The rounds stop once finalize begins; every call is checked
// against the finalize mark first and ends by the finalize deadline.
//
// It is safe for concurrent use.
type Reconciler struct {
	s         *Scaler
	resources ResourceLister
	remover   ResourceIDRemover
	processes ProcessLister
	finder    RunnerFinder
	log       *slog.Logger

	// ctx ends every call; Stop cancels it.
	ctx    context.Context
	cancel context.CancelFunc
	// ticks counts the pending tick timer and a round in progress; finds
	// counts the R149 goroutine.
	ticks inFlight
	finds inFlight
	// finding is set while an R149 goroutine runs.
	finding atomic.Bool

	// findErr and resourcesErr remember the last error of R149 and of the
	// Docker list, so a repeated error is logged and recorded once.
	findErr      errorOnce
	resourcesErr errorOnce

	mu      sync.Mutex
	started bool
	stopped bool
	timer   Timer
	next    time.Time
	// failed holds the IDs of leftover resources whose removal failed with a
	// permanent error, kept while they are listed (R42).
	failed map[string]bool
	// removing holds the IDs of leftover resources whose removal is in
	// progress.
	removing map[string]bool
	// procErrs remembers, per runner in the table, the last error of R83's
	// process list, so a list that keeps failing is logged and recorded
	// once.
	procErrs map[string]*errorOnce
}

// NewReconciler returns a Reconciler that has not started.
func NewReconciler(cfg ReconcilerConfig) (*Reconciler, error) {
	var missing []string
	for _, d := range []struct {
		name    string
		missing bool
	}{
		{"Scaler", cfg.Scaler == nil},
		{"Resources", cfg.Resources == nil},
		{"Remover", cfg.Remover == nil},
		{"Processes", cfg.Processes == nil},
		{"Finder", cfg.Finder == nil},
	} {
		if d.missing {
			missing = append(missing, d.name)
		}
	}
	if len(missing) > 0 {
		return nil, errors.New("scaler: reconciler missing " + strings.Join(missing, ", "))
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &Reconciler{
		s:         cfg.Scaler,
		resources: cfg.Resources,
		remover:   cfg.Remover,
		processes: cfg.Processes,
		finder:    cfg.Finder,
		log:       cfg.Scaler.log.With("component", "reconcile"),
		ctx:       ctx,
		cancel:    cancel,
		failed:    make(map[string]bool),
		removing:  make(map[string]bool),
		procErrs:  make(map[string]*errorOnce),
	}, nil
}

// Start schedules the first round reconcileInterval from now on the
// Scaler's Clock and returns. Later calls, and calls after Stop, do nothing.
func (r *Reconciler) Start() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.started || r.stopped {
		return
	}
	r.started = true
	r.next = r.s.clock.Now().Add(reconcileInterval)
	r.scheduleLocked()
}

// scheduleLocked sets the timer of the tick at r.next.
func (r *Reconciler) scheduleLocked() {
	r.ticks.begin()
	r.timer = r.s.clock.AfterFunc(r.next.Sub(r.s.clock.Now()), r.tick)
}

// tick runs one round and schedules the next. Exactly one of tick and Stop
// ends the work count of each timer: Stop only when Timer.Stop prevented the
// call.
func (r *Reconciler) tick() {
	defer r.ticks.end()
	r.mu.Lock()
	stopped := r.stopped
	r.mu.Unlock()
	if stopped {
		return
	}
	if r.s.table.Finalizing() {
		r.log.Info("finalize began; reconciliation stopped")
		return
	}
	r.round()

	r.mu.Lock()
	defer r.mu.Unlock()
	if r.stopped || r.s.table.Finalizing() {
		return
	}
	now := r.s.clock.Now()
	r.next = r.next.Add(reconcileInterval)
	for !r.next.After(now) {
		r.next = r.next.Add(reconcileInterval)
	}
	r.scheduleLocked()
}

// Stop ends the rounds and cancels every call in progress, the R149 lookups
// among them. A removal a round began goes on and counts for
// Scaler.WaitInFlight.
func (r *Reconciler) Stop() {
	r.mu.Lock()
	r.stopped = true
	if r.timer != nil && r.timer.Stop() {
		r.ticks.end()
	}
	r.timer = nil
	r.mu.Unlock()
	r.cancel()
}

// Wait waits until no round, pending tick or R149 goroutine is left, or ctx
// ends. Before Stop or finalize a tick is always pending.
func (r *Reconciler) Wait(ctx context.Context) error {
	if err := r.ticks.wait(ctx); err != nil {
		return err
	}
	return r.finds.wait(ctx)
}

// round is one reconciliation.
func (r *Reconciler) round() {
	s := r.s
	if s.table.Finalizing() {
		return
	}
	// R101 first: it needs no outside call to decide, so a slow call below
	// never holds it back.
	now := s.clock.Now()
	for _, info := range s.table.List() {
		if info.State == StateDone && !info.DoneCleanupStarted &&
			!now.Before(info.CompletedAt.Add(doneCleanupDelay)) {
			s.doneCleanup(info.Name)
		}
	}
	for _, info := range s.table.List() {
		if info.Abandoned && info.AbandonReason != AbandonPermanent {
			s.reclaimAbandoned(info.Name)
		}
	}

	// The snapshot comes before the Docker list: a row that is not starting
	// in it had its containers created already, so the list cannot miss
	// them and make the row look ended.
	rows := s.table.List()
	r.startRunnerCheck(rows)

	if s.table.Finalizing() {
		return
	}
	ctx, cancel, ok := s.callContext(r.ctx, dockerQueryTimeout)
	if !ok {
		return
	}
	res, err := r.resources.List(ctx)
	cancel()
	if err != nil {
		if r.ctx.Err() != nil {
			return
		}
		err = withOp(retry.OpContainerList, err)
		r.resourcesErr.report(r.log, s.errs, "reconcile list resources",
			"listing runner resources failed; round skipped", retry.Classify(r.ctx, retry.OpContainerList, err), err)
		return
	}
	r.resourcesErr.reset(r.log, "listing runner resources works again")

	groups := groupResources(res)
	inTable := make(map[string]bool, len(rows))
	for _, info := range rows {
		inTable[info.Name] = true
	}
	r.keepFailed(res)
	r.keepProcErrs(inTable)
	for _, info := range rows {
		r.reconcileRow(info, groups[info.Name])
	}
	names := make([]string, 0, len(groups))
	for name := range groups {
		switch {
		case inTable[name]:
		case !RunnerName(s.target.Label, name):
			// Another target's runner, or a name this scaler never gives.
		default:
			names = append(names, name)
		}
	}
	slices.Sort(names)
	for _, name := range names {
		r.removeLeftover(name, groups[name])
	}
}

// runnerResources is one runner's listed resources; any may be nil.
type runnerResources struct {
	runner, buildKit, network *Resource
}

// all returns the listed resources in removal order: the containers before
// the network, which Docker does not remove while a container is attached.
func (g *runnerResources) all() []*Resource {
	var all []*Resource
	for _, res := range []*Resource{g.runner, g.buildKit, g.network} {
		if res != nil {
			all = append(all, res)
		}
	}
	return all
}

// groupResources groups res by the runner-name label. A resource without
// the label or of an unknown kind is left out: removal goes by runner name.
func groupResources(res []Resource) map[string]*runnerResources {
	groups := make(map[string]*runnerResources)
	for i := range res {
		rs := &res[i]
		if rs.RunnerName == "" {
			continue
		}
		g := groups[rs.RunnerName]
		if g == nil {
			g = &runnerResources{}
		}
		switch rs.Kind {
		case ResourceRunner:
			g.runner = rs
		case ResourceBuildKit:
			g.buildKit = rs
		case ResourceNetwork:
			g.network = rs
		default:
			continue
		}
		groups[rs.RunnerName] = g
	}
	return groups
}

// keepProcErrs forgets the process list errors of runners not in inTable,
// the names of the round's table snapshot.
func (r *Reconciler) keepProcErrs(inTable map[string]bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for name := range r.procErrs {
		if !inTable[name] {
			delete(r.procErrs, name)
		}
	}
}

// procErr returns the process list error memory of runner name.
func (r *Reconciler) procErr(name string) *errorOnce {
	r.mu.Lock()
	defer r.mu.Unlock()
	e := r.procErrs[name]
	if e == nil {
		e = &errorOnce{}
		r.procErrs[name] = e
	}
	return e
}

// keepFailed forgets the permanently failed IDs that are no longer listed.
func (r *Reconciler) keepFailed(res []Resource) {
	listed := make(map[string]bool, len(res))
	for _, rs := range res {
		listed[rs.ID] = true
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for id := range r.failed {
		if !listed[id] {
			delete(r.failed, id)
		}
	}
}

// reconcileRow matches one row of the snapshot with its listed resources.
func (r *Reconciler) reconcileRow(info RunnerInfo, g *runnerResources) {
	if info.State == StateStarting || info.State == StateExiting {
		// The start path, or the owner of the removal, has the row.
		return
	}
	s := r.s
	log := s.log.With("runner", info.Name)
	if g == nil {
		g = &runnerResources{}
	}
	switch {
	case g.runner == nil:
		s.endRunner(log, info.Name, "runner container missing")
		return
	case g.runner.stopped():
		s.endRunner(log, info.Name, "runner container stopped")
		return
	case !g.runner.running():
		// Created, paused, restarting or being removed: look again next round.
		return
	}
	// R67 removes only for a BuildKit container that is gone or has ended
	// (exited, dead). One that is created, paused, restarting or being
	// removed may still run, so it is looked at again next round.
	if info.State == StateIdle && (g.buildKit == nil || g.buildKit.stopped()) {
		if s.table.Finalizing() {
			return
		}
		state := "missing"
		if g.buildKit != nil {
			state = g.buildKit.State
		}
		log.Warn("idle runner's BuildKit container is gone or stopped; removing the runner (R67)",
			"buildkit", state)
		if err := s.removeIdleRunner(r.ctx, info.Name); err != nil {
			log.Debug("R67 removal stopped", "err", err)
		}
		return
	}
	if needsBusyCheck(info) {
		r.checkWorker(log, info.Name)
	}
}

// needsBusyCheck reports whether reconciliation looks for a job on the
// runner (R83, R149): it is idle or busy, not done, has no allow verdict,
// and has not been marked busy (MarkBusy records only the first time).
func needsBusyCheck(info RunnerInfo) bool {
	return (info.State == StateIdle || info.State == StateBusy) &&
		info.Verdict != VerdictAllow && info.BusyAt.IsZero()
}

// checkWorker marks the runner busy when its container runs Runner.Worker
// (R83). A failed process list is skipped for this round; it is logged at
// Warn and recorded once per runner while the same error repeats.
func (r *Reconciler) checkWorker(log *slog.Logger, name string) {
	s := r.s
	if s.table.Finalizing() {
		return
	}
	ctx, cancel, ok := s.callContext(r.ctx, dockerQueryTimeout)
	if !ok {
		return
	}
	cmds, err := r.processes.Processes(ctx, name)
	cancel()
	switch {
	case err == nil:
		r.procErr(name).reset(log, "listing runner processes works again")
	case r.ctx.Err() != nil:
		return
	case isNotFound(err):
		log.Debug("runner container gone before its processes were listed")
		return
	default:
		err = withOp(retry.OpContainerTop, err)
		r.procErr(name).report(log, s.errs, "reconcile list runner processes",
			"listing runner processes failed; skipped this round (R83)",
			retry.Classify(r.ctx, retry.OpContainerTop, err), err)
		return
	}
	if hasWorker(cmds) && s.table.MarkBusyUnjudged(name, s.clock.Now()) {
		log.Info("Runner.Worker runs without an allow verdict; runner marked busy (R83)")
	}
}

// hasWorker reports whether one of the command lines runs Runner.Worker,
// the runner's job process (the apphost, or its .dll under dotnet).
func hasWorker(cmds []string) bool {
	for _, cmd := range cmds {
		for _, f := range strings.Fields(cmd) {
			if b := path.Base(f); b == "Runner.Worker" || b == "Runner.Worker.dll" {
				return true
			}
		}
	}
	return false
}

// startRunnerCheck starts R149 in its own goroutine when a row needs it,
// R149's rate limit allows it, and no earlier check still runs.
func (r *Reconciler) startRunnerCheck(rows []RunnerInfo) {
	s := r.s
	if s.table.Finalizing() {
		return
	}
	var names []string
	for _, info := range rows {
		if needsBusyCheck(info) {
			names = append(names, info.Name)
		}
	}
	if len(names) == 0 {
		return
	}
	if !s.limits.Allowed(retry.OpFindOrgRunner) {
		r.log.Debug("runner lookup deferred by rate limit", "until", s.limits.At(retry.OpFindOrgRunner))
		return
	}
	if !r.finding.CompareAndSwap(false, true) {
		r.log.Debug("previous runner lookups still running")
		return
	}
	r.mu.Lock()
	if r.stopped {
		r.mu.Unlock()
		r.finding.Store(false)
		return
	}
	r.finds.begin()
	r.mu.Unlock()
	go r.findRunners(names)
}

// findRunners is R149: it looks each runner up by name in the organization's
// runner list, one after another, each call within findRunnerTimeout, and
// marks busy each runner the lookup calls busy that is still neither allowed
// nor done (Table.MarkBusyUnjudged). A failed lookup is logged and skips
// that runner in this round; a rate limit ends the round's lookups and holds
// back the next until R52's time (R104); nothing starts a drain (R98).
func (r *Reconciler) findRunners(names []string) {
	defer r.finds.end()
	defer r.finding.Store(false)
	s := r.s
	for _, name := range names {
		if s.table.Finalizing() || r.ctx.Err() != nil {
			return
		}
		if !s.limits.Allowed(retry.OpFindOrgRunner) {
			r.log.Debug("runner lookups ended for this round by rate limit",
				"until", s.limits.At(retry.OpFindOrgRunner))
			return
		}
		r.findRunner(name)
	}
}

// findRunner looks one runner up and marks it busy when the organization's
// list says so.
func (r *Reconciler) findRunner(name string) {
	s := r.s
	ctx, cancel, ok := s.callContext(r.ctx, findRunnerTimeout)
	if !ok {
		return
	}
	found, ok, err := r.finder.FindRunner(ctx, name)
	cancel()
	if err != nil {
		if r.ctx.Err() != nil || s.table.Finalizing() {
			// Stopped, or ended by finalize, which removes every runner.
			return
		}
		s.limits.Record(retry.OpFindOrgRunner, err)
		err = withOp(retry.OpFindOrgRunner, err)
		r.findErr.report(r.log.With("runner", name), s.errs, "reconcile find runner",
			"looking a runner up failed; skipped this round (R149)",
			retry.Classify(r.ctx, retry.OpFindOrgRunner, err), err)
		return
	}
	r.findErr.reset(r.log, "looking runners up works again")
	if ok && found.Busy && s.table.MarkBusyUnjudged(name, s.clock.Now()) {
		r.log.Info("runner list says busy without an allow verdict; runner marked busy (R149)",
			"runner", name)
	}
}

// removeLeftover removes, one by one by ID, the resources of runner name,
// which had no row in the snapshot, unless a row appeared since. A resource
// whose removal is in progress, or failed with a permanent error before,
// is skipped (R42). Each resource has its own removal and retries, so a
// permanent failure marks the ID of the resource that failed and no other.
//
// The network goes last, and only once none of the runner's containers is
// still being removed or kept after a permanent failure: Docker refuses to
// remove a network with a container attached (403), so trying it then
// would only spend its retries. It is tried in a later round, once the
// containers are gone.
//
// The listing may be older than a retry that just finished, so a resource
// already gone can be removed once more; Docker answers 404, which counts
// as removed (R50).
func (r *Reconciler) removeLeftover(name string, g *runnerResources) {
	s := r.s
	if s.table.Finalizing() {
		return
	}
	if _, ok := s.table.Get(name); ok {
		return
	}
	for _, res := range g.all() {
		if s.table.Finalizing() {
			return
		}
		id := res.ID
		r.mu.Lock()
		skip := r.removing[id] || r.failed[id]
		if !skip && res.isNetwork() && r.containersLeftLocked(g) {
			r.mu.Unlock()
			r.log.Debug("network of a runner not in the table waits for its containers",
				"runner", name, "id", id)
			continue
		}
		if !skip {
			r.removing[id] = true
		}
		r.mu.Unlock()
		if skip {
			continue
		}
		log := r.log.With("runner", name, "kind", res.Kind, "id", id)
		log.Info("removing a resource of a runner not in the table")
		r.removeResource(log, *res, func(reason AbandonReason) {
			r.mu.Lock()
			defer r.mu.Unlock()
			delete(r.removing, id)
			if reason == AbandonPermanent {
				r.failed[id] = true
			}
		})
	}
}

// containersLeftLocked reports whether a container of g is still being
// removed or was kept after a permanent failure. r.mu must be held.
func (r *Reconciler) containersLeftLocked(g *runnerResources) bool {
	for _, c := range []*Resource{g.runner, g.buildKit} {
		if c != nil && (r.removing[c.ID] || r.failed[c.ID]) {
			return true
		}
	}
	return false
}

// removeResource removes res, a resource of a runner that has no row in the
// table, by its ID (R42): a runner container or an unpaired BuildKit
// container is force-removed, an unpaired network removed. The removal
// follows the retry rules of watch and reconcile (resourceRemoval) and
// counts as the Scaler's work in flight. done is called once when the
// removal ends (resourceRemoval.done).
func (r *Reconciler) removeResource(log *slog.Logger, res Resource, done func(AbandonReason)) {
	s := r.s
	s.work.begin()
	defer s.work.end()
	if s.table.Finalizing() {
		done(AbandonFinalize)
		return
	}
	rm := s.newResourceRemoval(context.Background(), log, res.RunnerName, false, done)
	rm.leftover = true
	rm.op = res.removalOp()
	kind, id := res.removalKind(), res.ID
	rm.remove = func(ctx context.Context) error { return r.remover.RemoveByID(ctx, kind, id) }
	rm.attempt()
}

// errorOnce remembers the last error of one call site, so a repeated error
// is logged at Warn and recorded only when it first appears or changes.
type errorOnce struct {
	mu   sync.Mutex
	last string
}

// report logs and records err when it differs from the last one; a repeat
// is logged at Debug only.
func (e *errorOnce) report(log *slog.Logger, errs ErrorRecorder, where, msg string, class retry.Class, err error) {
	text := class.String() + ": " + err.Error()
	e.mu.Lock()
	changed := text != e.last
	e.last = text
	e.mu.Unlock()
	if !changed {
		log.Debug(msg+" (same error again)", "class", class, "err", err)
		return
	}
	log.Warn(msg, "class", class, "err", err)
	errs.Record(where, err)
}

// reset forgets the last error, logging msg if there was one.
func (e *errorOnce) reset(log *slog.Logger, msg string) {
	e.mu.Lock()
	had := e.last != ""
	e.last = ""
	e.mu.Unlock()
	if had {
		log.Info(msg)
	}
}
