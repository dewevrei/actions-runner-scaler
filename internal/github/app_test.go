package github

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dewevrei/actions-runner-scaler/internal/logging"
	"github.com/dewevrei/actions-runner-scaler/internal/retry"
)

// appEnv is an App against a server that answers the installation lookup
// and, for App.REST, the installation token and the runner list.
type appEnv struct {
	app     *App
	srv     *httptest.Server
	secrets *logging.Secrets
	clock   *fakeClock

	mu       sync.Mutex
	requests []*http.Request
	// install answers GET /orgs/test-org/installation.
	install http.HandlerFunc
}

func newAppEnv(t *testing.T) *appEnv {
	t.Helper()
	e := &appEnv{clock: newFakeClock(), secrets: logging.NewSecrets()}
	key := rsaKey(t)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /orgs/test-org/installation", func(w http.ResponseWriter, r *http.Request) {
		e.mu.Lock()
		e.requests = append(e.requests, r)
		install := e.install
		e.mu.Unlock()
		if err := verifyJWT(&key.PublicKey, strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")); err != nil {
			t.Errorf("installation lookup is not signed by the App: %v", err)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		install(w, r)
	})
	mux.HandleFunc("POST /app/installations/42/access_tokens", func(w http.ResponseWriter, r *http.Request) {
		e.mu.Lock()
		e.requests = append(e.requests, r)
		e.mu.Unlock()
		writeJSON(w, http.StatusCreated, map[string]any{
			"token":      "fake-app-rest-token-abcdefghijklmnop",
			"expires_at": e.clock.Now().Add(time.Hour).Format(time.RFC3339),
		})
	})
	mux.HandleFunc("GET /orgs/test-org/actions/runners", func(w http.ResponseWriter, r *http.Request) {
		e.mu.Lock()
		e.requests = append(e.requests, r)
		e.mu.Unlock()
		if r.Header.Get("Authorization") != "Bearer fake-app-rest-token-abcdefghijklmnop" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"runners": []any{map[string]any{"id": 1, "name": "a", "busy": true}}})
	})
	e.srv = httptest.NewServer(mux)
	t.Cleanup(e.srv.Close)

	signer, err := NewAppSigner("Iv1.testclient", key, e.clock, e.secrets)
	if err != nil {
		t.Fatal(err)
	}
	e.app, err = NewApp(AppConfig{
		BaseURL:    e.srv.URL,
		Signer:     signer,
		HTTPClient: e.srv.Client(),
		Clock:      e.clock,
		Secrets:    e.secrets,
	})
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func (e *appEnv) paths() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	var out []string
	for _, r := range e.requests {
		out = append(out, r.Method+" "+r.URL.Path)
	}
	return out
}

// TestInstallationUsesJWT proves 0002 SPEC R108: the lookup carries the App
// JWT, not an installation token, and reads id and permissions.
func TestInstallationUsesJWT(t *testing.T) {
	e := newAppEnv(t)
	e.install = func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{
			"id":          99,
			"account":     map[string]any{"login": "test-org"},
			"permissions": map[string]any{"organization_self_hosted_runners": "write", "metadata": "read"},
		})
	}
	inst, err := e.app.Installation(context.Background(), testTarget)
	if err != nil {
		t.Fatal(err)
	}
	if inst.ID != 99 || inst.Permissions["organization_self_hosted_runners"] != "write" || inst.Permissions["metadata"] != "read" {
		t.Errorf("installation = %+v", inst)
	}
	got := e.paths()
	if len(got) != 1 || got[0] != "GET /orgs/test-org/installation" {
		t.Errorf("requests = %v, want only the installation lookup (no installation token)", got)
	}
	if h := e.requests[0].Header; h.Get("X-GitHub-Api-Version") == "" || h.Get("Accept") == "" {
		t.Errorf("headers = %v", h)
	}
}

func TestInstallationBadResponsesAreErrorsWithoutBody(t *testing.T) {
	const marker = "body-marker-do-not-quote"
	tests := map[string]string{
		"missing id":          `{"permissions":{"a":"write"},"x":"` + marker + `"}`,
		"zero id":             `{"id":0,"permissions":{"a":"write"},"x":"` + marker + `"}`,
		"negative id":         `{"id":-3,"permissions":{"a":"write"},"x":"` + marker + `"}`,
		"string id":           `{"id":"` + marker + `","permissions":{"a":"write"}}`,
		"missing permissions": `{"id":5,"x":"` + marker + `"}`,
		"null permissions":    `{"id":5,"permissions":null,"x":"` + marker + `"}`,
		"non-string level":    `{"id":5,"permissions":{"a":7},"x":"` + marker + `"}`,
		"not json":            marker,
		"array":               `[` + marker + `]`,
	}
	for name, body := range tests {
		t.Run(name, func(t *testing.T) {
			e := newAppEnv(t)
			e.install = func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(body)) }
			_, err := e.app.Installation(context.Background(), testTarget)
			if err == nil {
				t.Fatal("no error")
			}
			if strings.Contains(err.Error(), marker) {
				t.Errorf("error quotes the body: %v", err)
			}
			assertOp(t, err, retry.OpGetInstallation)
		})
	}
}

// TestInstallationNotInstalled proves 0002 SPEC R109.
func TestInstallationNotInstalled(t *testing.T) {
	e := newAppEnv(t)
	e.install = func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusNotFound, map[string]string{"message": "Not Found"})
	}
	ctx := context.Background()
	_, err := e.app.Installation(ctx, testTarget)
	if err == nil {
		t.Fatal("no error")
	}
	if !strings.Contains(err.Error(), "GitHub App is not installed on organization test-org") {
		t.Errorf("error = %q", err)
	}
	var sc retry.StatusCoder
	if !errors.As(err, &sc) || sc.HTTPStatus() != http.StatusNotFound {
		t.Errorf("error %v has no 404 status", err)
	}
	if c := retry.Classify(ctx, retry.OpGetInstallation, err); c != retry.Permanent {
		t.Errorf("Classify = %v, want permanent", c)
	}
	assertOp(t, err, retry.OpGetInstallation)
}

func TestInstallationErrorClasses(t *testing.T) {
	tests := []struct {
		name   string
		status int
		header map[string]string
		want   retry.Class
	}{
		{"unauthorized", 401, nil, retry.Permanent},
		{"server error", 502, nil, retry.Transient},
		{"too many requests", 429, map[string]string{"Retry-After": "30"}, retry.RateLimited},
		{"forbidden with retry-after", 403, map[string]string{"Retry-After": "30"}, retry.RateLimited},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			e := newAppEnv(t)
			e.install = func(w http.ResponseWriter, r *http.Request) {
				for k, v := range tc.header {
					w.Header().Set(k, v)
				}
				w.WriteHeader(tc.status)
			}
			ctx := context.Background()
			_, err := e.app.Installation(ctx, testTarget)
			if err == nil {
				t.Fatal("no error")
			}
			if c := retry.Classify(ctx, retry.OpGetInstallation, err); c != tc.want {
				t.Errorf("Classify = %v, want %v (error %v)", c, tc.want, err)
			}
		})
	}
}

func TestInstallationRejectsNonOrganizationTarget(t *testing.T) {
	e := newAppEnv(t)
	_, err := e.app.Installation(context.Background(), Target{})
	if err == nil {
		t.Fatal("no error")
	}
	if n := len(e.paths()); n != 0 {
		t.Errorf("requests = %d, want 0", n)
	}
}

// TestCheckPermission proves 0002 SPEC R110.
func TestCheckPermission(t *testing.T) {
	ctx := context.Background()
	const want = "GitHub App installation on organization test-org lacks organization_self_hosted_runners: write"
	for name, perms := range map[string]map[string]string{
		"missing":   {"metadata": "read"},
		"read":      {"organization_self_hosted_runners": "read"},
		"admin":     {"organization_self_hosted_runners": "admin"},
		"nil":       nil,
		"wrong key": {"repository_self_hosted_runners": "write"},
	} {
		err := CheckPermission(Installation{ID: 1, Permissions: perms}, testTarget)
		if err == nil {
			t.Errorf("%s: no error", name)
			continue
		}
		if err.Error() != want {
			t.Errorf("%s: error = %q, want %q", name, err, want)
		}
		if !errors.Is(err, retry.ErrTargetSetup) {
			t.Errorf("%s: error does not wrap ErrTargetSetup", name)
		}
		if c := retry.Classify(ctx, retry.OpGetInstallation, err); c != retry.Permanent {
			t.Errorf("%s: Classify = %v, want permanent", name, c)
		}
	}
	ok := Installation{ID: 1, Permissions: map[string]string{"organization_self_hosted_runners": "write"}}
	if err := CheckPermission(ok, testTarget); err != nil {
		t.Errorf("write: %v", err)
	}
}

// TestAppREST checks that the REST client of an installation buys tokens
// for that installation through the App's signer and HTTP client.
func TestAppREST(t *testing.T) {
	e := newAppEnv(t)
	if _, err := e.app.REST(0); err == nil {
		t.Error("REST(0): no error")
	}
	rest, err := e.app.REST(42)
	if err != nil {
		t.Fatal(err)
	}
	runners, err := rest.ListRunners(context.Background(), testTarget)
	if err != nil {
		t.Fatal(err)
	}
	if len(runners) != 1 || runners[0].Name != "a" || !runners[0].Busy {
		t.Errorf("runners = %+v", runners)
	}
	got := strings.Join(e.paths(), ",")
	if got != "POST /app/installations/42/access_tokens,GET /orgs/test-org/actions/runners" {
		t.Errorf("requests = %s", got)
	}
}

func TestNewAppRejectsBadConfig(t *testing.T) {
	e := newAppEnv(t)
	signer, err := NewAppSigner("Iv1.testclient", rsaKey(t), e.clock, e.secrets)
	if err != nil {
		t.Fatal(err)
	}
	good := AppConfig{BaseURL: DefaultAPIBaseURL, Signer: signer, HTTPClient: e.srv.Client(), Clock: e.clock, Secrets: e.secrets}
	if _, err := NewApp(good); err != nil {
		t.Fatalf("good config: %v", err)
	}
	for name, mutate := range map[string]func(*AppConfig){
		"no signer":  func(c *AppConfig) { c.Signer = nil },
		"no client":  func(c *AppConfig) { c.HTTPClient = nil },
		"no clock":   func(c *AppConfig) { c.Clock = nil },
		"no secrets": func(c *AppConfig) { c.Secrets = nil },
		"bad base":   func(c *AppConfig) { c.BaseURL = "api.github.com" },
		"query base": func(c *AppConfig) { c.BaseURL = "https://api.github.com?x=1" },
	} {
		cfg := good
		mutate(&cfg)
		if _, err := NewApp(cfg); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
}
