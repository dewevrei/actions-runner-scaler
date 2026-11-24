package scaler

import "time"

// Clock is the scaler's time source. It is injected so tests can control
// every wait and timer; the scaler never reads the time package directly.
//
// A Clock also satisfies retry.Clock, so the same value paces retries and
// rate-limit waits.
type Clock interface {
	// Now returns the current time.
	Now() time.Time
	// After returns a channel that receives the time once d has passed.
	After(d time.Duration) <-chan time.Time
	// AfterFunc calls f in its own goroutine once d has passed, unless the
	// returned Timer is stopped first.
	AfterFunc(d time.Duration, f func()) Timer
}

// Timer is a pending AfterFunc call.
type Timer interface {
	// Stop prevents the call if it has not started. It reports whether it
	// did; false means the call already ran, is running, or was stopped.
	Stop() bool
}

type systemClock struct{}

func (systemClock) Now() time.Time                         { return time.Now() }
func (systemClock) After(d time.Duration) <-chan time.Time { return time.After(d) }
func (systemClock) AfterFunc(d time.Duration, f func()) Timer {
	return time.AfterFunc(d, f)
}

// SystemClock returns a Clock backed by the time package.
func SystemClock() Clock { return systemClock{} }
