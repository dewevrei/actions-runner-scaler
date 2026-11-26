package daemon

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
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

// The start order of PLAN M5-3, then running; a signal drain with nothing
// to wait for ends in exit 0.
func TestStartupOrder(t *testing.T) {
	h := newHarness(t)
	h.running()
	order := []string{
		"lock", "status.Listen", "config", "docker.Ping", "docker.List", "exposure",
		"docker.ImageExists sha256:runner", "docker.ImageExists moby/buildkit",
		"github.New", "github.Installation owner", "github.Target owner 99", "github.RunnerGroupID ",
		"github.FindScaleSet 1 pool", "github.GetOrCreateScaleSet 1 pool", "github.SetSystemInfo", "jobs.New 127.0.0.1:8765", "jobs.Listen", "github.CreateSession 7 host",
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

// TestStartupOneTargetOnly: until 0002 PLAN M12 more than one target is an
// error wait.
func TestStartupOneTargetOnly(t *testing.T) {
	h := newHarness(t)
	second := config.DefaultTarget()
	second.URL = "https://github.com/other"
	h.cfg.Targets = append(h.cfg.Targets, second)
	h.start()
	h.waitState(status.ErrorWait)
	if r := h.report(); !strings.Contains(r.LastError, "targets: this build serves exactly 1 target, got 2") {
		t.Errorf("lastError = %q", r.LastError)
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
	h.waitState(status.ErrorWait)
	if r := h.report(); !strings.Contains(r.LastError, "GitHub App is not installed on organization owner") {
		t.Errorf("lastError = %q", r.LastError)
	}
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
			h.waitState(status.ErrorWait)
			r := h.report()
			if !strings.Contains(r.LastError, "organization_self_hosted_runners: write") || !strings.Contains(r.LastError, "organization owner") {
				t.Errorf("lastError = %q", r.LastError)
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
		h.waitState(status.ErrorWait)
		if r := h.report(); !strings.Contains(r.LastError, `"deploy"`) {
			t.Errorf("lastError = %q, want the group name", r.LastError)
		}
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
