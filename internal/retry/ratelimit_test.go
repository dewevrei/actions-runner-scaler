package retry

import (
	"errors"
	"math"
	"sync"
	"testing"
	"time"
)

func mustNextAllowed(t *testing.T, clock Clock) *NextAllowed {
	t.Helper()
	n, err := NewNextAllowed(clock)
	if err != nil {
		t.Fatalf("NewNextAllowed: %v", err)
	}
	return n
}

func TestNewNextAllowedRejectsNilClock(t *testing.T) {
	if n, err := NewNextAllowed(nil); err == nil {
		t.Fatalf("NewNextAllowed(nil) = %+v, want error", n)
	}
}

func TestNextAllowedRecord(t *testing.T) {
	tests := []struct {
		name string
		op   Op
		err  error
		ok   bool
		wait time.Duration
	}{
		{"retry after", OpGenerateJIT, &RateLimitError{RetryAfter: 90 * time.Second}, true, 90 * time.Second},
		{"no hint", OpGenerateJIT, &RateLimitError{}, true, 60 * time.Second},
		{"wrapped", OpAcquireJobs, &OpError{Op: OpAcquireJobs, Err: &RateLimitError{RetryAfter: time.Minute}}, true, time.Minute},
		{"bare 429", OpRemoveRunner, status(429), true, 60 * time.Second},
		{"not a rate limit", OpRemoveRunner, errors.New("boom"), false, 0},
		{"plain 403", OpListOrgRunners, status(403), false, 0},
		{"docker is not tracked", OpContainerRemove, &RateLimitError{}, false, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clock := newFakeClock()
			n := mustNextAllowed(t, clock)
			start := clock.Now()
			if got := n.Record(tt.op, tt.err); got != tt.ok {
				t.Fatalf("Record = %v, want %v", got, tt.ok)
			}
			if !tt.ok {
				if at := n.At(tt.op); !at.IsZero() {
					t.Fatalf("At = %v, want zero", at)
				}
				if !n.Allowed(tt.op) {
					t.Fatal("Allowed = false, want true")
				}
				return
			}
			if got := n.At(tt.op).Sub(start); got != tt.wait {
				t.Fatalf("next allowed after %v, want %v", got, tt.wait)
			}
			clock.advance(tt.wait - time.Nanosecond)
			if n.Allowed(tt.op) {
				t.Fatal("allowed before the wait ended")
			}
			clock.advance(time.Nanosecond)
			if !n.Allowed(tt.op) {
				t.Fatal("not allowed after the wait ended")
			}
		})
	}
}

// TestNextAllowedCapsWait checks that neither Retry-After nor the reset
// time can hold a call back for more than MaxRateLimitWait.
func TestNextAllowedCapsWait(t *testing.T) {
	start := newFakeClock().Now()
	tests := []struct {
		name string
		rl   *RateLimitError
		want time.Duration
	}{
		{"retry after at the cap", &RateLimitError{RetryAfter: MaxRateLimitWait}, MaxRateLimitWait},
		{"retry after past the cap", &RateLimitError{RetryAfter: MaxRateLimitWait + time.Second}, MaxRateLimitWait},
		{"largest retry after", &RateLimitError{RetryAfter: time.Duration(math.MaxInt64)}, MaxRateLimitWait},
		{"reset just under the cap", &RateLimitError{Reset: start.Add(MaxRateLimitWait - time.Second)}, MaxRateLimitWait - time.Second},
		{"reset past the cap", &RateLimitError{Reset: start.Add(MaxRateLimitWait + time.Hour)}, MaxRateLimitWait},
		// Further than a time.Duration reaches: Sub saturates.
		{"reset in year 9999", &RateLimitError{Reset: time.Date(9999, 1, 1, 0, 0, 0, 0, time.UTC)}, MaxRateLimitWait},
		{"both past the cap", &RateLimitError{RetryAfter: 72 * time.Hour, Reset: start.Add(48 * time.Hour)}, MaxRateLimitWait},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			n := mustNextAllowed(t, newFakeClock())
			if !n.Record(OpGenerateJIT, tt.rl) {
				t.Fatal("Record = false")
			}
			if got := n.At(OpGenerateJIT).Sub(start); got != tt.want {
				t.Fatalf("next allowed after %v, want %v", got, tt.want)
			}
		})
	}
}

func TestNextAllowedKindsAreIndependent(t *testing.T) {
	groups := [][]Op{
		{OpGenerateJIT},
		{OpAcquireJobs},
		{OpSessionCreate, OpGetMessage, OpDeleteMessage, OpSessionRefresh},
		{OpRemoveRunner},
		{OpGetRunnerByName, OpGetRunnerScaleSet, OpGetOrCreateScaleSet, OpSetSystemInfo, OpGetRunnerGroup},
		{OpGetInstallation},
		{OpListOrgRunners, OpFindOrgRunner},
	}
	for i, group := range groups {
		t.Run(string(group[0]), func(t *testing.T) {
			n := mustNextAllowed(t, newFakeClock())
			n.Record(group[0], &RateLimitError{})
			for j, other := range groups {
				for _, op := range other {
					if got, want := n.Allowed(op), i != j; got != want {
						t.Errorf("Allowed(%q) = %v, want %v", op, got, want)
					}
				}
			}
		})
	}
}

func TestNextAllowedKeepsLaterTime(t *testing.T) {
	clock := newFakeClock()
	n := mustNextAllowed(t, clock)
	n.Record(OpRemoveRunner, &RateLimitError{RetryAfter: 5 * time.Minute})
	want := n.At(OpRemoveRunner)
	n.Record(OpRemoveRunner, &RateLimitError{RetryAfter: time.Second})
	if got := n.At(OpRemoveRunner); !got.Equal(want) {
		t.Fatalf("At = %v, want %v", got, want)
	}
}

func TestNextAllowedConcurrent(t *testing.T) {
	n := mustNextAllowed(t, newFakeClock())
	ops := []Op{OpGenerateJIT, OpAcquireJobs, OpRemoveRunner, OpListOrgRunners}
	var wg sync.WaitGroup
	for i := range 50 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			op := ops[i%len(ops)]
			n.Record(op, &RateLimitError{RetryAfter: time.Duration(i) * time.Second})
			n.Allowed(op)
		}()
	}
	wg.Wait()
}
