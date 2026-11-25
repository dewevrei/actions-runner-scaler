package github

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptrace"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/actions/scaleset"
	"github.com/actions/scaleset/listener"

	"github.com/dewevrei/actions-runner-scaler/internal/logging"
	"github.com/dewevrei/actions-runner-scaler/internal/retry"
)

var _ listener.Client = (*Session)(nil)

// fakeSession serves the message session endpoints. Handlers for single
// endpoints can be replaced per test.
type fakeSession struct {
	e *testEnv

	mu      sync.Mutex
	token   string // queue token the server accepts
	create  http.HandlerFunc
	get     http.HandlerFunc
	acquire http.HandlerFunc
	refresh http.HandlerFunc
	ack     http.HandlerFunc // answers message deletes when set
	deleted int
}

func newFakeSession(t *testing.T) *fakeSession {
	t.Helper()
	return newFakeSessionIn(t, newTestEnv(t))
}

// newFakeSessionIn is newFakeSession on the test environment e.
func newFakeSessionIn(t *testing.T, e *testEnv) *fakeSession {
	t.Helper()
	s := &fakeSession{e: e, token: fakeQueueToken}
	sessionsPath := "/svc/_apis/runtime/runnerscalesets/7/sessions"
	s.e.gh.actions = func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		create, get, acquire, refresh, ack := s.create, s.get, s.acquire, s.refresh, s.ack
		s.mu.Unlock()
		path := r.URL.Path
		switch {
		case r.Method == http.MethodPost && path == sessionsPath:
			if create != nil {
				create(w, r)
				return
			}
			writeJSON(w, http.StatusOK, s.sessionBody(fakeQueueToken))
		case r.Method == http.MethodPatch && strings.HasPrefix(path, sessionsPath+"/"):
			if refresh != nil {
				refresh(w, r)
				return
			}
			s.mu.Lock()
			s.token = fakeQueueToken2
			s.mu.Unlock()
			writeJSON(w, http.StatusOK, s.sessionBody(fakeQueueToken2))
		case r.Method == http.MethodDelete && strings.HasPrefix(path, sessionsPath+"/"):
			s.mu.Lock()
			s.deleted++
			s.mu.Unlock()
			w.WriteHeader(http.StatusNoContent)
		case !s.authorized(r):
			w.WriteHeader(http.StatusUnauthorized)
		case r.Method == http.MethodGet && path == "/queue":
			if get != nil {
				get(w, r)
				return
			}
			w.WriteHeader(http.StatusAccepted)
		case r.Method == http.MethodDelete && strings.HasPrefix(path, "/queue/"):
			if ack != nil {
				ack(w, r)
				return
			}
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodPost && path == "/svc/_apis/runtime/runnerscalesets/7/acquirejobs":
			if acquire != nil {
				acquire(w, r)
				return
			}
			writeJSON(w, http.StatusOK, map[string]any{"count": 1, "value": []int64{11}})
		default:
			http.NotFound(w, r)
		}
	}
	return s
}

func (s *fakeSession) authorized(r *http.Request) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return r.Header.Get("Authorization") == "Bearer "+s.token
}

func (s *fakeSession) sessionBody(token string) map[string]any {
	return map[string]any{
		"sessionId":               testSessionID,
		"ownerName":               "host",
		"messageQueueUrl":         s.e.gh.srv.URL + "/queue",
		"messageQueueAccessToken": token,
		"statistics":              map[string]any{"totalAssignedJobs": 1},
	}
}

func (s *fakeSession) set(f func(*fakeSession)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f(s)
}

func registered(secrets *logging.Secrets, value string) bool {
	return secrets.Redact(value) == logging.Placeholder
}

func TestSessionLifecycle(t *testing.T) {
	s := newFakeSession(t)
	ctx := context.Background()
	sess, err := s.e.client.CreateSession(ctx, testScaleSetID, "host")
	if err != nil {
		t.Fatal(err)
	}
	if got := sess.Session().Statistics.TotalAssignedJobs; got != 1 {
		t.Errorf("TotalAssignedJobs = %d", got)
	}
	if !registered(s.e.secrets, fakeQueueToken) {
		t.Error("queue token not registered as a secret")
	}

	msg, err := sess.GetMessage(ctx, 0, 2)
	if err != nil || msg != nil {
		t.Errorf("GetMessage = %v, %v, want nil, nil", msg, err)
	}
	ids, err := sess.AcquireJobs(ctx, []int64{11})
	if err != nil || len(ids) != 1 || ids[0] != 11 {
		t.Errorf("AcquireJobs = %v, %v", ids, err)
	}
	if err := sess.DeleteMessage(ctx, 3); err != nil {
		t.Errorf("DeleteMessage: %v", err)
	}

	// The server rotates the token; the library refreshes the session and
	// the new token replaces the old one in the secrets set.
	s.set(func(s *fakeSession) { s.token = "rotated-away" })
	s.set(func(s *fakeSession) {
		s.refresh = func(w http.ResponseWriter, r *http.Request) {
			s.mu.Lock()
			s.token = fakeQueueToken2
			s.mu.Unlock()
			writeJSON(w, http.StatusOK, s.sessionBody(fakeQueueToken2))
		}
	})
	if _, err := sess.GetMessage(ctx, 0, 2); err != nil {
		t.Fatalf("GetMessage after rotation: %v", err)
	}
	if !registered(s.e.secrets, fakeQueueToken2) {
		t.Error("refreshed queue token not registered")
	}
	if registered(s.e.secrets, fakeQueueToken) {
		t.Error("old queue token still registered")
	}

	closeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	if err := sess.Close(closeCtx); err != nil {
		t.Fatalf("Close: %v", err)
	}
	s.mu.Lock()
	deleted := s.deleted
	s.mu.Unlock()
	if deleted != 1 {
		t.Errorf("session deletes = %d, want 1", deleted)
	}
	if registered(s.e.secrets, fakeQueueToken2) {
		t.Error("queue token still registered after Close")
	}
	s.e.assertNoSecrets(t, "logs", s.e.logs.String())
}

// A poll whose context ends while the library is retrying it returns at
// that end, with the deadline still in the error: the library retries a
// failed attempt (here a 503) but neither retries nor waits once the call's
// context is done. The daemon's poll bound relies on both (PLAN M5-5).
func TestGetMessageEndsAtCallDeadline(t *testing.T) {
	s := newFakeSession(t)
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	var polls atomic.Int32
	s.set(func(s *fakeSession) {
		s.get = func(w http.ResponseWriter, r *http.Request) {
			if polls.Add(1) == 1 {
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			// Later attempts hang like a held long poll.
			select {
			case <-r.Context().Done():
			case <-release:
			}
		}
	})
	sess, err := s.e.client.CreateSession(context.Background(), testScaleSetID, "host")
	if err != nil {
		t.Fatal(err)
	}
	const bound = 300 * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), bound)
	defer cancel()
	start := time.Now()
	msg, err := sess.GetMessage(ctx, 0, 2)
	took := time.Since(start)
	if msg != nil || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("GetMessage = %v, %v, want a deadline error", msg, err)
	}
	var op *retry.OpError
	if !errors.As(err, &op) || op.Op != retry.OpGetMessage {
		t.Errorf("error %v does not name %q", err, retry.OpGetMessage)
	}
	if took > bound+2*time.Second {
		t.Errorf("GetMessage took %v, want about %v", took, bound)
	}
	if n := polls.Load(); n < 2 {
		t.Errorf("poll requests = %d, want the 503 retried", n)
	}
}

// The library sends the poll with its caller's context down to net/http,
// so an httptrace.ClientTrace on that context sees whether the request was
// written: not when the dial hangs, yes when the server holds the request.
// The daemon's poll cut tells a hung connection from a held poll this way
// (PLAN M5-5).
func TestGetMessageTraceSeesWrite(t *testing.T) {
	for _, tt := range []struct {
		name      string
		hangDial  bool
		wantWrote bool
	}{
		{"dial hangs", true, false},
		{"server holds the poll", false, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var hang atomic.Bool
			e := newTestEnvTransport(t, func(base *http.Transport) http.RoundTripper {
				tr := base.Clone()
				// Every request dials, so the poll's own connection hangs.
				tr.DisableKeepAlives = true
				var d net.Dialer
				tr.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
					if hang.Load() {
						<-ctx.Done()
						return nil, ctx.Err()
					}
					return d.DialContext(ctx, network, addr)
				}
				return tr
			})
			s := newFakeSessionIn(t, e)
			release := make(chan struct{})
			t.Cleanup(func() { close(release) })
			s.set(func(s *fakeSession) {
				s.get = func(w http.ResponseWriter, r *http.Request) {
					select {
					case <-r.Context().Done():
					case <-release:
					}
				}
			})
			sess, err := s.e.client.CreateSession(context.Background(), testScaleSetID, "host")
			if err != nil {
				t.Fatal(err)
			}
			hang.Store(tt.hangDial)

			var gotConn, wrote atomic.Int32
			trace := &httptrace.ClientTrace{
				GetConn: func(string) { gotConn.Add(1) },
				WroteRequest: func(info httptrace.WroteRequestInfo) {
					if info.Err == nil {
						wrote.Add(1)
					}
				},
			}
			ctx, cancel := context.WithTimeout(httptrace.WithClientTrace(context.Background(), trace), 300*time.Millisecond)
			defer cancel()
			if _, err := sess.GetMessage(ctx, 0, 2); !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("GetMessage error %v, want the deadline", err)
			}
			if gotConn.Load() == 0 {
				t.Fatal("trace saw no request: the context did not reach net/http")
			}
			if got := wrote.Load() > 0; got != tt.wantWrote {
				t.Fatalf("request written = %v, want %v", got, tt.wantWrote)
			}
		})
	}
}

func TestSessionCreateConflictIsTransient(t *testing.T) {
	s := newFakeSession(t)
	s.set(func(s *fakeSession) {
		s.create = func(w http.ResponseWriter, r *http.Request) {
			writeException(w, http.StatusConflict, "TaskAgentSessionConflictException", "session exists")
		}
	})
	ctx := context.Background()
	_, err := s.e.client.CreateSession(ctx, testScaleSetID, "host")
	if err == nil {
		t.Fatal("CreateSession succeeded")
	}
	var op *retry.OpError
	if !errors.As(err, &op) || op.Op != retry.OpSessionCreate {
		t.Errorf("error %v does not name %q", err, retry.OpSessionCreate)
	}
	if c := retry.Classify(ctx, retry.OpSessionCreate, err); c != retry.Transient {
		t.Errorf("Classify = %v, want transient", c)
	}
}

func TestSessionCreateNotFoundIsPermanent(t *testing.T) {
	s := newFakeSession(t)
	s.set(func(s *fakeSession) {
		s.create = func(w http.ResponseWriter, r *http.Request) {
			writeException(w, http.StatusNotFound, "RunnerScaleSetNotFoundException", "gone")
		}
	})
	ctx := context.Background()
	_, err := s.e.client.CreateSession(ctx, testScaleSetID, "host")
	if c := retry.Classify(ctx, retry.OpSessionCreate, err); c != retry.Permanent {
		t.Errorf("Classify = %v, want permanent (err %v)", c, err)
	}
}

func TestSessionCallsNameTheirOp(t *testing.T) {
	notFound := func(w http.ResponseWriter, r *http.Request) {
		writeException(w, http.StatusNotFound, "MessageQueueNotFound", "gone")
	}
	tests := []struct {
		name string
		op   retry.Op
		set  func(*fakeSession)
		call func(context.Context, *Session) error
		want retry.Class
	}{
		{
			name: "get message 404",
			op:   retry.OpGetMessage,
			set:  func(s *fakeSession) { s.get = notFound },
			call: func(ctx context.Context, s *Session) error { _, err := s.GetMessage(ctx, 0, 2); return err },
			want: retry.SessionLost,
		},
		{
			name: "delete message 404",
			op:   retry.OpDeleteMessage,
			set:  func(s *fakeSession) { s.ack = notFound },
			call: func(ctx context.Context, s *Session) error { return s.DeleteMessage(ctx, 3) },
			want: retry.SessionLost,
		},
		{
			name: "refresh 401 inside delete message",
			op:   retry.OpDeleteMessage,
			set: func(s *fakeSession) {
				s.token = "rotated-away"
				s.refresh = func(w http.ResponseWriter, r *http.Request) {
					writeException(w, http.StatusUnauthorized, "Unauthorized", "no")
				}
			},
			call: func(ctx context.Context, s *Session) error { return s.DeleteMessage(ctx, 3) },
			want: retry.Permanent,
		},
		{
			name: "acquire jobs 404",
			op:   retry.OpAcquireJobs,
			set:  func(s *fakeSession) { s.acquire = notFound },
			call: func(ctx context.Context, s *Session) error { _, err := s.AcquireJobs(ctx, []int64{1}); return err },
			want: retry.SessionLost,
		},
		{
			name: "refresh 404 inside acquire jobs",
			op:   retry.OpAcquireJobs,
			set: func(s *fakeSession) {
				s.token = "rotated-away"
				s.refresh = notFound
			},
			call: func(ctx context.Context, s *Session) error { _, err := s.AcquireJobs(ctx, []int64{1}); return err },
			want: retry.SessionLost,
		},
		{
			name: "401 after refresh",
			op:   retry.OpGetMessage,
			set: func(s *fakeSession) {
				s.token = "rotated-away"
				s.refresh = func(w http.ResponseWriter, r *http.Request) {
					// The refreshed token is still not accepted.
					writeJSON(w, http.StatusOK, s.sessionBody(fakeQueueToken2))
				}
			},
			call: func(ctx context.Context, s *Session) error { _, err := s.GetMessage(ctx, 0, 2); return err },
			want: retry.Permanent,
		},
		{
			name: "acquire jobs rate limited",
			op:   retry.OpAcquireJobs,
			set: func(s *fakeSession) {
				s.acquire = func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("Retry-After", "30")
					w.WriteHeader(http.StatusTooManyRequests)
				}
			},
			call: func(ctx context.Context, s *Session) error { _, err := s.AcquireJobs(ctx, []int64{1}); return err },
			want: retry.RateLimited,
		},
		{
			name: "get message undecodable 200",
			op:   retry.OpGetMessage,
			set: func(s *fakeSession) {
				s.get = func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("Content-Type", "application/json")
					_, _ = w.Write([]byte(`{"messageId":1,"body":"` + fakeJIT))
				}
			},
			call: func(ctx context.Context, s *Session) error { _, err := s.GetMessage(ctx, 0, 2); return err },
			want: retry.Transient,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := newFakeSession(t)
			ctx := context.Background()
			sess, err := s.e.client.CreateSession(ctx, testScaleSetID, "host")
			if err != nil {
				t.Fatal(err)
			}
			s.set(tc.set)
			err = tc.call(ctx, sess)
			if err == nil {
				t.Fatal("call succeeded")
			}
			var op *retry.OpError
			if !errors.As(err, &op) || op.Op != tc.op {
				t.Errorf("error %v does not name %q", err, tc.op)
			}
			if c := retry.Classify(ctx, tc.op, err); c != tc.want {
				t.Errorf("Classify = %v, want %v (err %v)", c, tc.want, err)
			}
			s.e.assertNoSecrets(t, "error", err.Error())
			s.e.assertNoSecrets(t, "logs", s.e.logs.String())
		})
	}
}

// rotatingSession is a messageSession whose token changes on every read, as
// if the library refreshed the session between any two reads.
type rotatingSession struct {
	messageSession // not called
	reads          atomic.Int64
}

func (r *rotatingSession) Session() scaleset.RunnerScaleSetSession {
	n := r.reads.Add(1)
	// Yield so that concurrent trackToken calls interleave.
	runtime.Gosched()
	return scaleset.RunnerScaleSetSession{MessageQueueAccessToken: rotatingToken(n)}
}

func rotatingToken(n int64) string {
	return "rotating-queue-token-" + strconv.FormatInt(n, 10) + "-abcdefghijklmnop"
}

// TestTrackTokenConcurrent runs trackToken from many goroutines while the
// token changes on every read. The last read must win: only the newest
// token stays registered, and every older one is unregistered.
func TestTrackTokenConcurrent(t *testing.T) {
	for range 20 {
		secrets := logging.NewSecrets()
		inner := &rotatingSession{}
		s := &Session{inner: inner, secrets: secrets}
		var wg sync.WaitGroup
		for range 16 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for range 20 {
					s.trackToken()
				}
			}()
		}
		wg.Wait()
		last := inner.reads.Load()
		s.mu.Lock()
		tracked := s.token
		s.mu.Unlock()
		if want := rotatingToken(last); tracked != want {
			t.Fatalf("tracked token = %q, want the last read %q", tracked, want)
		}
		for n := int64(1); n <= last; n++ {
			if got, want := registered(secrets, rotatingToken(n)), n == last; got != want {
				t.Fatalf("token %d registered = %v, want %v", n, got, want)
			}
		}
	}
}

// closableRotating is a rotatingSession that can be closed.
type closableRotating struct {
	rotatingSession
}

func (*closableRotating) Close(context.Context) error { return nil }

// TestTrackTokenAfterCloseRegistersNothing runs a call that ends after
// Close, as a GetMessage still in flight at shutdown would. The token it
// reads must not be registered again.
func TestTrackTokenAfterCloseRegistersNothing(t *testing.T) {
	secrets := logging.NewSecrets()
	inner := &closableRotating{}
	s := &Session{inner: inner, secrets: secrets}
	s.trackToken()
	if !registered(secrets, rotatingToken(1)) {
		t.Fatal("first token not registered")
	}
	ctx := context.Background()
	if err := s.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := s.call(ctx, retry.OpGetMessage, func(context.Context) error { return nil }); err != nil {
		t.Fatalf("call: %v", err)
	}
	s.trackToken()
	for n := int64(1); n <= inner.reads.Load(); n++ {
		if registered(secrets, rotatingToken(n)) {
			t.Errorf("token %d registered after Close", n)
		}
	}
	s.mu.Lock()
	tracked := s.token
	s.mu.Unlock()
	if tracked != "" {
		t.Errorf("tracked token = %q after Close, want empty", tracked)
	}
}
