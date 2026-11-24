package retry

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

func mustBackoff(t *testing.T, clock Clock, random func() float64, initial, maxDelay time.Duration) *Backoff {
	t.Helper()
	b, err := NewBackoff(clock, random, initial, maxDelay)
	if err != nil {
		t.Fatalf("NewBackoff: %v", err)
	}
	return b
}

func mustDefaultBackoff(t *testing.T, clock Clock, random func() float64) *Backoff {
	t.Helper()
	b, err := NewDefaultBackoff(clock, random)
	if err != nil {
		t.Fatalf("NewDefaultBackoff: %v", err)
	}
	return b
}

func TestNewBackoffRejectsInvalidPolicy(t *testing.T) {
	clock, random := newFakeClock(), fixedRandom(0)
	tests := []struct {
		name     string
		clock    Clock
		random   func() float64
		initial  time.Duration
		maxDelay time.Duration
	}{
		{"nil clock", nil, random, time.Second, time.Minute},
		{"nil random", clock, nil, time.Second, time.Minute},
		{"zero initial", clock, random, 0, time.Minute},
		{"negative initial", clock, random, -time.Second, time.Minute},
		{"max below initial", clock, random, time.Minute, time.Second},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if b, err := NewBackoff(tt.clock, tt.random, tt.initial, tt.maxDelay); err == nil {
				t.Fatalf("NewBackoff = %+v, want error", b)
			}
		})
	}
	if _, err := NewBackoff(clock, random, time.Second, time.Second); err != nil {
		t.Fatalf("NewBackoff with max equal to initial: %v", err)
	}
}

func TestBackoffNextDoublesToCap(t *testing.T) {
	b := mustDefaultBackoff(t, newFakeClock(), fixedRandom(0))
	want := []time.Duration{1, 2, 4, 8, 16, 32, 60, 60, 60}
	for i, w := range want {
		if got := b.Next(); got != w*time.Second {
			t.Fatalf("delay %d = %v, want %v", i, got, w*time.Second)
		}
	}
}

func TestBackoffReset(t *testing.T) {
	b := mustDefaultBackoff(t, newFakeClock(), fixedRandom(0))
	b.Next()
	b.Next()
	b.Reset()
	if got := b.Next(); got != time.Second {
		t.Fatalf("after Reset delay = %v, want 1s", got)
	}
}

func TestBackoffCustomPolicy(t *testing.T) {
	b := mustBackoff(t, newFakeClock(), fixedRandom(0), 10*time.Second, 5*time.Minute)
	want := []time.Duration{10, 20, 40, 80, 160, 300, 300}
	for i, w := range want {
		if got := b.Next(); got != w*time.Second {
			t.Fatalf("delay %d = %v, want %v", i, got, w*time.Second)
		}
	}
}

func TestBackoffJitterStaysInBounds(t *testing.T) {
	base := []time.Duration{1, 2, 4, 8, 16, 32, 60, 60, 60}
	for _, random := range []float64{0, 0.25, 0.5, 0.999} {
		t.Run(fmt.Sprint(random), func(t *testing.T) {
			b := mustDefaultBackoff(t, newFakeClock(), fixedRandom(random))
			for i, d := range base {
				d *= time.Second
				hi := min(d+d/5, DefaultMax)
				if got := b.Next(); got < d || got > hi {
					t.Fatalf("delay %d = %v, want within [%v, %v]", i, got, d, hi)
				}
			}
		})
	}
}

func TestBackoffJitterAddsUpToFifth(t *testing.T) {
	b := mustDefaultBackoff(t, newFakeClock(), fixedRandom(0.5))
	want := []time.Duration{1100, 2200, 4400}
	for i, w := range want {
		if got := b.Next(); got != w*time.Millisecond {
			t.Fatalf("delay %d = %v, want %v", i, got, w*time.Millisecond)
		}
	}
}

func TestBackoffCustomPolicyNeverBelowStart(t *testing.T) {
	b := mustBackoff(t, newFakeClock(), fixedRandom(0.999), 10*time.Second, 5*time.Minute)
	if got := b.Next(); got < 10*time.Second || got > 12*time.Second {
		t.Fatalf("first delay = %v, want within [10s, 12s]", got)
	}
}

// Delays near the largest Duration must not overflow into a negative wait.
func TestBackoffNextNearMaxDuration(t *testing.T) {
	const maxDuration = time.Duration(1<<63 - 1)
	b := mustBackoff(t, newFakeClock(), fixedRandom(0.999), maxDuration/3, maxDuration)
	for i := range 4 {
		if got := b.Next(); got < maxDuration/3 || got > maxDuration {
			t.Fatalf("delay %d = %v, want within [%v, %v]", i, got, maxDuration/3, maxDuration)
		}
	}
}

func TestBackoffWait(t *testing.T) {
	now := newFakeClock().Now()
	tests := []struct {
		name string
		err  error
		want time.Duration
	}{
		{"plain error uses backoff", errors.New("boom"), time.Second},
		{"retry after longer than backoff", &RateLimitError{RetryAfter: 90 * time.Second}, 90 * time.Second},
		{"no hint waits at least 60s", &RateLimitError{}, 60 * time.Second},
		{"bare 429 waits at least 60s", status(429), 60 * time.Second},
		{"wrapped rate limit", &OpError{Op: OpGenerateJIT, Err: &RateLimitError{RetryAfter: 2 * time.Minute}}, 2 * time.Minute},
		{"reset time", &RateLimitError{Reset: now.Add(75 * time.Second)}, 75 * time.Second},
		{"later of retry after and reset", &RateLimitError{RetryAfter: 10 * time.Second, Reset: now.Add(30 * time.Second)}, 30 * time.Second},
		{"reset in the past falls back to backoff", &RateLimitError{Reset: now.Add(-time.Minute)}, time.Second},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clock := newFakeClock()
			b := mustDefaultBackoff(t, clock, fixedRandom(0))
			if err := b.Wait(context.Background(), tt.err); err != nil {
				t.Fatalf("Wait: %v", err)
			}
			if got := clock.lastWait(); got != tt.want {
				t.Fatalf("waited %v, want %v", got, tt.want)
			}
		})
	}
}

func TestBackoffWaitKeepsLongerBackoff(t *testing.T) {
	clock := newFakeClock()
	b := mustDefaultBackoff(t, clock, fixedRandom(0))
	for range 6 {
		b.Next()
	}
	if err := b.Wait(context.Background(), &RateLimitError{RetryAfter: 5 * time.Second}); err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if got := clock.lastWait(); got != 60*time.Second {
		t.Fatalf("waited %v, want backoff 60s", got)
	}
}

func TestBackoffWaitReturnsOnCancel(t *testing.T) {
	clock := newFakeClock()
	clock.block = true
	b := mustDefaultBackoff(t, clock, fixedRandom(0))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- b.Wait(ctx, &RateLimitError{}) }()
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Wait = %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Wait did not return after cancel")
	}
}
