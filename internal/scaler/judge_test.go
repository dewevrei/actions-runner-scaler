package scaler

import (
	"context"
	"errors"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dewevrei/actions-runner-scaler/internal/gate"
	"github.com/dewevrei/actions-runner-scaler/internal/retry"
)

// fakeRuns is the job check's REST lookups.
type fakeRuns struct {
	mu sync.Mutex
	// run and getErr are GetRun's answer.
	run    gate.Run
	getErr error
	// pages are handed to visit, each with total; listErr is returned after
	// them.
	pages   [][]gate.RunSummary
	total   int
	listErr error
	// started, if set, is closed when GetRun is first called; GetRun then
	// waits for its context to end.
	started chan struct{}
	gets    []int64
	lists   []listCall
	// left is the time left on each call's context, GetRun and
	// ListWorkflowRuns in call order; -1 for a context without a deadline.
	left []time.Duration
}

// noteLeft records the time left on ctx; f.mu must be held.
func (f *fakeRuns) noteLeftLocked(ctx context.Context) {
	left := time.Duration(-1)
	if d, ok := ctx.Deadline(); ok {
		left = time.Until(d)
	}
	f.left = append(f.left, left)
}

func (f *fakeRuns) leftTimes() []time.Duration {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.left)
}

type listCall struct {
	owner, repo string
	workflowID  int64
	createdFrom time.Time
}

func (f *fakeRuns) GetRun(ctx context.Context, owner, repo string, runID int64) (gate.Run, error) {
	f.mu.Lock()
	f.gets = append(f.gets, runID)
	f.noteLeftLocked(ctx)
	run, err, started := f.run, f.getErr, f.started
	f.started = nil
	f.mu.Unlock()
	if started != nil {
		close(started)
		<-ctx.Done()
		return gate.Run{}, ctx.Err()
	}
	if owner != "owner" || repo != "repo" {
		return gate.Run{}, errors.New("lookup outside the pool's repository")
	}
	return run, err
}

func (f *fakeRuns) ListWorkflowRuns(ctx context.Context, owner, repo string, workflowID int64, createdFrom time.Time,
	visit func([]gate.RunSummary, int) bool) error {
	f.mu.Lock()
	f.lists = append(f.lists, listCall{owner, repo, workflowID, createdFrom})
	f.noteLeftLocked(ctx)
	pages, total, err := slices.Clone(f.pages), f.total, f.listErr
	f.mu.Unlock()
	for _, p := range pages {
		if visit(slices.Clone(p), total) {
			break
		}
	}
	return err
}

func (f *fakeRuns) getCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.gets)
}

func (f *fakeRuns) listCalls() []listCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.lists)
}

func (f *fakeRuns) setErrs(getErr, listErr error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.getErr, f.listErr = getErr, listErr
}

func testPolicy() gate.Policy {
	return gate.Policy{
		Owner:            "owner",
		Repository:       "repo",
		AllowedWorkflows: []string{".github/workflows/deploy.yml", ".github/workflows/release.yml"},
		AllowedEvents:    []string{"push"},
	}
}

// goodJob, goodRun and goodPage pass all ten R9 conditions under
// testPolicy; goodJob matches withStarted's message.
func goodJob() Job {
	return Job{ID: "job-1", WorkflowRunID: 42, Gate: gate.Job{
		Owner:       "owner",
		Repository:  "repo",
		WorkflowRef: "owner/repo/.github/workflows/deploy.yml@refs/heads/main",
		Event:       "push",
	}}
}

func goodRun() gate.Run {
	return gate.Run{
		ID: 42, WorkflowID: 7, RunNumber: 10,
		CreatedAt:           testEpoch.Add(-10 * time.Minute),
		HeadRepository:      "owner/repo",
		HeadBranch:          "main",
		Event:               "push",
		Path:                ".github/workflows/deploy.yml",
		ReferencedWorkflows: gate.ReferencedWorkflowsEmpty,
	}
}

func goodPage() []gate.RunSummary {
	return []gate.RunSummary{
		{ID: 42, WorkflowID: 7, RunNumber: 10, HeadRepository: "owner/repo", HeadBranch: "main"},
		{ID: 40, WorkflowID: 7, RunNumber: 9, HeadRepository: "owner/repo", HeadBranch: "main"},
	}
}

func goodRuns() *fakeRuns {
	return &fakeRuns{run: goodRun(), pages: [][]gate.RunSummary{goodPage()}, total: 2}
}

// newJudgeHarness wires a real JobJudge with runs into a harness.
func newJudgeHarness(t *testing.T, runs *fakeRuns) (*harness, *JobJudge) {
	t.Helper()
	j, err := NewJobJudge(JudgeConfig{Runs: runs, Policy: testPolicy()})
	if err != nil {
		t.Fatal(err)
	}
	return newHarnessWith(t, testPool(2, 0), j), j
}

// seedToken seeds runner name with GitHub ID id in state st (idle or busy)
// and returns its token.
func seedToken(t *testing.T, h *harness, name string, id int64, st State) string {
	t.Helper()
	seed(t, h.table, name, id, StateIdle, h.clock.Now())
	tok := mustToken(t)
	h.table.SetToken(name, tok)
	if st == StateBusy {
		h.table.MarkBusy(name, h.clock.Now())
	}
	return tok.Reveal()
}

// judgeSeeded starts judging job on runner name.
func judgeSeeded(t *testing.T, h *harness, j *JobJudge, name string, job Job) {
	t.Helper()
	if !h.table.BeginJudging(name) {
		t.Fatalf("BeginJudging(%s) = false", name)
	}
	j.Start(name, job)
}

// waitJudge waits for the judge goroutines and deny paths; it must not be
// used while a deny retry is pending.
func waitJudge(t *testing.T, j *JobJudge) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := j.work.wait(ctx); err != nil {
		t.Fatalf("judge work left: %v", err)
	}
}

// eventually waits until cond holds, yielding to the goroutines under test;
// the 10 s limit is a failsafe, not a timing assertion.
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		runtime.Gosched()
	}
}

func verdict(t *testing.T, h *harness, name string) Verdict {
	t.Helper()
	info, ok := h.table.Get(name)
	if !ok {
		t.Fatalf("runner %s not in table", name)
	}
	return info.Verdict
}

// assertDenied checks that the whole deny path ran once for runner name with
// GitHub ID id: runner container, resources, registration, row.
func assertDenied(t *testing.T, h *harness, name string, id int64) {
	t.Helper()
	if got := h.docker.runnerRemovedNames(); !slices.Equal(got, []string{name}) {
		t.Errorf("RemoveContainer calls = %v, want [%s]", got, name)
	}
	if got := h.docker.removedNames(); !slices.Equal(got, []string{name}) {
		t.Errorf("RemoveResources calls = %v, want [%s]", got, name)
	}
	if got := h.gh.removedIDs(); !slices.Equal(got, []int64{id}) {
		t.Errorf("RemoveRunner calls = %v, want [%d]", got, id)
	}
	if !h.gone(name) {
		t.Errorf("%s still in table", name)
	}
}

// waitDenied waits until the deny path for runner name has unregistered it,
// its last step, and then checks the whole path ran once. Unlike waitJudge
// it does not wait for a check that never ends.
func waitDenied(t *testing.T, h *harness, name string, id int64) {
	t.Helper()
	eventually(t, "the deny path of "+name, func() bool { return slices.Contains(h.gh.removedIDs(), id) })
	assertDenied(t, h, name, id)
}

// waitDenyWait waits until the deny path for runner name waits for its
// container, with the stop grace timer set.
func waitDenyWait(t *testing.T, h *harness, name string) {
	t.Helper()
	eventually(t, "the deny path's wait for "+name, func() bool {
		return slices.Contains(h.docker.waitedNames(), name) && h.clock.pending() == 1
	})
}

func TestNewJobJudgeRequiresDependencies(t *testing.T) {
	if _, err := NewJobJudge(JudgeConfig{Policy: testPolicy()}); err == nil {
		t.Error("NewJobJudge without Runs succeeded")
	}
	if _, err := NewJobJudge(JudgeConfig{Runs: goodRuns()}); err == nil {
		t.Error("NewJobJudge without a repository succeeded")
	}
}

// TestJobJudgeBindsOnce: a JobJudge belongs to one Scaler.
func TestJobJudgeBindsOnce(t *testing.T) {
	h, j := newJudgeHarness(t, goodRuns())
	_, err := New(Config{
		Pool: testPool(2, 0), Table: NewTable(nil, h.secrets), Clock: h.clock, ScaleSet: h.gh,
		Containers: h.docker, Judge: j, Tokens: h.tokens, Limits: h.limits, Fatal: func(error) {},
		Secrets: h.secrets, Waiter: h.docker,
	})
	if err == nil {
		t.Fatal("New with a judge bound to another Scaler succeeded")
	}
}

// TestJobJudgeBindInstallsTimer: binding installs the job check timer in a
// table made without a BusyFunc, so R81 cannot be left unwired, and a second
// judge cannot take the same table.
func TestJobJudgeBindInstallsTimer(t *testing.T) {
	h, _ := newJudgeHarness(t, goodRuns())
	table := NewTable(nil, h.secrets)
	j, err := NewJobJudge(JudgeConfig{Runs: goodRuns(), Policy: testPolicy()})
	if err != nil {
		t.Fatal(err)
	}
	cfg := Config{
		Pool: testPool(2, 0), Table: table, Clock: h.clock, ScaleSet: h.gh,
		Containers: h.docker, Judge: j, Tokens: h.tokens, Limits: h.limits, Fatal: func(error) {},
		Secrets: h.secrets, Waiter: h.docker,
	}
	if _, err := New(cfg); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(j.Stop)
	timers := h.clock.pending()
	seed(t, table, "pool-a", 101, StateBusy, h.clock.Now())
	if n := h.clock.pending(); n != timers+1 {
		t.Errorf("pending timers after busy = %d, want %d", n, timers+1)
	}

	other, err := NewJobJudge(JudgeConfig{Runs: goodRuns(), Policy: testPolicy()})
	if err != nil {
		t.Fatal(err)
	}
	cfg.Judge = other
	if _, err := New(cfg); err == nil {
		t.Error("New with a table that has another judge's timer succeeded")
	}
}

// TestJobJudgeBindChecksRepository: the policy must name the pool's
// repo-url repository, ASCII case aside.
func TestJobJudgeBindChecksRepository(t *testing.T) {
	h, _ := newJudgeHarness(t, goodRuns())
	for url, wantOK := range map[string]bool{
		"https://github.com/owner/repo":       true,
		"https://github.com/OWNER/Repo":       true,
		"https://github.com/owner/other":      false,
		"https://github.com/other/repo":       false,
		"https://github.com/owner/repo/extra": false,
		"https://example.com/owner/repo":      false,
		"https://github.com/owner/repo/":      false,
		"https://github.com/owner/repo.git":   false,
		"http://github.com/owner/repo":        false,
		"https://www.github.com/owner/repo":   false,
		"":                                    false,
	} {
		j, err := NewJobJudge(JudgeConfig{Runs: goodRuns(), Policy: testPolicy()})
		if err != nil {
			t.Fatal(err)
		}
		pool := testPool(2, 0)
		pool.RepoURL = url
		_, err = New(Config{
			Pool: pool, Table: NewTable(nil, h.secrets), Clock: h.clock, ScaleSet: h.gh,
			Containers: h.docker, Judge: j, Tokens: h.tokens, Limits: h.limits, Fatal: func(error) {},
			Secrets: h.secrets, Waiter: h.docker,
		})
		if (err == nil) != wantOK {
			t.Errorf("repo-url %q: New = %v, want ok %v", url, err, wantOK)
		}
		j.Stop()
	}
}

// TestJudgeAllows: a job that passes all ten conditions is allowed, the
// latest run lookup reads the pool's repository from an hour before the
// run's created_at, and the job check timer stops (R9, R81).
func TestJudgeAllows(t *testing.T) {
	runs := goodRuns()
	h, j := newJudgeHarness(t, runs)
	tok := seedToken(t, h, "pool-a", 101, StateBusy)
	timers := h.clock.pending()
	if timers != 1 {
		t.Fatalf("pending timers after busy = %d, want 1 (the job check timer)", timers)
	}

	judgeSeeded(t, h, j, "pool-a", goodJob())
	waitJudge(t, j)

	if v := verdict(t, h, "pool-a"); v != VerdictAllow {
		t.Fatalf("verdict = %v, want allow", v)
	}
	if n := h.clock.pending(); n != 0 {
		t.Errorf("pending timers after allow = %d, want 0 (timer stopped)", n)
	}
	left := runs.leftTimes()
	if len(left) != 2 {
		t.Fatalf("lookups = %d, want 2", len(left))
	}
	for i, l := range left {
		if l <= runLookupTimeout-time.Second || l > runLookupTimeout {
			t.Errorf("lookup %d time left = %v, want just under %v", i, l, runLookupTimeout)
		}
	}
	if v, ok := h.table.Authenticate("pool-a", tok, h.clock.Now()); !ok || v != VerdictAllow {
		t.Errorf("hook reads %v, %v; want allow", v, ok)
	}
	want := []listCall{{"owner", "repo", 7, goodRun().CreatedAt.Add(-time.Hour)}}
	if got := runs.listCalls(); !slices.Equal(got, want) {
		t.Errorf("ListWorkflowRuns calls = %+v, want %+v", got, want)
	}
	h.clock.Advance(time.Hour)
	if n := h.docker.calls(); n != 0 {
		t.Errorf("Docker calls after allow = %d, want 0 (timer stopped)", n)
	}
}

// TestJudgeDeniesEachCondition: one wrong field per R9 condition denies the
// job and removes the runner; the latest run lookup is made only when the
// first nine pass (R9, R11).
func TestJudgeDeniesEachCondition(t *testing.T) {
	tests := []struct {
		cond     gate.Condition
		job      func(*Job)
		run      func(*gate.Run)
		page     func([]gate.RunSummary) []gate.RunSummary
		wantList bool
	}{
		{cond: gate.JobRepository, job: func(j *Job) { j.Gate.Owner = "other" }},
		{cond: gate.WorkflowRefTarget, job: func(j *Job) {
			j.Gate.WorkflowRef = "other/repo/.github/workflows/deploy.yml@refs/heads/main"
		}},
		{cond: gate.WorkflowRefMain, job: func(j *Job) {
			j.Gate.WorkflowRef = "owner/repo/.github/workflows/deploy.yml@refs/heads/dev"
		}},
		{cond: gate.JobEvent, job: func(j *Job) { j.Gate.Event = "pull_request_target" }},
		{cond: gate.RunHead, run: func(r *gate.Run) { r.HeadBranch = "dev" }},
		{cond: gate.RunEvent, run: func(r *gate.Run) { r.Event = "workflow_run" }},
		{cond: gate.RunPath, run: func(r *gate.Run) { r.Path = ".github/workflows/other.yml" }},
		{cond: gate.WorkflowPathMatch, job: func(j *Job) {
			j.Gate.WorkflowRef = "owner/repo/.github/workflows/release.yml@refs/heads/main"
		}},
		{cond: gate.NoReferencedWorkflows, run: func(r *gate.Run) { r.ReferencedWorkflows = gate.ReferencedWorkflowsNull }},
		{cond: gate.LatestRun, wantList: true, page: func(p []gate.RunSummary) []gate.RunSummary {
			return append(p, gate.RunSummary{ID: 43, WorkflowID: 7, RunNumber: 11, HeadRepository: "owner/repo", HeadBranch: "main"})
		}},
	}
	for _, tt := range tests {
		t.Run(tt.cond.String(), func(t *testing.T) {
			runs := goodRuns()
			job := goodJob()
			if tt.job != nil {
				tt.job(&job)
			}
			if tt.run != nil {
				tt.run(&runs.run)
			}
			if tt.page != nil {
				runs.pages = [][]gate.RunSummary{tt.page(goodPage())}
				runs.total = len(runs.pages[0])
			}
			h, j := newJudgeHarness(t, runs)
			seedToken(t, h, "pool-a", 101, StateBusy)

			judgeSeeded(t, h, j, "pool-a", job)
			waitJudge(t, j)

			assertDenied(t, h, "pool-a", 101)
			if n := len(runs.listCalls()); (n == 1) != tt.wantList || n > 1 {
				t.Errorf("ListWorkflowRuns calls = %d, want lookup %v", n, tt.wantList)
			}
			logs := h.logs.String()
			if !strings.Contains(logs, tt.cond.String()+":") || !strings.Contains(logs, "job denied; asking the runner to stop") {
				t.Errorf("logs lack the %s reason or the deny:\n%s", tt.cond, logs)
			}
		})
	}
}

// TestJudgeSkipsLatestLookupWithoutWorkflowOrCreatedAt: a run without
// workflow_id or created_at is denied without the latest run lookup (R9).
func TestJudgeSkipsLatestLookupWithoutWorkflowOrCreatedAt(t *testing.T) {
	for name, mutate := range map[string]func(*gate.Run){
		"workflow_id": func(r *gate.Run) { r.WorkflowID = 0 },
		"created_at":  func(r *gate.Run) { r.CreatedAt = time.Time{} },
	} {
		t.Run(name, func(t *testing.T) {
			runs := goodRuns()
			mutate(&runs.run)
			h, j := newJudgeHarness(t, runs)
			seedToken(t, h, "pool-a", 101, StateBusy)

			judgeSeeded(t, h, j, "pool-a", goodJob())
			waitJudge(t, j)

			assertDenied(t, h, "pool-a", 101)
			if n := len(runs.listCalls()); n != 0 {
				t.Errorf("ListWorkflowRuns calls = %d, want 0", n)
			}
			if !strings.Contains(h.logs.String(), gate.LatestRun.String()+":") {
				t.Errorf("logs lack the latest-run reason:\n%s", h.logs)
			}
		})
	}
}

// TestJudgeLookupErrorsDenyWithoutRetry: a failed GetRun or
// ListWorkflowRuns, transient, permanent or rate limited, denies that job
// alone: no retry, no rate limit recorded, no drain, and a lookup that
// failed after showing a passing page still denies (R52, R65, R98).
func TestJudgeLookupErrorsDenyWithoutRetry(t *testing.T) {
	errs := map[string]error{
		"transient":    errTransient,
		"server error": statusError(502),
		"permanent":    statusError(403),
		"auth":         &retry.OpError{Op: retry.OpGetRun, Err: retry.ErrAuth},
		"rate limited": &retry.RateLimitError{RetryAfter: time.Hour},
	}
	for _, lookup := range []string{"GetRun", "ListWorkflowRuns"} {
		for name, lookupErr := range errs {
			t.Run(lookup+"/"+name, func(t *testing.T) {
				runs := goodRuns()
				wantLists := 1
				if lookup == "GetRun" {
					runs.getErr = lookupErr
					wantLists = 0
				} else {
					// The page alone would pass LatestRun.
					runs.listErr = lookupErr
				}
				h, j := newJudgeHarness(t, runs)
				seedToken(t, h, "pool-a", 101, StateBusy)

				judgeSeeded(t, h, j, "pool-a", goodJob())
				waitJudge(t, j)
				h.clock.Advance(time.Hour)

				assertDenied(t, h, "pool-a", 101)
				if n := runs.getCount(); n != 1 {
					t.Errorf("GetRun calls = %d, want 1", n)
				}
				if n := len(runs.listCalls()); n != wantLists {
					t.Errorf("ListWorkflowRuns calls = %d, want %d", n, wantLists)
				}
				for _, op := range []retry.Op{retry.OpGetRun, retry.OpListWorkflowRuns} {
					if at := h.limits.At(op); !at.IsZero() {
						t.Errorf("rate limit recorded for %s until %v", op, at)
					}
				}
				if n := h.fatalCount(); n != 0 || h.table.Drain() != DrainNone {
					t.Errorf("Fatal calls = %d, drain %v; want none", n, h.table.Drain())
				}
				if got := h.errs.last().where; got != "job check lookup" {
					t.Errorf("last error recorded at %q, want the lookup", got)
				}
			})
		}
	}
}

// fixed returns an error function that returns errs in turn, then nil.
func fixed(errs ...error) func(string) error {
	var mu sync.Mutex
	return func(string) error {
		mu.Lock()
		defer mu.Unlock()
		if len(errs) == 0 {
			return nil
		}
		err := errs[0]
		errs = errs[1:]
		return err
	}
}

// TestDenyRecordedBeforeRemoval: the deny is recorded before the deny path
// waits for the runner container, so the hook reads it while the runner
// stops; the container is removed, with a 10 s bound, once it stopped, and
// the token is revoked only after that removal, then the rest of the runner
// goes (R11, R38).
func TestDenyRecordedBeforeRemoval(t *testing.T) {
	runs := goodRuns()
	runs.run.Event = "workflow_dispatch"
	h, j := newJudgeHarness(t, runs)
	tok := seedToken(t, h, "pool-a", 101, StateBusy)
	h.docker.addRunner("pool-a", "running", "running")

	var mu sync.Mutex
	var order []string
	note := func(s string) {
		mu.Lock()
		defer mu.Unlock()
		order = append(order, s)
	}
	h.docker.waitErr = func(name string) error {
		v, ok := h.table.Authenticate(name, tok, h.clock.Now())
		if !ok {
			note("wait: token refused")
		} else {
			note("wait: hook reads " + v.String())
		}
		// The hook asked the runner to stop, and its container stops.
		h.docker.exit(name)
		return nil
	}
	h.docker.setRemoveContainerErr(func(name string) error {
		v, ok := h.table.Authenticate(name, tok, h.clock.Now())
		if !ok {
			note("remove container: token refused")
		} else {
			note("remove container: hook reads " + v.String())
		}
		return nil
	})
	h.docker.setDuringRemove(func(_ context.Context, name string) error {
		info, _ := h.table.Get(name)
		if _, ok := h.table.Authenticate(name, tok, h.clock.Now()); ok {
			note("remove resources: verdict " + info.Verdict.String() + ", token valid")
		} else {
			note("remove resources: verdict " + info.Verdict.String() + ", token revoked")
		}
		return nil
	})
	h.gh.setRemoveErr(func(int64) error {
		note("remove runner")
		return nil
	})

	judgeSeeded(t, h, j, "pool-a", goodJob())
	waitJudge(t, j)

	want := []string{
		"wait: hook reads deny",
		"remove container: hook reads deny",
		"remove resources: verdict deny, token revoked",
		"remove runner",
	}
	mu.Lock()
	defer mu.Unlock()
	if !slices.Equal(order, want) {
		t.Errorf("order = %q, want %q", order, want)
	}
	left := h.docker.runnerRemovedLeftTimes()
	if len(left) != 1 || left[0] <= 9*time.Second || left[0] > removeContainerTimeout {
		t.Errorf("RemoveContainer time left = %v, want one just under %v", left, removeContainerTimeout)
	}
	if !h.gone("pool-a") {
		t.Error("runner still in table")
	}
}

// TestDenyRetriesEvery2sUntilRemoved: while removing the runner container
// fails, permanently or not, the token stays valid so the hook still reads
// the deny, and the owner tries again every 2 s, recording each new error
// once; the token is revoked once the removal succeeds (PLAN M4-2).
func TestDenyRetriesEvery2sUntilRemoved(t *testing.T) {
	h, j := newJudgeHarness(t, goodRuns())
	tok := seedToken(t, h, "pool-a", 101, StateBusy)
	h.docker.setRemoveContainerErr(fixed(statusError(400), statusError(400), errTransient))

	j.Reject("pool-a", "test")
	eventually(t, "the first attempt and its retry timer", func() bool {
		return len(h.docker.runnerRemovedNames()) == 1 && h.clock.pending() == 1
	})
	readsDeny := func(after string) {
		t.Helper()
		if v, ok := h.table.Authenticate("pool-a", tok, h.clock.Now()); !ok || v != VerdictDeny {
			t.Errorf("after %s the hook reads %v, %v; want deny", after, v, ok)
		}
	}
	readsDeny("a permanent error")
	if n := h.errs.count(); n != 1 {
		t.Errorf("errors recorded = %d, want 1", n)
	}

	h.clock.Advance(denyRetryInterval - time.Millisecond)
	if n := len(h.docker.runnerRemovedNames()); n != 1 {
		t.Fatalf("RemoveContainer calls before 2 s = %d, want 1", n)
	}
	h.clock.Advance(time.Millisecond)
	if n := len(h.docker.runnerRemovedNames()); n != 2 {
		t.Fatalf("RemoveContainer calls at 2 s = %d, want 2", n)
	}
	readsDeny("the same error again")
	if n := h.errs.count(); n != 1 {
		t.Errorf("errors recorded after a repeat = %d, want 1", n)
	}

	h.clock.Advance(denyRetryInterval)
	readsDeny("a transient error")
	if n := h.errs.count(); n != 2 {
		t.Errorf("errors recorded after a new error = %d, want 2", n)
	}
	h.clock.Advance(denyRetryInterval)
	if n := len(h.docker.runnerRemovedNames()); n != 4 {
		t.Fatalf("RemoveContainer calls = %d, want 4", n)
	}
	waitJudge(t, j)
	if !h.gone("pool-a") || !slices.Equal(h.gh.removedIDs(), []int64{101}) {
		t.Errorf("runner not removed after the container went: gone %v, RemoveRunner %v",
			h.gone("pool-a"), h.gh.removedIDs())
	}
	if !strings.Contains(h.logs.String(), "removed after retries") {
		t.Errorf("logs lack the recovery:\n%s", h.logs)
	}
}

// TestDenyNotFoundCountsAsRemoved: a runner container removal that finds
// the container gone counts as removed (R50).
func TestDenyNotFoundCountsAsRemoved(t *testing.T) {
	h, j := newJudgeHarness(t, goodRuns())
	seedToken(t, h, "pool-a", 101, StateBusy)
	h.docker.setRemoveContainerErr(fixed(statusError(404)))

	j.Reject("pool-a", "test")
	waitJudge(t, j)

	assertDenied(t, h, "pool-a", 101)
}

// TestDenyWaitsForRunnerStop: the deny path records the deny, then removes
// nothing while the runner container runs; once the container stops it is
// removed at once, and the token is revoked only then (R11).
func TestDenyWaitsForRunnerStop(t *testing.T) {
	h, j := newJudgeHarness(t, goodRuns())
	tok := seedToken(t, h, "pool-a", 101, StateBusy)
	h.docker.addRunner("pool-a", "running", "running")

	j.Reject("pool-a", "test")
	waitDenyWait(t, h, "pool-a")
	h.clock.Advance(denyStopGrace - time.Second)
	if n := h.docker.calls(); n != 0 {
		t.Fatalf("Docker calls while the runner runs = %d, want 0", n)
	}
	if v, ok := h.table.Authenticate("pool-a", tok, h.clock.Now()); !ok || v != VerdictDeny {
		t.Errorf("hook reads %v, %v while the runner stops; want deny", v, ok)
	}

	h.docker.exit("pool-a")
	waitJudge(t, j)
	assertDenied(t, h, "pool-a", 101)
	if n := h.clock.pending(); n != 0 {
		t.Errorf("pending timers = %d, want 0 (grace timer stopped)", n)
	}
	// slog writes the elapsed time in nanoseconds: 29 s on the Clock.
	if logs := h.logs.String(); !strings.Contains(logs, "denied runner's container stopped; removing it") ||
		!strings.Contains(logs, `"elapsed":29000000000`) {
		t.Errorf("logs lack the stop with its elapsed time:\n%s", logs)
	}
}

// TestDenyGraceEndsWait: a runner container still running 30 s after the
// deny is removed then, measured on the Clock (R11).
func TestDenyGraceEndsWait(t *testing.T) {
	h, j := newJudgeHarness(t, goodRuns())
	tok := seedToken(t, h, "pool-a", 101, StateBusy)
	h.docker.addRunner("pool-a", "running", "running")

	j.Reject("pool-a", "test")
	waitDenyWait(t, h, "pool-a")
	h.clock.Advance(denyStopGrace - time.Millisecond)
	if n := h.docker.calls(); n != 0 {
		t.Fatalf("Docker calls before the grace ended = %d, want 0", n)
	}
	if _, ok := h.table.Authenticate("pool-a", tok, h.clock.Now()); !ok {
		t.Error("token revoked before the removal")
	}
	h.clock.Advance(time.Millisecond)
	waitJudge(t, j)

	assertDenied(t, h, "pool-a", 101)
	if !strings.Contains(h.logs.String(), "runner did not stop within grace") {
		t.Errorf("logs lack the grace:\n%s", h.logs)
	}
}

// TestDenyNotFoundWaitEnds: a container that is gone counts as stopped, so
// the rest is removed without waiting for the grace.
func TestDenyNotFoundWaitEnds(t *testing.T) {
	h, j := newJudgeHarness(t, goodRuns())
	seedToken(t, h, "pool-a", 101, StateBusy)

	j.Reject("pool-a", "test")
	waitJudge(t, j)

	assertDenied(t, h, "pool-a", 101)
	if got := h.docker.waitedNames(); !slices.Equal(got, []string{"pool-a"}) {
		t.Errorf("Wait calls = %v, want [pool-a]", got)
	}
	if !h.clock.Now().Equal(testEpoch) {
		t.Errorf("clock moved to %v", h.clock.Now())
	}
	if !strings.Contains(h.logs.String(), "denied runner's container gone") {
		t.Errorf("logs lack the gone container:\n%s", h.logs)
	}
}

// TestDenyWaitErrorWaitsOutGrace: a wait that fails other than with not
// found is logged, and the container is still removed only when the grace
// ends (PLAN M4-2).
func TestDenyWaitErrorWaitsOutGrace(t *testing.T) {
	h, j := newJudgeHarness(t, goodRuns())
	seedToken(t, h, "pool-a", 101, StateBusy)
	h.docker.addRunner("pool-a", "running", "running")
	h.docker.waitErr = func(string) error { return statusError(500) }

	j.Reject("pool-a", "test")
	waitDenyWait(t, h, "pool-a")
	eventually(t, "the wait error log", func() bool {
		return strings.Contains(h.logs.String(), "waiting for the denied runner's container failed")
	})
	h.clock.Advance(denyStopGrace - time.Millisecond)
	assertStillWaiting(t, h, j)
	if n := len(h.docker.runnerRemovedNames()); n != 0 {
		t.Fatalf("RemoveContainer calls before the grace ended = %d, want 0", n)
	}
	h.clock.Advance(time.Millisecond)
	waitJudge(t, j)

	if n := len(h.docker.runnerRemovedNames()); n != 1 {
		t.Errorf("RemoveContainer calls at 30 s = %d, want 1", n)
	}
	assertDenied(t, h, "pool-a", 101)
}

// assertStillWaiting checks that the deny path is still blocked in its
// wait: the grace timer is pending and the judge's work does not end
// within a short time. A deny path that went on to the removal would have
// stopped the grace timer, so a check of the removal right after this one
// cannot pass merely because the removal had not run yet.
func assertStillWaiting(t *testing.T, h *harness, j *JobJudge) {
	t.Helper()
	if n := h.clock.pending(); n != 1 {
		t.Fatalf("pending timers = %d, want 1 (the grace timer)", n)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := j.work.wait(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("judge work wait = %v, want the deny path still waiting", err)
	}
}

// TestDenyHaltDuringGrace: Stop ends the wait for the runner container at
// once, also after a failed wait, and finalize ends it when the grace ends;
// either way nothing is removed and the row, its token revoked, is left to
// finalize.
func TestDenyHaltDuringGrace(t *testing.T) {
	for _, how := range []string{"Stop", "Stop after a wait error", "finalize"} {
		t.Run(how, func(t *testing.T) {
			h, j := newJudgeHarness(t, goodRuns())
			tok := seedToken(t, h, "pool-a", 101, StateBusy)
			h.docker.addRunner("pool-a", "running", "running")
			if how == "Stop after a wait error" {
				h.docker.waitErr = func(string) error { return statusError(500) }
			}

			j.Reject("pool-a", "test")
			waitDenyWait(t, h, "pool-a")
			if how == "Stop after a wait error" {
				eventually(t, "the wait error log", func() bool {
					return strings.Contains(h.logs.String(), "waiting for the denied runner's container failed")
				})
				h.clock.Advance(denyStopGrace / 2)
				assertStillWaiting(t, h, j)
			}
			if how != "finalize" {
				j.Stop()
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				// No clock move: Stop alone ends the wait.
				if err := j.Wait(ctx); err != nil {
					t.Fatalf("Wait: %v", err)
				}
				if n := h.clock.pending(); n != 0 {
					t.Errorf("pending timers after Stop = %d, want 0 (grace timer stopped)", n)
				}
			} else {
				h.finalize()
				h.clock.Advance(denyStopGrace)
				waitJudge(t, j)
			}
			h.clock.Advance(time.Minute)

			if n := h.docker.calls() + h.gh.calls(); n != 0 {
				t.Errorf("Docker and GitHub calls = %d, want 0", n)
			}
			info, _ := h.table.Get("pool-a")
			if info.AbandonReason != AbandonFinalize || info.Verdict != VerdictDeny {
				t.Errorf("row = %+v, want abandoned to finalize with the deny", info)
			}
			if _, ok := h.table.Authenticate("pool-a", tok, h.clock.Now()); ok {
				t.Error("token of a row left to finalize still valid")
			}
		})
	}
}

// TestDenyNotPendingSkipsGrace: when the verdict was not pending, as for a
// Reject after an allow (R102), the hook cannot see a deny, so the
// container is removed without waiting for it to stop.
func TestDenyNotPendingSkipsGrace(t *testing.T) {
	h, j := newJudgeHarness(t, goodRuns())
	seedToken(t, h, "pool-a", 101, StateBusy)
	h.docker.addRunner("pool-a", "running", "running")
	if !h.table.Allow("pool-a") {
		t.Fatal("Allow = false")
	}

	j.Reject("pool-a", "test")
	waitJudge(t, j)

	assertDenied(t, h, "pool-a", 101)
	if n := len(h.docker.waitedNames()); n != 0 {
		t.Errorf("Wait calls = %d, want 0", n)
	}
	if !strings.Contains(h.logs.String(), "verdict was not pending") {
		t.Errorf("logs lack the verdict:\n%s", h.logs)
	}
}

// TestRejectIsIdempotent: Reject twice, even at once, removes the runner
// once; Reject of a runner whose removal began does nothing (R102).
func TestRejectIsIdempotent(t *testing.T) {
	h, j := newJudgeHarness(t, goodRuns())
	seedToken(t, h, "pool-a", 101, StateBusy)
	seedToken(t, h, "pool-b", 102, StateBusy)
	h.table.BeginExit("pool-b")

	var wg sync.WaitGroup
	for range 4 {
		wg.Go(func() { j.Reject("pool-a", "test") })
	}
	wg.Wait()
	j.Reject("pool-b", "test")
	waitJudge(t, j)

	assertDenied(t, h, "pool-a", 101)
}

// TestAllowedJobNotJudgedAgain: a redelivered JobStarted after an allow
// starts no second check, so a lookup that would now fail removes nothing.
func TestAllowedJobNotJudgedAgain(t *testing.T) {
	runs := goodRuns()
	h, j := newJudgeHarness(t, runs)
	seedToken(t, h, "pool-a", 101, StateIdle)

	h.mustScale(withStarted(statsMsg(1, 1), "pool-a", "job-1"))
	waitJudge(t, j)
	if v := verdict(t, h, "pool-a"); v != VerdictAllow {
		t.Fatalf("verdict = %v, want allow", v)
	}

	runs.setErrs(errTransient, errTransient)
	h.mustScale(withStarted(statsMsg(2, 1), "pool-a", "job-1"))
	waitJudge(t, j)
	h.clock.Advance(time.Hour)

	if n := runs.getCount(); n != 1 {
		t.Errorf("GetRun calls = %d, want 1", n)
	}
	if n := h.docker.calls(); n != 0 {
		t.Errorf("Docker calls = %d, want 0", n)
	}
	if v := verdict(t, h, "pool-a"); v != VerdictAllow {
		t.Errorf("verdict = %v, want allow", v)
	}
}

// TestJudgeDoesNothingOnceFinalizing: Start, Reject and the timers make no
// call once finalize has begun, though Scale checked the mark before.
func TestJudgeDoesNothingOnceFinalizing(t *testing.T) {
	runs := goodRuns()
	h, j := newJudgeHarness(t, runs)
	seedToken(t, h, "pool-a", 101, StateIdle)
	seedToken(t, h, "pool-b", 102, StateBusy)
	h.finalize()
	timers := h.clock.pending()

	h.table.BeginJudging("pool-a")
	j.Start("pool-a", goodJob())
	j.Reject("pool-b", "test")
	j.BusyTimers().arm("pool-a", h.clock.Now())
	waitJudge(t, j)

	if n := runs.getCount(); n != 0 {
		t.Errorf("GetRun calls = %d, want 0", n)
	}
	if n := h.docker.calls(); n != 0 {
		t.Errorf("Docker calls = %d, want 0", n)
	}
	if n := h.clock.pending(); n != timers {
		t.Errorf("pending timers = %d, want %d", n, timers)
	}
	if st := h.state("pool-b"); st != StateBusy {
		t.Errorf("rejected runner state = %v, want busy (left to finalize)", st)
	}
}

// TestJudgeStopEndsCheckInProgress: Stop cancels a lookup in flight; the
// check then records nothing and calls nothing, and Wait returns.
func TestJudgeStopEndsCheckInProgress(t *testing.T) {
	runs := goodRuns()
	started := make(chan struct{})
	runs.started = started
	h, j := newJudgeHarness(t, runs)
	seedToken(t, h, "pool-a", 101, StateBusy)

	judgeSeeded(t, h, j, "pool-a", goodJob())
	waitDone(t, started, "GetRun")
	j.Stop()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := j.Wait(ctx); err != nil {
		t.Fatalf("Wait: %v", err)
	}
	h.clock.Advance(time.Hour)

	if n := len(runs.listCalls()); n != 0 {
		t.Errorf("ListWorkflowRuns calls = %d, want 0", n)
	}
	if n := h.docker.calls() + h.gh.calls(); n != 0 {
		t.Errorf("Docker and GitHub calls = %d, want 0", n)
	}
	if info, _ := h.table.Get("pool-a"); info.Verdict != VerdictPending || info.State != StateBusy {
		t.Errorf("runner = %v/%v, want busy and pending", info.State, info.Verdict)
	}
	j.Start("pool-a", goodJob())
	j.Reject("pool-a", "test")
	waitJudge(t, j)
	if n := runs.getCount() + h.docker.calls(); n != 1 {
		t.Errorf("calls after Stop = %d, want only the first GetRun", n)
	}
}

// TestDenyRetriesStop: Stop and finalize both end the deny retries; after
// finalize the row is left to it.
func TestDenyRetriesStop(t *testing.T) {
	for _, how := range []string{"Stop", "finalize"} {
		t.Run(how, func(t *testing.T) {
			h, j := newJudgeHarness(t, goodRuns())
			tok := seedToken(t, h, "pool-a", 101, StateBusy)
			h.docker.setRemoveContainerErr(func(string) error { return errTransient })

			j.Reject("pool-a", "test")
			eventually(t, "the first attempt and its retry timer", func() bool {
				return len(h.docker.runnerRemovedNames()) == 1 && h.clock.pending() == 1
			})
			if how == "Stop" {
				j.Stop()
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				if err := j.Wait(ctx); err != nil {
					t.Fatalf("Wait: %v", err)
				}
				// The dropped retry leaves the row as the deny path had it;
				// finalize, which Stop presumes, removes it.
				info, _ := h.table.Get("pool-a")
				if info.State != StateExiting || info.Verdict != VerdictDeny || info.Abandoned {
					t.Errorf("row after Stop = %+v, want exiting, denied, not abandoned", info)
				}
				if v, ok := h.table.Authenticate("pool-a", tok, h.clock.Now()); !ok || v != VerdictDeny {
					t.Errorf("hook after Stop reads %v, %v; want deny", v, ok)
				}
			} else {
				h.finalize()
			}
			h.clock.Advance(time.Minute)

			if n := len(h.docker.runnerRemovedNames()); n != 1 {
				t.Errorf("RemoveContainer calls = %d, want 1", n)
			}
			if how == "finalize" {
				if info, _ := h.table.Get("pool-a"); info.AbandonReason != AbandonFinalize || info.Verdict != VerdictDeny {
					t.Errorf("row = %+v, want abandoned to finalize with the deny", info)
				}
				if _, ok := h.table.Authenticate("pool-a", tok, h.clock.Now()); ok {
					t.Error("token of a row left to finalize still valid")
				}
			}
		})
	}
}

// TestJudgeRaces: checks, rejections, hook queries and timers on the same
// runners at once remove each runner exactly once (run with -race).
func TestJudgeRaces(t *testing.T) {
	h, j := newJudgeHarness(t, goodRuns())
	names := []string{"pool-a", "pool-b", "pool-c", "pool-d", "pool-e", "pool-f"}
	toks := make(map[string]string)
	for i, n := range names {
		toks[n] = seedToken(t, h, n, int64(101+i), StateBusy)
	}

	var wg sync.WaitGroup
	for i, n := range names {
		wg.Go(func() {
			if h.table.BeginJudging(n) {
				j.Start(n, goodJob())
			}
		})
		wg.Go(func() { h.table.Authenticate(n, toks[n], h.clock.Now()) })
		if i%2 == 0 {
			wg.Go(func() { j.Reject(n, "test") })
		}
	}
	wg.Go(func() { h.clock.Advance(busyTimeout) })
	wg.Wait()
	waitJudge(t, j)

	removed := h.docker.runnerRemovedNames()
	for i, n := range names {
		c := count(removed, n)
		info, ok := h.table.Get(n)
		switch {
		case c > 1:
			t.Errorf("%s container removed %d times", n, c)
		case i%2 == 0 && (c != 1 || ok):
			t.Errorf("rejected %s: removed %d times, in table %v", n, c, ok)
		case c == 0 && (!ok || info.Verdict != VerdictAllow):
			t.Errorf("%s neither removed nor allowed: %+v", n, info)
		case c == 1 && ok:
			t.Errorf("%s removed but still in table: %+v", n, info)
		}
	}
}
