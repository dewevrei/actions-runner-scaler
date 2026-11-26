//go:build integration

// Read-only integration test of the App calls against real GitHub (0002 PLAN
// M9-12). It makes no change: an installation lookup, a permission check, a
// runner group lookup and one runner list. Run it only with the user's
// approval:
//
//	SCALER_TEST_CONFIG=<path of a 0002 config.toml> \
//	go test -tags integration -run Integration -v ./internal/github/
//
// It prints only pass or fail and counts, never IDs, names or tokens.
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

// TestIntegrationTargets runs the read-only calls for every target of the
// configuration named by SCALER_TEST_CONFIG (0002 SPEC R108, R110, R111,
// R148).
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
	key, err := config.ReadAppPrivateKey(cfg.GitHub.AppPrivateKeyFile)
	if err != nil {
		t.Fatal("the App private key cannot be read")
	}

	secrets := logging.NewSecrets()
	clock := retry.SystemClock()
	signer, err := github.NewAppSigner(cfg.GitHub.AppClientID, key, clock, secrets)
	if err != nil {
		t.Fatal("cannot make the App signer")
	}
	app, err := github.NewApp(github.AppConfig{
		BaseURL:    github.DefaultAPIBaseURL,
		Signer:     signer,
		HTTPClient: &http.Client{Transport: github.NewTransport()},
		Clock:      clock,
		Secrets:    secrets,
	})
	if err != nil {
		t.Fatal("cannot make the App client")
	}

	for i, tc := range cfg.Targets {
		t.Run("target"+string(rune('0'+i%10)), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			target, err := github.ParseTargetURL(tc.URL)
			if err != nil {
				t.Fatal("the target URL is not an organization URL")
			}
			inst, err := app.Installation(ctx, target)
			if err != nil {
				t.Fatalf("installation lookup failed%s", failure(err))
			}
			if err := github.CheckPermission(inst, target); err != nil {
				t.Fatal("the installation lacks the self-hosted runners permission")
			}
			ss, err := github.NewScaleSetClient(github.ScaleSetConfig{
				ConfigURL:      target.ConfigURL(),
				InstallationID: inst.ID,
				Signer:         signer,
				Secrets:        secrets,
			})
			if err != nil {
				t.Fatal("cannot make the scale set client")
			}
			if _, err := ss.RunnerGroupID(ctx, tc.RunnerGroup); err != nil {
				t.Fatalf("runner group lookup failed%s", failure(err))
			}
			rest, err := app.REST(inst.ID)
			if err != nil {
				t.Fatal("cannot make the REST client")
			}
			runners, err := rest.ListRunners(ctx, target)
			if err != nil {
				t.Fatalf("runner list failed%s", failure(err))
			}
			t.Logf("pass: installation found, permission ok, runner group found, %d runners listed", len(runners))
		})
	}
}

// failure describes err by its HTTP status only: an error text can name an
// organization or a group.
func failure(err error) string {
	var sc retry.StatusCoder
	if errors.As(err, &sc) {
		return fmt.Sprintf(" (HTTP %d)", sc.HTTPStatus())
	}
	return ""
}
