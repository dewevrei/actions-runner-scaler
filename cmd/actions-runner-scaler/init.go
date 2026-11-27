package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/dewevrei/actions-runner-scaler/internal/config"
	"github.com/dewevrei/actions-runner-scaler/internal/configfile"
	"github.com/dewevrei/actions-runner-scaler/internal/docker"
)

const initUsage = "usage: init [-config path] [-app-client-id id -app-private-key-file path | -token-file path] [-repo URL[=max-runners]]... [-runner-image ref] [-buildkit-image ref]"

// initImageTimeout bounds one image lookup, so an unresponsive Docker does
// not hold the command.
const initImageTimeout = 10 * time.Second

// Lines init prints on stdout about the images it fills (0003 SPEC R286,
// R322).
const (
	msgInitDockerDown     = "could not check the Docker image: %v\n"
	msgInitRunnerFilled   = "runner.image: %s (current ID %s)\n"
	msgInitRunnerMissing  = "image %s is not present locally, so runner.image was not filled: run image/build.sh first\n"
	msgInitBuildKitFilled = "buildkit.image: %s (used when buildkit.enabled is true)\n"
)

// initRepoFlags collects the values of -repo. Set only appends: when it
// returns an error, flag prints the value with it, and a value with a
// credential must not be printed (0003 SPEC R314). initCmd checks the values
// after parsing.
type initRepoFlags []string

func (r *initRepoFlags) String() string { return "" }

func (r *initRepoFlags) Set(value string) error {
	*r = append(*r, value)
	return nil
}

// initOptions are the command line of init.
type initOptions struct {
	configPath string
	// values holds what the flags gave, other than -repo.
	values configfile.Values
	// repos are the raw -repo values.
	repos []string
}

// initImageInspector is the part of *docker.Client that init uses: image
// inspection only, so init cannot pull (0003 SPEC R289, R322).
type initImageInspector interface {
	ImageInspect(ctx context.Context, ref string) (docker.Image, bool, error)
}

var _ initImageInspector = (*docker.Client)(nil)

// initDeps is what init takes from the machine, so tests run it on fakes.
type initDeps struct {
	// Images connects to Docker for the image lookups of R286. init calls it
	// only when an image value is missing, and closes the result if it is an
	// io.Closer.
	Images func() (initImageInspector, error)
	// Stdin holds the answers; init reads it only when Terminal is true.
	Stdin    io.Reader
	Terminal bool
}

// newInitDeps wires the real Docker and standard input.
func newInitDeps() initDeps {
	return initDeps{
		Images: func() (initImageInspector, error) {
			engine, err := docker.NewEngine(docker.DefaultHost)
			if err != nil {
				return nil, fmt.Errorf("docker client: %w", err)
			}
			return initDockerImages{client: docker.New(engine), engine: engine}, nil
		},
		Stdin:    os.Stdin,
		Terminal: isTerminal(int(os.Stdin.Fd())),
	}
}

// initDockerImages gives a docker.Client's image inspection and nothing
// else, and closes the Engine client the Client was made from. It does not
// embed the Client, so none of its other methods, a pull among them, can be
// reached through the interface (0003 SPEC R289).
type initDockerImages struct {
	client *docker.Client
	engine io.Closer
}

func (d initDockerImages) ImageInspect(ctx context.Context, ref string) (docker.Image, bool, error) {
	return d.client.ImageInspect(ctx, ref)
}

func (d initDockerImages) Close() error { return d.engine.Close() }

// initCmd writes a new configuration file (0003 SPEC R282~R289).
func initCmd(ctx context.Context, e env, args []string) error {
	return runInitCmd(ctx, e, newInitDeps(), args)
}

func runInitCmd(ctx context.Context, e env, d initDeps, args []string) error {
	opts, err := parseInitFlags(e, args)
	if err != nil {
		return err
	}
	return runInit(ctx, e, d, opts)
}

func parseInitFlags(e env, args []string) (initOptions, error) {
	fs := newFlagSet("init", e)
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), initUsage)
		fs.PrintDefaults()
	}
	var o initOptions
	var repos initRepoFlags
	fs.StringVar(&o.configPath, "config", defaultConfigPath(), "path of the new TOML config file")
	fs.StringVar(&o.values.AppClientID, "app-client-id", "", "GitHub App client ID")
	fs.StringVar(&o.values.AppPrivateKeyFile, "app-private-key-file", "", "absolute path of the GitHub App private key file")
	fs.StringVar(&o.values.TokenFile, "token-file", "", "absolute path of a personal access token file, instead of an App")
	fs.Var(&repos, "repo", "repository `URL`[=max-runners] (max-runners defaults to 1); repeat for more repositories")
	fs.StringVar(&o.values.RunnerImage, "runner-image", "", "runner.image: an image reference <name>[:<tag>][@<digest>] or a local image ID")
	fs.StringVar(&o.values.BuildKitImage, "buildkit-image", "", "buildkit.image (default: the image pinned in image/build.sh)")
	if err := parseFlags(fs, args); err != nil {
		return initOptions{}, err
	}
	if fs.NArg() != 0 || o.configPath == "" {
		return initOptions{}, badUsage(initUsage)
	}
	o.repos = repos
	return o, nil
}

// runInit follows 0003 SPEC Design init: check the flags (R284, R288), fill
// the images (R286), ask for or name what is missing (R285, R287), then
// render, validate, make the parent directory and place the file (R282,
// R283). Nothing is made before the last two steps. Every failure is written
// on stderr and exits 1.
func runInit(ctx context.Context, e env, d initDeps, o initOptions) error {
	if err := initCheckAbsent(o.configPath); err != nil {
		return initFail(e, err)
	}
	v := o.values
	targets, problems := checkInitFlags(o)
	if len(problems) > 0 {
		return initFail(e, problems...)
	}
	v.Targets = targets

	fillInitImages(ctx, e, d, &v)
	if gaps := configfile.Gaps(v); len(gaps) > 0 {
		if !d.Terminal {
			errs := make([]error, len(gaps))
			for i, g := range gaps {
				errs[i] = fmt.Errorf("missing %s: set %s", g.What, g.Flag)
			}
			return initFail(e, errs...)
		}
		if err := configfile.NewPrompter(d.Stdin, e.stdout).Fill(&v); err != nil {
			if errors.Is(err, io.EOF) {
				err = errors.New("input ended before every value was given; nothing written")
			}
			return initFail(e, err)
		}
	}
	return initCreate(e, o.configPath, v)
}

// initFail writes each of errs on its own line on stderr and returns the
// error that makes the command exit 1 without a second report.
func initFail(e env, errs ...error) error {
	for _, err := range errs {
		fmt.Fprintln(e.stderr, err)
	}
	return reportedError{err: errors.Join(errs...)}
}

// initExistsError is R282's text for a path that already holds a file.
func initExistsError(path string) error {
	return fmt.Errorf("config file %s already exists; edit it, or use add-repo and remove-repo", path)
}

// initCheckAbsent returns R282's error when path exists, a dangling link
// included. CreateNew makes the same refusal again at the end, for a file
// that appears meanwhile.
func initCheckAbsent(path string) error {
	_, err := os.Lstat(path)
	switch {
	case err == nil:
		return initExistsError(path)
	case errors.Is(err, os.ErrNotExist):
		return nil
	default:
		return err
	}
}

// checkInitFlags returns the targets of the -repo values and, in the order
// of the flags, what is wrong with the values the flags gave (0003 SPEC
// R284, R288). No error quotes a value that can hold a secret.
func checkInitFlags(o initOptions) (targets []configfile.TargetEntry, problems []error) {
	for i, raw := range o.repos {
		t, err := parseInitRepo(targets, raw)
		if err != nil {
			problems = append(problems, fmt.Errorf("-repo #%d: %w", i+1, err))
			continue
		}
		targets = append(targets, t)
	}

	v := o.values
	if v.TokenFile != "" && (v.AppClientID != "" || v.AppPrivateKeyFile != "") {
		problems = append(problems, errors.New("-token-file cannot be combined with -app-client-id or -app-private-key-file"))
	}
	for _, c := range []struct {
		flag, value string
		check       func(string) error
	}{
		{"-app-client-id", v.AppClientID, configfile.CheckAppClientID},
		{"-app-private-key-file", v.AppPrivateKeyFile, configfile.CheckAppKeyPath},
		{"-token-file", v.TokenFile, configfile.CheckTokenPath},
		{"-runner-image", v.RunnerImage, configfile.CheckRunnerImage},
		{"-buildkit-image", v.BuildKitImage, configfile.CheckBuildKitImage},
	} {
		if c.value == "" {
			continue
		}
		if err := c.check(c.value); err != nil {
			problems = append(problems, fmt.Errorf("%s: %w", c.flag, err))
		}
	}
	return targets, problems
}

// parseInitRepo splits a -repo value at its first "=" into the URL and
// max-runners and checks both: the whole value first for a credential (0003
// SPEC R314; a split first would quote the part before an "=" inside the
// credential), then the rules of targets.url against the targets before it,
// then max-runners (R284).
func parseInitRepo(taken []configfile.TargetEntry, raw string) (configfile.TargetEntry, error) {
	if config.ContainsCredential(raw) {
		// ValidateRepositoryURL gives R314's text, which quotes nothing.
		return configfile.TargetEntry{}, config.ValidateRepositoryURL(raw)
	}
	url, n, hasN := strings.Cut(raw, "=")
	if err := configfile.CheckRepository(taken, url); err != nil {
		return configfile.TargetEntry{}, err
	}
	runners := config.DefaultMaxRunners
	if hasN {
		var err error
		if runners, err = configfile.ParseMaxRunners(n); err != nil {
			return configfile.TargetEntry{}, err
		}
	}
	return configfile.TargetEntry{URL: url, MaxRunners: runners}, nil
}

// fillInitImages sets the image values the flags left out, runner.image from
// Docker and buildkit.image to the pinned BuildKit image, and says on stdout
// what it set and why it did not (0003 SPEC R286). A Docker failure leaves
// runner.image empty. The file init writes has buildkit.enabled false, so
// the BuildKit image is not looked up.
func fillInitImages(ctx context.Context, e env, d initDeps, v *configfile.Values) {
	if v.RunnerImage == "" {
		v.RunnerImage = initRunnerImage(ctx, e, d)
	}
	if v.BuildKitImage == "" {
		v.BuildKitImage = docker.BuildKitImage
		fmt.Fprintf(e.stdout, msgInitBuildKitFilled, v.BuildKitImage)
	}
}

// initRunnerImage connects to Docker and returns the runner image tag when
// that image exists locally, or "" after saying why not.
func initRunnerImage(ctx context.Context, e env, d initDeps) string {
	f := &initImageFill{ctx: ctx, out: e.stdout}
	images, err := d.Images()
	if err != nil {
		f.down = true
		fmt.Fprintf(f.out, msgInitDockerDown, err)
	} else {
		f.images = images
		if c, ok := images.(io.Closer); ok {
			defer c.Close()
		}
	}
	return f.runnerImage()
}

// initImageFill looks images up for fillInitImages.
type initImageFill struct {
	ctx    context.Context
	out    io.Writer
	images initImageInspector
	// down is set when Docker did not answer, so no later lookup tries.
	down bool
}

// lookup returns the local image ref. Docker's failure is said once and
// ends the lookups.
func (f *initImageFill) lookup(ref string) (docker.Image, bool) {
	if f.down {
		return docker.Image{}, false
	}
	ctx, cancel := context.WithTimeout(f.ctx, initImageTimeout)
	defer cancel()
	img, ok, err := f.images.ImageInspect(ctx, ref)
	if err != nil {
		f.down = true
		fmt.Fprintf(f.out, msgInitDockerDown, err)
		return docker.Image{}, false
	}
	return img, ok
}

// runnerImage returns the runner image tag when that image exists locally,
// or "" after saying why not. The value is the tag, not
// the ID, so a later image/build.sh run needs no configuration change; the
// line it prints shows the tag's ID now (0003 SPEC R322).
func (f *initImageFill) runnerImage() string {
	img, ok := f.lookup(docker.RunnerImageTag)
	switch {
	case f.down:
	case !ok:
		fmt.Fprintf(f.out, msgInitRunnerMissing, docker.RunnerImageTag)
	default:
		fmt.Fprintf(f.out, msgInitRunnerFilled, docker.RunnerImageTag, img.ID)
		return docker.RunnerImageTag
	}
	return ""
}

// initCreate renders v, checks the result as config.Load would, makes the
// parent directory if it is missing and places the file (0003 SPEC R282,
// R283).
func initCreate(e env, path string, v configfile.Values) error {
	data := configfile.Render(v)
	if _, err := config.Decode(data); err != nil {
		return initFail(e, err)
	}
	if err := initMakeParent(path); err != nil {
		return initFail(e, err)
	}
	if err := configfile.CreateNew(path, data); err != nil {
		if errors.Is(err, configfile.ErrExists) {
			err = initExistsError(path)
		}
		return initFail(e, err)
	}
	fmt.Fprintf(e.stdout, "config file created: %s\n", path)
	fmt.Fprintf(e.stdout, "next: actions-runner-scaler doctor -config %s\n", path)
	return nil
}

// initMakeParent makes the directory that will hold path, and any missing
// directory above it, with mode 0700, as go-gh and runner-up do for their
// config files (0003 SPEC R283, R289).
func initMakeParent(path string) error {
	return os.MkdirAll(filepath.Dir(path), 0o700)
}
