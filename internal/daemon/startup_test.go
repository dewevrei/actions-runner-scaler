package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/dewevrei/actions-runner-scaler/internal/config"
	"github.com/dewevrei/actions-runner-scaler/internal/docker"
	"github.com/dewevrei/actions-runner-scaler/internal/github"
	"github.com/dewevrei/actions-runner-scaler/internal/retry"
	"github.com/dewevrei/actions-runner-scaler/internal/status"
)

// indexOf returns the index of the first call starting with prefix, or -1.
func indexOf(log []string, prefix string) int {
	return slices.IndexFunc(log, func(l string) bool { return strings.HasPrefix(l, prefix) })
}

// The start order of PLAN M5-3 with the job check server listening before
// the target's first GitHub call (0002 SPEC R176, Design 시작 순서), then
// running; a signal drain with nothing to wait for ends in exit 0.
func TestStartupOrder(t *testing.T) {
	h := newHarness(t)
	h.running()
	order := []string{
		"lock", "status.Listen", "config", "docker.Ping", "docker.List", "exposure",
		"docker.ImageExists sha256:runner", "docker.ImageExists moby/buildkit",
		"docker.Info", "jobs.New 127.0.0.1:8765", "jobs.Listen",
		"github.New", "github.Installation owner", "github.Target owner 99", "github.RunnerGroupID ",
		"github.FindScaleSet 1 pool", "github.GetOrCreateScaleSet 1 pool", "github.SetSystemInfo", "github.CreateSession 7 host",
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
	if r := h.report(); len(r.Targets) != 1 || r.Targets[0].URL != testTargetURL {
		t.Errorf("status targets = %+v, want the target %s", r.Targets, testTargetURL)
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
	h.cfgErr = errors.Join(errors.New("targets[0].max-runners: must be at least 1"), errors.New("unknown key x"))
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
	if !strings.Contains(r.LastError, "targets[0].max-runners") || !strings.Contains(r.LastError, "unknown key x") {
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
		{"github clients", func(h *harness) {
			h.newGitHubFn = func() (GitHub, error) { return nil, errors.New("bad key") }
		}, "bad key"},
		{"job check listen", func(h *harness) { h.jobs.listenErr = errors.New("address in use") }, "address in use"},
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

// assertTargetErrorWait checks that target tr is in the target error wait
// with want in its last error and the server's, while the scaler counts
// its start-up as finished (0002 SPEC R172, R195), and that the target makes
// no session and no GitHub call before its first retry (R174).
func (h *harness) assertTargetErrorWait(tr *targetRun, want string) {
	h.t.Helper()
	h.waitTargetState(tr, targetErrorWait)
	h.waitState(status.Running)
	if last, _ := tr.rec.Last(); !strings.Contains(last, want) {
		h.t.Errorf("target lastError = %q, want %q in it", last, want)
	}
	if r := h.report(); !strings.Contains(r.LastError, want) {
		h.t.Errorf("lastError = %q, want %q in it", r.LastError, want)
	}
	if tr.currentSession() != nil || h.calls.count("session") != 0 {
		h.t.Errorf("a session was made: %v", h.calls.snapshot())
	}
	h.assertNoCallsBeforeRetry(tr)
}

// assertNoCallsBeforeRetry waits for tr's next retry time and checks that
// nothing but Docker (the server's reconciliation) is called until just
// before it (0002 SPEC R174).
func (h *harness) assertNoCallsBeforeRetry(tr *targetRun) {
	h.t.Helper()
	waitFor(h.t, "a retry time", func() bool { return !tr.retryAt().IsZero() })
	at := tr.retryAt()
	h.clock.waitTimersAt(h.t, at, 1)
	before := len(h.calls.snapshot())
	h.clock.Advance(at.Sub(h.clock.Now()) - time.Nanosecond)
	for range 100 {
		runtime.Gosched()
	}
	for _, c := range h.calls.snapshot()[before:] {
		if !strings.HasPrefix(c, "docker.") {
			h.t.Errorf("call %q in the target error wait before its retry", c)
		}
	}
}

// TestTargetStartupPermanentErrorsTargetErrorWait proves 0002 SPEC R172 and
// R195 with one target: a permanent error of a scale set call or of the first
// session puts the target, not the scaler, in the error wait; a signal then
// exits 0.
func TestTargetStartupPermanentErrorsTargetErrorWait(t *testing.T) {
	tests := []struct {
		name  string
		setup func(h *harness)
		want  string
	}{
		{"several scale sets", func(h *harness) {
			h.gh.setErr = func(int) error { return opErr(retry.OpGetOrCreateScaleSet, retry.ErrAmbiguousScaleSet) }
		}, "several"},
		{"auth refused", func(h *harness) {
			h.gh.setErr = func(int) error { return opErr(retry.OpGetOrCreateScaleSet, retry.ErrAuth) }
		}, "authentication"},
		{"session create 404", func(h *harness) {
			h.gh.createErr = func(int) error { return opErr(retry.OpSessionCreate, statusError(404)) }
		}, "404"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t)
			tt.setup(h)
			h.start()
			h.assertTargetErrorWait(h.targetRuns()[0], tt.want)
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
	h.cfgErr = errors.Join(errors.New("targets[0].max-runners: must be at least 1"), errors.New("unknown key x"))
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
	if !strings.Contains(r.LastError, "targets[0].max-runners") || !strings.Contains(r.LastError, "unknown key x") {
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

// TestStartupNoTargets proves 0002 SPEC R105: a configuration with no target
// enters the error wait with the SPEC text in the last error and the log, and
// nothing calls GitHub.
func TestStartupNoTargets(t *testing.T) {
	h := newHarness(t)
	cfg := testConfig()
	cfg.Targets = nil
	h.cfgErr = cfg.Validate()
	h.start()
	h.waitState(status.ErrorWait)
	const want = "targets: at least 1 [[targets]] entry is required, got 0"
	if r := h.report(); !strings.Contains(r.LastError, want) {
		t.Errorf("lastError = %q, want %q in it", r.LastError, want)
	}
	if !strings.Contains(h.logs.String(), want) {
		t.Error("the log lacks the R105 text")
	}
	if indexOf(h.calls.snapshot(), "github.") >= 0 {
		t.Errorf("GitHub was called: %v", h.calls.snapshot())
	}
	h.signal()
	if err := h.result(); err != nil {
		t.Fatal(err)
	}
}

// TestStartup0001Format proves 0002 SPEC R107: a 0001 configuration enters the
// error wait and both SPEC texts are in the last error.
func TestStartup0001Format(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	content := `
[github]
app-client-id = "client"
app-installation-id = 12345
app-private-key-file = "/key.pem"

[[pools]]
name = "linux-arm64-deploy"
repo-url = "https://github.com/owner/repo"
max = 2
allowed-workflows = [".github/workflows/deploy.yml"]
`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	_, loadErr := config.Load(path)
	if loadErr == nil {
		t.Fatal("a 0001 configuration loaded")
	}
	h := newHarness(t)
	h.cfgErr = loadErr
	h.start()
	h.waitState(status.ErrorWait)
	r := h.report()
	for _, want := range []string{
		`pools: [[pools]] is the 0001 format; move max to [[targets]] max-runners, idle to [[targets]] idle, runner-*/build-* to [runner]/[buildkit] cpus, memory, pids, name to [scaling] label, and set [[targets]] url = "https://github.com/<org>"; allowed-workflows and allowed-events no longer exist`,
		"github.app-installation-id: no longer used; the installation is looked up for each target",
	} {
		if !strings.Contains(r.LastError, want) {
			t.Errorf("lastError lacks %q:\n%s", want, r.LastError)
		}
	}
	if indexOf(h.calls.snapshot(), "github.") >= 0 {
		t.Errorf("GitHub was called: %v", h.calls.snapshot())
	}
	h.signal()
	if err := h.result(); err != nil {
		t.Fatal(err)
	}
}

// TestStartupInstallationLookup proves 0002 SPEC R108: the looked-up
// installation ID is what the target's clients are made with.
func TestStartupInstallationLookup(t *testing.T) {
	h := newHarness(t)
	h.gh.inst.ID = 4242
	h.running()
	log := h.calls.snapshot()
	i, j := indexOf(log, "github.Installation owner"), indexOf(log, "github.Target owner 4242")
	if i < 0 || j < i {
		t.Fatalf("the installation did not reach the target clients; calls:\n%s", strings.Join(log, "\n"))
	}
	if n := h.calls.count("github.Installation"); n != 1 {
		t.Errorf("installation lookups = %d, want 1", n)
	}
}

// TestStartupNotInstalled proves 0002 SPEC R109.
func TestStartupNotInstalled(t *testing.T) {
	h := newHarness(t)
	h.gh.instErr = func(int) error {
		return opErr(retry.OpGetInstallation, &github.StatusError{Code: 404, Message: "GitHub App is not installed on organization owner"})
	}
	h.start()
	h.assertTargetErrorWait(h.targetRuns()[0], "GitHub App is not installed on organization owner")
	if n := h.calls.count("github.Installation"); n != 1 {
		t.Errorf("a permanent error was retried: %d lookups", n)
	}
	if indexOf(h.calls.snapshot(), "github.Target") >= 0 {
		t.Errorf("start-up went on: %v", h.calls.snapshot())
	}
	h.signal()
	if err := h.result(); err != nil {
		t.Fatal(err)
	}
}

// TestStartupMissingPermission proves 0002 SPEC R110: a missing or read-only
// permission names the permission and the organization.
func TestStartupMissingPermission(t *testing.T) {
	for name, perms := range map[string]map[string]string{
		"missing":   {"metadata": "read"},
		"read only": {"organization_self_hosted_runners": "read"},
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			h.gh.inst.Permissions = perms
			h.start()
			h.assertTargetErrorWait(h.targetRuns()[0], "organization_self_hosted_runners: write")
			if last, _ := h.targetRuns()[0].rec.Last(); !strings.Contains(last, "organization owner") {
				t.Errorf("target lastError = %q, want the organization", last)
			}
			if indexOf(h.calls.snapshot(), "github.Target") >= 0 {
				t.Errorf("start-up went on: %v", h.calls.snapshot())
			}
			h.signal()
			if err := h.result(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// TestStartupRunnerGroup proves 0002 SPEC R111: an empty name uses group 1, a
// name is looked up and its ID used, a missing group shows its name, and
// transient errors are retried.
func TestStartupRunnerGroup(t *testing.T) {
	t.Run("empty name", func(t *testing.T) {
		h := newHarness(t)
		h.running()
		if n := h.calls.count("github.RunnerGroupID "); n != 1 {
			t.Errorf("RunnerGroupID calls = %d, want 1 with an empty name", n)
		}
		if n := h.calls.count("github.GetOrCreateScaleSet 1 pool"); n != 1 {
			t.Errorf("scale set not made in group 1: %v", h.calls.snapshot())
		}
	})
	t.Run("named group", func(t *testing.T) {
		h := newHarness(t)
		h.cfg.Targets[0].RunnerGroup = "deploy"
		h.gh.groupID = 5
		h.running()
		log := h.calls.snapshot()
		if indexOf(log, "github.RunnerGroupID deploy") < 0 || indexOf(log, "github.GetOrCreateScaleSet 5 pool") < 0 {
			t.Errorf("calls:\n%s", strings.Join(log, "\n"))
		}
	})
	t.Run("missing group", func(t *testing.T) {
		h := newHarness(t)
		h.cfg.Targets[0].RunnerGroup = "deploy"
		h.gh.groupErr = func(int) error {
			return opErr(retry.OpGetRunnerGroup, fmt.Errorf("no runner group found with name %q: %w", "deploy", retry.ErrTargetSetup))
		}
		h.start()
		h.assertTargetErrorWait(h.targetRuns()[0], `"deploy"`)
		if n := h.calls.count("github.RunnerGroupID"); n != 1 {
			t.Errorf("a permanent error was retried: %d lookups", n)
		}
		if indexOf(h.calls.snapshot(), "github.GetOrCreateScaleSet") >= 0 {
			t.Errorf("start-up went on: %v", h.calls.snapshot())
		}
		h.signal()
		if err := h.result(); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("transient errors retried", func(t *testing.T) {
		h := newHarness(t)
		h.gh.groupErr = func(n int) error {
			if n <= 2 {
				return opErr(retry.OpGetRunnerGroup, statusError(503))
			}
			return nil
		}
		h.start()
		waitFor(t, "first lookup", func() bool { return h.calls.count("github.RunnerGroupID") == 1 })
		h.clock.advanceTo(t, testEpoch.Add(time.Second))
		waitFor(t, "second lookup", func() bool { return h.calls.count("github.RunnerGroupID") == 2 })
		h.clock.advanceTo(t, testEpoch.Add(3*time.Second))
		h.waitState(status.Running)
		if n := h.calls.count("github.RunnerGroupID"); n != 3 {
			t.Fatalf("lookups = %d, want 3", n)
		}
	})
}

// TestStartupScaleSetInGroup proves 0002 SPEC R112: the registration cleanup
// looks the scale set up and start-up gets or creates it, both in the
// target's runner group under the label.
func TestStartupScaleSetInGroup(t *testing.T) {
	h := newHarness(t)
	h.cfg.Scaling.Label = "ci"
	h.gh.groupID = 5
	h.running()
	log := h.calls.snapshot()
	i, j := indexOf(log, "github.FindScaleSet 5 ci"), indexOf(log, "github.GetOrCreateScaleSet 5 ci")
	if i < 0 || j < i {
		t.Fatalf("calls:\n%s", strings.Join(log, "\n"))
	}
}

// TestStartupKeepsScaleSetOnExit proves 0002 SPEC R112: the scale set is not
// deleted when the scaler ends, so it is made once and never removed.
func TestStartupKeepsScaleSetOnExit(t *testing.T) {
	h := newHarness(t)
	h.running()
	h.signal()
	if err := h.result(); err != nil {
		t.Fatal(err)
	}
	if n := h.calls.count("github.GetOrCreateScaleSet"); n != 1 {
		t.Errorf("scale set calls = %d, want 1", n)
	}
	if indexOf(h.calls.snapshot(), "github.DeleteScaleSet") >= 0 {
		t.Error("the scale set was deleted on exit")
	}
}

// logLine returns the first JSON log line whose msg is msg, decoded.
func logLine(t *testing.T, logs, msg string) map[string]any {
	t.Helper()
	for _, line := range strings.Split(logs, "\n") {
		if !strings.Contains(line, `"msg":"`+msg+`"`) {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("log line %q: %v", line, err)
		}
		return m
	}
	return nil
}

// capacityDaemon returns a Daemon with the status pieces Run makes, for
// calling checkCapacity directly with several targets.
func capacityDaemon(t *testing.T, h *harness) *Daemon {
	t.Helper()
	d, err := New(h.config())
	if err != nil {
		t.Fatal(err)
	}
	rec, err := status.NewRecorder(h.secrets, h.clock, d.log)
	if err != nil {
		t.Fatal(err)
	}
	st, err := status.New(rec)
	if err != nil {
		t.Fatal(err)
	}
	d.rec, d.st = rec, st
	return d
}

// targetsOf returns targets with the given idle and max-runners values.
func targetsOf(idle, maxRunners []int) []config.Target {
	var ts []config.Target
	for i := range idle {
		ts = append(ts, config.Target{
			URL: fmt.Sprintf("https://github.com/org%d", i), Idle: idle[i], MaxRunners: maxRunners[i],
		})
	}
	return ts
}

// TestStartupServerLimitLog proves 0002 SPEC R203 and R201: the INFO line
// with its five fields, and the limit in the status report. The Docker VM
// of the SPEC (10 CPUs, 7.75 GiB) with the default shares gives 2.
func TestStartupServerLimitLog(t *testing.T) {
	h := newHarness(t)
	h.docker.info = docker.Capacity{NCPU: 10, MemTotal: 7936 << 20}
	h.running()
	m := logLine(t, h.logs.String(), "server runner limit")
	if m == nil {
		t.Fatalf("no server runner limit line:\n%s", h.logs.String())
	}
	if m["level"] != "INFO" {
		t.Errorf("level = %v, want INFO", m["level"])
	}
	want := map[string]float64{
		"limit": 2, "ncpu": 10, "memTotal": 7936 << 20, "runnerCPUs": 3, "runnerMemory": 3 << 30,
	}
	for k, v := range want {
		if m[k] != v {
			t.Errorf("%s = %v, want %v", k, m[k], v)
		}
	}
	if r := h.report(); r.ServerLimit == nil || *r.ServerLimit != 2 {
		t.Errorf("status serverLimit = %v, want 2", r.ServerLimit)
	}
	if n := h.calls.count("docker.Info"); n != 1 {
		t.Errorf("/info calls = %d, want 1 (R201)", n)
	}
	if i, j := indexOf(h.calls.snapshot(), "docker.ImageExists moby"), indexOf(h.calls.snapshot(), "docker.Info"); j < i || indexOf(h.calls.snapshot(), "github.") < j {
		t.Errorf("docker.Info not between the image check and the GitHub calls: %v", h.calls.snapshot())
	}
}

// TestStartupDockerInfoRetried proves 0002 SPEC R201: a transient /info
// error is retried with the start-up backoff, and a permanent one is an
// error wait.
func TestStartupDockerInfoRetried(t *testing.T) {
	h := newHarness(t)
	h.docker.infoErr = func(n int) error {
		if n == 1 {
			return opErr(retry.OpDockerInfo, statusError(503))
		}
		return nil
	}
	h.start()
	waitFor(t, "first info call", func() bool { return h.calls.count("docker.Info") == 1 })
	h.clock.advanceTo(t, testEpoch.Add(time.Second))
	h.waitState(status.Running)
	if n := h.calls.count("docker.Info"); n != 2 {
		t.Fatalf("/info calls = %d, want 2 (one retry, then no more)", n)
	}
	h.signal()
	if err := h.result(); err != nil {
		t.Fatal(err)
	}

	h = newHarness(t)
	h.docker.infoErr = func(int) error { return opErr(retry.OpDockerInfo, statusError(400)) }
	h.start()
	h.waitState(status.ErrorWait)
	if r := h.report(); !strings.Contains(r.LastError, "docker info") {
		t.Errorf("lastError = %q, want the docker info error", r.LastError)
	}
	if n := h.calls.count("docker.Info"); n != 1 {
		t.Errorf("/info calls = %d, want 1", n)
	}
	h.signal()
	if err := h.result(); err != nil {
		t.Fatal(err)
	}
}

// TestStartupServerLimitZero proves 0002 SPEC R202: a limit of 0 is an error
// wait whose text carries the four key values. runner.cpus above NCPU is the
// dropped R118 case.
func TestStartupServerLimitZero(t *testing.T) {
	tests := []struct {
		name  string
		setup func(h *harness)
		want  string
	}{
		{"cpus", func(h *harness) {
			h.docker.info = docker.Capacity{NCPU: 10, MemTotal: 64 << 30}
			h.cfg.Runner.CPUs = 11
		}, "server runner limit is 0: one runner reserves 13 CPUs (runner.cpus 11 + buildkit.cpus 2) and 3221225472 bytes (runner.memory 1g + buildkit.memory 2g), Docker reports 10 CPUs and 68719476736 bytes"},
		{"memory", func(h *harness) {
			h.docker.info = docker.Capacity{NCPU: 64, MemTotal: 2 << 30}
		}, "server runner limit is 0: one runner reserves 3 CPUs (runner.cpus 1 + buildkit.cpus 2) and 3221225472 bytes (runner.memory 1g + buildkit.memory 2g), Docker reports 64 CPUs and 2147483648 bytes"},
		{"fraction", func(h *harness) {
			h.docker.info = docker.Capacity{NCPU: 1, MemTotal: 64 << 30}
			h.cfg.Runner.CPUs = 0.5
			h.cfg.BuildKit.CPUs = 1
		}, "server runner limit is 0: one runner reserves 1.5 CPUs (runner.cpus 0.5 + buildkit.cpus 1) and 3221225472 bytes (runner.memory 1g + buildkit.memory 2g), Docker reports 1 CPUs and 68719476736 bytes"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t)
			tt.setup(h)
			h.start()
			h.waitState(status.ErrorWait)
			if r := h.report(); !strings.Contains(r.LastError, tt.want) {
				t.Errorf("lastError = %q, want %q", r.LastError, tt.want)
			}
			if indexOf(h.calls.snapshot(), "github.") >= 0 {
				t.Errorf("GitHub was called: %v", h.calls.snapshot())
			}
			h.signal()
			if err := h.result(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// TestStartupIdleAboveLimit proves 0002 SPEC R167: the idle sum above the
// server limit is an error wait with the SPEC text, for one target through
// start and for several through checkCapacity.
func TestStartupIdleAboveLimit(t *testing.T) {
	h := newHarness(t)
	h.docker.info = docker.Capacity{NCPU: 10, MemTotal: 7936 << 20}
	h.cfg.Targets[0].MaxRunners, h.cfg.Targets[0].Idle = 3, 3
	h.start()
	h.waitState(status.ErrorWait)
	const want = "idle of all targets (3) exceeds the server runner limit (2)"
	if r := h.report(); !strings.Contains(r.LastError, want) {
		t.Errorf("lastError = %q, want %q", r.LastError, want)
	}
	if indexOf(h.calls.snapshot(), "github.") >= 0 {
		t.Errorf("GitHub was called: %v", h.calls.snapshot())
	}
	h.signal()
	if err := h.result(); err != nil {
		t.Fatal(err)
	}

	h = newHarness(t)
	h.docker.info = docker.Capacity{NCPU: 10, MemTotal: 7936 << 20}
	cfg := testConfig()
	cfg.Targets = targetsOf([]int{1, 2}, []int{2, 2})
	if _, err := capacityDaemon(t, h).checkCapacity(context.Background(), cfg); err == nil || !strings.Contains(err.Error(), "idle of all targets (3) exceeds the server runner limit (2)") {
		t.Errorf("checkCapacity = %v, want the R167 error", err)
	}
}

// TestStartupMaxRunnersAboveLimitInfo proves 0002 SPEC R204: the sum of
// max-runners above the limit is an INFO line, not a WARN, and start-up goes
// on.
func TestStartupMaxRunnersAboveLimitInfo(t *testing.T) {
	h := newHarness(t)
	h.docker.info = docker.Capacity{NCPU: 10, MemTotal: 7936 << 20}
	h.cfg.Targets[0].MaxRunners = 3
	h.running()
	m := logLine(t, h.logs.String(), "max-runners of all targets exceed the server limit")
	if m == nil {
		t.Fatalf("no max-runners line:\n%s", h.logs.String())
	}
	if m["level"] != "INFO" || m["sum"] != float64(3) || m["limit"] != float64(2) {
		t.Errorf("line = %v, want INFO with sum 3 and limit 2", m)
	}

	// At or under the limit there is no line.
	h = newHarness(t)
	h.docker.info = docker.Capacity{NCPU: 10, MemTotal: 7936 << 20}
	h.running()
	if logLine(t, h.logs.String(), "max-runners of all targets exceed the server limit") != nil {
		t.Error("the max-runners line was logged for a sum equal to the limit")
	}
}

// TestStartupIdleTakesEverySlot proves 0002 SPEC R168: with two or more
// targets, an idle sum equal to the limit and a target with idle 0 give one
// WARN per such target, and start-up goes on.
func TestStartupIdleTakesEverySlot(t *testing.T) {
	tests := []struct {
		name string
		idle []int
		want []string
	}{
		{"one idle 0", []int{2, 0}, []string{"https://github.com/org1"}},
		{"two idle 0", []int{2, 0, 0}, []string{"https://github.com/org1", "https://github.com/org2"}},
		{"no idle 0", []int{1, 1}, nil},
		{"sum below the limit", []int{1, 0}, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t)
			h.docker.info = docker.Capacity{NCPU: 10, MemTotal: 7936 << 20}
			cfg := testConfig()
			maxes := make([]int, len(tt.idle))
			for i := range maxes {
				maxes[i] = 2
			}
			cfg.Targets = targetsOf(tt.idle, maxes)
			limit, err := capacityDaemon(t, h).checkCapacity(context.Background(), cfg)
			if err != nil || limit != 2 {
				t.Fatalf("checkCapacity = %d, %v, want 2, nil", limit, err)
			}
			var got []string
			for _, line := range strings.Split(h.logs.String(), "\n") {
				if !strings.Contains(line, "idle runners of other targets take every slot") {
					continue
				}
				var m map[string]any
				if err := json.Unmarshal([]byte(line), &m); err != nil {
					t.Fatal(err)
				}
				if m["level"] != "WARN" {
					t.Errorf("level = %v, want WARN", m["level"])
				}
				got = append(got, m["target"].(string))
			}
			if !slices.Equal(got, tt.want) {
				t.Errorf("warned targets = %v, want %v", got, tt.want)
			}
		})
	}

	// One target never warns: idle equal to the limit with idle 0 elsewhere
	// needs a second target.
	h := newHarness(t)
	h.docker.info = docker.Capacity{NCPU: 10, MemTotal: 7936 << 20}
	h.cfg.Targets[0].Idle = 2
	h.running()
	if logLine(t, h.logs.String(), "idle runners of other targets take every slot") != nil {
		t.Error("a single target got the R168 warning")
	}
}

// TestStartupServerErrorBeforeTargets proves 0002 SPEC R176 for the checks
// before the capacity check and the capacity check itself: each leaves every
// target without a GitHub call and a session.
func TestStartupServerErrorBeforeTargets(t *testing.T) {
	tests := []struct {
		name  string
		setup func(h *harness)
	}{
		{"configuration", func(h *harness) { h.cfgErr = errors.New("targets: bad") }},
		{"docker", func(h *harness) {
			h.docker.pingErr = func(int) error { return opErr(retry.OpDockerPing, statusError(400)) }
		}},
		{"capacity", func(h *harness) { h.docker.info = docker.Capacity{NCPU: 1, MemTotal: 64 << 30} }},
		{"info", func(h *harness) {
			h.docker.infoErr = func(int) error { return opErr(retry.OpDockerInfo, statusError(400)) }
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t)
			tt.setup(h)
			h.start()
			h.waitState(status.ErrorWait)
			h.assertNoCallsFor(10 * time.Minute)
			if log := h.calls.snapshot(); indexOf(log, "github.") >= 0 || indexOf(log, "jobs.") >= 0 || h.calls.count("session") != 0 {
				t.Errorf("a GitHub, job server or session call ran: %v", log)
			}
			h.signal()
			if err := h.result(); err != nil {
				t.Fatal(err)
			}
		})
	}
}
