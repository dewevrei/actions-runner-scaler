package gate

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Routes of the job check server.
const (
	// VerdictPath is the hook's verdict poll (R66).
	VerdictPath = "/v1/verdict"
	// EventPath is the hook's event report (0002 SPEC R139).
	EventPath = "/v1/event"
)

// Header names the job-started hook sends (image/job-started.sh).
const (
	authorizationHeader = "Authorization"
	runnerNameHeader    = "X-Runner-Name"
	eventNameHeader     = "X-Event-Name"
	bearerPrefix        = "Bearer "
)

// maxEventRead is how much of an event body the server reads: one byte
// past MaxEventPayload, so a larger body is seen without reading it all
// (0002 SPEC R136).
const maxEventRead = MaxEventPayload + 1

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
	bodyNoMethod     = []byte(`{"reason":"method not allowed"}`)
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

// Authenticator checks a runner's job check token and keeps its verdict:
// the server's one dependency.
type Authenticator interface {
	// Authenticate compares presented with the named runner's token in
	// constant time. On a match it may record at as the runner's busy time
	// and returns the verdict and true. On a mismatch, an unknown name or a
	// revoked token it records nothing and returns false.
	Authenticate(name, presented string, at time.Time) (Verdict, bool)
	// Finalizing reports whether the end cleanup of a drain has begun for
	// runner name; its event report is then not judged (0002 SPEC R131).
	Finalizing(name string) bool
	// Record records d as the verdict on runner name's job, reported with
	// event name event, and returns the runner's verdict afterwards, which
	// is pending when nothing could be recorded. It must return without
	// waiting for Docker or GitHub (0002 SPEC R123, R131).
	Record(name, event string, d Decision) Verdict
}

// Clock is the server's time source.
type Clock interface {
	Now() time.Time
}

type wallClock struct{}

func (wallClock) Now() time.Time { return time.Now() }

// Server is the job check HTTP server. It answers GET /v1/verdict and
// POST /v1/event on a loopback address and nothing else (R43, 0002 SPEC
// Design 판정 "job 확인 서버 계약").
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

// ServeHTTP answers GET /v1/verdict and POST /v1/event, 405s another
// method on /v1/event and 404s everything else.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.URL.Path == EventPath && r.Method == http.MethodPost:
		s.serveEvent(w, r)
	case r.URL.Path == EventPath:
		// 0002 SPEC R135: nothing authenticated, nothing recorded.
		s.log.Info("job check server: method not allowed",
			"method", r.Method, "path", r.URL.Path)
		w.Header().Set("Allow", http.MethodPost)
		writeUnread(w, http.StatusMethodNotAllowed, bodyNoMethod)
	case r.URL.Path == VerdictPath && r.Method == http.MethodGet:
		s.serveVerdict(w, r)
	default:
		s.log.Info("job check server: unknown request",
			"method", r.Method, "path", r.URL.Path)
		writeJSON(w, http.StatusNotFound, bodyNotFound)
	}
}

// serveVerdict answers the hook's verdict poll (R66).
func (s *Server) serveVerdict(w http.ResponseWriter, r *http.Request) {
	name, v, ok := s.authenticate(w, r, writeJSON)
	if !ok {
		return
	}
	writeVerdict(s.log, w, name, http.StatusOK, v, writeJSON)
}

// serveEvent answers the hook's event report in the order of 0002 SPEC
// Design 판정 "job 확인 서버 계약". Every answer given before the whole body
// is read leaves the body unread (writeUnread).
func (s *Server) serveEvent(w http.ResponseWriter, r *http.Request) {
	// 0002 SPEC R134: the token first, before any read of the body.
	name, v, ok := s.authenticate(w, r, writeUnread)
	if !ok {
		return
	}
	// 0002 SPEC R132: a recorded verdict is answered and kept.
	if v != VerdictPending {
		writeVerdict(s.log, w, name, http.StatusOK, v, writeUnread)
		return
	}
	// 0002 SPEC R131: no verdict once the drain's end cleanup began.
	if s.auth.Finalizing(name) {
		s.log.Info("job check server: event report after finalize began; not judged", "runner", name)
		writeUnread(w, http.StatusOK, bodyPending)
		return
	}
	event := eventName(r.Header)
	body, err := io.ReadAll(io.LimitReader(r.Body, maxEventRead))
	if err != nil {
		// 0002 SPEC R137: a body cut short is not judged, so the hook's
		// next report is a first report. Abort closes the connection
		// without a response; net/http logs nothing for it.
		s.log.Info("job check server: event report body cut short; connection closed",
			"runner", name, "read", len(body), "err", err)
		stopReading(w)
		panic(http.ErrAbortHandler)
	}
	if len(body) > MaxEventPayload {
		// 0002 SPEC R136: denied without reading the rest.
		v = s.auth.Record(name, event, Decision{Reason: ReasonPayloadTooLarge})
		status := http.StatusOK
		if v == VerdictDeny {
			status = http.StatusRequestEntityTooLarge
		}
		writeVerdict(s.log, w, name, status, v, writeUnread)
		return
	}
	// 0002 SPEC R131: judged and recorded before the answer.
	v = s.auth.Record(name, event, Decide(event, body))
	writeVerdict(s.log, w, name, http.StatusOK, v, writeJSON)
}

// writer writes a fixed JSON response: writeJSON or writeUnread.
type writer func(w http.ResponseWriter, status int, body []byte)

// authenticate checks the request's credentials (R14, R40). On failure it
// answers 401 with write and returns false. The token is never logged; only
// the claimed runner name is.
func (s *Server) authenticate(w http.ResponseWriter, r *http.Request, write writer) (string, Verdict, bool) {
	name, token, ok := credentials(r.Header)
	if !ok {
		s.log.Info("job check server: request without credentials", "runner", name, "path", r.URL.Path)
		w.Header().Set("WWW-Authenticate", "Bearer")
		write(w, http.StatusUnauthorized, bodyNoAuth)
		return name, VerdictPending, false
	}
	// The token comparison is Authenticate's alone (constant time); the
	// server compares nothing itself.
	v, ok := s.auth.Authenticate(name, token, s.clock.Now())
	if !ok {
		s.log.Info("job check server: runner name or token rejected", "runner", name, "path", r.URL.Path)
		w.Header().Set("WWW-Authenticate", "Bearer")
		write(w, http.StatusUnauthorized, bodyUnauthorized)
		return name, VerdictPending, false
	}
	return name, v, true
}

// writeVerdict answers v with status, or 500 for an unknown verdict.
func writeVerdict(log *slog.Logger, w http.ResponseWriter, name string, status int, v Verdict, write writer) {
	body := v.body()
	if body == nil {
		log.Error("job check server: unknown verdict", "runner", name, "verdict", v)
		write(w, http.StatusInternalServerError, bodyInternal)
		return
	}
	write(w, status, body)
}

// eventName returns the single X-Event-Name value; a missing header or
// several values give "", which Decide denies (0002 SPEC R125).
func eventName(h http.Header) string {
	if v := h.Values(eventNameHeader); len(v) == 1 {
		return v[0]
	}
	return ""
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

// writeUnread is writeJSON for a request whose body the handler leaves
// unread. Connection: close alone is not enough: after the handler net/http
// still reads up to 256 KiB of an unread body when the request did not ask
// to close (Go 1.27.3 net/http/transfer.go lines 1019-1039 in body.Close,
// called by server.go line 1737 in finishRequest), so the read deadline is
// moved into the past and that read ends at once (0002 SPEC R134).
func writeUnread(w http.ResponseWriter, status int, body []byte) {
	w.Header().Set("Connection", "close")
	stopReading(w)
	writeJSON(w, status, body)
}

// stopReading makes every further read of the request's connection fail at
// once. A ResponseWriter without deadlines, as in tests, is left as it is.
func stopReading(w http.ResponseWriter) {
	_ = http.NewResponseController(w).SetReadDeadline(time.Unix(1, 0))
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
