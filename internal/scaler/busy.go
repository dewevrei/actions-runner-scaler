package scaler

import (
	"context"
	"sync"
	"time"
)

// busyTimeout is R81's wait: a runner without an allow verdict this long
// after it became busy is denied and removed.
const busyTimeout = 180 * time.Second

// BusyTimers holds one job check timer per busy runner (R81). Its arm is the
// Table's job check hook, which JobJudge.bind installs, so the timer starts
// at the first of the busy sources:
// the first hook request with a matching token (a verdict poll or an event
// report, 0002 SPEC R138), JobStarted, a
// JobStillRunningError during a drain (R53, R91), and what reconciliation
// discovers (R83, R103). When a timer fires and the runner has no allow
// verdict, the runner goes through the JobJudge's deny path; an allow stops
// the timer. The timers run during a drain too, so a job whose JobStarted
// never came is still removed (R92).
//
// Finalize can begin right after the Table decided to call the hook, so arm
// and the firing timer both check the finalize mark and do nothing once
// it is set. Stop stops every timer; Wait waits for the callbacks.
//
// It is safe for concurrent use.
type BusyTimers struct {
	j *JobJudge
	// work counts the pending timers and the callbacks in progress.
	work inFlight

	mu      sync.Mutex
	stopped bool
	timers  map[string]Timer
}

// arm starts runner name's timer, to fire busyTimeout after at, the time it
// became busy. It is a BusyFunc: it does not block. It does nothing once
// finalize has begun or Stop was called, or when the runner has a timer.
func (b *BusyTimers) arm(name string, at time.Time) {
	j := b.j
	if j.s == nil {
		return
	}
	log := j.log.With("runner", name)
	if j.s.table.Finalizing() {
		log.Info("finalize began; no job check timer")
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.stopped {
		log.Info("job check timers stopped; no timer")
		return
	}
	if _, ok := b.timers[name]; ok {
		return
	}
	wait := max(at.Add(busyTimeout).Sub(j.s.clock.Now()), 0)
	b.work.begin()
	// The callback runs in its own goroutine and takes b.mu, so it cannot
	// look for its entry before the entry is stored.
	b.timers[name] = j.s.clock.AfterFunc(wait, func() { b.fire(name) })
	log.Debug("job check timer armed", "busyAt", at, "in", wait)
}

// fire is the timer callback. It removes its own entry first; if cancel or
// Stop removed it already, it does nothing. Either way exactly one of them
// ends the work count.
func (b *BusyTimers) fire(name string) {
	b.mu.Lock()
	_, ok := b.timers[name]
	delete(b.timers, name)
	b.mu.Unlock()
	defer b.work.end()
	if !ok {
		return
	}
	j := b.j
	log := j.log.With("runner", name)
	if j.s.table.Finalizing() {
		log.Info("finalize began; job check timer did nothing")
		return
	}
	if info, ok := j.s.table.Get(name); !ok || info.Verdict == VerdictAllow {
		return
	}
	// BeginExitUnlessAllowed checks the verdict again under the table's
	// lock, so an allow that came after the check above keeps the runner.
	j.deny(log, name, "no allow verdict "+busyTimeout.String()+" after the runner became busy (R81)",
		j.s.table.BeginExitUnlessAllowed)
}

// cancel stops runner name's timer, if any.
func (b *BusyTimers) cancel(name string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	t, ok := b.timers[name]
	if !ok {
		return
	}
	delete(b.timers, name)
	if t.Stop() {
		b.work.end()
	}
}

// Stop stops every pending timer and refuses new ones. A callback already
// running is not interrupted; Wait waits for it.
func (b *BusyTimers) Stop() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.stopped = true
	for name, t := range b.timers {
		delete(b.timers, name)
		if t.Stop() {
			b.work.end()
		}
	}
}

// Wait waits until no timer is pending and no callback runs, or ctx ends.
// Before Stop a pending timer keeps it waiting up to busyTimeout.
func (b *BusyTimers) Wait(ctx context.Context) error {
	return b.work.wait(ctx)
}
