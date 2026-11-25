// Package status serves the scaler's state on an owner-only unix socket and
// prints it for the status command (R26, R27, R43, R45, R56, R58, R86).
//
// The daemon keeps a Status, sets its State as it moves through startup,
// normal operation, drain and error-wait, and adds each pool's runner table.
// A Server answers every connection with one JSON line built from the
// Status (SPEC "외부 계약"), and Show reads that line and prints it.
package status

import (
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/dewevrei/actions-runner-scaler/internal/scaler"
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
// is always present: an empty error text means no error, a zero time is
// written as "" (RFC 3339 otherwise), and nil Pools is written as [].
// MarshalJSON writes the *At keys last, after the other fields in order,
// which keeps the SPEC key order.
type Report struct {
	State       State     `json:"state"`
	Pools       []Pool    `json:"pools"`
	LastError   string    `json:"lastError"`
	LastErrorAt time.Time `json:"-"` // "lastErrorAt"; see MarshalJSON
}

// Pool is one pool's line. Busy counts runners running a job and done
// runners waiting for their container to end; Idle counts starting and
// idle runners. Runners whose removal began are not counted (SPEC "외부
// 계약").
//
// MarshalJSON writes lastStartErrorAt last, after the other fields in
// order, which keeps the SPEC key order.
type Pool struct {
	Name             string    `json:"name"`
	Busy             int       `json:"busy"`
	Idle             int       `json:"idle"`
	StartFailures    int       `json:"startFailures"`
	LastStartError   string    `json:"lastStartError"`
	LastStartErrorAt time.Time `json:"-"` // "lastStartErrorAt"; see MarshalJSON
}

// MarshalJSON writes lastErrorAt as "" when it is zero and nil Pools as [].
func (r Report) MarshalJSON() ([]byte, error) {
	type plain Report
	if r.Pools == nil {
		r.Pools = []Pool{}
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

// MarshalJSON writes lastStartErrorAt as "" when it is zero.
func (p Pool) MarshalJSON() ([]byte, error) {
	type plain Pool
	return json.Marshal(struct {
		plain
		LastStartErrorAt string `json:"lastStartErrorAt"`
	}{plain(p), formatWireTime(p.LastStartErrorAt)})
}

// UnmarshalJSON reads lastStartErrorAt as "", null or an RFC 3339 time.
func (p *Pool) UnmarshalJSON(b []byte) error {
	type plain Pool
	var aux struct {
		plain
		LastStartErrorAt string `json:"lastStartErrorAt"`
	}
	if err := json.Unmarshal(b, &aux); err != nil {
		return err
	}
	at, err := parseWireTime(aux.LastStartErrorAt)
	if err != nil {
		return fmt.Errorf("lastStartErrorAt: %w", err)
	}
	*p = Pool(aux.plain)
	p.LastStartErrorAt = at
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

// PoolTable is the part of a pool's runner table that status reads.
// *scaler.Table implements it.
type PoolTable interface {
	Counts() scaler.Counts
	StartStatus() scaler.StartStatus
}

var _ PoolTable = (*scaler.Table)(nil)

// Status is the daemon's state for the socket. It is safe for concurrent
// use.
type Status struct {
	rec *Recorder

	mu    sync.Mutex
	state State
	pools []namedTable
}

type namedTable struct {
	name  string
	table PoolTable
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

// AddPool adds a pool whose runner table the report reads. Pools are
// reported in the order they were added.
func (s *Status) AddPool(name string, table PoolTable) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pools = append(s.pools, namedTable{name: name, table: table})
}

// Report returns the current report. Each table is read once for its
// counts and once for its start record, so the two may be a moment apart.
func (s *Status) Report() Report {
	s.mu.Lock()
	state := s.state
	pools := make([]namedTable, len(s.pools))
	copy(pools, s.pools)
	s.mu.Unlock()

	r := Report{State: state, Pools: make([]Pool, 0, len(pools))}
	r.LastError, r.LastErrorAt = s.rec.Last()
	for _, p := range pools {
		r.Pools = append(r.Pools, s.pool(p))
	}
	return r
}

// pool builds one pool's line with the counting rules of SPEC "외부 계약".
func (s *Status) pool(p namedTable) Pool {
	c := p.table.Counts()
	st := p.table.StartStatus()
	out := Pool{
		Name:             p.name,
		Busy:             c.Busy + c.Done,
		Idle:             c.Starting + c.Idle,
		StartFailures:    st.Failures,
		LastStartErrorAt: st.LastErrorAt,
	}
	if st.LastError != nil {
		// The scaler redacts before storing; redacting again costs little
		// and keeps R31 if a caller forgets.
		out.LastStartError = s.rec.redact(st.LastError.Error())
	}
	return out
}
