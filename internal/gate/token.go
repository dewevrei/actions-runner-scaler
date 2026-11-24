package gate

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"io"
	"log/slog"
)

// tokenBytes is the token's entropy before encoding (43 base64url chars).
const tokenBytes = 32

// redacted replaces the token wherever it might be printed. It matches
// logging.Placeholder, written out because gate imports no internal package.
const redacted = "<redacted>"

// Token is a runner's job check token. It stays in process memory; String,
// GoString and LogValue hide it so it cannot leak through fmt or slog.
// The zero value matches nothing.
type Token struct {
	// The text sits behind a pointer so that fmt, printing a struct that
	// holds a Token in an unexported field (where it cannot call String),
	// shows an address instead of the text.
	value *string
}

// Reveal returns the token text, for handing it to the runner container.
func (t Token) Reveal() string {
	if t.value == nil {
		return ""
	}
	return *t.value
}

// Equal reports, in constant time, whether presented is this token.
func (t Token) Equal(presented string) bool {
	// ConstantTimeCompare treats two empty inputs as equal.
	if t.value == nil || *t.value == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(*t.value), []byte(presented)) == 1
}

// String hides the token.
func (t Token) String() string { return redacted }

// GoString hides the token from %#v.
func (t Token) GoString() string { return redacted }

// LogValue hides the token from slog.
func (t Token) LogValue() slog.Value { return slog.StringValue(redacted) }

// TokenGenerator makes job check tokens from a random source.
type TokenGenerator struct {
	random io.Reader
}

// NewTokenGenerator returns a generator reading from random. A nil random
// means crypto/rand.Reader; tests inject a fixed source.
func NewTokenGenerator(random io.Reader) *TokenGenerator {
	if random == nil {
		random = rand.Reader
	}
	return &TokenGenerator{random: random}
}

// New returns a fresh token: 32 random bytes, base64url without padding.
func (g *TokenGenerator) New() (Token, error) {
	b := make([]byte, tokenBytes)
	if _, err := io.ReadFull(g.random, b); err != nil {
		return Token{}, fmt.Errorf("gate: read random token bytes: %w", err)
	}
	v := base64.RawURLEncoding.EncodeToString(b)
	return Token{value: &v}, nil
}
