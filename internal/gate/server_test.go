package gate

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

var testNow = time.Date(2026, 11, 25, 9, 0, 0, 0, time.UTC)

type fixedClock struct{ t time.Time }

func (c fixedClock) Now() time.Time { return c.t }

type authCall struct {
	name, presented string
	at              time.Time
}

type fakeRow struct {
	token   Token
	verdict Verdict
	busyAt  time.Time
}

// fakeTable stands in for scaler.Table: it compares tokens with
// Token.Equal and records the busy time on the first match only.
type fakeTable struct {
	mu    sync.Mutex
	rows  map[string]*fakeRow
	calls []authCall
}

func newFakeTable() *fakeTable { return &fakeTable{rows: map[string]*fakeRow{}} }

func (f *fakeTable) add(name string, tok Token, v Verdict) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rows[name] = &fakeRow{token: tok, verdict: v}
}

func (f *fakeTable) set(name string, v Verdict) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rows[name].verdict = v
}

func (f *fakeTable) revoke(name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rows[name].token = Token{}
}

func (f *fakeTable) busyAt(name string) time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.rows[name].busyAt
}

func (f *fakeTable) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

func (f *fakeTable) Authenticate(name, presented string, at time.Time) (Verdict, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, authCall{name, presented, at})
	r, ok := f.rows[name]
	if !ok || !r.token.Equal(presented) {
		return VerdictPending, false
	}
	if r.busyAt.IsZero() {
		r.busyAt = at
	}
	return r.verdict, true
}

// syncBuffer is a log sink safe for the server's goroutines.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

type harness struct {
	srv   *Server
	table *fakeTable
	logs  *syncBuffer
	gen   *TokenGenerator
}

func newHarness(t *testing.T, auth Authenticator) *harness {
	t.Helper()
	h := &harness{table: newFakeTable(), logs: &syncBuffer{}, gen: NewTokenGenerator(nil)}
	if auth == nil {
		auth = h.table
	}
	log := slog.New(slog.NewTextHandler(h.logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	srv, err := NewServer("127.0.0.1:0", auth, fixedClock{testNow}, log)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	h.srv = srv
	return h
}

func (h *harness) token(t *testing.T) Token {
	t.Helper()
	tok, err := h.gen.New()
	if err != nil {
		t.Fatalf("token: %v", err)
	}
	return tok
}

// do sends the request the hook sends and returns the recorded response.
func (h *harness) do(name, token string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, VerdictPath, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("X-Runner-Name", name)
	return h.serve(req)
}

func (h *harness) serve(req *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.srv.ServeHTTP(rec, req)
	return rec
}

func assertUnauthorized(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("401 body %q is not JSON: %v", rec.Body.Bytes(), err)
	}
	if len(body) != 1 || body["reason"] == "" {
		t.Fatalf("401 body = %q, want only a non-empty reason", rec.Body.Bytes())
	}
	if got := rec.Header().Get("WWW-Authenticate"); got != "Bearer" {
		t.Errorf("WWW-Authenticate = %q, want Bearer", got)
	}
}

func TestVerdictBodies(t *testing.T) {
	for _, tc := range []struct {
		verdict Verdict
		body    string
	}{
		{VerdictAllow, `{"verdict":"allow"}`},
		{VerdictDeny, `{"verdict":"deny"}`},
		{VerdictPending, `{"verdict":"pending"}`},
	} {
		t.Run(tc.verdict.String(), func(t *testing.T) {
			h := newHarness(t, nil)
			tok := h.token(t)
			h.table.add("pool-a", tok, tc.verdict)

			rec := h.do("pool-a", tok.Reveal())
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200", rec.Code)
			}
			if got := rec.Body.Bytes(); !bytes.Equal(got, []byte(tc.body)) {
				t.Errorf("body = %q, want exactly %q", got, tc.body)
			}
			if got := rec.Header().Get("Content-Type"); got != "application/json" {
				t.Errorf("Content-Type = %q, want application/json", got)
			}
			if got := h.table.busyAt("pool-a"); !got.Equal(testNow) {
				t.Errorf("busy at %v, want the clock's %v", got, testNow)
			}
		})
	}
}

func TestAuthenticateGetsHookCredentials(t *testing.T) {
	h := newHarness(t, nil)
	tok := h.token(t)
	h.table.add("pool-a", tok, VerdictPending)

	h.do("pool-a", tok.Reveal())

	want := authCall{"pool-a", tok.Reveal(), testNow}
	if len(h.table.calls) != 1 || h.table.calls[0] != want {
		t.Fatalf("Authenticate calls = %+v, want one call with name, token and clock time", h.table.calls)
	}
}

func TestRejectedCredentials(t *testing.T) {
	h := newHarness(t, nil)
	tokA, tokB := h.token(t), h.token(t)
	h.table.add("pool-a", tokA, VerdictAllow)
	h.table.add("pool-b", tokB, VerdictAllow)
	a := tokA.Reveal()

	for _, tc := range []struct {
		name string
		req  func() *http.Request
	}{
		{"wrong token", func() *http.Request { return hookRequest("pool-a", "Bearer "+a[:len(a)-1]+flip(a[len(a)-1])) }},
		{"other runner's token", func() *http.Request { return hookRequest("pool-a", "Bearer "+tokB.Reveal()) }},
		{"token under another runner's name", func() *http.Request { return hookRequest("pool-b", "Bearer "+a) }},
		{"unknown name", func() *http.Request { return hookRequest("pool-c", "Bearer "+a) }},
		{"token prefix", func() *http.Request { return hookRequest("pool-a", "Bearer "+a[:10]) }},
		{"token with suffix", func() *http.Request { return hookRequest("pool-a", "Bearer "+a+"x") }},
		{"empty token", func() *http.Request { return hookRequest("pool-a", "Bearer ") }},
		{"no Authorization", func() *http.Request { return hookRequest("pool-a", "") }},
		{"other scheme", func() *http.Request { return hookRequest("pool-a", "Basic "+a) }},
		{"bare token", func() *http.Request { return hookRequest("pool-a", a) }},
		{"no runner name", func() *http.Request { return hookRequest("", "Bearer "+a) }},
		{"two Authorization headers", func() *http.Request {
			r := hookRequest("pool-a", "Bearer "+a)
			r.Header.Add("Authorization", "Bearer "+a)
			return r
		}},
		{"two runner names", func() *http.Request {
			r := hookRequest("pool-a", "Bearer "+a)
			r.Header.Add("X-Runner-Name", "pool-a")
			return r
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertUnauthorized(t, h.serve(tc.req()))
		})
	}

	for _, name := range []string{"pool-a", "pool-b"} {
		if got := h.table.busyAt(name); !got.IsZero() {
			t.Errorf("%s busy at %v after rejected polls, want none (R40)", name, got)
		}
	}
	logs := h.logs.String()
	for _, tok := range []string{a, tokB.Reveal(), a[:10]} {
		if strings.Contains(logs, tok) {
			t.Errorf("log contains a token:\n%s", logs)
		}
	}
	if !strings.Contains(logs, "runner=pool-a") {
		t.Errorf("log does not name the runner:\n%s", logs)
	}
}

func hookRequest(name, authorization string) *http.Request {
	req := httptest.NewRequest(http.MethodGet, VerdictPath, nil)
	if authorization != "" {
		req.Header.Set("Authorization", authorization)
	}
	if name != "" {
		req.Header.Set("X-Runner-Name", name)
	}
	return req
}

func TestBearerSchemeIsCaseInsensitive(t *testing.T) {
	h := newHarness(t, nil)
	tok := h.token(t)
	h.table.add("pool-a", tok, VerdictDeny)

	rec := h.serve(hookRequest("pool-a", "bearer "+tok.Reveal()))
	if rec.Code != http.StatusOK || rec.Body.String() != `{"verdict":"deny"}` {
		t.Fatalf("got %d %q, want 200 deny", rec.Code, rec.Body.String())
	}
}

func TestRevokedToken(t *testing.T) {
	h := newHarness(t, nil)
	tok := h.token(t)
	h.table.add("pool-a", tok, VerdictAllow)
	if rec := h.do("pool-a", tok.Reveal()); rec.Code != http.StatusOK {
		t.Fatalf("before revoke: status %d, want 200", rec.Code)
	}

	h.table.revoke("pool-a")

	assertUnauthorized(t, h.do("pool-a", tok.Reveal()))
}

func TestRepeatedPollsReturnSameVerdict(t *testing.T) {
	h := newHarness(t, nil)
	tok := h.token(t)
	h.table.add("pool-a", tok, VerdictPending)

	first := h.do("pool-a", tok.Reveal()).Body.String()
	for i := range 5 {
		rec := h.do("pool-a", tok.Reveal())
		if rec.Code != http.StatusOK || rec.Body.String() != first {
			t.Fatalf("poll %d: got %d %q, want 200 %q (R38)", i+2, rec.Code, rec.Body.String(), first)
		}
	}

	// The server reports the table's current verdict, not a cached one.
	h.table.set("pool-a", VerdictAllow)
	if got := h.do("pool-a", tok.Reveal()).Body.String(); got != `{"verdict":"allow"}` {
		t.Errorf("after the verdict: body %q, want allow", got)
	}
}

func TestUnknownRequestsAre404(t *testing.T) {
	h := newHarness(t, nil)
	tok := h.token(t)
	h.table.add("pool-a", tok, VerdictAllow)

	for _, tc := range []struct{ method, target string }{
		{http.MethodGet, "/"},
		{http.MethodGet, "/e2e-payload-ran"},
		{http.MethodGet, "/v1/verdict/"},
		{http.MethodGet, "/v1/verdicts"},
		{http.MethodGet, "/V1/verdict"},
		{http.MethodGet, "/v1//verdict"},
		{http.MethodGet, "/status"},
		{http.MethodHead, VerdictPath},
		{http.MethodPost, VerdictPath},
		{http.MethodPut, VerdictPath},
		{http.MethodDelete, VerdictPath},
		{http.MethodOptions, VerdictPath},
		{http.MethodPatch, VerdictPath},
	} {
		t.Run(tc.method+" "+tc.target, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, tc.target, nil)
			req.Header.Set("Authorization", "Bearer "+tok.Reveal())
			req.Header.Set("X-Runner-Name", "pool-a")

			rec := h.serve(req)

			if rec.Code != http.StatusNotFound {
				t.Fatalf("status = %d, want 404", rec.Code)
			}
			if !strings.Contains(h.logs.String(), fmt.Sprintf("method=%s path=%s", tc.method, tc.target)) {
				t.Errorf("path not logged:\n%s", h.logs.String())
			}
		})
	}
	if n := h.table.callCount(); n != 0 {
		t.Errorf("Authenticate called %d times for unknown requests, want 0", n)
	}
	if !h.table.busyAt("pool-a").IsZero() {
		t.Error("unknown requests recorded busy")
	}
	if strings.Contains(h.logs.String(), tok.Reveal()) {
		t.Errorf("log contains the token:\n%s", h.logs.String())
	}
}

func TestNotFoundLogLevel(t *testing.T) {
	h := newHarness(t, nil)
	h.serve(httptest.NewRequest(http.MethodGet, "/e2e-payload-ran", nil))
	if !strings.Contains(h.logs.String(), "level=INFO") {
		t.Errorf("404 not logged at Info:\n%s", h.logs.String())
	}
}

// verdictAuth returns a fixed verdict for any credentials.
type verdictAuth Verdict

func (v verdictAuth) Authenticate(string, string, time.Time) (Verdict, bool) {
	return Verdict(v), true
}

func TestUnknownVerdictIs500(t *testing.T) {
	h := newHarness(t, verdictAuth(7))
	rec := h.do("pool-a", "anything")
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "verdict") {
		t.Errorf("500 body %q looks like a verdict", rec.Body.String())
	}
}

func TestServerTimeouts(t *testing.T) {
	h := newHarness(t, nil)
	if got := h.srv.srv.ReadHeaderTimeout; got != 5*time.Second {
		t.Errorf("ReadHeaderTimeout = %v, want 5s", got)
	}
	if got := h.srv.srv.ReadTimeout; got != 10*time.Second {
		t.Errorf("ReadTimeout = %v, want 10s", got)
	}
	if got := h.srv.srv.WriteTimeout; got != 10*time.Second {
		t.Errorf("WriteTimeout = %v, want 10s", got)
	}
}

// newClient returns a client with its own transport and no proxy, closed
// when the test ends.
func newClient(t *testing.T) *http.Client {
	t.Helper()
	tr := &http.Transport{Proxy: nil, DisableKeepAlives: true}
	t.Cleanup(tr.CloseIdleConnections)
	return &http.Client{Transport: tr}
}

// TestStalledBodyIsCut declares a request body, sends none, and checks that
// ReadTimeout closes the connection so Shutdown is not held up by it.
func TestStalledBodyIsCut(t *testing.T) {
	const readTimeout = 200 * time.Millisecond
	for _, tc := range []struct{ name, header string }{
		{"content-length", "Content-Length: 10\r\n"},
		{"chunked", "Transfer-Encoding: chunked\r\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, nil)
			tok := h.token(t)
			h.table.add("pool-a", tok, VerdictPending)
			h.srv.srv.ReadTimeout = readTimeout
			done := startServer(t, h)

			conn, err := net.Dial("tcp", h.srv.Addr().String())
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			// A guard against hanging, far beyond readTimeout; reaching it
			// fails the test.
			if err := conn.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
				t.Fatal(err)
			}
			req := "GET " + VerdictPath + " HTTP/1.1\r\nHost: x\r\n" +
				"Authorization: Bearer " + tok.Reveal() + "\r\nX-Runner-Name: pool-a\r\n" +
				tc.header + "\r\n"
			if _, err := io.WriteString(conn, req); err != nil {
				t.Fatal(err)
			}

			br := bufio.NewReader(conn)
			resp, err := http.ReadResponse(br, nil)
			if err != nil {
				t.Fatalf("read response: %v", err)
			}
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode != http.StatusOK || string(body) != `{"verdict":"pending"}` {
				t.Fatalf("got %d %q, want 200 pending", resp.StatusCode, body)
			}
			// The server drains the declared body after the handler; only
			// ReadTimeout ends that drain and closes the connection.
			if n, err := br.Read(make([]byte, 1)); n != 0 || err != io.EOF {
				t.Fatalf("after the response: read %d bytes, err %v; want EOF from the server closing", n, err)
			}

			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			if err := h.srv.Shutdown(ctx); err != nil {
				t.Fatalf("Shutdown = %v, want nil", err)
			}
			if err := <-done; err != nil {
				t.Errorf("Serve = %v, want nil", err)
			}
		})
	}
}

func TestNewServerAddress(t *testing.T) {
	for _, addr := range []string{
		"0.0.0.0:8765",
		"192.168.65.254:8765",
		"10.0.0.1:8765",
		"[::]:8765",
		":8765",
		"localhost:8765",
		"host.docker.internal:8765",
		"127.0.0.1",
		"127.0.0.1:65536",
		"127.0.0.1:-1",
		"127.0.0.1:http",
		"",
	} {
		if _, err := NewServer(addr, newFakeTable(), nil, nil); err == nil {
			t.Errorf("NewServer(%q) succeeded, want a non-loopback error", addr)
		}
	}
	for _, addr := range []string{"127.0.0.1:8765", "127.0.0.2:0", "[::1]:0", "127.0.0.1:0"} {
		if _, err := NewServer(addr, newFakeTable(), nil, nil); err != nil {
			t.Errorf("NewServer(%q): %v", addr, err)
		}
	}
}

func TestNewServerNilAuthenticator(t *testing.T) {
	if _, err := NewServer("127.0.0.1:0", nil, nil, nil); err == nil {
		t.Error("NewServer with a nil Authenticator succeeded")
	}
}

// startServer listens on a loopback port, serves in a goroutine and
// returns the Serve result channel.
func startServer(t *testing.T, h *harness) <-chan error {
	t.Helper()
	if err := h.srv.Listen(); err != nil {
		t.Fatalf("Listen: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- h.srv.Serve() }()
	t.Cleanup(func() { _ = h.srv.Shutdown(context.Background()) })
	return done
}

func TestServeOverLoopback(t *testing.T) {
	h := newHarness(t, nil)
	tok := h.token(t)
	h.table.add("pool-a", tok, VerdictAllow)
	done := startServer(t, h)

	addr := h.srv.Addr().(*net.TCPAddr)
	if !addr.IP.IsLoopback() {
		t.Fatalf("bound to %v, want loopback", addr)
	}

	// The same request the hook makes.
	req, err := http.NewRequest(http.MethodGet, "http://"+addr.String()+VerdictPath, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+tok.Reveal())
	req.Header.Set("X-Runner-Name", "pool-a")
	resp, err := newClient(t).Do(req)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK || string(body) != `{"verdict":"allow"}` {
		t.Fatalf("got %d %q, want 200 allow", resp.StatusCode, body)
	}
	if !resp.Close {
		t.Error("connection kept alive, want closed after each poll")
	}

	// "OPTIONS *" must reach ServeHTTP too, not net/http's default 200.
	conn, err := net.Dial("tcp", addr.String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := io.WriteString(conn, "OPTIONS * HTTP/1.1\r\nHost: x\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	optResp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatalf("OPTIONS *: %v", err)
	}
	optResp.Body.Close()
	if optResp.StatusCode != http.StatusNotFound {
		t.Errorf("OPTIONS * status = %d, want 404", optResp.StatusCode)
	}

	if err := h.srv.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if err := <-done; err != nil {
		t.Errorf("Serve after Shutdown = %v, want nil", err)
	}
	if !strings.Contains(h.logs.String(), "path=*") {
		t.Errorf("OPTIONS * not logged:\n%s", h.logs.String())
	}
}

// blockingAuth holds Authenticate until release is closed.
type blockingAuth struct {
	once    sync.Once
	entered chan struct{}
	release chan struct{}
}

func (b *blockingAuth) Authenticate(string, string, time.Time) (Verdict, bool) {
	b.once.Do(func() { close(b.entered) })
	<-b.release
	return VerdictAllow, true
}

func TestShutdownClosesWhenContextEnds(t *testing.T) {
	auth := &blockingAuth{entered: make(chan struct{}), release: make(chan struct{})}
	defer close(auth.release)
	h := newHarness(t, auth)
	done := startServer(t, h)

	reqErr := make(chan error, 1)
	go func() {
		req, _ := http.NewRequest(http.MethodGet, "http://"+h.srv.Addr().String()+VerdictPath, nil)
		req.Header.Set("Authorization", "Bearer t")
		req.Header.Set("X-Runner-Name", "pool-a")
		resp, err := newClient(t).Do(req)
		if err == nil {
			_, err = io.ReadAll(resp.Body)
			resp.Body.Close()
		}
		reqErr <- err
	}()
	<-auth.entered

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := h.srv.Shutdown(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Shutdown = %v, want context.Canceled", err)
	}
	if err := <-reqErr; err == nil {
		t.Error("request in flight succeeded, want its connection closed")
	}
	if err := <-done; err != nil {
		t.Errorf("Serve = %v, want nil", err)
	}
}

func TestShutdownWithoutServeReleasesListener(t *testing.T) {
	h := newHarness(t, nil)
	if err := h.srv.Listen(); err != nil {
		t.Fatal(err)
	}
	addr := h.srv.Addr().String()

	if err := h.srv.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if err := h.srv.Serve(); err != nil {
		t.Errorf("Serve after Shutdown = %v, want nil", err)
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatalf("address still held after Shutdown: %v", err)
	}
	ln.Close()
}

func TestListenAndServeMisuse(t *testing.T) {
	h := newHarness(t, nil)
	if h.srv.Addr() != nil {
		t.Error("Addr before Listen is not nil")
	}
	if err := h.srv.Serve(); err == nil {
		t.Error("Serve before Listen succeeded")
	}
	if err := h.srv.Listen(); err != nil {
		t.Fatal(err)
	}
	defer h.srv.Shutdown(context.Background())
	if err := h.srv.Listen(); err == nil {
		t.Error("second Listen succeeded")
	}
}

func TestListenFailure(t *testing.T) {
	taken, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer taken.Close()
	srv, err := NewServer(taken.Addr().String(), newFakeTable(), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.Listen(); err == nil {
		srv.Shutdown(context.Background())
		t.Fatal("Listen on a taken port succeeded")
	}
}

func TestVerdictString(t *testing.T) {
	for v, want := range map[Verdict]string{
		VerdictPending: "pending", VerdictAllow: "allow", VerdictDeny: "deny", Verdict(9): "Verdict(9)",
	} {
		if got := v.String(); got != want {
			t.Errorf("%d.String() = %q, want %q", int(v), got, want)
		}
	}
	var zero Verdict
	if zero != VerdictPending {
		t.Error("zero Verdict is not pending")
	}
}
