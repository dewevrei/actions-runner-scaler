package daemon

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/actions/scaleset"

	"github.com/dewevrei/actions-runner-scaler/internal/config"
	"github.com/dewevrei/actions-runner-scaler/internal/docker"
	"github.com/dewevrei/actions-runner-scaler/internal/gate"
	"github.com/dewevrei/actions-runner-scaler/internal/github"
	"github.com/dewevrei/actions-runner-scaler/internal/logging"
	"github.com/dewevrei/actions-runner-scaler/internal/retry"
	"github.com/dewevrei/actions-runner-scaler/internal/scaler"
	"github.com/dewevrei/actions-runner-scaler/internal/status"
)

var testEpoch = time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)

// failsafe bounds every wait for the code under test, in wall time; the
// fake clock never moves by itself.
const failsafe = 10 * time.Second

// fakeClock moves only when Advance is called. Timers fire inside Advance
// in deadline order; each AfterFunc callback runs in its own goroutine and
// Advance waits for it before firing the next.
type fakeClock struct {
	mu     sync.Mutex
	now    time.Time
	timers []*fakeTimer
}

type fakeTimer struct {
	clock *fakeClock
	at    time.Time
	f     func()
	ch    chan time.Time
	done  bool
}

func newFakeClock() *fakeClock { return &fakeClock{now: testEpoch} }

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) After(d time.Duration) <-chan time.Time {
	ch := make(chan time.Time, 1)
	c.add(&fakeTimer{clock: c, ch: ch}, d)
	return ch
}

func (c *fakeClock) AfterFunc(d time.Duration, f func()) scaler.Timer {
	t := &fakeTimer{clock: c, f: f}
	c.add(t, d)
	return t
}

func (c *fakeClock) add(t *fakeTimer, d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	t.at = c.now.Add(d)
	c.timers = append(c.timers, t)
}

func (t *fakeTimer) Stop() bool {
	t.clock.mu.Lock()
	defer t.clock.mu.Unlock()
	if t.done {
		return false
	}
	t.done = true
	return true
}

// Advance moves the clock by d, firing every timer that comes due.
func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	end := c.now.Add(d)
	c.mu.Unlock()
	for {
		c.mu.Lock()
		var next *fakeTimer
		for _, t := range c.timers {
			if !t.done && !t.at.After(end) && (next == nil || t.at.Before(next.at)) {
				next = t
			}
		}
		if next == nil {
			if end.After(c.now) {
				c.now = end
			}
			c.timers = slices.DeleteFunc(c.timers, func(t *fakeTimer) bool { return t.done })
			c.mu.Unlock()
			return
		}
		next.done = true
		if next.at.After(c.now) {
			c.now = next.at
		}
		now := c.now
		c.mu.Unlock()
		if next.ch != nil {
			next.ch <- now
			continue
		}
		done := make(chan struct{})
		go func() {
			defer close(done)
			next.f()
		}()
		<-done
	}
}

// hasTimerAt reports whether a pending timer is due exactly at t.
func (c *fakeClock) hasTimerAt(t time.Time) bool { return c.timersAt(t) > 0 }

// timersAt counts the pending timers due exactly at t.
func (c *fakeClock) timersAt(t time.Time) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, tm := range c.timers {
		if !tm.done && tm.at.Equal(t) {
			n++
		}
	}
	return n
}

// waitTimersAt waits until n timers are due at t. Code that computes a wait
// from Now and then sets a timer must have set it before the test moves the
// clock, or the timer lands later than meant.
func (c *fakeClock) waitTimersAt(tb testing.TB, t time.Time, n int) {
	tb.Helper()
	waitFor(tb, fmt.Sprintf("%d timers at +%v", n, t.Sub(testEpoch)), func() bool { return c.timersAt(t) >= n })
}

// finalizeTimersAtA is how many timers finalize sets at deadline A: the
// scaler's deadline, deadline A for DeleteMessage, and stage 3's wait.
const finalizeTimersAtA = 3

// advanceTo waits until a timer is due at t, then moves the clock to t.
func (c *fakeClock) advanceTo(tb testing.TB, t time.Time) {
	tb.Helper()
	waitFor(tb, fmt.Sprintf("a timer at +%v", t.Sub(testEpoch)), func() bool { return c.hasTimerAt(t) })
	c.Advance(t.Sub(c.Now()))
}

// waitFor waits until cond holds.
func waitFor(tb testing.TB, what string, cond func() bool) {
	tb.Helper()
	deadline := time.Now().Add(failsafe)
	for !cond() {
		if time.Now().After(deadline) {
			tb.Fatalf("timed out waiting for %s", what)
		}
		runtime.Gosched()
	}
}

// statusError is a call error with an HTTP status, as the adapters return.
type statusError int

func (e statusError) Error() string   { return fmt.Sprintf("status %d", int(e)) }
func (e statusError) HTTPStatus() int { return int(e) }

func opErr(op retry.Op, err error) error { return &retry.OpError{Op: op, Err: err} }

// calls records every call the fakes get from the code under test, in
// order, so a test can tell that none came.
type calls struct {
	mu  sync.Mutex
	log []string
}

func (c *calls) add(format string, args ...any) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.log = append(c.log, fmt.Sprintf(format, args...))
}

func (c *calls) snapshot() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.log)
}

func (c *calls) count(prefix string) int {
	n := 0
	for _, l := range c.snapshot() {
		if strings.HasPrefix(l, prefix) {
			n++
		}
	}
	return n
}

// fakeDocker is the Docker side.
type fakeDocker struct {
	calls *calls

	mu         sync.Mutex
	pingErr    func(n int) error
	pingHook   func(ctx context.Context, n int) error
	pings      int
	imageErr   error
	inspectErr func(ref string, n int) error
	// inspectHook, when set, runs first in each lookup without the fake's
	// lock; true makes that lookup find nothing.
	inspectHook func(ref string) (missing bool)
	inspects    map[string]int
	missing     map[string]bool
	ids         map[string]string
	pullFn      func(ctx context.Context, ref string, n int) error
	// pullIDs, when it holds ref, is the ID a successful pull gives ref;
	// pullLeavesMissing makes a successful pull leave ref missing.
	pullIDs           map[string]string
	pullLeavesMissing bool
	pulls             map[string]int
	pulling           map[string]int
	maxPulling        map[string]int
	params            []docker.Params
	info              docker.Capacity
	infoErr           func(n int) error
	infos             int
	listErr           error
	resources         []docker.Resource
	startErr          func(ctx context.Context, name string) error
	started           []string
	removeErr         func(ctx context.Context, name string) error
	removeLeft        []time.Duration
	exited            map[string]chan struct{}
	// containerErr fails RemoveContainer.
	containerErr error
}

func newFakeDocker(c *calls) *fakeDocker {
	return &fakeDocker{calls: c, info: docker.Capacity{NCPU: 64, MemTotal: 64 << 30}, missing: map[string]bool{},
		ids: map[string]string{}, pullIDs: map[string]string{}, inspects: map[string]int{}, pulls: map[string]int{}, pulling: map[string]int{},
		maxPulling: map[string]int{}, exited: map[string]chan struct{}{}}
}

func (f *fakeDocker) Ping(ctx context.Context) error {
	f.calls.add("docker.Ping")
	f.mu.Lock()
	f.pings++
	n, fn, hook := f.pings, f.pingErr, f.pingHook
	f.mu.Unlock()
	if hook != nil {
		return hook(ctx, n)
	}
	if fn != nil {
		return fn(n)
	}
	return nil
}

// ImageInspect finds every image not in missing, with the ID in ids or
// "id-of-<ref>". inspectHook runs first; inspectErr, when set, gives a
// lookup's error (counted per ref from 1) before imageErr.
func (f *fakeDocker) ImageInspect(ctx context.Context, ref string) (docker.Image, bool, error) {
	f.calls.add("docker.ImageInspect %s", ref)
	f.mu.Lock()
	hook := f.inspectHook
	f.mu.Unlock()
	if hook != nil && hook(ref) {
		return docker.Image{}, false, nil
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.inspects[ref]++
	if f.inspectErr != nil {
		if err := f.inspectErr(ref, f.inspects[ref]); err != nil {
			return docker.Image{}, false, err
		}
	}
	if f.imageErr != nil {
		return docker.Image{}, false, f.imageErr
	}
	if f.missing[ref] {
		return docker.Image{}, false, nil
	}
	id := f.ids[ref]
	if id == "" {
		id = "id-of-" + ref
	}
	return docker.Image{ID: id, Architecture: "arm64"}, true, nil
}

// ImagePull calls pullFn, counted per ref from 1, when set; a pull that
// returns nil makes ref present. pulling counts the pulls in progress and
// maxPulling the most of one ref at once.
func (f *fakeDocker) ImagePull(ctx context.Context, ref string) error {
	f.calls.add("docker.ImagePull %s", ref)
	f.mu.Lock()
	f.pulls[ref]++
	f.pulling[ref]++
	f.maxPulling[ref] = max(f.maxPulling[ref], f.pulling[ref])
	n, fn := f.pulls[ref], f.pullFn
	f.mu.Unlock()
	var err error
	if fn != nil {
		err = fn(ctx, ref, n)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pulling[ref]--
	if err == nil && !f.pullLeavesMissing {
		delete(f.missing, ref)
		if id := f.pullIDs[ref]; id != "" {
			f.ids[ref] = id
		}
	}
	return err
}

// pullFailedErr is how docker.Client.ImagePull reports a failure that
// R320 sends to the error wait.
func pullFailedErr(cause error) error {
	return opErr(retry.OpImagePull, fmt.Errorf("%w: %v", retry.ErrPullFailed, cause))
}

// blockingPull returns a pull function that signals began, then waits for
// release or the end of ctx, as the client does: a deadline is a pull
// failure, a cancellation keeps context.Canceled.
func blockingPull(began chan<- string, release <-chan error) func(ctx context.Context, ref string, n int) error {
	return func(ctx context.Context, ref string, n int) error {
		if began != nil {
			began <- ref
		}
		select {
		case err := <-release:
			return err
		case <-ctx.Done():
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				return pullFailedErr(ctx.Err())
			}
			return opErr(retry.OpImagePull, ctx.Err())
		}
	}
}

// setImage makes ref present with id, or missing when id is empty.
func (f *fakeDocker) setImage(ref, id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if id == "" {
		f.missing[ref] = true
		return
	}
	delete(f.missing, ref)
	f.ids[ref] = id
}

func (f *fakeDocker) setPull(fn func(ctx context.Context, ref string, n int) error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pullFn = fn
}

// pullCount returns the pulls of ref begun so far and the most that ran at
// once.
func (f *fakeDocker) pullCount(ref string) (begun, most int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.pulls[ref], f.maxPulling[ref]
}

// startParams returns the Params of every StartRunner call, in order.
func (f *fakeDocker) startParams() []docker.Params {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.params)
}

func (f *fakeDocker) Info(ctx context.Context) (docker.Capacity, error) {
	f.calls.add("docker.Info")
	f.mu.Lock()
	defer f.mu.Unlock()
	f.infos++
	if f.infoErr != nil {
		if err := f.infoErr(f.infos); err != nil {
			return docker.Capacity{}, err
		}
	}
	return f.info, nil
}

func (f *fakeDocker) List(ctx context.Context) ([]docker.Resource, error) {
	f.calls.add("docker.List")
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.resources), f.listErr
}

func (f *fakeDocker) RemoveByID(ctx context.Context, kind, id string) error {
	f.calls.add("docker.RemoveByID %s %s", kind, id)
	return nil
}

func (f *fakeDocker) StartRunner(ctx context.Context, p docker.Params, in docker.Stdin) error {
	f.calls.add("docker.StartRunner %s", p.Name)
	f.mu.Lock()
	fn := f.startErr
	f.started = append(f.started, p.Name)
	f.params = append(f.params, p)
	f.mu.Unlock()
	if fn != nil {
		if err := fn(ctx, p.Name); err != nil {
			return err
		}
	}
	f.addRunner(p.Name)
	return nil
}

func (f *fakeDocker) RemoveResources(ctx context.Context, name string) error {
	f.calls.add("docker.RemoveResources %s", name)
	f.mu.Lock()
	if d, ok := ctx.Deadline(); ok {
		f.removeLeft = append(f.removeLeft, time.Until(d))
	}
	fn := f.removeErr
	f.mu.Unlock()
	if fn != nil {
		return fn(ctx, name)
	}
	return nil
}

func (f *fakeDocker) RemoveContainer(ctx context.Context, name string) error {
	f.calls.add("docker.RemoveContainer %s", name)
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.containerErr
}

func (f *fakeDocker) Wait(ctx context.Context, name string) (int64, error) {
	f.calls.add("docker.Wait %s", name)
	f.mu.Lock()
	ch := f.exited[name]
	f.mu.Unlock()
	if ch == nil {
		return 0, opErr(retry.OpContainerWait, statusError(404))
	}
	select {
	case <-ch:
		return 0, nil
	case <-ctx.Done():
		return 0, ctx.Err()
	}
}

func (f *fakeDocker) Processes(ctx context.Context, name string) ([]string, error) {
	f.calls.add("docker.Processes %s", name)
	return nil, nil
}

// addRunner adds runner name's three resources, running, as StartRunner
// would, so reconciliation and the watcher find them.
func (f *fakeDocker) addRunner(name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.resources = append(f.resources,
		docker.Resource{Kind: docker.KindRunner, ID: name + "-id", Name: name, RunnerName: name, State: "running"},
		docker.Resource{Kind: docker.KindBuildKit, ID: name + "-bk", Name: name + "-buildkit", RunnerName: name, State: "running"},
		docker.Resource{Kind: docker.KindNetwork, ID: name + "-net", Name: name + "-net", RunnerName: name})
	if f.exited[name] == nil {
		f.exited[name] = make(chan struct{})
	}
}

// dropRunner removes runner name's resources.
func (f *fakeDocker) dropRunner(name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.resources = slices.DeleteFunc(f.resources, func(r docker.Resource) bool { return r.RunnerName == name })
}

func (f *fakeDocker) startedNames() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.started)
}

func (f *fakeDocker) removeLefts() []time.Duration {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.removeLeft)
}

// fakeGitHub is GitHub and TargetGitHub. As GitHub it serves the
// installation lookup and the clients of each repository in repos from that
// repository's fake, and of any other repository from itself. Calls name a
// target by its repository name alone.
type fakeGitHub struct {
	calls *calls
	// prefix starts the calls it records as TargetGitHub: "github" for the
	// harness's fake, the repository name for one in repos; its sessions
	// record theirs as prefix-free "session<n>" or "<repo>-session<n>".
	prefix string
	// repos holds the fakes of the repositories a test gives their own.
	repos map[string]*fakeGitHub
	// setID is the scale set ID GetOrCreateScaleSet returns.
	setID int

	mu         sync.Mutex
	newErr     error
	createErr  func(n int) error
	creates    int
	setErr     func(n int) error
	sets       int
	sessions   []*fakeSession
	removeErr  func(ctx context.Context, id int64) error
	removed    []int64
	removeLeft []time.Duration
	jitErr     error
	// tokenKeys holds the credential key of each token target by its index
	// in the configuration; the other targets use the App.
	tokenKeys map[int]credentialKey
	// sourceErr fails the making of a token target's clients.
	sourceErr error
	// accessHook gives the error of the nth token check (0003 SPEC R279).
	accessHook func(ctx context.Context, n int) error
	accesses   int
	// instErr gives the error of the nth installation lookup; inst is what
	// a lookup returns.
	instErr  func(n int) error
	instHook func(ctx context.Context, n int) error
	insts    int
	inst     github.Installation
	// findSetErr fails R241's scale set lookup; with hasSet the lookup finds
	// the scale set setID, so R241 goes on to list the runners.
	findSetErr error
	hasSet     bool
	// listErr gives the error of the nth repository runner list (R241).
	listErr      func(n int) error
	lists        int
	createHook   func()
	nextID       int64
	systemInfo   scaleset.SystemInfo
	newSessionFn func() *fakeSession
}

// testInstallationID is the ID the fake installation lookup returns.
const testInstallationID = 99

func newFakeGitHub(c *calls) *fakeGitHub { return newRepoGitHub(c, "github") }

// newRepoGitHub returns a fake whose TargetGitHub calls start with prefix.
func newRepoGitHub(c *calls, prefix string) *fakeGitHub {
	return &fakeGitHub{
		calls:  c,
		prefix: prefix,
		repos:  map[string]*fakeGitHub{},
		setID:  7,
		nextID: 100,
		inst: github.Installation{
			ID:          testInstallationID,
			Permissions: map[string]string{"administration": "write"},
		},
	}
}

// repo returns the fake that serves repository name.
func (f *fakeGitHub) repo(name string) *fakeGitHub {
	f.mu.Lock()
	defer f.mu.Unlock()
	if o := f.repos[name]; o != nil {
		return o
	}
	return f
}

func (f *fakeGitHub) Installation(ctx context.Context, t github.Target) (github.Installation, error) {
	f.calls.add("github.Installation %s", t.Repo)
	o := f.repo(t.Repo)
	o.mu.Lock()
	o.insts++
	n, fn, hook, inst := o.insts, o.instErr, o.instHook, o.inst
	o.mu.Unlock()
	if hook != nil {
		if err := hook(ctx, n); err != nil {
			return github.Installation{}, err
		}
	}
	if fn != nil {
		if err := fn(n); err != nil {
			return github.Installation{}, err
		}
	}
	return inst, nil
}

func (f *fakeGitHub) Target(t github.Target, installationID int64) (TargetGitHub, error) {
	f.calls.add("github.Target %s %d", t.Repo, installationID)
	return f.repo(t.Repo), nil
}

// useCredentials keeps the keys of creds's tokens for the n configured
// targets, as NewGitHub picks each target's credential (0003 SPEC R273).
func (f *fakeGitHub) useCredentials(n int, creds config.Credentials) {
	keys := map[int]credentialKey{}
	for i := range n {
		if token := targetToken(creds, i); token != "" {
			keys[i] = tokenKey(token)
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tokenKeys = keys
}

// Auth serves a token target from the repository's fake as tokenSource and
// an App target as appSource.
func (f *fakeGitHub) Auth(t github.Target, index int) (targetAuth, error) {
	f.mu.Lock()
	key, ok := f.tokenKeys[index]
	f.mu.Unlock()
	if ok {
		newSource := func() (tokenSource, error) {
			o := f.repo(t.Repo)
			o.mu.Lock()
			err := o.sourceErr
			o.mu.Unlock()
			if err != nil {
				return nil, err
			}
			return f, nil
		}
		return &tokenAuth{t: t, key: key, newSource: newSource}, nil
	}
	return appAuth{src: f, t: t}, nil
}

func (f *fakeGitHub) CheckRepositoryAccess(ctx context.Context, t github.Target) (github.Access, error) {
	f.calls.add("github.CheckRepositoryAccess %s", t.Repo)
	o := f.repo(t.Repo)
	o.mu.Lock()
	o.accesses++
	n, fn := o.accesses, o.accessHook
	o.mu.Unlock()
	if fn != nil {
		if err := fn(ctx, n); err != nil {
			return github.Access{}, err
		}
	}
	return github.Access{Status: 200}, nil
}

func (f *fakeGitHub) TokenTarget(t github.Target) (TargetGitHub, error) {
	f.calls.add("github.TokenTarget %s", t.Repo)
	return f.repo(t.Repo), nil
}

func (f *fakeGitHub) FindScaleSet(ctx context.Context, groupID int, name string) (int, bool, error) {
	f.calls.add("%s.FindScaleSet %d %s", f.prefix, groupID, name)
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.findSetErr != nil || !f.hasSet {
		return 0, false, f.findSetErr
	}
	return f.setID, true, nil
}

func (f *fakeGitHub) ListRunners(ctx context.Context) ([]scaler.ListedRunner, error) {
	f.calls.add("%s.ListRunners", f.prefix)
	f.mu.Lock()
	f.lists++
	n, fn := f.lists, f.listErr
	f.mu.Unlock()
	if fn != nil {
		if err := fn(n); err != nil {
			return nil, err
		}
	}
	return nil, nil
}

func (f *fakeGitHub) FindRunner(ctx context.Context, name string) (scaler.ListedRunner, bool, error) {
	f.calls.add("%s.FindRunner %s", f.prefix, name)
	return scaler.ListedRunner{}, false, nil
}

func (f *fakeGitHub) GetRunnerByName(ctx context.Context, name string) (scaler.RegisteredRunner, bool, error) {
	f.calls.add("%s.GetRunnerByName %s", f.prefix, name)
	return scaler.RegisteredRunner{}, false, nil
}

func (f *fakeGitHub) RemoveRunner(ctx context.Context, id int64) error {
	f.calls.add("%s.RemoveRunner %d", f.prefix, id)
	f.mu.Lock()
	f.removed = append(f.removed, id)
	if d, ok := ctx.Deadline(); ok {
		f.removeLeft = append(f.removeLeft, time.Until(d))
	}
	fn := f.removeErr
	f.mu.Unlock()
	if fn != nil {
		return fn(ctx, id)
	}
	return nil
}

func (f *fakeGitHub) GetOrCreateScaleSet(ctx context.Context, groupID int, name string) (int, error) {
	f.calls.add("%s.GetOrCreateScaleSet %d %s", f.prefix, groupID, name)
	f.mu.Lock()
	f.sets++
	n, fn, id := f.sets, f.setErr, f.setID
	f.mu.Unlock()
	if fn != nil {
		if err := fn(n); err != nil {
			return 0, err
		}
	}
	return id, nil
}

func (f *fakeGitHub) SetSystemInfo(info scaleset.SystemInfo) {
	f.calls.add("%s.SetSystemInfo", f.prefix)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.systemInfo = info
}

func (f *fakeGitHub) GenerateJITConfig(ctx context.Context, scaleSetID int, name string) (scaler.JITRunner, error) {
	f.calls.add("%s.GenerateJITConfig %d %s", f.prefix, scaleSetID, name)
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.jitErr != nil {
		return scaler.JITRunner{}, f.jitErr
	}
	id := f.nextID
	f.nextID++
	return scaler.JITRunner{ID: id, Config: scaler.NewJITConfig("jit-secret-" + name)}, nil
}

func (f *fakeGitHub) CreateSession(ctx context.Context, scaleSetID int, owner string) (Session, error) {
	f.calls.add("%s.CreateSession %d %s", f.prefix, scaleSetID, owner)
	f.mu.Lock()
	f.creates++
	n, fn, hook := f.creates, f.createErr, f.createHook
	f.mu.Unlock()
	if hook != nil {
		hook()
	}
	if fn != nil {
		if err := fn(n); err != nil {
			return nil, err
		}
	}
	s := newNamedSession(f.calls, f.sessionName(n), n)
	f.mu.Lock()
	f.sessions = append(f.sessions, s)
	f.mu.Unlock()
	return s, nil
}

// sessionName names session n in the calls log.
func (f *fakeGitHub) sessionName(n int) string {
	if f.prefix == "github" {
		return fmt.Sprintf("session%d", n)
	}
	return fmt.Sprintf("%s-session%d", f.prefix, n)
}

// polled waits until session i exists and was polled, and returns it.
func (f *fakeGitHub) polled(tb testing.TB, i int) *fakeSession {
	tb.Helper()
	waitFor(tb, f.sessionName(i+1)+" polled", func() bool {
		s := f.session(i)
		if s == nil {
			return false
		}
		polls, _, _, _ := s.snapshot()
		return polls > 0
	})
	return f.session(i)
}

func (f *fakeGitHub) session(i int) *fakeSession {
	f.mu.Lock()
	defer f.mu.Unlock()
	if i >= len(f.sessions) {
		return nil
	}
	return f.sessions[i]
}

func (f *fakeGitHub) sessionCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.sessions)
}

func (f *fakeGitHub) removedIDs() []int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.removed)
}

func (f *fakeGitHub) removeLefts() []time.Duration {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.removeLeft)
}

type msgResult struct {
	msg *scaleset.RunnerScaleSetMessage
	err error
}

// fakeSession is one message session. GetMessage returns what the test
// sends on msgs, or ends with ctx unless stuck is set.
type fakeSession struct {
	calls *calls
	n     int
	name  string
	msgs  chan msgResult

	mu         sync.Mutex
	stuck      chan struct{} // when set, GetMessage ignores ctx until closed
	maxCaps    []int
	polls      int
	deleted    []int
	deleteLeft []time.Duration
	deleteFn   func(ctx context.Context, id int) error
	acquireErr func(n int) error
	acquires   int
	closed     int
	closeLeft  []time.Duration
	closeFn    func(ctx context.Context) error
}

func newFakeSession(c *calls, n int) *fakeSession {
	return newNamedSession(c, fmt.Sprintf("session%d", n), n)
}

// newNamedSession returns session n, recording its calls under name.
func newNamedSession(c *calls, name string, n int) *fakeSession {
	return &fakeSession{calls: c, n: n, name: name, msgs: make(chan msgResult)}
}

// GetMessage acts as a healthy server: it waits for a message, an error, or
// the end of ctx.
func (s *fakeSession) GetMessage(ctx context.Context, lastMessageID, maxCapacity int) (*scaleset.RunnerScaleSetMessage, error) {
	s.calls.add("%s.GetMessage", s.name)
	s.mu.Lock()
	s.maxCaps = append(s.maxCaps, maxCapacity)
	s.polls++
	stuck := s.stuck
	s.mu.Unlock()
	if stuck != nil {
		select {
		case r := <-s.msgs:
			return r.msg, r.err
		case <-stuck:
			return nil, opErr(retry.OpGetMessage, context.Canceled)
		}
	}
	select {
	case r := <-s.msgs:
		return r.msg, r.err
	case <-ctx.Done():
		return nil, opErr(retry.OpGetMessage, ctx.Err())
	}
}

func (s *fakeSession) DeleteMessage(ctx context.Context, id int) error {
	s.calls.add("%s.DeleteMessage %d", s.name, id)
	s.mu.Lock()
	s.deleted = append(s.deleted, id)
	if d, ok := ctx.Deadline(); ok {
		s.deleteLeft = append(s.deleteLeft, time.Until(d))
	}
	fn := s.deleteFn
	s.mu.Unlock()
	if fn != nil {
		return fn(ctx, id)
	}
	return nil
}

func (s *fakeSession) AcquireJobs(ctx context.Context, ids []int64) ([]int64, error) {
	s.calls.add("%s.AcquireJobs %v", s.name, ids)
	s.mu.Lock()
	s.acquires++
	n, fn := s.acquires, s.acquireErr
	s.mu.Unlock()
	if fn != nil {
		if err := fn(n); err != nil {
			return nil, err
		}
	}
	return ids, nil
}

// Session returns a session with an ID and statistics, which
// listener.Run requires.
func (s *fakeSession) Session() scaleset.RunnerScaleSetSession {
	out := scaleset.RunnerScaleSetSession{Statistics: &scaleset.RunnerScaleSetStatistic{}}
	out.SessionID[0] = byte(s.n)
	out.SessionID[15] = 1
	return out
}

func (s *fakeSession) Close(ctx context.Context) error {
	s.calls.add("%s.Close", s.name)
	s.mu.Lock()
	s.closed++
	if d, ok := ctx.Deadline(); ok {
		s.closeLeft = append(s.closeLeft, time.Until(d))
	}
	fn := s.closeFn
	s.mu.Unlock()
	if fn != nil {
		return fn(ctx)
	}
	return nil
}

// send delivers one GetMessage result, failing the test if the listener
// does not poll.
func (s *fakeSession) send(tb testing.TB, msg *scaleset.RunnerScaleSetMessage, err error) {
	tb.Helper()
	select {
	case s.msgs <- msgResult{msg, err}:
	case <-time.After(failsafe):
		tb.Fatalf("%s was not polled", s.name)
	}
}

func (s *fakeSession) snapshot() (polls int, caps []int, deleted []int, closed int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.polls, slices.Clone(s.maxCaps), slices.Clone(s.deleted), s.closed
}

// fakeServer is the status socket and the job check server.
type fakeServer struct {
	calls     *calls
	name      string
	listenErr error
	stopped   chan struct{}
	once      sync.Once
	// serveErr ends Serve with an error.
	serveErr chan error

	mu           sync.Mutex
	shutdownFn   func(ctx context.Context) error
	shutdownLeft []time.Duration
	shutdowns    int
}

func newFakeServer(c *calls, name string) *fakeServer {
	return &fakeServer{calls: c, name: name, stopped: make(chan struct{}), serveErr: make(chan error, 1)}
}

func (s *fakeServer) Listen() error {
	s.calls.add("%s.Listen", s.name)
	return s.listenErr
}

func (s *fakeServer) Serve() error {
	select {
	case <-s.stopped:
		return nil
	case err := <-s.serveErr:
		return err
	}
}

func (s *fakeServer) Shutdown(ctx context.Context) error {
	s.calls.add("%s.Shutdown", s.name)
	s.mu.Lock()
	s.shutdowns++
	if d, ok := ctx.Deadline(); ok {
		s.shutdownLeft = append(s.shutdownLeft, time.Until(d))
	}
	fn := s.shutdownFn
	s.mu.Unlock()
	defer s.once.Do(func() { close(s.stopped) })
	if fn != nil {
		return fn(ctx)
	}
	return nil
}

// syncBuffer is a log sink safe for concurrent writes.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// harness runs a Daemon on fakes.
type harness struct {
	t       *testing.T
	clock   *fakeClock
	calls   *calls
	docker  *fakeDocker
	gh      *fakeGitHub
	status  *fakeServer
	jobs    *fakeServer
	sigs    chan os.Signal
	logs    *syncBuffer
	secrets *logging.Secrets
	cfg     *config.Config
	// creds is what LoadConfig returns with cfg; the fake NewGitHub keeps
	// only its tokens' keys.
	creds config.Credentials

	mu          sync.Mutex
	cfgErr      error
	lockErr     error
	listenErr   error
	exposure    docker.Exposure
	rep         status.Reporter
	unlocked    bool
	jobAuth     gate.Authenticator
	newJobErr   error
	newGitHubFn func() (GitHub, error)

	d    *Daemon
	done chan error
}

// testMaxRunners is the max-runners of every test target: the tests were
// written for the first version's default of 2 (0002 SPEC R235 made it 1).
const testMaxRunners = 2

// testTargetURL is the one target of testConfig.
const testTargetURL = "https://github.com/acme/widget"

// The images of testConfig: runner.image a local image ID, which is never
// pulled, and buildkit.image a reference with a digest (0003 SPEC R318).
var (
	testRunnerImage   = "sha256:" + strings.Repeat("1", 64)
	testBuildKitImage = "moby/buildkit:v0-rootless@sha256:" + strings.Repeat("b", 64)
)

func testConfig() *config.Config {
	cfg := config.Default()
	cfg.GitHub = config.GitHub{AppClientID: "client", AppPrivateKeyFile: "/key.pem"}
	cfg.Runner.Image = testRunnerImage
	cfg.BuildKit.Image = testBuildKitImage
	cfg.Scaling.Label = "pool"
	target := config.DefaultTarget()
	target.URL = testTargetURL
	target.MaxRunners = testMaxRunners
	cfg.Targets = []config.Target{target}
	return &cfg
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	c := &calls{}
	h := &harness{
		t:       t,
		clock:   newFakeClock(),
		calls:   c,
		docker:  newFakeDocker(c),
		gh:      newFakeGitHub(c),
		status:  newFakeServer(c, "status"),
		jobs:    newFakeServer(c, "jobs"),
		sigs:    make(chan os.Signal, 4),
		logs:    &syncBuffer{},
		secrets: logging.NewSecrets(),
		cfg:     testConfig(),
	}
	return h
}

func (h *harness) config() Config {
	logger := slog.New(logging.NewRedactingHandler(
		slog.NewJSONHandler(h.logs, &slog.HandlerOptions{Level: slog.LevelDebug}), h.secrets))
	return Config{
		Clock:   h.clock,
		Signals: h.sigs,
		Logger:  logger,
		Secrets: h.secrets,
		Lock: func() (func(), error) {
			h.calls.add("lock")
			h.mu.Lock()
			defer h.mu.Unlock()
			if h.lockErr != nil {
				return nil, h.lockErr
			}
			return func() {
				h.mu.Lock()
				defer h.mu.Unlock()
				h.unlocked = true
			}, nil
		},
		ListenStatus: func(rep status.Reporter) (StatusServer, error) {
			h.calls.add("status.Listen")
			h.mu.Lock()
			defer h.mu.Unlock()
			if h.listenErr != nil {
				return nil, h.listenErr
			}
			h.rep = rep
			return h.status, nil
		},
		LoadConfig: func() (*config.Config, config.Credentials, error) {
			h.calls.add("config")
			h.mu.Lock()
			defer h.mu.Unlock()
			if h.cfgErr != nil {
				return nil, config.Credentials{}, h.cfgErr
			}
			return h.cfg, h.creds, nil
		},
		Docker: h.docker,
		CheckExposure: func(ctx context.Context) docker.Exposure {
			h.calls.add("exposure")
			h.mu.Lock()
			defer h.mu.Unlock()
			return h.exposure
		},
		NewGitHub: func(cfg *config.Config, creds config.Credentials) (GitHub, error) {
			h.calls.add("github.New")
			h.mu.Lock()
			fn := h.newGitHubFn
			h.mu.Unlock()
			if fn != nil {
				return fn()
			}
			h.gh.useCredentials(len(cfg.Targets), creds)
			return h.gh, nil
		},
		NewJobServer: func(addr string, auth gate.Authenticator) (JobServer, error) {
			h.calls.add("jobs.New %s", addr)
			h.mu.Lock()
			defer h.mu.Unlock()
			h.jobAuth = auth
			if h.newJobErr != nil {
				return nil, h.newJobErr
			}
			return h.jobs, nil
		},
		Owner:      "host",
		SystemInfo: scaleset.SystemInfo{System: "test"},
		Jitter:     func() float64 { return 0 },
	}
}

// start runs the daemon in a goroutine.
func (h *harness) start() {
	h.t.Helper()
	d, err := New(h.config())
	if err != nil {
		h.t.Fatal(err)
	}
	h.d = d
	h.done = make(chan error, 1)
	go func() { h.done <- d.Run(context.Background()) }()
	h.t.Cleanup(func() {
		if h.t.Failed() {
			h.t.Log(h.logs.String())
		}
		select {
		case <-h.done:
		default:
			// End a daemon a failed test left running.
			h.sigs <- os.Interrupt
			h.sigs <- os.Interrupt
			deadline := time.Now().Add(failsafe)
			for time.Now().Before(deadline) {
				select {
				case <-h.done:
					return
				default:
					h.clock.Advance(time.Minute)
					runtime.Gosched()
				}
			}
			h.t.Error("daemon still running at the end of the test")
		}
	})
}

func (h *harness) signal() { h.sigs <- os.Interrupt }

// result waits for Run to return.
func (h *harness) result() error {
	h.t.Helper()
	select {
	case err := <-h.done:
		h.done <- err
		return err
	case <-time.After(failsafe):
		h.t.Fatal("Run did not return")
		return nil
	}
}

func (h *harness) returned() bool {
	select {
	case err := <-h.done:
		h.done <- err
		return true
	default:
		return false
	}
}

func (h *harness) report() status.Report {
	h.mu.Lock()
	rep := h.rep
	h.mu.Unlock()
	if rep == nil {
		return status.Report{}
	}
	return rep.Report()
}

func (h *harness) waitState(s status.State) {
	h.t.Helper()
	waitFor(h.t, "state "+string(s), func() bool { return h.report().State == s })
}

// running starts the daemon and waits for the first session's first poll.
func (h *harness) running() *fakeSession {
	h.t.Helper()
	h.start()
	h.waitState(status.Running)
	waitFor(h.t, "first poll", func() bool {
		s := h.gh.session(0)
		if s == nil {
			return false
		}
		polls, _, _, _ := s.snapshot()
		return polls > 0
	})
	return h.gh.session(0)
}

// seedRunner puts a runner of the first target into the table in state st
// as the scaler's transitions would. It bypasses max-runners; the server
// limit still holds.
func (h *harness) seedRunner(name string, id int64, st scaler.State) {
	h.t.Helper()
	h.seedRunnerOf(h.targetRuns()[0], name, id, st)
}

// seedRunnerOf is seedRunner for target tr.
func (h *harness) seedRunnerOf(tr *targetRun, name string, id int64, st scaler.State) {
	h.t.Helper()
	tb := h.d.table
	if err := tb.Add(name, tr.t.Key(), tb.ServerLimit(), h.clock.Now()); err != nil {
		h.t.Fatal(err)
	}
	tb.SetRunnerID(name, id)
	if st != scaler.StateStarting {
		h.docker.addRunner(name)
	}
	switch st {
	case scaler.StateStarting:
		return
	case scaler.StateIdle:
		tb.MarkStarted(name)
	case scaler.StateBusy:
		tb.MarkStarted(name)
		tb.MarkBusy(name, h.clock.Now())
	case scaler.StateDone:
		tb.MarkStarted(name)
		tb.RecordJobCompleted(name, "job", h.clock.Now())
	}
}

// endRunner takes a runner out of the table as its watcher would.
func (h *harness) endRunner(name string) {
	h.d.table.BeginExitAndRevoke(name)
	h.d.table.MarkRemoved(name)
	h.docker.dropRunner(name)
}

// targetRuns waits until start-up built every target and returns them in
// config order.
func (h *harness) targetRuns() []*targetRun {
	h.t.Helper()
	waitFor(h.t, "every target built", func() bool { return len(h.report().Targets) == len(h.cfg.Targets) })
	return h.d.targets
}

// useTargets replaces the configured targets with one repository of owner
// acme per name, in that order, each served by its own fake, which it
// returns.
func (h *harness) useTargets(names ...string) []*fakeGitHub {
	h.t.Helper()
	h.cfg.Targets = nil
	fakes := make([]*fakeGitHub, len(names))
	for i, name := range names {
		tc := config.DefaultTarget()
		tc.URL = "https://github.com/acme/" + name
		tc.MaxRunners = testMaxRunners
		h.cfg.Targets = append(h.cfg.Targets, tc)
		fakes[i] = newRepoGitHub(h.calls, name)
		fakes[i].setID = 7 + i
		h.gh.repos[name] = fakes[i]
	}
	return fakes
}

// waitTargetState waits until target tr is in state s.
func (h *harness) waitTargetState(tr *targetRun, s targetState) {
	h.t.Helper()
	waitFor(h.t, tr.t.Key()+" "+s.String(), func() bool { return tr.currentState() == s })
}

// setInstErr replaces the installation lookup's error function while the
// daemon runs.
func (f *fakeGitHub) setInstErr(fn func(n int) error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.instErr = fn
}

// setInstallationID changes the installation ID the lookups of the
// repository f serves return, while the daemon runs.
func (f *fakeGitHub) setInstallationID(id int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.inst.ID = id
}

// setAccessHook replaces the token check's hook while the daemon runs.
func (f *fakeGitHub) setAccessHook(fn func(ctx context.Context, n int) error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.accessHook = fn
}

// tokenChecks counts the token checks of the repository f serves.
func (f *fakeGitHub) tokenChecks() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.accesses
}

// useTokens gives the configured targets the tokens, index-aligned; an
// empty token leaves that target on the App (0003 SPEC R273).
func (h *harness) useTokens(tokens ...string) {
	h.t.Helper()
	if len(tokens) != len(h.cfg.Targets) {
		h.t.Fatalf("%d tokens for %d targets", len(tokens), len(h.cfg.Targets))
	}
	h.creds = config.Credentials{TargetTokens: tokens}
}

// installations counts the installation lookups of the repository f
// serves.
func (f *fakeGitHub) installations() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.insts
}

// assertNoCallsFor checks that nothing calls the fakes while the clock
// moves a long way (R47).
func (h *harness) assertNoCallsFor(d time.Duration) {
	h.t.Helper()
	before := h.calls.snapshot()
	for step := time.Duration(0); step < d; step += 30 * time.Second {
		h.clock.Advance(30 * time.Second)
	}
	// Let any goroutine a timer woke run.
	for range 100 {
		runtime.Gosched()
	}
	if after := h.calls.snapshot(); len(after) != len(before) {
		h.t.Fatalf("calls after the error wait began: %v", after[len(before):])
	}
}
