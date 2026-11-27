package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/dewevrei/actions-runner-scaler/internal/docker"
	"github.com/dewevrei/actions-runner-scaler/internal/doctor"
)

// doctorRunnerImage is a well-formed runner.image value.
var doctorRunnerImage = "sha256:" + strings.Repeat("ab", 32)

// recorder is an httptest server that records "METHOD path?query" of
// every request.
type recorder struct {
	srv *httptest.Server

	mu       sync.Mutex
	requests []string
	// registryAuth counts the requests with an X-Registry-Auth header.
	registryAuth int
}

func newRecorder(t *testing.T, h func(w http.ResponseWriter, r *http.Request)) *recorder {
	t.Helper()
	rec := &recorder{}
	rec.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.mu.Lock()
		rec.requests = append(rec.requests, r.Method+" "+r.URL.RequestURI())
		if _, ok := r.Header["X-Registry-Auth"]; ok {
			rec.registryAuth++
		}
		rec.mu.Unlock()
		h(w, r)
	}))
	t.Cleanup(rec.srv.Close)
	return rec
}

func (r *recorder) Requests() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.requests...)
}

// RegistryAuth is the number of requests that carried X-Registry-Auth.
func (r *recorder) RegistryAuth() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.registryAuth
}

// doctorGitHub answers like GitHub for every target passing: the App
// installation with administration write, the runner list with the repo
// scope, and the policies list. A path in status gets that status and a
// message quoting the request's Authorization header.
func doctorGitHub(t *testing.T, status map[string]int) *recorder {
	return newRecorder(t, func(w http.ResponseWriter, r *http.Request) {
		if code, ok := status[r.URL.Path]; ok {
			w.WriteHeader(code)
			fmt.Fprintf(w, `{"message":"refused %s"}`, r.Header.Get("Authorization"))
			return
		}
		switch {
		case strings.HasSuffix(r.URL.Path, "/installation"):
			fmt.Fprint(w, `{"id":7,"permissions":{"administration":"write"}}`)
		case strings.HasSuffix(r.URL.Path, "/actions/runners"):
			w.Header().Set("X-OAuth-Scopes", "repo")
			fmt.Fprint(w, `{"total_count":0,"runners":[]}`)
		case strings.HasSuffix(r.URL.Path, "/actions/policies"):
			fmt.Fprint(w, `{}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
}

// doctorDocker answers like a Docker Engine with both images local on
// arm64.
func doctorDocker(t *testing.T) *recorder {
	return newRecorder(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Api-Version", "1.47")
		switch path := r.URL.Path; {
		case path == "/_ping":
			fmt.Fprint(w, "OK")
		case strings.HasSuffix(path, "/info"):
			fmt.Fprint(w, `{"NCPU":8,"MemTotal":17179869184}`)
		case strings.Contains(path, "/images/"):
			_ = json.NewEncoder(w).Encode(map[string]any{"Id": doctorRunnerImage, "Architecture": "arm64"})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
}

// doctorDockerMissing answers like a Docker Engine that has no image: image
// inspect is 404, and the registry has every reference for linux/arm64.
func doctorDockerMissing(t *testing.T) *recorder {
	return newRecorder(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Api-Version", "1.47")
		switch path := r.URL.Path; {
		case path == "/_ping":
			fmt.Fprint(w, "OK")
		case strings.HasSuffix(path, "/info"):
			fmt.Fprint(w, `{"NCPU":8,"MemTotal":17179869184}`)
		case strings.Contains(path, "/distribution/"):
			fmt.Fprint(w, `{"Descriptor":{},"Platforms":[{"architecture":"arm64","os":"linux"}]}`)
		default:
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprint(w, `{"message":"No such image"}`)
		}
	})
}

// writeDoctorConfig writes a configuration with valid images and the
// given [github] lines and targets in a temporary directory.
func writeDoctorConfig(t *testing.T, githubLines string, targets ...string) string {
	t.Helper()
	var b strings.Builder
	fmt.Fprintf(&b, "[github]\n%s\n\n[runner]\nimage = %q\n\n[buildkit]\nimage = %q\n", githubLines, doctorRunnerImage, docker.BuildKitImage)
	for _, tgt := range targets {
		fmt.Fprintf(&b, "\n[[targets]]\n%s\n", tgt)
	}
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func writeDoctorAppKey(t *testing.T) string {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 1024)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "app.pem")
	data := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// runDoctorAt runs runDoctor with the real wiring against fake GitHub
// and Docker servers; the R79 check is faked so the test never reaches
// the host's port 2375.
func runDoctorAt(t *testing.T, configPath string, gh, dk *recorder) (stdout, stderr string, err error) {
	t.Helper()
	engine, err := docker.NewEngine("tcp://" + dk.srv.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { engine.Close() })
	var out, errOut bytes.Buffer
	e := newEnv(&out, &errOut)
	deps := doctorDeps(e, configPath, gh.srv.URL, gh.srv.Client(), docker.New(engine))
	deps.Exposure = func(context.Context) docker.Exposure { return docker.NotExposed }
	err = runDoctor(context.Background(), e, deps)
	return out.String(), errOut.String(), err
}

func TestDoctorOnlyGET(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	path := writeDoctorConfig(t, fmt.Sprintf("app-client-id = \"Iv1.test\"\napp-private-key-file = %q", writeDoctorAppKey(t)),
		`url = "https://github.com/o/a"`,
		fmt.Sprintf("url = \"https://github.com/o/b\"\ntoken-file = %q", writeTokenFile(t, fixtureToken(t, "ghp_"))),
		fmt.Sprintf("url = \"https://github.com/o/c\"\ntoken-file = %q", writeTokenFile(t, fixtureToken(t, "github_pat_"))),
	)
	gh, dk := doctorGitHub(t, nil), doctorDocker(t)
	stdout, _, err := runDoctorAt(t, path, gh, dk)
	if err != nil || !strings.HasSuffix(stdout, "통과 12, 실패 0, 확인 못 함 0\n") {
		t.Fatalf("err = %v\n%s", err, stdout)
	}

	allowedGitHub := regexp.MustCompile(`^GET /repos/o/[abc]/(installation|actions/runners\?per_page=1|actions/policies\?per_page=1)$`)
	got := gh.Requests()
	for _, r := range got {
		if !allowedGitHub.MatchString(r) {
			t.Errorf("GitHub request %q is not an allowed GET", r)
		}
	}
	if len(got) != 4 {
		t.Errorf("GitHub requests = %v, want 4", got)
	}
	// The Engine client pings with HEAD and falls back to GET.
	allowedDocker := regexp.MustCompile(`^(HEAD|GET) /_ping$|^GET /v[0-9.]+/(info|images/[^?]+/json)$|^GET /v[0-9.]+/distribution/.+/json$`)
	for _, r := range dk.Requests() {
		if !allowedDocker.MatchString(r) {
			t.Errorf("Docker request %q is not ping, info, image inspect or distribution inspect", r)
		}
	}
	err = filepath.WalkDir(home, func(p string, _ fs.DirEntry, err error) error {
		if err == nil && p != home {
			t.Errorf("doctor made %s under HOME", p)
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}

	t.Run("missing references", func(t *testing.T) {
		const runnerTag = "actions-runner-scaler-runner:local"
		path := writeDoctorConfig(t, fmt.Sprintf("token-file = %q", writeTokenFile(t, fixtureToken(t, "ghp_"))),
			`url = "https://github.com/o/a"`)
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		s := strings.Replace(string(data), doctorRunnerImage, runnerTag, 1)
		if err := os.WriteFile(path, []byte(s), 0o600); err != nil {
			t.Fatal(err)
		}
		dk := doctorDockerMissing(t)
		stdout, _, err := runDoctorAt(t, path, doctorGitHub(t, nil), dk)
		if err != nil {
			t.Fatalf("err = %v\n%s", err, stdout)
		}
		for _, want := range []string{"통과 이미지 runner.image: 로컬에 없음, 시작 때 pull함", "통과 이미지 buildkit.image: 로컬에 없음, 시작 때 pull함"} {
			if !strings.Contains(stdout, want) {
				t.Errorf("no %q in:\n%s", want, stdout)
			}
		}
		distributions := 0
		for _, r := range dk.Requests() {
			if !allowedDocker.MatchString(r) {
				t.Errorf("Docker request %q is not ping, info, image inspect or distribution inspect", r)
			}
			if strings.Contains(r, "/distribution/") {
				distributions++
			}
			if strings.HasPrefix(r, "POST ") {
				t.Errorf("Docker request %q is a POST", r)
			}
		}
		if distributions != 2 {
			t.Errorf("distribution requests = %d, want one per missing reference (2): %v", distributions, dk.Requests())
		}
		if n := dk.RegistryAuth(); n != 0 {
			t.Errorf("%d requests carry X-Registry-Auth", n)
		}
	})
}

func TestDoctorExit(t *testing.T) {
	path := writeDoctorConfig(t, fmt.Sprintf("app-client-id = \"Iv1.test\"\napp-private-key-file = %q", writeDoctorAppKey(t)),
		`url = "https://github.com/o/a"`)
	tests := []struct {
		name   string
		status map[string]int
		exit   int
	}{
		{"pass", nil, 0},
		{"확인 못 함 only", map[string]int{"/repos/o/a/installation": 503}, 0},
		{"실패", map[string]int{"/repos/o/a/installation": 404}, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stdout, stderr, err := runDoctorAt(t, path, doctorGitHub(t, tt.status), doctorDocker(t))
			if got := exitCode(err); got != tt.exit {
				t.Errorf("exit = %d, want %d\n%s", got, tt.exit, stdout)
			}
			var re reportedError
			if err != nil && !errors.As(err, &re) {
				t.Errorf("err = %v, want a reportedError", err)
			}
			if stderr != "" {
				t.Errorf("stderr = %q", stderr)
			}
		})
	}
}

func TestDoctorNoTokenInOutput(t *testing.T) {
	classic, fine := fixtureToken(t, "ghp_"), fixtureToken(t, "github_pat_")
	path := writeDoctorConfig(t, fmt.Sprintf("token-file = %q", writeTokenFile(t, classic)),
		`url = "https://github.com/o/a"`,
		fmt.Sprintf("url = \"https://github.com/o/b\"\ntoken-file = %q", writeTokenFile(t, fine)),
		`url = "https://`+classic+`@github.com/o/c"`,
		fmt.Sprintf("url = \"https://github.com/o/d\"\ntoken-file = %q", fine),
	)
	// Both runner lists answer 418 with the Authorization header quoted.
	gh := doctorGitHub(t, map[string]int{"/repos/o/a/actions/runners": 418, "/repos/o/b/actions/runners": 401})
	stdout, stderr, err := runDoctorAt(t, path, gh, doctorDocker(t))
	for _, token := range []string{classic, fine} {
		if strings.Contains(stdout, token) || strings.Contains(stderr, token) {
			t.Errorf("a fixture token is in the output:\n%s\n%s", stdout, stderr)
		}
	}
	if exitCode(err) != 1 || !strings.Contains(stdout, "GitHub targets[2]") {
		t.Errorf("err = %v\n%s", err, stdout)
	}
}

func TestDoctorUsage(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if got := dispatch(context.Background(), newEnv(&stdout, &stderr), []string{"doctor", "extra"}, commands()); got != 1 {
		t.Errorf("exit = %d, want 1", got)
	}
	if !strings.Contains(stderr.String(), "usage: doctor [-config path]") || stdout.Len() != 0 {
		t.Errorf("stdout = %q, stderr = %q", stdout.String(), stderr.String())
	}
}

var _ doctor.DockerProbe = brokenDocker{}
