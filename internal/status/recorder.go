package status

import (
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/dewevrei/actions-runner-scaler/internal/logging"
)

// Clock is the time source for error times.
type Clock interface {
	Now() time.Time
}

type wallClock struct{}

func (wallClock) Now() time.Time { return time.Now() }

// Recorder keeps the last error and its time for status (R26, R45, R56,
// R58). It implements scaler.ErrorRecorder. It is safe for concurrent use.
//
// The text is redacted before it is stored (R31), so a secret that is
// unregistered later still never reaches status. Every Record replaces the
// last error and its time: callers suppress repeats themselves (the
// scaler's errorOnce, which forgets an error once the call succeeds) and
// log at Warn or Error, so the Recorder logs only at Debug.
type Recorder struct {
	redactor logging.Redactor
	clock    Clock
	log      *slog.Logger

	mu     sync.Mutex
	last   string
	lastAt time.Time
}

// NewRecorder returns a Recorder that redacts with r, which is the
// process's logging.Secrets. A nil clock means the wall clock and a nil log
// discards.
func NewRecorder(r logging.Redactor, clock Clock, log *slog.Logger) (*Recorder, error) {
	if r == nil {
		return nil, errors.New("status: nil Redactor")
	}
	if clock == nil {
		clock = wallClock{}
	}
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Recorder{redactor: r, clock: clock, log: log}, nil
}

// Record stores err as the last error with the current time. A nil err is
// ignored. The stored text is "<where>: <err>", redacted.
func (r *Recorder) Record(where string, err error) {
	if err == nil {
		return
	}
	msg := err.Error()
	if where != "" {
		msg = where + ": " + msg
	}
	text := r.redactor.Redact(msg)

	// The time is read under the lock so concurrent records keep their
	// order.
	r.mu.Lock()
	r.last, r.lastAt = text, r.clock.Now()
	r.mu.Unlock()

	r.log.Debug("error recorded for status", "where", where, "err", text)
}

// Last returns the last recorded error text and its time. The text is
// empty and the time zero when nothing was recorded.
func (r *Recorder) Last() (text string, at time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.last, r.lastAt
}

// redact applies the Recorder's redactor.
func (r *Recorder) redact(text string) string {
	return r.redactor.Redact(text)
}
