package main

import (
	"bytes"
	"context"
	"crypto/rsa"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/actions/scaleset"

	"github.com/dewevrei/actions-runner-scaler/internal/config"
	"github.com/dewevrei/actions-runner-scaler/internal/daemon"
	"github.com/dewevrei/actions-runner-scaler/internal/docker"
	"github.com/dewevrei/actions-runner-scaler/internal/github"
	"github.com/dewevrei/actions-runner-scaler/internal/retry"
)

const (
	testPool = "deploy"
	testSet  = 7
)

// statusError is an error with an HTTP status, as the github package's are.
type statusError struct{ code int }

func (e statusError) Error() string   { return "HTTP " + strconv.Itoa(e.code) }
func (e statusError) HTTPStatus() int { return e.code }

// setAnswer is one answer of GetScaleSet.
type setAnswer struct {
	set *scaleset.RunnerScaleSet
	err error
}

// fakeGitHub is scaleSetAPI on canned answers. It records every call as
// "<Method> <argument>".
type fakeGitHub struct {
	calls []string
	// sets are the answers of successive GetScaleSet calls; the last one
	// repeats.
	sets        []setAnswer
	repoRunners []github.RepoRunner
	listErr     error
	// registered is what GetRunnerByName knows, by name.
	registered map[string]github.Runner
	lookupErr  error
	// removeErr is the error of RemoveRunner, by runner ID.
	removeErr map[int64]error
	// deleteErrs are the errors of successive DeleteScaleSet calls; after
	// them it succeeds.
	deleteErrs []error
	getCalls   int
	delCalls   int
}

func (f *fakeGitHub) record(format string, args ...any) {
	f.calls = append(f.calls, fmt.Sprintf(format, args...))
}

func (f *fakeGitHub) GetScaleSet(_ context.Context, name string) (*scaleset.RunnerScaleSet, error) {
	f.record("GetScaleSet")
	a := f.sets[min(f.getCalls, len(f.sets)-1)]
	f.getCalls++
	return a.set, a.err
}

func (f *fakeGitHub) DeleteScaleSet(_ context.Context, id int) error {
	f.record("DeleteScaleSet %d", id)
	var err error
	if f.delCalls < len(f.deleteErrs) {
		err = f.deleteErrs[f.delCalls]
	}
	f.delCalls++
	return err
}

func (f *fakeGitHub) ListRepoRunners(context.Context) ([]github.RepoRunner, error) {
	f.record("ListRepoRunners")
	return f.repoRunners, f.listErr
}

func (f *fakeGitHub) GetRunnerByName(_ context.Context, name string) (*github.Runner, error) {
	f.record("GetRunnerByName %s", name)
	if f.lookupErr != nil {
		return nil, f.lookupErr
	}
	r, ok := f.registered[name]
	if !ok {
		return nil, nil
	}
	return &r, nil
}

func (f *fakeGitHub) RemoveRunner(_ context.Context, id int64) error {
	f.record("RemoveRunner %d", id)
	return f.removeErr[id]
}

func (f *fakeGitHub) mutations() []string {
	var out []string
	for _, c := range f.calls {
		if strings.HasPrefix(c, "RemoveRunner") || strings.HasPrefix(c, "DeleteScaleSet") {
			out = append(out, c)
		}
	}
	return out
}

// fakeDocker lists canned resources.
type fakeDocker struct {
	resources []docker.Resource
	err       error
	calls     int
}

func (f *fakeDocker) List(context.Context) ([]docker.Resource, error) {
	f.calls++
	return f.resources, f.err
}

// fakeClock fires every After at once and records the waits.
type fakeClock struct{ waits []time.Duration }

func (c *fakeClock) Now() time.Time { return time.Unix(0, 0) }

func (c *fakeClock) After(d time.Duration) <-chan time.Time {
	c.waits = append(c.waits, d)
	ch := make(chan time.Time, 1)
	ch <- c.Now()
	return ch
}

// deleteHarness wires fakes into deleteScaleSetDeps and counts what the
// command asks of them.
type deleteHarness struct {
	gh    *fakeGitHub
	dk    *fakeDocker
	clock *fakeClock
	cfg   *config.Config

	lockErr   error
	configErr error
	dockerErr error

	unlocks, loads, githubs, dockers int
	stdout, stderr                   bytes.Buffer
}

func newDeleteHarness() *deleteHarness {
	return &deleteHarness{
		gh: &fakeGitHub{
			sets:       []setAnswer{{set: &scaleset.RunnerScaleSet{ID: testSet, Name: testPool}}},
			registered: map[string]github.Runner{},
		},
		dk:    &fakeDocker{},
		clock: &fakeClock{},
		cfg: &config.Config{Pools: []config.Pool{
			{Name: "other", RepoURL: "https://github.com/o/r"},
			{Name: testPool, RepoURL: "https://github.com/o/r"},
		}},
	}
}

// addRunner registers a runner of scale set setID and lists it in the
// repository.
func (h *deleteHarness) addRunner(id int64, name string, setID int) {
	h.gh.repoRunners = append(h.gh.repoRunners, github.RepoRunner{ID: id, Name: name})
	h.gh.registered[name] = github.Runner{ID: id, Name: name, ScaleSetID: setID}
}

func (h *deleteHarness) deps() deleteScaleSetDeps {
	return deleteScaleSetDeps{
		Lock: func() (func(), error) {
			if h.lockErr != nil {
				return nil, h.lockErr
			}
			return func() { h.unlocks++ }, nil
		},
		LoadConfig: func() (*config.Config, *rsa.PrivateKey, error) {
			h.loads++
			return h.cfg, nil, h.configErr
		},
		GitHub: func(*config.Config, *rsa.PrivateKey, config.Pool) (scaleSetAPI, error) {
			h.githubs++
			return h.gh, nil
		},
		Docker: func() (containerLister, error) {
			h.dockers++
			if h.dockerErr != nil {
				return nil, h.dockerErr
			}
			return h.dk, nil
		},
		Clock:  h.clock,
		Jitter: func() float64 { return 0 },
	}
}

// run runs "delete-scaleset pool" through dispatch, so the exit code is the
// real mapping, and returns it.
func (h *deleteHarness) run(pool string) int {
	cmds := map[string]command{
		"delete-scaleset": {run: func(ctx context.Context, e env, _ []string) error {
			return runDeleteScaleSet(ctx, e, h.deps(), pool)
		}},
	}
	e := newEnv(&h.stdout, &h.stderr)
	return dispatch(context.Background(), e, []string{"delete-scaleset"}, cmds)
}

func (h *deleteHarness) logged() bool {
	return strings.Contains(h.stderr.String(), `"msg":"command failed"`)
}

func checkCalls(t *testing.T, got, want []string) {
	t.Helper()
	if !slices.Equal(got, want) {
		t.Errorf("calls = %q\nwant    %q", got, want)
	}
}

func checkExit(t *testing.T, got, want int) {
	t.Helper()
	if got != want {
		t.Errorf("exit = %d, want %d", got, want)
	}
}

func checkContains(t *testing.T, what, got string, wants ...string) {
	t.Helper()
	for _, w := range wants {
		if !strings.Contains(got, w) {
			t.Errorf("%s = %q, want it to contain %q", what, got, w)
		}
	}
}

// R49: with the lock held, nothing is read or deleted and the exit is 1.
func TestDeleteScaleSetLockHeld(t *testing.T) {
	h := newDeleteHarness()
	h.lockErr = fmt.Errorf("lock: %w", daemon.ErrLocked)
	checkExit(t, h.run(testPool), 1)
	checkContains(t, "stdout", h.stdout.String(), msgLocked)
	if h.logged() {
		t.Errorf("a held lock is reported on stdout, not logged: %q", h.stderr.String())
	}
	if h.loads+h.githubs+h.dockers+h.unlocks != 0 {
		t.Errorf("config loads %d, github clients %d, docker clients %d, unlocks %d, want all 0",
			h.loads, h.githubs, h.dockers, h.unlocks)
	}
	checkCalls(t, h.gh.calls, nil)
}

func TestDeleteScaleSetLockFailure(t *testing.T) {
	h := newDeleteHarness()
	h.lockErr = errors.New("permission denied")
	checkExit(t, h.run(testPool), 1)
	if !h.logged() {
		t.Errorf("a lock failure is logged: %q", h.stderr.String())
	}
	if h.loads+h.githubs != 0 {
		t.Errorf("config loads %d, github clients %d, want 0", h.loads, h.githubs)
	}
}

func TestDeleteScaleSetConfigErrors(t *testing.T) {
	t.Run("config error", func(t *testing.T) {
		h := newDeleteHarness()
		h.configErr = errors.New("unknown key")
		checkExit(t, h.run(testPool), 1)
		checkContains(t, "stderr", h.stderr.String(), "load config", "unknown key")
		if h.githubs != 0 || h.unlocks != 1 {
			t.Errorf("github clients %d, unlocks %d, want 0 and 1", h.githubs, h.unlocks)
		}
	})
	t.Run("unknown pool", func(t *testing.T) {
		h := newDeleteHarness()
		checkExit(t, h.run("nope"), 1)
		checkContains(t, "stderr", h.stderr.String(), `pool \"nope\" is not in the config`, "other, "+testPool)
		if h.githubs != 0 || h.unlocks != 1 {
			t.Errorf("github clients %d, unlocks %d, want 0 and 1", h.githubs, h.unlocks)
		}
		checkCalls(t, h.gh.calls, nil)
	})
}

func TestDeleteScaleSetNoScaleSet(t *testing.T) {
	h := newDeleteHarness()
	h.gh.sets = []setAnswer{{}}
	checkExit(t, h.run(testPool), 0)
	checkContains(t, "stdout", h.stdout.String(), "scale set 없음: "+testPool)
	checkCalls(t, h.gh.calls, []string{"GetScaleSet"})
	if h.unlocks != 1 {
		t.Errorf("unlocks = %d, want 1", h.unlocks)
	}
}

// R41: several scale sets with the name are for a person to fix; nothing is
// deleted.
func TestDeleteScaleSetDuplicates(t *testing.T) {
	h := newDeleteHarness()
	h.gh.sets = []setAnswer{{err: &retry.OpError{Op: retry.OpGetRunnerScaleSet, Err: retry.ErrAmbiguousScaleSet}}}
	h.addRunner(11, testPool+"-aaaaaaaa", testSet)
	checkExit(t, h.run(testPool), 1)
	checkContains(t, "stdout", h.stdout.String(), "같은 이름의 scale set이 여럿이다: "+testPool)
	if h.logged() {
		t.Errorf("duplicates are reported on stdout, not logged: %q", h.stderr.String())
	}
	checkCalls(t, h.gh.calls, []string{"GetScaleSet"})
	if h.dockers != 0 || h.unlocks != 1 {
		t.Errorf("docker clients %d, unlocks %d, want 0 and 1", h.dockers, h.unlocks)
	}
}

// PLAN M6-1: a member with a live runner container stops the command before
// anything is deleted; it prints the names and the instruction.
func TestDeleteScaleSetLiveContainer(t *testing.T) {
	h := newDeleteHarness()
	h.addRunner(11, testPool+"-aaaaaaaa", testSet)   // running container
	h.addRunner(12, testPool+"-bbbbbbbb", testSet)   // exited container and a running BuildKit
	h.addRunner(13, testPool+"-cccccccc", testSet)   // created container
	h.addRunner(14, testPool+"-dddddddd", testSet)   // no container
	h.addRunner(15, testPool+"-eeeeeeee", testSet+1) // another scale set; its container is alive
	h.dk.resources = []docker.Resource{
		{Kind: docker.KindRunner, RunnerName: testPool + "-aaaaaaaa", State: "running"},
		{Kind: docker.KindRunner, RunnerName: testPool + "-bbbbbbbb", State: "exited"},
		{Kind: docker.KindBuildKit, RunnerName: testPool + "-bbbbbbbb", State: "running"},
		{Kind: docker.KindRunner, RunnerName: testPool + "-cccccccc", State: "created"},
		{Kind: docker.KindRunner, RunnerName: testPool + "-eeeeeeee", State: "running"},
		{Kind: docker.KindRunner, RunnerName: "other-11111111", State: "running"},
	}
	checkExit(t, h.run(testPool), 1)

	out := h.stdout.String()
	checkContains(t, "stdout", out, testPool+"-aaaaaaaa", testPool+"-cccccccc", msgContainersLeft)
	for _, name := range []string{"-bbbbbbbb", "-dddddddd", "-eeeeeeee"} {
		if strings.Contains(out, testPool+name) {
			t.Errorf("stdout names %s, which has no live container of this scale set:\n%s", testPool+name, out)
		}
	}
	if !strings.HasSuffix(out, msgContainersLeft+"\n") {
		t.Errorf("stdout should end with the instruction:\n%s", out)
	}
	if h.logged() {
		t.Errorf("live containers are reported on stdout, not logged: %q", h.stderr.String())
	}
	checkCalls(t, h.gh.mutations(), nil)
}

// R23: the members' registrations are removed, then the scale set, then the
// scale set is looked up again. Runners of other pools, and registrations
// whose names or scale sets differ, are untouched.
func TestDeleteScaleSetSuccess(t *testing.T) {
	h := newDeleteHarness()
	h.gh.sets = []setAnswer{{set: &scaleset.RunnerScaleSet{ID: testSet}}, {}}
	h.addRunner(12, testPool+"-bbbbbbbb", testSet)
	h.addRunner(11, testPool+"-aaaaaaaa", testSet)
	h.addRunner(21, "other-0a1b2c3d", testSet+1)                                                         // another pool
	h.addRunner(22, testPool+"-0a1b2c3d", testSet+1)                                                     // pool's name, another scale set
	h.addRunner(23, testPool+"-extra-0a1b2c3d", testSet)                                                 // a pool named deploy-extra
	h.addRunner(24, testPool+"-0A1B2C3D", testSet)                                                       // not lowercase hex
	h.addRunner(25, testPool+"-0a1b2c3", testSet)                                                        // 7 characters
	h.addRunner(26, "build-box", 0)                                                                      // not a scale set runner
	h.gh.repoRunners = append(h.gh.repoRunners, github.RepoRunner{ID: 27, Name: testPool + "-ffffffff"}) // gone from the lookup
	h.dk.resources = []docker.Resource{{Kind: docker.KindRunner, RunnerName: testPool + "-bbbbbbbb", State: "exited"}}

	checkExit(t, h.run(testPool), 0)
	checkCalls(t, h.gh.calls, []string{
		"GetScaleSet",
		"ListRepoRunners",
		"GetRunnerByName " + testPool + "-bbbbbbbb",
		"GetRunnerByName " + testPool + "-aaaaaaaa",
		"GetRunnerByName " + testPool + "-0a1b2c3d",
		"GetRunnerByName " + testPool + "-ffffffff",
		"RemoveRunner 11",
		"RemoveRunner 12",
		"DeleteScaleSet 7",
		"GetScaleSet",
	})
	checkContains(t, "stdout", h.stdout.String(), "scale set 삭제됨: "+testPool+" (ID 7), runner 등록 2개 제거")
	if h.dk.calls != 1 || h.unlocks != 1 {
		t.Errorf("docker lists %d, unlocks %d, want 1 and 1", h.dk.calls, h.unlocks)
	}
	if h.logged() {
		t.Errorf("unexpected failure log: %q", h.stderr.String())
	}
}

// With no registration there is nothing to check in Docker.
func TestDeleteScaleSetNoRegistrations(t *testing.T) {
	h := newDeleteHarness()
	h.gh.sets = []setAnswer{{set: &scaleset.RunnerScaleSet{ID: testSet}}, {}}
	h.dockerErr = errors.New("docker is not running")
	checkExit(t, h.run(testPool), 0)
	checkCalls(t, h.gh.calls, []string{"GetScaleSet", "ListRepoRunners", "DeleteScaleSet 7", "GetScaleSet"})
	if h.dockers != 0 {
		t.Errorf("docker clients = %d, want 0", h.dockers)
	}
	checkContains(t, "stdout", h.stdout.String(), "runner 등록 0개 제거")
}

// The delete is confirmed by a second lookup that must find nothing.
func TestDeleteScaleSetStillExists(t *testing.T) {
	h := newDeleteHarness() // the set answers every GetScaleSet
	checkExit(t, h.run(testPool), 1)
	checkContains(t, "stderr", h.stderr.String(), "still exists after the delete")
	checkCalls(t, h.gh.calls, []string{"GetScaleSet", "ListRepoRunners", "DeleteScaleSet 7", "GetScaleSet"})
	if strings.Contains(h.stdout.String(), "삭제됨") {
		t.Errorf("stdout reports a deletion: %q", h.stdout.String())
	}
}

func TestDeleteScaleSetConfirmFails(t *testing.T) {
	h := newDeleteHarness()
	h.gh.sets = []setAnswer{{set: &scaleset.RunnerScaleSet{ID: testSet}}, {err: statusError{403}}}
	checkExit(t, h.run(testPool), 1)
	checkContains(t, "stderr", h.stderr.String(), "confirm scale set", "HTTP 403")
}

// A registration that cannot be removed keeps the scale set; the other
// registrations are still removed.
func TestDeleteScaleSetRemoveFailure(t *testing.T) {
	h := newDeleteHarness()
	h.addRunner(11, testPool+"-aaaaaaaa", testSet)
	h.addRunner(12, testPool+"-bbbbbbbb", testSet)
	h.gh.removeErr = map[int64]error{11: statusError{403}}
	checkExit(t, h.run(testPool), 1)
	checkContains(t, "stderr", h.stderr.String(), testPool+"-aaaaaaaa", "HTTP 403")
	checkCalls(t, h.gh.mutations(), []string{"RemoveRunner 11", "RemoveRunner 12"})
}

// R50: a registration that is gone already counts as removed.
func TestDeleteScaleSetGoneRegistration(t *testing.T) {
	h := newDeleteHarness()
	h.gh.sets = []setAnswer{{set: &scaleset.RunnerScaleSet{ID: testSet}}, {}}
	h.addRunner(11, testPool+"-aaaaaaaa", testSet)
	h.gh.removeErr = map[int64]error{11: statusError{404}}
	checkExit(t, h.run(testPool), 0)
	checkCalls(t, h.gh.mutations(), []string{"RemoveRunner 11", "DeleteScaleSet 7"})
}

// When the membership or the containers cannot be read, nothing is deleted.
func TestDeleteScaleSetLookupFailures(t *testing.T) {
	tests := []struct {
		name  string
		setup func(h *deleteHarness)
		want  string
	}{
		{"list runners", func(h *deleteHarness) { h.gh.listErr = statusError{403} }, "list repository runners"},
		{"look up a runner", func(h *deleteHarness) { h.gh.lookupErr = statusError{403} }, "look up runner"},
		{"docker client", func(h *deleteHarness) { h.dockerErr = errors.New("no socket") }, "no socket"},
		{"docker list", func(h *deleteHarness) { h.dk.err = statusError{403} }, "list runner containers"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newDeleteHarness()
			h.addRunner(11, testPool+"-aaaaaaaa", testSet)
			tt.setup(h)
			checkExit(t, h.run(testPool), 1)
			checkContains(t, "stderr", h.stderr.String(), tt.want)
			checkCalls(t, h.gh.mutations(), nil)
		})
	}
}

// R24 style retries: a transient error is repeated after the backoff, a
// permanent one is not, and the retries are bounded.
func TestDeleteScaleSetRetries(t *testing.T) {
	t.Run("transient then success", func(t *testing.T) {
		h := newDeleteHarness()
		h.gh.sets = []setAnswer{{set: &scaleset.RunnerScaleSet{ID: testSet}}, {}}
		h.gh.deleteErrs = []error{errors.New("connection reset"), statusError{503}}
		checkExit(t, h.run(testPool), 0)
		checkCalls(t, h.gh.mutations(), []string{"DeleteScaleSet 7", "DeleteScaleSet 7", "DeleteScaleSet 7"})
		if want := []time.Duration{time.Second, 2 * time.Second}; !slices.Equal(h.clock.waits, want) {
			t.Errorf("waits = %v, want %v", h.clock.waits, want)
		}
	})
	t.Run("permanent", func(t *testing.T) {
		h := newDeleteHarness()
		h.gh.deleteErrs = []error{statusError{403}}
		checkExit(t, h.run(testPool), 1)
		checkCalls(t, h.gh.mutations(), []string{"DeleteScaleSet 7"})
		if len(h.clock.waits) != 0 {
			t.Errorf("waits = %v, want none", h.clock.waits)
		}
		checkContains(t, "stderr", h.stderr.String(), "delete scale set", "HTTP 403")
	})
	t.Run("bounded", func(t *testing.T) {
		h := newDeleteHarness()
		boom := errors.New("connection reset")
		h.gh.deleteErrs = []error{boom, boom, boom, boom, boom, boom}
		checkExit(t, h.run(testPool), 1)
		if want := deleteCallRetries + 1; h.gh.delCalls != want {
			t.Errorf("DeleteScaleSet calls = %d, want %d", h.gh.delCalls, want)
		}
	})
}

func TestPoolRunnerNames(t *testing.T) {
	tests := []struct {
		pool, name string
		want       bool
	}{
		{"deploy", "deploy-0a1b2c3d", true},
		{"deploy", "deploy-0a1b2c3", false},
		{"deploy", "deploy-0a1b2c3de", false},
		{"deploy", "deploy-0A1B2C3D", false},
		{"deploy", "deploy-0a1b2c3g", false},
		{"deploy", "deploy-extra-0a1b2c3d", false},
		{"deploy-extra", "deploy-extra-0a1b2c3d", true},
		{"deploy", "other-0a1b2c3d", false},
		{"deploy", "xdeploy-0a1b2c3d", false},
		{"deploy", "deploy-0a1b2c3d\n", false},
		{"a.b", "a.b-0a1b2c3d", true},
		{"a.b", "axb-0a1b2c3d", false},
	}
	for _, tt := range tests {
		if got := poolRunnerNames(tt.pool)(tt.name); got != tt.want {
			t.Errorf("poolRunnerNames(%q)(%q) = %v, want %v", tt.pool, tt.name, got, tt.want)
		}
	}
}

// The real command checks its arguments before it touches the lock, the
// config or GitHub.
func TestDeleteScaleSetUsage(t *testing.T) {
	tests := [][]string{
		{"delete-scaleset"},
		{"delete-scaleset", "a", "b"},
		{"delete-scaleset", "-bogus", "a"},
	}
	for _, args := range tests {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			if got := dispatch(context.Background(), newEnv(&stdout, &stderr), args, commands()); got != 1 {
				t.Errorf("exit = %d, want 1", got)
			}
			checkContains(t, "stderr", stderr.String(), "delete-scaleset")
			if strings.Contains(stderr.String(), `"msg":"command failed"`) {
				t.Errorf("a usage mistake is not logged as a failure: %q", stderr.String())
			}
		})
	}
}
