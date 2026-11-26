//go:build integration

// Integration tests against the local Docker daemon (PLAN M2-8). They create
// real containers and networks and remove them before returning. Run them
// only with the user's approval:
//
//	SCALER_IT_RUNNER_IMAGE=sha256:... \
//	SCALER_IT_BUILDKIT_IMAGE=moby/buildkit:<version>-rootless@sha256:... \
//	go test -tags integration -run Integration -v ./internal/docker/
//
// The JIT config and token are random test values held in memory; the
// tests compare against them but never print them.
//
// Every resource the tests create carries managed-by=itManagedBy instead of
// the scaler's value, and the daemon matches label values exactly, so a
// scaler running on the same host neither lists nor removes them (R16, R17)
// and the tests never touch the scaler's. Each test checks at the end that
// nothing it created is left, anonymous BuildKit volumes included.
package docker

import (
	"archive/tar"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/client"

	"github.com/dewevrei/actions-runner-scaler/internal/config"
)

const (
	itRunnerImageEnv   = "SCALER_IT_RUNNER_IMAGE"
	itBuildKitImageEnv = "SCALER_IT_BUILDKIT_IMAGE"
	// buildKitVolume is the anonymous volume the rootless image declares.
	buildKitVolume = "/home/user/.local/share/buildkit"
	// itManagedBy is the managed-by value of every test resource.
	itManagedBy = ManagedBy + "-it"
)

type itEnv struct {
	engine *client.Client
	c      *Client
	runner string
	bk     string

	// owners are the runner names this test created resources for.
	owners []string
	// volumes are the anonymous BuildKit volumes this test created.
	volumes []string
}

func randomHex(t *testing.T, n int) string {
	t.Helper()
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(b)
}

func newITEnv(t *testing.T) *itEnv {
	t.Helper()
	runner, bk := os.Getenv(itRunnerImageEnv), os.Getenv(itBuildKitImageEnv)
	if runner == "" || bk == "" {
		t.Skipf("set %s and %s", itRunnerImageEnv, itBuildKitImageEnv)
	}
	engine, err := NewEngine(DefaultHost)
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	t.Cleanup(func() { engine.Close() })
	c := New(engine)
	c.managedBy = itManagedBy
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := c.Ping(ctx); err != nil {
		t.Fatalf("Ping: %v", err)
	}
	env := &itEnv{engine: engine, c: c, runner: runner, bk: bk}
	// Registered before any resource exists, so it runs after every other
	// cleanup of the test.
	t.Cleanup(func() { env.checkCleanedUp(t) })
	return env
}

// checkCleanedUp fails the test for each resource it created that is still
// there, and removes it. Test resources of other runs are only logged.
func (env *itEnv) checkCleanedUp(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	left, err := env.c.List(ctx)
	if err != nil {
		t.Errorf("final List: %v", err)
	}
	for _, r := range left {
		if !slices.Contains(env.owners, r.RunnerName) {
			t.Logf("test resource of another run left: %s %s", r.Kind, r.Name)
			continue
		}
		t.Errorf("left after cleanup: %s %s", r.Kind, r.Name)
		if r.Kind == KindNetwork {
			err = ignoreNotFound(env.c.removeNetwork(ctx, r.ID))
		} else {
			err = ignoreNotFound(env.c.removeContainer(ctx, r.ID))
		}
		if err != nil {
			t.Errorf("remove %s %s: %v", r.Kind, r.Name, err)
		}
	}
	for _, v := range env.volumes {
		if _, err := env.engine.VolumeInspect(ctx, v, client.VolumeInspectOptions{}); !IsNotFound(err) {
			t.Errorf("BuildKit volume %s left after cleanup: %v (R76)", v, err)
		}
	}
}

// startTestRunner starts a runner with fake secrets and registers removal.
func startTestRunner(t *testing.T, env *itEnv, jobCheckURL string) (Params, string, string) {
	t.Helper()
	limits := config.Default()
	runnerLimits, buildKitLimits := ContainerLimits(limits.Runner), ContainerLimits(limits.BuildKit)
	p := Params{
		Name:          "it-" + randomHex(t, 4),
		RunnerImage:   env.runner,
		BuildKitImage: env.bk,
		JobCheckURL:   jobCheckURL,
		Runner:        runnerLimits,
		BuildKit:      buildKitLimits,
	}
	jit, token := "it-jit-"+randomHex(t, 16), randomHex(t, 32)
	env.owners = append(env.owners, p.Name)

	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		if err := env.c.RemoveResources(ctx, p.Name); err != nil {
			t.Errorf("cleanup RemoveResources(%s): %v", p.Name, err)
		}
		left, err := env.c.List(ctx)
		if err != nil {
			t.Errorf("cleanup List: %v", err)
		}
		for _, r := range left {
			if r.RunnerName == p.Name {
				t.Errorf("left after cleanup: %+v", r)
			}
		}
	})

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := env.c.StartRunner(ctx, p, NewStdin(jit, token)); err != nil {
		t.Fatalf("StartRunner: %v", err)
	}
	for _, m := range inspect(t, env, BuildKitName(p.Name)).Mounts {
		if m.Type == mount.TypeVolume {
			env.volumes = append(env.volumes, m.Name)
		}
	}
	return p, jit, token
}

func inspect(t *testing.T, env *itEnv, name string) container.InspectResponse {
	t.Helper()
	res, err := env.engine.ContainerInspect(context.Background(), name, client.ContainerInspectOptions{})
	if err != nil {
		t.Fatalf("inspect %s: %v", name, err)
	}
	return res.Container
}

func imageEnv(t *testing.T, env *itEnv, ref string) []string {
	t.Helper()
	res, err := env.engine.ImageInspect(context.Background(), ref)
	if err != nil {
		t.Fatalf("image inspect: %v", err)
	}
	if res.Architecture != "arm64" || res.Os != "linux" {
		t.Errorf("image %s is %s/%s, want linux/arm64 (R35)", ref, res.Os, res.Architecture)
	}
	if res.Config == nil {
		return nil
	}
	return res.Config.Env
}

// assertNoSecret fails if any inspect-visible value holds a secret (R36).
func assertNoSecret(t *testing.T, what string, values []string, secrets ...string) {
	t.Helper()
	for _, v := range values {
		for _, s := range secrets {
			if strings.Contains(v, s) {
				t.Errorf("%s holds a secret value (R36)", what)
			}
		}
	}
}

func labelValues(m map[string]string) []string {
	var out []string
	for k, v := range m {
		out = append(out, k, v)
	}
	return out
}

// normalizeCaps returns caps as the daemon understands them: upper case,
// with the CAP_ prefix (except ALL), sorted and without duplicates. Inspect
// returns HostConfig.CapAdd as it was stored. moby client v0.6.2
// normalizes on create (container_create.go 51, 79~118) and the daemon only
// validates (docker-v29.6.1 daemon/container.go 299~307), but another
// client could send the short form, so both sides are compared normalized.
func normalizeCaps(caps []string) []string {
	out := make([]string, 0, len(caps))
	for _, c := range caps {
		c = strings.ToUpper(c)
		if c != "ALL" && !strings.HasPrefix(c, "CAP_") {
			c = "CAP_" + c
		}
		out = append(out, c)
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// readFile copies one file out of a container, running or stopped.
func readFile(t *testing.T, env *itEnv, name, path string) (string, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	res, err := env.engine.CopyFromContainer(ctx, name, client.CopyFromContainerOptions{SourcePath: path})
	if err != nil {
		return "", err
	}
	defer res.Content.Close()
	tr := tar.NewReader(res.Content)
	if _, err := tr.Next(); err != nil {
		return "", err
	}
	b, err := io.ReadAll(tr)
	return string(b), err
}

func TestIntegrationImages(t *testing.T) {
	env := newITEnv(t)
	ctx := context.Background()
	for _, ref := range []string{env.runner, env.bk} {
		ok, err := env.c.ImageExists(ctx, ref)
		if err != nil || !ok {
			t.Errorf("ImageExists(%s) = %v, %v; want true", ref, ok, err)
		}
		imageEnv(t, env, ref)
	}
	ok, err := env.c.ImageExists(ctx, "sha256:"+strings.Repeat("0", 64))
	if err != nil || ok {
		t.Errorf("ImageExists(missing) = %v, %v; want false, nil", ok, err)
	}
	info, err := env.c.Info(ctx)
	if err != nil || info.MemTotal <= 0 || info.NCPU <= 0 {
		t.Errorf("Info = %+v, %v", info, err)
	}
	t.Logf("Info = %+v", info)
}

func TestIntegrationIsolation(t *testing.T) {
	env := newITEnv(t)
	p, jit, token := startTestRunner(t, env, "http://host.docker.internal:8765")
	limits := config.Default()
	runnerLimits, buildKitLimits := ContainerLimits(limits.Runner), ContainerLimits(limits.BuildKit)

	t.Run("runner", func(t *testing.T) {
		got := inspect(t, env, p.Name)
		cfg, hc := got.Config, got.HostConfig

		// R70: the scaler adds exactly three variables to the image's.
		var added []string
		base := imageEnv(t, env, env.runner)
		for _, e := range cfg.Env {
			if !slices.Contains(base, e) {
				added = append(added, strings.SplitN(e, "=", 2)[0])
			}
		}
		slices.Sort(added)
		if want := []string{EnvBuildKitHost, EnvJobCheckURL, EnvRunnerName}; !slices.Equal(added, want) {
			t.Errorf("added env names = %q, want %q (R70)", added, want)
		}
		for _, e := range cfg.Env {
			name := strings.SplitN(e, "=", 2)[0]
			if strings.HasPrefix(name, "AWS_") || name == "KUBECONFIG" || name == "ACTIONS_RUNNER_INPUT_JITCONFIG" {
				t.Errorf("env has %s (R12, R36)", name)
			}
		}
		assertNoSecret(t, "runner Env", cfg.Env, jit, token)
		assertNoSecret(t, "runner Cmd/Entrypoint/Args", append(append(append([]string{got.Path}, got.Args...), cfg.Cmd...), cfg.Entrypoint...), jit, token)
		assertNoSecret(t, "runner Labels", labelValues(cfg.Labels), jit, token)

		if cfg.User != "runner" || hc.Privileged || !slices.Equal(normalizeCaps(hc.CapDrop), []string{"ALL"}) || len(hc.CapAdd) != 0 {
			t.Errorf("runner user/privileged/caps = %q, %v, %v, %v (R32)", cfg.User, hc.Privileged, hc.CapDrop, hc.CapAdd)
		}
		if hc.Init == nil || !*hc.Init {
			t.Errorf("runner Init = %v, want true", hc.Init)
		}
		if !slices.Equal(hc.SecurityOpt, []string{"no-new-privileges"}) {
			t.Errorf("runner SecurityOpt = %q (R32, R72)", hc.SecurityOpt)
		}
		if len(got.Mounts) != 0 || len(hc.Binds) != 0 {
			t.Errorf("runner mounts = %+v, binds = %q (R33)", got.Mounts, hc.Binds)
		}
		if hc.NanoCPUs != int64(math.Round(runnerLimits.CPUs*1e9)) || hc.Memory != int64(runnerLimits.Memory) ||
			hc.MemorySwap != hc.Memory || hc.PidsLimit == nil || *hc.PidsLimit != runnerLimits.Pids {
			t.Errorf("runner limits = %d CPU, %d mem, %d swap, %v pids (R34)", hc.NanoCPUs, hc.Memory, hc.MemorySwap, hc.PidsLimit)
		}
		if len(hc.PortBindings) != 0 || hc.PublishAllPorts {
			t.Errorf("runner publishes ports: %v", hc.PortBindings)
		}
		if nets := got.NetworkSettings.Networks; len(nets) != 1 || nets[NetworkName(p.Name)] == nil {
			t.Errorf("runner networks = %v, want only %s (R75)", nets, NetworkName(p.Name))
		}
	})

	t.Run("buildkit", func(t *testing.T) {
		got := inspect(t, env, BuildKitName(p.Name))
		cfg, hc := got.Config, got.HostConfig
		if cfg.User == "" || cfg.User == "root" || strings.HasPrefix(cfg.User, "0") {
			t.Errorf("BuildKit user = %q (R73)", cfg.User)
		}
		if hc.Privileged || !slices.Equal(normalizeCaps(hc.CapDrop), []string{"ALL"}) ||
			!slices.Equal(normalizeCaps(hc.CapAdd), normalizeCaps([]string{"SETUID", "SETGID"})) {
			t.Errorf("BuildKit privileged/caps = %v, %v, %v (R72)", hc.Privileged, hc.CapDrop, hc.CapAdd)
		}
		if !slices.Equal(hc.SecurityOpt, []string{"seccomp=unconfined"}) {
			t.Errorf("BuildKit SecurityOpt = %q (R72)", hc.SecurityOpt)
		}
		if hc.NanoCPUs != int64(math.Round(buildKitLimits.CPUs*1e9)) || hc.Memory != int64(buildKitLimits.Memory) ||
			hc.MemorySwap != hc.Memory || hc.PidsLimit == nil || *hc.PidsLimit != buildKitLimits.Pids {
			t.Errorf("BuildKit limits = %d CPU, %d mem, %d swap, %v pids (R74)", hc.NanoCPUs, hc.Memory, hc.MemorySwap, hc.PidsLimit)
		}
		if len(hc.PortBindings) != 0 || hc.PublishAllPorts || len(hc.Binds) != 0 {
			t.Errorf("BuildKit ports %v or binds %q (R73)", hc.PortBindings, hc.Binds)
		}
		// R76: one anonymous volume at the image's VOLUME.
		if len(got.Mounts) != 1 || got.Mounts[0].Type != mount.TypeVolume || got.Mounts[0].Destination != buildKitVolume {
			t.Fatalf("BuildKit mounts = %+v, want one volume at %s (R76)", got.Mounts, buildKitVolume)
		}
		vol := got.Mounts[0].Name
		if nets := got.NetworkSettings.Networks; len(nets) != 1 || nets[NetworkName(p.Name)] == nil {
			t.Errorf("BuildKit networks = %v, want only %s (R75)", nets, NetworkName(p.Name))
		}

		// The top API answers for a running container (R83's call).
		procs, err := env.c.Processes(context.Background(), BuildKitName(p.Name))
		if err != nil || len(procs) == 0 {
			t.Errorf("Processes = %q, %v", procs, err)
		}
		t.Logf("BuildKit processes: %q", procs)

		// Removal takes the anonymous volume with it (R62, R76).
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		if err := env.c.RemoveResources(ctx, p.Name); err != nil {
			t.Fatalf("RemoveResources: %v", err)
		}
		if _, err := env.engine.VolumeInspect(ctx, vol, client.VolumeInspectOptions{}); !IsNotFound(err) {
			t.Errorf("volume after removal: %v, want not found (R76)", err)
		}
		// A second removal finds nothing and still succeeds (R50).
		if err := env.c.RemoveResources(ctx, p.Name); err != nil {
			t.Errorf("second RemoveResources: %v", err)
		}
	})
}

// TestIntegrationStdin checks the two stdin lines reach the entrypoint and
// that it records the remote builder without a daemon (concern 7). It polls
// for the files the entrypoint writes and does not wait for the runner to
// exit: how long a runner with a fake JIT config keeps running is unknown.
// The copy API reads files from a running or a stopped container alike.
func TestIntegrationStdin(t *testing.T) {
	env := newITEnv(t)
	p, _, token := startTestRunner(t, env, "http://host.docker.internal:8765")

	got := pollFile(t, env, p.Name, "/home/runner/.scaler/token", 1)
	if got != token+"\n" {
		t.Error("token file does not hold the second stdin line")
	}
	endpoint := pollFile(t, env, p.Name, "/home/runner/.scaler/endpoint", 2)
	if want := p.JobCheckURL + "\n" + p.Name + "\n"; endpoint != want {
		t.Errorf("endpoint file = %q, want %q", endpoint, want)
	}

	// The builder record. Its directory is buildx's documented state
	// directory: $BUILDX_CONFIG, else $DOCKER_CONFIG/buildx, else
	// ~/.docker/buildx (docs.docker.com/build/building/variables/,
	// BUILDX_CONFIG). Where in it buildx keeps a builder (instances/<name>
	// in buildx's store, from memory) and its file format are unconfirmed,
	// so the check looks for the endpoint in any file under it.
	dir := buildxDir(inspect(t, env, p.Name).Config.Env)
	host := BuildKitHost(p.Name)
	var found string
	deadline := time.Now().Add(2 * time.Minute)
	for found == "" && time.Now().Before(deadline) {
		found = findInDir(t, env, p.Name, dir, host)
		if found == "" {
			time.Sleep(time.Second)
		}
	}
	if found == "" {
		t.Errorf("no file under %s holds the builder endpoint %s (concern 7)", dir, host)
	} else {
		t.Logf("buildx builder recorded in %s", found)
	}
	t.Logf("runner state at the end: %s", inspect(t, env, p.Name).State.Status)
}

// pollFile reads path from container name until it holds lines complete
// lines, for up to two minutes. The entrypoint creates each file with ">"
// before printf fills it, so an existing file may still be empty or, if the
// write is split, hold only its first line.
func pollFile(t *testing.T, env *itEnv, name, path string, lines int) string {
	t.Helper()
	deadline := time.Now().Add(2 * time.Minute)
	for {
		got, err := readFile(t, env, name, path)
		if err == nil && strings.Count(got, "\n") >= lines && strings.HasSuffix(got, "\n") {
			return got
		}
		if err == nil {
			if time.Now().After(deadline) {
				t.Fatalf("read %s: fewer than %d complete lines after two minutes", path, lines)
			}
			time.Sleep(time.Second)
			continue
		}
		if !IsNotFound(err) || time.Now().After(deadline) {
			t.Fatalf("read %s: %v", path, err)
		}
		time.Sleep(time.Second)
	}
}

// buildxDir returns the buildx state directory for a container environment
// env, in buildx's documented lookup order, for the runner user's home.
func buildxDir(env []string) string {
	vars := map[string]string{}
	for _, e := range env {
		if k, v, ok := strings.Cut(e, "="); ok {
			vars[k] = v
		}
	}
	switch {
	case vars["BUILDX_CONFIG"] != "":
		return vars["BUILDX_CONFIG"]
	case vars["DOCKER_CONFIG"] != "":
		return vars["DOCKER_CONFIG"] + "/buildx"
	}
	return "/home/runner/.docker/buildx"
}

// findInDir copies dir out of container name and returns the name of the
// first regular file whose content holds s, or "" if none does or dir does
// not exist yet.
func findInDir(t *testing.T, env *itEnv, name, dir, s string) string {
	t.Helper()
	res, err := env.engine.CopyFromContainer(context.Background(), name, client.CopyFromContainerOptions{SourcePath: dir})
	if err != nil {
		if !IsNotFound(err) {
			t.Logf("copy %s: %v", dir, err)
		}
		return ""
	}
	defer res.Content.Close()
	tr := tar.NewReader(res.Content)
	for {
		h, err := tr.Next()
		if err != nil {
			if !errors.Is(err, io.EOF) {
				t.Logf("read %s: %v", dir, err)
			}
			return ""
		}
		if h.Typeflag != tar.TypeReg {
			continue
		}
		b, err := io.ReadAll(io.LimitReader(tr, 1<<20))
		if err == nil && strings.Contains(string(b), s) {
			return h.Name
		}
	}
}

// TestIntegrationHostAccess checks that a container on a runner network
// reaches a host loopback listener through host.docker.internal (concern
// 10) and that the image's curl reads headers from stdin with -H @-.
func TestIntegrationHostAccess(t *testing.T) {
	env := newITEnv(t)

	hit := make(chan string, 1)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case hit <- r.Header.Get("X-Scaler-Test"):
		default:
		}
		_, _ = io.WriteString(w, "ok")
	}), ReadHeaderTimeout: 5 * time.Second}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	_, port, _ := net.SplitHostPort(ln.Addr().String())
	url := "http://" + net.JoinHostPort(jobCheckHost, port) + "/it"

	p, _, _ := startTestRunner(t, env, "http://"+net.JoinHostPort(jobCheckHost, port))

	code := runProbe(t, env, p.Name, NetworkName(p.Name),
		`printf 'X-Scaler-Test: yes\n' | /usr/bin/curl -q --noproxy '*' -sS --max-time 10 -H @- -o /dev/null "$0"`, url)
	select {
	case h := <-hit:
		if h != "yes" {
			t.Errorf("header from stdin = %q, want yes (curl -H @-)", h)
		}
	default:
		t.Errorf("host listener got no request (concern 10); probe exit code %d", code)
	}
}

// runProbe runs a bash script in a throwaway container of the runner image
// on network, with the runner's restrictions, and returns its exit code.
// args become $0, $1, ... of the script. The container carries the test
// labels of runner owner (kind "probe") and is removed by t.Cleanup, which
// runs before the owner's cleanup removes the network.
func runProbe(t *testing.T, env *itEnv, owner, network, script string, args ...string) int64 {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	name := owner + "-probe-" + randomHex(t, 3)
	useInit := true
	_, err := env.engine.ContainerCreate(ctx, client.ContainerCreateOptions{
		Name:     name,
		Platform: platform(),
		Config: &container.Config{
			Image:      env.runner,
			User:       runnerUser,
			Entrypoint: append([]string{"/bin/bash", "-c", script}, args...),
			Labels:     labels(itManagedBy, owner, "probe"),
		},
		HostConfig: &container.HostConfig{
			NetworkMode: container.NetworkMode(network),
			CapDrop:     []string{"ALL"},
			SecurityOpt: []string{"no-new-privileges"},
			Init:        &useInit,
		},
	})
	if err != nil {
		t.Fatalf("create probe: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := env.c.RemoveContainer(ctx, name); err != nil {
			t.Errorf("remove probe %s: %v", name, err)
		}
	})
	if _, err := env.engine.ContainerStart(ctx, name, client.ContainerStartOptions{}); err != nil {
		t.Fatalf("start probe: %v", err)
	}
	code, err := env.c.Wait(ctx, name)
	if err != nil {
		t.Fatalf("wait probe: %v", err)
	}
	return code
}

// Probe scripts for a TCP connection to $0 on the BuildKit port, through
// bash's /dev/tcp. timeout(1) bounds a connection the firewall drops
// silently (exit 124); a refusal or a name that does not resolve exits 1.
var (
	connectOnce = fmt.Sprintf(`timeout 5 bash -c 'exec 3<>"/dev/tcp/$0/%d"' "$0"`, buildKitPort)
	// connectRetry waits up to about a minute for buildkitd to listen.
	connectRetry = `for i in $(seq 30); do ` + connectOnce + ` 2>/dev/null && exit 0; sleep 1; done; exit 1`
)

// TestIntegrationNetworkIsolation checks R75: a container on another
// runner's network cannot open a TCP connection to this runner's BuildKit
// on port 1234, by name or by IP address. Probes on each runner's own
// network reaching its BuildKit are the positive controls; without them a
// failed connection could be a broken probe or a buildkitd not listening.
// The A-to-A controls run again after the B-to-A probes, so a BuildKit
// that stopped listening in between cannot pass for isolation.
func TestIntegrationNetworkIsolation(t *testing.T) {
	env := newITEnv(t)
	a, _, _ := startTestRunner(t, env, "http://host.docker.internal:8765")
	b, _, _ := startTestRunner(t, env, "http://host.docker.internal:8765")
	ctx := context.Background()

	// The network settings as the daemon reports them. Inspect's Options is
	// the driver option map the network was created with, request options
	// plus the daemon's default-network-opts (moby docker-v29.6.1
	// daemon/network.go 315~324 and 355, 753; daemon/libnetwork/network.go
	// 880~891, 1754~1763). That Docker Desktop's daemon behaves the same is
	// not confirmed; the probes below are the actual R75 check either way.
	for _, p := range []Params{a, b} {
		res, err := env.engine.NetworkInspect(ctx, NetworkName(p.Name), client.NetworkInspectOptions{})
		if err != nil {
			t.Fatalf("inspect network %s: %v", NetworkName(p.Name), err)
		}
		n := res.Network
		if n.Driver != networkDriver || n.Internal || !n.EnableIPv4 || n.EnableIPv6 ||
			n.Options[gatewayModeIPv4] != "nat" || n.Options[enableICC] != "true" {
			t.Errorf("network %s: driver %q, internal %v, IPv4 %v, IPv6 %v, options %v; want bridge, false, true, false, gateway mode nat, ICC true (R75)",
				n.Name, n.Driver, n.Internal, n.EnableIPv4, n.EnableIPv6, n.Options)
		}
	}

	bkA := inspect(t, env, BuildKitName(a.Name))
	ep := bkA.NetworkSettings.Networks[NetworkName(a.Name)]
	if ep == nil || !ep.IPAddress.IsValid() {
		t.Fatalf("BuildKit %s has no address on %s: %+v", bkA.Name, NetworkName(a.Name), bkA.NetworkSettings.Networks)
	}
	ipA := ep.IPAddress.String()

	// Positive controls: each network's probe reaches its own BuildKit.
	type control struct{ what, owner, network, target string }
	controlsA := []control{
		{"A to A BuildKit by name", a.Name, NetworkName(a.Name), BuildKitName(a.Name)},
		{"A to A BuildKit by IP", a.Name, NetworkName(a.Name), ipA},
	}
	controls := append(slices.Clone(controlsA), control{"B to B BuildKit by name", b.Name, NetworkName(b.Name), BuildKitName(b.Name)})
	for _, c := range controls {
		if code := runProbe(t, env, c.owner, c.network, connectRetry, c.target); code != 0 {
			t.Fatalf("control %s: exit %d, want 0; the isolation checks would mean nothing", c.what, code)
		}
	}

	// R75: from B's network, A's BuildKit is out of reach.
	for _, target := range []string{BuildKitName(a.Name), ipA} {
		code := runProbe(t, env, b.Name, NetworkName(b.Name), connectOnce, target)
		t.Logf("B to A BuildKit %s:%d: exit %d", target, buildKitPort, code)
		if code == 0 {
			t.Errorf("a container on %s connected to %s:%d (R75)", NetworkName(b.Name), target, buildKitPort)
		}
	}

	// A's BuildKit still listens, so the failures above were isolation.
	for _, c := range controlsA {
		if code := runProbe(t, env, c.owner, c.network, connectOnce, c.target); code != 0 {
			t.Errorf("control %s after the isolation checks: exit %d, want 0; they may not have tested isolation", c.what, code)
		}
	}
}

func TestIntegrationCheckExposure(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	t.Logf("Docker daemon on %s: %v (R79)", ExposedAddr, CheckExposure(ctx, nil))
}

func TestIntegrationRemoveMissing(t *testing.T) {
	env := newITEnv(t)
	ctx := context.Background()
	name := "it-missing-" + randomHex(t, 4)
	if err := env.c.RemoveResources(ctx, name); err != nil {
		t.Errorf("RemoveResources(missing) = %v, want nil (R50)", err)
	}
	if err := env.c.RemoveContainer(ctx, name); err != nil {
		t.Errorf("RemoveContainer(missing) = %v, want nil (R50)", err)
	}
	_, err := env.c.Wait(ctx, name)
	if !IsNotFound(err) {
		t.Errorf("Wait(missing) = %v, want not found", err)
	}
	var se *StatusError
	if !errors.As(err, &se) || se.Status != http.StatusNotFound {
		t.Errorf("Wait(missing) status = %v, want 404", err)
	}
}
