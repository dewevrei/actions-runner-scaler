package status

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/dewevrei/actions-runner-scaler/internal/logging"
	"github.com/dewevrei/actions-runner-scaler/internal/scaler"
)

type fakeTable struct {
	counts scaler.Counts
	start  scaler.StartStatus
}

func (f *fakeTable) Counts() scaler.Counts           { return f.counts }
func (f *fakeTable) StartStatus() scaler.StartStatus { return f.start }

func TestStatusCounts(t *testing.T) {
	rec, _, _ := newTestRecorder(t, logging.NewSecrets())
	s := mustNew(t, rec)
	tbl := &fakeTable{counts: scaler.Counts{Starting: 1, Idle: 2, Busy: 3, Done: 4, Exiting: 5}}
	s.AddPool("linux-arm64-deploy", tbl)
	s.SetState(Running)

	r := s.Report()
	if r.State != Running || len(r.Targets) != 1 {
		t.Fatalf("Report() = %+v", r)
	}
	p := r.Targets[0]
	if p.Busy != 3+4 {
		t.Errorf("busy = %d, want busy + done = 7", p.Busy)
	}
	if p.Idle != 1+2 {
		t.Errorf("idle = %d, want starting + idle = 3", p.Idle)
	}
	c := tbl.counts
	if got, want := p.Busy+p.Idle, c.Starting+c.Idle+c.Busy+c.Done; got != want {
		t.Errorf("busy + idle = %d, want %d (exiting left out)", got, want)
	}
	if p.StartFailures != 0 || p.LastStartError != "" || !p.LastStartErrorAt.IsZero() {
		t.Errorf("start record = %+v, want none", p)
	}
}

func TestStatusStartFailures(t *testing.T) {
	secrets := logging.NewSecrets()
	secrets.Register("jit-xyz")
	rec, _, _ := newTestRecorder(t, secrets)
	s := mustNew(t, rec)
	at := t0.Add(-time.Minute)
	s.AddPool("p", &fakeTable{start: scaler.StartStatus{
		Failures:    3,
		LastError:   errors.New("pull failed for jit-xyz"),
		LastErrorAt: at,
	}})
	p := s.Report().Targets[0]
	if p.StartFailures != 3 || !p.LastStartErrorAt.Equal(at) {
		t.Errorf("pool = %+v", p)
	}
	if strings.Contains(p.LastStartError, "jit-xyz") || !strings.Contains(p.LastStartError, "pull failed") {
		t.Errorf("lastStartError = %q, want it redacted", p.LastStartError)
	}
}

// A real table: two added runners of the target are starting, so idle is 2;
// another target's runner is not counted (0002 PLAN M11-1).
func TestStatusRealTable(t *testing.T) {
	rec, _, _ := newTestRecorder(t, logging.NewSecrets())
	s := mustNew(t, rec)
	tbl := scaler.NewTable(scaler.TableConfig{ServerLimit: 3})
	for _, name := range []string{"r1", "r2"} {
		if err := tbl.Add(name, "p", 2, t0); err != nil {
			t.Fatal(err)
		}
	}
	if err := tbl.Add("other", "q", 2, t0); err != nil {
		t.Fatal(err)
	}
	s.AddPool("p", tbl.View("p"))
	if p := s.Report().Targets[0]; p.Idle != 2 || p.Busy != 0 {
		t.Errorf("pool = %+v, want idle 2", p)
	}
}

// AddPool reports the pool as a running target with no max-runners and no
// waiting jobs (the interim adapter, 0002 PLAN M7).
func TestStatusAddPoolAdapter(t *testing.T) {
	rec, _, _ := newTestRecorder(t, logging.NewSecrets())
	s := mustNew(t, rec)
	s.AddPool("p", &fakeTable{})
	got := s.Report().Targets[0]
	if got.URL != "p" || got.State != TargetRunning || got.MaxRunners != 0 || got.WaitingJobs != 0 || got.NextRetryAt != nil {
		t.Errorf("target = %+v", got)
	}
}

type fakeView struct{ t Target }

func (f fakeView) TargetStatus() Target { return f.t }

// AddTarget keeps the order, SetServerLimit fills serverLimit (null before),
// and errors are redacted again (0002 SPEC R152, R180).
func TestStatusAddTargetAndServerLimit(t *testing.T) {
	secrets := logging.NewSecrets()
	secrets.Register("jit-xyz")
	rec, _, _ := newTestRecorder(t, secrets)
	s := mustNew(t, rec)
	if s.Report().ServerLimit != nil {
		t.Error("serverLimit set before SetServerLimit")
	}
	s.AddTarget(fakeView{Target{URL: "a", State: TargetStarting}})
	s.AddTarget(fakeView{Target{URL: "b", State: TargetErrorWait, LastError: "bad jit-xyz", LastStartError: "pull jit-xyz"}})
	s.SetServerLimit(2)
	r := s.Report()
	if r.ServerLimit == nil || *r.ServerLimit != 2 {
		t.Errorf("serverLimit = %v, want 2", r.ServerLimit)
	}
	if len(r.Targets) != 2 || r.Targets[0].URL != "a" || r.Targets[1].URL != "b" {
		t.Fatalf("targets = %+v, want a then b", r.Targets)
	}
	if b := r.Targets[1]; strings.Contains(b.LastError, "jit-xyz") || strings.Contains(b.LastStartError, "jit-xyz") {
		t.Errorf("target = %+v, want errors redacted", b)
	}
}

func TestStatusStartsStarting(t *testing.T) {
	rec, clock, _ := newTestRecorder(t, logging.NewSecrets())
	s := mustNew(t, rec)
	r := s.Report()
	if r.State != Starting || r.Targets == nil || len(r.Targets) != 0 || r.LastError != "" {
		t.Errorf("Report() = %+v, want starting with no targets and no error", r)
	}
	rec.Record("config", errors.New("unknown key x"))
	r = s.Report()
	if r.LastError != "config: unknown key x" || !r.LastErrorAt.Equal(clock.Now()) {
		t.Errorf("Report() = %+v, want the recorded error", r)
	}
}

func mustNew(t *testing.T, rec *Recorder) *Status {
	t.Helper()
	s, err := New(rec)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestNewNeedsRecorder(t *testing.T) {
	if _, err := New(nil); err == nil {
		t.Fatal("New(nil) succeeded, want an error")
	}
}
