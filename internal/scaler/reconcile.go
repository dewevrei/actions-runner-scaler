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

// ReconcileTarget is one target's part of the reconciliation (0002 PLAN
// M11-5).
type ReconcileTarget struct {
	// Scaler is the target's Scaler: the removal rules, the logger and the
	// error recorder for the target's rows. Every target's Scaler uses the
	// one Table.
	Scaler *Scaler
	// Finder looks the target's runners up by name (R149).
	Finder RunnerFinder
	// Limits holds the target's rate limits for R149; nil means the Scaler's.
	Limits RateLimits
}

// ReconcilerConfig holds a Reconciler's dependencies; all are required
// except OnRound.
type ReconcilerConfig struct {
	// Targets holds every target, by target key (TargetConfig.Key); at least
	// one.
	Targets   map[string]ReconcileTarget
	Resources ResourceLister
	// Remover removes the resources of runners not in the table, one by one
	// (R42).
	Remover   ResourceIDRemover
	Processes ProcessLister
	// OnRound is called at the end of each round, for the placement that
	// follows it (0002 SPEC R212): Placer.Kick. It must not block; nil means
	// none.
	OnRound func()
}

// reconcileTarget is a ReconcileTarget with its own R149 error memory.
type reconcileTarget struct {
	s      *Scaler
	finder RunnerFinder
	limits RateLimits
	// findErr remembers the last error of R149, so a repeated error is
	// logged and recorded once.
	findErr errorOnce
}

// Reconciler is the server's 30-second reconciliation (R42), one for all
// targets (0002 PLAN M11-5). Each round:
//
//  1. From the table alone, begins R101's removal of every done runner whose
//     5 minutes have passed and whose removal has not begun (the backup of
//     the per-runner timer), and claims abandoned rows (Table.ClaimAbandoned)
//     to remove them again. A row abandoned on a permanent error is never
//     claimed (R42).
//  2. Starts R149 in its own goroutine, at most one at a time, when a runner
//     that is neither done nor allowed has not been marked busy: it looks
//     the runners up by name one after another, each with its target's
//     finder and rate limits.
//  3. Lists the Docker resources and matches them with the table by the
//     runner-name label: a row with no runner container, or a stopped one,
//     is handled as ended (except starting rows, which belong to their start
//     path); an idle runner whose BuildKit container is gone or stopped is
//     removed by the scale-down rules (R67); a running runner without an
//     allow verdict that has a Runner.Worker process is marked busy (R83);
//     resources of a runner not in the table are removed one by one, by ID,
//     except one whose removal failed with a permanent error (R42).
//
// Each row is handled with its own target's Scaler; a row of a target not in
// Targets is left alone. ResourceLister returns every managed-by resource on
// the host, so a resource whose runner is not in the table is removed, once,
// only when its runner-name has the form a target's start path gives,
// "<label>-" and 8 hex characters (RunnerName); others are skipped. The
// first target in key order whose label matches removes it.
//
// Removals follow the retry rules of watch and reconcile and never start a
// drain (R98). The rounds stop once finalize begins; every call is checked
// against the finalize mark first and ends by the finalize deadline.
//
// It is safe for concurrent use.
type Reconciler struct {
	// targets holds every target by key; keys lists them sorted. lead, the
	// first, supplies the clock and the server-wide logger and error
	// recorder.
	targets   map[string]*reconcileTarget
	keys      []string
	lead      *Scaler
	table     *Table
	clock     Clock
	resources ResourceLister
	remover   ResourceIDRemover
	processes ProcessLister
	onRound   func()
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

	// resourcesErr remembers the last error of the Docker list, so a
	// repeated error is logged and recorded once.
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
		{"Targets", len(cfg.Targets) == 0},
		{"Resources", cfg.Resources == nil},
		{"Remover", cfg.Remover == nil},
		{"Processes", cfg.Processes == nil},
	} {
		if d.missing {
			missing = append(missing, d.name)
		}
	}
	keys := make([]string, 0, len(cfg.Targets))
	for key, t := range cfg.Targets {
		keys = append(keys, key)
		if t.Scaler == nil {
			missing = append(missing, "Targets["+key+"].Scaler")
		}
		if t.Finder == nil {
			missing = append(missing, "Targets["+key+"].Finder")
		}
	}
	if len(missing) > 0 {
		slices.Sort(missing)
		return nil, errors.New("scaler: reconciler missing " + strings.Join(missing, ", "))
	}
	slices.Sort(keys)
	lead := cfg.Targets[keys[0]].Scaler
	targets := make(map[string]*reconcileTarget, len(keys))
	for _, key := range keys {
		t := cfg.Targets[key]
		if t.Scaler.target.Key != key {
			return nil, errors.New("scaler: reconciler target " + key + " has the Scaler of target " + t.Scaler.target.Key)
		}
		if t.Scaler.table != lead.table {
			return nil, errors.New("scaler: reconciler targets must share one Table")
		}
		limits := t.Limits
		if limits == nil {
			limits = t.Scaler.limits
		}
		targets[key] = &reconcileTarget{s: t.Scaler, finder: t.Finder, limits: limits}
	}
	onRound := cfg.OnRound
	if onRound == nil {
		onRound = func() {}
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &Reconciler{
		targets:   targets,
		keys:      keys,
		lead:      lead,
		table:     lead.table,
		clock:     lead.clock,
		resources: cfg.Resources,
		remover:   cfg.Remover,
		processes: cfg.Processes,
		onRound:   onRound,
		log:       lead.baseLog.With("component", "reconcile"),
		ctx:       ctx,
		cancel:    cancel,
		failed:    make(map[string]bool),
		removing:  make(map[string]bool),
		procErrs:  make(map[string]*errorOnce),
	}, nil
}

// Start schedules the first round reconcileInterval from now on the
// Scalers' Clock and returns. Later calls, and calls after Stop, do nothing.
func (r *Reconciler) Start() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.started || r.stopped {
		return
	}
	r.started = true
	r.next = r.clock.Now().Add(reconcileInterval)
	r.scheduleLocked()
}

// scheduleLocked sets the timer of the tick at r.next.
func (r *Reconciler) scheduleLocked() {
	r.ticks.begin()
	r.timer = r.clock.AfterFunc(r.next.Sub(r.clock.Now()), r.tick)
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
	if r.table.Finalizing() {
		r.log.Info("finalize began; reconciliation stopped")
		return
	}
	r.round()
	r.onRound()

	r.mu.Lock()
	defer r.mu.Unlock()
	if r.stopped || r.table.Finalizing() {
		return
	}
	now := r.clock.Now()
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
	if r.table.Finalizing() {
		return
	}
	// R101 first: it needs no outside call to decide, so a slow call below
	// never holds it back.
	now := r.clock.Now()
	for _, info := range r.table.List() {
		if t := r.rowTarget(info.Target); t != nil && info.State == StateDone && !info.DoneCleanupStarted &&
			!now.Before(info.CompletedAt.Add(doneCleanupDelay)) {
			t.s.doneCleanup(info.Name)
		}
	}
	for _, info := range r.table.List() {
		if t := r.rowTarget(info.Target); t != nil && info.Abandoned && info.AbandonReason != AbandonPermanent {
			t.s.reclaimAbandoned(info.Name)
		}
	}

	// The snapshot comes before the Docker list: a row that is not starting
	// in it had its containers created already, so the list cannot miss
	// them and make the row look ended.
	rows := r.table.List()
	r.startRunnerCheck(rows)

	if r.table.Finalizing() {
		return
	}
	ctx, cancel, ok := r.lead.callContext(r.ctx, dockerQueryTimeout)
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
		r.resourcesErr.report(r.log, r.lead.errs, "reconcile list resources",
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
		if t := r.rowTarget(info.Target); t != nil {
			r.reconcileRow(t.s, info, groups[info.Name])
		}
	}
	names := make([]string, 0, len(groups))
	for name := range groups {
		if !inTable[name] && r.leftoverOwner(name) != nil {
			names = append(names, name)
		}
	}
	slices.Sort(names)
	for _, name := range names {
		r.removeLeftover(r.leftoverOwner(name), name, groups[name])
	}
}

// rowTarget returns the target of a row with target key key, nil when the
// reconciler has none or the target's finalize mark is set: from its error
// drain's end cleanup until it runs again its rows are skipped, while the
// other targets' rows are reconciled (0002 SPEC R190).
func (r *Reconciler) rowTarget(key string) *reconcileTarget {
	if r.table.TargetFinalizing(key) {
		return nil
	}
	return r.targets[key]
}

// leftoverOwner returns the Scaler that removes the resources of runner
// name, which has no row: the first target in key order whose start path
// gives names of that form (RunnerName), nil when none does.
func (r *Reconciler) leftoverOwner(name string) *Scaler {
	for _, key := range r.keys {
		if s := r.targets[key].s; RunnerName(s.target.Label, name) {
			return s
		}
	}
	return nil
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

// reconcileRow matches one row of the snapshot with its listed resources,
// by the rules of s, the Scaler of the row's target.
func (r *Reconciler) reconcileRow(s *Scaler, info RunnerInfo, g *runnerResources) {
	if info.State == StateStarting || info.State == StateExiting {
		// The start path, or the owner of the removal, has the row.
		return
	}
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
		r.checkWorker(s, log, info.Name)
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
func (r *Reconciler) checkWorker(s *Scaler, log *slog.Logger, name string) {
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

// findBatch is the runners of one target that R149 looks up in a round.
type findBatch struct {
	t     *reconcileTarget
	names []string
}

// startRunnerCheck starts R149 in its own goroutine when a row needs it, its
// target's R149 rate limit allows it, and no earlier check still runs.
func (r *Reconciler) startRunnerCheck(rows []RunnerInfo) {
	if r.table.Finalizing() {
		return
	}
	byTarget := make(map[string][]string)
	for _, info := range rows {
		if r.rowTarget(info.Target) != nil && needsBusyCheck(info) {
			byTarget[info.Target] = append(byTarget[info.Target], info.Name)
		}
	}
	var batches []findBatch
	for _, key := range r.keys {
		names := byTarget[key]
		if len(names) == 0 {
			continue
		}
		t := r.targets[key]
		if !t.limits.Allowed(retry.OpFindOrgRunner) {
			t.s.log.Debug("runner lookup deferred by rate limit", "until", t.limits.At(retry.OpFindOrgRunner))
			continue
		}
		batches = append(batches, findBatch{t: t, names: names})
	}
	if len(batches) == 0 {
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
	go r.findRunners(batches)
}

// findRunners is R149: it looks each runner up by name in its target's
// organization runner list, one after another, each call within
// findRunnerTimeout, and marks busy each runner the lookup calls busy that
// is still neither allowed nor done (Table.MarkBusyUnjudged). A failed
// lookup is logged and skips that runner in this round; a rate limit ends
// the round's lookups of that target and holds back its next until R52's
// time (R104), while other targets' lookups go on (0002 SPEC R188); nothing
// starts a drain (R98).
func (r *Reconciler) findRunners(batches []findBatch) {
	defer r.finds.end()
	defer r.finding.Store(false)
	for _, b := range batches {
		for _, name := range b.names {
			if r.table.Finalizing() || r.ctx.Err() != nil {
				return
			}
			if !b.t.limits.Allowed(retry.OpFindOrgRunner) {
				b.t.s.log.Debug("runner lookups ended for this round by rate limit",
					"until", b.t.limits.At(retry.OpFindOrgRunner))
				break
			}
			r.findRunner(b.t, name)
		}
	}
}

// findRunner looks one runner of target t up and marks it busy when the
// organization's list says so.
func (r *Reconciler) findRunner(t *reconcileTarget, name string) {
	s := t.s
	log := s.log.With("component", "reconcile")
	ctx, cancel, ok := s.callContext(r.ctx, findRunnerTimeout)
	if !ok {
		return
	}
	found, ok, err := t.finder.FindRunner(ctx, name)
	cancel()
	if err != nil {
		if r.ctx.Err() != nil || s.table.Finalizing() {
			// Stopped, or ended by finalize, which removes every runner.
			return
		}
		t.limits.Record(retry.OpFindOrgRunner, err)
		err = withOp(retry.OpFindOrgRunner, err)
		t.findErr.report(log.With("runner", name), s.errs, "reconcile find runner",
			"looking a runner up failed; skipped this round (R149)",
			retry.Classify(r.ctx, retry.OpFindOrgRunner, err), err)
		return
	}
	t.findErr.reset(log, "looking runners up works again")
	if ok && found.Busy && s.table.MarkBusyUnjudged(name, s.clock.Now()) {
		log.Info("runner list says busy without an allow verdict; runner marked busy (R149)",
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
func (r *Reconciler) removeLeftover(s *Scaler, name string, g *runnerResources) {
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
		r.removeResource(s, log, *res, func(reason AbandonReason) {
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
// counts as the work in flight of s, the Scaler that owns the leftover. done
// is called once when the removal ends (resourceRemoval.done).
func (r *Reconciler) removeResource(s *Scaler, log *slog.Logger, res Resource, done func(AbandonReason)) {
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
