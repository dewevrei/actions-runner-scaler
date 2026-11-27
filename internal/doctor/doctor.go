// Package doctor checks a configuration file and what it depends on (the
// key and token files, GitHub, Docker and the images) without creating,
// changing or deleting anything (0003 SPEC R296~R304, R317).
package doctor

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/dewevrei/actions-runner-scaler/internal/config"
	"github.com/dewevrei/actions-runner-scaler/internal/docker"
	"github.com/dewevrei/actions-runner-scaler/internal/github"
)

// DefaultTimeout bounds each GitHub and Docker call (0003 SPEC R302).
const DefaultTimeout = 10 * time.Second

// Outcome is the result word of one check (0003 SPEC R296).
type Outcome int

// The outcomes; only Fail makes doctor exit 1 (0003 SPEC R298).
const (
	Pass Outcome = iota
	Fail
	Unknown
)

// String returns the word printed for o.
func (o Outcome) String() string {
	switch o {
	case Pass:
		return "통과"
	case Fail:
		return "실패"
	default:
		return "확인 못 함"
	}
}

// Result is one check's line and the lines under it.
type Result struct {
	Outcome Outcome
	Name    string
	Summary string
	// Causes and Fix are printed for a result that did not pass; every
	// such result has at least one cause and a fix.
	Causes []string
	Fix    string
	// Warnings are printed whatever the outcome and change no count
	// (0003 SPEC R317).
	Warnings []string
}

// Report is the outcome of Run, in the order of the SPEC's check table.
type Report struct {
	Results []Result
	// redact is applied to every printed line, after Run has registered
	// the token values it read.
	redact func(string) string
}

// AppProbe looks up the App's installation on a repository (0002 SPEC
// R236). *github.App implements it.
type AppProbe interface {
	Installation(ctx context.Context, t github.Target) (github.Installation, error)
}

// TokenProbe makes the token checks of 0003 SPEC R301. *github.REST made
// by github.NewTokenREST implements it.
type TokenProbe interface {
	CheckRepositoryAccess(ctx context.Context, t github.Target) (github.Access, error)
	ActionsPoliciesStatus(ctx context.Context, t github.Target) (int, error)
}

// TokenClient is the probe of one token and the token's kind.
type TokenClient struct {
	Probe TokenProbe
	// FineGrained is true for a github_pat_ token; every other token is
	// treated as classic (0003 SPEC Design 자격).
	FineGrained bool
}

// GitHubClients makes the clients of the credentials whose files passed
// their checks. The files are read again here, so the values never pass
// through the configuration check.
type GitHubClients interface {
	App(clientID, keyPath string) (AppProbe, error)
	Token(path string) (TokenClient, error)
}

// DockerProbe is the part of the Docker client doctor calls (0003 SPEC
// R299, R323). *docker.Client implements it.
type DockerProbe interface {
	Ping(ctx context.Context) error
	Info(ctx context.Context) (docker.Capacity, error)
	ImageInspect(ctx context.Context, ref string) (docker.Image, bool, error)
	// DistributionInspect reads a registry's answer for an image reference
	// that is not local (R323). There is no pull method: doctor changes
	// nothing.
	DistributionInspect(ctx context.Context, ref string) (docker.Distribution, error)
}

// Deps are what Run checks with.
type Deps struct {
	// ConfigPath is the configuration file to check.
	ConfigPath string
	// Inspect reads the configuration; nil means config.Inspect.
	Inspect func(path string) (*config.Inspection, error)
	// GitHub makes the GitHub clients. Required.
	GitHub GitHubClients
	// Docker makes the Docker client for host: the configuration's
	// docker.host, or config.DefaultDockerHost when the file cannot be
	// read. It is not called when docker.host is refused. Run closes a
	// client that is an io.Closer. Required.
	Docker func(host string) (DockerProbe, error)
	// Exposure is the R79 check; it must return once ctx ends. Required.
	Exposure func(ctx context.Context) docker.Exposure
	// Now reads the clock; nil means time.Now.
	Now func() time.Time
	// Redact hides the registered secrets in a line. Required: it must be
	// the Redact of the set GitHub registers every token it reads in, so
	// no other set can stand in for it (0003 SPEC R277).
	Redact func(string) string
	// Timeout bounds each call; zero means DefaultTimeout.
	Timeout time.Duration
}

// checker runs the checks of one Run.
type checker struct {
	Deps
	// started is the run time R317's warning is measured from.
	started time.Time
	// host is the Docker endpoint checked, empty when docker.host is
	// refused; docker is its client.
	host   string
	docker DockerProbe
}

// Run makes every check, in the SPEC's table order, and returns them all:
// a check that fails does not stop the others (0003 SPEC R297). It returns
// an error, before any check, only when a required Deps field is nil.
func Run(ctx context.Context, d Deps) (Report, error) {
	switch {
	case d.Redact == nil:
		return Report{}, errors.New("doctor: nil Redact")
	case d.GitHub == nil:
		return Report{}, errors.New("doctor: nil GitHub")
	case d.Docker == nil:
		return Report{}, errors.New("doctor: nil Docker")
	case d.Exposure == nil:
		return Report{}, errors.New("doctor: nil Exposure")
	}
	if d.Inspect == nil {
		d.Inspect = config.Inspect
	}
	if d.Now == nil {
		d.Now = time.Now
	}
	if d.Timeout <= 0 {
		d.Timeout = DefaultTimeout
	}
	c := &checker{Deps: d, started: d.Now()}

	cfgResult, in := c.config()
	results := []Result{cfgResult}
	files := c.files(in)
	results = append(results, files.results...)
	results = append(results, c.targets(ctx, in, files)...)
	if closer := c.connectDocker(in); closer != nil {
		defer closer.Close()
	}
	ping := c.ping(ctx)
	results = append(results, ping, c.exposure(ctx))
	results = append(results, c.images(ctx, in, ping)...)
	results = append(results, c.serverLimit(ctx, in, ping))
	return Report{Results: results, redact: d.Redact}, nil
}

// connectDocker sets the Docker endpoint the checks use and makes its
// client: docker.host, or the default when the file could not be read.
// When docker.host is refused, nothing is made and the Docker line is
// blocked by 설정. A client that cannot be made answers every call with
// that error. It returns the client to close, nil for none.
func (c *checker) connectDocker(in *config.Inspection) io.Closer {
	c.host = config.DefaultDockerHost
	if in != nil {
		if in.Failed("docker.host") {
			c.host = ""
			return nil
		}
		c.host = in.Config.Docker.Host
	}
	probe, err := c.Docker(c.host)
	if err != nil {
		c.docker = brokenDocker{err: fmt.Errorf("docker client: %w", err)}
		return nil
	}
	c.docker = probe
	closer, _ := probe.(io.Closer)
	return closer
}

// brokenDocker answers every call with the error that kept the Docker
// client from being made, so the Docker lines still print.
type brokenDocker struct{ err error }

func (b brokenDocker) Ping(context.Context) error { return b.err }

func (b brokenDocker) Info(context.Context) (docker.Capacity, error) {
	return docker.Capacity{}, b.err
}

func (b brokenDocker) ImageInspect(context.Context, string) (docker.Image, bool, error) {
	return docker.Image{}, false, b.err
}

func (b brokenDocker) DistributionInspect(context.Context, string) (docker.Distribution, error) {
	return docker.Distribution{}, b.err
}

// withTimeout runs fn under the per-call deadline.
func (c *checker) withTimeout(ctx context.Context, fn func(context.Context)) {
	ctx, cancel := context.WithTimeout(ctx, c.Timeout)
	defer cancel()
	fn(ctx)
}

// parallel runs each of n checks in its own goroutine and returns their
// results in order. The GitHub checks of different targets wait on the
// network independently, so one slow target does not delay the rest.
func parallel(n int, check func(i int) Result) []Result {
	results := make([]Result, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() { results[i] = check(i) })
	}
	wg.Wait()
	return results
}
