package status

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// SPEC "외부 계약": the output and exit code of each state (R26, R45, R56,
// R58, R86).
func TestPrint(t *testing.T) {
	at := time.Date(2026, 11, 25, 9, 30, 0, 0, time.FixedZone("KST", 9*3600))
	pools := []Target{
		{URL: "a", State: TargetRunning, Busy: 2, Idle: 1, WaitingJobs: 0, MaxRunners: 2},
		{URL: "b", State: TargetRunning, StartFailures: 3, LastStartError: "pull failed", LastStartErrorAt: at, MaxRunners: 2},
	}
	tests := []struct {
		name    string
		r       Report
		want    string
		wantErr bool
	}{
		{
			name: "starting without error",
			r:    Report{State: Starting},
			want: "state: starting\nlast error: none\n",
		},
		{
			name: "starting with error",
			r:    Report{State: Starting, LastError: "docker: ping failed", LastErrorAt: at},
			want: "state: starting\nlast error: docker: ping failed (2026-11-25T09:30:00+09:00)\n",
		},
		{
			name: "running",
			r:    Report{State: Running, Targets: pools},
			want: "state: running\n" +
				"a: running, busy 2, idle 1, waiting jobs 0, max 2, start failures 0\n" +
				"b: running, busy 0, idle 0, waiting jobs 0, max 2, start failures 3 (pull failed, 2026-11-25T09:30:00+09:00)\n" +
				"total: busy 2, idle 1, max -\n" +
				"last error: none\n",
		},
		{
			name: "draining",
			r:    Report{State: Draining, Targets: pools[:1], LastError: "x", LastErrorAt: at},
			want: "state: draining\na: running, busy 2, idle 1, waiting jobs 0, max 2, start failures 0\n" +
				"total: busy 2, idle 1, max -\nlast error: x (2026-11-25T09:30:00+09:00)\n",
		},
		{
			name:    "error-wait",
			r:       Report{State: ErrorWait, Targets: pools, LastError: "config: unknown key x", LastErrorAt: at},
			want:    "state: error-wait\nerror: config: unknown key x (2026-11-25T09:30:00+09:00)\n",
			wantErr: true,
		},
		{
			name:    "unknown state",
			r:       Report{State: "paused"},
			want:    "state: unknown (\"paused\")\n",
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var b bytes.Buffer
			err := Print(&b, tt.r)
			if got := b.String(); got != tt.want {
				t.Errorf("output:\n%s\nwant:\n%s", got, tt.want)
			}
			if (err != nil) != tt.wantErr {
				t.Errorf("err = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestPrintErrorWaitIsErrErrorWait(t *testing.T) {
	if err := Print(&bytes.Buffer{}, Report{State: ErrorWait}); !errors.Is(err, ErrErrorWait) {
		t.Errorf("err = %v, want ErrErrorWait", err)
	}
}

// R27: no socket means "not running" and exit 1.
func TestShowNotRunning(t *testing.T) {
	var b bytes.Buffer
	err := Show(context.Background(), &b, filepath.Join(shortDir(t), "none.sock"))
	if err == nil {
		t.Error("Show without a socket returned nil, want an error (exit 1)")
	}
	if got := b.String(); got != NotRunning+"\n" {
		t.Errorf("output = %q, want %q", got, NotRunning+"\n")
	}
}

// End to end through the socket: JSON round trip keeps the format.
func TestShowThroughSocket(t *testing.T) {
	at := time.Date(2026, 11, 25, 0, 0, 0, 0, time.UTC)
	tests := []struct {
		r       Report
		want    string
		wantErr bool
	}{
		{Report{State: Running, Targets: []Target{{URL: "p", State: TargetRunning, Busy: 1, Idle: 1, MaxRunners: 2}}},
			"state: running\np: running, busy 1, idle 1, waiting jobs 0, max 2, start failures 0\ntotal: busy 1, idle 1, max -\nlast error: none\n", false},
		{Report{State: ErrorWait, LastError: "e", LastErrorAt: at},
			"state: error-wait\nerror: e (2026-11-25T00:00:00Z)\n", true},
	}
	for _, tt := range tests {
		t.Run(string(tt.r.State), func(t *testing.T) {
			path := filepath.Join(shortDir(t), "status.sock")
			serve(t, path, staticReporter{tt.r})
			var b bytes.Buffer
			err := Show(context.Background(), &b, path)
			if got := b.String(); got != tt.want {
				t.Errorf("output:\n%s\nwant:\n%s", got, tt.want)
			}
			if (err != nil) != tt.wantErr {
				t.Errorf("err = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

// The wire format has the SPEC keys.
func TestReportJSONKeys(t *testing.T) {
	path := filepath.Join(shortDir(t), "status.sock")
	at := time.Date(2026, 11, 25, 0, 0, 0, 0, time.UTC)
	serve(t, path, staticReporter{Report{
		State:       Running,
		ServerLimit: intPtr(2),
		Targets: []Target{{
			URL: "p", State: TargetErrorWait, Busy: 1, Idle: 2, WaitingJobs: 3, MaxRunners: 4,
			StartFailures: 1, LastStartError: "s", LastStartErrorAt: at,
			LastError: "t", LastErrorAt: at, NextRetryAt: &at,
		}},
		LastError:   "e",
		LastErrorAt: at,
	}})
	line := readLine(t, path)
	want := `{"state":"running","serverLimit":2,"targets":[{"url":"p","state":"error-wait","busy":1,"idle":2,` +
		`"waitingJobs":3,"maxRunners":4,"startFailures":1,"lastStartError":"s","lastStartErrorAt":"2026-11-25T00:00:00Z",` +
		`"lastError":"t","lastErrorAt":"2026-11-25T00:00:00Z","nextRetryAt":"2026-11-25T00:00:00Z"}],` +
		`"lastError":"e","lastErrorAt":"2026-11-25T00:00:00Z"}` + "\n"
	if line != want {
		t.Errorf("line:\n%s\nwant:\n%s", line, want)
	}
}

func readLine(t *testing.T, path string) string {
	t.Helper()
	conn, err := net.Dial("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	b, err := io.ReadAll(conn)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// With no error, the time keys are still present and empty, and a round
// trip keeps the zero times.
func TestReportJSONNoError(t *testing.T) {
	path := filepath.Join(shortDir(t), "status.sock")
	serve(t, path, staticReporter{Report{State: Starting, Targets: []Target{{URL: "p", State: TargetStarting}}}})
	line := readLine(t, path)
	want := `{"state":"starting","serverLimit":null,"targets":[{"url":"p","state":"starting","busy":0,"idle":0,` +
		`"waitingJobs":0,"maxRunners":0,"startFailures":0,"lastStartError":"","lastStartErrorAt":"",` +
		`"lastError":"","lastErrorAt":"","nextRetryAt":null}],"lastError":"","lastErrorAt":""}` + "\n"
	if line != want {
		t.Errorf("line:\n%s\nwant:\n%s", line, want)
	}
	r, err := Query(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	if g := r.Targets[0]; !r.LastErrorAt.IsZero() || !g.LastStartErrorAt.IsZero() || !g.LastErrorAt.IsZero() ||
		g.NextRetryAt != nil || r.ServerLimit != nil {
		t.Errorf("round trip = %+v, want zero times, null nextRetryAt and serverLimit", r)
	}
}

// A server that accepts and never writes: Query ends at the ctx deadline,
// and Show prints Unreadable and returns an error to log, not a reported
// one.
func TestQueryTimeout(t *testing.T) {
	path := filepath.Join(shortDir(t), "status.sock")
	silentServer(t, path)

	start := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := Query(ctx, path); err == nil {
		t.Error("Query against a silent server succeeded")
	}
	if d := time.Since(start); d >= time.Second {
		t.Errorf("Query took %v, want the 50ms ctx deadline to end it", d)
	}

	ctx2, cancel2 := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel2()
	var b bytes.Buffer
	err := Show(ctx2, &b, path)
	if err == nil || Reported(err) {
		t.Errorf("Show = %v, want an error that is not Reported", err)
	}
	if got := b.String(); got != Unreadable+"\n" {
		t.Errorf("output = %q, want %q", got, Unreadable+"\n")
	}
}

// A socket file with no process behind it (ECONNREFUSED) and a missing
// file (ENOENT) are both "not running".
func TestQueryNotRunning(t *testing.T) {
	dir := shortDir(t)
	stale := filepath.Join(dir, "stale.sock")
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: stale, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	ln.SetUnlinkOnClose(false)
	ln.Close()
	for _, path := range []string{stale, filepath.Join(dir, "missing.sock")} {
		var b bytes.Buffer
		err := Show(context.Background(), &b, path)
		if !errors.Is(err, ErrNotRunning) || !Reported(err) {
			t.Errorf("%s: err = %v, want ErrNotRunning", filepath.Base(path), err)
		}
		if b.String() != NotRunning+"\n" {
			t.Errorf("%s: output = %q", filepath.Base(path), b.String())
		}
	}
}

func TestReportedStates(t *testing.T) {
	if err := Print(&bytes.Buffer{}, Report{State: "paused"}); !errors.Is(err, ErrUnknownState) || !Reported(err) {
		t.Errorf("unknown state: err = %v, want ErrUnknownState", err)
	}
	if !Reported(Print(&bytes.Buffer{}, Report{State: ErrorWait})) {
		t.Error("error-wait is not Reported")
	}
}

func intPtr(n int) *int { return &n }

// The keys follow the SPEC order; serverLimit and nextRetryAt are null when
// unset and nil Targets is written as [] (0002 SPEC R152, R205, R222).
func TestReportJSONKeyOrder(t *testing.T) {
	b, err := json.Marshal(Report{State: Starting})
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"state":"starting","serverLimit":null,"targets":[],"lastError":"","lastErrorAt":""}`; string(b) != want {
		t.Errorf("json = %s, want %s", b, want)
	}
	b, err = json.Marshal(Report{State: Running, ServerLimit: intPtr(0), Targets: []Target{{URL: "u", State: TargetRunning}}})
	if err != nil {
		t.Fatal(err)
	}
	want := `{"state":"running","serverLimit":0,"targets":[{"url":"u","state":"running","busy":0,"idle":0,` +
		`"waitingJobs":0,"maxRunners":0,"startFailures":0,"lastStartError":"","lastStartErrorAt":"",` +
		`"lastError":"","lastErrorAt":"","nextRetryAt":null}],"lastError":"","lastErrorAt":""}`
	if string(b) != want {
		t.Errorf("json = %s, want %s", b, want)
	}
	var back Report
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	if back.ServerLimit == nil || *back.ServerLimit != 0 || back.Targets[0].NextRetryAt != nil {
		t.Errorf("round trip = %+v", back)
	}
}

var printAt = time.Date(2026, 11, 25, 9, 30, 0, 0, time.FixedZone("KST", 9*3600))

func printString(t *testing.T, r Report) (string, error) {
	t.Helper()
	var b bytes.Buffer
	err := Print(&b, r)
	return b.String(), err
}

// The four target states with their words and suffixes (0002 SPEC R180,
// R205, R222).
func TestPrintTargetLine(t *testing.T) {
	retry := printAt.Add(time.Minute)
	tests := []struct {
		name string
		t    Target
		want string
	}{
		{"starting", Target{URL: "u", State: TargetStarting, WaitingJobs: 2, MaxRunners: 3},
			"u: starting, busy 0, idle 0, waiting jobs 2, max 3, start failures 0\n"},
		{"running", Target{URL: "u", State: TargetRunning, Busy: 1, Idle: 2, MaxRunners: 3},
			"u: running, busy 1, idle 2, waiting jobs 0, max 3, start failures 0\n"},
		{"running with start failures", Target{URL: "u", State: TargetRunning, StartFailures: 2, LastStartError: "pull", LastStartErrorAt: printAt},
			"u: running, busy 0, idle 0, waiting jobs 0, max 0, start failures 2 (pull, 2026-11-25T09:30:00+09:00)\n"},
		{"error-draining", Target{URL: "u", State: TargetErrorDraining, Busy: 1, LastError: "boom", LastErrorAt: printAt},
			"u: error-draining, busy 1, idle 0, waiting jobs 0, max 0, start failures 0, error: boom (2026-11-25T09:30:00+09:00)\n"},
		{"error-wait", Target{URL: "u", State: TargetErrorWait, LastError: "boom", LastErrorAt: printAt, NextRetryAt: &retry,
			StartFailures: 1, LastStartError: "s", LastStartErrorAt: printAt},
			"u: error-wait, busy 0, idle 0, waiting jobs 0, max 0, start failures 1 (s, 2026-11-25T09:30:00+09:00)" +
				", error: boom (2026-11-25T09:30:00+09:00), next retry 2026-11-25T09:31:00+09:00\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// The starting state prints target lines and always exits 0.
			r := Report{State: Starting, Targets: []Target{tt.t}}
			got, err := printString(t, r)
			if err != nil {
				t.Fatalf("Print = %v", err)
			}
			if want := "state: starting\n" + tt.want + "last error: none\n"; got != want {
				t.Errorf("output:\n%s\nwant:\n%s", got, want)
			}
		})
	}
}

// The total line sums busy and idle for running and draining, and shows the
// server limit or "-" (0002 SPEC R152).
func TestPrintTotalLine(t *testing.T) {
	targets := []Target{
		{URL: "a", State: TargetRunning, Busy: 2, Idle: 0},
		{URL: "b", State: TargetRunning, Busy: 1, Idle: 1},
	}
	for _, state := range []State{Running, Draining} {
		got, err := printString(t, Report{State: state, ServerLimit: intPtr(2), Targets: targets})
		if err != nil {
			t.Fatalf("%s: Print = %v", state, err)
		}
		if want := "total: busy 3, idle 1, max 2\n"; !strings.Contains(got, want) {
			t.Errorf("%s: output lacks %q:\n%s", state, want, got)
		}
	}
	got, _ := printString(t, Report{State: Running, Targets: targets})
	if want := "total: busy 3, idle 1, max -\n"; !strings.Contains(got, want) {
		t.Errorf("null serverLimit: output lacks %q:\n%s", want, got)
	}
}

// The starting state prints the target lines, not the total line (0002 SPEC
// R198).
func TestPrintStartingShowsTargets(t *testing.T) {
	r := Report{State: Starting, ServerLimit: intPtr(2), Targets: []Target{
		{URL: "a", State: TargetStarting, MaxRunners: 2},
		{URL: "b", State: TargetErrorWait, LastError: "x", LastErrorAt: printAt, NextRetryAt: &printAt},
	}}
	got, err := printString(t, r)
	if err != nil {
		t.Fatalf("Print = %v", err)
	}
	want := "state: starting\n" +
		"a: starting, busy 0, idle 0, waiting jobs 0, max 2, start failures 0\n" +
		"b: error-wait, busy 0, idle 0, waiting jobs 0, max 0, start failures 0, error: x (2026-11-25T09:30:00+09:00), next retry 2026-11-25T09:30:00+09:00\n" +
		"last error: none\n"
	if got != want {
		t.Errorf("output:\n%s\nwant:\n%s", got, want)
	}
}

// With no running or starting target the first line changes, the other
// lines stay, and Print returns ErrNoRunningTarget, which Reported accepts
// (0002 SPEC R197). A starting or running target keeps `state: running` and exit 0.
func TestPrintNoRunningTarget(t *testing.T) {
	r := Report{State: Running, ServerLimit: intPtr(2), LastError: "x", LastErrorAt: printAt, Targets: []Target{
		{URL: "a", State: TargetErrorDraining, Busy: 1, LastError: "e1", LastErrorAt: printAt},
		{URL: "b", State: TargetErrorWait, LastError: "e2", LastErrorAt: printAt, NextRetryAt: &printAt},
	}}
	got, err := printString(t, r)
	if !errors.Is(err, ErrNoRunningTarget) || !Reported(err) {
		t.Errorf("err = %v, want ErrNoRunningTarget and Reported", err)
	}
	want := "state: running, no target\n" +
		"a: error-draining, busy 1, idle 0, waiting jobs 0, max 0, start failures 0, error: e1 (2026-11-25T09:30:00+09:00)\n" +
		"b: error-wait, busy 0, idle 0, waiting jobs 0, max 0, start failures 0, error: e2 (2026-11-25T09:30:00+09:00), next retry 2026-11-25T09:30:00+09:00\n" +
		"total: busy 1, idle 0, max 2\n" +
		"last error: x (2026-11-25T09:30:00+09:00)\n"
	if got != want {
		t.Errorf("output:\n%s\nwant:\n%s", got, want)
	}
	for _, live := range []TargetState{TargetStarting, TargetRunning} {
		r.Targets[1].State = live
		got, err := printString(t, r)
		if err != nil || !strings.HasPrefix(got, "state: running\n") {
			t.Errorf("with a %s target: err = %v, output:\n%s", live, err, got)
		}
	}
}

// silentServer listens at path, accepts and never writes. It closes the
// listener and every accepted conn, late ones included, when the test ends.
func silentServer(t *testing.T, path string) {
	t.Helper()
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	var (
		mu    sync.Mutex
		conns []net.Conn
	)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			conns = append(conns, c)
			mu.Unlock()
		}
	}()
	t.Cleanup(func() {
		ln.Close()
		<-done // no Accept after this, so conns is complete
		mu.Lock()
		defer mu.Unlock()
		for _, c := range conns {
			c.Close()
		}
	})
}

// A NextRetryAt left on a target outside TargetErrorWait is written as null
// (0002 SPEC R198).
func TestReportJSONNextRetryOnlyInErrorWait(t *testing.T) {
	path := filepath.Join(shortDir(t), "status.sock")
	at := time.Date(2026, 11, 25, 0, 0, 0, 0, time.UTC)
	serve(t, path, staticReporter{Report{State: Running, Targets: []Target{{URL: "p", State: TargetRunning, NextRetryAt: &at}}}})
	if line := readLine(t, path); !strings.Contains(line, `"nextRetryAt":null`) {
		t.Errorf("line = %s, want nextRetryAt null", line)
	}
}
