package doctor

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dewevrei/actions-runner-scaler/internal/docker"
	"github.com/dewevrei/actions-runner-scaler/internal/logging"
)

// testNow is the run time of every test; R317's window is measured from it.
var testNow = time.Date(2026, 11, 26, 0, 0, 0, 0, time.UTC)

type fixedClock struct{ t time.Time }

func (c fixedClock) Now() time.Time { return c.t }

// runnerImageID is a well-formed runner.image value.
var runnerImageID = "sha256:" + strings.Repeat("ab", 32)

// buildkitID is the ID the fake Docker gives the BuildKit image.
var buildkitID = "sha256:" + strings.Repeat("cd", 32)

// fixtureToken returns a token-shaped value built at run time, so no
// token-shaped literal is committed.
func fixtureToken(prefix string) string {
	return prefix + strings.Repeat("aB3dE5", 6)
}

// writeSecret writes data to a file of mode in a new temporary directory
// and returns its absolute path.
func writeSecret(t *testing.T, name string, data []byte, mode os.FileMode) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, data, mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
	return path
}

func writeTokenFile(t *testing.T, token string) string {
	t.Helper()
	return writeSecret(t, "token", []byte(token+"\n"), 0o600)
}

func writeAppKey(t *testing.T, mode os.FileMode) string {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 1024)
	if err != nil {
		t.Fatal(err)
	}
	data := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	return writeSecret(t, "app.pem", data, mode)
}

// writeConfig writes a configuration with valid images, the [github] lines
// and one [[targets]] entry per element of targets, and returns its path.
func writeConfig(t *testing.T, githubLines string, targets ...string) string {
	t.Helper()
	var b strings.Builder
	fmt.Fprintf(&b, "[github]\n%s\n\n[runner]\nimage = %q\n\n[buildkit]\nimage = %q\n", githubLines, runnerImageID, docker.BuildKitImage)
	for _, tgt := range targets {
		fmt.Fprintf(&b, "\n[[targets]]\n%s\n", tgt)
	}
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func appLines(keyPath string) string {
	return fmt.Sprintf("app-client-id = \"Iv1.test\"\napp-private-key-file = %q", keyPath)
}

func target(repo string, extra ...string) string {
	return strings.Join(append([]string{fmt.Sprintf("url = %q", "https://github.com/"+repo)}, extra...), "\n")
}

func tokenTarget(repo, tokenPath string) string {
	return target(repo, fmt.Sprintf("token-file = %q", tokenPath))
}

// reply is one canned GitHub answer.
type reply struct {
	status int
	header map[string]string
	body   string
	// hang waits for the request to be cancelled and sends nothing.
	hang bool
	// echoAuth quotes the request's Authorization header in the message.
	echoAuth bool
}

// githubServer is a fake GitHub API that answers by path and records
// every request as "METHOD path?query".
type githubServer struct {
	srv *httptest.Server

	mu       sync.Mutex
	replies  map[string]reply
	requests []string
	auth     []string
}

func newGitHubServer(t *testing.T, replies map[string]reply) *githubServer {
	t.Helper()
	g := &githubServer{replies: replies}
	g.srv = httptest.NewServer(http.HandlerFunc(g.serve))
	t.Cleanup(g.srv.Close)
	return g
}

func (g *githubServer) serve(w http.ResponseWriter, r *http.Request) {
	g.mu.Lock()
	g.requests = append(g.requests, r.Method+" "+r.URL.RequestURI())
	g.auth = append(g.auth, r.Header.Get("Authorization"))
	rep, ok := g.replies[r.URL.Path]
	g.mu.Unlock()
	if !ok {
		rep = reply{status: http.StatusNotFound, body: `{"message":"Not Found"}`}
	}
	if rep.hang {
		<-r.Context().Done()
		return
	}
	for k, v := range rep.header {
		w.Header().Set(k, v)
	}
	w.WriteHeader(rep.status)
	if rep.echoAuth {
		fmt.Fprintf(w, `{"message":"bad credentials %s"}`, r.Header.Get("Authorization"))
		return
	}
	fmt.Fprint(w, rep.body)
}

func (g *githubServer) Requests() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]string(nil), g.requests...)
}

// clients returns real GitHub clients against g, registering secrets in s.
func (g *githubServer) clients(s *logging.Secrets) GitHubClients {
	return NewGitHubClients(GitHubConfig{
		BaseURL:    g.srv.URL,
		HTTPClient: g.srv.Client(),
		Clock:      fixedClock{testNow},
		Secrets:    s,
	})
}

const installationWrite = `{"id":42,"permissions":{"administration":"write"}}`

// fakeDocker answers the Docker calls doctor makes and records them.
type fakeDocker struct {
	// connectErr fails the making of the client.
	connectErr error
	pingErr    error
	info       docker.Capacity
	infoErr    error
	images     map[string]docker.Image
	imageErr   error
	// dist answers every DistributionInspect call, unless distErr is set.
	dist    docker.Distribution
	distErr error
	// hangDist makes only DistributionInspect wait for ctx to end.
	hangDist bool
	// hang makes every call wait for ctx to end.
	hang bool

	mu    sync.Mutex
	calls []string
	// hosts are the hosts the client was made for.
	hosts []string
}

// connect is Deps.Docker: it records host and returns d, or connectErr.
func (d *fakeDocker) connect(host string) (DockerProbe, error) {
	d.mu.Lock()
	d.hosts = append(d.hosts, host)
	d.mu.Unlock()
	if d.connectErr != nil {
		return nil, d.connectErr
	}
	return d, nil
}

// Hosts returns the hosts the client was made for.
func (d *fakeDocker) Hosts() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.hosts...)
}

func (d *fakeDocker) record(call string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.calls = append(d.calls, call)
}

func (d *fakeDocker) Calls() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.calls...)
}

func (d *fakeDocker) Ping(ctx context.Context) error {
	d.record("ping")
	if d.hang {
		<-ctx.Done()
		return ctx.Err()
	}
	return d.pingErr
}

func (d *fakeDocker) Info(ctx context.Context) (docker.Capacity, error) {
	d.record("info")
	if d.hang {
		<-ctx.Done()
		return docker.Capacity{}, ctx.Err()
	}
	return d.info, d.infoErr
}

func (d *fakeDocker) ImageInspect(ctx context.Context, ref string) (docker.Image, bool, error) {
	d.record("image " + ref)
	if d.hang {
		<-ctx.Done()
		return docker.Image{}, false, ctx.Err()
	}
	if d.imageErr != nil {
		return docker.Image{}, false, d.imageErr
	}
	img, ok := d.images[ref]
	return img, ok, nil
}

func (d *fakeDocker) DistributionInspect(ctx context.Context, ref string) (docker.Distribution, error) {
	d.record("distribution " + ref)
	if d.hang || d.hangDist {
		<-ctx.Done()
		return docker.Distribution{}, ctx.Err()
	}
	return d.dist, d.distErr
}

// goodDocker answers every check with a pass: both images local on arm64,
// and room for 2 runners of the default share.
func goodDocker() *fakeDocker {
	return &fakeDocker{
		info: docker.Capacity{NCPU: 8, MemTotal: 16 << 30},
		images: map[string]docker.Image{
			runnerImageID:        {ID: runnerImageID, Architecture: "arm64"},
			docker.BuildKitImage: {ID: buildkitID, Architecture: "arm64"},
		},
	}
}

func notExposed(context.Context) docker.Exposure { return docker.NotExposed }

// testDeps returns Deps on the configuration at path, real GitHub clients
// against gh and the fake Docker d.
func testDeps(path string, gh *githubServer, d *fakeDocker) (Deps, *logging.Secrets) {
	secrets := logging.NewSecrets()
	return Deps{
		ConfigPath: path,
		GitHub:     gh.clients(secrets),
		Docker:     d.connect,
		Exposure:   notExposed,
		Now:        func() time.Time { return testNow },
		Redact:     secrets.Redact,
		Timeout:    2 * time.Second,
	}, secrets
}

// output runs doctor with deps and returns the printed text and the
// number of 실패 lines.
func output(t *testing.T, deps Deps) (string, int) {
	t.Helper()
	rep, err := Run(context.Background(), deps)
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	failed, err := Print(&buf, rep)
	if err != nil {
		t.Fatal(err)
	}
	return buf.String(), failed
}

// fakeGitHub is GitHubClients that records which clients were made.
type fakeGitHub struct {
	mu     sync.Mutex
	made   []string
	inner  GitHubClients
	appErr error
}

func (f *fakeGitHub) App(clientID, keyPath string) (AppProbe, error) {
	f.mu.Lock()
	f.made = append(f.made, "app")
	f.mu.Unlock()
	if f.appErr != nil {
		return nil, f.appErr
	}
	return f.inner.App(clientID, keyPath)
}

func (f *fakeGitHub) Token(path string) (TokenClient, error) {
	f.mu.Lock()
	f.made = append(f.made, "token")
	f.mu.Unlock()
	return f.inner.Token(path)
}
