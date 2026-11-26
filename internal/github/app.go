package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/dewevrei/actions-runner-scaler/internal/logging"
	"github.com/dewevrei/actions-runner-scaler/internal/retry"
)

// AppConfig configures an App.
type AppConfig struct {
	// BaseURL is the API root, such as DefaultAPIBaseURL.
	BaseURL string
	// Signer makes the App JWT every App request carries.
	Signer *AppSigner
	// HTTPClient sends the requests.
	HTTPClient HTTPDoer
	// Clock decides when a cached installation token is replaced.
	Clock Clock
	// Secrets receives every installation token the App's REST clients buy.
	Secrets *logging.Secrets
}

// App sends requests authenticated as the GitHub App itself, with its JWT,
// and builds the per-installation REST clients. It is safe for concurrent
// use.
type App struct {
	apiClient
	signer *AppSigner
}

// NewApp returns an App.
func NewApp(cfg AppConfig) (*App, error) {
	switch {
	case cfg.Signer == nil:
		return nil, errors.New("github: nil App signer")
	case cfg.HTTPClient == nil:
		return nil, errors.New("github: nil HTTP client")
	case cfg.Clock == nil:
		return nil, errors.New("github: nil clock")
	case cfg.Secrets == nil:
		return nil, errors.New("github: nil secrets")
	}
	api, err := newAPIClient(cfg.BaseURL, cfg.HTTPClient, cfg.Clock, cfg.Secrets)
	if err != nil {
		return nil, err
	}
	return &App{apiClient: api, signer: cfg.Signer}, nil
}

// Installation is the App's installation on one target.
type Installation struct {
	// ID is the installation ID.
	ID int64
	// Permissions maps each permission the installation holds to its level.
	Permissions map[string]string
}

// Installation looks up the App's installation on t with
// GET /repos/{owner}/{repo}/installation, which takes the App JWT only (0002
// SPEC R236). A 404 means the App is not installed on the repository and is
// permanent; its message is "GitHub App is not installed on repository
// <owner>/<repo>" (0002 SPEC R237).
func (a *App) Installation(ctx context.Context, t Target) (Installation, error) {
	const op = retry.OpGetInstallation
	if !t.valid() {
		return Installation{}, &retry.OpError{Op: op, Err: errors.New("target is not a repository")}
	}
	jwt, err := a.signer.Token(ctx)
	if err != nil {
		return Installation{}, &retry.OpError{Op: op, Err: err}
	}
	u := a.endpoint(strings.Split(t.InstallationPath(), "/")...)
	req, err := a.newRequest(ctx, http.MethodGet, u, "Bearer "+jwt)
	if err != nil {
		return Installation{}, &retry.OpError{Op: op, Err: err}
	}
	resp, err := a.send(req)
	if err != nil {
		return Installation{}, &retry.OpError{Op: op, Err: err}
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		err := a.responseError(resp)
		var se *StatusError
		if errors.As(err, &se) && se.Code == http.StatusNotFound {
			se.Message = "GitHub App is not installed on " + t.String()
		}
		return Installation{}, &retry.OpError{Op: op, Err: err}
	}
	body, err := readBody(resp.Body, maxBodyBytes)
	if err != nil {
		return Installation{}, &retry.OpError{Op: op, Err: err}
	}
	inst, err := decodeInstallation(body)
	if err != nil {
		return Installation{}, &retry.OpError{Op: op, Err: err}
	}
	return inst, nil
}

// decodeInstallation reads id and permissions, both required. Its errors do
// not quote the body.
func decodeInstallation(body []byte) (Installation, error) {
	var resp struct {
		ID          *int64             `json:"id"`
		Permissions *map[string]string `json:"permissions"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return Installation{}, decodeError(err)
	}
	switch {
	case resp.ID == nil || *resp.ID <= 0:
		return Installation{}, errors.New("decode response: id is missing or not positive")
	case resp.Permissions == nil || *resp.Permissions == nil:
		return Installation{}, errors.New("decode response: permissions is missing")
	}
	return Installation{ID: *resp.ID, Permissions: *resp.Permissions}, nil
}

// CheckPermission returns an error that wraps retry.ErrTargetSetup unless
// the installation holds t's permission, administration: write, at exactly
// that level; the error names the permission and the repository (0002 SPEC
// R238).
func CheckPermission(inst Installation, t Target) error {
	name, level := t.Permission()
	if inst.Permissions[name] == level {
		return nil
	}
	return &sentinelError{
		sentinel: retry.ErrTargetSetup,
		err:      fmt.Errorf("GitHub App installation on %s lacks %s: %s", t, name, level),
	}
}

// REST returns a REST client for installationID that shares the App's
// signer and HTTP client.
func (a *App) REST(installationID int64) (*REST, error) {
	return NewREST(RESTConfig{
		BaseURL:        a.base.String(),
		InstallationID: installationID,
		Signer:         a.signer,
		HTTPClient:     a.http,
		Clock:          a.clock,
		Secrets:        a.secrets,
	})
}
