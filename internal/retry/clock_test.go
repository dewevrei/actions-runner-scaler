package retry

import (
	"sync"
	"time"
)

// fakeClock records requested waits. When block is set, After never fires.
type fakeClock struct {
	mu    sync.Mutex
	now   time.Time
	waits []time.Duration
	block bool
}

func newFakeClock() *fakeClock {
	return &fakeClock{now: time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) After(d time.Duration) <-chan time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.waits = append(c.waits, d)
	ch := make(chan time.Time, 1)
	if !c.block {
		c.now = c.now.Add(d)
		ch <- c.now
	}
	return ch
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func (c *fakeClock) lastWait() time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.waits) == 0 {
		return 0
	}
	return c.waits[len(c.waits)-1]
}

func fixedRandom(v float64) func() float64 { return func() float64 { return v } }
