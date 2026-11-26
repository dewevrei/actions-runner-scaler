package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/dewevrei/actions-runner-scaler/internal/config"
	"github.com/dewevrei/actions-runner-scaler/internal/configfile"
)

// Messages of add-repo and remove-repo (0003 SPEC R292, R294, R295).
const (
	repoEditRestart = "스케일러를 다시 시작해야 반영됨"
	// repoEditScaleSetLeft follows a removal: the command changes only the
	// file, so the repository's scale set stays on GitHub.
	repoEditScaleSetLeft = "그 저장소의 scale set은 GitHub에 남음. 지우려면 그 저장소를 다시 넣고, 스케일러를 멈춘 뒤 delete-scaleset을 실행"
	repoEditProblems     = "config still has problems:"
	repoEditPaste        = "add this block to the config file by hand:"
)

// repoEditDeps is what add-repo and remove-repo take from the machine, so a
// test can watch their order and make a step fail. It has no GitHub or
// Docker client, and the scaler's own lock is not among them (0003 SPEC
// R295).
type repoEditDeps struct {
	// Lock takes the config file's lock, writing its notice to notify
	// (0003 SPEC R315).
	Lock     func(configPath string, notify io.Writer) (unlock func(), err error)
	ReadFile func(path string) ([]byte, error)
	// Replace puts data in place of the file at path atomically (R293).
	Replace func(path string, data []byte) error
	// LoadConfig reads and validates the edited file, to list the problems
	// it still has (R295).
	LoadConfig func(path string) error
}

func newRepoEditDeps() repoEditDeps {
	return repoEditDeps{
		Lock:     configfile.Lock,
		ReadFile: os.ReadFile,
		Replace:  configfile.Replace,
		LoadConfig: func(path string) error {
			_, err := config.Load(path)
			return err
		},
	}
}

// repoEdit is one run of add-repo or remove-repo whose arguments passed.
type repoEdit struct {
	command    string // names the command in messages
	configPath string
	url        string // shown on success, as given
	// apply returns the edited content of the config file.
	apply func(data []byte) ([]byte, error)
	// paste is the block to show with a shape refusal; empty for none.
	paste string
	// after holds the lines to print after the restart line.
	after []string
}

// addRepoCmd adds a [[targets]] entry to the configuration file (0003 SPEC
// R290).
func addRepoCmd(_ context.Context, e env, args []string) error {
	return addRepo(e, newRepoEditDeps(), args)
}

// removeRepoCmd removes a [[targets]] entry from the configuration file
// (0003 SPEC R291).
func removeRepoCmd(_ context.Context, e env, args []string) error {
	return removeRepo(e, newRepoEditDeps(), args)
}

func addRepo(e env, deps repoEditDeps, args []string) error {
	const usage = "usage: add-repo [-config path] [-max-runners n] [-idle n] [-token-file path] <repository URL>"
	fs := newFlagSet("add-repo", e)
	configPath := fs.String("config", defaultConfigPath(), "path to the TOML config file")
	// The numbers are read as text: the flag package quotes a value it
	// cannot parse, and no argument is echoed (0003 SPEC R277).
	maxRunners := fs.String("max-runners", "", "max-runners of the new target (default 1)")
	idle := fs.String("idle", "", "idle of the new target (default: the key is left out)")
	tokenFile := fs.String("token-file", "", "absolute path of the token file the new target uses in place of the default credential")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return badUsage(usage)
	}
	given := repoEditGivenFlags(fs)
	entry, err := addRepoArgs{
		url:        fs.Arg(0),
		maxRunners: given("max-runners", maxRunners),
		idle:       given("idle", idle),
		tokenFile:  given("token-file", tokenFile),
	}.entry()
	if err != nil {
		return usageError{err: fmt.Errorf("add-repo: %w", err)}
	}
	return runRepoEdit(e, deps, repoEdit{
		command:    "add-repo",
		configPath: *configPath,
		url:        fs.Arg(0),
		apply:      func(data []byte) ([]byte, error) { return configfile.AddTarget(data, entry) },
		paste:      configfile.TargetBlock(entry),
	})
}

func removeRepo(e env, deps repoEditDeps, args []string) error {
	fs := newFlagSet("remove-repo", e)
	configPath := fs.String("config", defaultConfigPath(), "path to the TOML config file")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return badUsage("usage: remove-repo [-config path] <repository URL>")
	}
	url := fs.Arg(0)
	if err := config.ValidateRepositoryURL(url); err != nil {
		return usageError{err: fmt.Errorf("remove-repo: %w", err)}
	}
	key, _ := config.RepositoryKey(url)
	return runRepoEdit(e, deps, repoEdit{
		command:    "remove-repo",
		configPath: *configPath,
		url:        url,
		apply:      func(data []byte) ([]byte, error) { return configfile.RemoveTarget(data, key) },
		after:      []string{repoEditScaleSetLeft},
	})
}

// repoEditGivenFlags returns a function that gives a flag's value when the
// command line set the flag and nil otherwise.
func repoEditGivenFlags(fs *flag.FlagSet) func(name string, value *string) *string {
	set := make(map[string]bool)
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })
	return func(name string, value *string) *string {
		if set[name] {
			return value
		}
		return nil
	}
}

// addRepoArgs holds the arguments of add-repo. A nil flag was not given.
type addRepoArgs struct {
	url                         string
	maxRunners, idle, tokenFile *string
}

// entry checks the arguments in the order of 0003 SPEC R292 (the URL with
// R314 first, max-runners, idle, the token file with R276 first) and returns
// the values of the block. An error quotes no argument that can hold a
// credential. The token file is read only to check it, and the token is
// dropped at once.
func (a addRepoArgs) entry() (configfile.TargetEntry, error) {
	if err := config.ValidateRepositoryURL(a.url); err != nil {
		return configfile.TargetEntry{}, err
	}
	entry := configfile.TargetEntry{URL: a.url, MaxRunners: config.DefaultMaxRunners}
	if a.maxRunners != nil {
		n, err := strconv.Atoi(*a.maxRunners)
		if err != nil || n < 1 {
			return configfile.TargetEntry{}, errors.New("-max-runners must be an integer of at least 1")
		}
		entry.MaxRunners = n
	}
	if a.idle != nil {
		n, err := strconv.Atoi(*a.idle)
		if err != nil || n < 0 || n > entry.MaxRunners {
			return configfile.TargetEntry{}, fmt.Errorf("-idle must be an integer from 0 to max-runners (%d)", entry.MaxRunners)
		}
		entry.Idle = &n
	}
	if a.tokenFile != nil {
		if _, err := config.ReadTokenFile(*a.tokenFile); err != nil {
			return configfile.TargetEntry{}, fmt.Errorf("-token-file: %w", err)
		}
		// TOML cannot hold invalid UTF-8, and TargetBlock would change it.
		if !utf8.ValidString(*a.tokenFile) {
			return configfile.TargetEntry{}, errors.New("-token-file: path must be valid UTF-8")
		}
		entry.TokenFile = *a.tokenFile
	}
	return entry, nil
}

// runRepoEdit locks the config file, edits it and prints the outcome (0003
// SPEC R292~R295, R315). Any refusal leaves the file as it was.
func runRepoEdit(e env, deps repoEditDeps, r repoEdit) error {
	// The link is resolved once, so a link pointed elsewhere between the
	// steps cannot make the lock and the file belong to different files.
	// Replace keeps working on the resolved file, which leaves the link as
	// it is (R293).
	path, err := filepath.EvalSymlinks(r.configPath)
	if err != nil {
		return repoEditFail(e, r.command, fmt.Errorf("config file: %w", err))
	}
	unlock, err := deps.Lock(path, e.stderr)
	if err != nil {
		return repoEditFail(e, r.command, err)
	}
	// The lock is released right after the replace, as R315 asks, and on
	// every other path when the function returns.
	var once sync.Once
	release := func() { once.Do(unlock) }
	defer release()

	data, err := deps.ReadFile(path)
	if err != nil {
		return repoEditFail(e, r.command, err)
	}
	edited, err := r.apply(data)
	if err != nil {
		if errors.Is(err, configfile.ErrShape) && r.paste != "" {
			// The arguments passed their checks, so the block holds only
			// their values; the token file is a path, never a token.
			return repoEditFail(e, r.command, err, repoEditPaste, strings.TrimSuffix(r.paste, "\n"))
		}
		return repoEditFail(e, r.command, err)
	}
	err = deps.Replace(path, edited)
	release()
	if err != nil {
		return repoEditFail(e, r.command, err)
	}

	fmt.Fprintln(e.stdout, r.url)
	fmt.Fprintln(e.stdout, repoEditRestart)
	for _, line := range r.after {
		fmt.Fprintln(e.stdout, line)
	}
	if err := deps.LoadConfig(path); err != nil {
		fmt.Fprintln(e.stdout, repoEditProblems)
		for _, line := range strings.Split(err.Error(), "\n") {
			fmt.Fprintf(e.stdout, "  %s\n", line)
		}
	}
	return nil
}

// repoEditFail writes err, then the lines, on stderr as plain text and
// returns what makes dispatch exit 1 without logging it again.
func repoEditFail(e env, command string, err error, lines ...string) error {
	fmt.Fprintf(e.stderr, "%s: %v\n", command, err)
	for _, line := range lines {
		fmt.Fprintln(e.stderr, line)
	}
	return reportedError{err: err}
}
