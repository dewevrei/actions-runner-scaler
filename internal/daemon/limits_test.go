package daemon

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dewevrei/actions-runner-scaler/internal/retry"
)

// rateLimited is a REST 429 answer with Retry-After: after.
func rateLimited(op retry.Op, after time.Duration) error {
	return opErr(op, &retry.RateLimitError{RetryAfter: after, Err: statusError(429)})
}

// TestInstallationWaitsSameID proves 0002 SPEC R266: get returns one
// pointer per installation ID, also when 8 goroutines ask at once.
func TestInstallationWaitsSameID(t *testing.T) {
	w, err := newCredentialWaits(newFakeClock())
	if err != nil {
		t.Fatal(err)
	}
	const n = 8
	got := make([][2]*retry.NextAllowed, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() { got[i] = [2]*retry.NextAllowed{w.get(installationKey(1)), w.get(installationKey(2))} })
	}
	wg.Wait()
	for i, g := range got {
		if g[0] != got[0][0] || g[1] != got[0][1] {
			t.Fatalf("goroutine %d got other waits for the same ID", i)
		}
	}
	if got[0][0] == got[0][1] {
		t.Fatal("two installation IDs share one wait")
	}
	if _, err := newCredentialWaits(nil); err == nil {
		t.Error("newCredentialWaits(nil) did not fail")
	}
}

// TestTargetLimitsRouting proves 0002 SPEC R265 and R266: the REST calls
// use the bound installation's waits, the other calls the target's own;
// before a bind the REST calls use the target's own.
func TestTargetLimitsRouting(t *testing.T) {
	clock := newFakeClock()
	w, err := newCredentialWaits(clock)
	if err != nil {
		t.Fatal(err)
	}
	own, err := retry.NewNextAllowed(clock)
	if err != nil {
		t.Fatal(err)
	}
	var inst atomic.Pointer[retry.NextAllowed]
	l := targetLimits{own: own, inst: &inst}

	l.Record(retry.OpListRepoRunners, rateLimited(retry.OpListRepoRunners, time.Minute))
	if own.Allowed(retry.OpFindRepoRunner) {
		t.Error("before a bind, a REST rate limit was not recorded in the target's own waits")
	}

	inst.Store(w.get(installationKey(1)))
	if !l.Allowed(retry.OpFindRepoRunner) {
		t.Error("after the bind, the target's own REST wait still delays it")
	}
	l.Record(retry.OpFindRepoRunner, rateLimited(retry.OpFindRepoRunner, time.Hour))
	if got, want := w.get(installationKey(1)).At(retry.OpListRepoRunners), clock.Now().Add(time.Hour); !got.Equal(want) {
		t.Errorf("installation wait = %v, want %v", got, want)
	}
	if !w.get(installationKey(2)).Allowed(retry.OpListRepoRunners) {
		t.Error("another installation's REST calls were delayed")
	}

	l.Record(retry.OpGenerateJIT, rateLimited(retry.OpGenerateJIT, time.Hour))
	if own.Allowed(retry.OpGenerateJIT) || !w.get(installationKey(1)).Allowed(retry.OpGenerateJIT) {
		t.Error("an Actions service rate limit was not recorded in the target's own waits only")
	}
}

// TestTargetLimitsConcurrentRebind runs targets' REST and Actions service
// waits while the bind moves between two installations, for -race (0002
// SPEC R266, R193).
func TestTargetLimitsConcurrentRebind(t *testing.T) {
	clock := newFakeClock()
	w, err := newCredentialWaits(clock)
	if err != nil {
		t.Fatal(err)
	}
	const targets = 4
	ls := make([]targetLimits, targets)
	ptrs := make([]*atomic.Pointer[retry.NextAllowed], targets)
	for i := range ls {
		own, err := retry.NewNextAllowed(clock)
		if err != nil {
			t.Fatal(err)
		}
		ptrs[i] = &atomic.Pointer[retry.NextAllowed]{}
		ls[i] = targetLimits{own: own, inst: ptrs[i]}
	}
	var wg sync.WaitGroup
	for i, l := range ls {
		wg.Go(func() {
			for j := range 200 {
				ptrs[i].Store(w.get(installationKey(int64(j % 2))))
			}
		})
		wg.Go(func() {
			for range 200 {
				for _, op := range []retry.Op{retry.OpListRepoRunners, retry.OpFindRepoRunner, retry.OpGenerateJIT} {
					l.Record(op, rateLimited(op, time.Second))
					_ = l.Allowed(op)
					_ = l.At(op)
				}
			}
		})
	}
	wg.Wait()
	for i, p := range ptrs {
		if got := p.Load(); got != w.get(installationKey(1)) {
			t.Errorf("target %d bound to %p, want the last stored ID 1's waits %p", i, got, w.get(installationKey(1)))
		}
	}
}
