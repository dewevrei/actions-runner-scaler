package gate

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"time"
)

// VerdictPath is the job check server's only route.
const VerdictPath = "/v1/verdict"

// Header names the job-started hook sends (image/job-started.sh).
const (
	authorizationHeader = "Authorization"
	runnerNameHeader    = "X-Runner-Name"
	bearerPrefix        = "Bearer "
)

// Server timeouts. A poll is answered without waiting on anything slow, so
// these only bound clients that send or read slowly.
const (
	readHeaderTimeout = 5 * time.Second
	// readTimeout bounds the whole request, body included. Without it the
	// header deadline is cleared once the headers are read, and a client
	// that declares a body and never sends it holds the connection while
	// net/http drains the body after the handler returns.
	readTimeout  = 10 * time.Second
	writeTimeout = 10 * time.Second
	// maxHeaderBytes caps the request header. The hook sends two short
	// headers besides what curl adds.
	maxHeaderBytes = 8 << 10
)

// Response bodies. They are fixed bytes without a trailing newline; the hook
// accepts a 200 body only when, with white space removed, it equals one of
// the three verdict bodies exactly.
var (
	bodyAllow        = []byte(`{"verdict":"allow"}`)
	bodyDeny         = []byte(`{"verdict":"deny"}`)
	bodyPending      = []byte(`{"verdict":"pending"}`)
	bodyUnauthorized = []byte(`{"reason":"unknown runner name or wrong token"}`)
	bodyNoAuth       = []byte(`{"reason":"missing runner name or bearer token"}`)
	bodyNotFound     = []byte(`{"reason":"not found"}`)
	bodyInternal     = []byte(`{"reason":"internal error"}`)
)

// Verdict is a runner's job verdict as the job check server reports it.
// The zero Verdict is VerdictPending.
type Verdict int

// Verdicts.
const (
	// VerdictPending means the job has not been judged yet.
	VerdictPending Verdict = iota
	// VerdictAllow means the job may run its steps.
	VerdictAllow
	// VerdictDeny means the job was rejected.
	VerdictDeny
)

func (v Verdict) String() string {
	switch v {
	case VerdictPending:
		return "pending"
	case VerdictAllow:
		return "allow"
	case VerdictDeny:
		return "deny"
	}
	return "Verdict(" + strconv.Itoa(int(v)) + ")"
}

// body returns the 200 response body for v, or nil for an unknown value.
func (v Verdict) body() []byte {
	switch v {
	case VerdictPending:
		return bodyPending
	case VerdictAllow:
		return bodyAllow
	case VerdictDeny:
		return bodyDeny
	}
	return nil
}

// Authenticator looks up a runner's verdict by its name and job check token.
type Authenticator interface {
	// Authenticate compares presented with the named runner's token in
	// constant time. On a match it may record at as the runner's busy time
	// and returns the verdict and true. On a mismatch, an unknown name or a
	// revoked token it records nothing and returns false.
	Authenticate(name, presented string, at time.Time) (Verdict, bool)
}

// Clock is the server's time source.
type Clock interface {
	Now() time.Time
}

type wallClock struct{}

func (wallClock) Now() time.Time { return time.Now() }

// Server is the job check HTTP server. It answers GET /v1/verdict on a
// loopback address and nothing else (R43).
type Server struct {
	addr  string
	auth  Authenticator
	clock Clock
	log   *slog.Logger
	srv   *http.Server

	mu sync.Mutex
	ln net.Listener
}

// NewServer returns a server that will listen on addr, which must be
// <loopback IP>:<port>; port 0 picks a free port. A nil clock means the
// wall clock and a nil log discards.
func NewServer(addr string, auth Authenticator, clock Clock, log *slog.Logger) (*Server, error) {
	if err := checkLoopback(addr); err != nil {
		return nil, err
	}
	if auth == nil {
		return nil, errors.New("gate: nil Authenticator")
	}
	if clock == nil {
		clock = wallClock{}
	}
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	s := &Server{addr: addr, auth: auth, clock: clock, log: log}
	s.srv = &http.Server{
		Handler:           s,
		ReadHeaderTimeout: readHeaderTimeout,
		ReadTimeout:       readTimeout,
		WriteTimeout:      writeTimeout,
		MaxHeaderBytes:    maxHeaderBytes,
		// Without this, "OPTIONS *" gets a 200 that ServeHTTP never sees.
		DisableGeneralOptionsHandler: true,
		// net/http reports handler panics and accept errors here.
		ErrorLog: slog.NewLogLogger(log.Handler(), slog.LevelWarn),
	}
	// Each hook poll is a new curl process, so a kept-alive connection
	// would only sit idle.
	s.srv.SetKeepAlivesEnabled(false)
	return s, nil
}

// checkLoopback reports whether addr is <loopback IP>:<port>, the rule
// config applies to jobcheck.listen, except that port 0 is allowed.
func checkLoopback(addr string) error {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("gate: listen address must be <loopback IP>:<port>, got %q", addr)
	}
	if ip, err := netip.ParseAddr(host); err != nil || !ip.IsLoopback() {
		return fmt.Errorf("gate: listen host must be a loopback IP address, got %q", host)
	}
	if _, err := strconv.ParseUint(port, 10, 16); err != nil {
		return fmt.Errorf("gate: listen port must be 0-65535, got %q", port)
	}
	return nil
}

// Listen binds the listen address. The daemon calls it during startup so a
// failure is reported before the first session (R46).
func (s *Server) Listen() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ln != nil {
		return errors.New("gate: server already listening")
	}
	ln, err := net.Listen("tcp", s.addr)
	if err != nil {
		return fmt.Errorf("gate: listen on %s: %w", s.addr, err)
	}
	s.ln = ln
	return nil
}

// Addr returns the bound address, or nil before Listen.
func (s *Server) Addr() net.Addr {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ln == nil {
		return nil
	}
	return s.ln.Addr()
}

// Serve answers requests on the listener bound by Listen until Shutdown. It
// returns nil after Shutdown and the accept error otherwise.
func (s *Server) Serve() error {
	s.mu.Lock()
	ln := s.ln
	s.mu.Unlock()
	if ln == nil {
		return errors.New("gate: Serve before Listen")
	}
	if err := s.srv.Serve(ln); !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("gate: serve: %w", err)
	}
	return nil
}

// Shutdown stops the server. It stops accepting, waits for requests in
// flight until ctx ends, then closes the remaining connections. It returns
// ctx's error when it had to close connections. It also releases a
// listener that Serve never used.
func (s *Server) Shutdown(ctx context.Context) error {
	err := s.srv.Shutdown(ctx)
	if err != nil {
		// Close ends the connections Shutdown was still waiting for.
		_ = s.srv.Close()
	}
	s.mu.Lock()
	if s.ln != nil {
		// Serve closes the listener it used; this covers Listen without
		// Serve. A second Close only returns an error, which is ignored.
		_ = s.ln.Close()
	}
	s.mu.Unlock()
	return err
}

// ServeHTTP answers GET /v1/verdict and 404s everything else.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != VerdictPath || r.Method != http.MethodGet {
		s.log.Info("job check server: unknown request",
			"method", r.Method, "path", r.URL.Path)
		writeJSON(w, http.StatusNotFound, bodyNotFound)
		return
	}

	// The token is never logged; only the claimed runner name is.
	name, token, ok := credentials(r.Header)
	if !ok {
		s.log.Info("job check server: request without credentials", "runner", name)
		w.Header().Set("WWW-Authenticate", "Bearer")
		writeJSON(w, http.StatusUnauthorized, bodyNoAuth)
		return
	}
	// The token comparison is Authenticate's alone (constant time); the
	// server compares nothing itself.
	v, ok := s.auth.Authenticate(name, token, s.clock.Now())
	if !ok {
		s.log.Info("job check server: runner name or token rejected", "runner", name)
		w.Header().Set("WWW-Authenticate", "Bearer")
		writeJSON(w, http.StatusUnauthorized, bodyUnauthorized)
		return
	}
	body := v.body()
	if body == nil {
		s.log.Error("job check server: unknown verdict", "runner", name, "verdict", v)
		writeJSON(w, http.StatusInternalServerError, bodyInternal)
		return
	}
	writeJSON(w, http.StatusOK, body)
}

// credentials returns the runner name and bearer token from the headers.
// Each header must appear once and be non-empty.
func credentials(h http.Header) (name, token string, ok bool) {
	names := h.Values(runnerNameHeader)
	if len(names) == 1 {
		name = names[0]
	}
	auths := h.Values(authorizationHeader)
	if len(auths) != 1 {
		return name, "", false
	}
	auth := auths[0]
	// The auth scheme is case-insensitive (RFC 9110 section 11.1).
	if len(auth) <= len(bearerPrefix) || !strings.EqualFold(auth[:len(bearerPrefix)], bearerPrefix) {
		return name, "", false
	}
	token = auth[len(bearerPrefix):]
	return name, token, name != ""
}

func writeJSON(w http.ResponseWriter, status int, body []byte) {
	h := w.Header()
	h.Set("Content-Type", "application/json")
	h.Set("Content-Length", strconv.Itoa(len(body)))
	h.Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	// A write error means the client went away; there is nothing to do.
	_, _ = w.Write(body)
}
