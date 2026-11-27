package doctor

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/dewevrei/actions-runner-scaler/internal/docker"
	"github.com/dewevrei/actions-runner-scaler/internal/retry"
)

func TestDoctorDockerChecks(t *testing.T) {
	gh := newGitHubServer(t, map[string]reply{
		"/repos/o/a/actions/runners": {status: 200, body: runnersOK, header: map[string]string{"X-OAuth-Scopes": "repo"}},
		"/repos/o/b/actions/runners": {status: 200, body: runnersOK, header: map[string]string{"X-OAuth-Scopes": "repo"}},
	})
	tokenLine := "token-file = \"" + writeTokenFile(t, fixtureToken("ghp_")) + "\""
	oneTarget := writeConfig(t, tokenLine, target("o/a"))
	idleTwo := writeConfig(t, tokenLine, target("o/a", "idle = 1"), target("o/b", "idle = 1"))

	tests := []struct {
		name     string
		config   string
		docker   func(*fakeDocker)
		exposure docker.Exposure
		want     []string
	}{
		{name: "ping fails", config: oneTarget,
			docker: func(d *fakeDocker) { d.pingErr = errors.New("connect: connection refused") },
			want:   []string{"FAIL Docker: could not connect to /var/run/docker.sock\n  cause: connect: connection refused\n  fix: " + fixDockerSocket}},
		{name: "ping 500", config: oneTarget,
			docker: func(d *fakeDocker) {
				d.pingErr = &retry.OpError{Op: retry.OpDockerPing, Err: &docker.StatusError{Status: 500, Err: errors.New("starting")}}
			},
			want: []string{"UNKNOWN Docker: could not check"}},
		{name: "2375 exposed", config: oneTarget, exposure: docker.Exposed,
			want: []string{"FAIL Docker TCP 2375: Docker API exposed on 127.0.0.1:2375\n  cause: Docker API answers on 127.0.0.1:2375 without TLS (R79)\n  fix: " + fixExposure}},
		{name: "2375 unknown", config: oneTarget, exposure: docker.ExposureUnknown,
			want: []string{"UNKNOWN Docker TCP 2375: could not check\n  cause: could not tell whether the Docker daemon is exposed on TCP 2375"}},
		{name: "image missing", config: oneTarget,
			docker: func(d *fakeDocker) { delete(d.images, runnerImageID) },
			want: []string{"FAIL Image runner.image: not present locally\n  cause: image " + runnerImageID + " is not present locally\n  fix: " + fixImageBuild,
				"PASS Image buildkit.image: present locally, arm64, " + docker.BuildKitImage + " = " + buildkitID}},
		{name: "amd64", config: oneTarget,
			docker: func(d *fakeDocker) {
				img := d.images[runnerImageID]
				img.Architecture = "amd64"
				d.images[runnerImageID] = img
			},
			want: []string{"FAIL Image runner.image: architecture is not arm64\n  cause: image " + runnerImageID + ` is "amd64", not arm64`}},
		{name: "limit 0", config: oneTarget,
			docker: func(d *fakeDocker) { d.info = docker.Capacity{NCPU: 2, MemTotal: 16 << 30} },
			want:   []string{"FAIL Server limit: 0\n  cause: server runner limit is 0: one runner reserves 3 CPUs (runner.cpus 1 + buildkit.cpus 2) and 3221225472 bytes (runner.memory 1g + buildkit.memory 2g), Docker reports 2 CPUs and 17179869184 bytes\n  fix: " + fixLimitZero}},
		{name: "idle above limit", config: idleTwo,
			docker: func(d *fakeDocker) { d.info = docker.Capacity{NCPU: 3, MemTotal: 16 << 30} },
			want:   []string{"FAIL Server limit: 1\n  cause: idle of all targets (2) exceeds the server runner limit (1)\n  fix: " + fixLimitIdle}},
		{name: "pass names the limit", config: idleTwo,
			want: []string{"PASS Server limit: 2 runners", "PASS Docker: /var/run/docker.sock answered", "PASS Docker TCP 2375: no Docker API on 127.0.0.1:2375"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := goodDocker()
			if tt.docker != nil {
				tt.docker(d)
			}
			deps, _ := testDeps(tt.config, gh, d)
			deps.Exposure = func(context.Context) docker.Exposure { return tt.exposure }
			out, _ := output(t, deps)
			for _, want := range tt.want {
				if got := block(out, want[:strings.IndexByte(want, ':')+1]); !strings.HasPrefix(got, want) {
					t.Errorf("got:\n%s\nwant:\n%s\nall:\n%s", got, want, out)
				}
			}
		})
	}
}

// runnerTag is a runner.image value that is a reference, as init writes it.
const runnerTag = "actions-runner-scaler-runner:local"

// writeReferenceConfig writes a configuration whose runner.image is
// runnerTag and whose buildkit.image is docker.BuildKitImage.
func writeReferenceConfig(t *testing.T) string {
	t.Helper()
	path := writeConfig(t, "token-file = \""+writeTokenFile(t, fixtureToken("ghp_"))+"\"", target("o/a"))
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	s := strings.Replace(string(data), runnerImageID, runnerTag, 1)
	if err := os.WriteFile(path, []byte(s), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func platforms(ps ...string) docker.Distribution {
	var d docker.Distribution
	for _, p := range ps {
		goos, arch, _ := strings.Cut(p, "/")
		d.Platforms = append(d.Platforms, ocispec.Platform{OS: goos, Architecture: arch})
	}
	return d
}

func distributionError(status int, text string) error {
	return &retry.OpError{Op: retry.OpDistributionInspect, Err: &docker.StatusError{Status: status, Err: errors.New(text)}}
}

// referenceGitHub is the GitHub side of the reference tests: one token
// target that passes.
func referenceGitHub(t *testing.T) *githubServer {
	return newGitHubServer(t, map[string]reply{
		"/repos/o/a/actions/runners": {status: 200, body: runnersOK, header: map[string]string{"X-OAuth-Scopes": "repo"}},
	})
}

func TestDoctorImageReference(t *testing.T) {
	localID := "sha256:" + strings.Repeat("ef", 32)
	localRef := func(d *fakeDocker) {
		d.images = map[string]docker.Image{
			runnerTag:            {ID: localID, Architecture: "arm64"},
			docker.BuildKitImage: {ID: localID, Architecture: "arm64"},
		}
	}
	notLocal := func(d *fakeDocker) { d.images = nil }
	unknown := func(key, cause, fix string) string {
		return "UNKNOWN Image " + key + ": could not check\n  cause: " + cause + "\n  fix: " + fix
	}
	failure := func(key, summary, cause, fix string) string {
		return "FAIL Image " + key + ": " + summary + "\n  cause: " + cause + "\n  fix: " + fix
	}

	tests := []struct {
		name   string
		docker func(d *fakeDocker)
		// timeout replaces the limit of the test.
		timeout time.Duration
		want    []string
		// distributions is how many registry calls the run makes.
		distributions int
	}{
		{name: "local reference", docker: localRef, want: []string{
			"PASS Image runner.image: present locally, arm64, " + runnerTag + " = " + localID,
			"PASS Image buildkit.image: present locally, arm64, " + docker.BuildKitImage + " = " + localID}},
		{name: "registry has linux/arm64", distributions: 2,
			docker: func(d *fakeDocker) { notLocal(d); d.dist = platforms("linux/amd64", "linux/arm64") },
			want: []string{
				"PASS Image runner.image: not present locally, pulled at start",
				"PASS Image buildkit.image: not present locally, pulled at start"}},
		{name: "linux/amd64 only", distributions: 2,
			docker: func(d *fakeDocker) { notLocal(d); d.dist = platforms("linux/amd64", "windows/arm64") },
			want: []string{
				failure("runner.image", "not a linux/arm64 image", "registry image "+runnerTag+" has no linux/arm64 platform", fixImageTag),
				failure("buildkit.image", "not a linux/arm64 image", "registry image "+docker.BuildKitImage+" has no linux/arm64 platform", fixImageNoPull)}},
		{name: "empty platforms", distributions: 2,
			docker: func(d *fakeDocker) { notLocal(d) },
			want: []string{
				unknown("runner.image", "the registry did not report the image's platforms", fixAgain),
				unknown("buildkit.image", "the registry did not report the image's platforms", fixAgain)}},
		{name: "401", distributions: 2,
			docker: func(d *fakeDocker) { notLocal(d); d.distErr = distributionError(401, "pull access denied") },
			want: []string{
				failure("runner.image", "not found in the registry", "the registry has no such image or denies reading it: inspect image in registry: status 401: pull access denied", fixImageTag),
				failure("buildkit.image", "not found in the registry", "the registry has no such image or denies reading it: inspect image in registry: status 401: pull access denied", fixImageNoPull)}},
		{name: "timeout", distributions: 2, timeout: 50 * time.Millisecond,
			docker: func(d *fakeDocker) { notLocal(d); d.hangDist = true },
			want: []string{
				unknown("runner.image", "Docker did not answer within 50ms", fixAgain),
				unknown("buildkit.image", "Docker did not answer within 50ms", fixAgain)}},
		{name: "503", distributions: 2,
			docker: func(d *fakeDocker) { notLocal(d); d.distErr = distributionError(503, "unavailable") },
			want: []string{
				unknown("runner.image", "inspect image in registry: status 503: unavailable", fixAgain),
				unknown("buildkit.image", "inspect image in registry: status 503: unavailable", fixAgain)}},
		{name: "418", distributions: 2,
			docker: func(d *fakeDocker) { notLocal(d); d.distErr = distributionError(418, "teapot") },
			want: []string{
				unknown("runner.image", "unexpected response code 418: inspect image in registry: status 418: teapot", fixRegistryUnknown),
				unknown("buildkit.image", "unexpected response code 418: inspect image in registry: status 418: teapot", fixRegistryUnknown)}},
	}
	gh := referenceGitHub(t)
	path := writeReferenceConfig(t)
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := goodDocker()
			tt.docker(d)
			deps, _ := testDeps(path, gh, d)
			if tt.timeout != 0 {
				deps.Timeout = tt.timeout
			}
			out, _ := output(t, deps)
			for _, want := range tt.want {
				if got := block(out, want[:strings.Index(want, ":")+1]); !strings.HasPrefix(got, want) {
					t.Errorf("got:\n%s\nwant:\n%s\nall:\n%s", got, want, out)
				}
			}
			n := 0
			for _, c := range d.Calls() {
				if strings.HasPrefix(c, "distribution ") {
					n++
				}
			}
			if n != tt.distributions {
				t.Errorf("distribution calls = %d, want %d: %v", n, tt.distributions, d.Calls())
			}
		})
	}

	// The build.sh hint is for runner.image only: the runner tag in
	// buildkit.image is a mistake that build.sh does not fix.
	t.Run("runner tag in buildkit.image keeps the registry hint", func(t *testing.T) {
		cfgPath := writeConfig(t, "token-file = \""+writeTokenFile(t, fixtureToken("ghp_"))+"\"", target("o/a"))
		data, err := os.ReadFile(cfgPath)
		if err != nil {
			t.Fatal(err)
		}
		s := strings.Replace(string(data), docker.BuildKitImage, runnerTag, 1)
		if err := os.WriteFile(cfgPath, []byte(s), 0o600); err != nil {
			t.Fatal(err)
		}
		d := goodDocker()
		d.dist = platforms("linux/amd64")
		deps, _ := testDeps(cfgPath, gh, d)
		out, _ := output(t, deps)
		want := failure("buildkit.image", "not a linux/arm64 image", "registry image "+runnerTag+" has no linux/arm64 platform", fixImageNoPull)
		if got := block(out, "FAIL Image buildkit.image:"); !strings.HasPrefix(got, want) {
			t.Errorf("got:\n%s\nwant:\n%s", got, want)
		}
	})

	t.Run("missing image ID is not looked up", func(t *testing.T) {
		d := goodDocker()
		delete(d.images, runnerImageID)
		d.dist = platforms("linux/arm64")
		deps, _ := testDeps(writeConfig(t, "token-file = \""+writeTokenFile(t, fixtureToken("ghp_"))+"\"", target("o/a")), gh, d)
		out, _ := output(t, deps)
		if got := block(out, "FAIL Image runner.image:"); !strings.Contains(got, "not present locally") {
			t.Errorf("output:\n%s", out)
		}
		for _, c := range d.Calls() {
			if strings.HasPrefix(c, "distribution ") {
				t.Errorf("registry asked about an image ID: %v", d.Calls())
			}
		}
	})
}

func TestDoctorProbeCalls(t *testing.T) {
	var methods []string
	probe := reflect.TypeFor[DockerProbe]()
	for i := range probe.NumMethod() {
		methods = append(methods, probe.Method(i).Name)
	}
	if want := []string{"DistributionInspect", "ImageInspect", "Info", "Ping"}; !slices.Equal(methods, want) {
		t.Errorf("DockerProbe methods = %v, want %v: doctor has no pull", methods, want)
	}

	d := goodDocker()
	d.images = nil
	d.dist = platforms("linux/arm64")
	deps, _ := testDeps(writeReferenceConfig(t), referenceGitHub(t), d)
	output(t, deps)
	want := []string{
		"ping", "info",
		"image " + runnerTag, "distribution " + runnerTag,
		"image " + docker.BuildKitImage, "distribution " + docker.BuildKitImage,
	}
	got := d.Calls()
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Errorf("Docker calls = %v, want %v", got, want)
	}
}

// TestDoctorDockerHost proves that the Docker checks use docker.host and
// that the Docker line names the host it checked; a refused docker.host
// makes no client and leaves the Docker checks to Config, and a file that
// cannot be read checks the default host.
func TestDoctorDockerHost(t *testing.T) {
	gh := newGitHubServer(t, map[string]reply{
		"/repos/o/a/actions/runners": {status: 200, body: runnersOK, header: map[string]string{"X-OAuth-Scopes": "repo"}},
	})
	tokenLine := "token-file = \"" + writeTokenFile(t, fixtureToken("ghp_")) + "\""
	withHost := func(host string) string {
		return writeConfig(t, tokenLine+"\n\n[docker]\nhost = \""+host+"\"", target("o/a"))
	}
	refused := errors.New("connect: connection refused")
	tests := []struct {
		name      string
		config    string
		docker    func(*fakeDocker)
		wantHosts []string
		want      []string
	}{
		{name: "tcp", config: withHost("tcp://127.0.0.1:2376"),
			wantHosts: []string{"tcp://127.0.0.1:2376"},
			want:      []string{"PASS Docker: tcp://127.0.0.1:2376 answered", "PASS Image runner.image:", "PASS Server limit:"}},
		{name: "tcp fails", config: withHost("tcp://127.0.0.1:2376"),
			docker:    func(d *fakeDocker) { d.pingErr = refused },
			wantHosts: []string{"tcp://127.0.0.1:2376"},
			want:      []string{"FAIL Docker: could not connect to tcp://127.0.0.1:2376\n  cause: connect: connection refused\n  fix: " + fixDockerTCP}},
		{name: "other socket fails", config: withHost("unix:///Users/u/.docker/run/docker.sock"),
			docker:    func(d *fakeDocker) { d.pingErr = refused },
			wantHosts: []string{"unix:///Users/u/.docker/run/docker.sock"},
			want:      []string{"FAIL Docker: could not connect to /Users/u/.docker/run/docker.sock\n  cause: connect: connection refused\n  fix: " + fixDockerHostSocket}},
		{name: "refused host", config: withHost("http://127.0.0.1:2376"),
			want: []string{
				"UNKNOWN Docker: not checked\n  cause: an earlier check did not pass: Config",
				"UNKNOWN Image runner.image: not checked\n  cause: an earlier check did not pass: Docker",
				"PASS Docker TCP 2375:",
			}},
		{name: "unreadable file", config: filepath.Join(t.TempDir(), "missing.toml"),
			wantHosts: []string{"unix:///var/run/docker.sock"},
			want:      []string{"PASS Docker: /var/run/docker.sock answered"}},
		{name: "client not made", config: withHost("tcp://127.0.0.1:2376"),
			docker:    func(d *fakeDocker) { d.connectErr = errors.New("bad host") },
			wantHosts: []string{"tcp://127.0.0.1:2376"},
			want:      []string{"FAIL Docker: could not connect to tcp://127.0.0.1:2376\n  cause: docker client: bad host"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := goodDocker()
			if tt.docker != nil {
				tt.docker(d)
			}
			deps, _ := testDeps(tt.config, gh, d)
			out, _ := output(t, deps)
			if got := d.Hosts(); !slices.Equal(got, tt.wantHosts) {
				t.Errorf("Docker clients made for %q, want %q", got, tt.wantHosts)
			}
			for _, want := range tt.want {
				if got := block(out, want[:strings.IndexByte(want, ':')+1]); !strings.HasPrefix(got, want) {
					t.Errorf("got:\n%s\nwant:\n%s\nall:\n%s", got, want, out)
				}
			}
			if tt.wantHosts == nil && len(d.Calls()) != 0 {
				t.Errorf("Docker calls = %v, want none", d.Calls())
			}
		})
	}
}

// closingDocker is a fakeDocker that counts its Close calls.
type closingDocker struct {
	*fakeDocker
	closed int
}

func (c *closingDocker) Close() error {
	c.closed++
	return nil
}

// Run closes the Docker client it made once the checks are done.
func TestDoctorClosesDocker(t *testing.T) {
	d := &closingDocker{fakeDocker: goodDocker()}
	path := writeConfig(t, "token-file = \""+writeTokenFile(t, fixtureToken("ghp_"))+"\"", target("o/a"))
	deps, _ := testDeps(path, newGitHubServer(t, nil), d.fakeDocker)
	deps.Docker = func(string) (DockerProbe, error) { return d, nil }
	output(t, deps)
	if d.closed != 1 {
		t.Errorf("Close calls = %d, want 1", d.closed)
	}
}
