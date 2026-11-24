// Package logging keeps secret values out of log output (R31).
package logging

import (
	"cmp"
	"context"
	"fmt"
	"log/slog"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"sync"
)

// Placeholder replaces every redacted value.
const Placeholder = "<redacted>"

// urlQuery matches the query string of an absolute URL or of a path that
// starts with "/" at a word boundary. Requiring the slash keeps prose such as
// "what? no" intact. The query must start with a character that Placeholder
// does not, so redacting twice changes nothing.
var urlQuery = regexp.MustCompile("(?m)((?:[A-Za-z][A-Za-z0-9+.-]*://|(?:^|[\\s\"'`(=])/)[^\\s?#\"'`<>]*)\\?[^\\s#\"'`<>]+")

// RedactURLQueries replaces the query string of every URL or path in s.
func RedactURLQueries(s string) string {
	return urlQuery.ReplaceAllString(s, "${1}?"+Placeholder)
}

// privateKeyPEM matches a PEM private key block, or a BEGIN line through the
// end of the text when the END line is cut off.
var privateKeyPEM = regexp.MustCompile(`(?s)-----BEGIN [A-Z ]*PRIVATE KEY-----.*?(?:-----END [A-Z ]*PRIVATE KEY-----|\z)`)

// githubToken matches GitHub token formats by their documented prefixes.
var githubToken = regexp.MustCompile(`(?:gh[sporu]_|github_pat_)[A-Za-z0-9_]+`)

// redactKnownFormats replaces private keys and GitHub tokens that were never
// registered, such as a key pasted into a file or a token echoed in an error.
func redactKnownFormats(s string) string {
	s = privateKeyPEM.ReplaceAllLiteralString(s, Placeholder)
	return githubToken.ReplaceAllLiteralString(s, Placeholder)
}

// Secrets is a set of secret values registered at runtime, such as JIT
// configs, installation tokens, and job-check tokens. It is safe for
// concurrent use.
type Secrets struct {
	mu       sync.RWMutex
	counts   map[string]int
	replacer *strings.Replacer
}

// NewSecrets returns an empty set.
func NewSecrets() *Secrets {
	return &Secrets{counts: make(map[string]int), replacer: strings.NewReplacer()}
}

// Register adds value to the set. Empty values are ignored because they
// would match everywhere. A value registered n times stays until it is
// unregistered n times, so independent owners of an equal value do not
// expose it for each other.
func (s *Secrets) Register(value string) {
	if value == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.counts[value]++
	if s.counts[value] == 1 {
		s.rebuild()
	}
}

// Unregister removes one registration of value.
func (s *Secrets) Unregister(value string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n, ok := s.counts[value]
	if !ok {
		return
	}
	if n > 1 {
		s.counts[value] = n - 1
		return
	}
	delete(s.counts, value)
	s.rebuild()
}

// rebuild must be called with mu held for writing.
func (s *Secrets) rebuild() {
	values := make([]string, 0, len(s.counts))
	for v := range s.counts {
		values = append(values, v)
	}
	// Longer values first so a secret that contains another is replaced whole.
	slices.SortFunc(values, func(a, b string) int {
		return cmp.Or(cmp.Compare(len(b), len(a)), strings.Compare(a, b))
	})
	pairs := make([]string, 0, 2*len(values))
	for _, v := range values {
		pairs = append(pairs, v, Placeholder)
	}
	s.replacer = strings.NewReplacer(pairs...)
}

// Redact replaces registered values, URL query strings, PEM private keys and
// GitHub tokens in text.
func (s *Secrets) Redact(text string) string {
	s.mu.RLock()
	r := s.replacer
	s.mu.RUnlock()
	// Queries first, so a secret inside a query is not replaced twice.
	// Registered values before known formats, so a format match inside a
	// registered value cannot split it and leave the rest readable.
	return redactKnownFormats(r.Replace(RedactURLQueries(text)))
}

// Redactor rewrites text so it no longer contains sensitive values.
type Redactor interface {
	Redact(text string) string
}

// RedactingHandler is a slog.Handler that redacts the message, every
// attribute key and value, and group names before passing a record to the
// wrapped handler.
type RedactingHandler struct {
	next     slog.Handler
	redactor Redactor
	// scopes holds WithGroup and WithAttrs calls. They are replayed on each
	// record so attributes bound before a secret was registered are still
	// redacted.
	scopes []scope
}

// scope is one WithGroup (group set) or WithAttrs (attrs set) call.
type scope struct {
	group string
	attrs []slog.Attr
}

// NewRedactingHandler wraps next so that its output passes through r.
func NewRedactingHandler(next slog.Handler, r Redactor) *RedactingHandler {
	return &RedactingHandler{next: next, redactor: r}
}

// Enabled reports whether the wrapped handler handles level.
func (h *RedactingHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.next.Enabled(ctx, level)
}

// Handle redacts rec and passes it to the wrapped handler.
func (h *RedactingHandler) Handle(ctx context.Context, rec slog.Record) error {
	next := h.next
	for _, sc := range h.scopes {
		if sc.attrs == nil {
			next = next.WithGroup(h.redactor.Redact(sc.group))
			continue
		}
		next = next.WithAttrs(h.redactAttrs(sc.attrs))
	}
	out := slog.NewRecord(rec.Time, rec.Level, h.redactor.Redact(rec.Message), rec.PC)
	rec.Attrs(func(a slog.Attr) bool {
		out.AddAttrs(h.redactAttr(a))
		return true
	})
	return next.Handle(ctx, out)
}

// WithAttrs returns a handler that adds attrs to every record.
func (h *RedactingHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	if len(attrs) == 0 {
		return h
	}
	return h.with(scope{attrs: slices.Clone(attrs)})
}

// WithGroup returns a handler that nests later attributes under name.
func (h *RedactingHandler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}
	return h.with(scope{group: name})
}

func (h *RedactingHandler) with(sc scope) *RedactingHandler {
	return &RedactingHandler{
		next:     h.next,
		redactor: h.redactor,
		scopes:   append(slices.Clip(h.scopes), sc),
	}
}

func (h *RedactingHandler) redactAttrs(attrs []slog.Attr) []slog.Attr {
	out := make([]slog.Attr, len(attrs))
	for i, a := range attrs {
		out[i] = h.redactAttr(a)
	}
	return out
}

func (h *RedactingHandler) redactAttr(a slog.Attr) slog.Attr {
	v := a.Value.Resolve()
	switch v.Kind() {
	case slog.KindString:
		v = slog.StringValue(h.redactor.Redact(v.String()))
	case slog.KindGroup:
		v = slog.GroupValue(h.redactAttrs(v.Group())...)
	case slog.KindAny:
		v = h.redactAny(v.Any())
	}
	return slog.Attr{Key: h.redactor.Redact(a.Key), Value: v}
}

// redactAny always replaces x with its redacted text. Passing x on would let
// the wrapped handler serialize it another way, such as json.Marshal reaching
// fields that the printed form hides behind a pointer.
func (h *RedactingHandler) redactAny(x any) slog.Value {
	return slog.StringValue(h.redactor.Redact(printed(x)))
}

// printed formats x as text that redaction can check. Errors, Stringers,
// byte slices and scalars print as their text. Any other value prints as its
// type name only: %+v of a struct, map or slice can show a URL query without
// a scheme, header values or bytes as numbers, which redaction cannot
// recognize, so it fails closed.
//
// fmt.Sprint is used for error and Stringer values because it turns a nil
// receiver into "<nil>" and a panicking method into an error marker instead
// of crashing.
func printed(x any) string {
	switch t := x.(type) {
	case nil:
		return "<nil>"
	case error, fmt.Stringer:
		return fmt.Sprint(t)
	case []byte:
		return string(t)
	}
	// A value whose String or Error method has a pointer receiver, such as
	// url.URL, is printed through a pointer to a copy.
	v := reflect.ValueOf(x)
	p := reflect.New(v.Type())
	p.Elem().Set(v)
	switch m := p.Interface().(type) {
	case error, fmt.Stringer:
		return fmt.Sprint(m)
	}
	if isScalar(v.Kind()) {
		return fmt.Sprint(x)
	}
	return fmt.Sprintf("<%T>", x)
}

// isScalar reports kinds that hold a single bool, number or string.
func isScalar(k reflect.Kind) bool {
	switch k {
	case reflect.Bool, reflect.String,
		reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64,
		reflect.Float32, reflect.Float64, reflect.Complex64, reflect.Complex128:
		return true
	}
	return false
}
