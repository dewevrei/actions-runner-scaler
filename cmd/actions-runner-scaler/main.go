// Command actions-runner-scaler runs ephemeral GitHub Actions self-hosted
// runners as local Docker containers and scales them with the number of
// assigned jobs, using github.com/actions/scaleset.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sort"

	"github.com/dewevrei/actions-runner-scaler/internal/daemon"
	"github.com/dewevrei/actions-runner-scaler/internal/logging"
)

// env carries the process streams and the logger so commands stay
// testable.
type env struct {
	stdout io.Writer
	// stderr receives plain-text usage and flag messages.
	stderr io.Writer
	// logger writes JSON to stderr with registered secrets and URL queries
	// redacted (R31).
	logger *slog.Logger
	// secrets is the set the logger redacts; commands register runtime
	// secrets such as tokens and JIT configs in it.
	secrets *logging.Secrets
}

// newEnv returns an env whose logger writes JSON to stderr.
func newEnv(stdout, stderr io.Writer) env {
	secrets := logging.NewSecrets()
	handler := logging.NewRedactingHandler(slog.NewJSONHandler(stderr, nil), secrets)
	return env{
		stdout:  stdout,
		stderr:  stderr,
		logger:  slog.New(handler),
		secrets: secrets,
	}
}

// command is one subcommand. run receives the arguments after the
// subcommand name. A signal-triggered stop returns nil (R84).
type command struct {
	summary string
	run     func(ctx context.Context, e env, args []string) error
}

// hosts builds what the commands that act on the machine need: the service
// command's launchd, status socket and lock, and the run command's daemon.
// main passes the real ones; tests pass fakes or ones that refuse, so no
// test reaches the user's LaunchAgent or scaler.
type hosts struct {
	service      func() (serviceHost, error)
	daemonConfig func(e env, configPath string, sigs <-chan os.Signal) (daemon.Config, error)
}

// realHosts returns the factories for this machine.
func realHosts() hosts {
	return hosts{service: realServiceHost, daemonConfig: realDaemonConfig}
}

func commands(h hosts) map[string]command {
	return map[string]command{
		"run":             {summary: "run the scaler service", run: runCmd(h)},
		"status":          {summary: "print the running scaler's state", run: statusCmd},
		"delete-scaleset": {summary: "delete a configured repository's scale set and its runners", run: deleteScaleSetCmd},
		"service":         {summary: "install, start, stop, restart or uninstall the LaunchAgent", run: serviceCmd(h)},
		"init":            {summary: "write a new config file", run: initCmd},
		"add-repo":        {summary: "add a repository to the config file", run: addRepoCmd},
		"remove-repo":     {summary: "remove a repository from the config file", run: removeRepoCmd},
		"doctor":          {summary: "check the config file and what it depends on", run: doctorCmd},
		"app-link":        {summary: "print the link that creates the GitHub App", run: appLinkCmd},
		"version":         {summary: "print the version", run: versionCmd},
	}
}

func main() {
	e := newEnv(os.Stdout, os.Stderr)
	os.Exit(dispatch(context.Background(), e, os.Args[1:], commands(realHosts())))
}

// dispatch runs the named subcommand and returns the process exit code.
func dispatch(ctx context.Context, e env, args []string, cmds map[string]command) int {
	if len(args) == 0 {
		usage(e.stderr, cmds)
		return 1
	}
	name := args[0]
	if name == "-h" || name == "-help" || name == "--help" || name == "help" {
		usage(e.stdout, cmds)
		return 0
	}
	cmd, ok := cmds[name]
	if !ok {
		fmt.Fprintf(e.stderr, "unknown command %q\n", name)
		usage(e.stderr, cmds)
		return 1
	}
	err := cmd.run(ctx, e, args[1:])
	// A bare usageError is a command-line mistake, not a failure to log as
	// JSON; joined with a real failure it is logged.
	if ue, ok := err.(usageError); ok {
		if !ue.shown {
			fmt.Fprintln(e.stderr, ue.err)
		}
	} else if _, ok := err.(reportedError); ok {
		// The command has already shown the outcome; it only exits 1.
	} else if err != nil && err != flag.ErrHelp {
		e.logger.Error("command failed", "command", name, "err", err)
	}
	return exitCode(err)
}

// exitCode maps a command result to the exit code. Signal stops return nil
// and exit 0 (R84); every other failure is non-zero (R85) so launchd
// restarts the service (KeepAlive SuccessfulExit=false).
func exitCode(err error) int {
	// Only a bare ErrHelp means "-h"; joined with a real failure it must
	// stay non-zero.
	if err == nil || err == flag.ErrHelp {
		return 0
	}
	return 1
}

func usage(w io.Writer, cmds map[string]command) {
	names := make([]string, 0, len(cmds))
	for n := range cmds {
		names = append(names, n)
	}
	sort.Strings(names)
	fmt.Fprintln(w, "usage: actions-runner-scaler <command> [flags]")
	fmt.Fprintln(w, "\ncommands:")
	for _, n := range names {
		fmt.Fprintf(w, "  %-16s %s\n", n, cmds[n].summary)
	}
}

// usageError is a command-line mistake. dispatch prints it as plain text
// instead of logging it, and the command exits 1.
type usageError struct {
	err error
	// shown means flag has already printed err and the usage.
	shown bool
}

func (e usageError) Error() string { return e.err.Error() }

func (e usageError) Unwrap() error { return e.err }

// reportedError is a failure the command has already reported on stdout,
// such as status finding the scaler not running or in error-wait. dispatch
// exits 1 without logging it. Like usageError it counts only bare, so a
// joined real failure is still logged.
type reportedError struct{ err error }

func (e reportedError) Error() string { return e.err.Error() }

func (e reportedError) Unwrap() error { return e.err }

// badUsage returns a usageError with msg, for wrong positional arguments.
func badUsage(msg string) error {
	return usageError{err: errors.New(msg)}
}

// parseFlags parses args into fs. A parse error becomes a usageError because
// flag has already printed it with the usage; -h stays flag.ErrHelp.
func parseFlags(fs *flag.FlagSet, args []string) error {
	err := fs.Parse(args)
	if err == nil || err == flag.ErrHelp {
		return err
	}
	return usageError{err: err, shown: true}
}

// newFlagSet returns a FlagSet that reports errors instead of exiting, so
// dispatch owns the exit code.
func newFlagSet(name string, e env) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(e.stderr)
	return fs
}
