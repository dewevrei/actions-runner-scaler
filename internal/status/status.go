// Package status serves the scaler's state on an owner-only unix socket and
// prints it for the status command (R26, R27, R43, R45, R56, R58, R86; 0002
// SPEC R152, R180, R197, R198, R205, R222).
//
// The daemon keeps a Status, sets its State as it moves through startup,
// normal operation, drain and error-wait, and adds each target's view.
// A Server answers every connection with one JSON line built from the
// Status (SPEC "외부 계약"), and Show reads that line and prints it.
package status

import (
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"
)

// State is the daemon's state as status reports it.
type State string

// The states of the status socket's "state" field.
const (
	// Starting is before the first session, backoff included (R58).
	Starting State = "starting"
	// Running is normal operation after the first session (R26).
	Running State = "running"
	// Draining is a signal or error drain, finalize included (R56).
	Draining State = "draining"
	// ErrorWait holds only the lock and status until a signal (R45).
	ErrorWait State = "error-wait"
)

// Report is the JSON line the socket serves. Every key of SPEC "외부 계약"
// and 0002 SPEC "status(R152, R180)" is always present: an empty error text
// means no error, a zero time is written as "" (RFC 3339 otherwise), a nil
// ServerLimit is written as null and nil Targets as []. MarshalJSON writes
// lastErrorAt last, after the other fields in order, which keeps the SPEC
// key order.
type Report struct {
	State       State     `json:"state"`
	ServerLimit *int      `json:"serverLimit"` // nil until computed (0002 SPEC R201)
	Targets     []Target  `json:"targets"`
	LastError   string    `json:"lastError"`
	LastErrorAt time.Time `json:"-"` // "lastErrorAt"; see MarshalJSON
}

// TargetState is one target's state as status reports it (0002 SPEC R180).
type TargetState string

// The states of a target's "state" field.
const (
	// TargetStarting is a target still in its start phase, backoff included.
	TargetStarting TargetState = "starting"
	// TargetRunning is a target taking jobs.
	TargetRunning TargetState = "running"
	// TargetErrorDraining is a target draining after its own error (R173).
	TargetErrorDraining TargetState = "error-draining"
	// TargetErrorWait is a target waiting to retry after an error (R174).
	TargetErrorWait TargetState = "error-wait"
)

// Target is one target's line. Busy counts runners running a job and done
// runners waiting for their container to end; Idle counts starting and
// idle runners. Runners whose removal began are not counted (SPEC "외부
// 계약"). WaitingJobs is the target's assigned jobs without a runner:
// TotalAssignedJobs of its last message with Statistics minus its live
// runners, at least 0, as of the last placement. MaxRunners is its
// max-runners (0002 SPEC R205).
// NextRetryAt is set only in TargetErrorWait and written as null otherwise
// (R180).
type Target struct {
	URL              string
	State            TargetState
	Busy             int
	Idle             int
	WaitingJobs      int
	MaxRunners       int
	StartFailures    int
	LastStartError   string
	LastStartErrorAt time.Time
	LastError        string
	LastErrorAt      time.Time
	NextRetryAt      *time.Time
}

// targetWire is Target as written to the socket, in the SPEC key order.
type targetWire struct {
	URL              string      `json:"url"`
	State            TargetState `json:"state"`
	Busy             int         `json:"busy"`
	Idle             int         `json:"idle"`
	WaitingJobs      int         `json:"waitingJobs"`
	MaxRunners       int         `json:"maxRunners"`
	StartFailures    int         `json:"startFailures"`
	LastStartError   string      `json:"lastStartError"`
	LastStartErrorAt string      `json:"lastStartErrorAt"`
	LastError        string      `json:"lastError"`
	LastErrorAt      string      `json:"lastErrorAt"`
	NextRetryAt      *string     `json:"nextRetryAt"`
}

// MarshalJSON writes lastErrorAt as "" when it is zero and nil Targets as [].
func (r Report) MarshalJSON() ([]byte, error) {
	type plain Report
	if r.Targets == nil {
		r.Targets = []Target{}
	}
	return json.Marshal(struct {
		plain
		LastErrorAt string `json:"lastErrorAt"`
	}{plain(r), formatWireTime(r.LastErrorAt)})
}

// UnmarshalJSON reads lastErrorAt as "", null or an RFC 3339 time.
func (r *Report) UnmarshalJSON(b []byte) error {
	type plain Report
	var aux struct {
		plain
		LastErrorAt string `json:"lastErrorAt"`
	}
	if err := json.Unmarshal(b, &aux); err != nil {
		return err
	}
	at, err := parseWireTime(aux.LastErrorAt)
	if err != nil {
		return fmt.Errorf("lastErrorAt: %w", err)
	}
	*r = Report(aux.plain)
	r.LastErrorAt = at
	return nil
}

// MarshalJSON writes zero times as "" and NextRetryAt as null unless the
// target is in TargetErrorWait (0002 SPEC R198).
func (t Target) MarshalJSON() ([]byte, error) {
	w := targetWire{
		URL:              t.URL,
		State:            t.State,
		Busy:             t.Busy,
		Idle:             t.Idle,
		WaitingJobs:      t.WaitingJobs,
		MaxRunners:       t.MaxRunners,
		StartFailures:    t.StartFailures,
		LastStartError:   t.LastStartError,
		LastStartErrorAt: formatWireTime(t.LastStartErrorAt),
		LastError:        t.LastError,
		LastErrorAt:      formatWireTime(t.LastErrorAt),
	}
	if t.NextRetryAt != nil && t.State == TargetErrorWait {
		s := formatWireTime(*t.NextRetryAt)
		w.NextRetryAt = &s
	}
	return json.Marshal(w)
}

// UnmarshalJSON reads the time keys as "", null or an RFC 3339 time.
func (t *Target) UnmarshalJSON(b []byte) error {
	var w targetWire
	if err := json.Unmarshal(b, &w); err != nil {
		return err
	}
	out := Target{
		URL:            w.URL,
		State:          w.State,
		Busy:           w.Busy,
		Idle:           w.Idle,
		WaitingJobs:    w.WaitingJobs,
		MaxRunners:     w.MaxRunners,
		StartFailures:  w.StartFailures,
		LastStartError: w.LastStartError,
		LastError:      w.LastError,
	}
	var err error
	if out.LastStartErrorAt, err = parseWireTime(w.LastStartErrorAt); err != nil {
		return fmt.Errorf("lastStartErrorAt: %w", err)
	}
	if out.LastErrorAt, err = parseWireTime(w.LastErrorAt); err != nil {
		return fmt.Errorf("lastErrorAt: %w", err)
	}
	if w.NextRetryAt != nil {
		at, err := parseWireTime(*w.NextRetryAt)
		if err != nil {
			return fmt.Errorf("nextRetryAt: %w", err)
		}
		out.NextRetryAt = &at
	}
	*t = out
	return nil
}

func formatWireTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.Format(time.RFC3339Nano)
}

func parseWireTime(s string) (time.Time, error) {
	if s == "" {
		return time.Time{}, nil
	}
	return time.Parse(time.RFC3339Nano, s)
}

// TargetView is what status reads from one target (0002 SPEC R152). The
// target keeps its own counts, so status asks for a snapshot.
type TargetView interface {
	TargetStatus() Target
}

// Status is the daemon's state for the socket. It is safe for concurrent
// use.
type Status struct {
	rec *Recorder

	mu          sync.Mutex
	state       State
	serverLimit *int
	targets     []TargetView
}

// New returns a Status in state Starting that reports rec's last error.
func New(rec *Recorder) (*Status, error) {
	if rec == nil {
		return nil, errors.New("status: nil Recorder")
	}
	return &Status{rec: rec, state: Starting}, nil
}

// SetState sets the state the socket reports.
func (s *Status) SetState(state State) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.state = state
}

// State returns the state the socket reports. Unlike Report it reads no
// target, so a TargetView may call it (0002 PLAN M15).
func (s *Status) State() State {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state
}

// SetServerLimit sets the server limit the report shows (0002 SPEC R201).
// It is null until set.
func (s *Status) SetServerLimit(n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.serverLimit = &n
}

// AddTarget adds a target the report reads. Targets are reported in the
// order they were added.
func (s *Status) AddTarget(v TargetView) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.targets = append(s.targets, v)
}

// Report returns the current report. Each target is read once, so a
// target's counts and start record may be a moment apart.
func (s *Status) Report() Report {
	s.mu.Lock()
	r := Report{State: s.state}
	if s.serverLimit != nil {
		n := *s.serverLimit
		r.ServerLimit = &n
	}
	views := make([]TargetView, len(s.targets))
	copy(views, s.targets)
	s.mu.Unlock()

	r.Targets = make([]Target, 0, len(views))
	r.LastError, r.LastErrorAt = s.rec.Last()
	for _, v := range views {
		t := v.TargetStatus()
		// The scaler redacts before storing; redacting again costs little
		// and keeps R31 if a caller forgets.
		t.LastStartError = s.rec.redact(t.LastStartError)
		t.LastError = s.rec.redact(t.LastError)
		r.Targets = append(r.Targets, t)
	}
	return r
}
