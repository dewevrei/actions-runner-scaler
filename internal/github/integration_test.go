//go:build integration

// Read-only integration test of the target checks against real GitHub (0002
// PLAN M9-12, 0002 PLAN M20-6, 0003 PLAN M31). It makes no change: for an
// App target an installation lookup, a permission check and one repository
// runner list; for a token target one token check (0003 SPEC R279). Run it
// only with the user's approval:
//
//	SCALER_TEST_CONFIG=<path of a config.toml> \
//	go test -tags integration -run Integration -v ./internal/github/
//
// It prints only pass or fail, status codes and counts, never IDs, names
// or tokens.
package github_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/dewevrei/actions-runner-scaler/internal/config"
	"github.com/dewevrei/actions-runner-scaler/internal/github"
	"github.com/dewevrei/actions-runner-scaler/internal/logging"
	"github.com/dewevrei/actions-runner-scaler/internal/retry"
)

// TestIntegrationTargets runs the read-only checks for every target of the
// configuration named by SCALER_TEST_CONFIG (0002 SPEC R236, R238, R241;
// 0003 SPEC R279).
func TestIntegrationTargets(t *testing.T) {
	path := os.Getenv("SCALER_TEST_CONFIG")
	if path == "" {
		t.Skip("SCALER_TEST_CONFIG is not set")
	}
	cfg, err := config.Load(path)
	if err != nil {
		// The text can quote a configuration value; the test needs only to
		// say that it failed.
		t.Fatal("the configuration does not load")
	}

	secrets := logging.NewSecrets()
	clock := retry.SystemClock()
	httpClient := &http.Client{Transport: github.NewTransport()}
	var app *github.App
	if cfg.GitHub.AppPrivateKeyFile != "" {
		app = newIntegrationApp(t, cfg, clock, secrets, httpClient)
	}
	for i, tc := range cfg.Targets {
		t.Run(fmt.Sprintf("target%d", i), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			target, err := github.ParseTargetURL(tc.URL)
			if err != nil {
				t.Fatal("the target URL is not a repository URL")
			}
			tokenFile := tc.TokenFile
			if tokenFile == "" {
				tokenFile = cfg.GitHub.TokenFile
			}
			if tokenFile != "" {
				checkToken(ctx, t, target, tokenFile, clock, secrets, httpClient)
				return
			}
			if app == nil {
				t.Fatal("the target has no credential")
			}
			checkApp(ctx, t, app, target)
		})
	}
}

// newIntegrationApp makes the App client of cfg.
func newIntegrationApp(t *testing.T, cfg *config.Config, clock retry.Clock, secrets *logging.Secrets, httpClient *http.Client) *github.App {
	t.Helper()
	key, err := config.ReadAppPrivateKey(cfg.GitHub.AppPrivateKeyFile)
	if err != nil {
		t.Fatal("the App private key cannot be read")
	}
	signer, err := github.NewAppSigner(cfg.GitHub.AppClientID, key, clock, secrets)
	if err != nil {
		t.Fatal("cannot make the App signer")
	}
	app, err := github.NewApp(github.AppConfig{
		BaseURL:    github.DefaultAPIBaseURL,
		Signer:     signer,
		HTTPClient: httpClient,
		Clock:      clock,
		Secrets:    secrets,
	})
	if err != nil {
		t.Fatal("cannot make the App client")
	}
	return app
}

// checkApp looks the installation up, checks its permission and lists the
// repository's runners once.
func checkApp(ctx context.Context, t *testing.T, app *github.App, target github.Target) {
	t.Helper()
	inst, err := app.Installation(ctx, target)
	if err != nil {
		t.Fatalf("installation lookup failed%s", failure(err))
	}
	if err := github.CheckPermission(inst, target); err != nil {
		t.Fatal("the installation lacks the administration permission")
	}
	rest, err := app.REST(inst.ID)
	if err != nil {
		t.Fatal("cannot make the REST client")
	}
	runners, err := rest.ListRunners(ctx, target)
	if err != nil {
		t.Fatalf("runner list failed%s", failure(err))
	}
	t.Logf("pass: installation found, permission ok, %d runners listed", len(runners))
}

// checkToken makes the token check (0003 SPEC R279) once with the token in
// tokenFile. The answer's expiration is not printed (0003 SPEC R317).
func checkToken(ctx context.Context, t *testing.T, target github.Target, tokenFile string, clock retry.Clock, secrets *logging.Secrets, httpClient *http.Client) {
	t.Helper()
	token, err := config.ReadTokenFile(tokenFile)
	if err != nil {
		t.Fatal("the token file cannot be read")
	}
	rest, err := github.NewTokenREST(github.TokenRESTConfig{
		BaseURL:    github.DefaultAPIBaseURL,
		Token:      token,
		HTTPClient: httpClient,
		Clock:      clock,
		Secrets:    secrets,
	})
	if err != nil {
		t.Fatal("cannot make the token REST client")
	}
	access, err := rest.CheckRepositoryAccess(ctx, target)
	if err != nil {
		t.Fatalf("token check failed%s", failure(err))
	}
	t.Logf("pass: token check answered %d, scopes header present %v, expiration header present %v",
		access.Status, access.ScopesPresent, access.ExpirationPresent)
}

// failure describes err by its HTTP status only: an error text can name a
// repository.
func failure(err error) string {
	var sc retry.StatusCoder
	if errors.As(err, &sc) {
		return fmt.Sprintf(" (HTTP %d)", sc.HTTPStatus())
	}
	return ""
}
