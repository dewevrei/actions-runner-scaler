package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"strings"
	"testing"

	"github.com/dewevrei/actions-runner-scaler/internal/logging"
)

func TestExitCode(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want int
	}{
		{"success or signal stop", nil, 0},
		{"help", flag.ErrHelp, 0},
		{"help joined with a failure", errors.Join(errors.New("x"), flag.ErrHelp), 1},
		{"failure", errors.New("lock held"), 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := exitCode(tt.err); got != tt.want {
				t.Errorf("exitCode(%v) = %d, want %d", tt.err, got, tt.want)
			}
		})
	}
}

func TestDispatch(t *testing.T) {
	var gotArgs []string
	cmds := map[string]command{
		"ok":   {run: func(_ context.Context, _ env, args []string) error { gotArgs = args; return nil }},
		"fail": {run: func(context.Context, env, []string) error { return errors.New("boom") }},
	}
	tests := []struct {
		name       string
		args       []string
		want       int
		wantStderr string
	}{
		{"no args", nil, 1, "usage:"},
		{"unknown", []string{"nope"}, 1, `unknown command "nope"`},
		{"help", []string{"-h"}, 0, ""},
		{"success", []string{"ok", "a", "b"}, 0, ""},
		{"failure", []string{"fail"}, 1, `"msg":"command failed"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			got := dispatch(context.Background(), newEnv(&stdout, &stderr), tt.args, cmds)
			if got != tt.want {
				t.Errorf("exit = %d, want %d", got, tt.want)
			}
			if !strings.Contains(stderr.String(), tt.wantStderr) {
				t.Errorf("stderr = %q, want it to contain %q", stderr.String(), tt.wantStderr)
			}
		})
	}
	if strings.Join(gotArgs, " ") != "a b" {
		t.Errorf("args passed to command = %v, want [a b]", gotArgs)
	}
}

// The final command error goes through the redacting JSON logger (R31).
func TestDispatchLogsRedactedJSONError(t *testing.T) {
	const (
		secret = "fake-installation-token"
		query  = "token=fake-query-value"
	)
	cmds := map[string]command{
		"fail": {run: func(context.Context, env, []string) error {
			return fmt.Errorf("Get %q: bad credentials %s", "https://h.invalid/x?"+query, secret)
		}},
	}
	var stdout, stderr bytes.Buffer
	e := newEnv(&stdout, &stderr)
	e.secrets.Register(secret)
	if got := dispatch(context.Background(), e, []string{"fail"}, cmds); got != 1 {
		t.Fatalf("exit = %d, want 1", got)
	}

	var line struct {
		Level   string `json:"level"`
		Msg     string `json:"msg"`
		Command string `json:"command"`
		Err     string `json:"err"`
	}
	if err := json.Unmarshal(stderr.Bytes(), &line); err != nil {
		t.Fatalf("stderr is not one JSON line: %v\n%s", err, stderr.String())
	}
	if line.Level != "ERROR" || line.Msg != "command failed" || line.Command != "fail" {
		t.Errorf("log line = %+v, want ERROR \"command failed\" for fail", line)
	}
	for _, leak := range []string{secret, query, "fake-query-value"} {
		if strings.Contains(stderr.String(), leak) {
			t.Errorf("stderr contains %q:\n%s", leak, stderr.String())
		}
	}
	if want := "https://h.invalid/x?" + logging.Placeholder; !strings.Contains(line.Err, want) {
		t.Errorf("err = %q, want it to contain %q", line.Err, want)
	}
	if !strings.Contains(line.Err, "bad credentials "+logging.Placeholder) {
		t.Errorf("err = %q, want the secret replaced by %s", line.Err, logging.Placeholder)
	}
}

func TestSubcommandArgs(t *testing.T) {
	tests := []struct {
		args []string
		want int
	}{
		{[]string{"version"}, 0},
		{[]string{"version", "-h"}, 0},
		{[]string{"run", "-bogus"}, 1},
		{[]string{"delete-scaleset"}, 1},
		{[]string{"service", "restart"}, 1},
	}
	for _, tt := range tests {
		t.Run(strings.Join(tt.args, " "), func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			if got := dispatch(context.Background(), newEnv(&stdout, &stderr), tt.args, commands()); got != tt.want {
				t.Errorf("exit = %d, want %d (stderr %q)", got, tt.want, stderr.String())
			}
		})
	}
}

// Command-line mistakes are printed once as plain text, not logged as JSON,
// and still exit 1. A real failure is logged.
func TestDispatchUsageErrors(t *testing.T) {
	tests := []struct {
		args       []string
		wantStderr string
		wantLog    bool
	}{
		{[]string{"run", "-bogus"}, "flag provided but not defined: -bogus", false},
		{[]string{"delete-scaleset"}, "usage: delete-scaleset", false},
		{[]string{"service", "restart"}, "usage: service install|uninstall", false},
	}
	for _, tt := range tests {
		t.Run(strings.Join(tt.args, " "), func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			if got := dispatch(context.Background(), newEnv(&stdout, &stderr), tt.args, commands()); got != 1 {
				t.Errorf("exit = %d, want 1", got)
			}
			out := stderr.String()
			if !strings.Contains(out, tt.wantStderr) {
				t.Errorf("stderr = %q, want it to contain %q", out, tt.wantStderr)
			}
			if logged := strings.Contains(out, `"msg":"command failed"`); logged != tt.wantLog {
				t.Errorf("command failed logged = %v, want %v:\n%s", logged, tt.wantLog, out)
			}
			if n := strings.Count(out, tt.wantStderr); !tt.wantLog && n != 1 {
				t.Errorf("stderr has %q %d times, want once:\n%s", tt.wantStderr, n, out)
			}
		})
	}
}
