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
	if r.State != Running || len(r.Pools) != 1 {
		t.Fatalf("Report() = %+v", r)
	}
	p := r.Pools[0]
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
	p := s.Report().Pools[0]
	if p.StartFailures != 3 || !p.LastStartErrorAt.Equal(at) {
		t.Errorf("pool = %+v", p)
	}
	if strings.Contains(p.LastStartError, "jit-xyz") || !strings.Contains(p.LastStartError, "pull failed") {
		t.Errorf("lastStartError = %q, want it redacted", p.LastStartError)
	}
}

// A real table: two added runners are starting, so idle is 2.
func TestStatusRealTable(t *testing.T) {
	rec, _, _ := newTestRecorder(t, logging.NewSecrets())
	s := mustNew(t, rec)
	tbl := scaler.NewTable(nil, nil)
	for _, name := range []string{"r1", "r2"} {
		if err := tbl.Add(name, t0); err != nil {
			t.Fatal(err)
		}
	}
	s.AddPool("p", tbl)
	if p := s.Report().Pools[0]; p.Idle != 2 || p.Busy != 0 {
		t.Errorf("pool = %+v, want idle 2", p)
	}
}

func TestStatusStartsStarting(t *testing.T) {
	rec, clock, _ := newTestRecorder(t, logging.NewSecrets())
	s := mustNew(t, rec)
	r := s.Report()
	if r.State != Starting || r.Pools == nil || len(r.Pools) != 0 || r.LastError != "" {
		t.Errorf("Report() = %+v, want starting with no pools and no error", r)
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
