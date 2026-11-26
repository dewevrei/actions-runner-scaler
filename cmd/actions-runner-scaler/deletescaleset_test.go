package main

import (
	"bytes"
	"context"
	"crypto/rsa"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
	// testPool is the label, which is the scale set's name.
	testPool = "deploy"
	testSet  = 7
	// testRepoURL is the configured target the tests delete; testInstID is
	// what the fake installation lookup returns.
	testRepoURL = "https://github.com/acme/widget"
	// testConfigPath is the path the harness reports as the config file.
	testConfigPath = "/etc/scaler/config.toml"
	testInstID     = 99
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

// fakeApp is deleteGitHub on canned answers. It records its calls apart from
// the target's.
type fakeApp struct {
	calls   []string
	inst    github.Installation
	instErr []error
	// target is what Target returns.
	target scaleSetAPI
}

func (f *fakeApp) Installation(_ context.Context, t github.Target) (github.Installation, error) {
	f.calls = append(f.calls, "Installation "+t.FullName())
	n := len(f.calls)
	if n <= len(f.instErr) && f.instErr[n-1] != nil {
		return github.Installation{}, f.instErr[n-1]
	}
	return f.inst, nil
}

func (f *fakeApp) Target(t github.Target, installationID int64) (scaleSetAPI, error) {
	f.calls = append(f.calls, fmt.Sprintf("Target %s %d", t.FullName(), installationID))
	return f.target, nil
}

// fakeGitHub is scaleSetAPI on canned answers. It records every call as
// "<Method> <argument>".
type fakeGitHub struct {
	calls []string
	// getArgs are the group and name of each GetScaleSetInGroup.
	getArgs []string
	// sets are the answers of successive GetScaleSet calls; the last one
	// repeats.
	sets    []setAnswer
	listed  []github.Runner
	listErr error
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

func (f *fakeGitHub) GetScaleSetInGroup(_ context.Context, groupID int, name string) (*scaleset.RunnerScaleSet, error) {
	f.record("GetScaleSet")
	f.getArgs = append(f.getArgs, fmt.Sprintf("%d %s", groupID, name))
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

func (f *fakeGitHub) ListRunners(context.Context) ([]github.Runner, error) {
	f.record("ListRunners")
	return f.listed, f.listErr
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
	closes    int
}

// Close counts the closes of the client, as an Engine client has one.
func (f *fakeDocker) Close() error {
	f.closes++
	return nil
}

func (f *fakeDocker) List(context.Context) ([]docker.Resource, error) {
	f.calls++
	return f.resources, f.err
}

// fakeClock fires every After at once and records the waits. With cancel
// set, the first wait cancels the context instead and never fires, as an
// interrupt during the wait would.
type fakeClock struct {
	waits  []time.Duration
	cancel context.CancelFunc
}

func (c *fakeClock) Now() time.Time { return time.Unix(0, 0) }

func (c *fakeClock) After(d time.Duration) <-chan time.Time {
	c.waits = append(c.waits, d)
	ch := make(chan time.Time, 1)
	if c.cancel != nil {
		c.cancel()
		return ch
	}
	ch <- c.Now()
	return ch
}

// deleteHarness wires fakes into deleteScaleSetDeps and counts what the
// command asks of them.
type deleteHarness struct {
	app   *fakeApp
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
	gh := &fakeGitHub{
		sets:       []setAnswer{{set: &scaleset.RunnerScaleSet{ID: testSet, Name: testPool}}},
		registered: map[string]github.Runner{},
	}
	return &deleteHarness{
		app: &fakeApp{
			inst: github.Installation{
				ID:          testInstID,
				Permissions: map[string]string{"administration": "write"},
			},
			target: gh,
		},
		gh:    gh,
		dk:    &fakeDocker{},
		clock: &fakeClock{},
		cfg: &config.Config{
			Scaling: config.Scaling{Label: testPool},
			Targets: []config.Target{
				{URL: "https://github.com/acme/other"},
				{URL: testRepoURL},
			},
		},
	}
}

// addRunner registers a runner of scale set setID and lists it in the
// repository.
func (h *deleteHarness) addRunner(id int64, name string, setID int) {
	h.gh.listed = append(h.gh.listed, github.Runner{ID: id, Name: name})
	h.gh.registered[name] = github.Runner{ID: id, Name: name, ScaleSetID: setID}
}

func (h *deleteHarness) deps() deleteScaleSetDeps {
	return deleteScaleSetDeps{
		ConfigPath: testConfigPath,
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
		GitHub: func(*config.Config, *rsa.PrivateKey) (deleteGitHub, error) {
			h.githubs++
			return h.app, nil
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

// run runs "delete-scaleset <target URL>" through dispatch, so the exit code
// is the real mapping, and returns it.
func (h *deleteHarness) run(targetURL string) int {
	return h.runCtx(context.Background(), targetURL)
}

// runCtx is run under ctx.
func (h *deleteHarness) runCtx(ctx context.Context, targetURL string) int {
	cmds := map[string]command{
		"delete-scaleset": {run: func(ctx context.Context, e env, _ []string) error {
			return runDeleteScaleSet(ctx, e, h.deps(), targetURL)
		}},
	}
	e := newEnv(&h.stdout, &h.stderr)
	return dispatch(ctx, e, []string{"delete-scaleset"}, cmds)
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
	checkExit(t, h.run(testRepoURL), 1)
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
	checkExit(t, h.run(testRepoURL), 1)
	if !h.logged() {
		t.Errorf("a lock failure is logged: %q", h.stderr.String())
	}
	if h.loads+h.githubs != 0 {
		t.Errorf("config loads %d, github clients %d, want 0", h.loads, h.githubs)
	}
}

// TestDeleteScaleSetConfigInvalid proves 0002 SPEC R269: a configuration that
// fails to load deletes nothing and exits 1.
func TestDeleteScaleSetConfigInvalid(t *testing.T) {
	t.Run("config error", func(t *testing.T) {
		h := newDeleteHarness()
		h.configErr = errors.New("unknown key")
		checkExit(t, h.run(testRepoURL), 1)
		checkContains(t, "stderr", h.stderr.String(), "load config", "unknown key")
		if h.githubs != 0 || h.unlocks != 1 {
			t.Errorf("github clients %d, unlocks %d, want 0 and 1", h.githubs, h.unlocks)
		}
	})
}

// TestDeleteScaleSetNotInConfig proves 0002 SPEC R269: an argument that is
// not a configured repository's URL (another repository, an organization URL
// or no URL) makes no GitHub call, deletes nothing, and exits 1 with the
// error that names the config path and the configured URLs.
func TestDeleteScaleSetNotInConfig(t *testing.T) {
	for name, arg := range map[string]string{
		"other repository": "https://github.com/acme/nope",
		"organization URL": "https://github.com/acme",
		"trailing slash":   testRepoURL + "/",
		"garbage":          "not a url",
		"empty":            "",
	} {
		t.Run(name, func(t *testing.T) {
			h := newDeleteHarness()
			checkExit(t, h.run(arg), 1)
			want := fmt.Sprintf("target %q is not in the config %s (targets: https://github.com/acme/other, %s); "+
				"delete-scaleset deletes only a repository listed in [[targets]]", arg, testConfigPath, testRepoURL)
			if got := loggedErr(t, h.stderr.String()); got != want {
				t.Errorf("error = %q\nwant    %q", got, want)
			}
			if h.githubs != 0 || h.unlocks != 1 {
				t.Errorf("github clients %d, unlocks %d, want 0 and 1", h.githubs, h.unlocks)
			}
			checkCalls(t, h.app.calls, nil)
			checkCalls(t, h.gh.calls, nil)
		})
	}
}

// loggedErr returns the err field of the one "command failed" line in
// stderr.
func loggedErr(t *testing.T, stderr string) string {
	t.Helper()
	var line struct {
		Msg string `json:"msg"`
		Err string `json:"err"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(stderr)), &line); err != nil {
		t.Fatalf("stderr %q is not one JSON line: %v", stderr, err)
	}
	if line.Msg != "command failed" {
		t.Fatalf("stderr msg = %q, want command failed", line.Msg)
	}
	return line.Err
}

// TestDeleteScaleSetNotInstalled proves 0002 SPEC R237, R151: no installation
// means an error on stderr, exit 1 and nothing deleted.
func TestDeleteScaleSetNotInstalled(t *testing.T) {
	h := newDeleteHarness()
	h.app.instErr = []error{&retry.OpError{Op: retry.OpGetInstallation,
		Err: &github.StatusError{Code: 404, Message: "GitHub App is not installed on repository acme/widget"}}}
	checkExit(t, h.run(testRepoURL), 1)
	checkContains(t, "stderr", h.stderr.String(), "GitHub App is not installed on repository acme/widget")
	checkCalls(t, h.app.calls, []string{"Installation acme/widget"})
	checkCalls(t, h.gh.calls, nil)
}

// TestDeleteScaleSetMissingPermission proves 0002 SPEC R238, R151.
func TestDeleteScaleSetMissingPermission(t *testing.T) {
	h := newDeleteHarness()
	h.app.inst.Permissions = map[string]string{"administration": "read"}
	checkExit(t, h.run(testRepoURL), 1)
	checkContains(t, "stderr", h.stderr.String(), "administration: write", "repository acme/widget")
	checkCalls(t, h.app.calls, []string{"Installation acme/widget"})
	checkCalls(t, h.gh.calls, nil)
}

// TestDeleteScaleSetNone proves 0002 SPEC R246: with no scale set of the
// label the command says so, deletes nothing and exits 0.
func TestDeleteScaleSetNone(t *testing.T) {
	h := newDeleteHarness()
	h.gh.sets = []setAnswer{{}}
	checkExit(t, h.run(testRepoURL), 0)
	if got, want := h.stdout.String(), "scale set 없음: "+testPool+". 지울 것이 없음\n"; got != want {
		t.Errorf("stdout = %q, want %q", got, want)
	}
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
	checkExit(t, h.run(testRepoURL), 1)
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
	checkExit(t, h.run(testRepoURL), 1)

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

// TestDeleteScaleSetTarget proves 0002 SPEC R150, R239: the target is found
// by its repository ignoring case, its installation is looked up, the scale
// set is looked up in runner group 1, the members' registrations are
// removed, then the scale set, then the scale set is looked up again. Other
// runners of the repository, and
// registrations whose names or scale sets differ, are untouched.
func TestDeleteScaleSetTarget(t *testing.T) {
	h := newDeleteHarness()
	h.gh.sets = []setAnswer{{set: &scaleset.RunnerScaleSet{ID: testSet}}, {}}
	h.addRunner(12, testPool+"-bbbbbbbb", testSet)
	h.addRunner(11, testPool+"-aaaaaaaa", testSet)
	h.addRunner(21, "other-0a1b2c3d", testSet+1)                                           // another label
	h.addRunner(22, testPool+"-0a1b2c3d", testSet+1)                                       // pool's name, another scale set
	h.addRunner(23, testPool+"-extra-0a1b2c3d", testSet)                                   // a pool named deploy-extra
	h.addRunner(24, testPool+"-0A1B2C3D", testSet)                                         // not lowercase hex
	h.addRunner(25, testPool+"-0a1b2c3", testSet)                                          // 7 characters
	h.addRunner(26, "build-box", 0)                                                        // not a scale set runner
	h.gh.listed = append(h.gh.listed, github.Runner{ID: 27, Name: testPool + "-ffffffff"}) // gone from the lookup
	h.dk.resources = []docker.Resource{{Kind: docker.KindRunner, RunnerName: testPool + "-bbbbbbbb", State: "exited"}}

	checkExit(t, h.run("https://github.com/ACME/Widget"), 0)
	checkCalls(t, h.app.calls, []string{"Installation acme/widget", "Target acme/widget 99"})
	if !slices.Equal(h.gh.getArgs, []string{"1 " + testPool, "1 " + testPool}) {
		t.Errorf("scale set lookups %q, want both in group 1", h.gh.getArgs)
	}
	checkCalls(t, h.gh.calls, []string{
		"GetScaleSet",
		"ListRunners",
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
	if h.dk.closes != 1 {
		t.Errorf("docker client closes = %d, want 1", h.dk.closes)
	}
	if h.logged() {
		t.Errorf("unexpected failure log: %q", h.stderr.String())
	}
}

// TestDeleteScaleSetUsesDefaultGroup proves 0002 SPEC R268, R239: the lookup
// asks runner group 1 for the label.
func TestDeleteScaleSetUsesDefaultGroup(t *testing.T) {
	h := newDeleteHarness()
	h.gh.sets = []setAnswer{{}}
	checkExit(t, h.run(testRepoURL), 0)
	if !slices.Equal(h.gh.getArgs, []string{"1 " + testPool}) {
		t.Errorf("scale set lookups %q, want one in group 1", h.gh.getArgs)
	}
}

// With no registration there is nothing to check in Docker.
func TestDeleteScaleSetNoRegistrations(t *testing.T) {
	h := newDeleteHarness()
	h.gh.sets = []setAnswer{{set: &scaleset.RunnerScaleSet{ID: testSet}}, {}}
	h.dockerErr = errors.New("docker is not running")
	checkExit(t, h.run(testRepoURL), 0)
	checkCalls(t, h.gh.calls, []string{"GetScaleSet", "ListRunners", "DeleteScaleSet 7", "GetScaleSet"})
	if h.dockers != 0 {
		t.Errorf("docker clients = %d, want 0", h.dockers)
	}
	checkContains(t, "stdout", h.stdout.String(), "runner 등록 0개 제거")
}

// The delete is confirmed by a second lookup that must find nothing.
func TestDeleteScaleSetStillExists(t *testing.T) {
	h := newDeleteHarness() // the set answers every GetScaleSet
	checkExit(t, h.run(testRepoURL), 1)
	checkContains(t, "stderr", h.stderr.String(), "still exists after the delete")
	checkCalls(t, h.gh.calls, []string{"GetScaleSet", "ListRunners", "DeleteScaleSet 7", "GetScaleSet"})
	if strings.Contains(h.stdout.String(), "삭제됨") {
		t.Errorf("stdout reports a deletion: %q", h.stdout.String())
	}
}

func TestDeleteScaleSetConfirmFails(t *testing.T) {
	h := newDeleteHarness()
	h.gh.sets = []setAnswer{{set: &scaleset.RunnerScaleSet{ID: testSet}}, {err: statusError{403}}}
	checkExit(t, h.run(testRepoURL), 1)
	checkContains(t, "stderr", h.stderr.String(), "confirm scale set", "HTTP 403")
}

// A registration that cannot be removed keeps the scale set; the other
// registrations are still removed.
func TestDeleteScaleSetRemoveFailure(t *testing.T) {
	h := newDeleteHarness()
	h.addRunner(11, testPool+"-aaaaaaaa", testSet)
	h.addRunner(12, testPool+"-bbbbbbbb", testSet)
	h.gh.removeErr = map[int64]error{11: statusError{403}}
	checkExit(t, h.run(testRepoURL), 1)
	checkContains(t, "stderr", h.stderr.String(), testPool+"-aaaaaaaa", "HTTP 403")
	checkCalls(t, h.gh.mutations(), []string{"RemoveRunner 11", "RemoveRunner 12"})
}

// R50: a registration that is gone already counts as removed.
func TestDeleteScaleSetGoneRegistration(t *testing.T) {
	h := newDeleteHarness()
	h.gh.sets = []setAnswer{{set: &scaleset.RunnerScaleSet{ID: testSet}}, {}}
	h.addRunner(11, testPool+"-aaaaaaaa", testSet)
	h.gh.removeErr = map[int64]error{11: statusError{404}}
	checkExit(t, h.run(testRepoURL), 0)
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
			checkExit(t, h.run(testRepoURL), 1)
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
		checkExit(t, h.run(testRepoURL), 0)
		checkCalls(t, h.gh.mutations(), []string{"DeleteScaleSet 7", "DeleteScaleSet 7", "DeleteScaleSet 7"})
		if want := []time.Duration{time.Second, 2 * time.Second}; !slices.Equal(h.clock.waits, want) {
			t.Errorf("waits = %v, want %v", h.clock.waits, want)
		}
	})
	t.Run("transient installation lookup", func(t *testing.T) {
		h := newDeleteHarness()
		h.gh.sets = []setAnswer{{set: &scaleset.RunnerScaleSet{ID: testSet}}, {}}
		h.app.instErr = []error{statusError{503}}
		checkExit(t, h.run(testRepoURL), 0)
		checkCalls(t, h.app.calls, []string{"Installation acme/widget", "Installation acme/widget", "Target acme/widget 99"})
		if want := []time.Duration{time.Second}; !slices.Equal(h.clock.waits, want) {
			t.Errorf("waits = %v, want %v", h.clock.waits, want)
		}
	})
	t.Run("permanent", func(t *testing.T) {
		h := newDeleteHarness()
		h.gh.deleteErrs = []error{statusError{403}}
		checkExit(t, h.run(testRepoURL), 1)
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
		checkExit(t, h.run(testRepoURL), 1)
		if want := deleteCallRetries + 1; h.gh.delCalls != want {
			t.Errorf("DeleteScaleSet calls = %d, want %d", h.gh.delCalls, want)
		}
	})
}

// A failed Docker list still closes the client.
func TestDeleteScaleSetClosesDockerOnError(t *testing.T) {
	h := newDeleteHarness()
	h.addRunner(11, testPool+"-aaaaaaaa", testSet)
	h.dk.err = statusError{403}
	checkExit(t, h.run(testRepoURL), 1)
	if h.dk.closes != 1 {
		t.Errorf("docker client closes = %d, want 1", h.dk.closes)
	}
}

type fakeCloser struct{ closed int }

func (c *fakeCloser) Close() error { c.closed++; return nil }

// The lister the command wires closes the Engine client it holds.
func TestEngineListerClose(t *testing.T) {
	c := &fakeCloser{}
	var l containerLister = engineLister{engine: c}
	closer, ok := l.(io.Closer)
	if !ok {
		t.Fatal("engineLister is not an io.Closer")
	}
	if err := closer.Close(); err != nil || c.closed != 1 {
		t.Errorf("Close() = %v, engine closes = %d, want nil and 1", err, c.closed)
	}
}

// A retry of DeleteScaleSet can get 404 because an earlier attempt deleted
// the scale set and its response was lost; the confirmation lookup then
// decides. A 404 on the first attempt is not explained that way.
func TestDeleteScaleSetGoneOnRetry(t *testing.T) {
	t.Run("retry gets 404, confirmed gone", func(t *testing.T) {
		h := newDeleteHarness()
		h.gh.sets = []setAnswer{{set: &scaleset.RunnerScaleSet{ID: testSet}}, {}}
		h.gh.deleteErrs = []error{errors.New("connection reset"), statusError{404}}
		checkExit(t, h.run(testRepoURL), 0)
		checkCalls(t, h.gh.calls, []string{
			"GetScaleSet", "ListRunners", "DeleteScaleSet 7", "DeleteScaleSet 7", "GetScaleSet",
		})
		checkContains(t, "stdout", h.stdout.String(), "scale set 삭제됨: "+testPool)
		if h.logged() {
			t.Errorf("unexpected failure log: %q", h.stderr.String())
		}
	})
	t.Run("retry gets 404, still exists", func(t *testing.T) {
		h := newDeleteHarness() // the set answers every GetScaleSet
		h.gh.deleteErrs = []error{errors.New("connection reset"), statusError{404}}
		checkExit(t, h.run(testRepoURL), 1)
		checkContains(t, "stderr", h.stderr.String(), "still exists after the delete")
		if strings.Contains(h.stdout.String(), "삭제됨") {
			t.Errorf("stdout reports a deletion: %q", h.stdout.String())
		}
	})
	t.Run("first attempt gets 404", func(t *testing.T) {
		h := newDeleteHarness()
		h.gh.sets = []setAnswer{{set: &scaleset.RunnerScaleSet{ID: testSet}}, {}}
		h.gh.deleteErrs = []error{statusError{404}}
		checkExit(t, h.run(testRepoURL), 1)
		checkContains(t, "stderr", h.stderr.String(), "delete scale set", "HTTP 404")
		checkCalls(t, h.gh.calls, []string{"GetScaleSet", "ListRunners", "DeleteScaleSet 7"})
	})
}

// R52: a wait honors the rate limit's Retry-After or reset time when it is
// longer than the backoff, and says on stdout until when it waits.
func TestDeleteScaleSetRateLimitWait(t *testing.T) {
	epoch := time.Unix(0, 0)
	tests := []struct {
		name string
		err  error
		want time.Duration
	}{
		{"retry-after", &retry.RateLimitError{RetryAfter: 90 * time.Second}, 90 * time.Second},
		{"reset", &retry.RateLimitError{Reset: epoch.Add(5 * time.Minute)}, 5 * time.Minute},
		{"no hint", &retry.RateLimitError{}, retry.DefaultRateLimitWait},
		{"bare 429", statusError{429}, retry.DefaultRateLimitWait},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newDeleteHarness()
			h.gh.sets = []setAnswer{{set: &scaleset.RunnerScaleSet{ID: testSet}}, {}}
			h.gh.deleteErrs = []error{tt.err}
			checkExit(t, h.run(testRepoURL), 0)
			if want := []time.Duration{tt.want}; !slices.Equal(h.clock.waits, want) {
				t.Errorf("waits = %v, want %v", h.clock.waits, want)
			}
			checkContains(t, "stdout", h.stdout.String(),
				epoch.Add(tt.want).Format(waitTimeLayout)+"까지 대기 (속도 제한)\n")
			if n := strings.Count(h.stdout.String(), "속도 제한"); n != 1 {
				t.Errorf("rate limit lines = %d, want 1:\n%s", n, h.stdout.String())
			}
		})
	}
	t.Run("transient error prints no wait line", func(t *testing.T) {
		h := newDeleteHarness()
		h.gh.sets = []setAnswer{{set: &scaleset.RunnerScaleSet{ID: testSet}}, {}}
		h.gh.deleteErrs = []error{errors.New("connection reset")}
		checkExit(t, h.run(testRepoURL), 0)
		if strings.Contains(h.stdout.String(), "속도 제한") {
			t.Errorf("stdout mentions a rate limit: %q", h.stdout.String())
		}
	})
	t.Run("a rate limit wait does not leak into a later transient wait", func(t *testing.T) {
		h := newDeleteHarness()
		h.gh.sets = []setAnswer{{set: &scaleset.RunnerScaleSet{ID: testSet}}, {}}
		h.gh.deleteErrs = []error{&retry.RateLimitError{RetryAfter: time.Minute}, errors.New("connection reset")}
		checkExit(t, h.run(testRepoURL), 0)
		if n := strings.Count(h.stdout.String(), "속도 제한"); n != 1 {
			t.Errorf("rate limit lines = %d, want 1:\n%s", n, h.stdout.String())
		}
	})
}

// An interrupt while the command waits to retry stops it with an error
// before it deletes anything further.
func TestDeleteScaleSetCanceledDuringRetryWait(t *testing.T) {
	boom := errors.New("connection reset")
	tests := []struct {
		name      string
		setup     func(h *deleteHarness)
		wantCalls []string
	}{
		{"list runners", func(h *deleteHarness) { h.gh.listErr = boom },
			[]string{"GetScaleSet", "ListRunners"}},
		{"delete scale set", func(h *deleteHarness) { h.gh.deleteErrs = []error{boom, boom} },
			[]string{"GetScaleSet", "ListRunners", "DeleteScaleSet 7"}},
		{"remove runner", func(h *deleteHarness) {
			h.addRunner(11, testPool+"-aaaaaaaa", testSet)
			h.gh.removeErr = map[int64]error{11: boom}
		}, []string{"GetScaleSet", "ListRunners", "GetRunnerByName " + testPool + "-aaaaaaaa", "RemoveRunner 11"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			h := newDeleteHarness()
			h.clock.cancel = cancel
			tt.setup(h)
			checkExit(t, h.runCtx(ctx, testRepoURL), 1)
			checkContains(t, "stderr", h.stderr.String(), "context canceled")
			checkCalls(t, h.gh.calls, tt.wantCalls)
			if len(h.clock.waits) != 1 {
				t.Errorf("waits = %v, want one", h.clock.waits)
			}
			if strings.Contains(h.stdout.String(), "삭제됨") {
				t.Errorf("stdout reports a deletion: %q", h.stdout.String())
			}
			if h.unlocks != 1 {
				t.Errorf("unlocks = %d, want 1", h.unlocks)
			}
		})
	}
}

// The real command checks its arguments before it touches the lock, the
// config or GitHub.
func TestDeleteScaleSetUsage(t *testing.T) {
	const usageLine = "usage: delete-scaleset [-config path] <repository URL>"
	tests := []struct {
		args []string
		want string
	}{
		{[]string{"delete-scaleset"}, usageLine},
		{[]string{"delete-scaleset", "a", "b"}, usageLine},
		{[]string{"delete-scaleset", "-bogus", "a"}, "delete-scaleset"},
	}
	for _, tt := range tests {
		args := tt.args
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			if got := dispatch(context.Background(), newEnv(&stdout, &stderr), args, commands()); got != 1 {
				t.Errorf("exit = %d, want 1", got)
			}
			checkContains(t, "stderr", stderr.String(), tt.want)
			if strings.Contains(stderr.String(), `"msg":"command failed"`) {
				t.Errorf("a usage mistake is not logged as a failure: %q", stderr.String())
			}
		})
	}
}
