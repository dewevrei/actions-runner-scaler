package scaler

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"time"

	"github.com/dewevrei/actions-runner-scaler/internal/gate"
	"github.com/dewevrei/actions-runner-scaler/internal/retry"
)

// redacted replaces a JIT config wherever it might be printed.
const redacted = "<redacted>"

// nameSuffixBytes random bytes make the 8 hex characters after the pool
// name.
const nameSuffixBytes = 4

// JITConfig is a runner's encoded JIT configuration. It is a credential:
// String, GoString and LogValue hide it, and only Reveal returns the text,
// for writing it to the runner's stdin. The zero value is empty.
type JITConfig struct {
	// Behind a pointer so that fmt, printing a struct that holds a
	// JITConfig in an unexported field, shows an address and not the text.
	value *string
}

// NewJITConfig wraps the EncodedJITConfig of a GenerateJitRunnerConfig
// response.
func NewJITConfig(encoded string) JITConfig { return JITConfig{value: &encoded} }

// Reveal returns the encoded config.
func (c JITConfig) Reveal() string {
	if c.value == nil {
		return ""
	}
	return *c.value
}

// String hides the config.
func (c JITConfig) String() string { return redacted }

// GoString hides the config from %#v.
func (c JITConfig) GoString() string { return redacted }

// LogValue hides the config from slog.
func (c JITConfig) LogValue() slog.Value { return slog.StringValue(redacted) }

// JITRunner is a registered runner and its JIT config.
type JITRunner struct {
	// ID is the GitHub runner ID (RunnerReference.ID), used by RemoveRunner.
	ID     int64
	Config JITConfig
}

// errStartAborted means a start path stopped without failing: the context
// ended, finalize began, or another caller began removing the runner. It is
// not a start failure (R61).
var errStartAborted = errors.New("runner start aborted")

// startRunners starts up to n runners one after another, inside the Scale
// that asked for them (R4). It stops early while draining, during the start
// backoff (R61), or while JIT generation is rate limited (R52, R87).
func (s *Scaler) startRunners(ctx context.Context, n int) error {
	for range n {
		if s.table.Draining() {
			return nil
		}
		if next := s.table.StartStatus().NextStartAt; s.clock.Now().Before(next) {
			s.log.Info("runner start deferred by start backoff", "until", next)
			return nil
		}
		if !s.limits.Allowed(retry.OpGenerateJIT) {
			s.log.Info("runner start deferred by JIT rate limit",
				"until", s.limits.At(retry.OpGenerateJIT))
			return nil
		}
		if err := s.startRunner(ctx); errors.Is(err, errStartAborted) {
			// Cancellation becomes Scale's error so the message is not acked.
			return ctx.Err()
		}
		// A failure is counted already; the checks above gate the next try.
	}
	return nil
}

// startPath is one runner's start: starting → JIT → token → containers →
// idle (R2, R18, R63, R64).
type startPath struct {
	s    *Scaler
	ctx  context.Context
	name string
	log  *slog.Logger
	// id is the GitHub runner ID; zero until the JIT config exists.
	id int64
	// secrets are the values registered with the SecretRegistry for the
	// length of the start path: the JIT config. The table registers the job
	// check token for as long as it holds it.
	secrets []string
}

// startRunner runs one start path. It returns nil when the runner is idle,
// errStartAborted when it stopped without failing, or the counted start
// failure.
func (s *Scaler) startRunner(ctx context.Context) error {
	s.work.begin()
	defer s.work.end()

	name, err := s.newName()
	if err != nil {
		return s.startFailed(ctx, s.log, "", fmt.Errorf("make runner name: %w", err))
	}
	log := s.log.With("runner", name)
	if err := s.table.Add(name, s.clock.Now()); err != nil {
		return s.startFailed(ctx, log, "", err)
	}
	p := &startPath{s: s, ctx: ctx, name: name, log: log}
	defer p.forgetSecrets()
	return p.run()
}

func (p *startPath) run() error {
	s := p.s
	if s.table.Finalizing() {
		p.abandon()
		return errStartAborted
	}

	callCtx, cancel := context.WithTimeout(p.ctx, callTimeout)
	jit, err := s.scaleSet.GenerateJITConfig(callCtx, p.name)
	cancel()
	if err != nil {
		s.limits.Record(retry.OpGenerateJIT, err)
		// Nothing is registered, so there is nothing to unregister (R18).
		p.abandon()
		return s.startFailed(p.ctx, p.log, retry.OpGenerateJIT, err)
	}
	p.id = jit.ID
	s.table.SetRunnerID(p.name, jit.ID)
	p.remember(jit.Config.Reveal())

	// From here on every exit removes the resources and the registration.
	if s.table.Finalizing() {
		p.cleanup()
		return errStartAborted
	}

	token, err := s.tokens.New()
	if err != nil {
		return p.fail("", fmt.Errorf("make job check token: %w", err))
	}
	if !s.table.SetToken(p.name, token) || s.table.Finalizing() {
		p.cleanup()
		return errStartAborted
	}

	startCtx, cancel := context.WithTimeout(p.ctx, startRunnerTimeout)
	err = s.containers.StartRunner(startCtx, p.name, jit.Config, token)
	cancel()
	if err != nil {
		return p.fail(retry.OpContainerStart, err)
	}

	// Finalize may have cleaned up before the containers existed; the start
	// path removes what it made before returning (M3-7). MarkStarted refuses
	// once finalize began, under the table's mutex, so a runner is watched
	// only if it became idle before finalize did.
	if !s.table.MarkStarted(p.name) {
		p.cleanup()
		return errStartAborted
	}
	s.startBackoff.Reset()
	s.table.RecordStartSuccess()
	p.log.Info("runner started", "runnerId", jit.ID)
	s.watcher.Watch(p.name)
	return nil
}

// fail counts a start failure after the JIT config existed, then cleans up.
// The failure is recorded first, while the JIT config and the token are
// still registered secrets, so the stored error is redacted with both
// (R31).
func (p *startPath) fail(op retry.Op, err error) error {
	err = p.s.startFailed(p.ctx, p.log, op, err)
	p.cleanup()
	return err
}

// abandon drops a runner that has no registration or containers yet.
func (p *startPath) abandon() {
	p.s.table.BeginExit(p.name)
	p.s.table.MarkRemoved(p.name)
}

// cleanup removes what the start path made after the JIT config existed:
// the resources, then the registration (R18). It runs whoever won
// BeginExit, because only the start path knows what it made, and revokes
// the token either way (R38). When another caller won BeginExit, that
// caller owns the row, so the start path never marks it abandoned
// (Table.AbandonExit).
//
// The first removal attempt runs here; transient failures are retried on
// timers (resourceRemoval), and the row is deleted once the resources are
// gone. RemoveRunner follows the first attempt whatever its result, so a
// container that could not be removed yet can never take a job (R18: the
// registration goes at once). A RemoveRunner that is rate limited or fails
// with a transient error is retried on a timer (unregistration), and the
// registration is listed in PendingUnregistrations until it is confirmed
// removed.
func (p *startPath) cleanup() {
	s := p.s
	owner := s.table.BeginExitAndRevoke(p.name)
	if !owner {
		s.table.RevokeToken(p.name)
	}
	s.removeResources(p.ctx, p.log, p.name, owner)
	s.unregister(p.ctx, p.log, p.name, p.id, unregStart)
}

// remember registers a secret for the length of the start path.
func (p *startPath) remember(secret string) {
	if secret == "" {
		return
	}
	p.s.secrets.Register(secret)
	p.secrets = append(p.secrets, secret)
}

func (p *startPath) forgetSecrets() {
	for _, v := range p.secrets {
		p.s.secrets.Unregister(v)
	}
}

// startFailed counts a start failure and sets the backoff (R61, R64, R96),
// unless ctx ended, which is not a failure. op names the failed call, or is
// empty when the failure was not a call. The error is stored as redacted
// text (StartStatus.LastError, R31).
func (s *Scaler) startFailed(ctx context.Context, log *slog.Logger, op retry.Op, err error) error {
	if ctx.Err() != nil {
		log.Info("runner start canceled")
		return errStartAborted
	}
	var class retry.Class
	if op != "" {
		err = withOp(op, err)
		class = retry.Classify(ctx, op, err)
	}
	now := s.clock.Now()
	failures := s.table.StartStatus().Failures + 1
	var next time.Time
	if failures >= startBackoffAfter {
		next = now.Add(s.startBackoff.Next())
	}
	s.table.RecordStartFailure(errors.New(s.secrets.Redact(err.Error())), now, next)
	log.Warn("runner start failed",
		"failures", failures, "class", class, "nextStartAt", next, "err", err)
	return err
}

// newName returns "<pool>-<8 hex characters>".
func (s *Scaler) newName() (string, error) {
	var b [nameSuffixBytes]byte
	if _, err := io.ReadFull(s.random, b[:]); err != nil {
		return "", err
	}
	return s.pool.Name + "-" + hex.EncodeToString(b[:]), nil
}

// poolRunnerName reports whether name has the form newName gives the
// runners of pool: "<pool>-" and 8 lowercase hex characters, which is
// ^<pool>-[0-9a-f]{8}$ with pool taken literally. A pool named "<pool>-x"
// has runners whose names start with "<pool>-" too, but its suffix is
// longer, so they do not match.
func poolRunnerName(pool, name string) bool {
	suffix, ok := strings.CutPrefix(name, pool+"-")
	if !ok || len(suffix) != 2*nameSuffixBytes {
		return false
	}
	for _, c := range []byte(suffix) {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// Interface checks for the concrete types the daemon will pass in.
var (
	_ TokenSource = (*gate.TokenGenerator)(nil)
	_ RateLimits  = (*retry.NextAllowed)(nil)
)
