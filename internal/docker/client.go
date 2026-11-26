package docker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"

	"github.com/dewevrei/actions-runner-scaler/internal/retry"
)

// DefaultHost is the Docker socket the scaler uses.
const DefaultHost = client.DefaultDockerHost

// Engine is the part of the Docker Engine API client this package uses.
// *client.Client implements it. Build it with NewEngine so that error
// responses keep their status (see StatusError).
type Engine interface {
	Ping(ctx context.Context, options client.PingOptions) (client.PingResult, error)
	Info(ctx context.Context, options client.InfoOptions) (client.SystemInfoResult, error)
	ImageInspect(ctx context.Context, image string, options ...client.ImageInspectOption) (client.ImageInspectResult, error)
	NetworkCreate(ctx context.Context, name string, options client.NetworkCreateOptions) (client.NetworkCreateResult, error)
	NetworkList(ctx context.Context, options client.NetworkListOptions) (client.NetworkListResult, error)
	NetworkRemove(ctx context.Context, network string, options client.NetworkRemoveOptions) (client.NetworkRemoveResult, error)
	ContainerCreate(ctx context.Context, options client.ContainerCreateOptions) (client.ContainerCreateResult, error)
	ContainerAttach(ctx context.Context, container string, options client.ContainerAttachOptions) (client.ContainerAttachResult, error)
	ContainerStart(ctx context.Context, container string, options client.ContainerStartOptions) (client.ContainerStartResult, error)
	ContainerWait(ctx context.Context, container string, options client.ContainerWaitOptions) client.ContainerWaitResult
	ContainerList(ctx context.Context, options client.ContainerListOptions) (client.ContainerListResult, error)
	ContainerTop(ctx context.Context, container string, options client.ContainerTopOptions) (client.ContainerTopResult, error)
	ContainerRemove(ctx context.Context, container string, options client.ContainerRemoveOptions) (client.ContainerRemoveResult, error)
}

// NewEngine returns an Engine API client for host, such as DefaultHost. It
// negotiates the API version on its first call and records the status of
// error responses for the Client.
func NewEngine(host string) (*client.Client, error) {
	return client.New(append(statusHooks(), client.WithHost(host))...)
}

// Client manages runner resources through an Engine.
//
// Every failed call returns a *retry.OpError whose Err reports the response
// status through HTTPStatus when the daemon answered. Callers classify it
// with retry.Classify and decide timeouts and retries themselves.
type Client struct {
	engine Engine
	// managedBy is the LabelManagedBy value the Client sets and lists.
	// New sets ManagedBy; the integration tests use another value so that
	// a running scaler and the tests never see each other's resources.
	managedBy string
}

// New returns a Client that uses engine.
func New(engine Engine) *Client {
	return &Client{engine: engine, managedBy: ManagedBy}
}

// Stdin holds the two secret lines the runner entrypoint reads from stdin:
// the JIT config, then the job-check token. They never go into the
// container's env, args or labels (R36), and Stdin never prints them.
//
// The values sit inside a closure. fmt calls String and GoString only on
// values it can reach as interfaces, so a Stdin in an unexported field of
// another struct would print its fields; a func field prints as an address
// at most, and neither fmt nor reflect can read what a closure captured.
type Stdin struct {
	secrets func() (jit, token string)
}

// NewStdin returns the stdin for a runner.
func NewStdin(jit, token string) Stdin {
	return Stdin{secrets: func() (string, string) { return jit, token }}
}

// values returns the JIT config and the token; both are empty for the zero
// Stdin.
func (s Stdin) values() (jit, token string) {
	if s.secrets == nil {
		return "", ""
	}
	return s.secrets()
}

const redacted = "<redacted>"

// String hides the secrets.
func (Stdin) String() string { return redacted }

// GoString hides the secrets.
func (Stdin) GoString() string { return redacted }

// LogValue hides the secrets from slog.
func (Stdin) LogValue() slog.Value { return slog.StringValue(redacted) }

// validate reports a missing or multi-line value without quoting it.
func (s Stdin) validate() error {
	jit, token := s.values()
	for _, v := range []struct{ name, value string }{{"JIT config", jit}, {"job-check token", token}} {
		if v.value == "" {
			return fmt.Errorf("stdin: %s is empty", v.name)
		}
		if strings.ContainsAny(v.value, "\r\n") {
			return fmt.Errorf("stdin: %s has a line break", v.name)
		}
	}
	return nil
}

// bytes returns the two lines, each ending in \n as the entrypoint needs.
func (s Stdin) bytes() []byte {
	jit, token := s.values()
	return []byte(jit + "\n" + token + "\n")
}

// undoTimeout bounds all removals that undo one failed start. They run
// without the caller's cancellation, so a start cancelled halfway (drain,
// shutdown) still removes what it created; the bound keeps a hung daemon
// from holding the caller. Each removal is one local Engine API call; 10s
// is the time R11 gives for removing a runner container.
const undoTimeout = 10 * time.Second

// StartRunner creates and starts the resources of runner p in order (R63):
// the network, the BuildKit container (created and started, without
// waiting for buildkitd), then the runner container, which is attached,
// started and sent the two stdin lines before stdin is closed.
//
// If a step fails, StartRunner removes what it created in reverse order
// (R18) and returns the step's error, with the text of any removal failure
// joined to it. Not-found removals count as done. A create that fails is
// undone too, by name: its response can be lost to cancellation or a
// dropped connection after the daemon made the resource. The names derive
// from the runner name, which is unique to this runner, so whatever holds
// them is this runner's. The removals keep ctx's values but not its
// cancellation or deadline, and get undoTimeout in all, so they are tried
// even when ctx is what ended the start. Callers still remove the runner's
// resources themselves after a failure, since a removal can fail or time
// out, or reach the daemon before a create it lost the answer to.
func (c *Client) StartRunner(ctx context.Context, p Params, in Stdin) (err error) {
	if p.Name == "" {
		return errors.New("start runner: empty runner name")
	}
	if err := in.validate(); err != nil {
		return err
	}

	p.managedBy = c.managedBy

	var undo []func(context.Context) error
	defer func() {
		if err == nil || len(undo) == 0 {
			return
		}
		uctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), undoTimeout)
		defer cancel()
		for i := len(undo) - 1; i >= 0; i-- {
			if uerr := undo[i](uctx); uerr != nil {
				// %v keeps an undo status out of the chain, so the failed
				// step alone decides how retry.Classify sees err.
				err = errors.Join(err, fmt.Errorf("undo start of runner %s: %v", p.Name, uerr))
			}
		}
	}()

	nctx := track(ctx)
	nw, err := c.engine.NetworkCreate(nctx, NetworkName(p.Name), NetworkSpec(p))
	// Remove by ID when the daemon returned one, else by name.
	netRef := nw.ID
	if err != nil {
		netRef = NetworkName(p.Name)
	}
	undo = append(undo, func(ctx context.Context) error { return ignoreNotFound(c.removeNetwork(ctx, netRef)) })
	if err != nil {
		return opError(nctx, retry.OpNetworkCreate, err)
	}

	bk, err := c.createUndoable(ctx, BuildKitSpec(p), &undo)
	if err != nil {
		return err
	}
	if err := c.start(ctx, bk); err != nil {
		return err
	}

	runner, err := c.createUndoable(ctx, RunnerSpec(p), &undo)
	if err != nil {
		return err
	}
	return c.startWithStdin(ctx, runner, in)
}

// createUndoable creates the container of opts and adds its removal to
// undo, by ID when the daemon returned one and by opts.Name when the create
// failed (see StartRunner).
func (c *Client) createUndoable(ctx context.Context, opts client.ContainerCreateOptions, undo *[]func(context.Context) error) (string, error) {
	id, err := c.create(ctx, opts)
	ref := id
	if err != nil {
		ref = opts.Name
	}
	*undo = append(*undo, func(ctx context.Context) error { return ignoreNotFound(c.removeContainer(ctx, ref)) })
	return id, err
}

// startWithStdin attaches to stdin of container id, starts it, writes the
// two lines and closes stdin. Attaching first means no input is lost.
func (c *Client) startWithStdin(ctx context.Context, id string, in Stdin) error {
	actx := track(ctx)
	att, err := c.engine.ContainerAttach(actx, id, client.ContainerAttachOptions{Stream: true, Stdin: true})
	if err != nil {
		// The Engine API has no attach Op; attaching is part of the start.
		return opError(actx, retry.OpContainerStart, fmt.Errorf("attach stdin: %w", err))
	}
	defer att.Close()
	// The hijacked connection is not tied to ctx after the handshake.
	stop := context.AfterFunc(ctx, att.Close)
	defer stop()

	if err := c.start(ctx, id); err != nil {
		return err
	}
	if _, err := att.Conn.Write(in.bytes()); err != nil {
		return opError(ctx, retry.OpContainerStart, fmt.Errorf("write stdin: %w", ctxErr(ctx, err)))
	}
	if err := att.CloseWrite(); err != nil {
		return opError(ctx, retry.OpContainerStart, fmt.Errorf("close stdin: %w", ctxErr(ctx, err)))
	}
	// The connection closes (deferred) right after CloseWrite. The Engine
	// API has no way to confirm the daemon received attached stdin: for a
	// StdinOnce container without a TTY the daemon ends the attach only
	// when the container stops (moby docker-v29.6.1 daemon/attach.go
	// 194~199), so neither an EOF nor anything else marks delivery. Whether
	// the two lines get lost is checked against a real daemon by
	// TestIntegrationStdin (PLAN M2-8).
	return nil
}

// ctxErr prefers ctx's error over err, which is then only a side effect of
// closing the connection on cancellation.
func ctxErr(ctx context.Context, err error) error {
	if cerr := ctx.Err(); cerr != nil {
		return cerr
	}
	return err
}

func (c *Client) create(ctx context.Context, opts client.ContainerCreateOptions) (string, error) {
	ctx = track(ctx)
	res, err := c.engine.ContainerCreate(ctx, opts)
	if err != nil {
		return "", opError(ctx, retry.OpContainerCreate, fmt.Errorf("%s: %w", opts.Name, err))
	}
	return res.ID, nil
}

func (c *Client) start(ctx context.Context, id string) error {
	ctx = track(ctx)
	_, err := c.engine.ContainerStart(ctx, id, client.ContainerStartOptions{})
	return opError(ctx, retry.OpContainerStart, err)
}

// removeContainer force-removes container id with its anonymous volumes
// (R62, R76). Not-found is returned as a 404 error.
func (c *Client) removeContainer(ctx context.Context, id string) error {
	ctx = track(ctx)
	_, err := c.engine.ContainerRemove(ctx, id, client.ContainerRemoveOptions{Force: true, RemoveVolumes: true})
	return opError(ctx, retry.OpContainerRemove, err)
}

// removeNetwork removes network id. Not-found is returned as a 404 error.
func (c *Client) removeNetwork(ctx context.Context, id string) error {
	ctx = track(ctx)
	_, err := c.engine.NetworkRemove(ctx, id, client.NetworkRemoveOptions{})
	return opError(ctx, retry.OpNetworkRemove, err)
}

// ignoreNotFound treats a removal of something already gone as done (R50).
func ignoreNotFound(err error) error {
	if IsNotFound(err) {
		return nil
	}
	return err
}

// RemoveResources removes the runner container and the BuildKit container
// of runner name, each with its anonymous volumes, then the network (R6,
// R62). It tries every step even if one fails and returns the failures
// joined; each is a *retry.OpError for a removal Op. retry.Classify looks
// through the whole joined error: a rate limit anywhere in it wins, then a
// call deadline anywhere, and otherwise the first status
// it finds (not necessarily the first failure) decides. Removal 409 and
// 403 are transient and not found is dropped, but a mix with another 4xx
// or a 5xx can classify either way, so callers retry per resource through
// the watch and reconcile rules. Resources already gone count as removed.
func (c *Client) RemoveResources(ctx context.Context, name string) error {
	return errors.Join(
		ignoreNotFound(c.removeContainer(ctx, name)),
		ignoreNotFound(c.removeContainer(ctx, BuildKitName(name))),
		ignoreNotFound(c.removeNetwork(ctx, NetworkName(name))),
	)
}

// RemoveContainer force-removes only the runner container of runner name,
// leaving its BuildKit container and network (the deny path and R53). A
// container already gone counts as removed.
func (c *Client) RemoveContainer(ctx context.Context, name string) error {
	return ignoreNotFound(c.removeContainer(ctx, name))
}

// RemoveByID removes one resource that List returned, by its ID. For kind
// KindNetwork it removes network id; for any other kind, an empty or
// unknown one included, it force-removes container id with its anonymous
// volumes (R76). A resource already gone counts as removed (R50); any other
// failure is a *retry.OpError for OpNetworkRemove or OpContainerRemove.
//
// Callers pass Resource.Kind, or KindNetwork for a resource without a
// kind label whose State is empty, since List fills State for every
// container and for no network.
func (c *Client) RemoveByID(ctx context.Context, kind, id string) error {
	if kind == KindNetwork {
		return ignoreNotFound(c.removeNetwork(ctx, id))
	}
	return ignoreNotFound(c.removeContainer(ctx, id))
}

// Wait blocks until container name is not running and returns its exit
// code. It does not time out; cancel ctx to stop waiting. A container that
// no longer exists yields an error for which IsNotFound is true.
func (c *Client) Wait(ctx context.Context, name string) (int64, error) {
	// The request is bound to ctx, so cancelling ctx ends the wait through
	// res.Error; the client sends at most one value on either channel.
	ctx = track(ctx)
	res := c.engine.ContainerWait(ctx, name, client.ContainerWaitOptions{Condition: container.WaitConditionNotRunning})
	select {
	case r := <-res.Result:
		if r.Error != nil && r.Error.Message != "" {
			// The daemon reports a failed wait inside a 200 response, so
			// there is no status to attach.
			return r.StatusCode, opError(context.Background(), retry.OpContainerWait, errors.New(r.Error.Message))
		}
		return r.StatusCode, nil
	case err := <-res.Error:
		return 0, opError(ctx, retry.OpContainerWait, err)
	}
}

// Resource is a container or network that carries the scaler's labels.
type Resource struct {
	// Kind is KindRunner, KindBuildKit or KindNetwork.
	Kind string
	ID   string
	// Name has no leading slash.
	Name string
	// RunnerName is the runner the resource belongs to.
	RunnerName string
	// State is the container state ("created", "running", "exited", ...);
	// it is empty for a network only. The daemon reports a state for every
	// container, labelled or not (moby v2.0.0-beta.31
	// daemon/container/view.go 308, daemon/container/state.go 129~155), so
	// State tells a container from a network when the kind label is
	// missing.
	State string
}

// managedFilter selects the resources labelled with managedBy. The daemon
// matches the value exactly (moby docker-v29.6.1
// daemon/internal/filters/parse.go 145~155).
func managedFilter(managedBy string) client.Filters {
	return make(client.Filters).Add("label", LabelManagedBy+"="+managedBy)
}

// List returns every container (running or not) and network labelled as
// the Client's (for New, the scaler's), containers first.
func (c *Client) List(ctx context.Context) ([]Resource, error) {
	cctx := track(ctx)
	cs, err := c.engine.ContainerList(cctx, client.ContainerListOptions{All: true, Filters: managedFilter(c.managedBy)})
	if err != nil {
		return nil, opError(cctx, retry.OpContainerList, err)
	}
	nctx := track(ctx)
	ns, err := c.engine.NetworkList(nctx, client.NetworkListOptions{Filters: managedFilter(c.managedBy)})
	if err != nil {
		return nil, opError(nctx, retry.OpNetworkList, err)
	}

	out := make([]Resource, 0, len(cs.Items)+len(ns.Items))
	for _, s := range cs.Items {
		var name string
		if len(s.Names) > 0 {
			name = strings.TrimPrefix(s.Names[0], "/")
		}
		out = append(out, Resource{
			Kind:       s.Labels[LabelKind],
			ID:         s.ID,
			Name:       name,
			RunnerName: s.Labels[LabelRunnerName],
			State:      string(s.State),
		})
	}
	for _, n := range ns.Items {
		out = append(out, Resource{
			Kind:       n.Labels[LabelKind],
			ID:         n.ID,
			Name:       n.Name,
			RunnerName: n.Labels[LabelRunnerName],
		})
	}
	return out, nil
}

// Processes returns the command line of each process in container name,
// as the top API lists them (R83).
func (c *Client) Processes(ctx context.Context, name string) ([]string, error) {
	ctx = track(ctx)
	res, err := c.engine.ContainerTop(ctx, name, client.ContainerTopOptions{})
	if err != nil {
		return nil, opError(ctx, retry.OpContainerTop, err)
	}
	col := len(res.Titles) - 1
	for i, t := range res.Titles {
		if t == "CMD" || t == "COMMAND" {
			col = i
			break
		}
	}
	cmds := make([]string, 0, len(res.Processes))
	for _, p := range res.Processes {
		if col >= 0 && col < len(p) {
			cmds = append(cmds, p[col])
		}
	}
	return cmds, nil
}

// ImageExists reports whether image ref is present locally. It never pulls.
func (c *Client) ImageExists(ctx context.Context, ref string) (bool, error) {
	_, ok, err := c.ImageInspect(ctx, ref)
	return ok, err
}

// Info returns the CPU count and memory of the Docker host from one /info
// call (0002 SPEC R201).
func (c *Client) Info(ctx context.Context) (Capacity, error) {
	ctx = track(ctx)
	res, err := c.engine.Info(ctx, client.InfoOptions{})
	if err != nil {
		return Capacity{}, opError(ctx, retry.OpDockerInfo, err)
	}
	return Capacity{NCPU: res.Info.NCPU, MemTotal: res.Info.MemTotal}, nil
}

// Ping checks that the daemon answers and negotiates the API version.
func (c *Client) Ping(ctx context.Context) error {
	ctx = track(ctx)
	_, err := c.engine.Ping(ctx, client.PingOptions{NegotiateAPIVersion: true})
	return opError(ctx, retry.OpDockerPing, err)
}
