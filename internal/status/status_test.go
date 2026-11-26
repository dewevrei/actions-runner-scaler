package status

import (
	"errors"
	"strings"
	"testing"

	"github.com/dewevrei/actions-runner-scaler/internal/logging"
)

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

func TestStateReadsNoTarget(t *testing.T) {
	rec, _, _ := newTestRecorder(t, logging.NewSecrets())
	s := mustNew(t, rec)
	if s.State() != Starting {
		t.Errorf("State() = %s, want starting", s.State())
	}
	s.SetState(Draining)
	if s.State() != Draining {
		t.Errorf("State() = %s, want draining", s.State())
	}
}
