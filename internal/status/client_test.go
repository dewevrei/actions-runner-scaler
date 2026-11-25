package status

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// SPEC "외부 계약": the output and exit code of each state (R26, R45, R56,
// R58, R86).
func TestPrint(t *testing.T) {
	at := time.Date(2026, 11, 25, 9, 30, 0, 0, time.FixedZone("KST", 9*3600))
	pools := []Pool{
		{Name: "a", Busy: 2, Idle: 1},
		{Name: "b", Busy: 0, Idle: 0, StartFailures: 3, LastStartError: "pull failed", LastStartErrorAt: at},
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
			want: "상태: 시작 중\n마지막 오류: 없음\n",
		},
		{
			name: "starting with error",
			r:    Report{State: Starting, LastError: "docker: ping failed", LastErrorAt: at},
			want: "상태: 시작 중\n마지막 오류: docker: ping failed (2026-11-25T09:30:00+09:00)\n",
		},
		{
			name: "running",
			r:    Report{State: Running, Pools: pools},
			want: "상태: 정상\n" +
				"a: 실행 중 2, 대기 1, 연속 시작 실패 0\n" +
				"b: 실행 중 0, 대기 0, 연속 시작 실패 3 (pull failed, 2026-11-25T09:30:00+09:00)\n" +
				"마지막 오류: 없음\n",
		},
		{
			name: "draining",
			r:    Report{State: Draining, Pools: pools[:1], LastError: "x", LastErrorAt: at},
			want: "상태: drain 중\na: 실행 중 2, 대기 1, 연속 시작 실패 0\n마지막 오류: x (2026-11-25T09:30:00+09:00)\n",
		},
		{
			name:    "error-wait",
			r:       Report{State: ErrorWait, Pools: pools, LastError: "config: unknown key x", LastErrorAt: at},
			want:    "상태: 오류 대기\n오류: config: unknown key x (2026-11-25T09:30:00+09:00)\n",
			wantErr: true,
		},
		{
			name:    "unknown state",
			r:       Report{State: "paused"},
			want:    "상태: 알 수 없음 (\"paused\")\n",
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

// R27: no socket means "실행 중 아님" and exit 1.
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
		{Report{State: Running, Pools: []Pool{{Name: "p", Busy: 1, Idle: 1}}},
			"상태: 정상\np: 실행 중 1, 대기 1, 연속 시작 실패 0\n마지막 오류: 없음\n", false},
		{Report{State: ErrorWait, LastError: "e", LastErrorAt: at},
			"상태: 오류 대기\n오류: e (2026-11-25T00:00:00Z)\n", true},
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
		Pools:       []Pool{{Name: "p", Busy: 1, Idle: 2, StartFailures: 1, LastStartError: "s", LastStartErrorAt: at}},
		LastError:   "e",
		LastErrorAt: at,
	}})
	line := readLine(t, path)
	want := `{"state":"running","pools":[{"name":"p","busy":1,"idle":2,"startFailures":1,` +
		`"lastStartError":"s","lastStartErrorAt":"2026-11-25T00:00:00Z"}],` +
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
	serve(t, path, staticReporter{Report{State: Starting, Pools: []Pool{{Name: "p"}}}})
	line := readLine(t, path)
	want := `{"state":"starting","pools":[{"name":"p","busy":0,"idle":0,"startFailures":0,` +
		`"lastStartError":"","lastStartErrorAt":""}],"lastError":"","lastErrorAt":""}` + "\n"
	if line != want {
		t.Errorf("line:\n%s\nwant:\n%s", line, want)
	}
	r, err := Query(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	if !r.LastErrorAt.IsZero() || !r.Pools[0].LastStartErrorAt.IsZero() {
		t.Errorf("round trip = %+v, want zero times", r)
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

// A nil Pools is written as [].
func TestReportJSONNilPools(t *testing.T) {
	b, err := json.Marshal(Report{State: Starting})
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"state":"starting","pools":[],"lastError":"","lastErrorAt":""}`; string(b) != want {
		t.Errorf("json = %s, want %s", b, want)
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
