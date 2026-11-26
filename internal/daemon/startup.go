package daemon

import (
	"context"
	"errors"
	"fmt"
	mathrand "math/rand/v2"
	"os"
	"path/filepath"
	"strconv"
	"syscall"

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

// waitError means start-up must enter the error wait (R46). The error is
// recorded for status already.
type waitError struct {
	where string
	err   error
}

func (e *waitError) Error() string { return e.where + ": " + e.err.Error() }
func (e *waitError) Unwrap() error { return e.err }

// toErrorWait records err for status, logs it, and returns the waitError
// that sends start-up to the error wait.
func (d *Daemon) toErrorWait(where string, err error) error {
	d.rec.Record(where, err)
	d.log.Error("start-up failed permanently; entering the error wait", "step", where, "err", err)
	return &waitError{where: where, err: err}
}

// errorOnce logs and records a repeated error only when it changes, as the
// status Recorder expects of its callers. Each retry loop owns one, so a
// success, which ends the loop, forgets it.
type errorOnce struct {
	last string
}

func (e *errorOnce) report(d *Daemon, where string, class retry.Class, err error) {
	if text := err.Error(); text != e.last {
		e.last = text
		d.log.Warn("start-up call failed; retrying", "step", where, "class", class, "err", err)
		d.rec.Record(where, err)
	}
}

// retryStartup calls fn until it succeeds (R24): before each call it waits
// for op's allowed time (R52), and a transient or rate-limited failure waits
// the 1 to 60 s backoff, or the rate limit's wait when that is longer, at
// most 24 h (R104). A permanent failure returns a waitError (R46); ctx ending
// (a signal, R57) returns ctx.Err(). Each call gets callTimeout on both the
// wall clock and the Clock (withTimeout); a call that runs out is transient.
func (d *Daemon) retryStartup(ctx context.Context, where string, op retry.Op, fn func(context.Context) error) error {
	backoff, err := retry.NewDefaultBackoff(d.clock, d.jitter())
	if err != nil {
		return err
	}
	var once errorOnce
	for {
		if err := d.waitAllowed(ctx, op); err != nil {
			return err
		}
		callCtx, cancel := withTimeout(ctx, d.clock, callTimeout)
		err := fn(callCtx)
		cancel()
		if err == nil {
			return nil
		}
		d.limits.Record(op, err)
		if !hasOp(err) {
			err = &retry.OpError{Op: op, Err: err}
		}
		switch class := retry.Classify(ctx, op, err); class {
		case retry.Canceled:
			return ctx.Err()
		case retry.Permanent:
			return d.toErrorWait(where, err)
		default:
			once.report(d, where, class, err)
			if err := backoff.Wait(ctx, err); err != nil {
				return err
			}
		}
	}
}

// waitAllowed waits until op may be called (R52).
func (d *Daemon) waitAllowed(ctx context.Context, op retry.Op) error {
	left := d.limits.At(op).Sub(d.clock.Now())
	if left <= 0 {
		return nil
	}
	d.log.Info("waiting for a rate limit to end", "op", op, "until", d.limits.At(op))
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

// start runs the start order up to the first session (PLAN M5-3). It
// returns nil once the first session exists, a waitError for the error
// wait, or ctx.Err() after a signal (R57); waits select on the signal.
func (d *Daemon) start(parent context.Context) error {
	// A signal ends start-up (R57); so does a fatal error, such as the job
	// check server dying during steps 14 and 15, which then waits (R46).
	sigCtx, cancelSig := chanContext(parent, d.sig.first)
	defer cancelSig()
	ctx, cancelStart := chanContext(sigCtx, d.fatalCh)
	defer cancelStart()

	// 3. Configuration; an error is recorded and waits for R16.
	cfg, key, cfgErr := d.cfg.LoadConfig()
	if cfgErr != nil {
		d.rec.Record("config", cfgErr)
		d.log.Error("configuration is invalid; leftover cleanup first, then the error wait (R28)", "err", cfgErr)
	}

	// 4. Docker.
	if err := d.retryStartup(ctx, "docker ping", retry.OpDockerPing, d.cfg.Docker.Ping); err != nil {
		return err
	}
	dk := dockerAdapter{d: d.cfg.Docker}

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

	// 6. Configuration errors (R28, R30, R105~R107, R166). Recorded again: a
	// retried Docker call may have replaced the last error since step 3.
	if cfgErr != nil {
		return d.toErrorWait("config", cfgErr)
	}
	if len(cfg.Targets) != 1 {
		// Until 0002 PLAN M12 serves several targets.
		return d.toErrorWait("config", fmt.Errorf("targets: this build serves exactly 1 target, got %d", len(cfg.Targets)))
	}
	tcfg := cfg.Targets[0]
	target, err := github.ParseTargetURL(tcfg.URL)
	if err != nil {
		return d.toErrorWait("config", fmt.Errorf("targets[0].url: %w", err))
	}

	// 7. R79.
	ectx, cancel := withTimeout(ctx, d.clock, exposureTimeout)
	exposure := d.cfg.CheckExposure(ectx)
	cancel()
	if err := ctx.Err(); err != nil {
		return err
	}
	switch exposure {
	case docker.Exposed:
		return d.toErrorWait("docker exposure", errors.New("Docker API answers on 127.0.0.1:2375 without TLS; stop whatever opened the port (Docker Desktop setting, socat, or other) (R79)"))
	case docker.ExposureUnknown:
		d.log.Warn("could not tell whether the Docker daemon is exposed on TCP 2375; going on (R79)")
	}

	// 8. Images.
	for _, img := range []struct{ where, ref string }{
		{"runner image", cfg.Runner.Image},
		{"buildkit image", cfg.BuildKit.Image},
	} {
		var exists bool
		err := d.retryStartup(ctx, img.where, retry.OpImageInspect, func(ctx context.Context) error {
			var err error
			exists, err = d.cfg.Docker.ImageExists(ctx, img.ref)
			return err
		})
		if err != nil {
			return err
		}
		if !exists {
			return d.toErrorWait(img.where, fmt.Errorf("image %s is not present locally", img.ref))
		}
	}

	// 8b. Docker capacity and the server limit (0002 SPEC R201~R204, R167, R168).
	limit, err := d.checkCapacity(ctx, cfg)
	if err != nil {
		return err
	}
	d.st.SetServerLimit(limit)

	// 9. GitHub: the installation of the target (R108~R110).
	gh, err := d.cfg.NewGitHub(cfg, key)
	if err != nil {
		return d.toErrorWait("github clients", err)
	}
	var inst github.Installation
	err = d.retryStartup(ctx, "installation", retry.OpGetInstallation, func(ctx context.Context) error {
		var err error
		inst, err = gh.Installation(ctx, target)
		return err
	})
	if err != nil {
		return err
	}
	if err := github.CheckPermission(inst, target); err != nil {
		return d.toErrorWait("installation", err)
	}
	tgh, err := gh.Target(target, inst.ID)
	if err != nil {
		return d.toErrorWait("github clients", err)
	}

	// 10. The runner group (R111).
	var groupID int
	err = d.retryStartup(ctx, "runner group", retry.OpGetRunnerGroup, func(ctx context.Context) error {
		var err error
		groupID, err = tgh.RunnerGroupID(ctx, tcfg.RunnerGroup)
		return err
	})
	if err != nil {
		return err
	}

	// 11. R148, then the scale set (R112).
	regCleanup, err := scaler.NewRegistrationCleanup(scaler.RegistrationCleanupConfig{
		ScaleSets: tgh, Runners: tgh, Registry: tgh, Resources: dk,
		Limits: d.limits, Clock: d.clock, Logger: d.log,
	})
	if err != nil {
		return err
	}
	if err := regCleanup.Run(ctx, groupID, cfg.Scaling.Label); err != nil {
		return err
	}
	var setID int
	err = d.retryStartup(ctx, "scale set", retry.OpGetOrCreateScaleSet, func(ctx context.Context) error {
		var err error
		setID, err = tgh.GetOrCreateScaleSet(ctx, groupID, cfg.Scaling.Label)
		return err
	})
	if err != nil {
		return err
	}

	// 12. System information (no request in scaleset v0.5.0).
	info := d.cfg.SystemInfo
	info.ScaleSetID = setID
	tgh.SetSystemInfo(info)

	// 13. The target's objects and the job check server.
	if err := d.buildTarget(cfg, target, tcfg, tgh, setID); err != nil {
		return err
	}
	if err := d.pool.server.Listen(); err != nil {
		return d.toErrorWait("job check server", err)
	}
	// Serve returns nil after Shutdown. Any other end leaves hooks without
	// verdicts, so no runner may start: an error drain (R51).
	go func() {
		if err := d.pool.server.Serve(); err != nil {
			d.fatal(fmt.Errorf("job check server stopped: %w", err))
		}
	}()

	// 14. Watch and reconcile. Watches start with each runner.
	d.pool.recon.Start()

	// 15. The first session.
	var sess Session
	err = d.retryStartup(ctx, "message session", retry.OpSessionCreate, func(ctx context.Context) error {
		var err error
		sess, err = tgh.CreateSession(ctx, setID, d.cfg.Owner)
		return err
	})
	if err != nil {
		return err
	}
	d.setSession(sess)
	return nil
}

// checkCapacity reads /info once, computes the server runner limit and
// checks the targets against it (0002 SPEC R201~R204, R167, R168; 0002 PLAN
// M10). It runs before any target's GitHub call, and its permanent errors
// are the server error wait (R176). The limit is not computed again while
// the scaler runs.
func (d *Daemon) checkCapacity(ctx context.Context, cfg *config.Config) (int, error) {
	var capacity docker.Capacity
	err := d.retryStartup(ctx, "docker info", retry.OpDockerInfo, func(ctx context.Context) error {
		var err error
		capacity, err = d.cfg.Docker.Info(ctx)
		return err
	})
	if err != nil {
		return 0, err
	}
	share := docker.RunnerShare(docker.ContainerLimits(cfg.Runner), docker.ContainerLimits(cfg.BuildKit))
	limit := docker.ServerLimit(capacity, share)
	d.log.Info("server runner limit", "limit", limit, "ncpu", capacity.NCPU, "memTotal", capacity.MemTotal,
		"runnerCPUs", float64(share.NanoCPUs)/1e9, "runnerMemory", share.Memory)
	if limit == 0 {
		return 0, d.toErrorWait("docker capacity", fmt.Errorf(
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
		return 0, d.toErrorWait("docker capacity", fmt.Errorf(
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

// formatCPUs prints a CPU count as configured: 3 and 0.5, not 3.000000.
func formatCPUs(cpus float64) string { return strconv.FormatFloat(cpus, 'g', -1, 64) }

// buildTarget makes the target's table, judge, scaler, reconciler and job
// check server; none of it calls GitHub or Docker yet.
func (d *Daemon) buildTarget(cfg *config.Config, target github.Target, tcfg config.Target, gh TargetGitHub, setID int) error {
	jobCheckURL, err := docker.JobCheckURL(cfg.JobCheck.Listen)
	if err != nil {
		return d.toErrorWait("config", err)
	}
	dk := dockerAdapter{d: d.cfg.Docker, params: docker.Params{
		RunnerImage:   cfg.Runner.Image,
		BuildKitImage: cfg.BuildKit.Image,
		JobCheckURL:   jobCheckURL,
		Runner:        docker.ContainerLimits(cfg.Runner),
		BuildKit:      docker.ContainerLimits(cfg.BuildKit),
	}}
	p := &poolRun{cfg: tcfg, drainTimeout: cfg.Shutdown.DrainTimeout, gh: gh, docker: dk, scaleSetID: setID}
	d.pool = p

	p.table = scaler.NewTable(nil, d.cfg.Secrets)
	p.judge = scaler.NewJobJudge()
	p.scaler, err = scaler.New(scaler.Config{
		Target:     scaler.TargetConfig{Key: target.Key(), Label: cfg.Scaling.Label, MaxRunners: tcfg.MaxRunners, Idle: tcfg.Idle},
		Table:      p.table,
		Clock:      d.clock,
		ScaleSet:   boundScaleSet{gh: gh, id: setID},
		Containers: dk,
		Judge:      p.judge,
		Tokens:     gate.NewTokenGenerator(nil),
		Limits:     d.limits,
		Fatal:      d.fatal,
		Secrets:    d.cfg.Secrets,
		Waiter:     dk,
		Logger:     d.log,
		Errors:     d.rec,
		Random:     d.cfg.Random,
		Jitter:     d.cfg.Jitter,
	})
	if err != nil {
		p.judge = nil
		return err
	}
	p.recon, err = scaler.NewReconciler(scaler.ReconcilerConfig{
		Scaler: p.scaler, Resources: dk, Remover: dk, Processes: dk, Finder: gh,
	})
	if err != nil {
		return err
	}
	d.st.AddPool(tcfg.URL, p.table)
	p.server, err = d.cfg.NewJobServer(cfg.JobCheck.Listen, jobCheck{t: p.table, j: p.judge})
	if err != nil {
		return d.toErrorWait("job check server", err)
	}
	return nil
}
