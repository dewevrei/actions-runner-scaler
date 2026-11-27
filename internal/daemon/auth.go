package daemon

import (
	"context"
	"errors"

	"github.com/dewevrei/actions-runner-scaler/internal/github"
	"github.com/dewevrei/actions-runner-scaler/internal/retry"
)

// targetAuth is one target's credential (0003 SPEC Design 자격): the start
// phase's check of it and the clients it makes. The start order and the
// error scope around it are the same for every credential.
type targetAuth interface {
	// check runs the start phase's credential check through call in sc and
	// returns the key of the credential whose REST waits the target's
	// clients use. A permanent failure returns sc.fail's error.
	check(ctx context.Context, call stepCaller, sc callScope, w authWaits) (credentialKey, error)
	// clients makes the target's REST and scale set clients for key, the
	// key check returned.
	clients(key credentialKey) (TargetGitHub, error)
	// checkWait returns the op of check's call and the waits it is
	// recorded in, which the target error wait reads (0002 SPEC R174).
	checkWait(w authWaits) (retry.Op, *retry.NextAllowed)
}

// authWaits are the R52 waits a credential check can use.
type authWaits struct {
	// installation is the wait of the default App's installation lookups,
	// the server's (0002 SPEC R189).
	installation *retry.NextAllowed
	// credentials holds the REST waits of each credential (0003 SPEC R280)
	// and the installation-lookup wait of every other App.
	credentials *credentialWaits
}

// app returns the installation-lookup wait of the App with clientID: one
// per App, since GitHub limits App JWT requests per App. An empty clientID
// names the default App, whose wait is the server's.
func (w authWaits) app(clientID string) *retry.NextAllowed {
	if clientID == "" {
		return w.installation
	}
	return w.credentials.get(appKey(clientID))
}

// appSource is one App's side: the default App, which the targets without
// a credential of their own share, or a target's own App.
type appSource interface {
	// Installation looks up the App's installation on t with the App JWT
	// (0002 SPEC R236).
	Installation(ctx context.Context, t github.Target) (github.Installation, error)
	// Target makes the REST and scale set clients of t for installationID.
	Target(t github.Target, installationID int64) (TargetGitHub, error)
}

// appAuth is the credential of an App target: the installation lookup and
// its permission check (0002 SPEC R236~R238).
type appAuth struct {
	src appSource
	t   github.Target
	// app is the client ID whose installation-lookup wait check uses;
	// empty for the default App (authWaits.app).
	app string
}

var _ targetAuth = appAuth{}

func (a appAuth) checkWait(w authWaits) (retry.Op, *retry.NextAllowed) {
	return retry.OpGetInstallation, w.app(a.app)
}

// check looks the installation up with the wait of its App (0002 SPEC
// R189, one per App) and returns its ID's key (R266), so targets share REST
// waits exactly when they share an installation, whether through the
// default App or their own. A retry that gets another ID moves the target
// to that ID's waits at bind (R193).
func (a appAuth) check(ctx context.Context, call stepCaller, sc callScope, w authWaits) (credentialKey, error) {
	op, limits := a.checkWait(w)
	sc.limits = limits
	var inst github.Installation
	err := call(ctx, sc, "installation", op, func(ctx context.Context) error {
		var err error
		inst, err = a.src.Installation(ctx, a.t)
		return err
	})
	if err != nil {
		return credentialKey{}, err
	}
	if err := github.CheckPermission(inst, a.t); err != nil {
		return credentialKey{}, sc.fail("installation", err)
	}
	return installationKey(inst.ID), nil
}

func (a appAuth) clients(key credentialKey) (TargetGitHub, error) {
	return a.src.Target(a.t, key.installationID)
}

// tokenSource is the clients of one token target, made with its token.
type tokenSource interface {
	// CheckRepositoryAccess makes 0003 SPEC R279's call with the token.
	CheckRepositoryAccess(ctx context.Context, t github.Target) (github.Access, error)
	// TokenTarget returns the target's REST and scale set clients.
	TokenTarget(t github.Target) (TargetGitHub, error)
}

// tokenAuth is the credential of a token target (0003 SPEC R278, R279).
// It holds the token's key, not the token; newSource makes the clients
// with the token at each start, so a failure to make them fails only this
// target (0002 SPEC R172). Only the target's goroutine uses it.
type tokenAuth struct {
	t         github.Target
	key       credentialKey
	newSource func() (tokenSource, error)
	// src is the source the last check made; clients uses it.
	src tokenSource
}

var _ targetAuth = (*tokenAuth)(nil)

func (a *tokenAuth) checkWait(w authWaits) (retry.Op, *retry.NextAllowed) {
	return retry.OpCheckTokenAccess, w.credentials.get(a.key)
}

// check makes the clients, then R279's call in the token's waits, not
// R189's (0003 SPEC R280). A refusal fails with R279's sentence alone as
// the last error.
func (a *tokenAuth) check(ctx context.Context, call stepCaller, sc callScope, w authWaits) (credentialKey, error) {
	src, err := a.newSource()
	if err != nil {
		return credentialKey{}, sc.fail("github clients", err)
	}
	a.src = src
	op, limits := a.checkWait(w)
	sc.limits = limits
	fail := sc.fail
	sc.fail = func(where string, err error) error {
		if r, ok := asRefusal(err); ok {
			return fail("", r)
		}
		return fail(where, err)
	}
	err = call(ctx, sc, "token check", op, func(ctx context.Context) error {
		// The answer's scopes and expiration are doctor's (0003 SPEC R301,
		// R317); the daemon neither keeps nor logs them.
		_, err := src.CheckRepositoryAccess(ctx, a.t)
		return err
	})
	if err != nil {
		return credentialKey{}, err
	}
	return a.key, nil
}

func (a *tokenAuth) clients(credentialKey) (TargetGitHub, error) {
	if a.src == nil {
		return nil, errors.New("daemon: token clients used before the token check")
	}
	return a.src.TokenTarget(a.t)
}

// refusal is a token check GitHub refused (0003 SPEC R279). Its text is
// R279's sentence alone; Unwrap keeps the operation and the status.
type refusal struct {
	text string
	err  error
}

func (r *refusal) Error() string { return r.text }
func (r *refusal) Unwrap() error { return r.err }

// logged returns what a log line shows of err: for a refusal the error it
// wraps, whose text keeps the operation and the status (0003 PLAN M30).
func logged(err error) error {
	if r, ok := err.(*refusal); ok {
		return r.err
	}
	return err
}

// asRefusal returns err as a refusal when it is R279's 401, 403 or 404,
// which github.CheckRepositoryAccess marks with retry.ErrTargetSetup and
// whose StatusError Message is R279's sentence.
func asRefusal(err error) (*refusal, bool) {
	var se *github.StatusError
	if !errors.Is(err, retry.ErrTargetSetup) || !errors.As(err, &se) || se.Message == "" {
		return nil, false
	}
	return &refusal{text: se.Message, err: err}, true
}
