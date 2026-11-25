package status

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"syscall"
	"time"
)

const (
	// queryTimeout bounds a whole query when ctx has no earlier deadline.
	queryTimeout = 5 * time.Second
	// maxReportBytes bounds the line read from the socket.
	maxReportBytes = 1 << 20
	// timeLayout formats times in the offset the daemon wrote them in.
	timeLayout = time.RFC3339
)

// Output lines for a query that fails.
const (
	// NotRunning is printed when connecting to the socket fails (R27).
	NotRunning = "실행 중 아님"
	// Unreadable is printed when the socket accepted but no report could
	// be read, such as a timeout or a malformed line.
	Unreadable = "상태를 읽지 못함"
)

// The errors Show returns after printing a complete answer. Every other
// error from Show is a failure the caller should log.
var (
	// ErrNotRunning means nothing listens at the socket path: the file is
	// missing (ENOENT) or no process accepts on it (ECONNREFUSED) (R27).
	ErrNotRunning = errors.New("status: the scaler is not running")
	// ErrErrorWait is returned by Print for the error-wait state, which
	// exits 1 (R45).
	ErrErrorWait = errors.New("status: the scaler is in error-wait")
	// ErrUnknownState is returned by Print for a state it does not know.
	ErrUnknownState = errors.New("status: unknown state")
)

// connectError is a failure to connect, as opposed to one after connecting.
type connectError struct{ err error }

func (e connectError) Error() string { return "status: connect: " + e.err.Error() }

func (e connectError) Unwrap() error { return e.err }

// Query connects to the socket at path and reads one report. When nothing
// listens at path the error wraps ErrNotRunning.
func Query(ctx context.Context, path string) (Report, error) {
	r, _, err := query(ctx, path, false)
	return r, err
}

// QueryPeer is Query that also returns the pid of the process serving the
// socket. The pid comes from the kernel's record of the connected peer
// (LOCAL_PEERPID on macOS), not from the report, so it names the process
// that listens on path. On error the pid is 0.
func QueryPeer(ctx context.Context, path string) (Report, int, error) {
	return query(ctx, path, true)
}

func query(ctx context.Context, path string, wantPID bool) (Report, int, error) {
	ctx, cancel := context.WithTimeout(ctx, queryTimeout)
	defer cancel()
	var d net.Dialer
	conn, err := d.DialContext(ctx, "unix", path)
	if err != nil {
		if errors.Is(err, syscall.ENOENT) || errors.Is(err, syscall.ECONNREFUSED) {
			err = fmt.Errorf("%w: %w", ErrNotRunning, err)
		}
		return Report{}, 0, connectError{err: err}
	}
	defer conn.Close()
	var pid int
	if wantPID {
		uc, ok := conn.(*net.UnixConn)
		if !ok {
			return Report{}, 0, fmt.Errorf("status: %T is not a unix connection", conn)
		}
		if pid, err = peerPID(uc); err != nil {
			return Report{}, 0, fmt.Errorf("status: peer pid: %w", err)
		}
	}
	if dl, ok := ctx.Deadline(); ok {
		if err := conn.SetDeadline(dl); err != nil {
			return Report{}, 0, fmt.Errorf("status: %w", err)
		}
	}
	var r Report
	if err := json.NewDecoder(io.LimitReader(conn, maxReportBytes)).Decode(&r); err != nil {
		return Report{}, 0, fmt.Errorf("status: read report: %w", err)
	}
	return r, pid, nil
}

// Show queries the socket at path and prints the result to w. It returns
// nil when the status command exits 0 and an error when it exits 1. A
// failed connection prints NotRunning (R27) and a failure after connecting
// prints Unreadable. The error wraps ErrNotRunning, ErrErrorWait or
// ErrUnknownState when the output is the whole answer; any other error,
// such as EACCES, a timeout or a malformed report, is a fault to log.
func Show(ctx context.Context, w io.Writer, path string) error {
	r, err := Query(ctx, path)
	if err != nil {
		if errors.As(err, new(connectError)) {
			fmt.Fprintln(w, NotRunning)
		} else {
			fmt.Fprintln(w, Unreadable)
		}
		return err
	}
	return Print(w, r)
}

// Reported reports whether err from Show is one whose printed output is the
// whole answer, so the caller exits 1 without logging it.
func Reported(err error) bool {
	return errors.Is(err, ErrNotRunning) || errors.Is(err, ErrErrorWait) || errors.Is(err, ErrUnknownState)
}

// Print writes r in the status command's format (SPEC "외부 계약") and
// returns nil for the states that exit 0 (starting, running, draining) and
// an error for those that exit 1.
func Print(w io.Writer, r Report) error {
	var b strings.Builder
	var err error
	switch r.State {
	case Starting:
		b.WriteString("상태: 시작 중\n")
		writeLastError(&b, r)
	case Running:
		b.WriteString("상태: 정상\n")
		writePools(&b, r.Pools)
		writeLastError(&b, r)
	case Draining:
		b.WriteString("상태: drain 중\n")
		writePools(&b, r.Pools)
		writeLastError(&b, r)
	case ErrorWait:
		b.WriteString("상태: 오류 대기\n")
		fmt.Fprintf(&b, "오류: %s\n", withTime(r.LastError, r.LastErrorAt))
		err = ErrErrorWait
	default:
		fmt.Fprintf(&b, "상태: 알 수 없음 (%q)\n", r.State)
		err = fmt.Errorf("%w %q", ErrUnknownState, r.State)
	}
	_, _ = io.WriteString(w, b.String())
	return err
}

func writePools(b *strings.Builder, pools []Pool) {
	for _, p := range pools {
		fmt.Fprintf(b, "%s: 실행 중 %d, 대기 %d, 연속 시작 실패 %d", p.Name, p.Busy, p.Idle, p.StartFailures)
		if p.StartFailures > 0 {
			fmt.Fprintf(b, " (%s, %s)", orNone(p.LastStartError), formatTime(p.LastStartErrorAt))
		}
		b.WriteByte('\n')
	}
}

func writeLastError(b *strings.Builder, r Report) {
	fmt.Fprintf(b, "마지막 오류: %s\n", withTime(r.LastError, r.LastErrorAt))
}

// withTime is "<text> (<time>)", or "없음" when there is no error.
func withTime(text string, at time.Time) string {
	if text == "" {
		return "없음"
	}
	return text + " (" + formatTime(at) + ")"
}

func orNone(text string) string {
	if text == "" {
		return "없음"
	}
	return text
}

func formatTime(at time.Time) string {
	if at.IsZero() {
		return "시각 없음"
	}
	return at.Format(timeLayout)
}
