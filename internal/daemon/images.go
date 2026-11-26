package daemon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/dewevrei/actions-runner-scaler/internal/config"
	"github.com/dewevrei/actions-runner-scaler/internal/docker"
	"github.com/dewevrei/actions-runner-scaler/internal/retry"
	"github.com/dewevrei/actions-runner-scaler/internal/scaler"
)

// pullTimeout bounds one image pull (0003 SPEC R320).
const pullTimeout = 30 * time.Minute

// errMissingAfterPull is the cause of a pull that succeeded while the image
// was still not present at the lookup that followed it (0003 SPEC R320).
var errMissingAfterPull = &retry.OpError{Op: retry.OpImagePull,
	Err: fmt.Errorf("%w: the image is not present after the pull", retry.ErrPullFailed)}

// imageDocker is the Docker side of the image lookups and pulls
// (*docker.Client).
type imageDocker interface {
	ImageInspect(ctx context.Context, ref string) (docker.Image, bool, error)
	ImagePull(ctx context.Context, ref string) error
}

// imageKey is one image setting.
type imageKey struct {
	// name is the configuration key, for the log lines (0003 SPEC R320,
	// R321); where names the start-up step for status.
	name, where string
	// value is the configured value; isID is true when it is a local image
	// ID, which cannot be pulled (config.ImageIsID).
	value string
	isID  bool
}

func newImageKey(name, where, value string) imageKey {
	return imageKey{name: name, where: where, value: value, isID: config.ImageIsID(value)}
}

// pullState is one reference's background pull (0003 SPEC R319).
type pullState struct {
	running bool
	// ended counts the pulls that ended, so a lookup that began before a
	// pull ended does not start another one (ensurePull).
	ended uint64
	// lastErr is the last pull's failure, nil after a success; no pull of
	// the reference starts before next.
	lastErr error
	next    time.Time
	backoff *retry.Backoff
}

// imageResolverConfig holds an imageResolver's dependencies; all are
// required.
type imageResolverConfig struct {
	Docker imageDocker
	Clock  scaler.Clock
	Logger *slog.Logger
	Jitter func() float64
	// Context lives as long as the server: finalize cancels it, which ends
	// every pull (the daemon's listenCtx).
	Context context.Context
	// Runner and BuildKit are runner.image and buildkit.image.
	Runner, BuildKit string
}

// imageResolver looks up both images at start-up and before each runner's
// JIT request, and pulls a missing image reference: in the foreground at
// start-up (Daemon.startupImage), in the background at runner creation
// (0003 SPEC R319~R321). It implements scaler.ImageResolver.
//
// At most one pull of a reference runs at a time. A background pull runs on
// the server context, never under the Placer's lock, and its end calls
// onPullDone (Placer.Kick), which never blocks. mu guards logged, pulls,
// stopped and onPullDone; it is never held across a Docker call.
type imageResolver struct {
	keys   [2]imageKey // runner.image, buildkit.image
	docker imageDocker
	clock  scaler.Clock
	log    *slog.Logger
	ctx    context.Context

	mu         sync.Mutex
	onPullDone func()
	// logged holds per key name the image ID of the last "image" line.
	logged map[string]string
	// pulls holds a state per image reference (IDs have none).
	pulls map[string]*pullState
	// stopped is set by wait, after which no pull starts.
	stopped bool
	// pulling counts the pull goroutines; Add only under mu while not
	// stopped.
	pulling sync.WaitGroup
}

var _ scaler.ImageResolver = (*imageResolver)(nil)

func newImageResolver(cfg imageResolverConfig) (*imageResolver, error) {
	if cfg.Docker == nil || cfg.Clock == nil || cfg.Logger == nil || cfg.Jitter == nil || cfg.Context == nil {
		return nil, errors.New("daemon: image resolver missing a dependency")
	}
	r := &imageResolver{
		keys: [2]imageKey{
			newImageKey("runner.image", "runner image", cfg.Runner),
			newImageKey("buildkit.image", "buildkit image", cfg.BuildKit),
		},
		docker: cfg.Docker,
		clock:  cfg.Clock,
		log:    cfg.Logger,
		ctx:    cfg.Context,
		logged: make(map[string]string),
		pulls:  make(map[string]*pullState),
	}
	for _, k := range r.keys {
		if k.isID || r.pulls[k.value] != nil {
			continue
		}
		// 0001 R24's 1 s doubling to 60 s, with the daemon's jitter.
		b, err := retry.NewDefaultBackoff(cfg.Clock, cfg.Jitter)
		if err != nil {
			return nil, err
		}
		r.pulls[k.value] = &pullState{backoff: b}
	}
	return r, nil
}

// setOnPullDone sets what the end of a background pull calls: Placer.Kick,
// once the Placer exists. It must not block.
func (r *imageResolver) setOnPullDone(f func()) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.onPullDone = f
}

// lookup is one GET /images/{name}/json for k under callTimeout, on the wall
// clock and the Clock.
func (r *imageResolver) lookup(ctx context.Context, k imageKey) (docker.Image, bool, error) {
	ctx, cancel := withTimeout(ctx, r.clock, callTimeout)
	defer cancel()
	return r.docker.ImageInspect(ctx, k.value)
}

// ResolveImages looks up both images for one runner (0003 SPEC R319). It
// makes only Docker lookups and returns at once when a reference is missing:
// it starts that reference's background pull unless one runs or its backoff
// is on, and returns scaler.ErrImagePending. The other key is still looked
// up, so two missing references are pulled at the same time. A failed
// lookup, a missing image ID and a reference whose last pull failed within
// its backoff are errors, which the caller counts.
func (r *imageResolver) ResolveImages(ctx context.Context) (scaler.ImageIDs, error) {
	var ids [2]string
	pending := false
	for i, k := range r.keys {
		id, err := r.resolve(ctx, k)
		switch {
		case errors.Is(err, scaler.ErrImagePending):
			pending = true
		case err != nil:
			return scaler.ImageIDs{}, err
		}
		ids[i] = id
	}
	if pending {
		return scaler.ImageIDs{}, scaler.ErrImagePending
	}
	return scaler.ImageIDs{Runner: ids[0], BuildKit: ids[1]}, nil
}

func (r *imageResolver) resolve(ctx context.Context, k imageKey) (string, error) {
	ended := r.pullsEnded(k)
	img, ok, err := r.lookup(ctx, k)
	switch {
	case err != nil:
		return "", err
	case ok:
		r.note(k, img.ID, false, false)
		return img.ID, nil
	case k.isID:
		return "", missingImage(k)
	}
	return "", r.ensurePull(k, ended)
}

// pullsEnded returns how many pulls of k ended; zero for an image ID.
func (r *imageResolver) pullsEnded(k imageKey) uint64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	if st := r.pulls[k.value]; st != nil {
		return st.ended
	}
	return 0
}

// ensurePull starts k's background pull unless one runs, one succeeded
// since the lookup began (ended is the count read before it), the last one
// failed and its backoff is on, or the resolver stopped. It returns
// scaler.ErrImagePending, or in the backoff the R320 error with the last
// failure. A pull that succeeded during the lookup kicked a placement at its
// end, which runs after the caller's and finds the image.
func (r *imageResolver) ensurePull(k imageKey, ended uint64) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	st := r.pulls[k.value]
	switch {
	case st.running, st.ended != ended && st.lastErr == nil:
		return scaler.ErrImagePending
	case st.lastErr != nil && r.clock.Now().Before(st.next):
		return pullFailed(k, st.lastErr)
	case r.stopped || r.ctx.Err() != nil:
		// Finalize: no pull starts, and the start is not a failure.
		return scaler.ErrImagePending
	}
	st.running = true
	r.pulling.Add(1)
	go r.pull(k, st)
	return scaler.ErrImagePending
}

// pull is one background pull of k and the lookup after it (0003 SPEC
// R319~R321). The end of the server context stops it and records nothing.
func (r *imageResolver) pull(k imageKey, st *pullState) {
	defer r.pulling.Done()
	r.log.Info("pulling image", "key", k.name, "ref", k.value)
	img, found, lookupErr, err := r.pullAndLookup(k)

	r.mu.Lock()
	st.running = false
	st.ended++
	if r.ctx.Err() != nil {
		r.mu.Unlock()
		return
	}
	line := false
	if err == nil {
		st.lastErr, st.next = nil, time.Time{}
		st.backoff.Reset()
		if found {
			line = r.noteLocked(k, img.ID, true, false)
		}
	} else {
		st.lastErr = err
		st.next = r.clock.Now().Add(st.backoff.Next())
	}
	done := r.onPullDone
	r.mu.Unlock()

	switch {
	case err != nil:
		r.log.Warn("image pull failed", "key", k.name, "ref", k.value, "err", err)
	case lookupErr != nil:
		// The pull succeeded; the next runner's lookup tries again.
		r.log.Warn("image lookup after the pull failed", "key", k.name, "ref", k.value, "err", lookupErr)
	case line:
		r.logImage(k, img.ID, true)
	}
	if done != nil {
		done()
	}
}

// pullAndLookup pulls k under pullTimeout and looks it up once. err is the
// pull's failure, or errMissingAfterPull when the lookup found nothing;
// lookupErr is a failed lookup after a successful pull, which is not a
// pull failure.
func (r *imageResolver) pullAndLookup(k imageKey) (img docker.Image, found bool, lookupErr, err error) {
	ctx, cancel := withTimeout(r.ctx, r.clock, pullTimeout)
	err = r.docker.ImagePull(ctx, k.value)
	cancel()
	if err != nil {
		return docker.Image{}, false, nil, err
	}
	img, found, lookupErr = r.lookup(r.ctx, k)
	if lookupErr == nil && !found {
		return docker.Image{}, false, nil, errMissingAfterPull
	}
	return img, found, lookupErr, nil
}

// note writes the R321 line for k's ID when startup or pulled is true or
// the ID differs from the last one logged for k.
func (r *imageResolver) note(k imageKey, id string, pulled, startup bool) {
	r.mu.Lock()
	line := r.noteLocked(k, id, pulled, startup)
	r.mu.Unlock()
	if line {
		r.logImage(k, id, pulled)
	}
}

// noteLocked records id as k's last logged ID and reports whether the R321
// line is due. The caller holds mu and writes the line after releasing it.
func (r *imageResolver) noteLocked(k imageKey, id string, pulled, startup bool) bool {
	line := startup || pulled || r.logged[k.name] != id
	r.logged[k.name] = id
	return line
}

func (r *imageResolver) logImage(k imageKey, id string, pulled bool) {
	r.log.Info("image", "key", k.name, "ref", k.value, "id", id, "pulled", pulled)
}

// wait stops new pulls and waits for the running ones, which end once the
// server context is canceled, or until ctx ends. Finalize calls it after
// that cancel.
func (r *imageResolver) wait(ctx context.Context) error {
	r.mu.Lock()
	r.stopped = true
	r.mu.Unlock()
	done := make(chan struct{})
	go func() {
		r.pulling.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// missingImage is the error of an image ID that is not present (0001 R24,
// 0003 SPEC R319).
func missingImage(k imageKey) error {
	return fmt.Errorf("image %s is not present locally", k.value)
}

// pullFailed is the R320 error of a reference whose pull failed with err.
func pullFailed(k imageKey, err error) error {
	return fmt.Errorf("image %s is not present locally and pulling it failed: %w", k.value, err)
}

// startupImage is start-up step 8 for one key (0003 SPEC R319~R321): the
// lookup with 0001 R24's retries; a missing image ID enters the error wait
// (R46); a missing reference is pulled in the foreground, a transport error
// or 5xx retried, any other failure entering the error wait with the R320
// text; then the lookup once more and the R321 line.
func (d *Daemon) startupImage(ctx context.Context, r *imageResolver, k imageKey) error {
	var (
		img docker.Image
		ok  bool
	)
	inspect := func(ctx context.Context) error {
		var err error
		img, ok, err = r.docker.ImageInspect(ctx, k.value)
		return err
	}
	if err := d.retryStartup(ctx, k.where, retry.OpImageInspect, inspect); err != nil {
		return err
	}
	pulled := false
	if !ok {
		if k.isID {
			return d.toErrorWait(k.where, missingImage(k))
		}
		sc := d.serverScope()
		sc.fail = func(where string, err error) error { return d.toErrorWait(where, pullFailed(k, err)) }
		err := d.retryInFor(ctx, sc, k.where, retry.OpImagePull, pullTimeout, func(ctx context.Context) error {
			r.log.Info("pulling image", "key", k.name, "ref", k.value)
			return r.docker.ImagePull(ctx, k.value)
		})
		if err != nil {
			return err
		}
		if err := d.retryStartup(ctx, k.where, retry.OpImageInspect, inspect); err != nil {
			return err
		}
		if !ok {
			return d.toErrorWait(k.where, pullFailed(k, errMissingAfterPull))
		}
		pulled = true
	}
	r.note(k, img.ID, pulled, true)
	return nil
}
