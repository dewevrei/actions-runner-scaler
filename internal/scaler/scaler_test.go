package scaler

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/actions/scaleset"
	"github.com/actions/scaleset/listener"

	"github.com/dewevrei/actions-runner-scaler/internal/logging"
	"github.com/dewevrei/actions-runner-scaler/internal/retry"
)

var (
	_ listener.Scaler = (*Scaler)(nil)
	// The daemon passes logging.Secrets as the SecretRegistry.
	_ SecretRegistry = (*logging.Secrets)(nil)
)

func TestScaleCounts(t *testing.T) {
	type rows map[State]int
	tests := []struct {
		name          string
		max, idle     int
		assigned      int
		existing      rows
		wantStarts    int
		wantRemovals  int
		wantRemaining rows
	}{
		{name: "nothing assigned", max: 2, assigned: 0, wantStarts: 0},
		{name: "one job", max: 2, assigned: 1, wantStarts: 1},
		{name: "capped at max", max: 2, assigned: 5, wantStarts: 2},
		{name: "exactly max", max: 3, assigned: 3, wantStarts: 3},
		{name: "idle setting (R7)", max: 2, idle: 1, assigned: 0, wantStarts: 1},
		{name: "idle plus jobs capped", max: 2, idle: 1, assigned: 4, wantStarts: 2},
		{name: "live runners count", max: 2, assigned: 2, existing: rows{StateBusy: 1}, wantStarts: 1},
		{name: "starting counts as live", max: 2, assigned: 1, existing: rows{StateStarting: 1}, wantStarts: 0},
		// target 2, live 0, but all 1: done holds a container (R3).
		{name: "done counts toward max only", max: 2, assigned: 2, existing: rows{StateDone: 1}, wantStarts: 1},
		{name: "done fills max", max: 2, assigned: 2, existing: rows{StateDone: 2}, wantStarts: 0},
		{name: "exiting fills max", max: 2, assigned: 2, existing: rows{StateExiting: 1, StateBusy: 1}, wantStarts: 0},
		{name: "removed counts for nothing", max: 2, assigned: 2, existing: rows{0: 3}, wantStarts: 2},
		{name: "surplus idle removed", max: 2, assigned: 0, existing: rows{StateIdle: 2}, wantRemovals: 2},
		{name: "idle setting kept", max: 3, idle: 1, assigned: 0, existing: rows{StateIdle: 2}, wantRemovals: 1},
		{name: "busy never removed", max: 2, assigned: 0, existing: rows{StateBusy: 2}, wantRemovals: 0},
		{name: "idle beside busy removed", max: 2, assigned: 1, existing: rows{StateBusy: 1, StateIdle: 1}, wantRemovals: 1},
		{name: "idle kept for a job", max: 2, assigned: 2, existing: rows{StateBusy: 1, StateIdle: 1}, wantRemovals: 0},
		{name: "surplus with busy", max: 3, assigned: 1, existing: rows{StateBusy: 2, StateIdle: 1}, wantRemovals: 1},
		// Done is not live, so the idle runner beyond the target goes.
		{name: "done does not protect idle", max: 2, assigned: 0, existing: rows{StateDone: 1, StateIdle: 1}, wantRemovals: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t, testTarget(tt.max, tt.idle))
			var id int64 = 1
			for _, st := range []State{StateStarting, StateIdle, StateBusy, StateDone, StateExiting, 0} {
				for i := range tt.existing[st] {
					seed(t, h.table, st.String()+string(rune('a'+i)), id, st, testEpoch.Add(time.Duration(id)*time.Second))
					id++
				}
			}
			h.mustScale(statsMsg(1, tt.assigned))
			if got := len(h.docker.startedNames()); got != tt.wantStarts {
				t.Errorf("starts %d, want %d", got, tt.wantStarts)
			}
			if got := len(h.gh.removedIDs()); got != tt.wantRemovals {
				t.Errorf("RemoveRunner calls %d, want %d", got, tt.wantRemovals)
			}
			if all := h.table.Counts(testKey).All(); all > tt.max {
				t.Errorf("runners holding containers %d > max %d (R3)", all, tt.max)
			}
		})
	}
}

func TestScaleStartsInsideScale(t *testing.T) {
	h := newHarness(t, testTarget(2, 0))
	h.mustScale(withAvailable(statsMsg(1, 0), 7))
	if h.acq.callCount() != 1 {
		t.Fatalf("AcquireJobs calls %d, want 1", h.acq.callCount())
	}
	names := h.docker.startedNames()
	if len(names) != 1 || !hasPrefixAll(names, "pool-") {
		t.Fatalf("started %v, want one pool- runner before Scale returned (R4)", names)
	}
	if st := h.state(names[0]); st != StateIdle {
		t.Fatalf("state %v, want idle", st)
	}
}

func TestScaleNilMessageKeepsStatistics(t *testing.T) {
	h := newHarness(t, testTarget(3, 0))
	h.mustScale(statsMsg(listener.InitialMessageID, 1))
	if n := len(h.docker.startedNames()); n != 1 {
		t.Fatalf("initial message: starts %d, want 1", n)
	}
	// One runner exits; nil and statistics-less messages keep target 1.
	name := h.docker.startedNames()[0]
	h.table.BeginExit(name)
	h.table.MarkRemoved(name)
	h.mustScale(nil)
	if n := len(h.docker.startedNames()); n != 2 {
		t.Fatalf("nil message: starts %d, want 2", n)
	}
	h.table.BeginExit(h.docker.startedNames()[1])
	h.table.MarkRemoved(h.docker.startedNames()[1])
	h.mustScale(&scaleset.RunnerScaleSetMessage{MessageID: 5})
	if n := len(h.docker.startedNames()); n != 3 {
		t.Fatalf("nil statistics: starts %d, want 3", n)
	}
}

func TestScaleUnknownRunnerMessages(t *testing.T) {
	h := newHarness(t, testTarget(2, 0))
	msg := withStarted(statsMsg(1, 0), "nobody", "j1")
	msg = withCompleted(msg, "nobody", "j1")
	msg = withStarted(msg, "", "j2")
	msg.JobStartedMessages = append(msg.JobStartedMessages, nil)
	msg.JobCompletedMessages = append(msg.JobCompletedMessages, nil)
	h.mustScale(msg)
	if len(h.judge.rejectedNames()) != 0 {
		t.Fatal("judge called for unknown runners")
	}
	if !strings.Contains(h.logs.String(), "for a runner not in the table") {
		t.Fatal("no log for unknown runner")
	}
}

// TestJobMessagesForMissingRunnersLogInfo: a JobStarted or JobCompleted for a
// runner that is not in the table or is being removed is logged once at Info
// with its job id (0002 SPEC R163).
func TestJobMessagesForMissingRunnersLogInfo(t *testing.T) {
	tests := []struct {
		name     string
		removing bool
		msg      func(*scaleset.RunnerScaleSetMessage) *scaleset.RunnerScaleSetMessage
		wantMsg  string
		wantAttr string
	}{
		{"started unknown", false,
			func(m *scaleset.RunnerScaleSetMessage) *scaleset.RunnerScaleSetMessage {
				return withStarted(m, "pool-x", "j-x")
			},
			"job started for a runner not in the table", `"workflowRunId":42`},
		{"started removing", true,
			func(m *scaleset.RunnerScaleSetMessage) *scaleset.RunnerScaleSetMessage {
				return withStarted(m, "pool-x", "j-x")
			},
			"job started while the runner is being removed", `"workflowRunId":42`},
		{"completed unknown", false,
			func(m *scaleset.RunnerScaleSetMessage) *scaleset.RunnerScaleSetMessage {
				return withCompleted(m, "pool-x", "j-x")
			},
			"job completed for a runner not in the table", `"result":"succeeded"`},
		{"completed removing", true,
			func(m *scaleset.RunnerScaleSetMessage) *scaleset.RunnerScaleSetMessage {
				return withCompleted(m, "pool-x", "j-x")
			},
			"job completed while the runner is being removed", `"result":"succeeded"`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, testTarget(2, 0))
			if tc.removing {
				seed(t, h.table, "pool-x", 1, StateIdle, testEpoch)
				h.table.BeginExit("pool-x")
			}
			h.mustScale(tc.msg(statsMsg(1, 0)))
			var got []string
			for _, line := range strings.Split(h.logs.String(), "\n") {
				if strings.Contains(line, `"msg":"`+tc.wantMsg+`"`) {
					got = append(got, line)
				}
			}
			if len(got) != 1 {
				t.Fatalf("%d records with msg %q, want 1; logs:\n%s", len(got), tc.wantMsg, h.logs.String())
			}
			for _, want := range []string{`"level":"INFO"`, `"jobId":"j-x"`, `"runner":"pool-x"`, tc.wantAttr} {
				if !strings.Contains(got[0], want) {
					t.Errorf("record %s lacks %s", got[0], want)
				}
			}
		})
	}
}

// TestJobStartedDoesNotJudge: a JobStarted records the job and the busy
// time, logs the job, and records no verdict and calls no judge; a
// redelivered one changes nothing (0002 SPEC R123, R81, R102).
func TestJobStartedDoesNotJudge(t *testing.T) {
	h := newHarness(t, testTarget(2, 0))
	seed(t, h.table, "pool-a", 1, StateIdle, testEpoch)
	msg := withStarted(statsMsg(1, 1), "pool-a", "j1")
	h.mustScale(msg)
	h.clock.Advance(time.Minute)
	h.mustScale(msg) // redelivered
	info, _ := h.table.Get("pool-a")
	if info.State != StateBusy || !info.BusyAt.Equal(testEpoch) || info.Verdict != VerdictPending {
		t.Fatalf("row %+v, want busy since the first JobStarted, verdict pending", info)
	}
	if got := h.busyCalls(); len(got) != 1 {
		t.Fatalf("busy callbacks %v, want 1", got)
	}
	if n := len(h.judge.rejectedNames()); n != 0 {
		t.Fatalf("Reject calls = %d, want 0", n)
	}
	job := info.Job
	if job == nil || job.ID != "j1" || job.WorkflowRunID != 42 || job.Owner != "owner" || job.Repository != "repo" ||
		job.Event != "push" {
		t.Fatalf("job %+v lacks message fields", job)
	}
	if n := h.gh.calls() + h.docker.calls(); n != 0 {
		t.Errorf("GitHub and Docker calls = %d, want 0", n)
	}
	if logs := h.logs.String(); !strings.Contains(logs, `"msg":"job started"`) || !strings.Contains(logs, `"workflowRunId":42`) ||
		!strings.Contains(logs, `"jobId":"j1"`) {
		t.Errorf("logs lack the job started line:\n%s", logs)
	}
}

func TestScaleJobCompleted(t *testing.T) {
	h := newHarness(t, testTarget(2, 0))
	seed(t, h.table, "pool-a", 1, StateIdle, testEpoch)
	h.mustScale(withStarted(statsMsg(1, 1), "pool-a", "j1"))
	h.clock.Advance(time.Minute)
	h.mustScale(withCompleted(statsMsg(2, 0), "pool-a", "j1"))
	info, _ := h.table.Get("pool-a")
	if info.State != StateDone || !info.CompletedAt.Equal(testEpoch.Add(time.Minute)) {
		t.Fatalf("state %v completed %v", info.State, info.CompletedAt)
	}
	h.clock.Advance(time.Minute)
	h.mustScale(withCompleted(statsMsg(3, 0), "pool-a", "j1"))
	if again, _ := h.table.Get("pool-a"); !again.CompletedAt.Equal(info.CompletedAt) {
		t.Fatal("redelivered JobCompleted moved the time (R101)")
	}
	if len(h.gh.removedIDs()) != 0 {
		t.Fatal("done runner was scaled down")
	}
}

func TestScaleR102(t *testing.T) {
	t.Run("JobCompleted for another job only logs", func(t *testing.T) {
		h := newHarness(t, testTarget(2, 0))
		seed(t, h.table, "pool-a", 1, StateIdle, testEpoch)
		h.mustScale(withStarted(statsMsg(1, 1), "pool-a", "j1"))
		h.mustScale(withCompleted(statsMsg(2, 1), "pool-a", "j2"))
		info, _ := h.table.Get("pool-a")
		if info.State != StateBusy || !info.CompletedAt.IsZero() {
			t.Fatalf("state %v completed %v; want busy unchanged", info.State, info.CompletedAt)
		}
		if !strings.Contains(h.logs.String(), "did not start") {
			t.Fatal("no log for mismatched JobCompleted")
		}
	})
	t.Run("done runner gets another job", func(t *testing.T) {
		h := newHarness(t, testTarget(2, 0))
		seed(t, h.table, "pool-a", 1, StateIdle, testEpoch)
		tok := mustToken(t)
		h.table.SetToken("pool-a", tok)
		h.mustScale(withCompleted(statsMsg(1, 0), "pool-a", "j1"))
		h.mustScale(withStarted(statsMsg(2, 1), "pool-a", "j2"))
		if got := h.judge.rejectedNames(); len(got) != 1 || got[0] != "pool-a" {
			t.Fatalf("rejected %v, want pool-a", got)
		}
		if _, ok := h.table.Authenticate("pool-a", tok.Reveal(), testEpoch); ok {
			t.Fatal("token still valid after the rejection (R102)")
		}
	})
	t.Run("done runner gets its own job again", func(t *testing.T) {
		h := newHarness(t, testTarget(2, 0))
		seed(t, h.table, "pool-a", 1, StateIdle, testEpoch)
		h.mustScale(withStarted(statsMsg(1, 1), "pool-a", "j1"))
		h.mustScale(withCompleted(statsMsg(2, 0), "pool-a", "j1"))
		h.mustScale(withStarted(statsMsg(3, 0), "pool-a", "j1"))
		if len(h.judge.rejectedNames()) != 0 {
			t.Fatal("rejected a done runner on its own job")
		}
		if h.state("pool-a") != StateDone {
			t.Fatalf("state %v, want done", h.state("pool-a"))
		}
	})
}

// TestScaleAcquireJobs: AcquireJobs errors stay in the placement (0002 SPEC
// R214, R216): Scale returns nil after handling the job messages, the entry
// stays, and the target's last error holds the error.
func TestScaleAcquireJobs(t *testing.T) {
	t.Run("transient error after job messages", func(t *testing.T) {
		h := newHarness(t, testTarget(3, 0))
		seed(t, h.table, "pool-a", 1, StateIdle, testEpoch)
		seed(t, h.table, "pool-b", 2, StateBusy, testEpoch)
		h.table.RecordJobStarted("pool-b", Job{ID: "jb"})
		h.acq.err = errTransient
		msg := withAvailable(statsMsg(1, 0), 9)
		msg = withStarted(msg, "pool-a", "ja")
		msg = withCompleted(msg, "pool-b", "jb")
		// Another job than the waiting one (0002 SPEC R206 matching).
		msg.JobCompletedMessages[0].RunnerRequestID = 8
		h.mustScale(msg)
		if h.state("pool-a") != StateBusy || h.state("pool-b") != StateDone {
			t.Fatal("JobStarted or JobCompleted not handled")
		}
		if c := retry.Classify(context.Background(), retry.OpAcquireJobs, h.errs.last().err); c != retry.Transient {
			t.Fatalf("last error class %v, want transient", c)
		}
		if len(h.docker.startedNames()) != 0 || h.fatalCount() != 0 {
			t.Fatal("started runners or reported fatal")
		}
		if n := h.placer.WaitingJobs(testKey); n != 1 {
			t.Fatalf("waiting jobs %d, want the entry kept", n)
		}
	})
	t.Run("session lost", func(t *testing.T) {
		h := newHarness(t, testTarget(2, 0))
		h.acq.err = errSessionLost
		h.mustScale(withAvailable(statsMsg(1, 0), 9))
		if h.acq.lostCount() != 1 {
			t.Fatalf("SessionLost calls %d, want 1 (R25)", h.acq.lostCount())
		}
	})
	t.Run("permanent goes to fatal", func(t *testing.T) {
		h := newHarness(t, testTarget(2, 0))
		h.acq.err = errPermanent
		h.mustScale(withAvailable(statsMsg(1, 2), 9))
		if h.fatalCount() != 1 {
			t.Fatalf("fatal reports %d, want 1", h.fatalCount())
		}
		if len(h.docker.startedNames()) != 0 {
			t.Fatal("started runners after a permanent AcquireJobs error (R51)")
		}
	})
	t.Run("rate limited then not called before allowed", func(t *testing.T) {
		h := newHarness(t, testTarget(2, 0))
		h.acq.setErr(errRateLimited())
		h.mustScale(withAvailable(statsMsg(1, 0), 9))
		h.acq.setErr(nil)
		h.clock.Advance(59 * time.Second)
		h.mustScale(nil)
		if h.acq.callCount() != 1 {
			t.Fatalf("AcquireJobs calls %d, want 1 (none before the allowed time)", h.acq.callCount())
		}
		h.clock.Advance(time.Second)
		h.mustScale(nil)
		if h.acq.callCount() != 2 || len(h.docker.startedNames()) != 1 {
			t.Fatalf("AcquireJobs calls %d, starts %d; want 2 and 1", h.acq.callCount(), len(h.docker.startedNames()))
		}
	})
	t.Run("adapter's op not repeated", func(t *testing.T) {
		h := newHarness(t, testTarget(2, 0))
		h.acq.setErr(&retry.OpError{Op: retry.OpAcquireJobs, Err: errTransient})
		h.mustScale(withAvailable(statsMsg(1, 0), 9))
		err := h.errs.last().err
		if n := strings.Count(err.Error(), string(retry.OpAcquireJobs)); n != 1 {
			t.Fatalf("error %q names the call %d times, want 1", err, n)
		}
		h.acq.setErr(errTransient)
		h.mustScale(nil)
		if err := h.errs.last().err; !strings.HasPrefix(err.Error(), string(retry.OpAcquireJobs)+": ") {
			t.Fatalf("bare error %q not wrapped with the op", err)
		}
	})
	t.Run("no available jobs, no call", func(t *testing.T) {
		h := newHarness(t, testTarget(2, 0))
		h.mustScale(statsMsg(1, 1))
		h.mustScale(nil)
		if h.acq.callCount() != 0 {
			t.Fatal("AcquireJobs called without JobAvailable")
		}
	})
}

// TestScaleDrain: during a signal drain Scale calls no AcquireJobs (0002
// SPEC R220) and starts nothing; the drain's scale-down (RemoveIdle) removes
// every idle runner, oldest first (R53, R91).
func TestScaleDrain(t *testing.T) {
	h := newHarness(t, testTarget(3, 1))
	seed(t, h.table, "pool-old", 1, StateIdle, testEpoch)
	seed(t, h.table, "pool-new", 2, StateIdle, testEpoch.Add(time.Second))
	seed(t, h.table, "pool-busy", 3, StateBusy, testEpoch)
	seed(t, h.table, "pool-starting", 4, StateStarting, testEpoch)
	h.gh.setRemoveErr(func(id int64) error {
		if id == 2 {
			return errStillRunning()
		}
		return nil
	})
	h.table.SetDraining(DrainSignal)

	h.mustScale(withAvailable(statsMsg(1, 3), 9))
	if h.acq.callCount() != 0 {
		t.Fatal("AcquireJobs called during a signal drain (0002 SPEC R220)")
	}
	if n := len(h.docker.startedNames()); n != 0 {
		t.Fatalf("starts %d during drain, want 0", n)
	}
	if err := h.s.RemoveIdle(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := h.gh.removedIDs(); !slices.Equal(got, []int64{1, 2}) {
		t.Fatalf("RemoveRunner ids %v, want every idle oldest first [1 2]", got)
	}
	if !h.gone("pool-old") {
		t.Fatalf("pool-old %v, want removed", h.state("pool-old"))
	}
	if h.state("pool-new") != StateBusy {
		t.Fatalf("pool-new %v, want busy after JobStillRunning (R53, R91)", h.state("pool-new"))
	}
	// Busy from JobStillRunningError is a busy source: the 180 s timer
	// hangs off the callback (R81, M4-3).
	if got := h.busyCalls(); !slices.Equal(got, []string{"pool-busy", "pool-new"}) {
		t.Fatalf("busy callbacks %v, want seed's pool-busy then pool-new", got)
	}
	if h.state("pool-busy") != StateBusy || h.state("pool-starting") != StateStarting {
		t.Fatal("busy or starting runner touched")
	}

	// The runner that was starting at the signal becomes idle and goes.
	h.table.MarkStarted("pool-starting")
	if err := h.s.RemoveIdle(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := h.gh.removedIDs(); !slices.Equal(got, []int64{1, 2, 4}) {
		t.Fatalf("RemoveRunner ids %v, want 4 added", got)
	}
	if !h.gone("pool-starting") {
		t.Fatalf("pool-starting %v, want removed", h.state("pool-starting"))
	}
	if got := h.docker.removedNames(); !slices.Equal(got, []string{"pool-old", "pool-starting"}) {
		t.Fatalf("resources removed %v", got)
	}
}

// TestErrorDrainRemoveIdle checks R53 through RemoveIdle, as the drain code
// calls it after Run returned: JobStillRunningError → busy; any other
// RemoveRunner failure → the runner container only, registration left.
func TestErrorDrainRemoveIdle(t *testing.T) {
	h := newHarness(t, testTarget(4, 0))
	seed(t, h.table, "pool-transient", 1, StateIdle, testEpoch)
	seed(t, h.table, "pool-permanent", 2, StateIdle, testEpoch.Add(time.Second))
	seed(t, h.table, "pool-running", 3, StateIdle, testEpoch.Add(2*time.Second))
	seed(t, h.table, "pool-ok", 4, StateIdle, testEpoch.Add(3*time.Second))
	tok := mustToken(t)
	h.table.SetToken("pool-transient", tok)
	h.gh.setRemoveErr(func(id int64) error {
		switch id {
		case 1:
			return errTransient
		case 2:
			return statusError(401)
		case 3:
			return errStillRunning()
		}
		return nil
	})
	h.table.SetDraining(DrainError)
	if err := h.s.RemoveIdle(context.Background()); err != nil {
		t.Fatal(err)
	}

	if got := h.docker.runnerRemovedNames(); !slices.Equal(got, []string{"pool-transient", "pool-permanent"}) {
		t.Fatalf("runner containers removed %v, want the two failed ones (R53)", got)
	}
	if got := h.docker.removedNames(); !slices.Equal(got, []string{"pool-ok"}) {
		t.Fatalf("full removals %v, want only pool-ok", got)
	}
	for _, name := range []string{"pool-transient", "pool-permanent"} {
		if h.state(name) != StateExiting {
			t.Fatalf("%s state %v, want exiting for finalize", name, h.state(name))
		}
	}
	if _, ok := h.table.Authenticate("pool-transient", tok.Reveal(), testEpoch); ok {
		t.Fatal("token valid after the container was removed")
	}
	if h.state("pool-running") != StateBusy || !slices.Contains(h.busyCalls(), "pool-running") {
		t.Fatal("JobStillRunningError did not mark the runner busy with a callback (R53)")
	}
	if !h.gone("pool-ok") {
		t.Fatal("pool-ok not removed")
	}
	if got := h.gh.removedIDs(); !slices.Equal(got, []int64{1, 2, 3, 4}) {
		t.Fatalf("RemoveRunner ids %v, want one call each", got)
	}
	// Nothing is left to remove.
	if err := h.s.RemoveIdle(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := len(h.gh.removedIDs()); n != 4 {
		t.Fatalf("RemoveRunner calls %d after a second RemoveIdle, want 4", n)
	}
}

func TestErrorDrainRemoveContainerFails(t *testing.T) {
	h := newHarness(t, testTarget(2, 0))
	seed(t, h.table, "pool-a", 1, StateIdle, testEpoch)
	h.gh.setRemoveErr(func(int64) error { return errTransient })
	h.docker.removeContainerErr = func(string) error { return errTransient }
	h.table.SetDraining(DrainError)
	if err := h.s.RemoveIdle(context.Background()); err != nil {
		t.Fatal(err)
	}
	if h.state("pool-a") != StateExiting {
		t.Fatalf("state %v, want exiting", h.state("pool-a"))
	}
	if last := h.errs.last(); last.where != "error drain remove runner container" {
		t.Fatalf("last recorded %+v, want the container removal", last)
	}
}

func TestErrorDrainRateLimitedStopsRest(t *testing.T) {
	h := newHarness(t, testTarget(2, 0))
	seed(t, h.table, "pool-a", 1, StateIdle, testEpoch)
	seed(t, h.table, "pool-b", 2, StateIdle, testEpoch.Add(time.Second))
	h.gh.setRemoveErr(func(int64) error { return errRateLimited() })
	h.table.SetDraining(DrainError)
	if err := h.s.RemoveIdle(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := h.gh.removedIDs(); !slices.Equal(got, []int64{1}) {
		t.Fatalf("RemoveRunner ids %v, want only the first (R95)", got)
	}
	if got := h.docker.runnerRemovedNames(); !slices.Equal(got, []string{"pool-a"}) {
		t.Fatalf("runner containers removed %v, want pool-a (R53)", got)
	}
	if h.state("pool-b") != StateIdle {
		t.Fatal("pool-b touched")
	}
}

// TestSignalDrainKeepsFailedIdle: R53's container removal is for the error
// drain only; a signal drain keeps R93 and R94.
func TestSignalDrainKeepsFailedIdle(t *testing.T) {
	h := newHarness(t, testTarget(2, 0))
	seed(t, h.table, "pool-a", 1, StateIdle, testEpoch)
	seed(t, h.table, "pool-b", 2, StateIdle, testEpoch.Add(time.Second))
	h.gh.setRemoveErr(func(id int64) error {
		if id == 1 {
			return errTransient
		}
		return statusError(401)
	})
	h.table.SetDraining(DrainSignal)
	if err := h.s.RemoveIdle(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := len(h.docker.runnerRemovedNames()); n != 0 {
		t.Fatalf("runner containers removed %d during a signal drain, want 0", n)
	}
	if h.state("pool-a") != StateIdle {
		t.Fatal("transient failure did not keep the runner idle (R93)")
	}
	if info, _ := h.table.Get("pool-b"); info.State != StateIdle || !info.RemoveGivenUp {
		t.Fatalf("row %+v, want idle and given up (R94)", info)
	}
}

// TestRemoveIdleOutsideDrain: outside a drain RemoveIdle removes nothing
// (its goal is 0 only while draining, 0002 PLAN M14-5); the placement
// removes the oldest runner beyond the goal (0002 SPEC R185).
func TestRemoveIdleOutsideDrain(t *testing.T) {
	h := newHarness(t, testTarget(3, 1))
	seed(t, h.table, "pool-a", 1, StateIdle, testEpoch)
	seed(t, h.table, "pool-b", 2, StateIdle, testEpoch.Add(time.Second))
	if err := h.s.RemoveIdle(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := h.gh.removedIDs(); len(got) != 0 {
		t.Fatalf("RemoveIdle outside a drain removed %v", got)
	}
	h.mustScale(nil)
	if got := h.gh.removedIDs(); !slices.Equal(got, []int64{1}) {
		t.Fatalf("RemoveRunner ids %v, want the oldest beyond idle 1", got)
	}
}

// TestScaleDownCanceled: shutdown during RemoveRunner returns the
// cancellation and leaves the runner idle, neither given up nor removed.
func TestScaleDownCanceled(t *testing.T) {
	h := newHarness(t, testTarget(2, 0))
	seed(t, h.table, "pool-a", 1, StateIdle, testEpoch)
	seed(t, h.table, "pool-b", 2, StateIdle, testEpoch.Add(time.Second))
	ctx, cancel := context.WithCancel(context.Background())
	h.gh.setRemoveErr(func(int64) error {
		cancel()
		return context.Canceled
	})
	h.table.SetDraining(DrainSignal)
	err := h.s.RemoveIdle(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("RemoveIdle = %v, want canceled", err)
	}
	if got := h.gh.removedIDs(); !slices.Equal(got, []int64{1}) {
		t.Fatalf("RemoveRunner ids %v, want the first only", got)
	}
	if info, _ := h.table.Get("pool-a"); info.State != StateIdle || info.RemoveGivenUp {
		t.Fatalf("row %+v, want idle and not given up", info)
	}
	if h.errs.count() != 0 || len(h.docker.removedNames()) != 0 {
		t.Fatal("cancellation recorded or removed resources")
	}
}

// TestScaleConcurrentWithJobCheck runs Scale while job check calls hit the
// same rows; -race checks the locking.
func TestScaleConcurrentWithJobCheck(t *testing.T) {
	h := newHarness(t, testTarget(3, 0))
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			default:
			}
			for _, r := range h.table.List() {
				h.table.Authenticate(r.Name, "wrong", h.clock.Now())
				h.table.Allow(r.Name)
				h.table.Get(r.Name)
			}
			h.table.Counts(testKey)
		}
	}()
	for i := range 30 {
		assigned := 3
		if i%2 == 1 {
			assigned = 0
		}
		h.mustScale(statsMsg(i+1, assigned))
	}
	close(stop)
	<-done
	if all := h.table.Counts(testKey).All(); all > 3 {
		t.Fatalf("runners %d > max 3 (R3)", all)
	}
}

func TestScaleJobStillRunningOutsideDrain(t *testing.T) {
	h := newHarness(t, testTarget(2, 0))
	seed(t, h.table, "pool-a", 1, StateIdle, testEpoch)
	h.gh.setRemoveErr(func(int64) error { return errStillRunning() })
	h.mustScale(statsMsg(1, 0))
	if h.state("pool-a") != StateIdle || len(h.busyCalls()) != 0 {
		t.Fatalf("state %v busy %v; want idle and not busy", h.state("pool-a"), h.busyCalls())
	}
	h.mustScale(nil)
	if n := len(h.gh.removedIDs()); n != 2 {
		t.Fatalf("RemoveRunner calls %d, want a retry on the next Scale", n)
	}
}

func TestScaleRemoveRunnerErrors(t *testing.T) {
	t.Run("transient retried next scale (R93)", func(t *testing.T) {
		h := newHarness(t, testTarget(2, 0))
		seed(t, h.table, "pool-a", 1, StateIdle, testEpoch)
		h.gh.setRemoveErr(func(int64) error { return errTransient })
		h.mustScale(statsMsg(1, 0))
		if h.state("pool-a") != StateIdle {
			t.Fatal("runner left idle after transient failure")
		}
		h.gh.setRemoveErr(nil)
		h.mustScale(nil)
		if n := len(h.gh.removedIDs()); n != 2 || !h.gone("pool-a") {
			t.Fatalf("calls %d gone %v; want 2 and removed", n, h.gone("pool-a"))
		}
	})
	t.Run("permanent not retried (R94)", func(t *testing.T) {
		h := newHarness(t, testTarget(2, 0))
		seed(t, h.table, "pool-a", 1, StateIdle, testEpoch)
		h.gh.setRemoveErr(func(int64) error { return statusError(401) })
		h.mustScale(statsMsg(1, 0))
		h.mustScale(nil)
		h.mustScale(nil)
		if n := len(h.gh.removedIDs()); n != 1 {
			t.Fatalf("RemoveRunner calls %d, want 1", n)
		}
		if h.errs.count() != 1 || h.fatalCount() != 0 {
			t.Fatalf("recorded %d fatal %d; want 1 and 0 (R94, R97)", h.errs.count(), h.fatalCount())
		}
		if info, _ := h.table.Get("pool-a"); info.State != StateIdle || !info.RemoveGivenUp {
			t.Fatalf("row %+v, want idle and given up", info)
		}
	})
	t.Run("rate limit cut by the call deadline stops the rest (R95)", func(t *testing.T) {
		// The server answered with a rate limit but reading the body hit
		// the call's own deadline: still a rate limit (retry.Classify
		// checks it before the call deadline), so the rest wait (R52, R95).
		h := newHarness(t, testTarget(3, 0))
		seed(t, h.table, "pool-a", 1, StateIdle, testEpoch)
		seed(t, h.table, "pool-b", 2, StateIdle, testEpoch.Add(time.Second))
		h.gh.setRemoveErr(func(int64) error {
			return &retry.RateLimitError{RetryAfter: 90 * time.Second, Err: context.DeadlineExceeded}
		})
		h.mustScale(statsMsg(1, 0))
		if got := h.gh.removedIDs(); !slices.Equal(got, []int64{1}) {
			t.Fatalf("RemoveRunner ids %v, want only the first", got)
		}
	})
	t.Run("rate limited stops the rest (R95)", func(t *testing.T) {
		h := newHarness(t, testTarget(3, 0))
		seed(t, h.table, "pool-a", 1, StateIdle, testEpoch)
		seed(t, h.table, "pool-b", 2, StateIdle, testEpoch.Add(time.Second))
		seed(t, h.table, "pool-c", 3, StateIdle, testEpoch.Add(2*time.Second))
		h.gh.setRemoveErr(func(int64) error { return errRateLimited() })
		h.mustScale(statsMsg(1, 0))
		if got := h.gh.removedIDs(); !slices.Equal(got, []int64{1}) {
			t.Fatalf("RemoveRunner ids %v, want only the first", got)
		}
		h.gh.setRemoveErr(nil)
		h.clock.Advance(59 * time.Second)
		h.mustScale(nil)
		if n := len(h.gh.removedIDs()); n != 1 {
			t.Fatalf("RemoveRunner calls %d before the allowed time, want 1", n)
		}
		h.clock.Advance(time.Second)
		h.mustScale(nil)
		if n := len(h.gh.removedIDs()); n != 4 {
			t.Fatalf("RemoveRunner calls %d after the allowed time, want 4", n)
		}
	})
	t.Run("gone counts as success", func(t *testing.T) {
		h := newHarness(t, testTarget(2, 0))
		seed(t, h.table, "pool-a", 1, StateIdle, testEpoch)
		h.gh.setRemoveErr(func(int64) error { return retry.ErrRunnerGone })
		h.mustScale(statsMsg(1, 0))
		if !h.gone("pool-a") {
			t.Fatalf("state %v, want removed", h.state("pool-a"))
		}
	})
}

func TestScaleStartFailures(t *testing.T) {
	for _, tt := range []struct {
		name string
		set  func(h *harness)
	}{
		{"JIT permanent", func(h *harness) { h.gh.setJITErr(func(string) error { return statusError(422) }) }},
		{"JIT unauthorized", func(h *harness) { h.gh.setJITErr(func(string) error { return statusError(401) }) }},
		{"container permanent", func(h *harness) { h.docker.setStartErr(func(string) error { return statusError(400) }) }},
		{"container transient", func(h *harness) { h.docker.setStartErr(func(string) error { return errTransient }) }},
		{"adapter's op error", func(h *harness) {
			h.gh.setJITErr(func(string) error { return &retry.OpError{Op: retry.OpGenerateJIT, Err: statusError(422)} })
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t, testTarget(1, 0))
			tt.set(h)
			h.mustScale(statsMsg(1, 1))
			if h.fatalCount() != 0 {
				t.Fatal("start failure reported fatal (R64, R98)")
			}
			st := h.table.StartStatus(testKey)
			if st.Failures != 1 || st.LastError == nil || !st.LastErrorAt.Equal(testEpoch) {
				t.Fatalf("start status %+v", st)
			}
			msg := st.LastError.Error()
			if strings.Count(msg, string(retry.OpGenerateJIT))+strings.Count(msg, string(retry.OpContainerStart)) > 1 {
				t.Fatalf("last error %q names the call more than once", msg)
			}
			h.mustScale(nil) // next poll tries again (R96)
			if got := h.table.StartStatus(testKey).Failures; got != 2 {
				t.Fatalf("failures %d after the next poll, want 2", got)
			}
		})
	}
}

// TestScaleStartBackoff checks R61: three failures in a row, then 10, 20,
// 40 seconds … up to 5 minutes, and a reset on the first success.
func TestScaleStartBackoff(t *testing.T) {
	h := newHarness(t, testTarget(1, 0))
	h.docker.setStartErr(func(string) error { return errTransient })
	h.mustScale(statsMsg(1, 1))
	h.mustScale(nil)
	h.mustScale(nil)
	if n := len(h.docker.startedNames()); n != 3 {
		t.Fatalf("attempts %d, want 3 before any backoff", n)
	}
	st := h.table.StartStatus(testKey)
	if st.Failures != 3 || !st.NextStartAt.Equal(h.clock.Now().Add(10*time.Second)) {
		t.Fatalf("after 3 failures: %+v, want next start in 10s", st)
	}

	waits := []time.Duration{10, 20, 40, 80, 160, 300, 300}
	for i, w := range waits {
		w *= time.Second
		h.clock.Advance(w - time.Millisecond)
		h.mustScale(nil)
		if n := len(h.docker.startedNames()); n != 3+i {
			t.Fatalf("wait %v: started before the backoff ended", w)
		}
		h.clock.Advance(time.Millisecond)
		h.mustScale(nil)
		if n := len(h.docker.startedNames()); n != 4+i {
			t.Fatalf("wait %v: no start once the backoff ended", w)
		}
		next := waits[min(i+1, len(waits)-1)] * time.Second
		if got := h.table.StartStatus(testKey).NextStartAt.Sub(h.clock.Now()); got != next {
			t.Fatalf("after wait %v: next backoff %v, want %v", w, got, next)
		}
	}

	h.docker.setStartErr(nil)
	h.clock.Advance(5 * time.Minute)
	h.mustScale(nil)
	st = h.table.StartStatus(testKey)
	if st.Failures != 0 || !st.NextStartAt.IsZero() || st.LastError == nil {
		t.Fatalf("after success: %+v, want 0 failures, no backoff, last error kept (R86)", st)
	}

	// The count starts over: two more failures do not back off.
	name := h.docker.startedNames()[len(h.docker.startedNames())-1]
	h.table.BeginExit(name)
	h.table.MarkRemoved(name)
	h.docker.setStartErr(func(string) error { return errTransient })
	h.mustScale(nil)
	h.mustScale(nil)
	if st := h.table.StartStatus(testKey); st.Failures != 2 || !st.NextStartAt.IsZero() {
		t.Fatalf("after reset: %+v, want 2 failures and no backoff", st)
	}
	h.mustScale(nil)
	if st := h.table.StartStatus(testKey); !st.NextStartAt.Equal(h.clock.Now().Add(10 * time.Second)) {
		t.Fatalf("after reset: backoff %v, want 10s again", st.NextStartAt.Sub(h.clock.Now()))
	}
}

// TestScaleCanceledContext: a start canceled by the Placer's context (finalize)
// is no start failure and ends the placement; Scale on a canceled context of
// its own returns the cancellation, so the message is not acknowledged (0002
// PLAN M14).
func TestScaleCanceledContext(t *testing.T) {
	h := newHarness(t, testTarget(2, 0))
	h.docker.duringStart = func(context.Context, string) { h.cancelPlacer() }
	h.docker.setStartErr(func(string) error { return context.Canceled })
	if err := h.s.Scale(context.Background(), statsMsg(1, 2)); err != nil {
		t.Fatalf("Scale = %v, want nil", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := h.s.Scale(ctx, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("Scale on a canceled context = %v, want context.Canceled", err)
	}
	if st := h.table.StartStatus(testKey); st.Failures != 0 {
		t.Fatalf("cancellation counted as a start failure: %+v", st)
	}
	if n := len(h.docker.startedNames()); n != 1 {
		t.Fatalf("starts %d, want 1 (stop after cancellation)", n)
	}
	if n := len(h.docker.removedNames()); n != 1 || len(h.gh.removedIDs()) != 1 {
		t.Fatal("canceled start not cleaned up")
	}
}

func TestNewRequiresDependencies(t *testing.T) {
	if _, err := New(Config{Target: testTarget(1, 0)}); err == nil {
		t.Fatal("New accepted missing dependencies")
	}
	h := newHarness(t, testTarget(1, 0))
	cfg := Config{
		Target: testTarget(0, 0), Table: h.table, Clock: h.clock, ScaleSet: h.gh, Containers: h.docker,
		Judge: h.judge, Tokens: h.tokens, Limits: h.limits, Fatal: func(error) {},
		Waiter: h.docker,
	}
	if _, err := New(cfg); err == nil {
		t.Fatal("New accepted max 0")
	}
	// Without a registry a start failure would be stored with the JIT config
	// and the token in clear (R31), so Secrets is required.
	cfg.Target = testTarget(1, 0)
	if _, err := New(cfg); err == nil || !strings.Contains(err.Error(), "Secrets") {
		t.Fatalf("New without Secrets = %v, want an error naming Secrets", err)
	}
	// A registry other than the Table's would leave the token unregistered.
	cfg.Secrets = &fakeSecrets{counts: make(map[string]int)}
	if _, err := New(cfg); err == nil || !strings.Contains(err.Error(), "NewTable") {
		t.Fatalf("New with a registry other than the Table's = %v, want an error naming NewTable", err)
	}
	cfg.Secrets = h.secrets
	// Without a waiter no runner would be watched (R6).
	cfg.Waiter = nil
	if _, err := New(cfg); err == nil || !strings.Contains(err.Error(), "Waiter") {
		t.Fatalf("New without Waiter = %v, want an error naming Waiter", err)
	}
	cfg.Waiter = h.docker
	if _, err := New(cfg); err != nil {
		t.Fatalf("New with every dependency = %v", err)
	}
}

// TestRemoveIdleCountsAsWork: a RemoveIdle in flight makes WaitInFlight wait.
func TestRemoveIdleCountsAsWork(t *testing.T) {
	h := newHarness(t, testTarget(2, 0))
	seed(t, h.table, "pool-a", 1, StateIdle, testEpoch)
	inRemove := make(chan struct{})
	release := make(chan struct{})
	h.gh.setRemoveErr(func(int64) error {
		close(inRemove)
		<-release
		return nil
	})
	h.table.SetDraining(DrainSignal)
	removed := make(chan error, 1)
	go func() { removed <- h.s.RemoveIdle(context.Background()) }()
	<-inRemove
	h.finalize()
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := h.s.WaitInFlight(canceled); !errors.Is(err, context.Canceled) {
		t.Fatalf("WaitInFlight = %v while RemoveIdle was in flight, want it to wait", err)
	}
	close(release)
	if err := <-removed; err != nil {
		t.Fatal(err)
	}
	if err := h.s.WaitInFlight(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// TestScaleDownSkipsRunnerBusyMeanwhile: a runner that took a job while
// scale-down was going through the candidates gets neither RemoveRunner nor
// any container removal (R37, R53).
func TestScaleDownSkipsRunnerBusyMeanwhile(t *testing.T) {
	t.Run("busy before its RemoveRunner", func(t *testing.T) {
		h := newHarness(t, testTarget(3, 0))
		seed(t, h.table, "pool-a", 1, StateIdle, testEpoch)
		seed(t, h.table, "pool-b", 2, StateIdle, testEpoch.Add(time.Second))
		h.gh.setRemoveErr(func(id int64) error {
			if id == 1 {
				h.table.MarkBusy("pool-b", h.clock.Now())
			}
			return nil
		})
		h.mustScale(statsMsg(1, 0))
		if got := h.gh.removedIDs(); !slices.Equal(got, []int64{1}) {
			t.Fatalf("RemoveRunner ids %v, want [1] only", got)
		}
		if got := h.docker.removedNames(); !slices.Equal(got, []string{"pool-a"}) {
			t.Fatalf("resources removed %v, want [pool-a]", got)
		}
		if h.state("pool-b") != StateBusy {
			t.Fatalf("pool-b %v, want busy", h.state("pool-b"))
		}
	})
	t.Run("busy during its RemoveRunner", func(t *testing.T) {
		h := newHarness(t, testTarget(2, 0))
		seed(t, h.table, "pool-a", 1, StateIdle, testEpoch)
		tok := mustToken(t)
		h.table.SetToken("pool-a", tok)
		h.gh.setRemoveErr(func(int64) error {
			h.table.MarkBusy("pool-a", h.clock.Now())
			return nil
		})
		h.mustScale(statsMsg(1, 0))
		if n := h.docker.calls(); n != 0 {
			t.Fatalf("Docker calls %d, want 0 for a runner that took a job", n)
		}
		if h.state("pool-a") != StateBusy {
			t.Fatalf("state %v, want busy", h.state("pool-a"))
		}
		if _, ok := h.table.Authenticate("pool-a", tok.Reveal(), testEpoch); !ok {
			t.Fatal("token of a busy runner revoked")
		}
		if !strings.Contains(h.logs.String(), "no longer idle") {
			t.Fatal("no log for the runner left to its watcher")
		}
	})
	t.Run("error drain, busy during a failing RemoveRunner", func(t *testing.T) {
		h := newHarness(t, testTarget(2, 0))
		seed(t, h.table, "pool-a", 1, StateIdle, testEpoch)
		h.table.SetDraining(DrainError)
		h.gh.setRemoveErr(func(int64) error {
			h.table.MarkBusy("pool-a", h.clock.Now())
			return errTransient
		})
		if err := h.s.RemoveIdle(context.Background()); err != nil {
			t.Fatal(err)
		}
		if n := h.docker.calls(); n != 0 {
			t.Fatalf("Docker calls %d, want no container removal (R53)", n)
		}
		if h.state("pool-a") != StateBusy {
			t.Fatalf("state %v, want busy", h.state("pool-a"))
		}
	})
}

// TestFinalizeDuringScaleDown: a RemoveRunner in flight when finalize
// begins keeps WaitInFlight waiting; the step it began ends within the
// finalize budget, the next candidate is never tried, and no Docker or
// GitHub call follows WaitInFlight (R47, M5-8).
func TestFinalizeDuringScaleDown(t *testing.T) {
	h := newHarness(t, testTarget(3, 0))
	seed(t, h.table, "pool-a", 1, StateIdle, testEpoch)
	seed(t, h.table, "pool-b", 2, StateIdle, testEpoch.Add(time.Second))
	inRemove := make(chan struct{})
	release := make(chan struct{})
	h.gh.setRemoveErr(func(id int64) error {
		if id == 1 {
			close(inRemove)
			<-release
		}
		return nil
	})
	scaled := make(chan error, 1)
	go func() { scaled <- h.scale(statsMsg(1, 0)) }()
	<-inRemove

	const budget = 5 * time.Second
	h.s.BeginFinalize(h.clock.Now().Add(budget))
	// With nothing in flight WaitInFlight returns nil even on a canceled
	// context; the Scale in flight must make it wait.
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := h.s.WaitInFlight(canceled); !errors.Is(err, context.Canceled) {
		t.Fatalf("WaitInFlight = %v while RemoveRunner was in flight, want it to wait", err)
	}
	waited := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		waited <- h.s.WaitInFlight(ctx)
	}()
	close(release)
	if err := <-scaled; !errors.Is(err, ErrFinalizing) {
		t.Fatalf("Scale = %v, want ErrFinalizing so the message is not acknowledged (R47)", err)
	}
	if err := <-waited; err != nil {
		t.Fatalf("WaitInFlight = %v", err)
	}

	if got := h.gh.removedIDs(); !slices.Equal(got, []int64{1}) {
		t.Fatalf("RemoveRunner ids %v, want [1]: no new step after finalize began", got)
	}
	left := h.docker.removeLeftTimes()
	if len(left) != 1 || left[0] <= 0 || left[0] > budget {
		t.Fatalf("time left on RemoveResources %v, want one call within the %v budget", left, budget)
	}

	docker, gh := h.docker.calls(), h.gh.calls()+h.acq.callCount()
	h.clock.Advance(time.Hour)
	if err := h.scale(withAvailable(statsMsg(2, 3), 9)); !errors.Is(err, ErrFinalizing) {
		t.Fatalf("Scale with available jobs = %v, want ErrFinalizing", err)
	}
	if err := h.s.RemoveIdle(context.Background()); err != nil {
		t.Fatal(err)
	}
	if h.docker.calls() != docker || h.gh.calls()+h.acq.callCount() != gh {
		t.Fatalf("calls after WaitInFlight: Docker %d→%d, GitHub %d→%d; want none (R47)",
			docker, h.docker.calls(), gh, h.gh.calls()+h.acq.callCount())
	}
}

// TestFinalizeDuringErrorDrainRemoval: finalize begins while R53's
// RemoveRunner fails; the container removal it then makes ends by the
// finalize deadline, the row is left abandoned, and the next idle runner is
// not tried.
func TestFinalizeDuringErrorDrainRemoval(t *testing.T) {
	h := newHarness(t, testTarget(3, 0))
	seed(t, h.table, "pool-a", 1, StateIdle, testEpoch)
	seed(t, h.table, "pool-b", 2, StateIdle, testEpoch.Add(time.Second))
	h.table.SetDraining(DrainError)
	const budget = 5 * time.Second
	h.gh.setRemoveErr(func(int64) error {
		h.s.BeginFinalize(h.clock.Now().Add(budget))
		return errTransient
	})
	if err := h.s.RemoveIdle(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := h.gh.removedIDs(); !slices.Equal(got, []int64{1}) {
		t.Fatalf("RemoveRunner ids %v, want [1]", got)
	}
	left := h.docker.runnerRemovedLeftTimes()
	if len(left) != 1 || left[0] <= 0 || left[0] > budget {
		t.Fatalf("time left on RemoveContainer %v, want one call within the %v budget", left, budget)
	}
	info, _ := h.table.Get("pool-a")
	if info.State != StateExiting || !info.Abandoned {
		t.Fatalf("row %+v, want exiting and abandoned for finalize and reconciliation", info)
	}
	if !h.table.ClaimAbandoned("pool-a") {
		t.Fatal("abandoned row not claimable")
	}
}

// TestPermanentAcquireSetsErrorDrain: the Scale that gets a permanent
// AcquireJobs error sets the target's error drain mark itself, so the next
// Scale starts nothing even before the daemon's drain begins (R51), and
// only this target drains (0002 SPEC R171). A signal drain keeps its kind
// (R100).
func TestPermanentAcquireSetsErrorDrain(t *testing.T) {
	t.Run("no drain yet", func(t *testing.T) {
		h := newHarness(t, testTarget(2, 1))
		h.acq.err = errPermanent
		if err := h.scale(withAvailable(statsMsg(1, 2), 9)); err != nil {
			t.Fatalf("Scale = %v, want nil (R51)", err)
		}
		if k := h.table.Drain(); k != DrainNone || !h.table.TargetDraining(h.s.target.Key) {
			t.Fatalf("server drain %v, target drain %v; want none and set", k, h.table.TargetDraining(h.s.target.Key))
		}
		h.acq.err = nil
		h.mustScale(statsMsg(2, 2))
		h.mustScale(nil)
		if n := len(h.docker.startedNames()); n != 0 {
			t.Fatalf("StartRunner calls %d after a permanent AcquireJobs error, want 0 (R51)", n)
		}
		if h.gh.jitCalls() != 0 {
			t.Fatal("JIT called after a permanent AcquireJobs error")
		}
		if h.fatalCount() != 1 {
			t.Fatalf("fatal reports %d, want 1", h.fatalCount())
		}
	})
	t.Run("drain set before Fatal", func(t *testing.T) {
		h := newHarness(t, testTarget(2, 1))
		h.acq.err = errPermanent
		if err := h.scale(withAvailable(statsMsg(1, 2), 9)); err != nil {
			t.Fatalf("Scale = %v, want nil (R51)", err)
		}
		if got := h.fatalDrains(); len(got) != 1 || !got[0] {
			t.Fatalf("target drain mark at Fatal %v, want set before the call", got)
		}
	})
	t.Run("signal drain", func(t *testing.T) {
		// During a signal drain no AcquireJobs is called at all (0002 SPEC
		// R220), so no permanent error can arise from it.
		h := newHarness(t, testTarget(2, 0))
		h.table.SetDraining(DrainSignal)
		h.acq.err = errPermanent
		if err := h.scale(withAvailable(statsMsg(1, 2), 9)); err != nil {
			t.Fatalf("Scale = %v, want nil", err)
		}
		if k := h.table.Drain(); k != DrainSignal {
			t.Fatalf("drain %v, want signal kept (R100)", k)
		}
		if h.acq.callCount() != 0 || h.fatalCount() != 0 {
			t.Fatalf("AcquireJobs %d, fatal %d during a signal drain; want 0 and 0", h.acq.callCount(), h.fatalCount())
		}
	})
}

// TestScaleFinalizingNotAcked: once finalize has begun every Scale returns
// ErrFinalizing, so the listener never acknowledges the message with
// DeleteMessage (R47), whatever the message holds.
func TestScaleFinalizingNotAcked(t *testing.T) {
	h := newHarness(t, testTarget(2, 0))
	seed(t, h.table, "pool-a", 1, StateIdle, testEpoch)
	h.finalize()
	for _, msg := range []*scaleset.RunnerScaleSetMessage{
		nil,
		statsMsg(1, 0),
		statsMsg(2, 2),
		withCompleted(statsMsg(3, 0), "pool-a", "j1"),
		withAvailable(statsMsg(4, 1), 9),
	} {
		h.scaleFinalizing(msg)
	}
	if n := h.docker.calls() + h.gh.calls() + h.acq.callCount(); n != 0 {
		t.Fatalf("calls %d during finalize, want 0", n)
	}
}

// TestJobStartedDuringFinalize covers the finalize branches of jobStarted:
// no Reject, no BusyFunc; finalize removes the runner itself.
func TestJobStartedDuringFinalize(t *testing.T) {
	t.Run("recorded", func(t *testing.T) {
		h := newHarness(t, testTarget(2, 0))
		seed(t, h.table, "pool-a", 1, StateIdle, testEpoch)
		h.finalize()
		h.scaleFinalizing(withStarted(statsMsg(1, 1), "pool-a", "j1"))
		if h.state("pool-a") != StateBusy {
			t.Fatalf("state %v, want busy", h.state("pool-a"))
		}
		if got := h.busyCalls(); len(got) != 0 {
			t.Fatalf("busy callbacks %v during finalize, want none", got)
		}
	})
	t.Run("done runner gets another job", func(t *testing.T) {
		h := newHarness(t, testTarget(2, 0))
		seed(t, h.table, "pool-a", 1, StateIdle, testEpoch)
		tok := mustToken(t)
		h.table.SetToken("pool-a", tok)
		h.mustScale(withCompleted(statsMsg(1, 0), "pool-a", "j1"))
		h.finalize()
		h.scaleFinalizing(withStarted(statsMsg(2, 1), "pool-a", "j2"))
		if n := len(h.judge.rejectedNames()); n != 0 {
			t.Fatalf("Reject calls %d during finalize, want 0", n)
		}
		if _, ok := h.table.Authenticate("pool-a", tok.Reveal(), testEpoch); ok {
			t.Fatal("token still valid (R102)")
		}
		if !strings.Contains(h.logs.String(), "left to finalize") {
			t.Fatal("no log for the runner left to finalize")
		}
	})
}

// TestJobStartedSecondJob: a live runner that recorded one job keeps it when
// JobStarted names another.
func TestJobStartedSecondJob(t *testing.T) {
	h := newHarness(t, testTarget(2, 0))
	seed(t, h.table, "pool-a", 1, StateIdle, testEpoch)
	h.mustScale(withStarted(statsMsg(1, 1), "pool-a", "j1"))
	h.mustScale(withStarted(statsMsg(2, 1), "pool-a", "j2"))
	info, _ := h.table.Get("pool-a")
	if info.Job == nil || info.Job.ID != "j1" || info.State != StateBusy {
		t.Fatalf("row %+v, want busy with job j1", info)
	}
	if len(h.judge.rejectedNames()) != 0 {
		t.Fatal("rejected a busy runner")
	}
	if !strings.Contains(h.logs.String(), "second job") {
		t.Fatal("no log for the second job")
	}
}

// TestErrorDrainRemoveContainerOutcomes: the reason the error drain's row is
// abandoned follows the runner container removal (R42, R53).
func TestErrorDrainRemoveContainerOutcomes(t *testing.T) {
	for _, tt := range []struct {
		name      string
		err       error
		reason    AbandonReason
		claimable bool
		recorded  int
	}{
		{"removed", nil, AbandonErrorDrain, true, 1},
		{"not found counts as removed", statusError(404), AbandonErrorDrain, true, 1},
		{"transient", errTransient, AbandonErrorDrain, true, 2},
		{"permanent", statusError(400), AbandonPermanent, false, 2},
	} {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t, testTarget(2, 0))
			seed(t, h.table, "pool-a", 1, StateIdle, testEpoch)
			h.gh.setRemoveErr(func(int64) error { return errTransient })
			h.docker.removeContainerErr = func(string) error { return tt.err }
			h.table.SetDraining(DrainError)
			if err := h.s.RemoveIdle(context.Background()); err != nil {
				t.Fatal(err)
			}
			info, _ := h.table.Get("pool-a")
			if info.State != StateExiting || info.AbandonReason != tt.reason {
				t.Fatalf("row %+v, want exiting abandoned as %v", info, tt.reason)
			}
			if h.errs.count() != tt.recorded {
				t.Fatalf("recorded %d, want %d", h.errs.count(), tt.recorded)
			}
			if got := h.table.ClaimAbandoned("pool-a"); got != tt.claimable {
				t.Fatalf("ClaimAbandoned = %v, want %v", got, tt.claimable)
			}
		})
	}
}

// otherTarget is a second target for the tests of the shared table.
func otherTarget(maxRunners, idle int) TargetConfig {
	return TargetConfig{Key: "other", Label: "pool", MaxRunners: maxRunners, Idle: idle}
}

// TestConvergeTwoTargetsConcurrent: two targets' Scale calls run in
// parallel on one table; the runners of both never pass the server limit,
// each target stays within its max-runners, and a start refused for lack of
// room is not a start failure (0002 SPEC R114, 0002 PLAN M11-4).
func TestConvergeTwoTargetsConcurrent(t *testing.T) {
	const limit = 3
	a := newLimitHarness(t, testTarget(3, 0), limit)
	b := newTargetHarness(t, a, otherTarget(3, 0))
	var over atomic.Int32
	a.docker.mu.Lock()
	a.docker.duringStart = func(context.Context, string) {
		if a.table.CountsAll().All() > limit {
			over.Add(1)
		}
	}
	a.docker.mu.Unlock()

	var wg sync.WaitGroup
	for _, h := range []*harness{a, b} {
		wg.Go(func() {
			for i := range 5 {
				if err := h.s.Scale(context.Background(), statsMsg(i+1, 3)); err != nil {
					t.Errorf("Scale: %v", err)
				}
			}
		})
	}
	wg.Wait()

	ca, cb := a.table.Counts(testKey), a.table.Counts("other")
	if over.Load() != 0 || ca.All()+cb.All() != limit || ca.All() > 3 || cb.All() > 3 {
		t.Fatalf("runners a %d b %d (over the limit during a start %d times); want %d in all",
			ca.All(), cb.All(), over.Load(), limit)
	}
	for _, key := range []string{testKey, "other"} {
		if st := a.table.StartStatus(key); st.Failures != 0 || st.LastError != nil {
			t.Errorf("start status of %s = %+v; ErrNoRoom must not count (M11-4)", key, st)
		}
	}
}

// TestConvergeServerFullStartsNothing: a target with room under its own
// max-runners starts nothing while the other target's runners fill the
// server, and calls no JIT (0002 SPEC R114).
func TestConvergeServerFullStartsNothing(t *testing.T) {
	a := newLimitHarness(t, testTarget(2, 0), 2)
	b := newTargetHarness(t, a, otherTarget(2, 0))
	a.mustScale(statsMsg(1, 2))
	if n := a.table.Counts(testKey).All(); n != 2 {
		t.Fatalf("a has %d runners, want 2", n)
	}
	b.mustScale(statsMsg(1, 2))
	if n := a.table.Counts("other").All(); n != 0 || b.gh.jitCalls() != 0 {
		t.Fatalf("b started %d runners with %d JIT calls on a full server", n, b.gh.jitCalls())
	}
}

// TestScaleOnlyOwnTarget: a target's messages and drain scale-down never
// touch the rows of another target (0002 PLAN M11-4, M11-5); the placement
// a's Scale runs keeps b's runners within b's goal (idle 2).
func TestScaleOnlyOwnTarget(t *testing.T) {
	a := newHarness(t, testTarget(2, 0))
	b := newTargetHarness(t, a, otherTarget(2, 2))
	seedFor(t, a.table, "other", "pool-b", 2, StateIdle, testEpoch)
	seedFor(t, a.table, "other", "pool-c", 3, StateBusy, testEpoch)

	msg := withStarted(statsMsg(1, 0), "pool-b", "j1")
	msg = withCompleted(msg, "pool-c", "j2")
	a.mustScale(msg)
	if b.state("pool-b") != StateIdle || b.state("pool-c") != StateBusy || len(a.gh.removedIDs()) != 0 {
		t.Fatalf("a's message or scale-down changed b's rows: b=%v c=%v removed %v",
			b.state("pool-b"), b.state("pool-c"), a.gh.removedIDs())
	}
	a.table.SetTargetDraining(testKey)
	if err := a.s.RemoveIdle(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(a.gh.removedIDs()) != 0 || len(b.gh.removedIDs()) != 0 {
		t.Fatalf("a's drain RemoveIdle removed a %v b %v", a.gh.removedIDs(), b.gh.removedIDs())
	}
}

// TestStartNoRoomNotFailure: when another target takes the last slot after
// converge counted the room, Table.Add refuses the start with ErrNoRoom; the
// start loop ends without a JIT call and without counting a start failure
// (0002 PLAN M11-4).
func TestStartNoRoomNotFailure(t *testing.T) {
	a := newLimitHarness(t, testTarget(2, 0), 2)
	a.docker.mu.Lock()
	a.docker.duringStart = func(context.Context, string) {
		seedFor(t, a.table, "other", "pool-taken", 9, StateIdle, testEpoch)
	}
	a.docker.mu.Unlock()
	a.mustScale(statsMsg(1, 2))
	if n := a.table.CountsAll().All(); n != 2 || a.gh.jitCalls() != 1 {
		t.Fatalf("rows %d, JIT calls of a %d; want 2 rows and 1 JIT call", n, a.gh.jitCalls())
	}
	if st := a.table.StartStatus(testKey); st.Failures != 0 || st.LastError != nil {
		t.Fatalf("start status = %+v, want no failure", st)
	}
	if !strings.Contains(a.logs.String(), "no room under max-runners or the server limit") {
		t.Fatal("no log for the refused start")
	}
}
