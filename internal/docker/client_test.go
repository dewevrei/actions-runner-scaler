package docker

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"iter"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/jsonstream"
	"github.com/moby/moby/client"

	"github.com/dewevrei/actions-runner-scaler/internal/retry"
)

// Secret values the tests look for everywhere except the attach stream.
const (
	testJIT   = "eyJKSVQtU0VDUkVULXZhbHVlIjoxfQ"
	testToken = "TOKEN-SECRET-abcdefghijklmnopqrstuvwxyz0123456"
)

// call is one Engine API request the fake daemon received.
type call struct {
	method string
	path   string // without the /vX.Y prefix
	query  url.Values
	body   []byte
}

func (c call) String() string { return c.method + " " + c.path }

// fakeDaemon imitates the Engine API routes the Client uses. fail, when set,
// can answer any request with an error status before the route runs.
type fakeDaemon struct {
	t   *testing.T
	srv *httptest.Server

	mu    sync.Mutex
	calls []call
	stdin [][]byte
	// attached gets a value each time an attach stream ends.
	attached chan struct{}

	// fail returns a status and message to answer r with, or 0 to serve it.
	fail func(method, path string) (int, string)
	// drop, if it returns true, serves the request and then closes the
	// connection without a response, as if the answer were lost.
	drop func(method, path string) bool
	// wait, if set, serves POST /containers/{id}/wait.
	wait func(w http.ResponseWriter, r *http.Request)
	// handle, if set, serves routes the fake does not know.
	handle func(w http.ResponseWriter, r *http.Request, path string) bool
	// attachConn, if set, takes over a hijacked attach connection before
	// any response is written.
	attachConn func(conn net.Conn, rw *bufio.ReadWriter)
}

var versionPrefix = regexp.MustCompile(`^/v[0-9]+\.[0-9]+`)

func newFakeDaemon(t *testing.T) *fakeDaemon {
	t.Helper()
	d := &fakeDaemon{t: t, attached: make(chan struct{}, 16)}
	d.srv = httptest.NewServer(http.HandlerFunc(d.serve))
	t.Cleanup(d.srv.Close)
	return d
}

// engine returns an Engine API client for the fake daemon with the hooks
// NewEngine installs and a fixed API version, so no version negotiation
// request shows up in the calls.
func (d *fakeDaemon) engine(t *testing.T) *client.Client {
	t.Helper()
	opts := append(statusHooks(), client.WithHost("tcp://"+d.srv.Listener.Addr().String()), client.WithAPIVersion("1.56"))
	cli, err := client.New(opts...)
	if err != nil {
		t.Fatalf("client.New: %v", err)
	}
	t.Cleanup(func() { cli.Close() })
	return cli
}

// client returns a Client on engine.
func (d *fakeDaemon) client() *Client {
	d.t.Helper()
	return New(d.engine(d.t))
}

func (d *fakeDaemon) record(c call) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.calls = append(d.calls, c)
}

// callNames returns "METHOD path" of every request in order.
func (d *fakeDaemon) callNames() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	var out []string
	for _, c := range d.calls {
		out = append(out, c.String())
	}
	return out
}

func (d *fakeDaemon) find(method, path string) (call, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, c := range d.calls {
		if c.method == method && c.path == path {
			return c, true
		}
	}
	return call{}, false
}

// stdinData waits for one attach stream to end and returns all streams.
func (d *fakeDaemon) stdinData() [][]byte {
	d.t.Helper()
	select {
	case <-d.attached:
	case <-time.After(5 * time.Second):
		d.t.Fatal("no attach stream ended")
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([][]byte(nil), d.stdin...)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"message": msg})
}

func (d *fakeDaemon) serve(w http.ResponseWriter, r *http.Request) {
	path := versionPrefix.ReplaceAllString(r.URL.Path, "")
	body, _ := io.ReadAll(r.Body)
	d.record(call{method: r.Method, path: path, query: r.URL.Query(), body: body})

	if d.drop != nil && d.drop(r.Method, path) {
		if conn, _, err := w.(http.Hijacker).Hijack(); err == nil {
			conn.Close()
		}
		return
	}
	if d.fail != nil {
		if status, msg := d.fail(r.Method, path); status != 0 {
			writeError(w, status, msg)
			return
		}
	}

	parts := strings.Split(strings.Trim(path, "/"), "/")
	switch {
	case path == "/_ping":
		w.Header().Set("Api-Version", "1.56")
		_, _ = io.WriteString(w, "OK")
	case r.Method == http.MethodPost && path == "/networks/create":
		writeJSON(w, http.StatusCreated, map[string]string{"Id": "net-id"})
	case r.Method == http.MethodPost && path == "/containers/create":
		writeJSON(w, http.StatusCreated, map[string]any{"Id": "id-" + r.URL.Query().Get("name"), "Warnings": []string{}})
	case r.Method == http.MethodPost && len(parts) == 3 && parts[0] == "containers" && parts[2] == "start":
		w.WriteHeader(http.StatusNoContent)
	case r.Method == http.MethodPost && len(parts) == 3 && parts[0] == "containers" && parts[2] == "attach":
		d.attach(w)
	case r.Method == http.MethodPost && len(parts) == 3 && parts[0] == "containers" && parts[2] == "wait" && d.wait != nil:
		d.wait(w, r)
	case r.Method == http.MethodDelete && len(parts) == 2 && (parts[0] == "containers" || parts[0] == "networks"):
		w.WriteHeader(http.StatusNoContent)
	default:
		if d.handle != nil && d.handle(w, r, path) {
			return
		}
		d.t.Errorf("fake daemon: unexpected request %s %s", r.Method, path)
		writeError(w, http.StatusNotImplemented, "not implemented")
	}
}

// attach upgrades the connection like the daemon (101 UPGRADED, raw
// stream) and keeps everything the client writes until it closes stdin.
func (d *fakeDaemon) attach(w http.ResponseWriter) {
	conn, rw, err := w.(http.Hijacker).Hijack()
	if err != nil {
		d.t.Errorf("hijack: %v", err)
		return
	}
	defer conn.Close()
	if d.attachConn != nil {
		d.attachConn(conn, rw)
		return
	}
	_, _ = rw.WriteString("HTTP/1.1 101 UPGRADED\r\nContent-Type: application/vnd.docker.raw-stream\r\nConnection: Upgrade\r\nUpgrade: tcp\r\n\r\n")
	if err := rw.Flush(); err != nil {
		d.t.Errorf("attach flush: %v", err)
		return
	}
	_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	data, err := io.ReadAll(rw)
	if err != nil {
		d.t.Errorf("attach read: %v", err)
	}
	d.mu.Lock()
	d.stdin = append(d.stdin, data)
	d.mu.Unlock()
	d.attached <- struct{}{}
}

// assertNoSecrets checks that no request line, query or body carried the
// JIT config or the token (R36).
func (d *fakeDaemon) assertNoSecrets(t *testing.T) {
	t.Helper()
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, c := range d.calls {
		text := c.method + " " + c.path + "?" + c.query.Encode() + "\n" + string(c.body)
		for _, secret := range []string{testJIT, testToken} {
			if strings.Contains(text, secret) {
				t.Errorf("request %s carries a secret", c)
			}
		}
	}
}

func assertErrorHasNoSecrets(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		return
	}
	for _, secret := range []string{testJIT, testToken} {
		if strings.Contains(err.Error(), secret) || strings.Contains(fmt.Sprintf("%+v %#v", err, err), secret) {
			t.Errorf("error %q carries a secret", err)
		}
	}
}

func httpStatus(err error) (int, bool) {
	var sc retry.StatusCoder
	if errors.As(err, &sc) {
		return sc.HTTPStatus(), true
	}
	return 0, false
}

func opOf(err error) retry.Op {
	var oe *retry.OpError
	if errors.As(err, &oe) {
		return oe.Op
	}
	return ""
}

var (
	bkName  = testRunner + "-buildkit"
	netName = testRunner + "-net"
)

// The happy path of StartRunner, step by step (R63).
var startCalls = []string{
	"POST /networks/create",
	"POST /containers/create",                  // BuildKit
	"POST /containers/id-" + bkName + "/start", // BuildKit started before the runner exists
	"POST /containers/create",                  // runner
	"POST /containers/id-" + testRunner + "/attach",
	"POST /containers/id-" + testRunner + "/start",
}

func TestStartRunner(t *testing.T) {
	d := newFakeDaemon(t)
	c := d.client()

	if err := c.StartRunner(context.Background(), testParams(), NewStdin(testJIT, testToken)); err != nil {
		t.Fatalf("StartRunner: %v", err)
	}

	if got := d.callNames(); !reflect.DeepEqual(got, startCalls) {
		t.Errorf("calls:\n got %q\nwant %q", got, startCalls)
	}
	// The two creates are BuildKit then runner, by name.
	var names []string
	d.mu.Lock()
	for _, cl := range d.calls {
		if cl.path == "/containers/create" {
			names = append(names, cl.query.Get("name"))
		}
	}
	d.mu.Unlock()
	if want := []string{bkName, testRunner}; !reflect.DeepEqual(names, want) {
		t.Errorf("create order = %q, want %q", names, want)
	}

	// The two lines reached stdin, each ending in \n, and nothing else.
	stdin := d.stdinData()
	if want := testJIT + "\n" + testToken + "\n"; len(stdin) != 1 || string(stdin[0]) != want {
		t.Errorf("stdin = %q, want one stream %q", stdin, want)
	}
	if c, ok := d.find(http.MethodPost, "/containers/id-"+testRunner+"/attach"); ok {
		if got := c.query; got.Get("stream") != "1" || got.Get("stdin") != "1" || got.Has("stdout") || got.Has("stderr") || got.Has("logs") {
			t.Errorf("attach query = %v, want stream and stdin only", got)
		}
	}
	d.assertNoSecrets(t)
}

// With buildkit.enabled false, StartRunner makes the network and the runner
// container only, the runner without BUILDKIT_HOST, and a failed runner
// create undoes the network alone.
func TestStartRunnerWithoutBuildKit(t *testing.T) {
	p := testParams()
	p.BuildKitEnabled = false

	d := newFakeDaemon(t)
	if err := d.client().StartRunner(context.Background(), p, NewStdin(testJIT, testToken)); err != nil {
		t.Fatalf("StartRunner: %v", err)
	}
	want := []string{
		"POST /networks/create",
		"POST /containers/create", // runner
		"POST /containers/id-" + testRunner + "/attach",
		"POST /containers/id-" + testRunner + "/start",
	}
	if got := d.callNames(); !reflect.DeepEqual(got, want) {
		t.Errorf("calls:\n got %q\nwant %q", got, want)
	}
	c, ok := d.find(http.MethodPost, "/containers/create")
	if !ok {
		t.Fatal("no container create")
	}
	if name := c.query.Get("name"); name != testRunner {
		t.Errorf("created %q, want only the runner %q", name, testRunner)
	}
	var body container.Config
	if err := json.Unmarshal(c.body, &body); err != nil {
		t.Fatal(err)
	}
	for _, e := range body.Env {
		if strings.HasPrefix(e, EnvBuildKitHost+"=") {
			t.Errorf("runner Env has %q", e)
		}
	}

	d = newFakeDaemon(t)
	d.fail = func(method, path string) (int, string) {
		if path == "/containers/create" {
			return http.StatusInternalServerError, "create failed"
		}
		return 0, ""
	}
	if err := d.client().StartRunner(context.Background(), p, NewStdin(testJIT, testToken)); err == nil {
		t.Fatal("StartRunner succeeded with a failing create")
	}
	want = []string{
		"POST /networks/create",
		"POST /containers/create",
		"DELETE /containers/" + testRunner,
		"DELETE /networks/net-id",
	}
	if got := d.callNames(); !reflect.DeepEqual(got, want) {
		t.Errorf("undo calls:\n got %q\nwant %q", got, want)
	}
}

func TestStartRunnerRequestBodies(t *testing.T) {
	d := newFakeDaemon(t)
	c := d.client()
	if err := c.StartRunner(context.Background(), testParams(), NewStdin(testJIT, testToken)); err != nil {
		t.Fatalf("StartRunner: %v", err)
	}

	nc, ok := d.find(http.MethodPost, "/networks/create")
	if !ok {
		t.Fatal("no network create")
	}
	var nreq struct {
		Name       string
		Driver     string
		EnableIPv4 *bool
		EnableIPv6 *bool
		Labels     map[string]string
	}
	if err := json.Unmarshal(nc.body, &nreq); err != nil {
		t.Fatal(err)
	}
	if nreq.Name != netName || nreq.Driver != "bridge" || nreq.Labels[LabelKind] != KindNetwork {
		t.Errorf("network create = %+v", nreq)
	}
	if nreq.EnableIPv4 == nil || !*nreq.EnableIPv4 || nreq.EnableIPv6 == nil || *nreq.EnableIPv6 {
		t.Errorf("network create EnableIPv4 = %v, EnableIPv6 = %v; want sent as true and false (R75)", nreq.EnableIPv4, nreq.EnableIPv6)
	}

	type createBody struct {
		container.Config
		HostConfig container.HostConfig
	}
	bodies := map[string]createBody{}
	d.mu.Lock()
	for _, cl := range d.calls {
		if cl.path != "/containers/create" {
			continue
		}
		if p := cl.query.Get("platform"); p != "linux/arm64" {
			t.Errorf("create %s platform = %q, want linux/arm64 (R35)", cl.query.Get("name"), p)
		}
		var b createBody
		if err := json.Unmarshal(cl.body, &b); err != nil {
			t.Fatal(err)
		}
		bodies[cl.query.Get("name")] = b
	}
	d.mu.Unlock()

	bk, rn := bodies[bkName], bodies[testRunner]
	// The client sends capabilities in canonical form.
	checkFields(t, []field{
		{"buildkit Image", bk.Image, testBuildKitImage},
		{"buildkit CapDrop", bk.HostConfig.CapDrop, []string{"ALL"}},
		{"buildkit CapAdd", bk.HostConfig.CapAdd, []string{"CAP_SETGID", "CAP_SETUID"}},
		{"buildkit SecurityOpt", bk.HostConfig.SecurityOpt, []string{"seccomp=unconfined"}},
		{"buildkit MemorySwap", bk.HostConfig.MemorySwap, bk.HostConfig.Memory},
		{"runner Image", rn.Image, testRunnerImage},
		{"runner Env", len(rn.Env), 3},
		{"runner CapDrop", rn.HostConfig.CapDrop, []string{"ALL"}},
		{"runner CapAdd", len(rn.HostConfig.CapAdd), 0},
		{"runner SecurityOpt", rn.HostConfig.SecurityOpt, []string{"no-new-privileges"}},
		{"runner OpenStdin", rn.OpenStdin, true},
		{"runner StdinOnce", rn.StdinOnce, true},
		{"runner MemorySwap", rn.HostConfig.MemorySwap, rn.HostConfig.Memory},
	})
}

// TestStartRunnerUndo fails each step in turn and checks that what was
// created is removed in reverse order, with volumes (R18, R62). A failed
// create is removed by name, since the daemon may have made it anyway.
func TestStartRunnerUndo(t *testing.T) {
	rmRunner := "DELETE /containers/id-" + testRunner
	rmBK := "DELETE /containers/id-" + bkName
	rmNet := "DELETE /networks/net-id"
	rmRunnerByName := "DELETE /containers/" + testRunner
	rmBKByName := "DELETE /containers/" + bkName
	rmNetByName := "DELETE /networks/" + netName
	tests := []struct {
		name   string
		failAt int // index in startCalls
		op     retry.Op
		undo   []string
	}{
		{"network create", 0, retry.OpNetworkCreate, []string{rmNetByName}},
		{"buildkit create", 1, retry.OpContainerCreate, []string{rmBKByName, rmNet}},
		{"buildkit start", 2, retry.OpContainerStart, []string{rmBK, rmNet}},
		{"runner create", 3, retry.OpContainerCreate, []string{rmRunnerByName, rmBK, rmNet}},
		{"runner attach", 4, retry.OpContainerStart, []string{rmRunner, rmBK, rmNet}},
		{"runner start", 5, retry.OpContainerStart, []string{rmRunner, rmBK, rmNet}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := newFakeDaemon(t)
			var n int
			var mu sync.Mutex
			d.fail = func(method, path string) (int, string) {
				mu.Lock()
				defer mu.Unlock()
				i := n
				n++
				if i == tt.failAt {
					return http.StatusInternalServerError, "boom"
				}
				return 0, ""
			}
			c := d.client()

			err := c.StartRunner(context.Background(), testParams(), NewStdin(testJIT, testToken))
			if err == nil {
				t.Fatal("StartRunner succeeded")
			}
			if got := opOf(err); got != tt.op {
				t.Errorf("op = %q, want %q", got, tt.op)
			}
			// A failed attach upgrade carries the recorded status too.
			if s, ok := httpStatus(err); !ok || s != http.StatusInternalServerError {
				t.Errorf("status = %d, %v; want 500", s, ok)
			}

			want := append(append([]string(nil), startCalls[:tt.failAt+1]...), tt.undo...)
			if got := d.callNames(); !reflect.DeepEqual(got, want) {
				t.Errorf("calls:\n got %q\nwant %q", got, want)
			}
			for _, name := range tt.undo {
				if !strings.HasPrefix(name, "DELETE /containers/") {
					continue
				}
				c, _ := d.find(http.MethodDelete, strings.TrimPrefix(name, "DELETE "))
				if c.query.Get("force") != "1" || c.query.Get("v") != "1" {
					t.Errorf("%s query = %v, want force=1 and v=1", name, c.query)
				}
			}
			d.assertNoSecrets(t)
			assertErrorHasNoSecrets(t, err)
		})
	}
}

func TestStartRunnerUndoErrors(t *testing.T) {
	tests := []struct {
		name       string
		removeCode int
		wantUndo   bool
	}{
		{"removal not found counts as done", http.StatusNotFound, false},
		{"removal failure is reported", http.StatusInternalServerError, true},
		{"removal conflict does not change the class", http.StatusConflict, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := newFakeDaemon(t)
			d.fail = func(method, path string) (int, string) {
				switch {
				case method == http.MethodPost && path == "/containers/id-"+testRunner+"/start":
					return http.StatusInternalServerError, "start failed"
				case method == http.MethodDelete:
					return tt.removeCode, "remove says no"
				}
				return 0, ""
			}
			err := d.client().StartRunner(context.Background(), testParams(), NewStdin(testJIT, testToken))
			if err == nil {
				t.Fatal("StartRunner succeeded")
			}
			if got := strings.Contains(err.Error(), "undo start"); got != tt.wantUndo {
				t.Errorf("error %q mentions undo = %v, want %v", err, got, tt.wantUndo)
			}
			// The start failure alone decides the status and class.
			if got := opOf(err); got != retry.OpContainerStart {
				t.Errorf("op = %q, want %q", got, retry.OpContainerStart)
			}
			if s, _ := httpStatus(err); s != http.StatusInternalServerError {
				t.Errorf("status = %d, want the start's 500", s)
			}
			if n := len(d.callNames()); n != len(startCalls)+3 {
				t.Errorf("%d calls, want every removal tried: %q", n, d.callNames())
			}
		})
	}
}

// TestStartRunnerUndoLostCreate loses the answer to each create after the
// daemon handled it: the resource may exist, so it is removed by name (R18).
func TestStartRunnerUndoLostCreate(t *testing.T) {
	tests := []struct {
		name  string
		match func(method, path string, q url.Values) bool
		op    retry.Op
		undo  []string
	}{
		{"network", func(m, p string, _ url.Values) bool { return p == "/networks/create" },
			retry.OpNetworkCreate, []string{"DELETE /networks/" + netName}},
		{"buildkit", func(m, p string, q url.Values) bool { return p == "/containers/create" && q.Get("name") == bkName },
			retry.OpContainerCreate, []string{"DELETE /containers/" + bkName, "DELETE /networks/net-id"}},
		{"runner", func(m, p string, q url.Values) bool { return p == "/containers/create" && q.Get("name") == testRunner },
			retry.OpContainerCreate, []string{"DELETE /containers/" + testRunner, "DELETE /containers/id-" + bkName, "DELETE /networks/net-id"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := newFakeDaemon(t)
			d.drop = func(method, path string) bool {
				d.mu.Lock()
				last := d.calls[len(d.calls)-1]
				d.mu.Unlock()
				return tt.match(method, path, last.query)
			}
			err := d.client().StartRunner(context.Background(), testParams(), NewStdin(testJIT, testToken))
			if err == nil {
				t.Fatal("StartRunner succeeded")
			}
			if got := opOf(err); got != tt.op {
				t.Errorf("op = %q, want %q", got, tt.op)
			}
			if _, ok := httpStatus(err); ok {
				t.Errorf("lost answer has a status: %v", err)
			}
			if strings.Contains(err.Error(), "undo start") {
				t.Errorf("removals failed: %v", err)
			}
			calls := d.callNames()
			if n := len(calls); n < len(tt.undo) || !reflect.DeepEqual(calls[n-len(tt.undo):], tt.undo) {
				t.Errorf("calls end with %q, want %q", calls, tt.undo)
			}
			for _, rm := range tt.undo {
				if !strings.HasPrefix(rm, "DELETE /containers/") {
					continue
				}
				c, _ := d.find(http.MethodDelete, strings.TrimPrefix(rm, "DELETE "))
				if c.query.Get("force") != "1" || c.query.Get("v") != "1" {
					t.Errorf("%s query = %v, want force=1 and v=1", rm, c.query)
				}
			}
		})
	}
}

// A create the daemon refused made nothing: the removal by name finds
// nothing, which counts as done.
func TestStartRunnerUndoCreateNotFound(t *testing.T) {
	d := newFakeDaemon(t)
	d.fail = func(method, path string) (int, string) {
		switch {
		case method == http.MethodPost && path == "/containers/create":
			if _, ok := d.find(http.MethodPost, "/containers/id-"+bkName+"/start"); ok {
				return http.StatusBadRequest, "invalid"
			}
		case method == http.MethodDelete && path == "/containers/"+testRunner:
			return http.StatusNotFound, "no such container"
		}
		return 0, ""
	}
	err := d.client().StartRunner(context.Background(), testParams(), NewStdin(testJIT, testToken))
	if err == nil {
		t.Fatal("StartRunner succeeded")
	}
	if strings.Contains(err.Error(), "undo start") {
		t.Errorf("not-found removal reported: %v", err)
	}
	if s, _ := httpStatus(err); s != http.StatusBadRequest {
		t.Errorf("status = %d, want the create's 400", s)
	}
	want := append(append([]string(nil), startCalls[:4]...),
		"DELETE /containers/"+testRunner, "DELETE /containers/id-"+bkName, "DELETE /networks/net-id")
	if got := d.callNames(); !reflect.DeepEqual(got, want) {
		t.Errorf("calls:\n got %q\nwant %q", got, want)
	}
}

func TestStartRunnerRejectsBadInput(t *testing.T) {
	tests := []struct {
		name string
		p    Params
		in   Stdin
	}{
		{"empty name", Params{}, NewStdin(testJIT, testToken)},
		{"zero stdin", testParams(), Stdin{}},
		{"empty JIT", testParams(), NewStdin("", testToken)},
		{"empty token", testParams(), NewStdin(testJIT, "")},
		{"JIT with newline", testParams(), NewStdin(testJIT+"\n"+testJIT, testToken)},
		{"token with carriage return", testParams(), NewStdin(testJIT, testToken+"\r")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := newFakeDaemon(t)
			err := d.client().StartRunner(context.Background(), tt.p, tt.in)
			if err == nil {
				t.Fatal("StartRunner succeeded")
			}
			if n := len(d.callNames()); n != 0 {
				t.Errorf("%d Docker calls, want 0", n)
			}
			assertErrorHasNoSecrets(t, err)
		})
	}
}

func TestStdinHidesSecrets(t *testing.T) {
	in := NewStdin(testJIT, testToken)
	var logs bytes.Buffer
	slog.New(slog.NewJSONHandler(&logs, nil)).Info("start", "stdin", in)
	slog.New(slog.NewTextHandler(&logs, nil)).Info("start", "stdin", in)
	js, _ := json.Marshal(in)
	outputs := []string{
		fmt.Sprintf("%v %+v %#v %s %q", in, in, in, in, in),
		fmt.Sprint(in, &in),
		logs.String(),
		string(js),
	}
	for _, out := range outputs {
		if strings.Contains(out, testJIT) || strings.Contains(out, testToken) {
			t.Errorf("output carries a secret: %s", out)
		}
	}
}

// TestRemoveStatus checks the converted status of each removal outcome and
// how retry.Classify reads it (R50).
func TestRemoveStatus(t *testing.T) {
	tests := []struct {
		name   string
		code   int
		target string // "container" or "network"
		want   retry.Class
	}{
		{"container not found", http.StatusNotFound, "container", retry.Gone},
		{"container removal in progress", http.StatusConflict, "container", retry.Transient},
		{"container server error", http.StatusInternalServerError, "container", retry.Transient},
		{"container bad parameter", http.StatusBadRequest, "container", retry.Permanent},
		{"network not found", http.StatusNotFound, "network", retry.Gone},
		{"network has endpoints", http.StatusForbidden, "network", retry.Transient},
		{"network server error", http.StatusInternalServerError, "network", retry.Transient},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := newFakeDaemon(t)
			d.fail = func(method, path string) (int, string) {
				if method == http.MethodDelete {
					return tt.code, "No such object: x"
				}
				return 0, ""
			}
			c := d.client()
			ctx := context.Background()

			var err error
			op := retry.OpContainerRemove
			if tt.target == "network" {
				op = retry.OpNetworkRemove
				err = c.removeNetwork(ctx, "x")
			} else {
				err = c.removeContainer(ctx, "x")
			}
			if s, ok := httpStatus(err); !ok || s != tt.code {
				t.Fatalf("HTTPStatus = %d, %v; want %d (err %v)", s, ok, tt.code, err)
			}
			if got := opOf(err); got != op {
				t.Errorf("op = %q, want %q", got, op)
			}
			if got := retry.Classify(ctx, op, err); got != tt.want {
				t.Errorf("Classify = %v, want %v", got, tt.want)
			}
			if got := IsNotFound(err); got != (tt.code == http.StatusNotFound) {
				t.Errorf("IsNotFound = %v", got)
			}
		})
	}
}

func TestRemoveResources(t *testing.T) {
	want := []string{
		"DELETE /containers/" + testRunner,
		"DELETE /containers/" + bkName,
		"DELETE /networks/" + netName,
	}
	tests := []struct {
		name     string
		fail     func(method, path string) (int, string)
		wantErr  bool
		wantCls  retry.Class
		wantCode int
	}{
		{"all removed", nil, false, 0, 0},
		{"all already gone", func(string, string) (int, string) { return http.StatusNotFound, "No such container" }, false, 0, 0},
		{"runner container busy, rest still tried", func(m, p string) (int, string) {
			if p == "/containers/"+testRunner {
				return http.StatusConflict, "removal of container is already in progress"
			}
			return 0, ""
		}, true, retry.Transient, http.StatusConflict},
		{"network still has endpoints", func(m, p string) (int, string) {
			if strings.HasPrefix(p, "/networks/") {
				return http.StatusForbidden, "network has active endpoints"
			}
			return http.StatusNotFound, "No such container"
		}, true, retry.Transient, http.StatusForbidden},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := newFakeDaemon(t)
			d.fail = tt.fail
			ctx := context.Background()
			err := d.client().RemoveResources(ctx, testRunner)
			if (err != nil) != tt.wantErr {
				t.Fatalf("RemoveResources = %v, want error %v", err, tt.wantErr)
			}
			if got := d.callNames(); !reflect.DeepEqual(got, want) {
				t.Errorf("calls:\n got %q\nwant %q", got, want)
			}
			for _, p := range want[:2] {
				c, _ := d.find(http.MethodDelete, strings.TrimPrefix(p, "DELETE "))
				if c.query.Get("force") != "1" || c.query.Get("v") != "1" {
					t.Errorf("%s query = %v, want force=1 and v=1 (R76)", p, c.query)
				}
			}
			if err == nil {
				return
			}
			if s, _ := httpStatus(err); s != tt.wantCode {
				t.Errorf("status = %d, want %d", s, tt.wantCode)
			}
			for _, op := range []retry.Op{retry.OpContainerRemove, retry.OpNetworkRemove} {
				if got := retry.Classify(ctx, op, err); got != tt.wantCls {
					t.Errorf("Classify(%s) = %v, want %v", op, got, tt.wantCls)
				}
			}
		})
	}
}

func TestRemoveContainer(t *testing.T) {
	for _, code := range []int{0, http.StatusNotFound} {
		d := newFakeDaemon(t)
		d.fail = func(method, path string) (int, string) { return code, "No such container" }
		if err := d.client().RemoveContainer(context.Background(), testRunner); err != nil {
			t.Errorf("status %d: RemoveContainer = %v, want nil", code, err)
		}
		// Only the runner container; BuildKit and the network stay.
		if got, want := d.callNames(), []string{"DELETE /containers/" + testRunner}; !reflect.DeepEqual(got, want) {
			t.Errorf("calls = %q, want %q", got, want)
		}
		c, _ := d.find(http.MethodDelete, "/containers/"+testRunner)
		if c.query.Get("force") != "1" || c.query.Get("v") != "1" {
			t.Errorf("query = %v, want force=1 and v=1", c.query)
		}
	}

	d := newFakeDaemon(t)
	d.fail = func(string, string) (int, string) { return http.StatusConflict, "in progress" }
	err := d.client().RemoveContainer(context.Background(), testRunner)
	if got := retry.Classify(context.Background(), retry.OpContainerRemove, err); got != retry.Transient {
		t.Errorf("409: Classify = %v, want Transient (err %v)", got, err)
	}
}

// TestRemoveByID checks the route each kind takes, the container removal
// flags (R76), not found as success and the Op on other failures (R50).
func TestRemoveByID(t *testing.T) {
	const id = "0123abcd"
	routes := []struct {
		kind string
		call string
		op   retry.Op
	}{
		{KindNetwork, "DELETE /networks/" + id, retry.OpNetworkRemove},
		{KindRunner, "DELETE /containers/" + id, retry.OpContainerRemove},
		{KindBuildKit, "DELETE /containers/" + id, retry.OpContainerRemove},
		{"", "DELETE /containers/" + id, retry.OpContainerRemove},
		{"volume", "DELETE /containers/" + id, retry.OpContainerRemove},
	}
	statuses := []struct {
		code    int
		wantErr bool
		want    retry.Class
	}{
		{0, false, 0},
		{http.StatusNotFound, false, 0},
		{http.StatusConflict, true, retry.Transient},
		{http.StatusForbidden, true, retry.Transient},
		{http.StatusInternalServerError, true, retry.Transient},
		{http.StatusBadRequest, true, retry.Permanent},
	}
	for _, rt := range routes {
		for _, st := range statuses {
			t.Run(fmt.Sprintf("kind %q status %d", rt.kind, st.code), func(t *testing.T) {
				d := newFakeDaemon(t)
				d.fail = func(string, string) (int, string) { return st.code, "failed" }
				ctx := context.Background()
				err := d.client().RemoveByID(ctx, rt.kind, id)

				if got := d.callNames(); !reflect.DeepEqual(got, []string{rt.call}) {
					t.Errorf("calls = %q, want %q", got, []string{rt.call})
				}
				c, ok := d.find(http.MethodDelete, strings.TrimPrefix(rt.call, "DELETE "))
				if !ok {
					t.Fatalf("no recorded %s", rt.call)
				}
				if rt.op == retry.OpContainerRemove {
					if c.query.Get("force") != "1" || c.query.Get("v") != "1" {
						t.Errorf("query = %v, want force=1 and v=1 (R76)", c.query)
					}
				} else if len(c.query) != 0 {
					t.Errorf("network query = %v, want none", c.query)
				}

				if !st.wantErr {
					if err != nil {
						t.Errorf("RemoveByID = %v, want nil", err)
					}
					return
				}
				if got := opOf(err); got != rt.op {
					t.Errorf("op = %q, want %q (err %v)", got, rt.op, err)
				}
				if s, ok := httpStatus(err); !ok || s != st.code {
					t.Errorf("HTTPStatus = %d, %v; want %d", s, ok, st.code)
				}
				if got := retry.Classify(ctx, rt.op, err); got != st.want {
					t.Errorf("Classify = %v, want %v", got, st.want)
				}
			})
		}
	}
}

// TestRemoveByIDEmptyID checks that an empty ID fails before any request,
// as a permanent error with the removal Op.
func TestRemoveByIDEmptyID(t *testing.T) {
	for kind, op := range map[string]retry.Op{KindNetwork: retry.OpNetworkRemove, KindRunner: retry.OpContainerRemove} {
		d := newFakeDaemon(t)
		ctx := context.Background()
		err := d.client().RemoveByID(ctx, kind, "")
		if err == nil {
			t.Fatalf("kind %s: RemoveByID with empty id = nil, want error", kind)
		}
		if got := opOf(err); got != op {
			t.Errorf("kind %s: op = %q, want %q", kind, got, op)
		}
		if got := retry.Classify(ctx, op, err); got != retry.Permanent {
			t.Errorf("kind %s: Classify = %v, want Permanent", kind, got)
		}
		if got := d.callNames(); len(got) != 0 {
			t.Errorf("kind %s: calls = %q, want none", kind, got)
		}
	}
}

func TestWait(t *testing.T) {
	d := newFakeDaemon(t)
	d.wait = func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("condition"); got != "not-running" {
			t.Errorf("condition = %q, want not-running", got)
		}
		writeJSON(w, http.StatusOK, container.WaitResponse{StatusCode: 3})
	}
	code, err := d.client().Wait(context.Background(), testRunner)
	if err != nil || code != 3 {
		t.Errorf("Wait = %d, %v; want 3, nil", code, err)
	}
	if got, want := d.callNames(), []string{"POST /containers/" + testRunner + "/wait"}; !reflect.DeepEqual(got, want) {
		t.Errorf("calls = %q, want %q", got, want)
	}
}

func TestWaitNotFound(t *testing.T) {
	d := newFakeDaemon(t)
	d.fail = func(string, string) (int, string) { return http.StatusNotFound, "No such container" }
	_, err := d.client().Wait(context.Background(), testRunner)
	if !IsNotFound(err) || opOf(err) != retry.OpContainerWait {
		t.Errorf("Wait = %v, want not found from %q", err, retry.OpContainerWait)
	}
}

func TestWaitCanceled(t *testing.T) {
	d := newFakeDaemon(t)
	started := make(chan struct{})
	d.wait = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		close(started)
		<-r.Context().Done()
	}
	// Built here: d.client may call t.Fatalf, which must not run in the
	// goroutine below.
	c := d.client()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := c.Wait(ctx, testRunner)
		done <- err
	}()
	<-started
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("Wait returned nil after cancel")
		}
		if got := retry.Classify(ctx, retry.OpContainerWait, err); got != retry.Canceled {
			t.Errorf("Classify = %v, want Canceled (err %v)", got, err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Wait did not return after cancel")
	}
}

func TestList(t *testing.T) {
	d := newFakeDaemon(t)
	d.handle = func(w http.ResponseWriter, r *http.Request, path string) bool {
		switch path {
		case "/containers/json":
			writeJSON(w, http.StatusOK, []map[string]any{
				{"Id": "c1", "Names": []string{"/" + testRunner}, "State": "running", "Labels": labels("", testRunner, KindRunner)},
				{"Id": "c2", "Names": []string{"/" + bkName}, "State": "exited", "Labels": labels("", testRunner, KindBuildKit)},
				// No kind label: State still tells it is a container.
				{"Id": "c3", "Names": []string{"/stray"}, "State": "created", "Labels": map[string]string{LabelManagedBy: ManagedBy}},
			})
		case "/networks":
			writeJSON(w, http.StatusOK, []map[string]any{
				{"Id": "n1", "Name": netName, "Labels": labels("", testRunner, KindNetwork)},
			})
		default:
			return false
		}
		return true
	}
	got, err := d.client().List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	want := []Resource{
		{Kind: KindRunner, ID: "c1", Name: testRunner, RunnerName: testRunner, State: "running"},
		{Kind: KindBuildKit, ID: "c2", Name: bkName, RunnerName: testRunner, State: "exited"},
		{ID: "c3", Name: "stray", State: "created"},
		{Kind: KindNetwork, ID: "n1", Name: netName, RunnerName: testRunner},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("List:\n got %+v\nwant %+v", got, want)
	}

	cl, _ := d.find(http.MethodGet, "/containers/json")
	if cl.query.Get("all") != "1" {
		t.Errorf("container list query = %v, want all=1", cl.query)
	}
	wantFilter := `{"label":{"managed-by=actions-runner-scaler":true}}`
	if got := cl.query.Get("filters"); got != wantFilter {
		t.Errorf("container filters = %s, want %s", got, wantFilter)
	}
	nl, _ := d.find(http.MethodGet, "/networks")
	if got := nl.query.Get("filters"); got != wantFilter {
		t.Errorf("network filters = %s, want %s", got, wantFilter)
	}
}

// TestManagedByValue checks that a Client with another managed-by value (as
// the integration tests build one) labels everything it creates with that
// value and lists only by it, so a running scaler, which lists and removes
// by ManagedBy, never touches those resources (R16, R17).
func TestManagedByValue(t *testing.T) {
	const other = "actions-runner-scaler-test"
	d := newFakeDaemon(t)
	d.handle = func(w http.ResponseWriter, r *http.Request, path string) bool {
		writeJSON(w, http.StatusOK, []any{})
		return true
	}
	c := d.client()
	c.managedBy = other
	if err := c.StartRunner(context.Background(), testParams(), NewStdin(testJIT, testToken)); err != nil {
		t.Fatalf("StartRunner: %v", err)
	}
	if _, err := c.List(context.Background()); err != nil {
		t.Fatalf("List: %v", err)
	}

	d.mu.Lock()
	defer d.mu.Unlock()
	var created int
	for _, cl := range d.calls {
		if cl.path != "/networks/create" && cl.path != "/containers/create" {
			continue
		}
		created++
		var body struct{ Labels map[string]string }
		if err := json.Unmarshal(cl.body, &body); err != nil {
			t.Fatal(err)
		}
		if got := body.Labels[LabelManagedBy]; got != other {
			t.Errorf("%s label %s = %q, want %q", cl, LabelManagedBy, got, other)
		}
	}
	if created != 3 {
		t.Errorf("created %d resources, want 3", created)
	}
	wantFilter := `{"label":{"managed-by=` + other + `":true}}`
	for _, cl := range d.calls {
		if cl.path == "/containers/json" || cl.path == "/networks" {
			if got := cl.query.Get("filters"); got != wantFilter {
				t.Errorf("%s filters = %s, want %s", cl, got, wantFilter)
			}
		}
	}
}

func TestListErrors(t *testing.T) {
	for _, tt := range []struct {
		failPath string
		op       retry.Op
	}{
		{"/containers/json", retry.OpContainerList},
		{"/networks", retry.OpNetworkList},
	} {
		d := newFakeDaemon(t)
		d.fail = func(m, p string) (int, string) {
			if p == tt.failPath {
				return http.StatusInternalServerError, "boom"
			}
			return 0, ""
		}
		d.handle = func(w http.ResponseWriter, r *http.Request, path string) bool {
			writeJSON(w, http.StatusOK, []any{})
			return true
		}
		_, err := d.client().List(context.Background())
		if opOf(err) != tt.op {
			t.Errorf("%s fails: err = %v, want op %q", tt.failPath, err, tt.op)
		}
	}
}

func TestProcesses(t *testing.T) {
	d := newFakeDaemon(t)
	d.handle = func(w http.ResponseWriter, r *http.Request, path string) bool {
		if path != "/containers/"+testRunner+"/top" {
			return false
		}
		writeJSON(w, http.StatusOK, container.TopResponse{
			Titles: []string{"UID", "PID", "PPID", "C", "STIME", "TTY", "TIME", "CMD"},
			Processes: [][]string{
				{"1001", "10", "1", "0", "10:00", "?", "00:00:00", "/home/runner/bin/Runner.Listener run"},
				{"1001", "11", "10", "0", "10:00", "?", "00:00:01", "/home/runner/bin/Runner.Worker spawnclient 1 2"},
			},
		})
		return true
	}
	got, err := d.client().Processes(context.Background(), testRunner)
	if err != nil {
		t.Fatalf("Processes: %v", err)
	}
	want := []string{"/home/runner/bin/Runner.Listener run", "/home/runner/bin/Runner.Worker spawnclient 1 2"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Processes = %q, want %q", got, want)
	}
}

// TestInfoCapacity checks that one /info call yields NCPU and MemTotal, and
// that a failure carries OpDockerInfo and the status (0002 SPEC R201).
func TestInfoCapacity(t *testing.T) {
	d := newFakeDaemon(t)
	d.handle = func(w http.ResponseWriter, r *http.Request, path string) bool {
		if path != "/info" {
			return false
		}
		writeJSON(w, http.StatusOK, map[string]any{"NCPU": 10, "MemTotal": 8321515520})
		return true
	}
	got, err := d.client().Info(context.Background())
	if err != nil || got != (Capacity{NCPU: 10, MemTotal: 8321515520}) {
		t.Errorf("Info = %+v, %v", got, err)
	}
	var infos int
	for _, name := range d.callNames() {
		if strings.HasSuffix(name, "/info") {
			infos++
		}
	}
	if infos != 1 {
		t.Errorf("/info calls = %d, want 1", infos)
	}

	d = newFakeDaemon(t)
	d.fail = func(method, path string) (int, string) {
		if path == "/info" {
			return http.StatusInternalServerError, "boom"
		}
		return 0, ""
	}
	_, err = d.client().Info(context.Background())
	if err == nil || opOf(err) != retry.OpDockerInfo {
		t.Fatalf("Info error = %v, op %q, want op %q", err, opOf(err), retry.OpDockerInfo)
	}
	if st, ok := httpStatus(err); !ok || st != http.StatusInternalServerError {
		t.Errorf("status = %d, %v, want 500", st, ok)
	}
}

func TestPing(t *testing.T) {
	d := newFakeDaemon(t)
	if err := d.client().Ping(context.Background()); err != nil {
		t.Errorf("Ping: %v", err)
	}

	// No daemon: a transport error without a status, so Transient.
	cli, err := NewEngine("tcp://127.0.0.1:1")
	if err != nil {
		t.Fatal(err)
	}
	defer cli.Close()
	ctx := context.Background()
	err = New(cli).Ping(ctx)
	if err == nil {
		t.Fatal("Ping succeeded without a daemon")
	}
	if opOf(err) != retry.OpDockerPing {
		t.Errorf("op = %q, want %q", opOf(err), retry.OpDockerPing)
	}
	if _, ok := httpStatus(err); ok {
		t.Errorf("transport error has a status: %v", err)
	}
	if got := retry.Classify(ctx, retry.OpDockerPing, err); got != retry.Transient {
		t.Errorf("Classify = %v, want Transient", got)
	}
}

func TestConvert(t *testing.T) {
	ctx := context.Background()
	if opError(ctx, retry.OpDockerPing, nil) != nil {
		t.Error("opError(nil) != nil")
	}
	// Context errors keep no status, so Classify sees them as such.
	err := opError(ctx, retry.OpContainerWait, context.DeadlineExceeded)
	if _, ok := httpStatus(err); ok || !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("deadline: %v", err)
	}
	// A recorded status wins over the errdefs class.
	tctx := track(ctx)
	slotOf(tctx).code.Store(http.StatusUnprocessableEntity)
	err = opError(tctx, retry.OpContainerCreate, fmt.Errorf("wrapped: %w", context.Canceled))
	if s, ok := httpStatus(err); !ok || s != http.StatusUnprocessableEntity {
		t.Errorf("recorded status: HTTPStatus = %d, %v; want 422", s, ok)
	}
}

// An undo failure must not lend its status to a step error that has none
// (an attach whose connection drops before any response), or Classify
// would read the undo's 409.
func TestStartRunnerUndoStatusHidden(t *testing.T) {
	d := newFakeDaemon(t)
	d.attachConn = func(net.Conn, *bufio.ReadWriter) {} // closes at once
	d.fail = func(method, path string) (int, string) {
		if method == http.MethodDelete {
			return http.StatusConflict, "removal in progress"
		}
		return 0, ""
	}
	ctx := context.Background()
	err := d.client().StartRunner(ctx, testParams(), NewStdin(testJIT, testToken))
	if err == nil {
		t.Fatal("StartRunner succeeded")
	}
	if s, ok := httpStatus(err); ok {
		t.Errorf("status = %d from the undo, want none", s)
	}
	if got := retry.Classify(ctx, retry.OpContainerStart, err); got != retry.Transient {
		t.Errorf("Classify = %v, want Transient", got)
	}
}

// TestStatusWithoutErrdefsClass checks responses whose status has no errdefs
// class of its own: the client keeps only the "unknown" class, so the
// status comes from the recorded response, and retry.Classify then sees a
// permanent 4xx (PLAN M1-4 step 6, R41).
func TestStatusWithoutErrdefsClass(t *testing.T) {
	codes := []int{
		http.StatusMethodNotAllowed,
		http.StatusRequestEntityTooLarge,
		http.StatusTeapot,
		http.StatusUnprocessableEntity,
	}
	for _, code := range codes {
		t.Run(strconv.Itoa(code), func(t *testing.T) {
			d := newFakeDaemon(t)
			d.fail = func(method, path string) (int, string) {
				if path == "/containers/create" || strings.HasSuffix(path, "/attach") || method == http.MethodDelete {
					return code, "says no"
				}
				return 0, ""
			}
			c := d.client()
			ctx := context.Background()
			for _, tc := range []struct {
				name string
				op   retry.Op
				err  error
			}{
				{"create", retry.OpContainerCreate, func() error { _, err := c.create(ctx, RunnerSpec(testParams())); return err }()},
				{"attach upgrade", retry.OpContainerStart, c.startWithStdin(ctx, testRunner, NewStdin(testJIT, testToken))},
				{"container removal", retry.OpContainerRemove, c.removeContainer(ctx, testRunner)},
				{"network removal", retry.OpNetworkRemove, c.removeNetwork(ctx, netName)},
			} {
				if s, ok := httpStatus(tc.err); !ok || s != code {
					t.Errorf("%s: HTTPStatus = %d, %v; want %d (err %v)", tc.name, s, ok, code, tc.err)
				}
				if got := retry.Classify(ctx, tc.op, tc.err); got != retry.Permanent {
					t.Errorf("%s: Classify = %v, want Permanent", tc.name, got)
				}
			}
		})
	}

	// The same response through an Engine without the hooks: the class
	// alone cannot give the status back.
	d := newFakeDaemon(t)
	d.fail = func(string, string) (int, string) { return http.StatusUnprocessableEntity, "says no" }
	cli, err := client.New(client.WithHost("tcp://"+d.srv.Listener.Addr().String()), client.WithAPIVersion("1.56"))
	if err != nil {
		t.Fatal(err)
	}
	defer cli.Close()
	if s, ok := httpStatus(New(cli).removeContainer(context.Background(), testRunner)); ok {
		t.Errorf("without hooks: HTTPStatus = %d, want none", s)
	}
}

// Calls running at the same time each get their own status.
func TestStatusConcurrentCalls(t *testing.T) {
	d := newFakeDaemon(t)
	d.fail = func(method, path string) (int, string) {
		code, err := strconv.Atoi(strings.TrimPrefix(path, "/containers/c"))
		if method != http.MethodDelete || err != nil {
			return 0, ""
		}
		// Hold the first responses a little so the calls overlap.
		time.Sleep(time.Duration(code%7) * time.Millisecond)
		return code, "says no"
	}
	c := d.client()
	codes := []int{400, 403, 404, 405, 409, 413, 418, 422, 500, 503}
	var wg sync.WaitGroup
	for range 5 {
		for _, code := range codes {
			wg.Go(func() {
				err := c.removeContainer(context.Background(), "c"+strconv.Itoa(code))
				if s, _ := httpStatus(err); s != code {
					t.Errorf("call for %d: HTTPStatus = %d (err %v)", code, s, err)
				}
			})
		}
	}
	wg.Wait()
}

// A transport error after an error response in the same call keeps no
// status: each request starts with an empty slot.
func TestStatusClearedBeforeEachRequest(t *testing.T) {
	cli, err := client.New(append(statusHooks(), client.WithHost("tcp://127.0.0.1:1"), client.WithAPIVersion("1.56"))...)
	if err != nil {
		t.Fatal(err)
	}
	defer cli.Close()
	ctx := track(context.Background())
	slotOf(ctx).code.Store(http.StatusServiceUnavailable) // an earlier response
	_, err = cli.ContainerRemove(ctx, testRunner, client.ContainerRemoveOptions{})
	if err == nil {
		t.Fatal("ContainerRemove succeeded without a daemon")
	}
	if s, ok := httpStatus(opError(ctx, retry.OpContainerRemove, err)); ok {
		t.Errorf("transport error has status %d", s)
	}
}

// StartRunner cancelled halfway still removes what it created (R18): the
// removals do not use the cancelled ctx.
func TestStartRunnerUndoAfterCancel(t *testing.T) {
	d := newFakeDaemon(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	d.fail = func(method, path string) (int, string) {
		if method == http.MethodPost && path == "/containers/create" {
			if _, ok := d.find(http.MethodPost, "/containers/id-"+bkName+"/start"); ok {
				cancel() // the runner create: the caller gives up now
				return http.StatusInternalServerError, "boom"
			}
		}
		return 0, ""
	}
	err := d.client().StartRunner(ctx, testParams(), NewStdin(testJIT, testToken))
	if err == nil {
		t.Fatal("StartRunner succeeded")
	}
	want := append(append([]string(nil), startCalls[:4]...),
		"DELETE /containers/"+testRunner, "DELETE /containers/id-"+bkName, "DELETE /networks/net-id")
	if got := d.callNames(); !reflect.DeepEqual(got, want) {
		t.Errorf("calls:\n got %q\nwant %q", got, want)
	}
	if strings.Contains(err.Error(), "undo start") {
		t.Errorf("removals failed: %v", err)
	}
	if got := retry.Classify(ctx, retry.OpContainerCreate, err); got != retry.Canceled {
		t.Errorf("Classify = %v, want Canceled", got)
	}
}

// startStalled runs StartRunner until stalled is closed, cancels it and
// checks that it returns soon with the cancellation and removes the
// runner's resources anyway.
func startStalled(t *testing.T, d *fakeDaemon, c *Client, stalled <-chan struct{}) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- c.StartRunner(ctx, testParams(), NewStdin(testJIT, testToken)) }()
	select {
	case <-stalled:
	case err := <-done:
		t.Fatalf("StartRunner returned before the stall: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("the stall was not reached")
	}
	cancel()
	var err error
	select {
	case err = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("StartRunner did not return after cancel")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("StartRunner = %v, want context.Canceled", err)
	}
	if got := retry.Classify(ctx, retry.OpContainerStart, err); got != retry.Canceled {
		t.Errorf("Classify = %v, want Canceled", got)
	}
	for _, rm := range []string{"DELETE /containers/id-" + testRunner, "DELETE /containers/id-" + bkName, "DELETE /networks/net-id"} {
		if !slices.Contains(d.callNames(), rm) {
			t.Errorf("no %s after cancel: %q", rm, d.callNames())
		}
	}
	assertErrorHasNoSecrets(t, err)
}

// The daemon accepts the attach request but never answers the upgrade.
func TestStartRunnerCancelDuringAttach(t *testing.T) {
	d := newFakeDaemon(t)
	stalled := make(chan struct{})
	d.attachConn = func(conn net.Conn, rw *bufio.ReadWriter) {
		close(stalled)
		// Wait for the client to give up and close the connection.
		_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
		_, _ = io.Copy(io.Discard, rw)
	}
	startStalled(t, d, d.client(), stalled)
}

// fakeEngine is an Engine whose image pull and registry lookup are the
// functions it holds; any other call panics, as the embedded Engine is nil.
type fakeEngine struct {
	Engine
	pull         func(ctx context.Context, ref string, options client.ImagePullOptions) (client.ImagePullResponse, error)
	distribution func(ctx context.Context, ref string, options client.DistributionInspectOptions) (client.DistributionInspectResult, error)
}

func (e fakeEngine) ImagePull(ctx context.Context, ref string, options client.ImagePullOptions) (client.ImagePullResponse, error) {
	return e.pull(ctx, ref, options)
}

func (e fakeEngine) DistributionInspect(ctx context.Context, ref string, options client.DistributionInspectOptions) (client.DistributionInspectResult, error) {
	return e.distribution(ctx, ref, options)
}

// fakePullResponse is a client.ImagePullResponse that counts its Wait and
// Close calls. Wait returns waitErr, or what waitFn returns when it is set.
type fakePullResponse struct {
	waitErr error
	waitFn  func(ctx context.Context) error
	waits   int
	closes  int
}

func (r *fakePullResponse) Read([]byte) (int, error) { return 0, io.EOF }

func (r *fakePullResponse) Close() error {
	r.closes++
	return nil
}

func (r *fakePullResponse) JSONMessages(context.Context) iter.Seq2[jsonstream.Message, error] {
	return func(func(jsonstream.Message, error) bool) {}
}

func (r *fakePullResponse) Wait(ctx context.Context) error {
	r.waits++
	if r.waitFn != nil {
		return r.waitFn(ctx)
	}
	return r.waitErr
}

// pipeAttachEngine answers attach with one end of a pipe nobody reads, so
// writing stdin blocks until the connection is closed.
type pipeAttachEngine struct {
	Engine
	conn net.Conn
}

func (e pipeAttachEngine) ContainerAttach(context.Context, string, client.ContainerAttachOptions) (client.ContainerAttachResult, error) {
	return client.ContainerAttachResult{HijackedResponse: client.NewHijackedResponse(e.conn, "")}, nil
}

// The upgrade succeeds but the stdin write blocks.
func TestStartRunnerCancelDuringStdinWrite(t *testing.T) {
	d := newFakeDaemon(t)
	local, remote := net.Pipe()
	t.Cleanup(func() { remote.Close() })
	stalled := make(chan struct{})
	d.fail = func(method, path string) (int, string) {
		if method == http.MethodPost && path == "/containers/id-"+testRunner+"/start" {
			close(stalled) // the next step is the blocking write
		}
		return 0, ""
	}
	c := New(pipeAttachEngine{Engine: d.engine(t), conn: local})
	startStalled(t, d, c, stalled)
	// The closed pipe shows the write was abandoned, not finished.
	if _, err := remote.Write([]byte{0}); err == nil {
		t.Error("pipe still open after cancel")
	}
}

// Stdin kept in an unexported field of another struct, where fmt cannot
// call its String or GoString, still prints no secret.
func TestStdinHiddenInUnexportedField(t *testing.T) {
	in := NewStdin(testJIT, testToken)
	type holder struct {
		name  string
		stdin Stdin
		ptr   *Stdin
		list  []Stdin
		m     map[string]Stdin
	}
	h := holder{name: "r", stdin: in, ptr: &in, list: []Stdin{in}, m: map[string]Stdin{"k": in}}
	for _, verb := range []string{"%v", "%+v", "%#v", "%s", "%q", "%x"} {
		for _, v := range []any{h, &h, struct{ h holder }{h}} {
			out := fmt.Sprintf(verb, v)
			if strings.Contains(out, testJIT) || strings.Contains(out, testToken) ||
				strings.Contains(out, fmt.Sprintf("%x", testToken)) {
				t.Errorf("%s of %T carries a secret: %s", verb, v, out)
			}
		}
	}
	// The values are still there for the entrypoint.
	if got := string(in.bytes()); got != testJIT+"\n"+testToken+"\n" {
		t.Errorf("bytes = %q", got)
	}
}
