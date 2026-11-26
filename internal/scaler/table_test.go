package scaler

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dewevrei/actions-runner-scaler/internal/gate"
)

// race runs f from n goroutines released at once and returns how many
// returned true.
func race(n int, f func() bool) int {
	var wins atomic.Int32
	var ready, done sync.WaitGroup
	start := make(chan struct{})
	ready.Add(n)
	done.Add(n)
	for range n {
		go func() {
			defer done.Done()
			ready.Done()
			<-start
			if f() {
				wins.Add(1)
			}
		}()
	}
	ready.Wait()
	close(start)
	done.Wait()
	return int(wins.Load())
}

func TestBeginExitConcurrentCleansUpOnce(t *testing.T) {
	for _, st := range []State{StateStarting, StateIdle, StateBusy, StateDone} {
		t.Run(st.String(), func(t *testing.T) {
			tb := NewTable(nil, nil)
			seed(t, tb, "r", 1, st, testEpoch)
			var cleanups atomic.Int32
			wins := race(32, func() bool {
				if tb.BeginExit("r") {
					cleanups.Add(1)
					return true
				}
				return false
			})
			if wins != 1 || cleanups.Load() != 1 {
				t.Fatalf("winners %d, cleanups %d; want 1 and 1", wins, cleanups.Load())
			}
			if got, _ := tb.Get("r"); got.State != StateExiting {
				t.Fatalf("state %v, want exiting", got.State)
			}
		})
	}
}

func TestBeginExitLosesOnceEnding(t *testing.T) {
	tb := NewTable(nil, nil)
	seed(t, tb, "r", 1, 0, testEpoch)
	if tb.BeginExit("r") {
		t.Fatal("BeginExit won on a removed runner")
	}
	if tb.BeginExit("missing") {
		t.Fatal("BeginExit won on an unknown runner")
	}
}

func TestVerdictTransitions(t *testing.T) {
	t.Run("allow then deny", func(t *testing.T) {
		tb := NewTable(nil, nil)
		seed(t, tb, "r", 1, StateBusy, testEpoch)
		if !tb.Allow("r") {
			t.Fatal("Allow failed from pending")
		}
		if tb.Deny("r") {
			t.Fatal("Deny succeeded after allow")
		}
		if got, _ := tb.Get("r"); got.Verdict != VerdictAllow {
			t.Fatalf("verdict %v, want allow", got.Verdict)
		}
	})
	t.Run("deny then allow", func(t *testing.T) {
		tb := NewTable(nil, nil)
		seed(t, tb, "r", 1, StateBusy, testEpoch)
		if !tb.Deny("r") {
			t.Fatal("Deny failed from pending")
		}
		if tb.Allow("r") {
			t.Fatal("Allow succeeded after deny")
		}
	})
	t.Run("allow after BeginExit stays pending", func(t *testing.T) {
		tb := NewTable(nil, nil)
		seed(t, tb, "r", 1, StateBusy, testEpoch)
		tok := mustToken(t)
		tb.SetToken("r", tok)
		tb.BeginExit("r")
		if tb.Allow("r") {
			t.Fatal("Allow succeeded after BeginExit")
		}
		v, ok := tb.Authenticate("r", tok.Reveal(), testEpoch)
		if !ok || v != VerdictPending {
			t.Fatalf("Authenticate = %v, %v; want pending, true", v, ok)
		}
		// The deny path records deny after removing the container.
		if !tb.Deny("r") {
			t.Fatal("Deny failed on an exiting runner from pending")
		}
	})
	t.Run("allow needs idle or busy", func(t *testing.T) {
		tb := NewTable(nil, nil)
		seed(t, tb, "s", 1, StateStarting, testEpoch)
		seed(t, tb, "d", 2, StateDone, testEpoch)
		seed(t, tb, "i", 3, StateIdle, testEpoch)
		if tb.Allow("s") || tb.Allow("d") {
			t.Fatal("Allow succeeded on starting or done")
		}
		if !tb.Allow("i") {
			t.Fatal("Allow failed on idle")
		}
	})
	t.Run("concurrent allow and deny pick one", func(t *testing.T) {
		tb := NewTable(nil, nil)
		seed(t, tb, "r", 1, StateBusy, testEpoch)
		var i atomic.Int32
		wins := race(32, func() bool {
			if i.Add(1)%2 == 0 {
				return tb.Allow("r")
			}
			return tb.Deny("r")
		})
		if wins != 1 {
			t.Fatalf("verdict changes %d, want 1", wins)
		}
	})
}

func TestMarkBusyFirstCallOnly(t *testing.T) {
	var calls atomic.Int32
	var gotAt time.Time
	var mu sync.Mutex
	tb := NewTable(func(name string, at time.Time) {
		calls.Add(1)
		mu.Lock()
		gotAt = at
		mu.Unlock()
	}, nil)
	seed(t, tb, "r", 1, StateIdle, testEpoch)
	var n atomic.Int64
	wins := race(32, func() bool {
		at := testEpoch.Add(time.Duration(n.Add(1)) * time.Second)
		return tb.MarkBusy("r", at)
	})
	if wins != 1 || calls.Load() != 1 {
		t.Fatalf("first calls %d, callbacks %d; want 1 and 1", wins, calls.Load())
	}
	info, _ := tb.Get("r")
	mu.Lock()
	defer mu.Unlock()
	if !info.BusyAt.Equal(gotAt) || info.State != StateBusy {
		t.Fatalf("busy at %v state %v; callback at %v", info.BusyAt, info.State, gotAt)
	}
	if tb.MarkBusy("r", testEpoch.Add(time.Hour)) {
		t.Fatal("second MarkBusy reported first")
	}
	if again, _ := tb.Get("r"); !again.BusyAt.Equal(info.BusyAt) {
		t.Fatal("second MarkBusy changed the time")
	}
}

func TestMarkBusyIgnoresDoneAndEnding(t *testing.T) {
	for _, st := range []State{StateDone, StateExiting, 0} {
		var calls atomic.Int32
		tb := NewTable(func(string, time.Time) { calls.Add(1) }, nil)
		seed(t, tb, "r", 1, st, testEpoch)
		if tb.MarkBusy("r", testEpoch) || calls.Load() != 0 {
			t.Errorf("%v: MarkBusy acted", st)
		}
		got, ok := tb.Get("r")
		if st == 0 {
			if ok {
				t.Errorf("removed runner back in the table: %+v", got)
			}
			continue
		}
		if got.State != st || !got.BusyAt.IsZero() {
			t.Errorf("%v: row changed to %v busy %v", st, got.State, got.BusyAt)
		}
	}
}

// TestMarkBusyUnjudged: the discovery mark of R83 and R103 marks an idle
// (or starting) runner busy once and calls the BusyFunc, and does nothing
// for a runner with an allow verdict, a done runner, one already marked
// busy, one whose removal began, or an unknown name.
func TestMarkBusyUnjudged(t *testing.T) {
	later := testEpoch.Add(time.Minute)
	tests := []struct {
		name  string
		setup func(t *testing.T, tb *Table)
		// want is MarkBusyUnjudged's result, which is also whether the
		// BusyFunc is called.
		want      bool
		wantState State
	}{
		{
			name:      "idle",
			setup:     func(t *testing.T, tb *Table) { seed(t, tb, "r", 1, StateIdle, testEpoch) },
			want:      true,
			wantState: StateBusy,
		},
		{
			name:      "starting",
			setup:     func(t *testing.T, tb *Table) { seed(t, tb, "r", 1, StateStarting, testEpoch) },
			want:      true,
			wantState: StateBusy,
		},
		{
			name: "allow verdict",
			setup: func(t *testing.T, tb *Table) {
				seed(t, tb, "r", 1, StateIdle, testEpoch)
				if !tb.Allow("r") {
					t.Fatal("Allow failed")
				}
			},
			wantState: StateIdle,
		},
		{
			name:      "done",
			setup:     func(t *testing.T, tb *Table) { seed(t, tb, "r", 1, StateDone, testEpoch) },
			wantState: StateDone,
		},
		{
			name:      "already busy",
			setup:     func(t *testing.T, tb *Table) { seed(t, tb, "r", 1, StateBusy, testEpoch) },
			wantState: StateBusy,
		},
		{
			name:      "exiting",
			setup:     func(t *testing.T, tb *Table) { seed(t, tb, "r", 1, StateExiting, testEpoch) },
			wantState: StateExiting,
		},
		{
			name:  "unknown",
			setup: func(*testing.T, *Table) {},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var calls atomic.Int32
			tb := NewTable(func(string, time.Time) { calls.Add(1) }, nil)
			tt.setup(t, tb)
			before, _ := tb.Get("r")
			busyCalls := calls.Load()
			if got := tb.MarkBusyUnjudged("r", later); got != tt.want {
				t.Fatalf("MarkBusyUnjudged = %v, want %v", got, tt.want)
			}
			if n := calls.Load() - busyCalls; (n == 1) != tt.want || n > 1 {
				t.Fatalf("BusyFunc calls %d, want %v", n, tt.want)
			}
			after, ok := tb.Get("r")
			if tt.wantState == 0 {
				if ok {
					t.Fatalf("unknown runner added: %+v", after)
				}
				return
			}
			if after.State != tt.wantState {
				t.Fatalf("state %v, want %v", after.State, tt.wantState)
			}
			switch {
			case tt.want && !after.BusyAt.Equal(later):
				t.Fatalf("busy at %v, want %v", after.BusyAt, later)
			case !tt.want && !after.BusyAt.Equal(before.BusyAt):
				t.Fatalf("busy time changed from %v to %v", before.BusyAt, after.BusyAt)
			}
			if tb.MarkBusyUnjudged("r", later.Add(time.Minute)) {
				t.Fatal("second MarkBusyUnjudged reported first")
			}
		})
	}
}

// TestMarkBusyUnjudgedDuringFinalize: once finalize has begun the first mark
// still records busy but does not call the BusyFunc (M5-8).
func TestMarkBusyUnjudgedDuringFinalize(t *testing.T) {
	var calls atomic.Int32
	tb := NewTable(func(string, time.Time) { calls.Add(1) }, nil)
	seed(t, tb, "r", 1, StateIdle, testEpoch)
	tb.BeginFinalize(testEpoch.Add(time.Minute))
	if !tb.MarkBusyUnjudged("r", testEpoch) || calls.Load() != 0 {
		t.Fatalf("BusyFunc calls %d during finalize, want 0 and a first mark", calls.Load())
	}
	if got, _ := tb.Get("r"); got.State != StateBusy {
		t.Fatalf("state %v, want busy", got.State)
	}
}

// TestBusyFuncMayCallTable checks that the BusyFunc runs without the
// table's mutex: a callback that reads the table must not deadlock.
func TestBusyFuncMayCallTable(t *testing.T) {
	var tb *Table
	var seen RunnerInfo
	tb = NewTable(func(name string, at time.Time) {
		seen, _ = tb.Get(name)
		tb.Counts()
	}, nil)
	seed(t, tb, "a", 1, StateIdle, testEpoch)
	seed(t, tb, "b", 2, StateIdle, testEpoch)
	tok := mustToken(t)
	tb.SetToken("b", tok)

	done := make(chan struct{})
	go func() {
		defer close(done)
		tb.MarkBusy("a", testEpoch)
		if seen.Name != "a" || seen.State != StateBusy {
			t.Errorf("callback saw %+v, want a busy", seen)
		}
		tb.Authenticate("b", tok.Reveal(), testEpoch)
		if seen.Name != "b" || seen.State != StateBusy {
			t.Errorf("callback saw %+v, want b busy", seen)
		}
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("BusyFunc calling the table deadlocked")
	}
}

func TestAuthenticate(t *testing.T) {
	var calls atomic.Int32
	tb := NewTable(func(string, time.Time) { calls.Add(1) }, nil)
	seed(t, tb, "a", 1, StateIdle, testEpoch)
	seed(t, tb, "b", 2, StateIdle, testEpoch)
	tokA := mustToken(t)
	tb.SetToken("a", tokA)

	if _, ok := tb.Authenticate("a", "wrong", testEpoch); ok {
		t.Fatal("wrong token accepted")
	}
	if _, ok := tb.Authenticate("b", tokA.Reveal(), testEpoch); ok {
		t.Fatal("token accepted for another runner (R14)")
	}
	if calls.Load() != 0 {
		t.Fatal("failed authentication recorded busy (R40)")
	}
	for range 3 {
		v, ok := tb.Authenticate("a", tokA.Reveal(), testEpoch)
		if !ok || v != VerdictPending {
			t.Fatalf("Authenticate = %v, %v", v, ok)
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("busy callbacks %d, want 1", calls.Load())
	}
	tb.RevokeToken("a")
	if _, ok := tb.Authenticate("a", tokA.Reveal(), testEpoch); ok {
		t.Fatal("revoked token accepted (R14)")
	}
}

func TestMarkRemovedRevokesToken(t *testing.T) {
	secrets := &fakeSecrets{counts: make(map[string]int)}
	tb := NewTable(nil, nil)
	seed(t, tb, "r", 1, StateExiting, testEpoch)
	tok := mustToken(t)
	tb.SetToken("r", tok) // refused: removal has begun
	if _, ok := tb.Authenticate("r", tok.Reveal(), testEpoch); ok {
		t.Fatal("SetToken worked on an exiting runner")
	}

	tb2 := NewTable(nil, secrets)
	seed(t, tb2, "r", 1, StateIdle, testEpoch)
	tb2.SetToken("r", tok)
	tb2.BeginExit("r")
	if !tb2.MarkRemoved("r") {
		t.Fatal("MarkRemoved failed")
	}
	if _, ok := tb2.Authenticate("r", tok.Reveal(), testEpoch); ok {
		t.Fatal("token valid after removal (R38)")
	}
	if tb2.MarkRemoved("r") {
		t.Fatal("MarkRemoved true twice")
	}
	if secrets.live() != 0 {
		t.Fatalf("%d secrets still registered after removal", secrets.live())
	}
}

// TestRemovedRowLeavesTable checks that MarkRemoved deletes the row and that
// every lookup by the name then acts as for a name never added.
func TestRemovedRowLeavesTable(t *testing.T) {
	tb := NewTable(nil, nil)
	seed(t, tb, "r", 1, StateBusy, testEpoch)
	tok := mustToken(t)
	tb.SetToken("r", tok)
	tb.RecordJobStarted("r", Job{ID: "j"})
	tb.BeginExit("r")
	tb.MarkRemoved("r")

	for _, name := range []string{"r", "never-added"} {
		if _, ok := tb.Get(name); ok {
			t.Errorf("%s: Get found a row", name)
		}
		if v, ok := tb.Authenticate(name, tok.Reveal(), testEpoch); ok || v != VerdictPending {
			t.Errorf("%s: Authenticate = %v, %v; want pending, false", name, v, ok)
		}
		if got := tb.RecordJobStarted(name, Job{ID: "j"}); got != JobStartedUnknown {
			t.Errorf("%s: RecordJobStarted = %v, want unknown", name, got)
		}
		if got := tb.RecordJobCompleted(name, "j", testEpoch); got != JobCompletedUnknown {
			t.Errorf("%s: RecordJobCompleted = %v, want unknown", name, got)
		}
		if tb.MarkBusy(name, testEpoch) || tb.Allow(name) || tb.Deny(name) || tb.BeginExit(name) {
			t.Errorf("%s: a transition succeeded", name)
		}
	}
	if len(tb.List()) != 0 || tb.Counts() != (Counts{}) {
		t.Fatal("removed row still listed or counted")
	}
	// The name may be used again.
	if err := tb.Add("r", testEpoch); err != nil {
		t.Fatalf("Add after removal = %v", err)
	}
}

// TestTokenSecretLifetime checks that the job check token is registered as a
// secret from SetToken until it is revoked, and only once (R31).
func TestTokenSecretLifetime(t *testing.T) {
	secrets := &fakeSecrets{counts: make(map[string]int)}
	tb := NewTable(nil, secrets)
	if err := tb.Add("r", testEpoch); err != nil {
		t.Fatal(err)
	}
	first, second := mustToken(t), mustToken(t)
	tb.SetToken("r", first)
	tb.SetToken("r", second)
	if secrets.counts[first.Reveal()] != 0 || secrets.counts[second.Reveal()] != 1 {
		t.Fatal("replaced token still registered or new one not registered")
	}
	tb.RevokeToken("r")
	tb.RevokeToken("r")
	if secrets.live() != 0 {
		t.Fatalf("%d secrets registered after revoke", secrets.live())
	}

	tb.BeginExit("r")
	if tb.SetToken("r", first) || secrets.live() != 0 {
		t.Fatal("SetToken registered a token for an exiting runner")
	}
}

func TestJobCompletedFirstTimeKept(t *testing.T) {
	tb := NewTable(nil, nil)
	seed(t, tb, "r", 1, StateBusy, testEpoch)
	tb.RecordJobStarted("r", Job{ID: "j1"})
	first := testEpoch.Add(time.Minute)
	if got := tb.RecordJobCompleted("r", "j1", first); got != JobCompletedDone {
		t.Fatalf("first JobCompleted = %v", got)
	}
	if got := tb.RecordJobCompleted("r", "j1", first.Add(4*time.Minute)); got != JobCompletedAlreadyDone {
		t.Fatalf("redelivered JobCompleted = %v", got)
	}
	info, _ := tb.Get("r")
	if !info.CompletedAt.Equal(first) || info.State != StateDone {
		t.Fatalf("completed at %v state %v; want %v done (R101)", info.CompletedAt, info.State, first)
	}
}

func TestJobCompletedOtherJobLeavesState(t *testing.T) {
	tb := NewTable(nil, nil)
	seed(t, tb, "r", 1, StateBusy, testEpoch)
	tb.RecordJobStarted("r", Job{ID: "j1"})
	if got := tb.RecordJobCompleted("r", "j2", testEpoch); got != JobCompletedOtherJob {
		t.Fatalf("got %v, want other job (R102)", got)
	}
	info, _ := tb.Get("r")
	if info.State != StateBusy || !info.CompletedAt.IsZero() {
		t.Fatalf("state %v completed %v; want busy and zero", info.State, info.CompletedAt)
	}
}

func TestJobStartedOnDoneRunner(t *testing.T) {
	tb := NewTable(nil, nil)
	seed(t, tb, "r", 1, StateIdle, testEpoch)
	// JobCompleted without JobStarted: the completed job ID is the reference.
	tb.RecordJobCompleted("r", "j1", testEpoch)
	tok := mustToken(t)
	tb.SetToken("r", tok)
	if got := tb.RecordJobStarted("r", Job{ID: "j1"}); got != JobStartedDoneSame {
		t.Fatalf("same job = %v, want done same", got)
	}
	if _, ok := tb.Authenticate("r", tok.Reveal(), testEpoch); !ok {
		t.Fatal("token revoked for the runner's own job")
	}
	if got := tb.RecordJobStarted("r", Job{ID: "j2"}); got != JobStartedDoneOther {
		t.Fatalf("other job = %v, want done other", got)
	}
	if got, _ := tb.Get("r"); got.State != StateDone {
		t.Fatalf("state %v, want done", got.State)
	}
	if _, ok := tb.Authenticate("r", tok.Reveal(), testEpoch); ok {
		t.Fatal("token still valid after JobStarted for another job (R102)")
	}
}

func TestJobStartedKeepsFirstJob(t *testing.T) {
	tb := NewTable(nil, nil)
	seed(t, tb, "r", 1, StateIdle, testEpoch)
	if got := tb.RecordJobStarted("r", Job{ID: "j1"}); got != JobStartedRecorded {
		t.Fatalf("first = %v", got)
	}
	if got := tb.RecordJobStarted("r", Job{ID: "j1"}); got != JobStartedRecorded {
		t.Fatalf("redelivered = %v", got)
	}
	if got := tb.RecordJobStarted("r", Job{ID: "j2"}); got != JobStartedOtherJob {
		t.Fatalf("second job = %v", got)
	}
	if info, _ := tb.Get("r"); info.Job == nil || info.Job.ID != "j1" {
		t.Fatalf("job %+v, want j1", info.Job)
	}
}

func TestRemovalCandidates(t *testing.T) {
	tb := NewTable(nil, nil)
	seed(t, tb, "new", 1, StateIdle, testEpoch.Add(2*time.Minute))
	seed(t, tb, "old", 2, StateIdle, testEpoch)
	seed(t, tb, "mid", 3, StateIdle, testEpoch.Add(time.Minute))
	seed(t, tb, "busy", 4, StateBusy, testEpoch)
	seed(t, tb, "done", 5, StateDone, testEpoch)
	seed(t, tb, "starting", 6, StateStarting, testEpoch)
	tb.GiveUpRemoval("mid")

	got := tb.RemovalCandidates()
	var names []string
	for _, r := range got {
		names = append(names, r.Name)
	}
	if len(names) != 2 || names[0] != "old" || names[1] != "new" {
		t.Fatalf("candidates %v, want [old new] (R37 idle only, R94 given up skipped)", names)
	}
}

func TestCounts(t *testing.T) {
	tb := NewTable(nil, nil)
	seed(t, tb, "s", 1, StateStarting, testEpoch)
	seed(t, tb, "i", 2, StateIdle, testEpoch)
	seed(t, tb, "b", 3, StateBusy, testEpoch)
	seed(t, tb, "d", 4, StateDone, testEpoch)
	seed(t, tb, "e", 5, StateExiting, testEpoch)
	seed(t, tb, "r", 6, 0, testEpoch)
	c := tb.Counts()
	want := Counts{Starting: 1, Idle: 1, Busy: 1, Done: 1, Exiting: 1}
	if c != want {
		t.Fatalf("counts %+v, want %+v (removed not counted)", c, want)
	}
	if c.Live() != 3 || c.All() != 5 {
		t.Fatalf("live %d all %d; want 3 and 5 (done only in R3's count)", c.Live(), c.All())
	}
}

func TestMarkStarted(t *testing.T) {
	tb := NewTable(nil, nil)
	seed(t, tb, "s", 1, StateStarting, testEpoch)
	if !tb.MarkStarted("s") || mustState(t, tb, "s") != StateIdle {
		t.Fatal("starting runner did not become idle")
	}
	seed(t, tb, "b", 2, StateStarting, testEpoch)
	tb.MarkBusy("b", testEpoch)
	if !tb.MarkStarted("b") || mustState(t, tb, "b") != StateBusy {
		t.Fatal("busy runner changed by MarkStarted")
	}
	seed(t, tb, "e", 3, StateStarting, testEpoch)
	tb.BeginExit("e")
	if tb.MarkStarted("e") {
		t.Fatal("MarkStarted true after BeginExit")
	}
}

func TestBeginDoneCleanupOnce(t *testing.T) {
	tb := NewTable(nil, nil)
	seed(t, tb, "d", 1, StateDone, testEpoch)
	seed(t, tb, "b", 2, StateBusy, testEpoch)
	if wins := race(16, func() bool { return tb.BeginDoneCleanup("d") }); wins != 1 {
		t.Fatalf("winners %d, want 1 (R101)", wins)
	}
	if tb.BeginDoneCleanup("b") {
		t.Fatal("BeginDoneCleanup true for a busy runner")
	}
}

func TestAddDuplicate(t *testing.T) {
	tb := NewTable(nil, nil)
	if err := tb.Add("r", testEpoch); err != nil {
		t.Fatal(err)
	}
	if err := tb.Add("r", testEpoch); err != ErrDuplicateName {
		t.Fatalf("duplicate Add = %v, want ErrDuplicateName", err)
	}
}

func TestBeginFinalizeImpliesDraining(t *testing.T) {
	tb := NewTable(nil, nil)
	if tb.Draining() || tb.Finalizing() {
		t.Fatal("new table draining or finalizing")
	}
	if _, ok := tb.FinalizeDeadline(); ok {
		t.Fatal("deadline before finalize")
	}
	deadline := testEpoch.Add(45 * time.Second)
	tb.BeginFinalize(deadline)
	tb.BeginFinalize(deadline.Add(time.Hour))
	if !tb.Draining() || !tb.Finalizing() {
		t.Fatal("finalize did not set both marks")
	}
	if got, ok := tb.FinalizeDeadline(); !ok || !got.Equal(deadline) {
		t.Fatalf("deadline %v, %v; want the first one, %v", got, ok, deadline)
	}
	if tb.Drain() != DrainNone {
		t.Fatalf("drain kind %v, want none", tb.Drain())
	}
}

func TestDrainKind(t *testing.T) {
	tb := NewTable(nil, nil)
	if tb.SetDraining(DrainNone) || tb.Draining() {
		t.Fatal("SetDraining(DrainNone) started a drain")
	}
	if !tb.SetDraining(DrainSignal) || !tb.Draining() || tb.Drain() != DrainSignal {
		t.Fatal("signal drain not started")
	}
	// A permanent error during a signal drain keeps it a signal drain.
	if tb.SetDraining(DrainError) || tb.Drain() != DrainSignal {
		t.Fatalf("kind %v after a second SetDraining, want signal", tb.Drain())
	}
}

// TestTableConcurrentTransitions drives every transition at once on one
// runner; -race checks the locking, and the end state must be consistent.
func TestTableConcurrentTransitions(t *testing.T) {
	var busyCalls atomic.Int32
	tb := NewTable(func(string, time.Time) { busyCalls.Add(1) }, nil)
	seed(t, tb, "r", 1, StateIdle, testEpoch)
	tok := mustToken(t)
	tb.SetToken("r", tok)
	ops := []func(){
		func() { tb.MarkBusy("r", testEpoch) },
		func() { tb.Authenticate("r", tok.Reveal(), testEpoch) },
		func() { tb.RecordJobStarted("r", Job{ID: "j"}) },
		func() { tb.RecordJobCompleted("r", "j", testEpoch) },
		func() { tb.Allow("r") },
		func() { tb.Deny("r") },
		func() { tb.BeginExit("r") },
		func() { tb.Counts() },
		func() { tb.RemovalCandidates() },
		func() { tb.List() },
		func() { tb.GiveUpRemoval("r") },
		func() { tb.BeginDoneCleanup("r") },
		func() { tb.IdleRemovable("r") },
		func() { tb.BeginExitIfIdle("r") },
		func() { tb.BeginExitAndRevoke("r") },
		func() { tb.AbandonExit("r", AbandonRetriesExhausted) },
		func() { tb.ClaimAbandoned("r") },
	}
	var i atomic.Int32
	race(len(ops)*8, func() bool {
		ops[int(i.Add(1))%len(ops)]()
		return false
	})
	info, _ := tb.Get("r")
	if info.State != StateExiting {
		t.Fatalf("state %v, want exiting after BeginExit", info.State)
	}
	if busyCalls.Load() > 1 {
		t.Fatalf("busy callbacks %d, want at most 1", busyCalls.Load())
	}
}

func mustToken(t *testing.T) gate.Token {
	t.Helper()
	tok, err := gate.NewTokenGenerator(nil).New()
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

func mustState(t *testing.T, tb *Table, name string) State {
	t.Helper()
	info, ok := tb.Get(name)
	if !ok {
		t.Fatalf("%s not in table", name)
	}
	return info.State
}

func TestIdleRemovable(t *testing.T) {
	tb := NewTable(nil, nil)
	seed(t, tb, "idle", 1, StateIdle, testEpoch)
	seed(t, tb, "given-up", 2, StateIdle, testEpoch)
	tb.GiveUpRemoval("given-up")
	for _, st := range []State{StateStarting, StateBusy, StateDone, StateExiting} {
		seed(t, tb, st.String(), 3, st, testEpoch)
	}
	if !tb.IdleRemovable("idle") {
		t.Fatal("idle runner not removable")
	}
	for _, name := range []string{"given-up", "starting", "busy", "done", "exiting", "missing"} {
		if tb.IdleRemovable(name) {
			t.Errorf("%s removable", name)
		}
	}
}

// TestBeginExitIfIdle: only an idle runner whose removal was not given up
// wins, and the win revokes the token in the same step (R37, R38, R94).
func TestBeginExitIfIdle(t *testing.T) {
	t.Run("idle wins once and revokes", func(t *testing.T) {
		secrets := &fakeSecrets{counts: make(map[string]int)}
		tb := NewTable(nil, secrets)
		seed(t, tb, "r", 1, StateIdle, testEpoch)
		tok := mustToken(t)
		tb.SetToken("r", tok)
		if wins := race(32, func() bool { return tb.BeginExitIfIdle("r") }); wins != 1 {
			t.Fatalf("winners %d, want 1", wins)
		}
		if mustState(t, tb, "r") != StateExiting {
			t.Fatal("not exiting")
		}
		if _, ok := tb.Authenticate("r", tok.Reveal(), testEpoch); ok || secrets.live() != 0 {
			t.Fatal("token valid or still registered after BeginExitIfIdle")
		}
	})
	t.Run("not idle loses", func(t *testing.T) {
		for _, st := range []State{StateStarting, StateBusy, StateDone, StateExiting} {
			tb := NewTable(nil, nil)
			seed(t, tb, "r", 1, st, testEpoch)
			tok := mustToken(t)
			tb.SetToken("r", tok)
			if tb.BeginExitIfIdle("r") {
				t.Errorf("%v: won", st)
			}
			if got := mustState(t, tb, "r"); got != st {
				t.Errorf("%v: state changed to %v", st, got)
			}
			if st != StateExiting {
				if _, ok := tb.Authenticate("r", tok.Reveal(), testEpoch); !ok {
					t.Errorf("%v: token revoked by a losing call", st)
				}
			}
		}
		tb := NewTable(nil, nil)
		seed(t, tb, "r", 1, StateIdle, testEpoch)
		tb.GiveUpRemoval("r")
		if tb.BeginExitIfIdle("r") || tb.BeginExitIfIdle("missing") {
			t.Fatal("given-up or unknown runner won")
		}
	})
	t.Run("races MarkBusy", func(t *testing.T) {
		for range 200 {
			tb := NewTable(nil, nil)
			seed(t, tb, "r", 1, StateIdle, testEpoch)
			var exited, busied atomic.Bool
			var i atomic.Int32
			race(2, func() bool {
				if i.Add(1) == 1 {
					exited.Store(tb.BeginExitIfIdle("r"))
				} else {
					busied.Store(tb.MarkBusy("r", testEpoch))
				}
				return false
			})
			info, _ := tb.Get("r")
			switch {
			case exited.Load() == busied.Load():
				t.Fatalf("exit won %v, busy won %v; want exactly one", exited.Load(), busied.Load())
			case exited.Load() && (info.State != StateExiting || !info.BusyAt.IsZero()):
				t.Fatalf("exit won but row %+v", info)
			case busied.Load() && info.State != StateBusy:
				t.Fatalf("busy won but state %v", info.State)
			}
		}
	})
}

// TestBeginExitAndRevoke: the winner's token is revoked in the same step,
// so no Authenticate succeeds after the transition (R14, R38).
func TestBeginExitAndRevoke(t *testing.T) {
	secrets := &fakeSecrets{counts: make(map[string]int)}
	tb := NewTable(nil, secrets)
	seed(t, tb, "r", 1, StateBusy, testEpoch)
	tok := mustToken(t)
	tb.SetToken("r", tok)

	var exited atomic.Bool
	var lateAuth atomic.Int32
	stop := make(chan struct{})
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				after := exited.Load()
				if _, ok := tb.Authenticate("r", tok.Reveal(), testEpoch); ok && after {
					lateAuth.Add(1)
				}
			}
		}()
	}
	if !tb.BeginExitAndRevoke("r") {
		t.Fatal("BeginExitAndRevoke lost on a busy runner")
	}
	exited.Store(true)
	if tb.BeginExitAndRevoke("r") || tb.BeginExit("r") {
		t.Fatal("second exit won")
	}
	close(stop)
	wg.Wait()
	if lateAuth.Load() != 0 {
		t.Fatalf("%d authentications succeeded after the exit", lateAuth.Load())
	}
	if _, ok := tb.Authenticate("r", tok.Reveal(), testEpoch); ok || secrets.live() != 0 {
		t.Fatal("token valid or registered after BeginExitAndRevoke")
	}
	if mustState(t, tb, "r") != StateExiting {
		t.Fatal("not exiting")
	}
}

// TestAbandonAndClaim: an abandoned exiting row is handed to exactly one
// new owner (M3-8).
func TestAbandonAndClaim(t *testing.T) {
	tb := NewTable(nil, nil)
	seed(t, tb, "idle", 1, StateIdle, testEpoch)
	if tb.AbandonExit("idle", AbandonRetriesExhausted) || tb.AbandonExit("missing", AbandonRetriesExhausted) {
		t.Fatal("AbandonExit marked a runner that is not exiting")
	}
	if tb.ClaimAbandoned("idle") {
		t.Fatal("claimed a runner that was not abandoned")
	}
	seed(t, tb, "r", 2, StateExiting, testEpoch)
	if tb.ClaimAbandoned("r") {
		t.Fatal("claimed an exiting runner whose owner did not abandon it")
	}
	if !tb.AbandonExit("r", AbandonRetriesExhausted) {
		t.Fatal("AbandonExit failed on an exiting runner")
	}
	if info, _ := tb.Get("r"); !info.Abandoned || info.State != StateExiting {
		t.Fatalf("row %+v, want exiting and abandoned", info)
	}
	if wins := race(32, func() bool { return tb.ClaimAbandoned("r") }); wins != 1 {
		t.Fatalf("claim winners %d, want 1", wins)
	}
	if info, _ := tb.Get("r"); info.Abandoned {
		t.Fatal("still abandoned after the claim")
	}
	// The new owner may abandon it again.
	tb.AbandonExit("r", AbandonRetriesExhausted)
	if !tb.ClaimAbandoned("r") {
		t.Fatal("second abandonment not claimable")
	}
	if c := tb.Counts(); c.Exiting != 1 {
		t.Fatalf("counts %+v, want the exiting row counted (R3)", c)
	}
}

// TestBusyFuncNotCalledDuringFinalize: once finalize began, MarkBusy and a
// matching Authenticate still record busy but do not call the BusyFunc, so
// no job check timer starts after finalize stopped them (M5-8).
func TestBusyFuncNotCalledDuringFinalize(t *testing.T) {
	var calls atomic.Int32
	tb := NewTable(func(string, time.Time) { calls.Add(1) }, nil)
	seed(t, tb, "a", 1, StateIdle, testEpoch)
	seed(t, tb, "b", 2, StateIdle, testEpoch)
	tok := mustToken(t)
	tb.SetToken("b", tok)
	tb.BeginFinalize(testEpoch.Add(45 * time.Second))

	if !tb.MarkBusy("a", testEpoch) {
		t.Fatal("MarkBusy did not report the first call")
	}
	if v, ok := tb.Authenticate("b", tok.Reveal(), testEpoch); !ok || v != VerdictPending {
		t.Fatalf("Authenticate = %v %v, want pending and ok", v, ok)
	}
	for _, name := range []string{"a", "b"} {
		if info, _ := tb.Get(name); info.State != StateBusy || !info.BusyAt.Equal(testEpoch) {
			t.Fatalf("%s: row %+v, want busy since the call", name, info)
		}
	}
	if n := calls.Load(); n != 0 {
		t.Fatalf("BusyFunc calls %d during finalize, want 0", n)
	}
}

// TestMarkStartedRefusedDuringFinalize: a start path that finishes after
// finalize began must clean up and not watch its runner (M3-7).
func TestMarkStartedRefusedDuringFinalize(t *testing.T) {
	tb := NewTable(nil, nil)
	seed(t, tb, "r", 1, StateStarting, testEpoch)
	tb.BeginFinalize(testEpoch.Add(45 * time.Second))
	if tb.MarkStarted("r") {
		t.Fatal("MarkStarted succeeded during finalize")
	}
	if info, _ := tb.Get("r"); info.State != StateStarting {
		t.Fatalf("state %v, want starting until the start path cleans up", info.State)
	}
}

// TestAbandonReasons: a row abandoned on a permanent error is never claimed
// (R42), and that mark is not replaced; the other reasons are claimable.
func TestAbandonReasons(t *testing.T) {
	tb := NewTable(nil, nil)
	seed(t, tb, "p", 1, StateExiting, testEpoch)
	if tb.AbandonExit("p", AbandonNone) {
		t.Fatal("AbandonExit accepted AbandonNone")
	}
	if !tb.AbandonExit("p", AbandonPermanent) {
		t.Fatal("AbandonExit refused a permanent mark")
	}
	if tb.ClaimAbandoned("p") {
		t.Fatal("claimed a row abandoned on a permanent error (R42)")
	}
	if tb.AbandonExit("p", AbandonRetriesExhausted) {
		t.Fatal("a later reason replaced the permanent mark")
	}
	if info, _ := tb.Get("p"); !info.Abandoned || info.AbandonReason != AbandonPermanent {
		t.Fatalf("row %+v, want abandoned as permanent", info)
	}
	if tb.ClaimAbandoned("p") {
		t.Fatal("permanent row claimed after another AbandonExit")
	}

	for i, reason := range []AbandonReason{
		AbandonRetriesExhausted, AbandonNoBackoff, AbandonNoBudget, AbandonFinalize, AbandonErrorDrain,
	} {
		name := "r" + reason.String()
		seed(t, tb, name, int64(10+i), StateExiting, testEpoch)
		if !tb.AbandonExit(name, reason) {
			t.Fatalf("%v: AbandonExit refused", reason)
		}
		if info, _ := tb.Get(name); info.AbandonReason != reason {
			t.Fatalf("%v: reason %v", reason, info.AbandonReason)
		}
		if !tb.ClaimAbandoned(name) {
			t.Fatalf("%v: not claimable", reason)
		}
		if info, _ := tb.Get(name); info.Abandoned || info.AbandonReason != AbandonNone {
			t.Fatalf("%v: row %+v still abandoned after the claim", reason, info)
		}
	}
}

// Once finalize has begun, an allow is not recorded, so a report that races
// with finalize leaves the runner pending (0002 SPEC R131).
func TestAllowRefusedAfterFinalize(t *testing.T) {
	tb := NewTable(nil, nil)
	seed(t, tb, "r", 1, StateBusy, testEpoch)
	tb.BeginFinalize(testEpoch.Add(time.Minute))
	if tb.Allow("r") {
		t.Fatal("Allow succeeded after finalize began")
	}
	if info, _ := tb.Get("r"); info.Verdict != VerdictPending {
		t.Errorf("verdict = %v, want pending", info.Verdict)
	}
}
