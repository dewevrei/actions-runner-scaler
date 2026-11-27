package status

import (
	"bytes"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dewevrei/actions-runner-scaler/internal/logging"
	"github.com/dewevrei/actions-runner-scaler/internal/scaler"
)

var _ scaler.ErrorRecorder = (*Recorder)(nil)

type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

var t0 = time.Date(2026, 11, 25, 9, 30, 0, 0, time.UTC)

// newTestRecorder returns a Recorder whose log goes to the returned buffer
// through a plain text handler, so the test sees what the Recorder itself
// redacts.
func newTestRecorder(t *testing.T, secrets *logging.Secrets) (*Recorder, *fakeClock, *bytes.Buffer) {
	t.Helper()
	clock := &fakeClock{now: t0}
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	rec, err := NewRecorder(secrets, clock, log)
	if err != nil {
		t.Fatal(err)
	}
	return rec, clock, &buf
}

func TestNewRecorderNeedsRedactor(t *testing.T) {
	if _, err := NewRecorder(nil, nil, nil); err == nil {
		t.Fatal("NewRecorder(nil redactor) succeeded, want an error")
	}
}

// R31: registered secrets, GitHub tokens, private keys and URL queries
// never reach the stored text or the log.
func TestRecorderMasks(t *testing.T) {
	secrets := logging.NewSecrets()
	const jit = "jit-config-value-123"
	secrets.Register(jit)
	rec, _, buf := newTestRecorder(t, secrets)

	token := "ghs_" + strings.Repeat("a", 36)
	key := "-----BEGIN RSA PRIVATE KEY-----\nMIIabc\n-----END RSA PRIVATE KEY-----"
	err := errors.New("start failed: jit " + jit + " token " + token +
		" key " + key + " url https://api.github.com/x?access_token=q1w2e3")
	rec.Record("start runner", err)

	text, at := rec.Last()
	for _, secret := range []string{jit, token, "MIIabc", "q1w2e3"} {
		if strings.Contains(text, secret) {
			t.Errorf("stored text has %q: %s", secret, text)
		}
		if strings.Contains(buf.String(), secret) {
			t.Errorf("log has %q: %s", secret, buf.String())
		}
	}
	if !strings.HasPrefix(text, "start runner: start failed: jit "+logging.Placeholder) {
		t.Errorf("text = %q, want the site and the redacted error", text)
	}
	if !at.Equal(t0) {
		t.Errorf("at = %v, want %v", at, t0)
	}
	// Unregistering later does not expose what was stored.
	secrets.Unregister(jit)
	if text, _ := rec.Last(); strings.Contains(text, jit) {
		t.Errorf("text after Unregister has the secret: %s", text)
	}
}

// Every Record updates the last error and its time, a repeat included:
// repeats are suppressed by the callers' errorOnce, which forgets an error
// once the call succeeds, so a repeat that reaches the Recorder is news.
func TestRecorderRecordsRepeats(t *testing.T) {
	rec, clock, buf := newTestRecorder(t, logging.NewSecrets())

	rec.Record("session", errors.New("503"))
	clock.advance(time.Minute)
	rec.Record("session", errors.New("503"))
	if text, at := rec.Last(); text != "session: 503" || !at.Equal(t0.Add(time.Minute)) {
		t.Errorf("Last() = %q, %v; want the repeat's time %v", text, at, t0.Add(time.Minute))
	}

	clock.advance(time.Minute)
	rec.Record("watch", errors.New("409"))
	if text, at := rec.Last(); text != "watch: 409" || !at.Equal(t0.Add(2*time.Minute)) {
		t.Errorf("Last() = %q, %v; want the other site's record", text, at)
	}

	clock.advance(time.Minute)
	rec.Record("watch", nil)
	if text, at := rec.Last(); text != "watch: 409" || !at.Equal(t0.Add(2*time.Minute)) {
		t.Errorf("a nil error changed Last() to %q, %v", text, at)
	}

	// The Recorder logs only at Debug; callers log at Warn or Error.
	if strings.Contains(buf.String(), "level=WARN") || strings.Contains(buf.String(), "level=ERROR") {
		t.Errorf("Recorder logged above Debug:\n%s", buf.String())
	}
	if n := strings.Count(buf.String(), "level=DEBUG"); n != 3 {
		t.Errorf("Debug lines = %d, want 3", n)
	}
}

func TestRecorderEmpty(t *testing.T) {
	rec, _, _ := newTestRecorder(t, logging.NewSecrets())
	if text, at := rec.Last(); text != "" || !at.IsZero() {
		t.Errorf("Last() = %q, %v; want nothing", text, at)
	}
}

// TestWithWhere checks that where is not repeated when the error text
// already starts with it (an error of the same operation).
func TestWithWhere(t *testing.T) {
	tests := []struct{ where, msg, want string }{
		{"docker ping", "docker ping: no such file", "docker ping: no such file"},
		{"docker ping", "dial: refused", "docker ping: dial: refused"},
		{"", "boom", "boom"},
		{"docker", "docker ping: x", "docker: docker ping: x"},
	}
	for _, tt := range tests {
		if got := WithWhere(tt.where, tt.msg); got != tt.want {
			t.Errorf("WithWhere(%q, %q) = %q, want %q", tt.where, tt.msg, got, tt.want)
		}
	}
}

// TestRecorderNoRepeatedWhere records an error of the same operation, as a
// failed Docker ping is, and checks that where appears once.
func TestRecorderNoRepeatedWhere(t *testing.T) {
	rec, _, _ := newTestRecorder(t, logging.NewSecrets())
	rec.Record("docker ping", errors.New("docker ping: refused"))
	if text, _ := rec.Last(); text != "docker ping: refused" {
		t.Errorf("Last() = %q, want %q", text, "docker ping: refused")
	}
}
