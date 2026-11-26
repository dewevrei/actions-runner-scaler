package daemon

import (
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"github.com/dewevrei/actions-runner-scaler/internal/retry"
	"github.com/dewevrei/actions-runner-scaler/internal/scaler"
)

// installationWaits holds the R52 waits of the GitHub REST calls, one per
// installation ID, so the targets of one installation share them and the
// targets of another do not (0002 SPEC R266). Entries are never removed: a
// target that moves to another installation leaves its old entry to the
// targets still bound to it. It is safe for concurrent use and takes no
// other lock.
type installationWaits struct {
	clock retry.Clock

	mu   sync.Mutex
	byID map[int64]*retry.NextAllowed
}

// newInstallationWaits returns an empty installationWaits. It returns an
// error when clock is nil.
func newInstallationWaits(clock retry.Clock) (*installationWaits, error) {
	if clock == nil {
		return nil, errors.New("daemon: nil clock")
	}
	return &installationWaits{clock: clock, byID: make(map[int64]*retry.NextAllowed)}, nil
}

// get returns the waits of installation id, made on first use; every call
// with the same id returns the same pointer.
func (w *installationWaits) get(id int64) *retry.NextAllowed {
	w.mu.Lock()
	defer w.mu.Unlock()
	if n := w.byID[id]; n != nil {
		return n
	}
	n, err := retry.NewNextAllowed(w.clock)
	if err != nil {
		// newInstallationWaits checked the clock.
		panic("daemon: " + err.Error())
	}
	w.byID[id] = n
	return n
}

// restOp reports whether op is a GitHub REST call made with the
// installation token (0002 SPEC R241, R242, R266).
func restOp(op retry.Op) bool {
	return op == retry.OpListRepoRunners || op == retry.OpFindRepoRunner
}

// targetLimits is one target's scaler.RateLimits: the REST calls use the
// waits of the installation the target is bound to (0002 SPEC R266), every
// other call the target's own (R265). Before the first successful
// installation lookup no installation is bound and the REST calls use the
// target's own waits; no REST call is made then, since the REST client is
// made from the installation ID.
type targetLimits struct {
	own  *retry.NextAllowed
	inst *atomic.Pointer[retry.NextAllowed]
}

var _ scaler.RateLimits = targetLimits{}

// of returns the waits op is recorded in.
func (l targetLimits) of(op retry.Op) *retry.NextAllowed {
	if restOp(op) {
		if n := l.inst.Load(); n != nil {
			return n
		}
	}
	return l.own
}

func (l targetLimits) Record(op retry.Op, err error) bool { return l.of(op).Record(op, err) }
func (l targetLimits) Allowed(op retry.Op) bool           { return l.of(op).Allowed(op) }
func (l targetLimits) At(op retry.Op) time.Time           { return l.of(op).At(op) }
