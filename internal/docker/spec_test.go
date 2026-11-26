package docker

import (
	"math"
	"reflect"
	"testing"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/client"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/dewevrei/actions-runner-scaler/internal/config"
)

const (
	testRunner        = "linux-arm64-deploy-0123abcd"
	testRunnerImage   = "sha256:1111111111111111111111111111111111111111111111111111111111111111"
	testBuildKitImage = "moby/buildkit:v0.35.1-rootless@sha256:2222222222222222222222222222222222222222222222222222222222222222"
	testJobCheckURL   = "http://host.docker.internal:8765"
)

// testParams uses the default limits from the configuration.
func testParams() Params {
	cfg := config.Default()
	runner, buildKit := ContainerLimits(cfg.Runner), ContainerLimits(cfg.BuildKit)
	return Params{
		Name:          testRunner,
		RunnerImage:   testRunnerImage,
		BuildKitImage: testBuildKitImage,
		JobCheckURL:   testJobCheckURL,
		Runner:        runner,
		BuildKit:      buildKit,
	}
}

func ptr[T any](v T) *T { return &v }

type field struct {
	name      string
	got, want any
}

func checkFields(t *testing.T, fields []field) {
	t.Helper()
	for _, f := range fields {
		if !reflect.DeepEqual(f.got, f.want) {
			t.Errorf("%s = %#v, want %#v", f.name, f.got, f.want)
		}
	}
}

func TestNames(t *testing.T) {
	checkFields(t, []field{
		{"NetworkName", NetworkName("r"), "r-net"},
		{"BuildKitName", BuildKitName("r"), "r-buildkit"},
		{"BuildKitHost", BuildKitHost("r"), "tcp://r-buildkit:1234"},
	})
}

// TestNetworkSpec checks the network side of R75: a user-defined bridge
// whose gateway mode keeps other networks away from unpublished ports.
func TestNetworkSpec(t *testing.T) {
	got := NetworkSpec(testParams())
	checkFields(t, []field{
		{"Driver (R75)", got.Driver, "bridge"},
		{"Labels", got.Labels, map[string]string{"managed-by": "actions-runner-scaler", "runner-name": testRunner, "kind": "network"}},
		// "nat": other networks reach only published ports, and there are
		// none (R73). "routed" or "nat-unprotected" would open direct routing.
		// enable_icc "true": the runner reaches BuildKit on this network,
		// whatever the daemon's default-network-opts say (R75).
		{"Options (R75 gateway mode, ICC)", got.Options, map[string]string{
			"com.docker.network.bridge.gateway_mode_ipv4": "nat",
			"com.docker.network.bridge.enable_icc":        "true",
		}},
		// Stated, so a daemon default of enable_ipv4=false cannot apply.
		{"EnableIPv4", got.EnableIPv4, ptr(true)},
		{"EnableIPv6", got.EnableIPv6, ptr(false)},
		// Not internal: BuildKit needs a route out for image pulls and pushes.
		{"Internal", got.Internal, false},
		{"Attachable", got.Attachable, false},
		{"ConfigFrom", got.ConfigFrom, ""},
	})
	// Nothing else may be set.
	want := client.NetworkCreateOptions{Driver: got.Driver, EnableIPv4: got.EnableIPv4, EnableIPv6: got.EnableIPv6, Options: got.Options, Labels: got.Labels}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("NetworkSpec has extra settings:\n got %#v\nwant %#v", got, want)
	}
}

// TestContainersOnlyOnOwnNetwork checks the container side of R75: both
// containers join only the runner's network (NetworkMode, and no
// NetworkingConfig that could add another), publish no port, and the
// runner reaches BuildKit by its name on that network. The DeepEqual checks
// in TestBuildKitSpec and TestRunnerSpec also rule out any other setting.
func TestContainersOnlyOnOwnNetwork(t *testing.T) {
	p := testParams()
	for _, spec := range []client.ContainerCreateOptions{BuildKitSpec(p), RunnerSpec(p)} {
		hc := spec.HostConfig
		checkFields(t, []field{
			{spec.Name + " NetworkMode", hc.NetworkMode, container.NetworkMode(NetworkName(p.Name))},
			{spec.Name + " NetworkingConfig", spec.NetworkingConfig == nil, true},
			{spec.Name + " PortBindings", len(hc.PortBindings), 0},
			{spec.Name + " PublishAllPorts", hc.PublishAllPorts, false},
			{spec.Name + " ExposedPorts", len(spec.Config.ExposedPorts), 0},
			{spec.Name + " Links", len(hc.Links), 0},
			{spec.Name + " ExtraHosts", len(hc.ExtraHosts), 0},
		})
		if hc.NetworkMode.IsDefault() || hc.NetworkMode.IsBridge() || hc.NetworkMode.IsHost() || hc.NetworkMode.IsContainer() {
			t.Errorf("%s NetworkMode %q is not a user-defined network", spec.Name, hc.NetworkMode)
		}
	}
	if got, want := BuildKitHost(p.Name), "tcp://"+BuildKitSpec(p).Name+":1234"; got != want {
		t.Errorf("BUILDKIT_HOST = %q, want the BuildKit container name %q", got, want)
	}
}

func TestBuildKitSpec(t *testing.T) {
	got := BuildKitSpec(testParams())
	cfg, hc := got.Config, got.HostConfig
	checkFields(t, []field{
		{"Name", got.Name, testRunner + "-buildkit"},
		{"Image (option)", got.Image, ""},
		{"Platform", got.Platform, &ocispec.Platform{OS: "linux", Architecture: "arm64"}},
		{"NetworkingConfig", got.NetworkingConfig == nil, true},

		{"Config.Image", cfg.Image, testBuildKitImage},
		{"Config.User (R73 non-root)", cfg.User, "1000:1000"},
		{"Config.Cmd", cfg.Cmd, []string{"--addr", "tcp://0.0.0.0:1234", "--oci-worker-no-process-sandbox"}},
		{"Config.Entrypoint", cfg.Entrypoint, []string(nil)},
		{"Config.Env", cfg.Env, []string(nil)},
		{"Config.Labels", cfg.Labels, map[string]string{"managed-by": "actions-runner-scaler", "runner-name": testRunner, "kind": "buildkit"}},
		{"Config.Volumes", cfg.Volumes, map[string]struct{}(nil)},
		{"Config.ExposedPorts", len(cfg.ExposedPorts), 0},
		{"Config.OpenStdin", cfg.OpenStdin, false},

		{"HostConfig.NetworkMode (R75)", hc.NetworkMode, container.NetworkMode(testRunner + "-net")},
		{"HostConfig.Privileged (R72)", hc.Privileged, false},
		{"HostConfig.CapDrop (R72)", hc.CapDrop, []string{"ALL"}},
		{"HostConfig.CapAdd (R72)", hc.CapAdd, []string{"SETUID", "SETGID"}},
		{"HostConfig.SecurityOpt (R72)", hc.SecurityOpt, []string{"seccomp=unconfined"}},
		{"HostConfig.Init", hc.Init, (*bool)(nil)},
		{"HostConfig.Binds (R73)", hc.Binds, []string(nil)},
		{"HostConfig.Mounts (R73, R76)", hc.Mounts, []mount.Mount(nil)},
		{"HostConfig.VolumesFrom", hc.VolumesFrom, []string(nil)},
		{"HostConfig.PortBindings (R73)", len(hc.PortBindings), 0},
		{"HostConfig.PublishAllPorts (R73)", hc.PublishAllPorts, false},
		{"HostConfig.NanoCPUs (R74)", hc.NanoCPUs, int64(2_000_000_000)},
		{"HostConfig.Memory (R74)", hc.Memory, int64(2 << 30)},
		{"HostConfig.MemorySwap (R74 no swap)", hc.MemorySwap, int64(2 << 30)},
		{"HostConfig.PidsLimit (R74)", hc.PidsLimit, ptr(int64(1024))},
		{"HostConfig.AutoRemove", hc.AutoRemove, false},
	})

	// Every other setting stays at the daemon default (R72: seccomp is the
	// only changed security option).
	wantCfg := container.Config{Image: cfg.Image, User: cfg.User, Cmd: cfg.Cmd, Labels: cfg.Labels}
	if !reflect.DeepEqual(*cfg, wantCfg) {
		t.Errorf("BuildKit Config has extra settings:\n got %#v\nwant %#v", *cfg, wantCfg)
	}
	wantHC := container.HostConfig{
		NetworkMode: hc.NetworkMode,
		CapDrop:     hc.CapDrop,
		CapAdd:      hc.CapAdd,
		SecurityOpt: hc.SecurityOpt,
		Resources:   container.Resources{NanoCPUs: hc.NanoCPUs, Memory: hc.Memory, MemorySwap: hc.MemorySwap, PidsLimit: hc.PidsLimit},
	}
	if !reflect.DeepEqual(*hc, wantHC) {
		t.Errorf("BuildKit HostConfig has extra settings:\n got %#v\nwant %#v", *hc, wantHC)
	}
}

func TestRunnerSpec(t *testing.T) {
	got := RunnerSpec(testParams())
	cfg, hc := got.Config, got.HostConfig
	checkFields(t, []field{
		{"Name", got.Name, testRunner},
		{"Image (option)", got.Image, ""},
		{"Platform (R35)", got.Platform, &ocispec.Platform{OS: "linux", Architecture: "arm64"}},
		{"NetworkingConfig", got.NetworkingConfig == nil, true},

		{"Config.Image", cfg.Image, testRunnerImage},
		{"Config.User (R32)", cfg.User, "runner"},
		{"Config.Env (R70)", cfg.Env, []string{
			"SCALER_JOBCHECK_URL=" + testJobCheckURL,
			"SCALER_RUNNER_NAME=" + testRunner,
			"BUILDKIT_HOST=tcp://" + testRunner + "-buildkit:1234",
		}},
		{"Config.Cmd (R36)", cfg.Cmd, []string(nil)},
		{"Config.Entrypoint (R36)", cfg.Entrypoint, []string(nil)},
		{"Config.Labels (R36)", cfg.Labels, map[string]string{"managed-by": "actions-runner-scaler", "runner-name": testRunner, "kind": "runner"}},
		{"Config.AttachStdin", cfg.AttachStdin, true},
		{"Config.OpenStdin", cfg.OpenStdin, true},
		{"Config.StdinOnce", cfg.StdinOnce, true},
		{"Config.AttachStdout", cfg.AttachStdout, false},
		{"Config.AttachStderr", cfg.AttachStderr, false},
		{"Config.Tty", cfg.Tty, false},
		{"Config.Volumes (R33)", cfg.Volumes, map[string]struct{}(nil)},
		{"Config.ExposedPorts", len(cfg.ExposedPorts), 0},

		{"HostConfig.NetworkMode (R75)", hc.NetworkMode, container.NetworkMode(testRunner + "-net")},
		{"HostConfig.Privileged (R32)", hc.Privileged, false},
		{"HostConfig.CapDrop (R32)", hc.CapDrop, []string{"ALL"}},
		{"HostConfig.CapAdd (R32)", hc.CapAdd, []string(nil)},
		{"HostConfig.SecurityOpt (R32, R72)", hc.SecurityOpt, []string{"no-new-privileges"}},
		{"HostConfig.Init", hc.Init, ptr(true)},
		{"HostConfig.Binds (R33)", hc.Binds, []string(nil)},
		{"HostConfig.Mounts (R33)", hc.Mounts, []mount.Mount(nil)},
		{"HostConfig.VolumesFrom (R33)", hc.VolumesFrom, []string(nil)},
		{"HostConfig.PortBindings", len(hc.PortBindings), 0},
		{"HostConfig.PublishAllPorts", hc.PublishAllPorts, false},
		{"HostConfig.NanoCPUs (R34)", hc.NanoCPUs, int64(1_000_000_000)},
		{"HostConfig.Memory (R34)", hc.Memory, int64(1 << 30)},
		{"HostConfig.MemorySwap (R34 no swap)", hc.MemorySwap, int64(1 << 30)},
		{"HostConfig.PidsLimit (R34)", hc.PidsLimit, ptr(int64(512))},
		{"HostConfig.AutoRemove", hc.AutoRemove, false},
	})

	wantCfg := container.Config{
		Image: cfg.Image, User: cfg.User, Env: cfg.Env, Labels: cfg.Labels,
		AttachStdin: true, OpenStdin: true, StdinOnce: true,
	}
	if !reflect.DeepEqual(*cfg, wantCfg) {
		t.Errorf("runner Config has extra settings:\n got %#v\nwant %#v", *cfg, wantCfg)
	}
	wantHC := container.HostConfig{
		NetworkMode: hc.NetworkMode,
		CapDrop:     hc.CapDrop,
		SecurityOpt: hc.SecurityOpt,
		Init:        hc.Init,
		Resources:   container.Resources{NanoCPUs: hc.NanoCPUs, Memory: hc.Memory, MemorySwap: hc.MemorySwap, PidsLimit: hc.PidsLimit},
	}
	if !reflect.DeepEqual(*hc, wantHC) {
		t.Errorf("runner HostConfig has extra settings:\n got %#v\nwant %#v", *hc, wantHC)
	}
}

// TestSpecLimits proves 0002 SPEC R121, R122: the containers' limits are the
// [runner] and [buildkit] values, with swap off.
func TestSpecLimits(t *testing.T) {
	p := testParams()
	p.Runner = ContainerLimits(config.Container{CPUs: 1.5, Memory: 768 * config.Mebibyte, Pids: 64})
	p.BuildKit = ContainerLimits(config.Container{CPUs: 0.25, Memory: 3 * config.Gibibyte, Pids: 4096})
	r := RunnerSpec(p).HostConfig.Resources
	b := BuildKitSpec(p).HostConfig.Resources
	checkFields(t, []field{
		{"runner NanoCPUs", r.NanoCPUs, int64(1_500_000_000)},
		{"runner Memory", r.Memory, int64(768 << 20)},
		{"runner MemorySwap", r.MemorySwap, int64(768 << 20)},
		{"runner PidsLimit", *r.PidsLimit, int64(64)},
		{"buildkit NanoCPUs", b.NanoCPUs, int64(250_000_000)},
		{"buildkit Memory", b.Memory, int64(3 << 30)},
		{"buildkit MemorySwap", b.MemorySwap, int64(3 << 30)},
		{"buildkit PidsLimit", *b.PidsLimit, int64(4096)},
	})
}

// TestNanoCPUs checks the CPUs-to-NanoCPUs conversion at the edges of the
// range the configuration accepts (SPEC settings table: at least 0.01 and
// convertible to an int64 NanoCPUs value).
func TestNanoCPUs(t *testing.T) {
	// The largest value the configuration accepts: the float just below
	// MaxInt64/1e9, whose product with 1e9 is the float just below 2^63.
	largest := math.Nextafter(math.MaxInt64/1e9, 0)
	tests := []struct {
		name string
		cpus float64
		want int64
	}{
		{"lower bound 0.01", 0.01, 10_000_000},
		{"sum with a float error 0.1+0.2", 0.1 + 0.2, 300_000_000},
		// 1.005*1e9 is 1004999999.9999999 as a float; truncating would lose
		// one nanoCPU.
		{"rounded, not truncated", 1.005, 1_005_000_000},
		{"one CPU", 1, 1_000_000_000},
		{"below the upper bound 9e9", 9e9, 9_000_000_000_000_000_000},
		{"largest accepted", largest, 9_223_372_036_854_774_784},
	}
	for _, tt := range tests {
		got := resources(Limits{CPUs: tt.cpus}).NanoCPUs
		if shared := nanoCPUs(tt.cpus); got != shared {
			t.Errorf("%s: resources gives %d, nanoCPUs of the capacity check gives %d", tt.name, got, shared)
		}
		if got != tt.want {
			t.Errorf("%s: NanoCPUs(%v) = %d, want %d", tt.name, tt.cpus, got, tt.want)
		}
		if got <= 0 {
			t.Errorf("%s: NanoCPUs(%v) = %d overflowed", tt.name, tt.cpus, got)
		}
	}
}

func TestContainerLimits(t *testing.T) {
	cfg := config.Config{
		Runner:   config.Container{Image: "x", CPUs: 1, Memory: 2, Pids: 3},
		BuildKit: config.Container{Image: "y", CPUs: 4, Memory: 5, Pids: 6},
	}
	checkFields(t, []field{
		{"runner", ContainerLimits(cfg.Runner), Limits{CPUs: 1, Memory: 2, Pids: 3}},
		{"buildkit", ContainerLimits(cfg.BuildKit), Limits{CPUs: 4, Memory: 5, Pids: 6}},
	})
}

func TestJobCheckURL(t *testing.T) {
	tests := []struct {
		listen  string
		want    string
		wantErr bool
	}{
		{"127.0.0.1:8765", "http://host.docker.internal:8765", false},
		{"[::1]:9000", "http://host.docker.internal:9000", false},
		{"localhost:1", "http://host.docker.internal:1", false},
		{"127.0.0.1", "", true},
		{"127.0.0.1:", "", true},
		{"", "", true},
	}
	for _, tt := range tests {
		got, err := JobCheckURL(tt.listen)
		if (err != nil) != tt.wantErr || got != tt.want {
			t.Errorf("JobCheckURL(%q) = %q, %v; want %q, error %v", tt.listen, got, err, tt.want, tt.wantErr)
		}
	}
}
