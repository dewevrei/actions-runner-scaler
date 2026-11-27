package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/dewevrei/actions-runner-scaler/internal/daemon"
	"github.com/dewevrei/actions-runner-scaler/internal/service"
	"github.com/dewevrei/actions-runner-scaler/internal/status"
)

// refusingHosts returns the host factories every test that goes through
// dispatch passes to commands: they never build a host, so a test can never
// reach the user's launchd, status socket, lock or scaler.
func refusingHosts() hosts {
	return countingHosts(new(int), new(int))
}

// countingHosts returns refusing factories that count their calls.
func countingHosts(serviceCalls, runCalls *int) hosts {
	return hosts{
		// A path in a directory that does not exist: status finds no scaler.
		statusSocket: func() (string, error) { return "/nonexistent-dir/status.sock", nil },
		deleteDeps: func(env, string) (deleteScaleSetDeps, error) {
			return deleteScaleSetDeps{}, errRealHostInTests
		},
		service: func() (serviceHost, error) {
			*serviceCalls++
			return serviceHost{}, errRealHostInTests
		},
		daemonConfig: func(env, string, <-chan os.Signal) (daemon.Config, error) {
			*runCalls++
			return daemon.Config{}, errRealHostInTests
		},
	}
}

// With the test factories, service and run commands that parse reach only
// the factory: it refuses, nothing else is called, and the command exits 1.
func TestDispatchRefusesRealHost(t *testing.T) {
	for _, args := range [][]string{
		{"service", "start"},
		{"service", "restart"},
		{"service", "stop"},
		{"service", "install"},
		{"service", "uninstall"},
		{"run"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			var serviceCalls, runCalls int
			var stdout, stderr bytes.Buffer
			if got := dispatch(context.Background(), newEnv(&stdout, &stderr), args, commands(countingHosts(&serviceCalls, &runCalls))); got != 1 {
				t.Errorf("exit = %d, want 1", got)
			}
			if !strings.Contains(stderr.String(), "real host refused in tests") {
				t.Errorf("stderr = %q, want the refusal", stderr.String())
			}
			if wantService := b2i(args[0] == "service"); serviceCalls != wantService || runCalls != 1-wantService {
				t.Errorf("factory calls: service %d, run %d", serviceCalls, runCalls)
			}
			if stdout.Len() != 0 {
				t.Errorf("stdout = %q, want nothing", stdout.String())
			}
		})
	}
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

// A command-line mistake is found before any factory is called.
func TestDispatchUsageErrorBuildsNoHost(t *testing.T) {
	var serviceCalls, runCalls int
	for _, args := range [][]string{{"service", "bogus"}, {"service"}, {"run", "extra"}} {
		var stdout, stderr bytes.Buffer
		if got := dispatch(context.Background(), newEnv(&stdout, &stderr), args, commands(countingHosts(&serviceCalls, &runCalls))); got != 1 {
			t.Errorf("%v: exit = %d, want 1", args, got)
		}
	}
	if serviceCalls != 0 || runCalls != 0 {
		t.Errorf("factories called: service %d, run %d", serviceCalls, runCalls)
	}
}

// A fake host passed to commands is the one service start uses.
func TestDispatchServiceUsesInjectedHost(t *testing.T) {
	f := &fakeLaunchctl{}
	h := refusingHosts()
	h.service = func() (serviceHost, error) {
		host := testHost(t.TempDir(), f)
		d := newFakeDaemon(false, 0)
		host.daemon = d
		// An installed plist, so start has something to start.
		if err := runService(context.Background(), newEnv(new(bytes.Buffer), new(bytes.Buffer)), host, "install", writeTestConfig(t, t.TempDir(), "10m")); err != nil {
			return serviceHost{}, err
		}
		f.loaded = true
		f.onKickstart = func() { d.launch(4343) }
		return host, nil
	}
	var stdout, stderr bytes.Buffer
	if got := dispatch(context.Background(), newEnv(&stdout, &stderr), []string{"service", "start"}, commands(h)); got != 0 {
		t.Fatalf("exit = %d, stderr %s", got, stderr.String())
	}
	if n := len(f.calls); n == 0 || f.calls[len(f.calls)-1] != "kickstart gui/501/"+service.Label {
		t.Errorf("calls = %q, want them to end with a kickstart", f.calls)
	}
}

// delete-scaleset builds its lock, GitHub and Docker only through the
// factory, which refuses here, and status connects only to the path the
// factory gives.
func TestDispatchDeleteScaleSetAndStatusUseFactories(t *testing.T) {
	var stdout, stderr bytes.Buffer
	args := []string{"delete-scaleset", "https://github.com/acme/widget"}
	if got := dispatch(context.Background(), newEnv(&stdout, &stderr), args, commands(refusingHosts())); got != 1 {
		t.Errorf("delete-scaleset exit = %d, want 1", got)
	}
	if !strings.Contains(stderr.String(), "real host refused in tests") {
		t.Errorf("delete-scaleset stderr = %q, want the refusal", stderr.String())
	}

	// The real factories under test refuse the default socket, so status
	// reports not running without connecting to it.
	stdout.Reset()
	stderr.Reset()
	if got := dispatch(context.Background(), newEnv(&stdout, &stderr), []string{"status"}, commands(realHosts())); got != 1 {
		t.Errorf("status exit = %d, want 1", got)
	}
	if !strings.Contains(stdout.String(), status.NotRunning) {
		t.Errorf("status stdout = %q, want the not-running text", stdout.String())
	}
	if !strings.Contains(stderr.String(), "real host refused in tests") {
		t.Errorf("status stderr = %q, want the refusal", stderr.String())
	}
}

// The real factories refuse in a test binary, so even a test that builds
// them cannot touch the machine.
func TestRealHostsRefuseInTests(t *testing.T) {
	h := realHosts()
	if _, err := h.service(); !errors.Is(err, errRealHostInTests) {
		t.Errorf("real service host: err = %v, want %v", err, errRealHostInTests)
	}
	if _, err := h.daemonConfig(newEnv(new(bytes.Buffer), new(bytes.Buffer)), "x.toml", nil); !errors.Is(err, errRealHostInTests) {
		t.Errorf("real daemon config: err = %v, want %v", err, errRealHostInTests)
	}
	if _, err := h.statusSocket(); !errors.Is(err, errRealHostInTests) {
		t.Errorf("real status socket: err = %v, want %v", err, errRealHostInTests)
	}
	if _, err := h.deleteDeps(newEnv(new(bytes.Buffer), new(bytes.Buffer)), "x.toml"); !errors.Is(err, errRealHostInTests) {
		t.Errorf("real delete-scaleset deps: err = %v, want %v", err, errRealHostInTests)
	}
}
