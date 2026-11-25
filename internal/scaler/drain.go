package scaler

import (
	"context"
	"strconv"
	"time"
)

// drainPollInterval is how often a drain checks whether it is complete and
// asks scale-down to remove idle runners when no message comes. Not from
// SPEC: chosen so a drain ends within seconds of its last busy runner, and
// so that a RemoveRunner failing with a transient error is retried at most
// this often (R93 asks only for a later retry).
const drainPollInterval = 5 * time.Second

// DrainResult says how Drain ended.
type DrainResult int

// Drain results.
const (
	// DrainCompleted means every runner the drain waits for is gone.
	DrainCompleted DrainResult = iota + 1
	// DrainTimedOut means shutdown.drain-timeout passed first.
	DrainTimedOut
	// DrainInterrupted means the context ended first: a second signal
	// during a signal drain, or a signal during an error drain (R21).
	DrainInterrupted
)

func (r DrainResult) String() string {
	switch r {
	case DrainCompleted:
		return "completed"
	case DrainTimedOut:
		return "timed out"
	case DrainInterrupted:
		return "interrupted"
	default:
		return "DrainResult(" + strconv.Itoa(int(r)) + ")"
	}
}

// Drain runs a drain of kind, DrainSignal or DrainError, until it is
// complete, timeout passes on the Clock, or ctx ends, and reports which came
// first. It sets the table's drain with SetDraining, which keeps a drain
// already begun, so a permanent error during a signal drain stays a signal
// drain (R100); the kind given still decides when the drain is complete:
//
//   - DrainSignal: no busy, done or starting runner is left (R20, R92). A
//     runner that was starting when the signal came is waited for, and
//     scale-down removes it once it is idle.
//   - DrainError: no busy or done runner is left (R55).
//
// Scale-down passes (RemoveIdle) remove the idle runners also when no
// message comes, for example after Run returned (R37, R53, R91): a
// JobStillRunningError marks the runner busy and the drain waits for it,
// and during an error drain any other failure removes only the runner
// container. The first pass starts at once and another every
// drainPollInterval, at most one at a time, and the drain is complete only
// after a pass has ended, so an idle runner that in fact took a job is
// counted as busy before the drain can end. When the drain looks complete
// but idle runners are still listed (their RemoveRunner failed transiently
// or was rate limited), one more pass runs at once before the drain ends;
// what that pass leaves is not waited for, and finalize removes it.
//
// Each pass runs in its own goroutine, counts as work in flight for
// WaitInFlight from before it starts, and gets ctx, so Drain returns at once
// when ctx ends even while a Scale holds the scale-down lock. Scale keeps
// removing idle runners after each message too, and nothing starts a runner
// while draining.
//
// Drain never finalizes; the caller does that next (M5-8).
func (s *Scaler) Drain(ctx context.Context, kind DrainKind, timeout time.Duration) DrainResult {
	s.table.SetDraining(kind)
	log := s.log.With("drain", kind)
	log.Info("drain started", "timeout", timeout)
	timedOut, timeoutTimer := s.timer(timeout)
	defer timeoutTimer.Stop()
	passDone := make(chan struct{}, 1)
	var (
		poll      <-chan struct{}
		pollTimer Timer
		running   bool
		needPass  = true
		passes    int
		// lastPass is set while the drain looks complete and one more pass
		// retries the idle runners still listed (RemoveRunner failed
		// transiently or was rate limited).
		lastPass bool
	)
	defer func() {
		if pollTimer != nil {
			pollTimer.Stop()
		}
	}()
	for {
		if ctx.Err() != nil {
			log.Info("drain interrupted")
			return DrainInterrupted
		}
		if !running && passes > 0 {
			switch {
			case !drained(kind, s.table.Counts()):
				lastPass = false
			case !lastPass && len(s.table.RemovalCandidates()) > 0:
				lastPass, needPass = true, true
			default:
				log.Info("drain completed")
				return DrainCompleted
			}
		}
		if !running && needPass {
			running, needPass = true, false
			s.removeIdleInBackground(ctx, passDone)
		}
		if poll == nil {
			poll, pollTimer = s.timer(drainPollInterval)
		}
		select {
		case <-ctx.Done():
			log.Info("drain interrupted")
			return DrainInterrupted
		case <-timedOut:
			c := s.table.Counts()
			log.Warn("drain timed out", "starting", c.Starting, "busy", c.Busy, "done", c.Done)
			return DrainTimedOut
		case <-passDone:
			running = false
			passes++
		case <-poll:
			poll, pollTimer = nil, nil
			needPass = true
		}
	}
}

// timer returns a channel closed once d has passed on the Clock, and the
// Timer to stop it with, so a drain that ends early leaves no timer behind.
func (s *Scaler) timer(d time.Duration) (<-chan struct{}, Timer) {
	fired := make(chan struct{})
	t := s.clock.AfterFunc(d, func() { close(fired) })
	return fired, t
}

// drained reports whether a drain of kind has nothing left to wait for.
func drained(kind DrainKind, c Counts) bool {
	left := c.Busy + c.Done
	if kind == DrainSignal {
		left += c.Starting
	}
	return left == 0
}

// removeIdleInBackground runs one scale-down pass in a goroutine and signals
// done when it ends. The pass is counted before the goroutine starts, so a
// WaitInFlight that follows the drain always waits for it. done must have
// room for the signal, so the goroutine never blocks on it.
func (s *Scaler) removeIdleInBackground(ctx context.Context, done chan<- struct{}) {
	s.work.begin()
	go func() {
		defer s.work.end()
		if err := s.removeIdleCounted(ctx); err != nil && ctx.Err() == nil {
			s.log.Debug("drain scale-down stopped", "err", err)
		}
		done <- struct{}{}
	}()
}
