package daemon

import (
	"crypto/sha256"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"github.com/dewevrei/actions-runner-scaler/internal/retry"
	"github.com/dewevrei/actions-runner-scaler/internal/scaler"
)

// credentialKey names the credential whose GitHub waits a target uses: an
// App installation or a personal access token for the REST calls (0003
// SPEC R280), or an App for its JWT's installation lookups (0002 SPEC
// R189, one per App). A token is held by the SHA-256 of its value, so the
// key never holds the token. The kind tells the three apart, so keys of
// different kinds never collide whatever their values.
type credentialKey struct {
	kind           keyKind
	installationID int64
	tokenDigest    [sha256.Size]byte
	appClientID    string
}

// keyKind is the kind of credential a credentialKey names.
type keyKind uint8

const (
	keyInstallation keyKind = iota + 1
	keyToken
	keyApp
)

// installationKey is the key of App installation id (0002 SPEC R266).
func installationKey(id int64) credentialKey {
	return credentialKey{kind: keyInstallation, installationID: id}
}

// tokenKey is the key of a personal access token; equal tokens give equal
// keys (0003 SPEC R280).
func tokenKey(token string) credentialKey {
	return credentialKey{kind: keyToken, tokenDigest: sha256.Sum256([]byte(token))}
}

// appKey is the key of the installation-lookup wait of the App with
// clientID; one App's targets share it (0002 SPEC R189).
func appKey(clientID string) credentialKey {
	return credentialKey{kind: keyApp, appClientID: clientID}
}

// credentialWaits holds the R52 waits of the GitHub calls, one per
// credential key, so the targets of one installation, one token or one App
// share them and the targets of another credential do not (0002 SPEC R189,
// R266, 0003 SPEC R280). Entries are never removed: a target that moves to another
// installation leaves its old entry to the targets still bound to it. It
// is safe for concurrent use and takes no other lock.
type credentialWaits struct {
	clock retry.Clock

	mu    sync.Mutex
	byKey map[credentialKey]*retry.NextAllowed
}

// newCredentialWaits returns an empty credentialWaits. It returns an error
// when clock is nil.
func newCredentialWaits(clock retry.Clock) (*credentialWaits, error) {
	if clock == nil {
		return nil, errors.New("daemon: nil clock")
	}
	return &credentialWaits{clock: clock, byKey: make(map[credentialKey]*retry.NextAllowed)}, nil
}

// get returns the waits of key, made on first use; every call with an
// equal key returns the same pointer.
func (w *credentialWaits) get(key credentialKey) *retry.NextAllowed {
	w.mu.Lock()
	defer w.mu.Unlock()
	if n := w.byKey[key]; n != nil {
		return n
	}
	n, err := retry.NewNextAllowed(w.clock)
	if err != nil {
		// newCredentialWaits checked the clock.
		panic("daemon: " + err.Error())
	}
	w.byKey[key] = n
	return n
}

// restOp reports whether op is a GitHub REST call made with the target's
// credential (0002 SPEC R241, R242, R266; 0003 SPEC R279, R280).
func restOp(op retry.Op) bool {
	switch op {
	case retry.OpListRepoRunners, retry.OpFindRepoRunner, retry.OpCheckTokenAccess:
		return true
	}
	return false
}

// targetLimits is one target's scaler.RateLimits: the REST calls use the
// waits of the credential the target is bound to (0002 SPEC R266, 0003
// SPEC R280), every other call the target's own (R265). Before the first
// bind no credential is bound and the REST calls use the target's own
// waits; no REST call is made then, since no REST client is bound.
type targetLimits struct {
	own  *retry.NextAllowed
	inst *atomic.Pointer[retry.NextAllowed]
}

var _ scaler.RateLimits = targetLimits{}

// pinnedLimits returns a targetLimits whose REST calls always use rest: the
// start phase's cleanup, whose client is not bound yet (0002 SPEC R266).
func pinnedLimits(own, rest *retry.NextAllowed) targetLimits {
	p := new(atomic.Pointer[retry.NextAllowed])
	p.Store(rest)
	return targetLimits{own: own, inst: p}
}

// of returns the waits op is recorded in.
func (l targetLimits) of(op retry.Op) *retry.NextAllowed {
	if restOp(op) {
		if n := l.inst.Load(); n != nil {
			return n
		}
	}
	return l.own
}

func (l targetLimits) Record(op retry.Op, err error) bool { return l.of(op).Record(op, err) }
func (l targetLimits) Allowed(op retry.Op) bool           { return l.of(op).Allowed(op) }
func (l targetLimits) At(op retry.Op) time.Time           { return l.of(op).At(op) }
