package retry

import (
	"errors"
	"sync"
	"time"
)

// limitKind groups calls that share one rate-limit wait (R52).
type limitKind int

const (
	kindJIT limitKind = iota + 1
	kindAcquireJobs
	kindSession
	kindRemoveRunner
	kindScaleSet
	kindInstallation
	kindRepoRunners
)

// kindOf maps an operation to the wait it shares.
func kindOf(op Op) (limitKind, bool) {
	switch op {
	case OpGenerateJIT:
		return kindJIT, true
	case OpAcquireJobs:
		return kindAcquireJobs, true
	case OpSessionCreate, OpSessionRefresh, OpGetMessage, OpDeleteMessage:
		return kindSession, true
	case OpRemoveRunner:
		return kindRemoveRunner, true
	// One wait for all scale set setup calls: they run in one start-up
	// sequence, so a limit on one must also hold back the next (R52).
	case OpGetRunnerByName, OpGetRunnerScaleSet, OpGetOrCreateScaleSet, OpSetSystemInfo:
		return kindScaleSet, true
	// The installation lookup is the one App JWT wait (0002 SPEC R189).
	case OpGetInstallation:
		return kindInstallation, true
	// The repository runner list and name lookup (0002 SPEC R241, R242).
	case OpListRepoRunners, OpFindRepoRunner:
		return kindRepoRunners, true
	}
	return 0, false
}

// NextAllowed remembers, per kind of call, the earliest time the call may be
// made again after a rate limit. Separate kinds keep one limit from stopping
// unrelated calls. The daemon keeps one per target for the Actions service
// calls (0002 SPEC R265), one per installation for the REST calls (0002 SPEC
// R266, 0002 PLAN M21) and one shared by all targets for the App JWT
// installation lookup (0002 SPEC R189). It is safe for concurrent use.
type NextAllowed struct {
	clock Clock

	mu   sync.Mutex
	next map[limitKind]time.Time
}

// NewNextAllowed returns an empty NextAllowed. It returns an error when
// clock is nil.
func NewNextAllowed(clock Clock) (*NextAllowed, error) {
	if clock == nil {
		return nil, errors.New("retry: nil clock")
	}
	return &NextAllowed{clock: clock, next: make(map[limitKind]time.Time)}, nil
}

// Record pushes back the next allowed time of op's kind when err is a
// RateLimitError or a 429. It reports whether it recorded anything;
// untracked operations and other errors are ignored.
func (n *NextAllowed) Record(op Op, err error) bool {
	kind, ok := kindOf(op)
	if !ok {
		return false
	}
	rl, ok := rateLimitOf(err)
	if !ok {
		return false
	}
	now := n.clock.Now()
	at := now.Add(rateLimitWait(rl, now))

	n.mu.Lock()
	defer n.mu.Unlock()
	if at.After(n.next[kind]) {
		n.next[kind] = at
	}
	return true
}

// At returns the earliest time op may be called again. The zero time means
// no limit was recorded.
func (n *NextAllowed) At(op Op) time.Time {
	kind, ok := kindOf(op)
	if !ok {
		return time.Time{}
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.next[kind]
}

// Allowed reports whether op may be called now.
func (n *NextAllowed) Allowed(op Op) bool {
	return !n.clock.Now().Before(n.At(op))
}
