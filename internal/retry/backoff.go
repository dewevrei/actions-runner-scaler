package retry

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// Defaults from R24 and R52.
const (
	DefaultInitial = time.Second
	DefaultMax     = 60 * time.Second
	// DefaultRateLimitWait is the wait after a rate limit that gives neither
	// Retry-After nor a reset time.
	DefaultRateLimitWait = 60 * time.Second
	// MaxRateLimitWait caps the wait a rate limit imposes, whether it comes
	// from Retry-After or from the reset time. GitHub's primary limits are
	// counted per hour, so a longer hint can only come from a broken header;
	// without the cap such a value would stop the call for days or years.
	// It is the one cap for every call path (library and REST).
	MaxRateLimitWait = 24 * time.Hour
)

// jitterFraction is the largest share of a delay that jitter adds. Jitter
// only lengthens delays so no wait is shorter than the stated start (R24,
// R61); the result is still cut at the maximum.
const jitterFraction = 0.2

// Clock is the time source for waits.
type Clock interface {
	Now() time.Time
	After(d time.Duration) <-chan time.Time
}

type systemClock struct{}

func (systemClock) Now() time.Time                         { return time.Now() }
func (systemClock) After(d time.Duration) <-chan time.Time { return time.After(d) }

// SystemClock returns a Clock backed by the time package.
func SystemClock() Clock { return systemClock{} }

// Backoff produces exponentially growing delays with jitter. It is not safe
// for concurrent use; each retry loop owns one. Create it with NewBackoff or
// NewDefaultBackoff; the zero value is not usable.
type Backoff struct {
	// initial is the first delay; each failure doubles it up to max.
	initial time.Duration
	max     time.Duration

	clock    Clock
	random   func() float64
	failures int
}

// NewBackoff returns a Backoff whose delays start at initial and double up
// to maxDelay, such as 10s and 5m for runner start failures (R61). random
// must return values in [0, 1), like math/rand/v2.Float64. It returns an
// error when clock or random is nil, initial is not positive, or maxDelay is
// less than initial.
func NewBackoff(clock Clock, random func() float64, initial, maxDelay time.Duration) (*Backoff, error) {
	switch {
	case clock == nil:
		return nil, errors.New("retry: nil clock")
	case random == nil:
		return nil, errors.New("retry: nil random")
	case initial <= 0:
		return nil, fmt.Errorf("retry: initial delay %v must be positive", initial)
	case maxDelay < initial:
		return nil, fmt.Errorf("retry: max delay %v is less than initial delay %v", maxDelay, initial)
	}
	return &Backoff{initial: initial, max: maxDelay, clock: clock, random: random}, nil
}

// NewDefaultBackoff returns a Backoff with DefaultInitial and DefaultMax,
// the start-up policy (R24).
func NewDefaultBackoff(clock Clock, random func() float64) (*Backoff, error) {
	return NewBackoff(clock, random, DefaultInitial, DefaultMax)
}

// Next returns the delay for the next retry and counts one more failure.
func (b *Backoff) Next() time.Duration {
	// Adding at most max-d keeps d within [initial, max], so neither the
	// doubling nor the jitter can overflow into a delay of 0 or less.
	d := b.initial
	for i := 0; i < b.failures && d < b.max; i++ {
		d += min(d, b.max-d)
	}
	if d < b.max {
		b.failures++
	}
	jitter := time.Duration(float64(d) * jitterFraction * b.random())
	return d + min(jitter, b.max-d)
}

// Reset starts the sequence over after a success.
func (b *Backoff) Reset() { b.failures = 0 }

// Wait sleeps for the next delay, or for the rate limit's wait when err is a
// RateLimitError or a 429 and that is longer (R52). It returns ctx.Err() if
// ctx ends first.
func (b *Backoff) Wait(ctx context.Context, err error) error {
	d := b.Next()
	if rl, ok := rateLimitOf(err); ok {
		d = max(d, rateLimitWait(rl, b.clock.Now()))
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-b.clock.After(d):
		return nil
	}
}

// rateLimitWait is how long rl forbids the same call, measured from now:
// the later of RetryAfter and Reset, at most MaxRateLimitWait (R52).
// Time.Sub saturates instead of overflowing, so a far Reset is still capped.
func rateLimitWait(rl *RateLimitError, now time.Time) time.Duration {
	if rl.RetryAfter <= 0 && rl.Reset.IsZero() {
		return DefaultRateLimitWait
	}
	return min(max(rl.RetryAfter, rl.Reset.Sub(now), 0), MaxRateLimitWait)
}
