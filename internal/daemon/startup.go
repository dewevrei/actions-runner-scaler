package daemon

import (
	"context"
	"errors"
	"fmt"
	"math"
	mathrand "math/rand/v2"
	"os"
	"path/filepath"
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

	// 6. Configuration errors (R28, R30, R68). Recorded again: a retried
	// Docker call may have replaced the last error since step 3.
	if cfgErr != nil {
		return d.toErrorWait("config", cfgErr)
	}
	if len(cfg.Pools) != 1 {
		// Validate guarantees one pool (R68); this guards a LoadConfig that
		// does not.
		return d.toErrorWait("config", fmt.Errorf("pools: this version supports exactly 1 pool, got %d", len(cfg.Pools)))
	}
	pool := cfg.Pools[0]

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

	// 9. R29.
	d.checkMemory(ctx, cfg.Pools)

	// 10. GitHub clients and R17.
	gh, err := d.cfg.NewGitHub(cfg, key)
	if err != nil {
		return d.toErrorWait("github clients", err)
	}
	pgh, err := gh.Pool(pool)
	if err != nil {
		return d.toErrorWait("github clients", err)
	}
	regCleanup, err := scaler.NewRegistrationCleanup(scaler.RegistrationCleanupConfig{
		ScaleSets: pgh, Runners: pgh, Registry: pgh, Resources: dk,
		Limits: d.limits, Clock: d.clock, Logger: d.log,
	})
	if err != nil {
		return err
	}
	if err := regCleanup.Run(ctx, pool.Name); err != nil {
		return err
	}

	// 11. The scale set.
	var setID int
	err = d.retryStartup(ctx, "scale set", retry.OpGetOrCreateScaleSet, func(ctx context.Context) error {
		var err error
		setID, err = pgh.GetOrCreateScaleSet(ctx, pool.Name)
		return err
	})
	if err != nil {
		return err
	}

	// 12. System information (no request in scaleset v0.5.0).
	info := d.cfg.SystemInfo
	info.ScaleSetID = setID
	pgh.SetSystemInfo(info)

	// 13. The pool's objects and the job check server.
	if err := d.buildPool(cfg, pool, pgh, setID); err != nil {
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
		sess, err = pgh.CreateSession(ctx, setID, d.cfg.Owner)
		return err
	})
	if err != nil {
		return err
	}
	d.setSession(sess)
	return nil
}

// checkMemory warns when the pools' memory limits together exceed what
// Docker can use (R29). A failure to read it is a warning too.
func (d *Daemon) checkMemory(ctx context.Context, pools []config.Pool) {
	ctx, cancel := withTimeout(ctx, d.clock, callTimeout)
	total, err := d.cfg.Docker.MemTotal(ctx)
	cancel()
	if err != nil {
		d.log.Warn("could not read Docker's memory; not checked (R29)", "err", err)
		return
	}
	need := int64(0)
	for _, p := range pools {
		per := int64(p.RunnerMemory) + int64(p.BuildMemory)
		if per > 0 && int64(p.Max) > (math.MaxInt64-need)/per {
			need = math.MaxInt64
			break
		}
		need += int64(p.Max) * per
	}
	if need > total {
		d.log.Warn("pools may use more memory than Docker has; going on (R29)",
			"need", config.ByteSize(need), "dockerMemory", config.ByteSize(total))
	}
}

// buildPool makes the pool's table, judge, scaler, reconciler and job check
// server; none of it calls GitHub or Docker yet.
func (d *Daemon) buildPool(cfg *config.Config, pool config.Pool, gh PoolGitHub, setID int) error {
	if _, _, err := github.ParseRepoURL(pool.RepoURL); err != nil {
		return d.toErrorWait("config", err)
	}
	jobCheckURL, err := docker.JobCheckURL(cfg.JobCheck.Listen)
	if err != nil {
		return d.toErrorWait("config", err)
	}
	runnerLimits, buildKitLimits := docker.PoolLimits(pool)
	dk := dockerAdapter{d: d.cfg.Docker, params: docker.Params{
		RunnerImage:   cfg.Runner.Image,
		BuildKitImage: cfg.BuildKit.Image,
		JobCheckURL:   jobCheckURL,
		Runner:        runnerLimits,
		BuildKit:      buildKitLimits,
	}}
	p := &poolRun{cfg: pool, drainTimeout: cfg.Shutdown.DrainTimeout, gh: gh, docker: dk, scaleSetID: setID}
	d.pool = p

	p.table = scaler.NewTable(nil, d.cfg.Secrets)
	// allowed-workflows and allowed-events are decoded but unused until
	// 0002 PLAN M9 (0002 PLAN M8-5).
	p.judge = scaler.NewJobJudge()
	p.scaler, err = scaler.New(scaler.Config{
		Pool:       pool,
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
		Scaler: p.scaler, Resources: dk, Remover: dk, Processes: dk, Runners: gh,
	})
	if err != nil {
		return err
	}
	d.st.AddPool(pool.Name, p.table)
	p.server, err = d.cfg.NewJobServer(cfg.JobCheck.Listen, jobCheck{t: p.table, j: p.judge})
	if err != nil {
		return d.toErrorWait("job check server", err)
	}
	return nil
}
