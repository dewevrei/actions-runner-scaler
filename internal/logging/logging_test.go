package logging

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
)

const (
	jitConfig  = "fake-jit-config-value"
	installTok = "fake-installation-token"
	checkTok   = "fake-job-check-token"
)

// Unregistered secrets in known formats, kept short so they are not real
// token formats. The PEM header is split so scanners do not flag the file.
const (
	pemBody  = "ZmFrZS1rZXktbWF0ZXJpYWw="
	pemBlock = "-----BEGIN " + "RSA PRIVATE KEY-----\n" + pemBody + "\n-----END RSA PRIVATE KEY-----\n"
	ghToken  = "ghs_" + "fakeInstallation01"
	patToken = "github_pat_" + "fake_Pat02"
)

type stringer struct{ s string }

func (s stringer) String() string { return s.s }

type valuer struct{ s string }

func (v valuer) LogValue() slog.Value { return slog.StringValue(v.s) }

type config struct {
	Name  string
	Token string
}

type label string

func newLogger(t *testing.T, json bool) (*slog.Logger, *Secrets, *bytes.Buffer) {
	t.Helper()
	var buf bytes.Buffer
	var base slog.Handler = slog.NewTextHandler(&buf, nil)
	if json {
		base = slog.NewJSONHandler(&buf, nil)
	}
	secrets := NewSecrets()
	secrets.Register(jitConfig)
	secrets.Register(installTok)
	secrets.Register(checkTok)
	return slog.New(NewRedactingHandler(base, secrets)), secrets, &buf
}

func assertClean(t *testing.T, out string, leaks ...string) {
	t.Helper()
	for _, s := range append([]string{jitConfig, installTok, checkTok}, leaks...) {
		if strings.Contains(out, s) {
			t.Errorf("output contains %q:\n%s", s, out)
		}
	}
	if !strings.Contains(out, Placeholder) {
		t.Errorf("output has no %s:\n%s", Placeholder, out)
	}
}

func TestRedactingHandler(t *testing.T) {
	const query = "sessionId=abc123&token=xyz789"
	url := "https://example.invalid/_apis/messages?" + query
	tests := []struct {
		name string
		log  func(*slog.Logger)
	}{
		{"message", func(l *slog.Logger) { l.Info("got " + jitConfig) }},
		{"string attr", func(l *slog.Logger) { l.Info("m", "jit", jitConfig) }},
		{"error attr", func(l *slog.Logger) {
			l.Error("m", "err", fmt.Errorf("start: %w", errors.New("bad token "+installTok)))
		}},
		{"error in group", func(l *slog.Logger) {
			l.Info("m", slog.Group("runner", "err", errors.New(checkTok)))
		}},
		{"nested group", func(l *slog.Logger) {
			l.Info("m", slog.Group("a", slog.Group("b", "v", checkTok)))
		}},
		{"stringer", func(l *slog.Logger) { l.Info("m", "s", stringer{"x " + jitConfig}) }},
		{"log valuer", func(l *slog.Logger) { l.Info("m", "v", valuer{installTok}) }},
		{"bytes", func(l *slog.Logger) { l.Info("m", "b", []byte(jitConfig)) }},
		{"named string", func(l *slog.Logger) { l.Info("m", "s", label("x "+checkTok)) }},
		{"with attrs", func(l *slog.Logger) { l.With("jit", jitConfig).Info("m") }},
		{"with group", func(l *slog.Logger) { l.WithGroup("g").With("t", checkTok).Info("m", "u", installTok) }},
		{"url in message", func(l *slog.Logger) { l.Info("request GET " + url + " failed") }},
		{"url in attr", func(l *slog.Logger) { l.Info("m", "url", url) }},
		{"url in error", func(l *slog.Logger) { l.Warn("m", "err", fmt.Errorf("Get %q: EOF", url)) }},
		{"pem in message", func(l *slog.Logger) { l.Info("read " + pemBlock) }},
		{"pem in error", func(l *slog.Logger) { l.Error("m", "err", errors.New("bad key: "+pemBlock)) }},
		{"token in attr", func(l *slog.Logger) { l.Info("m", "auth", "Bearer "+ghToken) }},
		{"token in error", func(l *slog.Logger) { l.Error("m", "err", fmt.Errorf("token %s: 401", patToken)) }},
		{"secret as attr key", func(l *slog.Logger) { l.Info("m", ghToken, 1) }},
		{"secret as group attr key", func(l *slog.Logger) { l.Info("m", slog.Group(checkTok, "n", 1)) }},
		{"secret as group name", func(l *slog.Logger) { l.WithGroup(jitConfig).Info("m", "n", 1) }},
		{"token as group name", func(l *slog.Logger) { l.WithGroup(patToken).With("n", 1).Info("m") }},
	}
	for _, format := range []string{"text", "json"} {
		for _, tt := range tests {
			t.Run(format+"/"+tt.name, func(t *testing.T) {
				logger, _, buf := newLogger(t, format == "json")
				tt.log(logger)
				assertClean(t, buf.String(), query, "abc123", "xyz789", pemBody, ghToken, patToken)
			})
		}
	}
}

func TestRedactingHandlerKeepsStructure(t *testing.T) {
	logger, _, buf := newLogger(t, true)
	logger.WithGroup("pool").With("name", "deploy").Info("started", "count", 2, slog.Group("runner", "token", checkTok))

	var got map[string]any
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("decode %s: %v", buf, err)
	}
	pool, ok := got["pool"].(map[string]any)
	if !ok {
		t.Fatalf("no pool group in %s", buf)
	}
	if pool["name"] != "deploy" || pool["count"] != float64(2) {
		t.Errorf("plain attrs changed: %s", buf)
	}
	runner, ok := pool["runner"].(map[string]any)
	if !ok || runner["token"] != Placeholder {
		t.Errorf("nested group not redacted in place: %s", buf)
	}
}

type inner struct{ Token string }

type outer struct{ In *inner }

type body struct{ Body []byte }

type nested struct{ P **body }

// byteDigits is secret as %+v prints it inside a []byte: decimal numbers.
func byteDigits(secret string) string {
	return strings.Trim(fmt.Sprint([]byte(secret)), "[]")
}

// TestRedactingHandlerFailsClosed covers composite values, whose printed
// form can hold secrets that redaction cannot recognize: a URL query without
// a scheme, header values, bytes as numbers, or fields that json.Marshal
// would reach behind a pointer. They are logged as their type name only.
func TestRedactingHandlerFailsClosed(t *testing.T) {
	const query = "token=abc123"
	u := url.URL{Scheme: "https", Host: "h.invalid", Path: "/p", RawQuery: query}
	rel := &url.URL{Path: "/p", RawQuery: query}
	bodyPtr := &body{Body: []byte(installTok)}
	var tokenArray [len(installTok)]byte
	copy(tokenArray[:], installTok)
	tests := []struct {
		name string
		val  any
		want string // the logged value
	}{
		{"url value", u, "https://h.invalid/p?" + Placeholder},
		{"url pointer", &u, "https://h.invalid/p?" + Placeholder},
		{"struct", config{Name: "pool", Token: installTok}, "<logging.config>"},
		{"map", map[string]string{"T": checkTok}, "<map[string]string>"},
		{"pointer in struct", outer{In: &inner{Token: installTok}}, "<logging.outer>"},
		{"pointer to struct with pointer", &outer{In: &inner{Token: installTok}}, "<*logging.outer>"},
		{"map of pointers", map[string]*inner{"a": {Token: installTok}}, "<map[string]*logging.inner>"},
		{"slice of pointers", []*inner{{Token: installTok}}, "<[]*logging.inner>"},
		{"slice of url pointers", []*url.URL{&u}, "<[]*url.URL>"},
		{"url value in struct", struct{ U url.URL }{u}, "<struct { U url.URL }>"},
		{"relative url pointer in struct", struct{ U *url.URL }{rel}, "<struct { U *url.URL }>"},
		{"map of relative url pointers", map[string]*url.URL{"u": rel}, "<map[string]*url.URL>"},
		{"url values", url.Values{"token": {"abc123"}}, "<url.Values>"},
		{"http header", http.Header{"Authorization": {"Bearer abc123"}}, "<http.Header>"},
		{"struct with registered secret", inner{Token: checkTok}, "<logging.inner>"},
		{"byte slice in struct", struct{ Body []byte }{[]byte(installTok)}, "<struct { Body []uint8 }>"},
		{"byte slice behind nested pointers", &nested{P: &bodyPtr}, "<*logging.nested>"},
		{"byte array in struct", struct{ A [len(installTok)]byte }{tokenArray}, "<struct { A [23]uint8 }>"},
		{"byte slice in map", map[string][]byte{"k": []byte(installTok)}, "<map[string][]uint8>"},
		{"byte slice in interface slice", []any{1, []byte(installTok)}, "<[]interface {}>"},
		{"pointer to scalar", new(int), "<*int>"},
	}
	for _, format := range []string{"text", "json"} {
		for _, tt := range tests {
			t.Run(format+"/"+tt.name, func(t *testing.T) {
				logger, _, buf := newLogger(t, format == "json")
				logger.Info("m", "v", tt.val)
				out := buf.String()
				for _, s := range []string{installTok, checkTok, query, "abc123", byteDigits(installTok)} {
					if strings.Contains(out, s) {
						t.Errorf("output contains %q:\n%s", s, out)
					}
				}
				if !strings.Contains(out, tt.want) {
					t.Errorf("output lacks %q:\n%s", tt.want, out)
				}
			})
		}
	}
}

// Scalars of named types are logged as their text, redacted.
func TestRedactingHandlerScalars(t *testing.T) {
	type mode int
	logger, _, buf := newLogger(t, false)
	logger.Info("m", "mode", mode(7), "label", label("a"), "nil", nil)
	for _, want := range []string{"mode=7", "label=a", "nil=<nil>"} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("output %q lacks %q", buf.String(), want)
		}
	}
}

type nilErr struct{ msg string }

func (e *nilErr) Error() string { return e.msg }

type nilStringer struct{ s string }

func (s *nilStringer) String() string { return s.s }

type panicStringer struct{}

func (panicStringer) String() string { panic("boom") }

func TestRedactingHandlerTypedNil(t *testing.T) {
	var e *nilErr
	var s *nilStringer
	tests := []struct {
		name string
		val  any
		want string
	}{
		{"nil error pointer", e, "<nil>"},
		{"nil stringer pointer", s, "<nil>"},
		{"panicking stringer", panicStringer{}, "PANIC"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			logger, _, buf := newLogger(t, false)
			logger.Info("m", "v", tt.val)
			if !strings.Contains(buf.String(), tt.want) {
				t.Fatalf("output %q lacks %q", buf.String(), tt.want)
			}
		})
	}
}

func TestRedactingHandlerLateRegistration(t *testing.T) {
	logger, secrets, buf := newLogger(t, false)
	const late = "fake-token-registered-later"
	bound := logger.With("token", late)
	secrets.Register(late)
	bound.Info("m")
	assertClean(t, buf.String(), late)
}

func TestSecretsUnregister(t *testing.T) {
	s := NewSecrets()
	s.Register(installTok)
	s.Register(installTok)
	s.Unregister(installTok)
	if got := s.Redact(installTok); got != Placeholder {
		t.Fatalf("still registered once: Redact = %q", got)
	}
	s.Unregister(installTok)
	if got := s.Redact(installTok); got != installTok {
		t.Fatalf("after last Unregister: Redact = %q", got)
	}
	s.Unregister("never registered")
}

func TestSecretsRedact(t *testing.T) {
	s := NewSecrets()
	s.Register("")
	s.Register("abc")
	s.Register("abcdef")
	tests := []struct {
		in, want string
	}{
		{"plain text", "plain text"},
		{"x abcdef y", "x " + Placeholder + " y"},
		{"abc abcdef", Placeholder + " " + Placeholder},
		{"https://h.invalid/p?a=1&b=2 next", "https://h.invalid/p?" + Placeholder + " next"},
		{`Get "https://h.invalid/p?a=1#frag": EOF`, `Get "https://h.invalid/p?` + Placeholder + `#frag": EOF`},
		{"wss://h.invalid/x?t=1", "wss://h.invalid/x?" + Placeholder},
		{"https://h.invalid/p no query", "https://h.invalid/p no query"},
		{"what? no url", "what? no url"},
		{"/_apis/x?token=abc", "/_apis/x?" + Placeholder},
		{"request GET /_apis/x?a=1&b=2 failed", "request GET /_apis/x?" + Placeholder + " failed"},
		{`Get "/x?y=1": EOF`, `Get "/x?` + Placeholder + `": EOF`},
		{"path=/x?y=1", "path=/x?" + Placeholder},
		{"(/x?y=1)", "(/x?" + Placeholder},
		{"and/or? maybe", "and/or? maybe"},
		{"1/2? half", "1/2? half"},
		{"see a/b?c", "see a/b?c"},
		{"key:\n" + pemBlock + "done", "key:\n" + Placeholder + "\ndone"},
		{"-----BEGIN ENCRYPTED PRIVATE KEY-----\nAAAA\n-----END ENCRYPTED PRIVATE KEY----- x", Placeholder + " x"},
		{"-----BEGIN OPENSSH PRIVATE KEY-----\nAAAA\n-----END OPENSSH PRIVATE KEY-----", Placeholder},
		{"cut: -----BEGIN PRIVATE KEY-----\nAAAA\nBBBB", "cut: " + Placeholder},
		{"-----BEGIN PUBLIC KEY-----\nAAAA", "-----BEGIN PUBLIC KEY-----\nAAAA"},
		{"t=" + ghToken + ".", "t=" + Placeholder + "."},
		{"ghp_a ghu_b gho_c ghr_d", strings.Repeat(Placeholder+" ", 3) + Placeholder},
		{"x-access-token:" + ghToken + "@github.com", "x-access-token:" + Placeholder + "@github.com"},
		{patToken + " end", Placeholder + " end"},
		{"ghs_ ghx_xyz gh_xyz", "ghs_ ghx_xyz gh_xyz"},
	}
	for _, tt := range tests {
		got := s.Redact(tt.in)
		if got != tt.want {
			t.Errorf("Redact(%q) = %q, want %q", tt.in, got, tt.want)
		}
		if again := s.Redact(got); again != got {
			t.Errorf("Redact not idempotent: %q -> %q", got, again)
		}
	}
}

func TestSecretsConcurrent(t *testing.T) {
	var buf bytes.Buffer
	secrets := NewSecrets()
	logger := slog.New(NewRedactingHandler(slog.NewTextHandler(&lockedWriter{w: &buf}, nil), secrets))
	var wg sync.WaitGroup
	for i := range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			v := fmt.Sprintf("fake-secret-%02d", i)
			secrets.Register(v)
			logger.Info("m", "v", v)
			secrets.Unregister(v)
		}()
	}
	wg.Wait()
	if strings.Contains(buf.String(), "fake-secret-") {
		t.Fatalf("secret logged while registered:\n%s", buf.String())
	}
}

type lockedWriter struct {
	mu sync.Mutex
	w  *bytes.Buffer
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}
