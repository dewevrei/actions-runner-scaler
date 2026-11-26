package main

import (
	"context"
	"fmt"
	"net/http"

	"github.com/dewevrei/actions-runner-scaler/internal/docker"
	"github.com/dewevrei/actions-runner-scaler/internal/doctor"
	"github.com/dewevrei/actions-runner-scaler/internal/github"
	"github.com/dewevrei/actions-runner-scaler/internal/scaler"
)

// doctorCmd checks the configuration and what it depends on, and exits 1
// only when a check failed (0003 SPEC R296~R304, R317). It takes neither
// the scaler lock nor the status socket and writes no file (R299).
func doctorCmd(ctx context.Context, e env, args []string) error {
	fs := newFlagSet("doctor", e)
	configPath := fs.String("config", defaultConfigPath(), "path to the TOML config file")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return badUsage("usage: doctor [-config path]")
	}

	var probe doctor.DockerProbe
	engine, err := docker.NewEngine(docker.DefaultHost)
	if err != nil {
		probe = brokenDocker{err: fmt.Errorf("docker client: %w", err)}
	} else {
		defer engine.Close()
		probe = docker.New(engine)
	}
	return runDoctor(ctx, e, doctorDeps(e, *configPath, github.DefaultAPIBaseURL,
		&http.Client{Transport: github.NewTransport()}, probe))
}

// doctorDeps wires the real clients. Every token the GitHub side reads is
// registered in e.secrets, which redacts each printed line (0003 SPEC
// R277).
func doctorDeps(e env, configPath, baseURL string, httpClient github.HTTPDoer, probe doctor.DockerProbe) doctor.Deps {
	clock := scaler.SystemClock()
	return doctor.Deps{
		ConfigPath: configPath,
		GitHub: doctor.NewGitHubClients(doctor.GitHubConfig{
			BaseURL:    baseURL,
			HTTPClient: httpClient,
			Clock:      clock,
			Secrets:    e.secrets,
		}),
		Docker:   probe,
		Exposure: func(ctx context.Context) docker.Exposure { return docker.CheckExposure(ctx, nil) },
		Now:      clock.Now,
		Redact:   e.secrets.Redact,
	}
}

// runDoctor runs the checks and prints them; a 실패 line exits 1 (R298).
func runDoctor(ctx context.Context, e env, deps doctor.Deps) error {
	rep, err := doctor.Run(ctx, deps)
	if err != nil {
		return err
	}
	n, err := doctor.Print(e.stdout, rep)
	if err != nil {
		return fmt.Errorf("doctor: write report: %w", err)
	}
	if n > 0 {
		return reportedError{err: fmt.Errorf("doctor: %d checks failed", n)}
	}
	return nil
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
