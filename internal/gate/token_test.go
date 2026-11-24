package gate

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"log/slog"
	"strings"
	"testing"
)

func newTestToken(t *testing.T) Token {
	t.Helper()
	tok, err := NewTokenGenerator(rand.Reader).New()
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	return tok
}

func TestTokenFormat(t *testing.T) {
	tok := newTestToken(t)
	s := tok.Reveal()
	if len(s) != 43 {
		t.Fatalf("token length = %d, want 43", len(s))
	}
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		t.Fatalf("token is not unpadded base64url: %v", err)
	}
	if len(b) != 32 {
		t.Errorf("decoded token = %d bytes, want 32", len(b))
	}
}

func TestTokensDiffer(t *testing.T) {
	a, b := newTestToken(t), newTestToken(t)
	if a.Reveal() == b.Reveal() {
		t.Fatal("two tokens are equal")
	}
	if a.Equal(b.Reveal()) || b.Equal(a.Reveal()) {
		t.Error("a token matches another token")
	}
}

func TestTokenUsesInjectedReader(t *testing.T) {
	seed := bytes.Repeat([]byte{0xfb}, 32)
	tok, err := NewTokenGenerator(bytes.NewReader(seed)).New()
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	if want := base64.RawURLEncoding.EncodeToString(seed); tok.Reveal() != want {
		t.Errorf("token = %q, want %q", tok.Reveal(), want)
	}
}

func TestTokenShortRandom(t *testing.T) {
	tok, err := NewTokenGenerator(bytes.NewReader(make([]byte, 31))).New()
	if err == nil {
		t.Fatal("New() with 31 random bytes succeeded")
	}
	if tok.Equal("") || tok.Reveal() != "" {
		t.Error("failed New() returned a usable token")
	}
}

func TestTokenEqual(t *testing.T) {
	tok := newTestToken(t)
	s := tok.Reveal()
	tests := []struct {
		name      string
		presented string
		want      bool
	}{
		{name: "same", presented: s, want: true},
		{name: "empty", presented: "", want: false},
		{name: "prefix", presented: s[:42], want: false},
		{name: "longer", presented: s + "A", want: false},
		{name: "last char changed", presented: s[:42] + flip(s[42]), want: false},
		{name: "redacted text", presented: redacted, want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tok.Equal(tt.presented); got != tt.want {
				t.Errorf("Equal() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestZeroTokenMatchesNothing(t *testing.T) {
	var tok Token
	if tok.Equal("") {
		t.Error("zero Token matches empty string")
	}
}

func TestEmptyValueTokenMatchesNothing(t *testing.T) {
	if (Token{value: new(string)}).Equal("") {
		t.Error("token holding an empty value matches empty string")
	}
}

func TestNilReaderUsesCryptoRand(t *testing.T) {
	tok, err := NewTokenGenerator(nil).New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if len(tok.Reveal()) != 43 {
		t.Errorf("token length = %d, want 43", len(tok.Reveal()))
	}
}

func TestTokenDoesNotPrint(t *testing.T) {
	tok := newTestToken(t)
	secret := tok.Reveal()

	// fmt cannot call String on an unexported field, so these print the
	// Token's own fields.
	hidden := struct {
		name string
		tok  Token
	}{name: "r1", tok: tok}
	exported := struct{ Tok Token }{Tok: tok}

	var jsonLog, textLog bytes.Buffer
	for _, l := range []*slog.Logger{
		slog.New(slog.NewJSONHandler(&jsonLog, nil)),
		slog.New(slog.NewTextHandler(&textLog, nil)),
	} {
		l.Info("start", "token", tok, "hidden", hidden, "exported", exported, "ptr", &hidden)
	}

	outputs := map[string]string{
		"slog json": jsonLog.String(),
		"slog text": textLog.String(),
	}
	values := map[string]any{"token": tok, "hidden": hidden, "&hidden": &hidden, "exported": exported}
	// A variable list of verbs keeps vet from rejecting %d and %s on structs,
	// which is exactly the misuse being tested.
	verbs := []string{"%v", "%+v", "%#v", "%s", "%q", "%d", "%x"}
	for name, v := range values {
		for _, verb := range verbs {
			outputs[name+" "+verb] = fmt.Sprintf(verb, v)
		}
	}
	for name, out := range outputs {
		if strings.Contains(out, secret) {
			t.Errorf("%s output contains the token: %q", name, out)
		}
	}
}

// The token prints as the placeholder the logging package uses.
func TestTokenPrintsPlaceholder(t *testing.T) {
	tok := newTestToken(t)
	const want = "<redacted>"
	for name, got := range map[string]string{
		"String":   tok.String(),
		"GoString": tok.GoString(),
		"LogValue": tok.LogValue().String(),
	} {
		if got != want {
			t.Errorf("%s() = %q, want %q", name, got, want)
		}
	}
}

func flip(c byte) string {
	if c == 'A' {
		return "B"
	}
	return "A"
}
