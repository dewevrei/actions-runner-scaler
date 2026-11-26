package daemon

import (
	"context"
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/dewevrei/actions-runner-scaler/internal/docker"
	"github.com/dewevrei/actions-runner-scaler/internal/retry"
	"github.com/dewevrei/actions-runner-scaler/internal/status"
)

// indexOf returns the index of the first call starting with prefix, or -1.
func indexOf(log []string, prefix string) int {
	return slices.IndexFunc(log, func(l string) bool { return strings.HasPrefix(l, prefix) })
}

// The start order of PLAN M5-3, then running; a signal drain with nothing
// to wait for ends in exit 0.
func TestStartupOrder(t *testing.T) {
	h := newHarness(t)
	h.running()
	order := []string{
		"lock", "status.Listen", "config", "docker.Ping", "docker.List", "exposure",
		"docker.ImageExists sha256:runner", "docker.ImageExists moby/buildkit", "docker.MemTotal",
		"github.New", "github.Pool", "github.FindScaleSet pool", "github.GetOrCreateScaleSet pool",
		"github.SetSystemInfo", "jobs.New 127.0.0.1:8765", "jobs.Listen", "github.CreateSession 7 host",
		"session1.GetMessage",
	}
	log := h.calls.snapshot()
	last := -1
	for _, want := range order {
		i := indexOf(log, want)
		if i < 0 || i < last {
			t.Fatalf("call %q at %d after %d; calls:\n%s", want, i, last, strings.Join(log, "\n"))
		}
		last = i
	}
	h.gh.mu.Lock()
	info := h.gh.systemInfo
	h.gh.mu.Unlock()
	if info.ScaleSetID != 7 || info.System != "test" {
		t.Errorf("system info = %+v, want the scale set ID filled in", info)
	}
	if r := h.report(); len(r.Targets) != 1 || r.Targets[0].URL != "pool" {
		t.Errorf("status targets = %+v, want the pool as a target", r.Targets)
	}

	h.signal()
	if err := h.result(); err != nil {
		t.Fatalf("Run = %v, want nil after the signal drain", err)
	}
	if !h.unlocked {
		t.Error("lock not released")
	}
	if h.status.shutdowns != 1 || h.jobs.shutdowns != 1 {
		t.Errorf("shutdowns status %d jobs %d, want 1 each", h.status.shutdowns, h.jobs.shutdowns)
	}
}

func TestLockHeldExits1(t *testing.T) {
	h := newHarness(t)
	h.lockErr = ErrLocked
	h.start()
	if err := h.result(); !errors.Is(err, ErrLocked) {
		t.Fatalf("Run = %v, want ErrLocked", err)
	}
	if got := h.calls.snapshot(); !slices.Equal(got, []string{"lock"}) {
		t.Fatalf("calls = %v, want only the lock", got)
	}
}

func TestStatusListenFailureExits1(t *testing.T) {
	h := newHarness(t)
	h.listenErr = errors.New("address in use")
	h.start()
	if err := h.result(); err == nil {
		t.Fatal("Run = nil, want the listen error")
	}
	if got := h.calls.snapshot(); !slices.Equal(got, []string{"lock", "status.Listen"}) {
		t.Fatalf("calls = %v", got)
	}
	if !h.unlocked {
		t.Error("lock not released")
	}
}

// A configuration error still runs R16, then waits; status shows every
// error (R28).
func TestConfigErrorRunsLeftoverCleanupThenErrorWait(t *testing.T) {
	h := newHarness(t)
	h.cfgErr = errors.Join(errors.New("pools.max: must be at least 1"), errors.New("unknown key x"))
	h.docker.resources = []docker.Resource{{Kind: docker.KindRunner, ID: "c1", Name: "pool-0000000a", State: "exited"}}
	h.start()
	h.waitState(status.ErrorWait)
	log := h.calls.snapshot()
	if i, j := indexOf(log, "docker.List"), indexOf(log, "docker.RemoveByID runner c1"); i < 0 || j < i {
		t.Fatalf("R16 did not run: %v", log)
	}
	if indexOf(log, "exposure") >= 0 || indexOf(log, "github.") >= 0 {
		t.Fatalf("steps after the config check ran: %v", log)
	}
	r := h.report()
	if !strings.Contains(r.LastError, "pools.max") || !strings.Contains(r.LastError, "unknown key x") {
		t.Errorf("lastError = %q, want every config error", r.LastError)
	}
	h.assertNoCallsFor(10 * time.Minute)
	h.signal()
	if err := h.result(); err != nil {
		t.Fatalf("Run = %v, want nil on a signal in the error wait", err)
	}
}

// Permanent start-up failures enter the error wait; after it nothing calls
// GitHub or Docker, and a signal exits 0 (R46, R47, R48).
func TestStartupPermanentErrorsEnterErrorWait(t *testing.T) {
	tests := []struct {
		name  string
		setup func(h *harness)
		want  string
	}{
		{"docker ping 400", func(h *harness) {
			h.docker.pingErr = func(int) error { return opErr(retry.OpDockerPing, statusError(400)) }
		}, "docker ping"},
		{"exposed on 2375", func(h *harness) { h.exposure = docker.Exposed }, "2375"},
		{"runner image missing", func(h *harness) { h.docker.missing["sha256:runner"] = true }, "not present"},
		{"several scale sets", func(h *harness) {
			h.gh.setErr = func(int) error { return opErr(retry.OpGetOrCreateScaleSet, retry.ErrAmbiguousScaleSet) }
		}, "several"},
		{"auth refused", func(h *harness) {
			h.gh.setErr = func(int) error { return opErr(retry.OpGetOrCreateScaleSet, retry.ErrAuth) }
		}, "authentication"},
		{"github clients", func(h *harness) {
			h.newGitHubFn = func() (GitHub, error) { return nil, errors.New("bad key") }
		}, "bad key"},
		{"job check listen", func(h *harness) { h.jobs.listenErr = errors.New("address in use") }, "address in use"},
		{"session create 404", func(h *harness) {
			h.gh.createErr = func(int) error { return opErr(retry.OpSessionCreate, statusError(404)) }
		}, "404"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t)
			tt.setup(h)
			h.start()
			h.waitState(status.ErrorWait)
			if r := h.report(); !strings.Contains(r.LastError, tt.want) {
				t.Errorf("lastError = %q, want %q in it", r.LastError, tt.want)
			}
			if h.calls.count("session") != 0 {
				t.Errorf("a session call ran: %v", h.calls.snapshot())
			}
			// Reconciliation, watches and timers are stopped too.
			h.assertNoCallsFor(10 * time.Minute)
			h.signal()
			if err := h.result(); err != nil {
				t.Fatalf("Run = %v, want nil", err)
			}
		})
	}
}

func TestExposureUnknownGoesOn(t *testing.T) {
	h := newHarness(t)
	h.exposure = docker.ExposureUnknown
	h.running()
	if !strings.Contains(h.logs.String(), "could not tell") {
		t.Error("ExposureUnknown not logged")
	}
}

// Transient start-up failures back off 1, 2 s ... until success (R24); the
// status shows the error once while it repeats.
func TestStartupTransientBackoff(t *testing.T) {
	h := newHarness(t)
	h.gh.setErr = func(n int) error {
		if n <= 2 {
			return opErr(retry.OpGetOrCreateScaleSet, statusError(503))
		}
		return nil
	}
	h.start()
	waitFor(t, "first scale set call", func() bool { return h.calls.count("github.GetOrCreateScaleSet") == 1 })
	h.clock.advanceTo(t, testEpoch.Add(time.Second))
	waitFor(t, "second call", func() bool { return h.calls.count("github.GetOrCreateScaleSet") == 2 })
	h.clock.advanceTo(t, testEpoch.Add(3*time.Second))
	h.waitState(status.Running)
	if n := h.calls.count("github.GetOrCreateScaleSet"); n != 3 {
		t.Fatalf("scale set calls = %d, want 3", n)
	}
	if r := h.report(); !strings.Contains(r.LastError, "503") {
		t.Errorf("lastError = %q, want the transient error", r.LastError)
	}
}

// A rate limit waits for its time, at most 24 h (R52, R104).
func TestStartupRateLimitWaitsCappedAt24h(t *testing.T) {
	h := newHarness(t)
	h.gh.setErr = func(n int) error {
		if n == 1 {
			return opErr(retry.OpGetOrCreateScaleSet, &retry.RateLimitError{RetryAfter: 48 * time.Hour})
		}
		return nil
	}
	h.start()
	waitFor(t, "first call", func() bool { return h.calls.count("github.GetOrCreateScaleSet") == 1 })
	day := testEpoch.Add(retry.MaxRateLimitWait)
	waitFor(t, "the 24 h wait", func() bool { return h.clock.hasTimerAt(day) })
	h.clock.Advance(retry.MaxRateLimitWait - time.Nanosecond)
	if n := h.calls.count("github.GetOrCreateScaleSet"); n != 1 {
		t.Fatalf("called again before the rate limit ended (%d calls)", n)
	}
	h.clock.Advance(time.Nanosecond)
	h.waitState(status.Running)
}

// A signal during a start-up backoff exits 0 at once (R57).
func TestSignalDuringStartupBackoffExits0(t *testing.T) {
	h := newHarness(t)
	h.docker.pingErr = func(int) error { return opErr(retry.OpDockerPing, errors.New("connection refused")) }
	h.start()
	waitFor(t, "backoff", func() bool { return h.clock.hasTimerAt(testEpoch.Add(time.Second)) })
	if r := h.report(); r.State != status.Starting || !strings.Contains(r.LastError, "connection refused") {
		t.Errorf("report = %+v, want starting with the error", r)
	}
	h.signal()
	if err := h.result(); err != nil {
		t.Fatalf("Run = %v, want nil", err)
	}
	if n := h.calls.count("docker.Ping"); n != 1 {
		t.Errorf("pings = %d, want 1", n)
	}
}

// A signal while the first session is being retried exits 0 and stops what
// start-up began.
func TestSignalBeforeFirstSessionExits0(t *testing.T) {
	h := newHarness(t)
	h.gh.createErr = func(int) error { return opErr(retry.OpSessionCreate, statusError(409)) }
	h.start()
	waitFor(t, "session backoff", func() bool { return h.calls.count("github.CreateSession") == 1 })
	h.signal()
	if err := h.result(); err != nil {
		t.Fatalf("Run = %v, want nil", err)
	}
	if h.jobs.shutdowns != 1 {
		t.Errorf("job check server shutdowns = %d, want 1", h.jobs.shutdowns)
	}
	h.assertNoCallsFor(5 * time.Minute)
}

func TestMemoryWarning(t *testing.T) {
	h := newHarness(t)
	h.docker.memTotal = 1 << 30
	h.running()
	if !strings.Contains(h.logs.String(), "R29") {
		t.Error("memory warning not logged")
	}
}

func TestLockFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state", "lock")
	unlock, err := LockFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := LockFile(path); !errors.Is(err, ErrLocked) {
		t.Fatalf("second LockFile = %v, want ErrLocked", err)
	}
	unlock()
	unlock2, err := LockFile(path)
	if err != nil {
		t.Fatalf("LockFile after unlock = %v", err)
	}
	unlock2()
}

// A config error is shown in status even after Docker calls were retried
// with their own errors (R28).
func TestConfigErrorShownAfterRetriedPing(t *testing.T) {
	h := newHarness(t)
	h.cfgErr = errors.Join(errors.New("pools.max: must be at least 1"), errors.New("unknown key x"))
	h.docker.pingErr = func(n int) error {
		if n <= 2 {
			return opErr(retry.OpDockerPing, errors.New("connection refused"))
		}
		return nil
	}
	h.start()
	h.clock.advanceTo(t, testEpoch.Add(time.Second))
	h.clock.advanceTo(t, testEpoch.Add(3*time.Second))
	h.waitState(status.ErrorWait)
	r := h.report()
	if !strings.Contains(r.LastError, "pools.max") || !strings.Contains(r.LastError, "unknown key x") {
		t.Fatalf("lastError = %q, want every config error", r.LastError)
	}
	if strings.Contains(r.LastError, "connection refused") {
		t.Fatalf("lastError = %q, a stale Docker error", r.LastError)
	}
	h.signal()
	if err := h.result(); err != nil {
		t.Fatal(err)
	}
}

// A start-up call that stalls ends at its 30 s bound on the injected clock
// and counts as transient: it is retried after the backoff (R24).
func TestStalledStartupCallTimesOut(t *testing.T) {
	h := newHarness(t)
	h.docker.pingHook = func(ctx context.Context, n int) error {
		if n == 1 {
			<-ctx.Done()
			return opErr(retry.OpDockerPing, ctx.Err())
		}
		return nil
	}
	h.start()
	h.clock.advanceTo(t, testEpoch.Add(callTimeout))
	h.clock.advanceTo(t, testEpoch.Add(callTimeout+time.Second))
	h.waitState(status.Running)
	if n := h.calls.count("docker.Ping"); n != 2 {
		t.Fatalf("pings = %d, want 2", n)
	}
	if !strings.Contains(h.logs.String(), `"class":"transient"`) {
		t.Error("the stalled call was not classified as transient")
	}
}

// A permanent error listing leftovers is logged and start-up goes on (R16).
func TestLeftoverListPermanentErrorGoesOn(t *testing.T) {
	h := newHarness(t)
	h.docker.listErr = opErr(retry.OpContainerList, statusError(400))
	h.running()
	if !strings.Contains(h.logs.String(), "not removed (R16)") {
		t.Error("permanent list error not logged")
	}
}

// Watch and reconcile start before the first session (M5-3 steps 14, 15).
func TestReconcileStartsBeforeFirstSession(t *testing.T) {
	h := newHarness(t)
	var timerSet bool
	h.gh.createHook = func() { timerSet = h.clock.hasTimerAt(testEpoch.Add(30 * time.Second)) }
	h.running()
	if !timerSet {
		t.Fatal("reconciliation's first round was not scheduled before CreateSession")
	}
}

// The job check server dying while start-up retries the first session ends
// the retries at once and enters the error wait (R46).
func TestJobServerDeathDuringStartup(t *testing.T) {
	h := newHarness(t)
	h.gh.createErr = func(int) error { return opErr(retry.OpSessionCreate, statusError(409)) }
	h.start()
	waitFor(t, "session backoff", func() bool { return h.clock.hasTimerAt(testEpoch.Add(time.Second)) })
	h.jobs.serveErr <- errors.New("listener closed")
	h.waitState(status.ErrorWait)
	if r := h.report(); !strings.Contains(r.LastError, "job check server stopped") {
		t.Errorf("lastError = %q", r.LastError)
	}
	if n := h.calls.count("github.CreateSession"); n != 1 {
		t.Errorf("sessions tried = %d, want 1", n)
	}
	h.assertNoCallsFor(5 * time.Minute)
	h.signal()
	if err := h.result(); err != nil {
		t.Fatal(err)
	}
}
