package service

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/dewevrei/actions-runner-scaler/internal/status"
)

func callsOf(f *fakeLaunchctl) string {
	calls, _ := f.snapshot()
	return strings.Join(calls, "\n")
}

// A loaded label is started with kickstart, without -k, after a look at the
// status socket, and Start waits until the scaler answers there.
func TestStartKickstartsLoadedLabel(t *testing.T) {
	f := &fakeLaunchctl{loaded: true}
	m, _ := newTestManagerWithDaemon(t, f, &fakeDaemon{runsFromFind: 3})
	writeInstalled(t, m)
	started, err := m.Start(context.Background(), Label)
	if err != nil || !started {
		t.Fatalf("Start = %v, %v, want true, nil", started, err)
	}
	if got, want := callsOf(f), "find\nprint "+target+"\nkickstart "+target+"\nfind\nfind"; got != want {
		t.Fatalf("calls:\n%s\nwant:\n%s", got, want)
	}
}

// Without a plist Start fails, says to install, and calls nothing.
func TestStartNotInstalled(t *testing.T) {
	f := &fakeLaunchctl{}
	m, _ := newTestManager(t, f)
	started, err := m.Start(context.Background(), Label)
	if started || err == nil || !strings.Contains(err.Error(), "service install") {
		t.Fatalf("Start = %v, %v, want an error naming service install", started, err)
	}
	if got := callsOf(f); got != "" {
		t.Fatalf("calls = %q, want none", got)
	}
}

// A running scaler is left alone.
func TestStartAlreadyRunning(t *testing.T) {
	f := &fakeLaunchctl{loaded: true}
	m, _ := newTestManagerWithDaemon(t, f, runningDaemon(4242, status.Running))
	writeInstalled(t, m)
	started, err := m.Start(context.Background(), Label)
	if err != nil || started {
		t.Fatalf("Start = %v, %v, want false, nil", started, err)
	}
	if got := callsOf(f); got != "find" {
		t.Fatalf("calls = %q, want only find", got)
	}
}

// A plist that is not loaded is bootstrapped; RunAtLoad starts the scaler.
func TestStartBootstrapsUnloadedLabel(t *testing.T) {
	f := &fakeLaunchctl{}
	m, _ := newTestManagerWithDaemon(t, f, &fakeDaemon{runsFromFind: 2})
	writeInstalled(t, m)
	started, err := m.Start(context.Background(), Label)
	if err != nil || !started {
		t.Fatalf("Start = %v, %v, want true, nil", started, err)
	}
	want := "find\nprint " + target + "\nbootstrap gui/501/ " + m.PlistPath(Label) + "\nfind"
	if got := callsOf(f); got != want {
		t.Fatalf("calls:\n%s\nwant:\n%s", got, want)
	}
}

// A scaler that never answers after the kickstart is an error that names
// the launchctl print command to look at.
func TestStartScalerNeverAnswers(t *testing.T) {
	f := &fakeLaunchctl{loaded: true}
	m, _ := newTestManager(t, f)
	m.startLimit = 20 * time.Millisecond
	writeInstalled(t, m)
	_, err := m.Start(context.Background(), Label)
	if err == nil || !strings.Contains(err.Error(), "launchctl print "+target) || !strings.Contains(err.Error(), "did not answer") {
		t.Fatalf("Start = %v, want an error naming launchctl print %s", err, target)
	}
}

// A failed status query while waiting is an error.
func TestStartWaitFindError(t *testing.T) {
	boom := errors.New("boom")
	f := &fakeLaunchctl{loaded: true}
	m, _ := newTestManagerWithDaemon(t, f, &fakeDaemon{})
	writeInstalled(t, m)
	d := m.daemon.(*fakeDaemon)
	// The first find (before the start) is fine; later ones fail.
	d.runsFromFind = 0
	m.daemon = &failingAfterFirstFind{fakeDaemon: d, err: boom}
	if _, err := m.Start(context.Background(), Label); !errors.Is(err, boom) {
		t.Fatalf("Start = %v, want an error wrapping boom", err)
	}
}

// failingAfterFirstFind answers the first Find as not running and fails
// the rest.
type failingAfterFirstFind struct {
	*fakeDaemon
	err   error
	calls int
}

func (d *failingAfterFirstFind) Find(ctx context.Context) (status.Report, int, bool, error) {
	d.calls++
	if d.calls > 1 {
		return status.Report{}, 0, false, d.err
	}
	return d.fakeDaemon.Find(ctx)
}

func TestStartErrors(t *testing.T) {
	boom := errors.New("boom")
	for _, tt := range []struct {
		name string
		f    *fakeLaunchctl
		d    *fakeDaemon
		want string
	}{
		{"kickstart fails", &fakeLaunchctl{loaded: true, kickstartErr: boom}, &fakeDaemon{}, "kickstart " + target},
		{"bootstrap fails", &fakeLaunchctl{bootstrapErr: boom}, &fakeDaemon{}, "bootstrap"},
		{"print fails", &fakeLaunchctl{printErr: boom}, &fakeDaemon{}, "print " + target},
		{"find fails", &fakeLaunchctl{loaded: true}, &fakeDaemon{findErr: boom}, "status socket"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			m, _ := newTestManagerWithDaemon(t, tt.f, tt.d)
			writeInstalled(t, m)
			started, err := m.Start(context.Background(), Label)
			if started || !errors.Is(err, boom) || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Start = %v, %v, want an error wrapping boom and naming %q", started, err, tt.want)
			}
		})
	}
}
