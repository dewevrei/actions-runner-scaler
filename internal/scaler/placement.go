package scaler

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/actions/scaleset"

	"github.com/dewevrei/actions-runner-scaler/internal/retry"
)

// PlacementSession is a target's current message session as placement uses
// it (0002 PLAN M14-4): AcquireJobs for one waiting job (0002 SPEC R214),
// and SessionLost when that call found the session gone, which ends the
// listener on it so the target's session loop makes a new session (R25,
// 0002 SPEC R216). SessionLost is called with the Placer's lock held, so it
// must not block and must not call the Placer.
type PlacementSession interface {
	JobAcquirer
	SessionLost(err error)
}

// PlacerConfig holds a Placer's dependencies; all are required.
type PlacerConfig struct {
	Table *Table
	Clock Clock
	// Context lives as long as the server: finalize or the end of the daemon
	// cancels it. Every AcquireJobs, JIT and runner start of a placement runs
	// on it, never on the context of the Scale that ran the placement, so a
	// target's listener ending does not abort another target's call (0002
	// PLAN M14).
	Context context.Context
	// Logger is optional; nil discards logs.
	Logger *slog.Logger
}

// Placer is the server's one placement (0002 SPEC R184~R187, R212~R229): it
// holds the waiting job list (0002 PLAN M5) and is the only code that calls
// AcquireJobs or starts runners (0002 SPEC R115, R214). Every target's
// Scaler is added to it in config order (AddTarget).
//
// One placement runs after each message a running target's Scale applied,
// after each Kick (a row leaving the table, the end of a reconciliation
// round; 0002 SPEC R170, R212), never during a drain of the server or
// finalize (R220). The whole placement holds the Placer's lock, network calls
// included, so placements and the list never run concurrently; the lock
// order is a Scaler's scaleMu, then this lock, then Table.mu, and nothing
// called under this lock calls a Placer method other than Kick or takes a
// scaleMu (package comment).
//
// It is safe for concurrent use.
type Placer struct {
	table *Table
	clock Clock
	ctx   context.Context
	log   *slog.Logger
	// kick holds one pending placement request (Kick).
	kick chan struct{}
	// runs counts the kick goroutine, for Wait.
	runs      inFlight
	startOnce sync.Once

	// waitMu guards waiting, which status reads without waiting for a
	// placement in progress (0002 SPEC R222).
	waitMu  sync.Mutex
	waiting map[string]int

	mu      sync.Mutex
	list    *WaitingList
	targets []*placeTarget // config order
	byKey   map[string]*placeTarget
}

// placeTarget is one target as the Placer sees it.
type placeTarget struct {
	s     *Scaler
	order int // config order (0002 SPEC R184)
	// sess is the target's current session; nil between sessions.
	sess PlacementSession
	// active is true while the target is running (0002 SPEC R180, R210).
	active bool
}

func (t *placeTarget) key() string { return t.s.target.Key }

// url is the target's URL for log lines, its key when the URL is unknown.
func (t *placeTarget) url() string {
	if t.s.target.URL != "" {
		return t.s.target.URL
	}
	return t.s.target.Key
}

// NewPlacer returns a Placer without targets whose kick goroutine has not
// started.
func NewPlacer(cfg PlacerConfig) (*Placer, error) {
	var missing []string
	if cfg.Table == nil {
		missing = append(missing, "Table")
	}
	if cfg.Clock == nil {
		missing = append(missing, "Clock")
	}
	if cfg.Context == nil {
		missing = append(missing, "Context")
	}
	if len(missing) > 0 {
		return nil, errors.New("scaler: placer missing " + strings.Join(missing, ", "))
	}
	log := cfg.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Placer{
		table:   cfg.Table,
		clock:   cfg.Clock,
		ctx:     cfg.Context,
		log:     log.With("component", "placement"),
		kick:    make(chan struct{}, 1),
		waiting: make(map[string]int),
		list:    NewWaitingList(),
		byKey:   make(map[string]*placeTarget),
	}, nil
}

// AddTarget adds the next target in config order. From then on s's Scale
// hands its messages to the Placer, and only the Placer starts s's runners.
// The target starts inactive (SetActive). s must use the Placer's table and
// may be added once.
func (p *Placer) AddTarget(s *Scaler) error {
	if s.table != p.table {
		return errors.New("scaler: placer target " + s.target.Key + " uses another Table")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.byKey[s.target.Key] != nil || s.placer != nil {
		return errors.New("scaler: placer target " + s.target.Key + " added twice")
	}
	t := &placeTarget{s: s, order: len(p.targets)}
	p.targets = append(p.targets, t)
	p.byKey[s.target.Key] = t
	s.placer = p
	return nil
}

// Start starts the kick goroutine, which runs one placement per Kick until
// the Placer's context ends. Later calls do nothing.
func (p *Placer) Start() {
	p.startOnce.Do(func() {
		p.runs.begin()
		go func() {
			defer p.runs.end()
			for {
				select {
				case <-p.ctx.Done():
					return
				case <-p.kick:
					p.mu.Lock()
					p.placeLocked()
					p.mu.Unlock()
				}
			}
		}()
	})
}

// Kick asks for one placement (0002 SPEC R170, R212). It never blocks and
// takes no lock, so it may be called anywhere, also under the Placer's lock
// (Table.OnRemoved during a start's cleanup). A kick that arrives while a
// placement or a Scale holds the lock runs once they are done.
func (p *Placer) Kick() {
	select {
	case p.kick <- struct{}{}:
	default:
	}
}

// Wait waits until the kick goroutine has returned, which it does after the
// Placer's context ended and the placement it was running finished, or until
// ctx ends.
func (p *Placer) Wait(ctx context.Context) error { return p.runs.wait(ctx) }

// SetActive records whether target is running (0002 SPEC R180). Making it
// inactive, when it enters its target error drain or wait, removes its
// entries and keeps its messages out of the list until it runs again (R210).
// It must not be called under the Placer's lock.
func (p *Placer) SetActive(target string, active bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	t := p.byKey[target]
	if t == nil || t.active == active {
		return
	}
	t.active = active
	if !active {
		p.dropLocked(t)
	}
}

// dropLocked removes t's entries and its Statistics state (0002 SPEC R210).
func (p *Placer) dropLocked(t *placeTarget) {
	t.active = false
	p.list.DropTarget(t.key())
	p.setWaiting(t.key(), 0)
}

// SetSession sets target's current session, or nil while it has none (0002
// PLAN M14-4): the target's session loop sets it right before listener.Run
// and clears it right after; a placement clears it too when AcquireJobs
// found it lost. Without a session the target's unassigned entries wait
// (0002 SPEC R213 (1)). It must not be called under the Placer's lock.
func (p *Placer) SetSession(target string, s PlacementSession) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if t := p.byKey[target]; t != nil {
		t.sess = s
	}
}

// WaitingJobs returns target's entries the last placement did not cover,
// taken-back and skipped ones included (0002 SPEC R222). It does not wait
// for a placement in progress.
func (p *Placer) WaitingJobs(target string) int {
	p.waitMu.Lock()
	defer p.waitMu.Unlock()
	return p.waiting[target]
}

func (p *Placer) setWaiting(target string, n int) {
	p.waitMu.Lock()
	defer p.waitMu.Unlock()
	p.waiting[target] = n
}

// scaled is the Placer's share of s's Scale, with s's scaleMu held: the
// table steps, then the waiting list when s's target runs (0002 SPEC R206~
// R209, R219, R221, R226), then one placement (R212), all in one section
// under the Placer's lock, so a kicked placement never sees a JobStarted in
// the table whose entry is still listed (0002 PLAN M14-1). A message of an
// inactive target changes the table only (R210).
func (p *Placer) scaled(s *Scaler, msg *scaleset.RunnerScaleSetMessage) {
	p.mu.Lock()
	defer p.mu.Unlock()
	s.applyJobs(msg)
	t := p.byKey[s.target.Key]
	if t == nil || !t.active {
		return
	}
	if msg != nil {
		p.applyLocked(t, msg)
	}
	p.placeLocked()
}

// applyLocked applies msg to the list and logs what changed.
func (p *Placer) applyLocked(t *placeTarget, msg *scaleset.RunnerScaleSetMessage) {
	log := p.log.With("target", t.url())
	ev := p.list.Apply(t.key(), t.order, msg, p.clock.Now())
	for _, e := range ev.Offered {
		log.Info("waiting job offered", e.jobAttrs()...)
	}
	for _, e := range ev.Assigned {
		log.Info("waiting job assigned", e.jobAttrs()...)
	}
	for _, e := range ev.TakenBack {
		log.Info("waiting job taken back", e.jobAttrs()...)
	}
	for _, e := range ev.Removed {
		log.Info("waiting job removed", e.jobAttrs()...)
	}
	st := msg.Statistics
	if st == nil {
		return
	}
	// JobStarted of this message is in the table already: scaled recorded
	// it first (0002 SPEC R224).
	started := p.table.Snapshot()[t.key()].Started
	res := p.list.Statistics(t.key(), *st, started)
	for _, e := range res.Dropped {
		log.Info("waiting job dropped; GitHub counts fewer assigned jobs",
			"runnerRequestId", e.requestID, "workflowRunId", e.runID, "jobDisplayName", e.jobName,
			"totalAssignedJobs", st.TotalAssignedJobs, "startedRunners", started)
	}
	if res.Warn {
		log.Warn("GitHub counts more available jobs than the scaler knows",
			"github", st.TotalAvailableJobs, "known", res.Known)
	}
	if res.UnlistedChanged {
		log.Info("assigned jobs not in the waiting list", "count", res.Unlisted)
	}
}

// jobAttrs are the log fields of one entry (0002 PLAN M14-1).
func (e Entry) jobAttrs() []any {
	return []any{"runnerRequestId", e.requestID, "workflowRunId", e.runID,
		"jobDisplayName", e.jobName, "jobId", e.jobID}
}

// placeLocked runs one placement (0002 PLAN M14-3) with the Placer's lock
// held.
func (p *Placer) placeLocked() {
	pl := &placement{p: p, now: p.clock.Now(), limit: p.table.ServerLimit()}
	defer pl.storeWaiting()
	if pl.halt() {
		return
	}
	for _, e := range p.list.Expire(pl.now) {
		log := p.log
		if t := p.byKey[e.target]; t != nil {
			log = log.With("target", t.url())
		}
		log.Info("waiting job expired", "runnerRequestId", e.requestID, "queueTime", e.queueTime)
	}
	pl.count()
	pl.entries()
	pl.unlisted()
	pl.goals()
	pl.idle()
	pl.scaleDown()
}

// storeWaiting stores each target's entries the placement did not cover
// (0002 SPEC R222, 0002 PLAN M14-3.8), also when it halted before counting:
// it covered none then, so every entry counts.
func (pl *placement) storeWaiting() {
	for _, t := range pl.p.targets {
		n := 0
		if t.active { // a target dropped in this placement has none
			covered := 0
			if c := pl.of(t.key()); c != nil {
				covered = c.covered
			}
			n = max(0, pl.p.list.Count(t.key())-covered)
		}
		pl.p.setWaiting(t.key(), n)
	}
}

// placement is one placement's counts (0002 PLAN M14-3.2): the table
// snapshot taken once, changed by every start of the placement so a later
// step never starts a runner for a need an earlier step met.
type placement struct {
	p     *Placer
	now   time.Time
	limit int
	// sumAll is the rows of all targets (0002 SPEC R114); skipped counts
	// the entries skipped for now (R213 (1), R229).
	sumAll, skipped int
	counts          []*targetCount // config order
	halted          bool
}

// targetCount is one target's counts in a placement.
type targetCount struct {
	t *placeTarget
	// The snapshot (0002 SPEC R224); coverIdle and coverBusy are the
	// remaining cover.
	coverIdle, coverBusy, started, all, live int
	// covered and skipped count the entries of the target the placement
	// covered and skipped for now (0002 SPEC R115, R213).
	covered, skipped int
	// stopped: the placement looks at no more entries of the target (0002
	// SPEC R216, R220).
	stopped bool
	// failed: a start of the target failed in this placement, so the
	// stages of R187 do not try it again for the same need.
	failed bool
	goal   int
}

// count takes the snapshot (0002 PLAN M14-3.2).
func (pl *placement) count() {
	snap := pl.p.table.Snapshot()
	for _, ts := range snap {
		pl.sumAll += ts.All
	}
	for _, t := range pl.p.targets {
		ts := snap[t.key()]
		pl.counts = append(pl.counts, &targetCount{
			t: t, coverIdle: ts.CoverIdle, coverBusy: ts.CoverBusy,
			started: ts.Started, all: ts.All, live: ts.Live,
		})
	}
}

func (pl *placement) of(target string) *targetCount {
	for _, c := range pl.counts {
		if c.t.key() == target {
			return c
		}
	}
	return nil
}

// halt reports whether the placement must end at once: a drain of the
// server or finalize began, or the Placer's context ended (0002 SPEC R220,
// 0002 PLAN M14-3 re-check).
func (pl *placement) halt() bool {
	if !pl.halted && (pl.p.ctx.Err() != nil || pl.p.table.Draining()) {
		pl.halted = true
	}
	return pl.halted
}

// usable is the re-check right before each AcquireJobs and each start
// (0002 PLAN M14-3): false when the placement halted, or when c's target no
// longer runs or its drain mark is set, which stops c.
func (pl *placement) usable(c *targetCount) bool {
	if pl.halt() {
		return false
	}
	if !c.t.active || pl.p.table.TargetDraining(c.t.key()) {
		c.stopped = true
	}
	return !c.stopped
}

// backoff reports whether c's target is in its start backoff (R61).
func (pl *placement) backoff(c *targetCount) bool {
	return pl.now.Before(pl.p.table.StartStatus(c.t.key()).NextStartAt)
}

// room reports whether a new runner of c's target fits both limits (R3,
// 0002 SPEC R114).
func (pl *placement) room(c *targetCount) bool {
	return c.all < c.t.s.target.MaxRunners && pl.sumAll < pl.limit
}

// canFill reports whether the stages of 0002 SPEC R187 may start a runner of
// c's target: it runs, the placement did not stop it, and neither its start
// backoff nor its JIT wait is on (R186, R87), which it logs.
func (pl *placement) canFill(c *targetCount) bool {
	if !c.t.active || c.stopped || c.failed || pl.p.table.TargetDraining(c.t.key()) {
		return false
	}
	s := c.t.s
	if next := pl.p.table.StartStatus(c.t.key()).NextStartAt; pl.now.Before(next) {
		s.log.Info("runner start deferred by start backoff", "until", next)
		return false
	}
	if !s.limits.Allowed(retry.OpGenerateJIT) {
		s.log.Info("runner start deferred by JIT rate limit", "until", s.limits.At(retry.OpGenerateJIT))
		return false
	}
	return true
}

func (pl *placement) skip(c *targetCount) {
	c.skipped++
	pl.skipped++
}

// entries walks the list in placement order (0002 PLAN M14-3.3, 0002 SPEC
// R184, R213, R225).
func (pl *placement) entries() {
	p := pl.p
	for _, e := range p.list.Ordered(func(target string) bool {
		t := p.byKey[target]
		return t != nil && t.active
	}) {
		if pl.halt() {
			return
		}
		c := pl.of(e.target)
		if !c.t.active {
			// A permanent AcquireJobs error earlier in this placement
			// dropped the target's entries (0002 SPEC R210).
			continue
		}
		pl.entry(c, e)
	}
}

// entry places one entry (0002 SPEC R213). The call an entry needs is the
// one it would make now: AcquireJobs when unassigned and it gets cover or a
// runner, JIT when it gets a runner. An entry that gets neither needs no
// call and is left (R169), not skipped.
func (pl *placement) entry(c *targetCount, e *Entry) {
	limits := c.t.s.limits
	assigned := e.assigned // before AcquireJobs changes it
	cover := c.coverIdle > 0 || (assigned && c.coverBusy > 0)
	start := !cover && pl.room(c)
	acquire := !assigned && (cover || start)
	if acquire && e.requestID == 0 {
		// No RunnerRequestID to acquire with; the job waits for a message
		// that carries one.
		return
	}
	switch {
	case c.stopped, pl.backoff(c),
		acquire && (c.t.sess == nil || !limits.Allowed(retry.OpAcquireJobs)),
		start && !limits.Allowed(retry.OpGenerateJIT):
		pl.skip(c) // (1), R186, R216
		return
	case !cover && !start:
		return // (4), R169
	}
	if acquire {
		switch pl.acquire(c, e) {
		case acquireGone, acquireDropped, acquireHalt:
			return
		case acquireSkip:
			pl.skip(c)
			return
		}
	}
	if cover { // (2): busy cover only for an entry GitHub assigned already
		if assigned && c.coverBusy > 0 {
			c.coverBusy--
		} else {
			c.coverIdle--
		}
		c.covered++
		return
	}
	switch pl.start(c) { // (3); the runner is the entry's, not cover
	case startDone:
		c.covered++
	case startFailed, startNoRoom:
		pl.skip(c) // keeps its share of the goal and its room (R229)
	case startStopped:
		if !pl.halted {
			pl.skip(c)
		}
	}
}

// acquireResult is what AcquireJobs for one entry led to.
type acquireResult int

const (
	acquireOK      acquireResult = iota // the entry is assigned now
	acquireGone                         // removed: GitHub did not return it
	acquireSkip                         // kept; the target is stopped
	acquireDropped                      // permanent: the target's entries are gone
	acquireHalt                         // the placement ends
)

// acquire calls AcquireJobs for one unassigned entry (0002 SPEC R214,
// R216, R171).
func (pl *placement) acquire(c *targetCount, e *Entry) acquireResult {
	p := pl.p
	if !pl.usable(c) {
		if pl.halted {
			return acquireHalt
		}
		return acquireSkip
	}
	s := c.t.s
	log := p.log.With("target", c.t.url(), "runnerRequestId", e.requestID)
	ctx, cancel := context.WithTimeout(p.ctx, callTimeout)
	got, err := c.t.sess.AcquireJobs(ctx, []int64{e.requestID})
	cancel()
	if err == nil {
		if slices.Contains(got, e.requestID) {
			p.list.MarkAcquired(e)
			log.Info("job acquired")
			return acquireOK
		}
		p.list.Remove(e)
		log.Info("job no longer available")
		return acquireGone
	}
	if p.ctx.Err() != nil {
		pl.halted = true
		return acquireHalt
	}
	s.limits.Record(retry.OpAcquireJobs, err)
	err = withOp(retry.OpAcquireJobs, err)
	c.stopped = true
	switch class := retry.Classify(p.ctx, retry.OpAcquireJobs, err); class {
	case retry.Permanent:
		log.Error("AcquireJobs failed permanently; target error drain (0002 SPEC R171)", "err", err)
		// Under the Placer's lock, not through SetActive: the target's own
		// goroutine runs the drain after this placement (0002 PLAN M14-3.4).
		// During a server drain the target does not enter its error drain
		// (R100, 0002 SPEC R191); the placement halts then anyway.
		if p.table.Drain() != DrainNone {
			s.fatal(err) // logged only (R191)
			return acquireSkip
		}
		p.dropLocked(c.t)
		p.table.SetTargetDraining(c.t.key())
		s.fatal(err)
		return acquireDropped
	case retry.SessionLost:
		log.Warn("AcquireJobs found the session gone; new session (R25, 0002 SPEC R216)", "err", err)
		s.errs.Record("AcquireJobs", err)
		c.t.sess.SessionLost(err)
		// No AcquireJobs on the dead session until the session loop sets
		// the new one (0002 SPEC R213 (1), R216).
		c.t.sess = nil
	default:
		log.Warn("AcquireJobs failed; the target's entries wait (0002 SPEC R216)", "class", class, "err", err)
		s.errs.Record("AcquireJobs", err)
	}
	return acquireSkip
}

// startResult is what one start of a placement led to.
type startResult int

const (
	startDone    startResult = iota // the runner is idle
	startFailed                     // counted (R61, R64) or stopped without a failure
	startNoRoom                     // Table.Add found no room
	startStopped                    // the re-check stopped the target or halted
)

// start starts one runner of c's target, adding it to the counts first and
// taking it back when the start fails (0002 PLAN M14-3.2).
func (pl *placement) start(c *targetCount) startResult {
	if !pl.usable(c) {
		return startStopped
	}
	c.live++
	c.all++
	pl.sumAll++
	err := c.t.s.startRunner(pl.p.ctx)
	if err == nil {
		return startDone
	}
	c.live--
	c.all--
	pl.sumAll--
	switch {
	case errors.Is(err, ErrNoRoom):
		c.t.s.log.Info("runner start deferred; no room under max-runners or the server limit")
		return startNoRoom
	case errors.Is(err, errStartAborted) && pl.halt():
		return startStopped
	}
	c.failed = true
	return startFailed
}

// unlisted is the first stage of 0002 SPEC R187: runners for the assigned
// jobs not in the list (R219), up to the remaining cover, which holds busy
// runners without their own JobStarted too (R228); with a skipped entry only
// while room beyond the skipped entries is left (R229).
func (pl *placement) unlisted() {
	for _, c := range pl.counts {
		want := pl.p.list.Unlisted(c.t.key())
		for c.coverIdle+c.coverBusy < want && pl.room(c) && pl.canFill(c) &&
			(pl.skipped == 0 || pl.limit-pl.sumAll-pl.skipped > 0) {
			if pl.start(c) != startDone {
				break
			}
			c.coverIdle++
		}
		if pl.halted {
			return
		}
	}
}

// goals computes every running target's goal (0002 SPEC R115), also when
// the second stage starts nothing, since scaleDown reads it.
func (pl *placement) goals() {
	for _, c := range pl.counts {
		tc := c.t.s.target
		c.goal = min(tc.MaxRunners,
			c.started+c.covered+c.skipped+tc.Idle+pl.p.list.Unlisted(c.t.key()))
	}
}

// idle is the second stage of 0002 SPEC R187: runners up to the goal, only
// when no entry was skipped (R229). Earlier starts of this placement raised
// live already, so no need is met twice.
func (pl *placement) idle() {
	if pl.skipped > 0 || pl.halted {
		return
	}
	for _, c := range pl.counts {
		for c.live < c.goal && pl.room(c) && pl.canFill(c) {
			if pl.start(c) != startDone {
				break
			}
		}
		if pl.halted {
			return
		}
	}
}

// scaleDown removes up to live − goal runners that are not busy from each
// running target (0002 SPEC R185, R37).
func (pl *placement) scaleDown() {
	for _, c := range pl.counts {
		if c.live <= c.goal || !c.t.active || pl.p.table.TargetDraining(c.t.key()) {
			continue
		}
		if pl.halt() {
			return
		}
		if err := c.t.s.removeIdle(pl.p.ctx, c.live-c.goal); err != nil {
			c.t.s.log.Debug("placement scale-down stopped", "err", err)
		}
	}
}
