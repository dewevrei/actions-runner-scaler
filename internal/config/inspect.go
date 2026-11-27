package config

import (
	"crypto/rsa"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"slices"
)

// Inspection is the result of decoding and validating a configuration file
// with the key and token file checks kept apart from the other problems,
// for a report that gives each file its own line (0003 SPEC R304).
type Inspection struct {
	// Config holds the decoded values over the defaults, also when there
	// are problems.
	Config *Config
	// Problems are the decoding and validation errors other than those of
	// Files, each prefixed with its key.
	Problems []error
	// Files has one entry for each key or token file key that is set and
	// decoded, in the order checked.
	Files []FileCheck

	// all holds every error in Load's order.
	all []error
	// decodeFailed holds the keys and tables that failed to decode; invalid
	// the keys of validation errors.
	decodeFailed map[string]bool
	invalid      map[string]bool
}

// FileCheck is the check of one key or token file.
type FileCheck struct {
	// Key is github.app-private-key-file, github.token-file,
	// targets[<i>].app-private-key-file or targets[<i>].token-file.
	Key string
	// Err is nil when the file passed. It holds neither the path nor the
	// file's content (R31, 0003 SPEC R277).
	Err error
}

// Inspect reads, decodes and validates the configuration file at path like
// Load. Only a read or syntax error is returned as err; every other problem
// is in the Inspection.
func Inspect(path string) (*Inspection, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("config %s: %w", path, err)
	}
	return inspect(data, "config "+path)
}

// inspect decodes and validates data; source names it in errors.
func inspect(data []byte, source string) (*Inspection, error) {
	cfg, decodeErrs, failed, err := decode(data, source)
	if err != nil {
		return nil, err
	}
	p := cfg.validate(failed)
	in := &Inspection{
		Config:       cfg,
		Problems:     slices.Clone(decodeErrs),
		Files:        p.files,
		all:          slices.Concat(decodeErrs, p.errors()),
		decodeFailed: failed,
		invalid:      make(map[string]bool),
	}
	for _, f := range p.found {
		in.invalid[f.key] = true
		if !f.file {
			in.Problems = append(in.Problems, f.err)
		}
	}
	cfg.blankRefused()
	return in, nil
}

// blankRefused clears the values refused as secrets (0003 SPEC R276, R314),
// so no later printer of the configuration shows them (R277). Validation
// has recorded their errors already.
func (c *Config) blankRefused() {
	if looksLikeToken(c.GitHub.TokenFile) {
		c.GitHub.TokenFile = ""
	}
	for i := range c.Targets {
		t := &c.Targets[i]
		if looksLikeToken(t.TokenFile) {
			t.TokenFile = ""
		}
		if ContainsCredential(t.URL) {
			t.URL = ""
		}
	}
}

// Failed reports whether key failed to decode, or sits in a table that did,
// or has a validation error of its own.
func (in *Inspection) Failed(key string) bool {
	return in.invalid[key] || inTable(in.decodeFailed, key)
}

// result returns the configuration, or every error joined in Load's order.
func (in *Inspection) result() (*Config, error) {
	if err := errors.Join(in.all...); err != nil {
		return nil, err
	}
	return in.Config, nil
}

// Credentials holds the secrets a validated configuration names. Every way
// fmt, slog and encoding/json print it shows only "<redacted>" (0003 SPEC
// R277). That holds only for a Credentials printed itself or held in an
// exported field: fmt cannot call methods through an unexported field, so
// %v of a struct holding it there shows the tokens. Callers never format a
// struct that holds a Credentials.
type Credentials struct {
	// AppKey is the App private key, nil without an App.
	AppKey *rsa.PrivateKey
	// DefaultToken is the token of github.token-file, empty without one.
	DefaultToken string
	// TargetTokens is index-aligned with Config.Targets: the token of each
	// target's token-file, empty for a target without one.
	TargetTokens []string
	// TargetAppKeys is index-aligned with Config.Targets: the App private
	// key of each target's app-private-key-file, nil for a target without
	// one. A target with neither a token nor an App key uses the default
	// credential. Nothing outside this package reads it yet; validation
	// refuses a per-target App until it does (TODO(wiring)).
	TargetAppKeys []*rsa.PrivateKey
}

const redacted = "<redacted>"

// String implements fmt.Stringer.
func (Credentials) String() string { return redacted }

// GoString implements fmt.GoStringer.
func (Credentials) GoString() string { return redacted }

// Format implements fmt.Formatter, so no verb prints a field.
func (Credentials) Format(f fmt.State, _ rune) { io.WriteString(f, redacted) }

// LogValue implements slog.LogValuer.
func (Credentials) LogValue() slog.Value { return slog.StringValue(redacted) }

// MarshalJSON implements json.Marshaler: slog's JSON handler marshals a
// Credentials held in another value's field without calling LogValue.
func (Credentials) MarshalJSON() ([]byte, error) { return []byte(`"` + redacted + `"`), nil }

// LoadCredentials reads the default App private key, when cfg has one, every
// target's App private key and every token file of cfg once (0003 SPEC R271,
// R273). Errors are prefixed with their key like those of Load and contain
// no path or secret.
func LoadCredentials(cfg *Config) (Credentials, error) {
	var (
		creds Credentials
		p     problems
	)
	if path := cfg.GitHub.AppPrivateKeyFile; path != "" {
		key, err := ReadAppPrivateKey(path)
		p.file("github.app-private-key-file", err)
		creds.AppKey = key
	}
	if path := cfg.GitHub.TokenFile; path != "" {
		token, err := ReadTokenFile(path)
		p.file("github.token-file", err)
		creds.DefaultToken = token
	}
	creds.TargetTokens = make([]string, len(cfg.Targets))
	creds.TargetAppKeys = make([]*rsa.PrivateKey, len(cfg.Targets))
	for i, t := range cfg.Targets {
		if t.AppPrivateKeyFile != "" {
			key, err := ReadAppPrivateKey(t.AppPrivateKeyFile)
			p.file(fmt.Sprintf("targets[%d].app-private-key-file", i), err)
			creds.TargetAppKeys[i] = key
		}
		if t.TokenFile != "" {
			token, err := ReadTokenFile(t.TokenFile)
			p.file(fmt.Sprintf("targets[%d].token-file", i), err)
			creds.TargetTokens[i] = token
		}
	}
	if err := errors.Join(p.errors()...); err != nil {
		return Credentials{}, err
	}
	return creds, nil
}
