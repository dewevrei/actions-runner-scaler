package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
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

// fakeToken is deleteTokenGitHub whose check is the real token client's
// against a test server and whose target is canned. It records its calls.
type fakeToken struct {
	calls []string
	// check is the real token side the harness made with the token.
	check  deleteTokenGitHub
	target scaleSetAPI
}

func (f *fakeToken) CheckRepositoryAccess(ctx context.Context, t github.Target) (github.Access, error) {
	f.calls = append(f.calls, "CheckRepositoryAccess "+t.FullName())
	return f.check.CheckRepositoryAccess(ctx, t)
}

func (f *fakeToken) Target(t github.Target) (scaleSetAPI, error) {
	f.calls = append(f.calls, "Target "+t.FullName())
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
	creds config.Credentials
	tok   *fakeToken
	// tokenAPI is the base URL the token side's real REST client asks.
	tokenAPI string

	lockErr   error
	configErr error
	dockerErr error

	unlocks, loads, githubs, dockers int
	// tokens are the tokens the token side was made with.
	tokens []string
	// appIDs and appKeys are the client IDs and keys the App side was made
	// with; dockerHosts the hosts Docker was connected to.
	appIDs         []string
	appKeys        []*rsa.PrivateKey
	dockerHosts    []string
	stdout, stderr bytes.Buffer
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
		tok:   &fakeToken{target: gh},
		dk:    &fakeDocker{},
		clock: &fakeClock{},
		// Both targets use the default App.
		cfg: &config.Config{
			GitHub:  config.GitHub{AppClientID: testClientID, AppPrivateKeyFile: "/key.pem"},
			Scaling: config.Scaling{Label: testPool},
			Targets: []config.Target{
				{URL: "https://github.com/acme/other"},
				{URL: testRepoURL},
			},
		},
		creds: config.Credentials{AppKey: deleteTestKey()},
	}
}

// testClientID is the default App's client ID in the harness.
const testClientID = "Iv1.default"

// deleteTestKey returns the default App's key in the harness.
func deleteTestKey() *rsa.PrivateKey {
	key, err := initTestKey()
	if err != nil {
		panic(err)
	}
	return key
}

// addRunner registers a runner of scale set setID and lists it in the
// repository.
func (h *deleteHarness) addRunner(id int64, name string, setID int) {
	h.gh.listed = append(h.gh.listed, github.Runner{ID: id, Name: name})
	h.gh.registered[name] = github.Runner{ID: id, Name: name, ScaleSetID: setID}
}

func (h *deleteHarness) deps(e env) deleteScaleSetDeps {
	return deleteScaleSetDeps{
		ConfigPath: testConfigPath,
		Lock: func() (func(), error) {
			if h.lockErr != nil {
				return nil, h.lockErr
			}
			return func() { h.unlocks++ }, nil
		},
		LoadConfig: func() (*config.Config, config.Credentials, error) {
			h.loads++
			return h.cfg, h.creds, h.configErr
		},
		GitHub: func(clientID string, key *rsa.PrivateKey) (deleteGitHub, error) {
			h.githubs++
			h.appIDs = append(h.appIDs, clientID)
			h.appKeys = append(h.appKeys, key)
			return h.app, nil
		},
		Token: func(token string) (deleteTokenGitHub, error) {
			h.tokens = append(h.tokens, token)
			check, err := newGitHubToken(e, h.clock, h.tokenAPI, http.DefaultClient, token)
			if err != nil {
				return nil, err
			}
			h.tok.check = check
			return h.tok, nil
		},
		Docker: func(host string) (containerLister, error) {
			h.dockers++
			h.dockerHosts = append(h.dockerHosts, host)
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
			return runDeleteScaleSet(ctx, e, h.deps(e), targetURL)
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

// TestDeleteScaleSetNotInstalled proves 0002 SPEC R237, R269: no installation
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

// TestDeleteScaleSetMissingPermission proves 0002 SPEC R238, R269.
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
	if got, want := h.stdout.String(), "no scale set: "+testPool+"; nothing to delete\n"; got != want {
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
	checkContains(t, "stdout", h.stdout.String(), "several scale sets have the same name: "+testPool)
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

// TestDeleteScaleSetTarget proves 0002 SPEC R268, R239: the target is found
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
	checkContains(t, "stdout", h.stdout.String(), "scale set deleted: "+testPool+" (ID 7), runner registrations removed: 2")
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
	checkContains(t, "stdout", h.stdout.String(), "runner registrations removed: 0")
}

// The delete is confirmed by a second lookup that must find nothing.
func TestDeleteScaleSetStillExists(t *testing.T) {
	h := newDeleteHarness() // the set answers every GetScaleSet
	checkExit(t, h.run(testRepoURL), 1)
	checkContains(t, "stderr", h.stderr.String(), "still exists after the delete")
	checkCalls(t, h.gh.calls, []string{"GetScaleSet", "ListRunners", "DeleteScaleSet 7", "GetScaleSet"})
	if strings.Contains(h.stdout.String(), "scale set deleted") {
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
	var l containerLister = engineClient{engine: c}
	closer, ok := l.(io.Closer)
	if !ok {
		t.Fatal("engineClient is not an io.Closer")
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
		checkContains(t, "stdout", h.stdout.String(), "scale set deleted: "+testPool)
		if h.logged() {
			t.Errorf("unexpected failure log: %q", h.stderr.String())
		}
	})
	t.Run("retry gets 404, still exists", func(t *testing.T) {
		h := newDeleteHarness() // the set answers every GetScaleSet
		h.gh.deleteErrs = []error{errors.New("connection reset"), statusError{404}}
		checkExit(t, h.run(testRepoURL), 1)
		checkContains(t, "stderr", h.stderr.String(), "still exists after the delete")
		if strings.Contains(h.stdout.String(), "scale set deleted") {
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
				"waiting until "+epoch.Add(tt.want).Format(waitTimeLayout)+" (rate limit)\n")
			if n := strings.Count(h.stdout.String(), "rate limit"); n != 1 {
				t.Errorf("rate limit lines = %d, want 1:\n%s", n, h.stdout.String())
			}
		})
	}
	t.Run("transient error prints no wait line", func(t *testing.T) {
		h := newDeleteHarness()
		h.gh.sets = []setAnswer{{set: &scaleset.RunnerScaleSet{ID: testSet}}, {}}
		h.gh.deleteErrs = []error{errors.New("connection reset")}
		checkExit(t, h.run(testRepoURL), 0)
		if strings.Contains(h.stdout.String(), "rate limit") {
			t.Errorf("stdout mentions a rate limit: %q", h.stdout.String())
		}
	})
	t.Run("a rate limit wait does not leak into a later transient wait", func(t *testing.T) {
		h := newDeleteHarness()
		h.gh.sets = []setAnswer{{set: &scaleset.RunnerScaleSet{ID: testSet}}, {}}
		h.gh.deleteErrs = []error{&retry.RateLimitError{RetryAfter: time.Minute}, errors.New("connection reset")}
		checkExit(t, h.run(testRepoURL), 0)
		if n := strings.Count(h.stdout.String(), "rate limit"); n != 1 {
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
			if strings.Contains(h.stdout.String(), "scale set deleted") {
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
			if got := dispatch(context.Background(), newEnv(&stdout, &stderr), args, commands(refusingHosts())); got != 1 {
				t.Errorf("exit = %d, want 1", got)
			}
			checkContains(t, "stderr", stderr.String(), tt.want)
			if strings.Contains(stderr.String(), `"msg":"command failed"`) {
				t.Errorf("a usage mistake is not logged as a failure: %q", stderr.String())
			}
		})
	}
}

// fakeTokenAPI is a GitHub API that answers 0003 SPEC R279's call: request
// i gets statuses[i], the last one repeating. A refusal carries header and a
// body that echoes the request's token, so a test sees whether the command
// ever shows it.
type fakeTokenAPI struct {
	srv   *httptest.Server
	mu    sync.Mutex
	paths []string
	auths []string
}

func newFakeTokenAPI(t *testing.T, header http.Header, statuses ...int) *fakeTokenAPI {
	t.Helper()
	a := &fakeTokenAPI{}
	a.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		a.mu.Lock()
		n := len(a.paths)
		a.paths = append(a.paths, r.URL.RequestURI())
		a.auths = append(a.auths, r.Header.Get("Authorization"))
		a.mu.Unlock()
		status := statuses[min(n, len(statuses)-1)]
		w.Header().Set("Content-Type", "application/json")
		if status == http.StatusOK {
			w.WriteHeader(status)
			fmt.Fprint(w, `{"total_count":0,"runners":[]}`)
			return
		}
		for k, v := range header {
			w.Header()[k] = v
		}
		w.WriteHeader(status)
		fmt.Fprintf(w, `{"message":"refused %s"}`, strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
	}))
	t.Cleanup(a.srv.Close)
	return a
}

// requests returns the request URIs and Authorization headers received.
func (a *fakeTokenAPI) requests() (paths, auths []string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return slices.Clone(a.paths), slices.Clone(a.auths)
}

// newTokenHarness is a harness on creds whose token side's REST client asks
// api.
//
// The configuration names the files creds was read from: github.token-file
// for a default token, in place of the default App, and token-file for
// each target token. Without a default token the default App stays, with
// the harness key unless creds has its own.
func newTokenHarness(api *fakeTokenAPI, creds config.Credentials) *deleteHarness {
	h := newDeleteHarness()
	if creds.DefaultToken != "" {
		h.cfg.GitHub = config.GitHub{TokenFile: "/token"}
	} else if creds.AppKey == nil {
		creds.AppKey = h.creds.AppKey
	}
	for i, token := range creds.TargetTokens {
		if token != "" && i < len(h.cfg.Targets) {
			h.cfg.Targets[i].TokenFile = fmt.Sprintf("/token-%d", i)
		}
	}
	h.creds = creds
	h.tokenAPI = api.srv.URL
	return h
}

// checkNotShown fails when stdout or stderr holds one of values. It never
// prints a value, since they may be token-shaped (0003 SPEC R277).
func checkNotShown(t *testing.T, h *deleteHarness, values ...string) {
	t.Helper()
	for i, v := range values {
		if strings.Contains(h.stdout.String(), v) {
			t.Errorf("stdout shows value %d", i)
		}
		if strings.Contains(h.stderr.String(), v) {
			t.Errorf("stderr shows value %d", i)
		}
	}
}

// checkToken fails unless the token side was made once with want. It
// never prints the tokens.
func checkToken(t *testing.T, h *deleteHarness, want string) {
	t.Helper()
	if len(h.tokens) != 1 || h.tokens[0] != want {
		t.Errorf("token side made %d times, or with another token; want once with the target's token", len(h.tokens))
	}
}

// TestDeleteScaleSetTokenTarget proves 0003 SPEC R281: for a token target,
// with its own token-file's or the default token, the command makes R279's
// call once with that token in place of the installation lookup, then
// deletes with the token's clients and exits 0.
func TestDeleteScaleSetTokenTarget(t *testing.T) {
	own := fixtureToken(t, "github_pat_")
	def := fixtureToken(t, "ghp_")
	tests := []struct {
		name  string
		creds config.Credentials
		want  string
	}{
		{"own token-file", config.Credentials{DefaultToken: def, TargetTokens: []string{"", own}}, own},
		{"default token", config.Credentials{DefaultToken: def, TargetTokens: []string{own, ""}}, def},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			api := newFakeTokenAPI(t, nil, http.StatusOK)
			h := newTokenHarness(api, tt.creds)
			h.gh.sets = []setAnswer{{set: &scaleset.RunnerScaleSet{ID: testSet}}, {}}
			h.addRunner(11, testPool+"-aaaaaaaa", testSet)

			checkExit(t, h.run(testRepoURL), 0)
			checkToken(t, h, tt.want)
			paths, auths := api.requests()
			checkCalls(t, paths, []string{"/repos/acme/widget/actions/runners?per_page=1"})
			if len(auths) != 1 || auths[0] != "Bearer "+tt.want {
				t.Errorf("the check did not carry the target's token as a bearer token")
			}
			if h.githubs != 0 {
				t.Errorf("App side made %d times, want 0", h.githubs)
			}
			checkCalls(t, h.app.calls, nil)
			checkCalls(t, h.tok.calls, []string{"CheckRepositoryAccess acme/widget", "Target acme/widget"})
			checkCalls(t, h.gh.calls, []string{
				"GetScaleSet", "ListRunners", "GetRunnerByName " + testPool + "-aaaaaaaa",
				"RemoveRunner 11", "DeleteScaleSet 7", "GetScaleSet",
			})
			checkContains(t, "stdout", h.stdout.String(), "scale set deleted: "+testPool+" (ID 7), runner registrations removed: 1")
			if h.logged() {
				t.Errorf("unexpected failure log: %q", h.stderr.String())
			}
			checkNotShown(t, h, own, def)
		})
	}
}

// Tokens that are not index-aligned with the targets stop the command
// before any GitHub call, so no target takes another's credential.
func TestDeleteScaleSetCredentialsMisaligned(t *testing.T) {
	tok := fixtureToken(t, "ghp_")
	for name, tokens := range map[string][]string{
		"shorter": {tok},
		"longer":  {"", tok, ""},
	} {
		t.Run(name, func(t *testing.T) {
			api := newFakeTokenAPI(t, nil, http.StatusOK)
			h := newTokenHarness(api, config.Credentials{TargetTokens: tokens})
			checkExit(t, h.run(testRepoURL), 1)
			if got, want := loggedErr(t, h.stderr.String()), "credentials do not match the configured targets"; got != want {
				t.Errorf("error = %q, want %q", got, want)
			}
			if h.githubs != 0 || len(h.tokens) != 0 || h.unlocks != 1 {
				t.Errorf("App side %d, token side %d, unlocks %d, want 0, 0 and 1", h.githubs, len(h.tokens), h.unlocks)
			}
			if paths, _ := api.requests(); len(paths) != 0 {
				t.Errorf("token API got %d requests, want 0", len(paths))
			}
			checkCalls(t, h.gh.calls, nil)
			checkNotShown(t, h, tok)
		})
	}
}

// An App target keeps 0002 SPEC R268 when another target has a token.
func TestDeleteScaleSetAppTargetBesideToken(t *testing.T) {
	tok := fixtureToken(t, "ghp_")
	api := newFakeTokenAPI(t, nil, http.StatusOK)
	h := newTokenHarness(api, config.Credentials{TargetTokens: []string{tok, ""}})
	h.gh.sets = []setAnswer{{set: &scaleset.RunnerScaleSet{ID: testSet}}, {}}
	checkExit(t, h.run(testRepoURL), 0)
	checkCalls(t, h.app.calls, []string{"Installation acme/widget", "Target acme/widget 99"})
	if len(h.tokens) != 0 {
		t.Errorf("token side made %d times, want 0", len(h.tokens))
	}
	if paths, _ := api.requests(); len(paths) != 0 {
		t.Errorf("token API got %d requests, want 0", len(paths))
	}
	checkNotShown(t, h, tok)
}

// TestDeleteScaleSetTokenRefused proves 0003 SPEC R281 with R269: when
// GitHub refuses the token at R279's call, stderr has R279's sentence,
// nothing is asked of the scale set or deleted, and the exit is 1.
func TestDeleteScaleSetTokenRefused(t *testing.T) {
	const cannotManage = "personal access token for repository acme/widget cannot manage its runners (%d): " +
		"a classic token needs the repo scope and admin access, " +
		"a fine-grained token needs Administration: Read and write on this repository"
	sso := "required; url=https://github.com/orgs/acme/sso?authorization_request=" + strings.Repeat("x", 12)
	tests := []struct {
		name   string
		status int
		header http.Header
		want   string
	}{
		{"401", http.StatusUnauthorized, nil,
			"personal access token for repository acme/widget was refused (401): it is invalid, expired or revoked"},
		{"403", http.StatusForbidden,
			http.Header{"X-Accepted-Github-Permissions": {"administration=write"}, "X-Github-Sso": {sso}},
			fmt.Sprintf(cannotManage, 403) + "; GitHub says the endpoint accepts administration=write" +
				"; an organization requires SAML SSO authorization of this token"},
		{"404", http.StatusNotFound, nil, fmt.Sprintf(cannotManage, 404)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tok := fixtureToken(t, "github_pat_")
			api := newFakeTokenAPI(t, tt.header, tt.status)
			h := newTokenHarness(api, config.Credentials{TargetTokens: []string{"", tok}})

			checkExit(t, h.run(testRepoURL), 1)
			if got := loggedErr(t, h.stderr.String()); got != tt.want {
				t.Errorf("error = %q\nwant    %q", got, tt.want)
			}
			if paths, _ := api.requests(); len(paths) != 1 {
				t.Errorf("token checks = %d, want 1 (a refusal is not retried)", len(paths))
			}
			checkCalls(t, h.tok.calls, []string{"CheckRepositoryAccess acme/widget"})
			checkCalls(t, h.gh.calls, nil)
			checkCalls(t, h.app.calls, nil)
			if h.stdout.Len() != 0 || h.dockers != 0 || h.unlocks != 1 {
				t.Errorf("stdout %q, docker clients %d, unlocks %d, want empty, 0 and 1", h.stdout.String(), h.dockers, h.unlocks)
			}
			checkNotShown(t, h, tok, sso)
		})
	}
}

// A transient answer to R279's call keeps the command's retry; one that
// never ends deletes nothing.
func TestDeleteScaleSetTokenCheckRetries(t *testing.T) {
	t.Run("transient then success", func(t *testing.T) {
		tok := fixtureToken(t, "ghp_")
		api := newFakeTokenAPI(t, nil, http.StatusServiceUnavailable, http.StatusOK)
		h := newTokenHarness(api, config.Credentials{DefaultToken: tok})
		h.gh.sets = []setAnswer{{set: &scaleset.RunnerScaleSet{ID: testSet}}, {}}
		checkExit(t, h.run(testRepoURL), 0)
		checkCalls(t, h.tok.calls, []string{
			"CheckRepositoryAccess acme/widget", "CheckRepositoryAccess acme/widget", "Target acme/widget",
		})
		if want := []time.Duration{time.Second}; !slices.Equal(h.clock.waits, want) {
			t.Errorf("waits = %v, want %v", h.clock.waits, want)
		}
		checkNotShown(t, h, tok)
	})
	t.Run("bounded", func(t *testing.T) {
		tok := fixtureToken(t, "ghp_")
		api := newFakeTokenAPI(t, nil, http.StatusServiceUnavailable)
		h := newTokenHarness(api, config.Credentials{DefaultToken: tok})
		checkExit(t, h.run(testRepoURL), 1)
		if paths, _ := api.requests(); len(paths) != deleteCallRetries+1 {
			t.Errorf("token checks = %d, want %d", len(paths), deleteCallRetries+1)
		}
		checkContains(t, "stderr", h.stderr.String(), "check the token's access to repository acme/widget", "503")
		checkCalls(t, h.gh.calls, nil)
		checkNotShown(t, h, tok)
	})
}

// TestDeleteScaleSetCredentialURL proves 0003 SPEC R314, R277: an argument
// with "@" or a token prefix and 20 more characters is refused before any
// other check, without quoting it, and nothing is asked of GitHub.
func TestDeleteScaleSetCredentialURL(t *testing.T) {
	classic := fixtureToken(t, "ghp_")
	fine := fixtureToken(t, "github_pat_")
	tests := map[string]struct {
		arg    string
		hidden []string
	}{
		"token before @":     {"https://" + classic + "@github.com/acme/widget", []string{classic}},
		"user before @":      {"https://someone@github.com/acme/widget", []string{"someone"}},
		"token as repo":      {"https://github.com/acme/" + classic, []string{classic}},
		"token as owner":     {"https://github.com/" + fine + "/widget", []string{fine}},
		"token, not a URL":   {fine, []string{fine}},
		"configured, plus @": {testRepoURL + "@x", nil},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			api := newFakeTokenAPI(t, nil, http.StatusOK)
			h := newTokenHarness(api, config.Credentials{DefaultToken: classic})
			checkExit(t, h.run(tt.arg), 1)
			if got, want := loggedErr(t, h.stderr.String()),
				"delete-scaleset: value contains a credential; set the repository URL only"; got != want {
				t.Errorf("error = %q\nwant    %q", got, want)
			}
			checkNotShown(t, h, append(tt.hidden, tt.arg)...)
			if h.githubs != 0 || len(h.tokens) != 0 {
				t.Errorf("App side made %d times, token side %d times, want 0", h.githubs, len(h.tokens))
			}
			if paths, _ := api.requests(); len(paths) != 0 {
				t.Errorf("token API got %d requests, want 0", len(paths))
			}
			checkCalls(t, h.app.calls, nil)
			checkCalls(t, h.gh.calls, nil)
		})
	}
}

// The real token side sends the token as a bearer token, registers it for
// redaction, makes the target's clients without a request, and prints no
// token under any fmt verb.
func TestGitHubToken(t *testing.T) {
	tok := fixtureToken(t, "github_pat_")
	api := newFakeTokenAPI(t, nil, http.StatusOK)
	var stdout, stderr bytes.Buffer
	e := newEnv(&stdout, &stderr)
	g, err := newGitHubToken(e, &fakeClock{}, api.srv.URL, http.DefaultClient, tok)
	if err != nil {
		t.Fatalf("newGitHubToken: %v", err)
	}
	if e.secrets.Redact(tok) == tok {
		t.Errorf("the token is not registered for redaction")
	}
	target := github.Target{Owner: "acme", Repo: "widget"}
	if _, err := g.CheckRepositoryAccess(context.Background(), target); err != nil {
		t.Fatalf("CheckRepositoryAccess: %v", err)
	}
	if _, auths := api.requests(); len(auths) != 1 || auths[0] != "Bearer "+tok {
		t.Errorf("the check did not carry the token as a bearer token")
	}
	gh, err := g.Target(target)
	if err != nil || gh == nil {
		t.Fatalf("Target: %v; want clients", err)
	}
	if paths, _ := api.requests(); len(paths) != 1 {
		t.Errorf("requests = %d, want 1 (making the clients asks nothing)", len(paths))
	}
	if s := fmt.Sprintf("%v %+v %#v %v %+v", g, g, g, gh, gh); strings.Contains(s, tok) {
		t.Errorf("a fmt verb prints the token")
	}
	if stdout.Len()+stderr.Len() != 0 {
		t.Errorf("output %q %q, want none", stdout.String(), stderr.String())
	}

	if _, err := newGitHubToken(e, &fakeClock{}, api.srv.URL, http.DefaultClient, ""); err == nil {
		t.Errorf("newGitHubToken with an empty token succeeded")
	}
}

// TestDeleteScaleSetTargetApp proves that a target with its own App is
// deleted with that App: the App side is made once with the target's client
// ID and key, the installation is looked up with it, and no token side is
// made, whether the default credential is a token or there is none.
func TestDeleteScaleSetTargetApp(t *testing.T) {
	key, err := initTestKey()
	if err != nil {
		t.Fatal(err)
	}
	def := fixtureToken(t, "ghp_")
	tests := []struct {
		name   string
		github config.GitHub
		creds  config.Credentials
	}{
		{"beside a default token", config.GitHub{TokenFile: "/token"},
			config.Credentials{DefaultToken: def, TargetTokens: []string{"", ""}, TargetAppKeys: []*rsa.PrivateKey{nil, key}}},
		{"without [github]", config.GitHub{},
			config.Credentials{TargetTokens: []string{"", ""}, TargetAppKeys: []*rsa.PrivateKey{key, key}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			api := newFakeTokenAPI(t, nil, http.StatusOK)
			h := newTokenHarness(api, tt.creds)
			h.cfg.GitHub = tt.github
			h.cfg.Targets[1].AppClientID, h.cfg.Targets[1].AppPrivateKeyFile = "own-app", "/own.pem"
			h.cfg.Docker.Host = "tcp://127.0.0.1:2376"
			h.gh.sets = []setAnswer{{set: &scaleset.RunnerScaleSet{ID: testSet}}, {}}
			h.addRunner(11, testPool+"-aaaaaaaa", testSet)

			checkExit(t, h.run(testRepoURL), 0)
			if h.githubs != 1 || !slices.Equal(h.appIDs, []string{"own-app"}) || h.appKeys[0] != key {
				t.Errorf("App side made %d times with %q, want once with the target's own App", h.githubs, h.appIDs)
			}
			checkCalls(t, h.app.calls, []string{"Installation acme/widget", "Target acme/widget 99"})
			if len(h.tokens) != 0 {
				t.Errorf("token side made %d times, want 0", len(h.tokens))
			}
			if paths, _ := api.requests(); len(paths) != 0 {
				t.Errorf("token API got %d requests, want 0", len(paths))
			}
			if !slices.Equal(h.dockerHosts, []string{"tcp://127.0.0.1:2376"}) {
				t.Errorf("Docker hosts = %q, want docker.host", h.dockerHosts)
			}
			checkContains(t, "stdout", h.stdout.String(), "scale set deleted: "+testPool+" (ID 7), runner registrations removed: 1")
			checkNotShown(t, h, def)
		})
	}

	t.Run("the default token target beside it", func(t *testing.T) {
		api := newFakeTokenAPI(t, nil, http.StatusOK)
		h := newTokenHarness(api, config.Credentials{DefaultToken: def, TargetTokens: []string{"", ""}, TargetAppKeys: []*rsa.PrivateKey{key, nil}})
		h.cfg.GitHub = config.GitHub{TokenFile: "/token"}
		h.cfg.Targets[0].AppClientID, h.cfg.Targets[0].AppPrivateKeyFile = "own-app", "/own.pem"
		h.gh.sets = []setAnswer{{set: &scaleset.RunnerScaleSet{ID: testSet}}, {}}
		checkExit(t, h.run(testRepoURL), 0)
		checkToken(t, h, def)
		if h.githubs != 0 {
			t.Errorf("App side made %d times, want 0", h.githubs)
		}
		checkNotShown(t, h, def)
	})

	t.Run("the default App beside it", func(t *testing.T) {
		other, err := rsa.GenerateKey(rand.Reader, 1024)
		if err != nil {
			t.Fatal(err)
		}
		h := newDeleteHarness()
		h.cfg.GitHub = config.GitHub{AppClientID: "default-app", AppPrivateKeyFile: "/key.pem"}
		h.cfg.Targets[0].AppClientID, h.cfg.Targets[0].AppPrivateKeyFile = "own-app", "/own.pem"
		h.creds = config.Credentials{AppKey: other, TargetTokens: []string{"", ""}, TargetAppKeys: []*rsa.PrivateKey{key, nil}}
		h.gh.sets = []setAnswer{{set: &scaleset.RunnerScaleSet{ID: testSet}}, {}}
		checkExit(t, h.run(testRepoURL), 0)
		if !slices.Equal(h.appIDs, []string{"default-app"}) || h.appKeys[0] != other {
			t.Errorf("App side made with %q, want the default App", h.appIDs)
		}
	})

	t.Run("own App without its key", func(t *testing.T) {
		h := newDeleteHarness()
		h.cfg.GitHub = config.GitHub{AppClientID: "default-app", AppPrivateKeyFile: "/key.pem"}
		h.cfg.Targets[1].AppClientID = "own-app"
		h.creds = config.Credentials{AppKey: key}
		checkExit(t, h.run(testRepoURL), 1)
		if got, want := loggedErr(t, h.stderr.String()), "targets[1]: the secret of targets.app-client-id and targets.app-private-key-file is not loaded"; got != want {
			t.Errorf("error = %q, want %q", got, want)
		}
		if h.githubs != 0 || len(h.tokens) != 0 {
			t.Errorf("App side %d, token side %d, want 0 and 0", h.githubs, len(h.tokens))
		}
	})

	t.Run("misaligned App keys", func(t *testing.T) {
		h := newDeleteHarness()
		h.creds = config.Credentials{TargetAppKeys: []*rsa.PrivateKey{key}}
		checkExit(t, h.run(testRepoURL), 1)
		if got, want := loggedErr(t, h.stderr.String()), "credentials do not match the configured targets"; got != want {
			t.Errorf("error = %q, want %q", got, want)
		}
		if h.githubs != 0 {
			t.Errorf("App side made %d times, want 0", h.githubs)
		}
	})
}
