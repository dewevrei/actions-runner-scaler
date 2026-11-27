// Package docker creates, watches and removes the Docker resources of one
// runner: a dedicated network, a BuildKit container and the runner container.
//
// The functions in spec.go are pure and hold every isolation setting
// (R32~R36, R70, R72~R76); client.go only sends them. The package makes one
// Engine API call per step and leaves timeouts and retries to its callers.
package docker

import (
	"fmt"
	"net"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/dewevrei/actions-runner-scaler/internal/config"
)

// Labels on every resource the scaler creates.
const (
	// LabelManagedBy marks a resource as the scaler's; its value is ManagedBy.
	LabelManagedBy = "managed-by"
	// LabelRunnerName holds the name of the runner the resource belongs to.
	LabelRunnerName = "runner-name"
	// LabelKind holds KindNetwork, KindBuildKit or KindRunner.
	LabelKind = "kind"

	ManagedBy = "actions-runner-scaler"

	KindNetwork  = "network"
	KindBuildKit = "buildkit"
	KindRunner   = "runner"
)

// Environment variables the runner container gets (R70). They are the only
// ones the scaler sets, and none of them is a secret.
const (
	EnvJobCheckURL  = "SCALER_JOBCHECK_URL"
	EnvRunnerName   = "SCALER_RUNNER_NAME"
	EnvBuildKitHost = "BUILDKIT_HOST"
)

const (
	// runnerUser is the non-root user of the runner image (R32).
	runnerUser = "runner"
	// buildKitUser is the non-root uid:gid of the rootless BuildKit image
	// (R73). It is set explicitly so a root image cannot slip in.
	buildKitUser = "1000:1000"
	// buildKitPort is where buildkitd listens inside the runner network.
	buildKitPort = 1234
	// networkDriver gives each runner its own bridge, so a container on
	// another runner's network cannot reach this BuildKit (R75).
	networkDriver = "bridge"
	// gatewayModeIPv4 is the bridge option that decides whether containers
	// in other networks can reach unpublished ports. In mode "nat" (the
	// default) they cannot; in "routed" they can by direct routing (Docker
	// docs, engine/network/port-publishing, "Gateway modes"). A daemon's
	// default-network-opts fills in any option a create request leaves out
	// (moby docker-v29.6.1 daemon/network.go 315~324), so the scaler states
	// the mode instead of relying on the default.
	gatewayModeIPv4 = "com.docker.network.bridge.gateway_mode_ipv4"
	// enableICC is the bridge option that allows traffic between containers
	// on the same network. The runner reaches BuildKit that way (R75), so
	// the scaler states it too: a daemon's default-network-opts could set it
	// to false (same daemon/network.go lines; option parsed in moby
	// docker-v29.6.1 daemon/libnetwork/drivers/bridge/labels.go 16 and
	// bridge_linux.go 380~383, default true at 584).
	enableICC = "com.docker.network.bridge.enable_icc"
	// jobCheckHost is how a container reaches the host's loopback listener
	// on Docker Desktop.
	jobCheckHost = "host.docker.internal"
)

// Limits are the CPU, memory and process limits of one container. Swap is
// always off: the memory-plus-swap limit equals Memory (R34, R74).
type Limits struct {
	CPUs   float64
	Memory config.ByteSize
	Pids   int64
}

// ContainerLimits returns the limits of the container kind c describes: the
// [runner] or [buildkit] table (0002 SPEC R121, R122).
func ContainerLimits(c config.Container) Limits {
	return Limits{CPUs: c.CPUs, Memory: c.Memory, Pids: c.Pids}
}

// Params describe one runner's resources. None of them is a secret; the
// JIT config and the job-check token go to StartRunner separately.
type Params struct {
	// Name is the runner name; the other resource names derive from it.
	Name string
	// RunnerImage is the runner image ID resolved for this runner from
	// runner.image (0003 SPEC R319).
	RunnerImage string
	// BuildKitImage is the BuildKit image ID resolved for this runner from
	// buildkit.image (0003 SPEC R319).
	BuildKitImage string
	// JobCheckURL is the job-check server URL as seen from a container;
	// see JobCheckURL.
	JobCheckURL string
	Runner      Limits
	BuildKit    Limits

	// managedBy is the LabelManagedBy value; empty means ManagedBy.
	// StartRunner sets it from the Client, so only a Client built for the
	// integration tests labels resources differently.
	managedBy string
}

// NetworkName returns the name of the dedicated network of runner name.
func NetworkName(name string) string { return name + "-net" }

// BuildKitName returns the name of the BuildKit container of runner name.
func BuildKitName(name string) string { return name + "-buildkit" }

// BuildKitHost returns the BUILDKIT_HOST value for runner name: the
// BuildKit container on the runner's own network.
func BuildKitHost(name string) string {
	return fmt.Sprintf("tcp://%s:%d", BuildKitName(name), buildKitPort)
}

// JobCheckURL returns the job-check server URL a container uses for the
// server listening on listen (host:port on the host's loopback).
func JobCheckURL(listen string) (string, error) {
	_, port, err := net.SplitHostPort(listen)
	if err != nil {
		return "", fmt.Errorf("job-check listen address %q: %w", listen, err)
	}
	if port == "" {
		return "", fmt.Errorf("job-check listen address %q: no port", listen)
	}
	return "http://" + net.JoinHostPort(jobCheckHost, port), nil
}

// labels returns the labels of the kind resource of runner name, with
// managedBy as the LabelManagedBy value (empty means ManagedBy).
func labels(managedBy, name, kind string) map[string]string {
	if managedBy == "" {
		managedBy = ManagedBy
	}
	return map[string]string{
		LabelManagedBy:  managedBy,
		LabelRunnerName: name,
		LabelKind:       kind,
	}
}

// platform is the only platform the images are built for (R35).
func platform() *ocispec.Platform {
	return &ocispec.Platform{OS: "linux", Architecture: "arm64"}
}

// resources converts l into Engine API limits with swap off.
func resources(l Limits) container.Resources {
	pids := l.Pids
	return container.Resources{
		NanoCPUs:   nanoCPUs(l.CPUs),
		Memory:     int64(l.Memory),
		MemorySwap: int64(l.Memory),
		PidsLimit:  &pids,
	}
}

// NetworkSpec returns the options of the dedicated network of p; its name
// is NetworkName(p.Name).
//
// It is a plain user-defined bridge, which by default drops traffic from
// containers in other networks to unpublished ports (R75; Docker docs
// engine/network/drivers/bridge and engine/network/port-publishing; moby
// docker-v29.6.1 bridge iptabler/network.go 240~263, nftabler/network.go
// 163~175). The gateway mode is pinned to that default and IPv6 stays off,
// so daemon-wide default options cannot turn on direct routing. IPv4 is
// stated as on: a daemon whose default-network-opts hold
// com.docker.network.enable_ipv4=false would otherwise create the network
// without IPv4, and the runner could not reach BuildKit or the job-check
// server. An explicit EnableIPv4 wins over that option (moby docker-v29.6.1
// daemon/network.go 326~336; client v0.6.2 network_create.go 14, 39; the
// daemon ignores the field below API 1.48, daemon/server/router/network/
// network_routes.go 259~262). It is not
// internal: BuildKit pulls and pushes images, and an internal network has
// no route to other networks or the outside. Inter-container connectivity
// is stated as on, since the runner reaches BuildKit on this network and
// daemon-wide default options could otherwise turn it off (R75).
func NetworkSpec(p Params) client.NetworkCreateOptions {
	ipv4, ipv6 := true, false
	return client.NetworkCreateOptions{
		Driver:     networkDriver,
		EnableIPv4: &ipv4,
		EnableIPv6: &ipv6,
		Options:    map[string]string{gatewayModeIPv4: "nat", enableICC: "true"},
		Labels:     labels(p.managedBy, p.Name, KindNetwork),
	}
}

// BuildKitSpec returns the BuildKit container of p (R72~R76): rootless
// buildkitd without process sandbox, all capabilities dropped but SETUID and
// SETGID, seccomp unconfined and nothing else changed, on the runner network
// only, with no mounts and no published ports. Its one volume is the
// anonymous volume the image declares, removed with the container.
//
// BuildKit's rootless guide calls --oci-worker-no-process-sandbox
// discouraged (build steps can kill daemon processes, and leftover
// processes cannot be terminated) but uses it in its Kubernetes examples;
// its main Docker recipe uses systempaths=unconfined instead, which this
// design does not grant (moby/buildkit docs/rootless.md). The runner reaches buildkitd over TCP on the
// per-runner network rather than through a shared socket volume, as ARC and
// runscaler do for dind, because that network also keeps runners apart.
func BuildKitSpec(p Params) client.ContainerCreateOptions {
	return client.ContainerCreateOptions{
		Name:     BuildKitName(p.Name),
		Platform: platform(),
		Config: &container.Config{
			Image:  p.BuildKitImage,
			User:   buildKitUser,
			Cmd:    []string{"--addr", fmt.Sprintf("tcp://0.0.0.0:%d", buildKitPort), "--oci-worker-no-process-sandbox"},
			Labels: labels(p.managedBy, p.Name, KindBuildKit),
		},
		HostConfig: &container.HostConfig{
			NetworkMode: container.NetworkMode(NetworkName(p.Name)),
			CapDrop:     []string{"ALL"},
			CapAdd:      []string{"SETUID", "SETGID"},
			SecurityOpt: []string{"seccomp=unconfined"},
			Privileged:  false,
			Resources:   resources(p.BuildKit),
		},
	}
}

// RunnerSpec returns the runner container of p (R32~R36, R70): the runner
// user, no capabilities, no-new-privileges, an init process, the three
// environment variables only, stdin open for the two secret lines, on the
// runner network only, with no mounts and no published ports.
func RunnerSpec(p Params) client.ContainerCreateOptions {
	useInit := true
	return client.ContainerCreateOptions{
		Name:     p.Name,
		Platform: platform(),
		Config: &container.Config{
			Image: p.RunnerImage,
			User:  runnerUser,
			Env: []string{
				EnvJobCheckURL + "=" + p.JobCheckURL,
				EnvRunnerName + "=" + p.Name,
				EnvBuildKitHost + "=" + BuildKitHost(p.Name),
			},
			AttachStdin: true,
			OpenStdin:   true,
			StdinOnce:   true,
			Labels:      labels(p.managedBy, p.Name, KindRunner),
		},
		HostConfig: &container.HostConfig{
			NetworkMode: container.NetworkMode(NetworkName(p.Name)),
			CapDrop:     []string{"ALL"},
			SecurityOpt: []string{"no-new-privileges"},
			Privileged:  false,
			Init:        &useInit,
			Resources:   resources(p.Runner),
		},
	}
}
