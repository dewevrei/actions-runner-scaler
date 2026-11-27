package daemon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	mathrand "math/rand/v2"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/dewevrei/actions-runner-scaler/internal/config"
	"github.com/dewevrei/actions-runner-scaler/internal/docker"
	"github.com/dewevrei/actions-runner-scaler/internal/gate"
	"github.com/dewevrei/actions-runner-scaler/internal/github"
	"github.com/dewevrei/actions-runner-scaler/internal/retry"
	"github.com/dewevrei/actions-runner-scaler/internal/scaler"
	"github.com/dewevrei/actions-runner-scaler/internal/status"
)

// lockFileName is the lock's file name, next to the status socket (SPEC
// "외부 계약").
const lockFileName = "lock"

// DefaultLockPath returns ~/.local/state/actions-runner-scaler/lock.
func DefaultLockPath() (string, error) {
	sock, err := status.DefaultSocketPath()
	if err != nil {
		return "", err
	}
	return filepath.Join(filepath.Dir(sock), lockFileName), nil
}

// ErrLocked means another process holds the lock (R78).
var ErrLocked = errors.New("daemon: another scaler holds the lock")

// LockFile takes an exclusive flock on path without waiting, creating the
// file (0600) and its directory (0700) when missing. It returns ErrLocked
// when another process holds the lock. unlock releases it; the kernel also
// releases it when the process ends, so a crash never leaves it held.
func LockFile(path string) (unlock func(), err error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("daemon: lock directory: %w", err)
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("daemon: open lock file: %w", err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, ErrLocked
		}
		return nil, fmt.Errorf("daemon: lock: %w", err)
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
	}, nil
}

// waitError means start-up must enter the error wait (R46). retry marks a
// Docker-side cause, which retryStep tries again; any other waitError is
// a configuration error, recorded for status already, which waits for a
// restart.
type waitError struct {
	where string
	err   error
	retry bool
}

func (e *waitError) Error() string {
	return status.WithWhere(e.where, e.err.Error())
}
func (e *waitError) Unwrap() error { return e.err }

// toErrorWait records err for status, logs it, and returns the waitError
// that sends start-up to the error wait until a restart: a configuration
// error, which nothing watches the file for.
func (d *Daemon) toErrorWait(where string, err error) error {
	d.rec.Record(where, err)
	d.log.Error("start-up failed permanently; entering the error wait", "step", where, "err", err)
	return &waitError{where: where, err: err}
}

// retryLater returns the waitError of a Docker-side cause (Docker not
// ready, R79's exposure, an image, the capacity check, the job check
// server listen), which retryStep tries again. retryStep logs and records
// it with the next retry time, so call it only inside a retryStep step.
func (d *Daemon) retryLater(where string, err error) error {
	return &waitError{where: where, err: err, retry: true}
}

// retryStep runs one start-up step until it succeeds, ends without a
// Docker-side cause, or ctx ends. After a Docker-side cause (retryLater)
// status shows the error wait with the error and the next retry time,
// and the step runs again once that time came: the first retry after
// errorRetryFirst, the interval doubled after each failure up to
// errorRetryMax, with no limit on the number of retries, as in a target's
// error wait (0002 SPEC R174). Nothing calls GitHub or Docker in between.
// ARC's listener exits for its pod to be made again and runscaler for
// launchd to restart it; retrying in the process keeps status answering.
func (d *Daemon) retryStep(ctx context.Context, step func() error) error {
	interval := errorRetryFirst
	for {
		err := step()
		var we *waitError
		if !errors.As(err, &we) || !we.retry {
			return err
		}
		// The error with its retry time is recorded and the timer set before
		// status shows the wait, as for a target's next retry (sleepUntil).
		at := d.clock.Now().Add(interval)
		d.rec.Record(we.where, fmt.Errorf("%w (next retry at %s)", we.err, at.Format(time.RFC3339)))
		fired, t := d.timer(interval)
		d.st.SetState(status.ErrorWait)
		d.log.Error("start-up failed; error wait until the next retry", "step", we.where, "err", we.err, "nextRetryAt", at)
		select {
		case <-ctx.Done():
			t.Stop()
			return ctx.Err()
		case <-fired:
		}
		// A signal at the same time as the timer runs no step.
		if err := ctx.Err(); err != nil {
			return err
		}
		// The retry time is past: the error stands without it, also once
		// the step succeeds.
		d.rec.Record(we.where, we.err)
		d.st.SetState(status.Starting)
		d.log.Info("start-up retry", "step", we.where)
		interval = min(2*interval, errorRetryMax)
	}
}

// callScope is whose waits and errors a start-up call's retries use: the
// server's (server steps, 0002 SPEC R176) or one target's (its start phase,
// R172, R265).
type callScope struct {
	limits *retry.NextAllowed
	log    *slog.Logger
	// record records a repeated error as the last error.
	record func(where string, err error)
	// fail records a permanent error and returns the waitError of the
	// scope's error wait.
	fail func(where string, err error) error
}

// authWaits returns the waits the targets' credential checks use (0002
// SPEC R189, 0003 SPEC R280): the default App's installation lookups keep
// the server's wait, and every other App gets one of its own in restWaits
// (authWaits.app).
func (d *Daemon) authWaits() authWaits {
	return authWaits{installation: d.limits, credentials: d.restWaits}
}

// serverScope is the scope of the server steps, all of them Docker calls,
// whose permanent errors retryStep tries again.
func (d *Daemon) serverScope() callScope {
	return callScope{limits: d.limits, log: d.log, record: d.rec.Record, fail: d.retryLater}
}

// errorOnce logs and records a repeated error only when it changes, as the
// status Recorder expects of its callers. Each retry loop owns one, so a
// success, which ends the loop, forgets it.
type errorOnce struct {
	last string
}

func (e *errorOnce) report(sc callScope, where string, class retry.Class, err error) {
	if text := err.Error(); text != e.last {
		e.last = text
		sc.log.Warn("start-up call failed; retrying", "step", where, "class", class, "err", err)
		sc.record(where, err)
	}
}

// retryStartup is retryIn for a server step.
func (d *Daemon) retryStartup(ctx context.Context, where string, op retry.Op, fn func(context.Context) error) error {
	return d.retryIn(ctx, d.serverScope(), where, op, fn)
}

// retryIn calls fn until it succeeds (R24): before each call it waits for
// op's allowed time in sc's limits (R52), and a transient or rate-limited
// failure waits the 1 to 60 s backoff, or the rate limit's wait when that is
// longer, at most 24 h (R104). A permanent failure returns sc.fail's
// waitError (R46, 0002 SPEC R172); ctx ending (a signal, R57) returns
// ctx.Err(). Each call gets callTimeout on both the wall clock and the Clock
// (withTimeout); a call that runs out is transient.
func (d *Daemon) retryIn(ctx context.Context, sc callScope, where string, op retry.Op, fn func(context.Context) error) error {
	return d.retryInFor(ctx, sc, where, op, callTimeout, fn)
}

// retryInFor is retryIn with timeout in place of callTimeout for each call,
// such as an image pull's pullTimeout (0003 SPEC R320). Whether a call that
// runs out is transient is for retry.Classify to say: a pull that runs out
// fails with retry.ErrPullFailed, which is permanent.
func (d *Daemon) retryInFor(ctx context.Context, sc callScope, where string, op retry.Op, timeout time.Duration, fn func(context.Context) error) error {
	backoff, err := retry.NewDefaultBackoff(d.clock, d.jitter())
	if err != nil {
		return err
	}
	var once errorOnce
	for {
		if err := d.waitAllowed(ctx, sc.limits, sc.log, op); err != nil {
			return err
		}
		// No call once ctx ended, also when a wait ended at the same time.
		if err := ctx.Err(); err != nil {
			return err
		}
		callCtx, cancel := withTimeout(ctx, d.clock, timeout)
		err := fn(callCtx)
		cancel()
		if err == nil {
			return nil
		}
		sc.limits.Record(op, err)
		if !hasOp(err) {
			err = &retry.OpError{Op: op, Err: err}
		}
		switch class := retry.Classify(ctx, op, err); class {
		case retry.Canceled:
			return ctx.Err()
		case retry.Permanent:
			return sc.fail(where, err)
		default:
			once.report(sc, where, class, err)
			if err := backoff.Wait(ctx, err); err != nil {
				return err
			}
		}
	}
}

// waitAllowed waits until op may be called by limits (R52).
func (d *Daemon) waitAllowed(ctx context.Context, limits *retry.NextAllowed, log *slog.Logger, op retry.Op) error {
	left := limits.At(op).Sub(d.clock.Now())
	if left <= 0 {
		return nil
	}
	log.Info("waiting for a rate limit to end", "op", op, "until", limits.At(op))
	return d.sleep(ctx, left)
}

func hasOp(err error) bool {
	var oe *retry.OpError
	return errors.As(err, &oe)
}

func (d *Daemon) jitter() func() float64 {
	if d.cfg.Jitter != nil {
		return d.cfg.Jitter
	}
	return mathrand.Float64
}

// start runs the server steps of the start order (PLAN M5-3, 0002 SPEC
// Design 시작 순서) up to the job check server listen and the GitHub side,
// then starts every target's start phase in its own goroutine (0002 PLAN
// M12-3). It returns nil once any target made its first session (R178) or
// every target ended its start phase in the target error wait (R195), a
// waitError for the server's error wait (R176) after a configuration error,
// or ctx.Err() after a signal (R57); waits select on the signal. A
// Docker-side cause waits and runs its step again (retryStep) instead.
func (d *Daemon) start(parent context.Context) error {
	// A signal ends start-up (R57); so does a fatal error, such as the job
	// check server dying while targets start, which then waits (R46).
	sigCtx, cancelSig := chanContext(parent, d.sig.first)
	defer cancelSig()
	ctx, cancelStart := chanContext(sigCtx, d.fatalCh)
	defer cancelStart()

	// 3. Configuration; an error is recorded and waits for R16.
	cfg, creds, cfgErr := d.cfg.LoadConfig()
	if cfgErr != nil {
		d.rec.Record("config", cfgErr)
		d.log.Error("configuration is invalid; leftover cleanup first, then the error wait (R28)", "err", cfgErr)
	}

	// 4. Docker, at docker.host; without a valid configuration, at the
	// default host, for R16.
	host := config.DefaultDockerHost
	if cfgErr == nil {
		host = cfg.Docker.Host
	}
	dc, err := d.cfg.NewDocker(host)
	if err != nil {
		return d.toErrorWait("docker client", err)
	}
	d.dockerClient = dc
	ping := d.serverScope()
	if cfgErr != nil {
		// The configuration error stays in status while Docker is retried,
		// since it is what the user has to fix once Docker answers.
		withConfig := func(where string, err error) (string, error) {
			return "config", fmt.Errorf("%v; %s: %w", cfgErr, where, err)
		}
		ping.record = func(where string, err error) { d.rec.Record(withConfig(where, err)) }
		ping.fail = func(where string, err error) error { return d.retryLater(withConfig(where, err)) }
	}
	if err := d.retryStep(ctx, func() error {
		return d.retryIn(ctx, ping, "docker ping", retry.OpDockerPing, dc.Ping)
	}); err != nil {
		return err
	}
	dk := dockerAdapter{d: dc}

	// 5. R16, whatever the configuration.
	cleanup, err := scaler.NewResourceCleanup(scaler.ResourceCleanupConfig{
		Resources: dk, Remover: dk, Clock: d.clock, Logger: d.log, Jitter: d.cfg.Jitter,
	})
	if err != nil {
		return err
	}
	if err := cleanup.Run(ctx); err != nil {
		return err
	}

	// 6. Configuration errors (R28, R30, R105~R107, R233). Recorded again: a
	// retried Docker call may have replaced the last error since step 3.
	if cfgErr != nil {
		return d.toErrorWait("config", cfgErr)
	}
	targets := make([]github.Target, len(cfg.Targets))
	for i, tc := range cfg.Targets {
		t, err := github.ParseTargetURL(tc.URL)
		if err != nil {
			return d.toErrorWait("config", fmt.Errorf("targets[%d].url: %w", i, err))
		}
		targets[i] = t
	}

	// 7. R79.
	if exposedHost(cfg.Docker.Host) {
		return d.toErrorWait("config", fmt.Errorf(
			"docker.host %s is the Docker API without TLS on TCP 2375, which R79 refuses; point docker.host at the Docker socket, such as %s",
			cfg.Docker.Host, config.DefaultDockerHost))
	}
	if err := d.retryStep(ctx, func() error { return d.checkExposure(ctx) }); err != nil {
		return err
	}

	// 8. Images, runner.image then buildkit.image; a missing reference is
	// pulled here, before any target starts (0003 SPEC R319~R321).
	d.images, err = newImageResolver(imageResolverConfig{
		Docker: dc, Clock: d.clock, Logger: d.log, Jitter: d.jitter(),
		Context: d.listenCtx, Runner: cfg.Runner.Image, BuildKit: cfg.BuildKit.Image,
	})
	if err != nil {
		return err
	}
	for _, k := range d.images.keys {
		if err := d.retryStep(ctx, func() error { return d.startupImage(ctx, d.images, k) }); err != nil {
			return err
		}
	}

	// 8b. Docker capacity and the server limit (0002 SPEC R201~R204, R167, R168).
	var limit int
	if err := d.retryStep(ctx, func() error {
		var err error
		limit, err = d.checkCapacity(ctx, cfg)
		return err
	}); err != nil {
		return err
	}
	d.st.SetServerLimit(limit)

	// 9. The shared objects and every target's scaler and judge; none of
	// it calls GitHub or Docker.
	if err := d.build(cfg, targets, limit); err != nil {
		return err
	}

	// 10. The job check server listens before any target's GitHub call,
	// since any target's first session may start runners; a listen failure
	// is the server's error wait (0002 SPEC R176) until a retry listens.
	if err := d.retryStep(ctx, func() error {
		if err := d.server.Listen(); err != nil {
			return d.retryLater("job check server", err)
		}
		return nil
	}); err != nil {
		return err
	}
	// Serve returns nil after Shutdown. Any other end leaves hooks without
	// verdicts, so no runner may start: an error drain (R51).
	go func() {
		if err := d.server.Serve(); err != nil {
			d.fatal(fmt.Errorf("job check server stopped: %w", err))
		}
	}()

	// 11. The GitHub side: one App signer for every App target, and each
	// target's credential (0003 SPEC R273).
	app, err := d.cfg.NewGitHub(cfg, creds)
	if err != nil {
		return d.toErrorWait("github clients", err)
	}
	for i, tr := range d.targets {
		if tr.auth, err = app.Auth(tr.t, i); err != nil {
			// Errors name the target's index already.
			return d.toErrorWait("github clients", err)
		}
	}

	// 12. Reconcile and placement kicks. Watches start with each runner.
	d.recon.Start()
	d.placer.Start()

	// 13. Every target's start phase (0002 SPEC R177).
	return d.startTargets(ctx, cfg.Scaling.Label)
}

// startTargets starts each target's goroutine and waits until any target
// made its first session (0002 SPEC R178), every target ended its start
// phase without one (R195), or ctx ends (R57). The start phases go on
// after it returns; they end when a drain or finalize begins (R196).
func (d *Daemon) startTargets(ctx context.Context, label string) error {
	d.startsEnded = make(chan struct{}, len(d.targets))
	runDone := make(chan struct{})
	d.runDone = runDone
	var wg sync.WaitGroup
	for _, tr := range d.targets {
		wg.Go(func() { tr.run(d.startCtx, label) })
	}
	go func() {
		wg.Wait()
		close(runDone)
	}()

	for left := len(d.targets); left > 0; {
		select {
		case <-d.firstSession:
			return nil
		case <-d.startsEnded:
			left--
		case <-ctx.Done():
			// A signal before any first session ends every start phase at
			// once (R57).
			d.cancelStarts()
			return ctx.Err()
		}
	}
	select {
	case <-d.firstSession:
		return nil
	default:
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	d.log.Warn("every target is in the target error wait; start-up finished (0002 SPEC R195)")
	return nil
}

// checkCapacity reads /info once, computes the server runner limit and
// checks the targets against it (0002 SPEC R201~R204, R167, R168; 0002 PLAN
// M10). It runs before any target's GitHub call. A positive
// scaling.server-limit is the limit in place of the computed one; /info is
// then read once for the log line only, and its failure does not stop
// start-up. The checks against a computed limit are Docker-side causes,
// which retryStep tries again; against a configured one they are
// configuration errors. The limit is not computed again while the scaler
// runs.
func (d *Daemon) checkCapacity(ctx context.Context, cfg *config.Config) (int, error) {
	share := docker.RunnerShare(docker.ContainerLimits(cfg.Runner), docker.ContainerLimits(cfg.BuildKit))
	fail := d.retryLater
	var capacity docker.Capacity
	var computed int
	if configured := cfg.Scaling.ServerLimit; configured > 0 {
		fail = func(_ string, err error) error { return d.toErrorWait("config", err) }
		callCtx, cancel := withTimeout(ctx, d.clock, callTimeout)
		c, err := d.dockerClient.Info(callCtx)
		cancel()
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		if err != nil {
			d.log.Warn("docker info failed; using scaling.server-limit", "limit", configured, "err", err)
		} else {
			capacity, computed = c, docker.ServerLimit(c, share)
		}
	} else {
		err := d.retryStartup(ctx, "docker info", retry.OpDockerInfo, func(ctx context.Context) error {
			var err error
			capacity, err = d.dockerClient.Info(ctx)
			return err
		})
		if err != nil {
			return 0, err
		}
		computed = docker.ServerLimit(capacity, share)
	}
	limit := cfg.Scaling.ServerLimitOr(computed)
	d.log.Info("server runner limit", "limit", limit, "computed", computed, "configured", cfg.Scaling.ServerLimit,
		"ncpu", capacity.NCPU, "memTotal", capacity.MemTotal,
		"runnerCPUs", float64(share.NanoCPUs)/1e9, "runnerMemory", share.Memory)
	if limit == 0 {
		return 0, fail("docker capacity", fmt.Errorf(
			"server runner limit is 0: one runner reserves %s CPUs (runner.cpus %s + buildkit.cpus %s) and %d bytes (runner.memory %s + buildkit.memory %s), Docker reports %d CPUs and %d bytes",
			formatCPUs(float64(share.NanoCPUs)/1e9), formatCPUs(cfg.Runner.CPUs), formatCPUs(cfg.BuildKit.CPUs),
			share.Memory, cfg.Runner.Memory, cfg.BuildKit.Memory, capacity.NCPU, capacity.MemTotal))
	}
	var idle, maxRunners int
	for _, t := range cfg.Targets {
		idle += t.Idle
		maxRunners += t.MaxRunners
	}
	if idle > limit {
		return 0, fail("docker capacity", fmt.Errorf(
			"idle of all targets (%d) exceeds the server runner limit (%d)", idle, limit))
	}
	if maxRunners > limit {
		d.log.Info("max-runners of all targets exceed the server limit", "sum", maxRunners, "limit", limit)
	}
	if len(cfg.Targets) >= 2 && idle == limit {
		for _, t := range cfg.Targets {
			if t.Idle == 0 {
				d.log.Warn("idle runners of other targets take every slot", "target", t.URL)
			}
		}
	}
	return limit, nil
}

// checkExposure is the R79 check: an exposed Docker API is a Docker-side
// cause, which retryStep tries again; an unknown answer goes on.
func (d *Daemon) checkExposure(ctx context.Context) error {
	ectx, cancel := withTimeout(ctx, d.clock, exposureTimeout)
	exposure := d.cfg.CheckExposure(ectx)
	cancel()
	if err := ctx.Err(); err != nil {
		return err
	}
	switch exposure {
	case docker.Exposed:
		return d.retryLater("docker exposure", errors.New("Docker API answers on 127.0.0.1:2375 without TLS; stop whatever opened the port (Docker Desktop setting, socat, or other) (R79)"))
	case docker.ExposureUnknown:
		d.log.Warn("could not tell whether the Docker daemon is exposed on TCP 2375; going on (R79)")
	}
	return nil
}

// exposedHost reports whether host is docker.ExposedAddr's port on the
// loopback address: then R79's check finds the endpoint the user configured,
// which no retry can change.
func exposedHost(host string) bool {
	u, err := url.Parse(host)
	if err != nil || u.Scheme != "tcp" {
		return false
	}
	_, port, err := net.SplitHostPort(docker.ExposedAddr)
	if err != nil || u.Port() != port {
		return false
	}
	switch u.Hostname() {
	case "127.0.0.1", "localhost", "::1":
		return true
	}
	return false
}

// formatCPUs prints a CPU count as configured: 3 and 0.5, not 3.000000.
func formatCPUs(cpus float64) string { return strconv.FormatFloat(cpus, 'g', -1, 64) }

// build makes the shared table with the server limit (0002 PLAN M11), the
// Docker adapter, each target's run with its judge and scaler, the one
// reconciler and the job check server (0002 PLAN M12-1). Nothing here calls
// GitHub or Docker; a target's scaler reaches its scale set only after the
// target's start phase bound it.
func (d *Daemon) build(cfg *config.Config, targets []github.Target, serverLimit int) error {
	jobCheckURL, err := docker.JobCheckURL(cfg.JobCheck.Listen)
	if err != nil {
		return d.toErrorWait("config", err)
	}
	// The image IDs come from each runner's lookup, never from the
	// configured values (0003 SPEC R319).
	d.docker = dockerAdapter{d: d.dockerClient, params: docker.Params{
		JobCheckURL: jobCheckURL,
		Runner:      docker.ContainerLimits(cfg.Runner),
		BuildKit:    docker.ContainerLimits(cfg.BuildKit),
	}}
	d.drainTimeout = cfg.Shutdown.DrainTimeout
	// A row leaving the table kicks a placement (0002 SPEC R170); no row
	// leaves before the placer below exists.
	var placer *scaler.Placer
	d.table = scaler.NewTable(scaler.TableConfig{ServerLimit: serverLimit, Secrets: d.cfg.Secrets,
		OnRemoved: func(string) { placer.Kick() }})
	// The placement's calls run on the listeners' parent context, which
	// finalize cancels (0002 PLAN M14).
	placer, err = scaler.NewPlacer(scaler.PlacerConfig{Table: d.table, Clock: d.clock, Context: d.listenCtx, Logger: d.log})
	if err != nil {
		return err
	}
	d.placer = placer
	// The end of a background pull kicks one placement (0003 SPEC R319,
	// 0002 SPEC R212); no runner creation runs before this.
	d.images.setOnPullDone(placer.Kick)

	recon := make(map[string]scaler.ReconcileTarget, len(targets))
	judges := make(map[string]*scaler.JobJudge, len(targets))
	for i, t := range targets {
		tcfg := cfg.Targets[i]
		tr, err := newTargetRun(d, tcfg, t)
		if err != nil {
			return err
		}
		judge := scaler.NewJobJudge()
		tr.scaler, err = scaler.New(scaler.Config{
			Target: scaler.TargetConfig{Key: t.Key(), URL: tcfg.URL, Label: cfg.Scaling.Label,
				MaxRunners: tcfg.MaxRunners, Idle: tcfg.Idle},
			Table:      d.table,
			Clock:      d.clock,
			ScaleSet:   targetScaleSet{tr: tr},
			Containers: d.docker,
			Images:     d.images,
			Judge:      judge,
			Tokens:     gate.NewTokenGenerator(nil),
			Limits:     tr.rateLimits(),
			Fatal:      tr.fatal,
			Secrets:    d.cfg.Secrets,
			Waiter:     d.docker,
			Logger:     d.log,
			Errors:     tr,
			Random:     d.cfg.Random,
			Jitter:     d.cfg.Jitter,
		})
		if err != nil {
			return err
		}
		if err := placer.AddTarget(tr.scaler); err != nil {
			return err
		}
		tr.judge = judge
		d.targets = append(d.targets, tr)
		recon[t.Key()] = scaler.ReconcileTarget{Scaler: tr.scaler, Finder: targetFinder{tr: tr}, Limits: tr.rateLimits()}
		judges[t.Key()] = judge
		d.st.AddTarget(tr) // 0002 PLAN M15
	}
	d.recon, err = scaler.NewReconciler(scaler.ReconcilerConfig{
		Targets:   recon,
		Resources: d.docker, Remover: d.docker, Processes: d.docker,
		OnRound: placer.Kick, // 0002 SPEC R212
	})
	if err != nil {
		return err
	}
	d.server, err = d.cfg.NewJobServer(cfg.JobCheck.Listen, jobCheck{t: d.table, judges: judges})
	if err != nil {
		return d.toErrorWait("job check server", err)
	}
	return nil
}
