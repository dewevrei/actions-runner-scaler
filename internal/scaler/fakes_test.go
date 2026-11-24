package scaler

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/actions/scaleset"

	"github.com/dewevrei/actions-runner-scaler/internal/config"
	"github.com/dewevrei/actions-runner-scaler/internal/gate"
	"github.com/dewevrei/actions-runner-scaler/internal/retry"
)

var testEpoch = time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)

// fakeClock moves only when Advance is called. AfterFunc callbacks and
// After channels fire inside Advance, in deadline order, without the
// clock's lock held. Each AfterFunc callback runs in its own goroutine, as
// Clock documents, and Advance waits for it before firing the next timer,
// so tests stay deterministic.
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

func (c *fakeClock) AfterFunc(d time.Duration, f func()) Timer {
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

// Advance moves the clock by d and fires every timer that came due.
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
			c.now = end
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
		} else {
			done := make(chan struct{})
			go func() {
				defer close(done)
				next.f()
			}()
			<-done
		}
	}
}

// pending returns how many timers have not fired or been stopped.
func (c *fakeClock) pending() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, t := range c.timers {
		if !t.done {
			n++
		}
	}
	return n
}

// statusError is a call error carrying an HTTP status, as the github and
// docker adapters produce.
type statusError int

func (e statusError) Error() string   { return fmt.Sprintf("status %d", int(e)) }
func (e statusError) HTTPStatus() int { return int(e) }

var (
	errTransient   = errors.New("connection reset by peer")
	errPermanent   = statusError(403)
	errSessionLost = statusError(404)
)

func errRateLimited() error { return &retry.RateLimitError{} }

func errStillRunning() error {
	return fmt.Errorf("remove runner: %w", retry.ErrJobStillRunning)
}

// fakeScaleSet hands out runner IDs from 100 and records calls.
type fakeScaleSet struct {
	mu       sync.Mutex
	nextID   int64
	jitNames []string
	removed  []int64
	// removeLeft is the time left on each RemoveRunner call's context.
	removeLeft []time.Duration
	jitErr     func(name string) error
	removeErr  func(id int64) error
	// duringJIT runs inside GenerateJITConfig, before it returns.
	duringJIT func(name string)
}

func newFakeScaleSet() *fakeScaleSet { return &fakeScaleSet{nextID: 100} }

func (f *fakeScaleSet) GenerateJITConfig(ctx context.Context, name string) (JITRunner, error) {
	f.mu.Lock()
	f.jitNames = append(f.jitNames, name)
	errFn, during := f.jitErr, f.duringJIT
	f.mu.Unlock()
	if during != nil {
		during(name)
	}
	if errFn != nil {
		if err := errFn(name); err != nil {
			return JITRunner{}, err
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	id := f.nextID
	f.nextID++
	return JITRunner{ID: id, Config: NewJITConfig("jit-secret-" + name)}, nil
}

func (f *fakeScaleSet) RemoveRunner(ctx context.Context, id int64) error {
	f.mu.Lock()
	f.removed = append(f.removed, id)
	if d, ok := ctx.Deadline(); ok {
		f.removeLeft = append(f.removeLeft, time.Until(d))
	}
	errFn := f.removeErr
	f.mu.Unlock()
	if errFn != nil {
		return errFn(id)
	}
	return nil
}

func (f *fakeScaleSet) removeLeftTimes() []time.Duration {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.removeLeft)
}

// calls is the number of GitHub calls made so far.
func (f *fakeScaleSet) calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.jitNames) + len(f.removed)
}

func (f *fakeScaleSet) jitCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.jitNames)
}

func (f *fakeScaleSet) removedIDs() []int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.removed)
}

func (f *fakeScaleSet) setJITErr(fn func(string) error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.jitErr = fn
}

func (f *fakeScaleSet) setRemoveErr(fn func(int64) error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.removeErr = fn
}

// fakeContainers records starts and removals.
type fakeContainers struct {
	mu        sync.Mutex
	started   []string
	removed   []string
	stdin     []string
	startErr  func(name string) error
	removeErr func(name string) error
	// removeLeft is the time left on each RemoveResources call's context.
	removeLeft []time.Duration
	// runnerRemoved records RemoveContainer calls and runnerRemovedLeft the
	// time left on their contexts.
	runnerRemoved      []string
	runnerRemovedLeft  []time.Duration
	removeContainerErr func(name string) error
	// duringStart runs inside StartRunner with its context.
	duringStart func(ctx context.Context, name string)
	// duringRemove runs inside RemoveResources with its context; a non-nil
	// result is the call's error.
	duringRemove func(ctx context.Context, name string) error
}

func (f *fakeContainers) StartRunner(ctx context.Context, name string, jit JITConfig, token gate.Token) error {
	f.mu.Lock()
	f.started = append(f.started, name)
	f.stdin = append(f.stdin, jit.Reveal()+"\n"+token.Reveal()+"\n")
	errFn, during := f.startErr, f.duringStart
	f.mu.Unlock()
	if during != nil {
		during(ctx, name)
	}
	if errFn != nil {
		return errFn(name)
	}
	return nil
}

func (f *fakeContainers) RemoveResources(ctx context.Context, name string) error {
	f.mu.Lock()
	f.removed = append(f.removed, name)
	if d, ok := ctx.Deadline(); ok {
		f.removeLeft = append(f.removeLeft, time.Until(d))
	}
	errFn, during := f.removeErr, f.duringRemove
	f.mu.Unlock()
	if during != nil {
		if err := during(ctx, name); err != nil {
			return err
		}
	}
	if errFn != nil {
		return errFn(name)
	}
	return nil
}

func (f *fakeContainers) RemoveContainer(ctx context.Context, name string) error {
	f.mu.Lock()
	f.runnerRemoved = append(f.runnerRemoved, name)
	if d, ok := ctx.Deadline(); ok {
		f.runnerRemovedLeft = append(f.runnerRemovedLeft, time.Until(d))
	}
	errFn := f.removeContainerErr
	f.mu.Unlock()
	if errFn != nil {
		return errFn(name)
	}
	return nil
}

func (f *fakeContainers) runnerRemovedNames() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.runnerRemoved)
}

func (f *fakeContainers) removeLeftTimes() []time.Duration {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.removeLeft)
}

func (f *fakeContainers) runnerRemovedLeftTimes() []time.Duration {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.runnerRemovedLeft)
}

func (f *fakeContainers) setRemoveErr(fn func(string) error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.removeErr = fn
}

// calls is the number of Docker calls made so far.
func (f *fakeContainers) calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.started) + len(f.removed) + len(f.runnerRemoved)
}

func (f *fakeContainers) startedNames() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.started)
}

func (f *fakeContainers) removedNames() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.removed)
}

func (f *fakeContainers) setStartErr(fn func(string) error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.startErr = fn
}

// fakeJudge records judge starts and rejections.
type fakeJudge struct {
	mu       sync.Mutex
	started  []string
	jobs     []Job
	rejected []string
}

func (f *fakeJudge) Start(name string, job Job) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.started = append(f.started, name)
	f.jobs = append(f.jobs, job)
}

func (f *fakeJudge) Reject(name, reason string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rejected = append(f.rejected, name)
}

func (f *fakeJudge) startedNames() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.started)
}

func (f *fakeJudge) rejectedNames() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.rejected)
}

// fakeAcquirer records AcquireJobs calls.
type fakeAcquirer struct {
	mu    sync.Mutex
	calls [][]int64
	err   error
}

func (f *fakeAcquirer) AcquireJobs(ctx context.Context, ids []int64) ([]int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, slices.Clone(ids))
	if f.err != nil {
		return nil, f.err
	}
	return ids, nil
}

func (f *fakeAcquirer) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

// fakeTokens wraps the real generator and can fail.
type fakeTokens struct {
	gen *gate.TokenGenerator
	mu  sync.Mutex
	err error
}

func (f *fakeTokens) New() (gate.Token, error) {
	f.mu.Lock()
	err := f.err
	f.mu.Unlock()
	if err != nil {
		return gate.Token{}, err
	}
	return f.gen.New()
}

type recorded struct {
	where string
	err   error
}

type fakeRecorder struct {
	mu   sync.Mutex
	errs []recorded
}

func (f *fakeRecorder) Record(where string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.errs = append(f.errs, recorded{where, err})
}

func (f *fakeRecorder) last() recorded {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.errs) == 0 {
		return recorded{}
	}
	return f.errs[len(f.errs)-1]
}

func (f *fakeRecorder) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.errs)
}

// fakeSecrets counts registrations per value.
type fakeSecrets struct {
	mu     sync.Mutex
	counts map[string]int
	seen   []string
}

func (f *fakeSecrets) Register(v string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.counts[v]++
	f.seen = append(f.seen, v)
}

func (f *fakeSecrets) Unregister(v string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.counts[v]--
}

// Redact replaces the values registered now, as logging.Secrets does.
func (f *fakeSecrets) Redact(text string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	for v, c := range f.counts {
		if c > 0 && v != "" {
			text = strings.ReplaceAll(text, v, "<redacted>")
		}
	}
	return text
}

func (f *fakeSecrets) live() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.counts {
		n += c
	}
	return n
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

// harness wires a Scaler to fakes.
type harness struct {
	t       *testing.T
	clock   *fakeClock
	table   *Table
	gh      *fakeScaleSet
	docker  *fakeContainers
	judge   *fakeJudge
	acq     *fakeAcquirer
	tokens  *fakeTokens
	limits  *retry.NextAllowed
	errs    *fakeRecorder
	secrets *fakeSecrets
	logs    *syncBuffer
	s       *Scaler
	ss      *SessionScaler

	mu     sync.Mutex
	fatals []error
	busy   []string
}

func testPool(maxRunners, idle int) config.Pool {
	p := config.DefaultPool()
	p.Name = "pool"
	p.Max = maxRunners
	p.Idle = idle
	return p
}

func newHarness(t *testing.T, pool config.Pool) *harness {
	t.Helper()
	h := &harness{
		t:       t,
		clock:   newFakeClock(),
		gh:      newFakeScaleSet(),
		docker:  &fakeContainers{},
		judge:   &fakeJudge{},
		acq:     &fakeAcquirer{},
		tokens:  &fakeTokens{gen: gate.NewTokenGenerator(nil)},
		errs:    &fakeRecorder{},
		secrets: &fakeSecrets{counts: make(map[string]int)},
		logs:    &syncBuffer{},
	}
	h.table = NewTable(func(name string, at time.Time) {
		h.mu.Lock()
		defer h.mu.Unlock()
		h.busy = append(h.busy, name)
	}, h.secrets)
	limits, err := retry.NewNextAllowed(h.clock)
	if err != nil {
		t.Fatal(err)
	}
	h.limits = limits
	s, err := New(Config{
		Pool:       pool,
		Table:      h.table,
		Clock:      h.clock,
		ScaleSet:   h.gh,
		Containers: h.docker,
		Judge:      h.judge,
		Tokens:     h.tokens,
		Limits:     h.limits,
		Fatal: func(err error) {
			h.mu.Lock()
			defer h.mu.Unlock()
			h.fatals = append(h.fatals, err)
		},
		Logger:  slog.New(slog.NewJSONHandler(h.logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
		Errors:  h.errs,
		Secrets: h.secrets,
		Jitter:  func() float64 { return 0 },
	})
	if err != nil {
		t.Fatal(err)
	}
	h.s = s
	h.ss = s.ForSession(h.acq)
	return h
}

func (h *harness) scale(msg *scaleset.RunnerScaleSetMessage) error {
	h.t.Helper()
	return h.ss.Scale(context.Background(), msg)
}

func (h *harness) mustScale(msg *scaleset.RunnerScaleSetMessage) {
	h.t.Helper()
	if err := h.scale(msg); err != nil {
		h.t.Fatalf("Scale: %v", err)
	}
}

// scaleFinalizing runs Scale once finalize began or begins during it: the
// message must not be acknowledged (R47).
func (h *harness) scaleFinalizing(msg *scaleset.RunnerScaleSetMessage) {
	h.t.Helper()
	if err := h.scale(msg); !errors.Is(err, ErrFinalizing) {
		h.t.Fatalf("Scale = %v, want ErrFinalizing", err)
	}
}

func (h *harness) fatalCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.fatals)
}

func (h *harness) busyCalls() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Clone(h.busy)
}

func (h *harness) state(name string) State {
	h.t.Helper()
	info, ok := h.table.Get(name)
	if !ok {
		h.t.Fatalf("runner %s not in table", name)
	}
	return info.State
}

// gone reports whether the runner left the table (Table.MarkRemoved).
func (h *harness) gone(name string) bool {
	_, ok := h.table.Get(name)
	return !ok
}

// finalize begins finalize with a 45 s budget.
func (h *harness) finalize() {
	h.s.BeginFinalize(h.clock.Now().Add(45 * time.Second))
}

// seed puts a runner with GitHub ID id into the table in state st, the way
// the real transitions would. A zero st means a runner that was added and
// then removed, so it is no longer in the table.
func seed(t *testing.T, tb *Table, name string, id int64, st State, at time.Time) {
	t.Helper()
	if err := tb.Add(name, at); err != nil {
		t.Fatal(err)
	}
	tb.SetRunnerID(name, id)
	tok, err := gate.NewTokenGenerator(nil).New()
	if err != nil {
		t.Fatal(err)
	}
	tb.SetToken(name, tok)
	switch st {
	case StateStarting:
		return
	case StateIdle:
		tb.MarkStarted(name)
	case StateBusy:
		tb.MarkStarted(name)
		tb.MarkBusy(name, at)
	case StateDone:
		tb.MarkStarted(name)
		tb.RecordJobCompleted(name, "job-"+name, at)
	case StateExiting:
		tb.BeginExit(name)
	case 0:
		tb.BeginExit(name)
		tb.MarkRemoved(name)
		if _, ok := tb.Get(name); ok {
			t.Fatalf("seed %s: still in table after MarkRemoved", name)
		}
		return
	}
	if got, _ := tb.Get(name); got.State != st {
		t.Fatalf("seed %s: state %v, want %v", name, got.State, st)
	}
}

// Message builders.

func statsMsg(id, assigned int) *scaleset.RunnerScaleSetMessage {
	return &scaleset.RunnerScaleSetMessage{
		MessageID:  id,
		Statistics: &scaleset.RunnerScaleSetStatistic{TotalAssignedJobs: assigned},
	}
}

func withAvailable(m *scaleset.RunnerScaleSetMessage, reqIDs ...int64) *scaleset.RunnerScaleSetMessage {
	for _, id := range reqIDs {
		m.JobAvailableMessages = append(m.JobAvailableMessages, &scaleset.JobAvailable{
			JobMessageBase: scaleset.JobMessageBase{RunnerRequestID: id},
		})
	}
	return m
}

func withStarted(m *scaleset.RunnerScaleSetMessage, runner, jobID string) *scaleset.RunnerScaleSetMessage {
	m.JobStartedMessages = append(m.JobStartedMessages, &scaleset.JobStarted{
		RunnerName: runner,
		JobMessageBase: scaleset.JobMessageBase{
			JobID:          jobID,
			OwnerName:      "owner",
			RepositoryName: "repo",
			JobWorkflowRef: "owner/repo/.github/workflows/deploy.yml@refs/heads/main",
			EventName:      "push",
			WorkflowRunID:  42,
		},
	})
	return m
}

func withCompleted(m *scaleset.RunnerScaleSetMessage, runner, jobID string) *scaleset.RunnerScaleSetMessage {
	m.JobCompletedMessages = append(m.JobCompletedMessages, &scaleset.JobCompleted{
		RunnerName:     runner,
		Result:         "succeeded",
		JobMessageBase: scaleset.JobMessageBase{JobID: jobID},
	})
	return m
}

func hasPrefixAll(names []string, prefix string) bool {
	for _, n := range names {
		if !strings.HasPrefix(n, prefix) {
			return false
		}
	}
	return true
}
