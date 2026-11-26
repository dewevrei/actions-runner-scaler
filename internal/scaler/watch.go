package scaler

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/dewevrei/actions-runner-scaler/internal/retry"
)

// doneCleanupDelay is R101's wait: a done runner whose container is still
// there this long after its first JobCompleted is removed.
const doneCleanupDelay = 5 * time.Minute

// ContainerWaiter waits for a runner container to stop (docker.Client.Wait).
type ContainerWaiter interface {
	// Wait blocks until the runner container called name is not running and
	// returns its exit code. It has no timeout of its own and returns when
	// ctx ends. For a container that does not exist it returns an error
	// carrying status 404 (retry.StatusCoder).
	Wait(ctx context.Context, name string) (exitCode int64, err error)
}

// RunnerWatcher waits, in one goroutine per runner, for each started
// runner's container to end, and then removes the runner (R6, R38): the
// caller that wins Table.BeginExitAndRevoke removes the resources and the
// registration by the retry rules of watch and reconcile (Scaler.retire).
// A wait that fails other than with "not found" ends the watch for that
// runner and is logged; the 30-second reconciliation then finds the stopped
// or missing container (Reconciler).
//
// Watch never blocks. Stop ends every wait, and Scaler.BeginFinalize and
// Scaler.StopWatches call it; Wait waits for the goroutines to return.
// Scaler.ResumeWatches lets it watch again (0002 PLAN M13-6). Removals a
// goroutine began count as the Scaler's work in flight
// (Scaler.WaitInFlight).
//
// It is safe for concurrent use.
type RunnerWatcher struct {
	s      *Scaler
	waiter ContainerWaiter
	work   inFlight

	mu sync.Mutex
	// ctx ends the waits begun since the last resume; Stop cancels it.
	ctx     context.Context
	cancel  context.CancelFunc
	stopped bool
	// watching holds, per runner being watched, a channel closed when its
	// goroutine returns.
	watching map[string]chan struct{}
}

func newRunnerWatcher(s *Scaler, waiter ContainerWaiter) *RunnerWatcher {
	ctx, cancel := context.WithCancel(context.Background())
	return &RunnerWatcher{
		s:        s,
		waiter:   waiter,
		ctx:      ctx,
		cancel:   cancel,
		watching: make(map[string]chan struct{}),
	}
}

// Watcher returns the scaler's RunnerWatcher. The start path calls its Watch
// for every runner that became idle; the daemon calls Stop and Wait when it
// finalizes (M5-8).
func (s *Scaler) Watcher() *RunnerWatcher { return s.watch }

// Watch starts waiting for runner name's container in a new goroutine and
// returns at once. It does nothing once finalize has begun or Stop was
// called, or when name is watched already.
func (w *RunnerWatcher) Watch(name string) {
	log := w.s.log.With("runner", name)
	if w.s.table.Finalizing() {
		log.Info("finalize began; runner not watched")
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.stopped {
		log.Info("watcher stopped; runner not watched")
		return
	}
	if _, ok := w.watching[name]; ok {
		return
	}
	done := make(chan struct{})
	w.watching[name] = done
	w.work.begin()
	go w.watch(w.ctx, log, name, done)
}

func (w *RunnerWatcher) watch(ctx context.Context, log *slog.Logger, name string, done chan struct{}) {
	defer func() {
		w.mu.Lock()
		delete(w.watching, name)
		w.mu.Unlock()
		close(done)
		w.work.end()
	}()
	code, err := w.waiter.Wait(ctx, name)
	if ctx.Err() != nil {
		log.Debug("runner watch stopped")
		return
	}
	switch {
	case err == nil:
		log.Info("runner container stopped", "exitCode", code)
	case isNotFound(err):
		log.Info("runner container gone")
	default:
		err = withOp(retry.OpContainerWait, err)
		log.Warn("waiting for the runner container failed; left to reconciliation",
			"class", retry.Classify(ctx, retry.OpContainerWait, err), "err", err)
		w.s.errs.Record("watch runner container", err)
		return
	}
	w.s.endRunner(log, name, "runner container ended")
}

// Stop ends every wait and refuses new watches. A removal a goroutine began
// goes on and counts for Scaler.WaitInFlight.
func (w *RunnerWatcher) Stop() {
	w.mu.Lock()
	w.stopped = true
	cancel := w.cancel
	w.mu.Unlock()
	cancel()
}

// resume accepts new watches after Stop, with a new context, unless
// finalize has begun; the mark is read under w.mu, so a finalize that stops
// the watcher concurrently comes after and stops it again, or is seen. A
// watch Stop ended is not begun again; reconciliation finds its runner
// (0002 PLAN M13-6).
func (w *RunnerWatcher) resume() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.stopped || w.s.table.Finalizing() {
		return
	}
	w.ctx, w.cancel = context.WithCancel(context.Background())
	w.stopped = false
}

// Wait waits until every watch goroutine has returned, or ctx ends. Outside
// Stop the goroutines wait for their containers without a timeout.
func (w *RunnerWatcher) Wait(ctx context.Context) error {
	return w.work.wait(ctx)
}

// endRunner removes runner name, whose container ended or is gone, if the
// caller wins BeginExitAndRevoke (R6, R38). It reports whether it did. A
// starting runner belongs to its start path, and one whose removal began
// belongs to that removal's owner; both are left alone. Once finalize has
// begun the row is left to finalize (M5-8).
func (s *Scaler) endRunner(log *slog.Logger, name, why string) bool {
	s.work.begin()
	defer s.work.end()
	if s.table.Finalizing() {
		log.Info("finalize began; ended runner left to finalize", "reason", why)
		return false
	}
	info, ok := s.table.Get(name)
	if !ok || info.State == StateStarting {
		return false
	}
	if !s.table.BeginExitAndRevoke(name) {
		log.Debug("runner removal already begun", "reason", why)
		return false
	}
	log.Info("runner ended; removing it", "reason", why, "state", info.State)
	s.retire(log, name, info.ID, unregEnded)
	return true
}

// doneCleanup is R101's removal of a done runner whose container is still
// there 5 minutes after its first JobCompleted. Its timer (armDoneCleanup)
// and reconciliation both call it; Table.BeginDoneCleanup lets one caller
// begin, once per runner. It does nothing before the 5 minutes have passed
// or once finalize has begun, and it runs on every drain kind. It reports
// whether it began the removal.
func (s *Scaler) doneCleanup(name string) bool {
	s.work.begin()
	defer s.work.end()
	log := s.log.With("runner", name)
	if s.table.Finalizing() {
		return false
	}
	info, ok := s.table.Get(name)
	if !ok || info.State != StateDone || s.clock.Now().Before(info.CompletedAt.Add(doneCleanupDelay)) {
		return false
	}
	if !s.table.BeginDoneCleanup(name) {
		return false
	}
	if !s.table.BeginExitAndRevoke(name) {
		log.Debug("done runner's removal already begun")
		return false
	}
	log.Info("runner still there 5 minutes after JobCompleted; removing it (R101)",
		"completedAt", info.CompletedAt)
	s.retire(log, name, info.ID, unregDone)
	return true
}

// reclaimAbandoned takes over an abandoned row with Table.ClaimAbandoned and
// removes it again (R42). A row abandoned on a permanent error is never
// claimed. It reports whether it claimed the row.
func (s *Scaler) reclaimAbandoned(name string) bool {
	s.work.begin()
	defer s.work.end()
	if s.table.Finalizing() {
		return false
	}
	info, ok := s.table.Get(name)
	if !ok || !s.table.ClaimAbandoned(name) {
		return false
	}
	log := s.log.With("runner", name)
	log.Info("removing an abandoned runner again", "reason", info.AbandonReason)
	s.retire(log, name, info.ID, unregAbandoned)
	return true
}

// isNotFound reports whether err carries status 404, as the docker package
// reports a container that does not exist.
func isNotFound(err error) bool {
	var sc retry.StatusCoder
	return errors.As(err, &sc) && sc.HTTPStatus() == http.StatusNotFound
}
