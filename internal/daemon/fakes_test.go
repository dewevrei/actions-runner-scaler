package daemon

import (
	"bytes"
	"context"
	"crypto/rsa"
	"errors"
	"fmt"
	"log/slog"
	"net/http/httptrace"
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
	missing    map[string]bool
	memTotal   int64
	listErr    error
	resources  []docker.Resource
	startErr   func(ctx context.Context, name string) error
	started    []string
	removeErr  func(ctx context.Context, name string) error
	removeLeft []time.Duration
	exited     map[string]chan struct{}
	// containerErr fails RemoveContainer.
	containerErr error
}

func newFakeDocker(c *calls) *fakeDocker {
	return &fakeDocker{calls: c, memTotal: 64 << 30, missing: map[string]bool{}, exited: map[string]chan struct{}{}}
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

func (f *fakeDocker) ImageExists(ctx context.Context, ref string) (bool, error) {
	f.calls.add("docker.ImageExists %s", ref)
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.imageErr != nil {
		return false, f.imageErr
	}
	return !f.missing[ref], nil
}

func (f *fakeDocker) MemTotal(ctx context.Context) (int64, error) {
	f.calls.add("docker.MemTotal")
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.memTotal, nil
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

// fakeGitHub is GitHub and PoolGitHub.
type fakeGitHub struct {
	calls *calls

	mu           sync.Mutex
	newErr       error
	createErr    func(n int) error
	creates      int
	setErr       func(n int) error
	sets         int
	sessions     []*fakeSession
	removeErr    func(ctx context.Context, id int64) error
	removed      []int64
	removeLeft   []time.Duration
	jitErr       error
	createHook   func()
	nextID       int64
	systemInfo   scaleset.SystemInfo
	newSessionFn func() *fakeSession
}

func newFakeGitHub(c *calls) *fakeGitHub { return &fakeGitHub{calls: c, nextID: 100} }

func (f *fakeGitHub) Pool(p config.Pool) (PoolGitHub, error) {
	f.calls.add("github.Pool %s", p.Name)
	return f, nil
}

func (f *fakeGitHub) FindScaleSet(ctx context.Context, name string) (int, bool, error) {
	f.calls.add("github.FindScaleSet %s", name)
	return 0, false, nil
}

func (f *fakeGitHub) ListRepoRunners(ctx context.Context) ([]scaler.RepoRunner, error) {
	f.calls.add("github.ListRepoRunners")
	return nil, nil
}

func (f *fakeGitHub) GetRunnerByName(ctx context.Context, name string) (scaler.RegisteredRunner, bool, error) {
	f.calls.add("github.GetRunnerByName %s", name)
	return scaler.RegisteredRunner{}, false, nil
}

func (f *fakeGitHub) RemoveRunner(ctx context.Context, id int64) error {
	f.calls.add("github.RemoveRunner %d", id)
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

// GetRun waits until the check is stopped, so a job stays pending.
func (f *fakeGitHub) GetRun(ctx context.Context, owner, repo string, runID int64) (gate.Run, error) {
	f.calls.add("github.GetRun %d", runID)
	<-ctx.Done()
	return gate.Run{}, ctx.Err()
}

func (f *fakeGitHub) ListWorkflowRuns(ctx context.Context, owner, repo string, workflowID int64, createdFrom time.Time,
	visit func(page []gate.RunSummary, totalCount int) (stop bool)) error {
	f.calls.add("github.ListWorkflowRuns %d", workflowID)
	return errors.New("not used")
}

func (f *fakeGitHub) GetOrCreateScaleSet(ctx context.Context, name string) (int, error) {
	f.calls.add("github.GetOrCreateScaleSet %s", name)
	f.mu.Lock()
	f.sets++
	n, fn := f.sets, f.setErr
	f.mu.Unlock()
	if fn != nil {
		if err := fn(n); err != nil {
			return 0, err
		}
	}
	return 7, nil
}

func (f *fakeGitHub) SetSystemInfo(info scaleset.SystemInfo) {
	f.calls.add("github.SetSystemInfo")
	f.mu.Lock()
	defer f.mu.Unlock()
	f.systemInfo = info
}

func (f *fakeGitHub) GenerateJITConfig(ctx context.Context, scaleSetID int, name string) (scaler.JITRunner, error) {
	f.calls.add("github.GenerateJITConfig %d %s", scaleSetID, name)
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
	f.calls.add("github.CreateSession %d %s", scaleSetID, owner)
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
	s := newFakeSession(f.calls, n)
	f.mu.Lock()
	f.sessions = append(f.sessions, s)
	f.mu.Unlock()
	return s, nil
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
	msgs  chan msgResult

	mu         sync.Mutex
	stuck      chan struct{} // when set, GetMessage ignores ctx until closed
	unwritten  bool          // when set, a poll's request never reaches a connection
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
	return &fakeSession{calls: c, n: n, msgs: make(chan msgResult)}
}

// GetMessage acts as a healthy server unless unwritten is set: the poll's
// request reaches a connection (reported to an httptrace.ClientTrace on
// ctx), then it waits for a message, an error, or the end of ctx.
func (s *fakeSession) GetMessage(ctx context.Context, lastMessageID, maxCapacity int) (*scaleset.RunnerScaleSetMessage, error) {
	s.calls.add("session%d.GetMessage", s.n)
	s.mu.Lock()
	unwritten := s.unwritten
	s.mu.Unlock()
	if !unwritten {
		wroteRequest(ctx)
	}
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

// setUnwritten makes later polls fail to reach a connection.
func (s *fakeSession) setUnwritten() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.unwritten = true
}

// wroteRequest reports to the httptrace.ClientTrace on ctx, if any, that a
// request got a connection and was written, as net/http does.
func wroteRequest(ctx context.Context) {
	tr := httptrace.ContextClientTrace(ctx)
	if tr == nil {
		return
	}
	if tr.GetConn != nil {
		tr.GetConn("queue.example:443")
	}
	if tr.WroteRequest != nil {
		tr.WroteRequest(httptrace.WroteRequestInfo{})
	}
}

func (s *fakeSession) DeleteMessage(ctx context.Context, id int) error {
	s.calls.add("session%d.DeleteMessage %d", s.n, id)
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
	s.calls.add("session%d.AcquireJobs %v", s.n, ids)
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
	s.calls.add("session%d.Close", s.n)
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
		tb.Fatalf("session %d was not polled", s.n)
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

func testConfig() *config.Config {
	cfg := config.Default()
	cfg.GitHub = config.GitHub{AppClientID: "client", AppInstallationID: 1, AppPrivateKeyFile: "/key.pem"}
	cfg.Runner.Image = "sha256:runner"
	cfg.BuildKit.Image = "moby/buildkit:v0-rootless@sha256:bk"
	pool := config.DefaultPool()
	pool.Name = "pool"
	pool.RepoURL = "https://github.com/owner/repo"
	pool.AllowedWorkflows = []string{".github/workflows/deploy.yml"}
	cfg.Pools = []config.Pool{pool}
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
		LoadConfig: func() (*config.Config, *rsa.PrivateKey, error) {
			h.calls.add("config")
			h.mu.Lock()
			defer h.mu.Unlock()
			if h.cfgErr != nil {
				return nil, nil, h.cfgErr
			}
			return h.cfg, nil, nil
		},
		Docker: h.docker,
		CheckExposure: func(ctx context.Context) docker.Exposure {
			h.calls.add("exposure")
			h.mu.Lock()
			defer h.mu.Unlock()
			return h.exposure
		},
		NewGitHub: func(*config.Config, *rsa.PrivateKey) (GitHub, error) {
			h.calls.add("github.New")
			h.mu.Lock()
			fn := h.newGitHubFn
			h.mu.Unlock()
			if fn != nil {
				return fn()
			}
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

// seedRunner puts a runner into the table in state st as the scaler's
// transitions would.
func (h *harness) seedRunner(name string, id int64, st scaler.State) {
	h.t.Helper()
	tb := h.d.pool.table
	if err := tb.Add(name, h.clock.Now()); err != nil {
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
	h.d.pool.table.BeginExitAndRevoke(name)
	h.d.pool.table.MarkRemoved(name)
	h.docker.dropRunner(name)
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
