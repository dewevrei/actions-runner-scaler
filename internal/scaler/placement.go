package scaler

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/actions/scaleset"

	"github.com/dewevrei/actions-runner-scaler/internal/retry"
)

// PlacerConfig holds a Placer's dependencies; all are required.
type PlacerConfig struct {
	Table *Table
	Clock Clock
	// Context lives as long as the server: finalize or the end of the daemon
	// cancels it. Every JIT call and runner start of a placement runs on it,
	// never on the context of the Scale that ran the placement, so a target's
	// listener ending does not abort another target's call (0002 PLAN M14).
	Context context.Context
	// Logger is optional; nil discards logs.
	Logger *slog.Logger
}

// Placer scales every target to its desired runner count and is the
// server's one start gate.
//
// A target's desired count is min(max-runners, idle + TotalAssignedJobs of
// its last message with Statistics), as in ARC (cmd/ghalistener/scaler
// scaler.go) and the actions/scaleset example (examples/dockerscaleset
// scaler.go). Below it the target asks the gate for starts; above it the
// placement removes idle runners. The gate hands the free slots under the
// server limit to the asks in the order they were made, across targets:
// first the asks for assigned jobs, then the asks that only fill idle (0002
// SPEC R187). A target that cannot start now (start backoff, JIT rate limit,
// an image being pulled, max-runners reached) keeps its asks and their
// place; while one waits for its backoff, rate limit or pull, idle fills
// of other targets leave it a slot for each of its job asks.
//
// One placement runs after each message a running target's Scale applied
// and after each Kick (a row leaving the table, the end of a reconciliation
// round, the end of an image pull; 0002 SPEC R170, R212), never during a
// drain of the server or finalize. The whole placement holds the Placer's
// lock, network calls included, so placements never run concurrently; the
// lock order is a Scaler's scaleMu, then this lock, then Table.mu, and
// nothing called under this lock calls a Placer method other than Kick or
// takes a scaleMu (package comment).
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
	// placement in progress.
	waitMu  sync.Mutex
	waiting map[string]int

	mu      sync.Mutex
	targets []*placeTarget // config order
	byKey   map[string]*placeTarget
	// seq numbers the asks in the order they were made.
	seq uint64
}

// placeTarget is one target as the Placer sees it.
type placeTarget struct {
	s *Scaler
	// sess is the target's current session; nil between sessions.
	sess JobAcquirer
	// active is true while the target is running (0002 SPEC R180).
	active bool
	// assigned is TotalAssignedJobs of the target's last message with
	// Statistics since it began running; 0 before the first.
	assigned int
	// jobAsks and idleAsks are the target's start asks not yet granted, for
	// assigned jobs and for idle runners, each oldest first, numbered by
	// Placer.seq when made. Each kind keeps its own place, so an idle ask
	// made early never takes the place of a later job ask.
	jobAsks, idleAsks []uint64
	// desired is the desired count last logged; logged reports whether one
	// was. held reports whether the last placement logged the target's
	// starts as held by its backoff or rate limit.
	desired      int
	logged, held bool
}

// forget drops the target's asks and what it learned from its session.
func (t *placeTarget) forget() {
	t.assigned, t.jobAsks, t.idleAsks, t.logged, t.held = 0, nil, nil, false, false
}

// asks is the number of the target's asks of both kinds.
func (t *placeTarget) asks() int { return len(t.jobAsks) + len(t.idleAsks) }

func (t *placeTarget) key() string { return t.s.target.Key }

// url is the target's URL for log lines, its key when the URL is unknown.
func (s *Scaler) url() string {
	if s.target.URL != "" {
		return s.target.URL
	}
	return s.target.Key
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
	t := &placeTarget{s: s}
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
// inactive, when it enters its target error drain or wait, forgets its asks
// and its last TotalAssignedJobs, which belong to the session it is leaving;
// until it runs again its messages change the table only. It must not be
// called under the Placer's lock.
func (p *Placer) SetActive(target string, active bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	t := p.byKey[target]
	if t == nil || t.active == active {
		return
	}
	t.active = active
	if !active {
		t.forget()
		p.setWaiting(target, 0)
	}
}

// SetSession sets target's current session, or nil while it has none: the
// target's session loop sets it right before listener.Run and clears it
// right after. Scale acquires the target's available jobs on it. It must not
// be called under the Placer's lock.
func (p *Placer) SetSession(target string, s JobAcquirer) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if t := p.byKey[target]; t != nil {
		t.sess = s
	}
}

// WaitingJobs returns target's assigned jobs that no runner of it covers:
// TotalAssignedJobs of its last message with Statistics minus its live
// runners, at least 0, as of the last placement. It does not wait for a
// placement in progress.
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
// table steps and the message's TotalAssignedJobs in one section under the
// Placer's lock, so no placement sees one without the other; then
// AcquireJobs for the message's available jobs at once, outside the lock;
// then one placement (0002 SPEC R212). A message of an inactive target
// changes the table only. It returns the AcquireJobs error, which ends the
// listener so the target's session loop handles it (R25, 0002 SPEC R171).
func (p *Placer) scaled(ctx context.Context, s *Scaler, msg *scaleset.RunnerScaleSetMessage) error {
	sess, active := p.record(s, msg)
	if !active {
		return nil
	}
	if err := s.acquireAvailable(ctx, sess, msg); err != nil {
		return err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.placeLocked()
	return nil
}

// record applies msg's job messages to the table and, when s's target runs,
// keeps the message's TotalAssignedJobs. It returns the target's session and
// whether the target runs.
func (p *Placer) record(s *Scaler, msg *scaleset.RunnerScaleSetMessage) (JobAcquirer, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	s.applyJobs(msg)
	t := p.byKey[s.target.Key]
	if t == nil || !t.active {
		return nil, false
	}
	if msg != nil && msg.Statistics != nil {
		t.assigned = max(0, msg.Statistics.TotalAssignedJobs)
	}
	return t.sess, true
}

// ask makes the number of asks in list n: new asks join the back of the
// gate's order, and asks no longer needed leave from the newest.
func (p *Placer) ask(list []uint64, n int) []uint64 {
	for len(list) < n {
		p.seq++
		list = append(list, p.seq)
	}
	return list[:n]
}

// acquireAvailable calls AcquireJobs once for every available job of msg
// that has a RunnerRequestID, as soon as the message arrives (ARC
// cmd/ghalistener/scaler/scaler.go, the actions/scaleset example
// examples/dockerscaleset/scaler.go). During a drain of the server or of
// the target, and once finalize began, it calls nothing (0002 SPEC R220).
// A permanent error starts the target's error drain at once: the drain mark
// is set before Fatal, so no placement starts for the target however late
// the drain itself begins (0002 SPEC R171). Every error is returned.
func (s *Scaler) acquireAvailable(ctx context.Context, sess JobAcquirer, msg *scaleset.RunnerScaleSetMessage) error {
	if msg == nil || len(msg.JobAvailableMessages) == 0 {
		return nil
	}
	var ids []int64
	for _, m := range msg.JobAvailableMessages {
		if m != nil && m.RunnerRequestID != 0 {
			ids = append(ids, m.RunnerRequestID)
		}
	}
	switch {
	case len(ids) == 0:
		return nil
	case s.draining():
		s.log.Info("available jobs not acquired during a drain", "count", len(ids))
		return nil
	case sess == nil:
		s.log.Warn("available jobs not acquired; the target has no session", "count", len(ids))
		return nil
	}
	callCtx, cancel := context.WithTimeout(ctx, callTimeout)
	got, err := sess.AcquireJobs(callCtx, ids)
	cancel()
	if err == nil {
		s.log.Info("jobs acquired", "target", s.url(), "requested", len(ids), "acquired", len(got))
		return nil
	}
	err = withOp(retry.OpAcquireJobs, err)
	if retry.Classify(ctx, retry.OpAcquireJobs, err) == retry.Permanent && s.table.Drain() == DrainNone {
		s.log.Error("AcquireJobs failed permanently; target error drain (0002 SPEC R171)",
			"target", s.url(), "err", err)
		s.table.SetTargetDraining(s.target.Key)
		s.placer.SetActive(s.target.Key, false)
		s.fatal(err)
	}
	return err
}

// placeLocked runs one placement with the Placer's lock held.
func (p *Placer) placeLocked() {
	pl := &placement{p: p, now: p.clock.Now(), limit: p.table.ServerLimit()}
	defer pl.storeWaiting()
	if pl.halt() {
		return
	}
	pl.count()
	pl.grant()
	pl.scaleDown()
}

// placement is one placement's counts: the table snapshot taken once,
// changed by every start of the placement so a later start never meets a
// need an earlier one met.
type placement struct {
	p     *Placer
	now   time.Time
	limit int
	// sumAll is the rows of all targets (0002 SPEC R114).
	sumAll int
	counts []*targetCount // config order
	halted bool
}

// targetCount is one target's counts in a placement.
type targetCount struct {
	t *placeTarget
	// all and live are the snapshot's (TargetSnapshot), raised by each start.
	all, live int
	desired   int
	// out: the target does not run or drains; it asks for nothing.
	out bool
	// blocked: the start backoff or the JIT rate limit holds the target's
	// starts for the whole placement (R61, R86, 0002 SPEC R186).
	blocked bool
	// failed: a start of the target failed or found no room in this
	// placement, so the placement does not try it again.
	failed bool
	// pending: a start of the target found an image being pulled in this
	// placement (0003 SPEC R319); the pull's end kicks the next placement.
	pending bool
}

// count takes the snapshot, computes each running target's desired count
// and brings its asks to the runners it lacks: job asks for the assigned
// jobs no live runner covers, min(max-runners, assigned) − live, and idle
// asks for the rest. Live runners cover assigned jobs first, since GitHub
// gives a job to any idle runner of the scale set.
func (pl *placement) count() {
	p := pl.p
	snap := p.table.Snapshot()
	for _, ts := range snap {
		pl.sumAll += ts.All
	}
	for _, t := range p.targets {
		ts := snap[t.key()]
		c := &targetCount{t: t, all: ts.All, live: ts.Live}
		pl.counts = append(pl.counts, c)
		if !t.active || p.table.TargetDraining(t.key()) {
			c.out = true
			t.jobAsks, t.idleAsks, t.held = nil, nil, false
			continue
		}
		tc := t.s.target
		c.desired = min(tc.MaxRunners, tc.Idle+t.assigned)
		need := max(0, c.desired-c.live)
		jobs := min(need, max(0, min(tc.MaxRunners, t.assigned)-c.live))
		t.jobAsks = p.ask(t.jobAsks, jobs)
		t.idleAsks = p.ask(t.idleAsks, need-jobs)
		if !t.logged || t.desired != c.desired {
			t.s.log.Info("desired runners", "target", t.s.url(), "desired", c.desired, "assignedJobs", t.assigned,
				"idle", tc.Idle, "maxRunners", tc.MaxRunners, "runners", c.live)
			t.desired, t.logged = c.desired, true
		}
		if t.asks() > 0 {
			c.blocked = pl.blocked(c)
		} else {
			t.held = false
		}
	}
}

// blocked reports whether c's target may start no runner in this placement:
// its start backoff or its JIT rate limit is on (R61, R86, 0002 SPEC R186).
// It logs only when the target becomes held, not on every placement.
func (pl *placement) blocked(c *targetCount) bool {
	t, s := c.t, c.t.s
	held, msg, until := false, "", time.Time{}
	if next := pl.p.table.StartStatus(t.key()).NextStartAt; pl.now.Before(next) {
		held, msg, until = true, "runner start deferred by start backoff", next
	} else if !s.limits.Allowed(retry.OpGenerateJIT) {
		held, msg, until = true, "runner start deferred by JIT rate limit", s.limits.At(retry.OpGenerateJIT)
	}
	if held && !t.held {
		s.log.Info(msg, "until", until)
	}
	t.held = held
	return held
}

// canStart reports whether a new runner of c's target may start now: it
// asks, nothing holds it, and it fits under its max-runners (R3).
func (c *targetCount) canStart() bool {
	return c.t.asks() > 0 && !c.out && !c.blocked && !c.failed && !c.pending &&
		c.all < c.t.s.target.MaxRunners
}

// waitsForSlot reports whether c's target cannot start now for a reason of
// its own that a free server slot does not end: its backoff, its rate
// limit, an image pull, or a start that failed in this placement. A target
// held only by its max-runners is not one: a row of its own must leave
// first, and that frees a slot.
func (c *targetCount) waitsForSlot() bool {
	return !c.out && (c.blocked || c.failed || c.pending)
}

// grant hands the free slots under the server limit to the asks (0002 SPEC
// R114, R187).
func (pl *placement) grant() {
	for pl.sumAll < pl.limit {
		c := pl.next()
		if c == nil {
			return
		}
		if pl.start(c) == startDone {
			if t := c.t; len(t.jobAsks) > 0 {
				t.jobAsks = t.jobAsks[1:]
			} else {
				t.idleAsks = t.idleAsks[1:]
			}
		}
		if pl.halted {
			return
		}
	}
}

// next returns the target whose ask gets the next slot: the oldest job ask
// among the targets that can start, else the oldest idle ask. An idle ask
// gets a slot only while more slots are free than the job asks of the
// targets that wait for something else (waitsForSlot), so a job that waits
// for its target's backoff or image pull keeps a slot. A target's job asks
// are granted before its idle asks.
func (pl *placement) next() *targetCount {
	var best *targetCount
	held := 0
	for _, c := range pl.counts {
		if !c.canStart() {
			if c.waitsForSlot() {
				held += len(c.t.jobAsks)
			}
			continue
		}
		if best == nil || before(c.t, best.t) {
			best = c
		}
	}
	if best != nil && len(best.t.jobAsks) == 0 && pl.limit-pl.sumAll <= held {
		return nil
	}
	return best
}

// before reports whether t's next ask goes before u's: a job ask before an
// idle ask, then the order they were made in.
func before(t, u *placeTarget) bool {
	tj, uj := len(t.jobAsks) > 0, len(u.jobAsks) > 0
	switch {
	case tj != uj:
		return tj
	case tj:
		return t.jobAsks[0] < u.jobAsks[0]
	default:
		return t.idleAsks[0] < u.idleAsks[0]
	}
}

func (pl *placement) storeWaiting() {
	for _, t := range pl.p.targets {
		n := 0
		if t.active {
			n = max(0, t.assigned-pl.p.table.Counts(t.key()).Live())
		}
		pl.p.setWaiting(t.key(), n)
	}
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

// usable is the re-check right before each start (0002 PLAN M14-3): false
// when the placement halted, or when c's target no longer runs or its drain
// mark is set, which takes c out.
func (pl *placement) usable(c *targetCount) bool {
	if pl.halt() {
		return false
	}
	if !c.t.active || pl.p.table.TargetDraining(c.t.key()) {
		c.out = true
	}
	return !c.out
}

// startResult is what one start of a placement led to.
type startResult int

const (
	startDone    startResult = iota // the runner is idle
	startFailed                     // counted (R61, R64), stopped without a failure, or no room
	startStopped                    // the re-check took the target out or halted
	startPending                    // an image is being pulled (0003 SPEC R319)
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
	case errors.Is(err, ErrImagePending):
		// Not a failure (0003 SPEC R319): no row was added and nothing was
		// registered.
		c.pending = true
		c.t.s.log.Info("runner start deferred; image being pulled")
		return startPending
	case errors.Is(err, errStartAborted) && pl.halt():
		return startStopped
	}
	c.failed = true
	return startFailed
}

// scaleDown removes up to live − desired runners that are not busy from
// each running target (0002 SPEC R185, R37).
func (pl *placement) scaleDown() {
	for _, c := range pl.counts {
		if c.out || c.live <= c.desired || !c.t.active || pl.p.table.TargetDraining(c.t.key()) {
			continue
		}
		if pl.halt() {
			return
		}
		if err := c.t.s.removeIdle(pl.p.ctx, c.live-c.desired); err != nil {
			c.t.s.log.Debug("placement scale-down stopped", "err", err)
		}
	}
}
