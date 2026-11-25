package service

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// fakeBinary writes a shell script standing in for launchctl. It appends
// its arguments to a log, prints output, and exits with code. The real
// launchctl is never run.
func fakeBinary(t *testing.T, output string, code int) (bin, argLog string) {
	t.Helper()
	dir := t.TempDir()
	argLog = filepath.Join(dir, "args")
	bin = filepath.Join(dir, "launchctl")
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> '" + argLog + "'\n" +
		"printf '%s' '" + output + "'\nexit " + strconv.Itoa(code) + "\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin, argLog
}

func readArgs(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(data))
}

func TestExecLaunchctlPrint(t *testing.T) {
	ctx := context.Background()
	bin, args := fakeBinary(t, "service = {...}", 0)
	loaded, err := NewExecLaunchctl(bin).Print(ctx, "gui/501/x")
	if err != nil || !loaded {
		t.Errorf("Print exit 0 = %v, %v, want loaded", loaded, err)
	}
	if got := readArgs(t, args); got != "print gui/501/x" {
		t.Errorf("args = %q", got)
	}

	bin, _ = fakeBinary(t, "Could not find service", exitServiceNotFound)
	loaded, err = NewExecLaunchctl(bin).Print(ctx, "gui/501/x")
	if err != nil || loaded {
		t.Errorf("Print exit 113 = %v, %v, want not loaded and no error", loaded, err)
	}

	bin, _ = fakeBinary(t, "Input/output error", 5)
	loaded, err = NewExecLaunchctl(bin).Print(ctx, "gui/501/x")
	if err == nil || loaded || !strings.Contains(err.Error(), "exit status 5") || !strings.Contains(err.Error(), "Input/output error") {
		t.Errorf("Print exit 5 = %v, %v, want an error with the status and output", loaded, err)
	}

	loaded, err = NewExecLaunchctl(filepath.Join(t.TempDir(), "missing")).Print(ctx, "gui/501/x")
	if err == nil || loaded {
		t.Errorf("Print with no binary = %v, %v, want an error", loaded, err)
	}

	canceled, cancel := context.WithCancel(ctx)
	cancel()
	bin, _ = fakeBinary(t, "", 0)
	if loaded, err := NewExecLaunchctl(bin).Print(canceled, "gui/501/x"); err == nil || loaded {
		t.Errorf("Print with canceled ctx = %v, %v, want an error", loaded, err)
	}
}

func TestExecLaunchctlBootoutBootstrap(t *testing.T) {
	ctx := context.Background()
	bin, args := fakeBinary(t, "", 0)
	l := NewExecLaunchctl(bin)
	if err := l.Bootout(ctx, "gui/501/x"); err != nil {
		t.Fatal(err)
	}
	if err := l.Bootstrap(ctx, "gui/501/", "/Users/someone/Library/LaunchAgents/x.plist"); err != nil {
		t.Fatal(err)
	}
	want := "bootout gui/501/x\nbootstrap gui/501/ /Users/someone/Library/LaunchAgents/x.plist"
	if got := readArgs(t, args); got != want {
		t.Errorf("args = %q, want %q", got, want)
	}

	bin, _ = fakeBinary(t, "Bootstrap failed: 5: Input/output error", 5)
	err := NewExecLaunchctl(bin).Bootstrap(ctx, "gui/501/", "/p")
	if err == nil || !strings.Contains(err.Error(), "exit status 5") || !strings.Contains(err.Error(), "Input/output error") {
		t.Errorf("err = %v, want exit status and output", err)
	}
}
