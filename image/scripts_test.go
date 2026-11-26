package image

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/dewevrei/actions-runner-scaler/internal/docker"
)

// The scripts run on the host with fake inputs. Every process the hook may
// kill is a sleep process this test started and lists in a fake proc
// directory, so a broken hook cannot reach real host processes.

const (
	testToken  = "TESTTOKEN-not-a-secret-0123456789abcdefghij"
	testJIT    = "TESTJIT-not-a-secret-9876543210zyxwvutsrqp"
	testName   = "linux-arm64-deploy-0123abcd"
	killPrefix = "scaler-job-started: kill -KILL "
	pollFast   = "0.1"
)

// Waits that only bound how long an event may take. They are generous so
// a loaded machine does not fail them; the bounds that come from a
// requirement (the 1s from SIGINT to kill) stay exact where they are used.
const (
	// signalExitTimeout bounds how long the hook takes to end after a
	// signal. A hook that hangs (the bash 3.2 spin) still fails it.
	signalExitTimeout = 5 * time.Second
	// outputTimeout bounds the wait for a log line the next poll prints.
	outputTimeout = 10 * time.Second
	// settle lets a few more polls (pollFast apart) run before checking
	// that the hook is still running. A slow machine runs fewer polls,
	// which weakens the check but never fails it.
	settle = 500 * time.Millisecond
)

// TestMain puts every temporary directory under one root, so any process
// left behind (a hook, its curl, a sleeper whose argv[0] is under root) can
// be found by its arguments afterwards.
func TestMain(m *testing.M) {
	os.Exit(runTests(m))
}

func runTests(m *testing.M) int {
	root, err := os.MkdirTemp("", "scripts-test-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer os.RemoveAll(root)
	if err := os.Setenv("TMPDIR", root); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	code := m.Run()
	// A curl orphaned by a killed hook ends within its --max-time.
	if left := processesUnder(root, 10*time.Second); left != "" {
		fmt.Fprintf(os.Stderr, "processes left behind:\n%s", left)
		return 1
	}
	return code
}

// processesUnder waits up to timeout until no process has root in its
// arguments and returns the ones that remain. It never signals them.
func processesUnder(root string, timeout time.Duration) string {
	ps, err := exec.LookPath("ps")
	if err != nil {
		return ""
	}
	deadline := time.Now().Add(timeout)
	for {
		out, err := exec.Command(ps, "-A", "-ww", "-o", "pid=,command=").Output()
		if err != nil {
			return "ps: " + err.Error() + "\n"
		}
		var left strings.Builder
		for line := range strings.Lines(string(out)) {
			if strings.Contains(line, root) {
				left.WriteString(line)
			}
		}
		if left.Len() == 0 || time.Now().After(deadline) {
			return left.String()
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// testBash returns the bash that runs the scripts: HOOK_TEST_BASH if set,
// else /bin/bash. macOS ships bash 3.2 and the image ships a newer bash
// (version not yet checked), so the same tests can run under both.
func testBash() string {
	if b := os.Getenv("HOOK_TEST_BASH"); b != "" {
		return b
	}
	return "/bin/bash"
}

func requireShellTools(t *testing.T) {
	t.Helper()
	if b := os.Getenv("HOOK_TEST_BASH"); b != "" {
		if _, err := os.Stat(b); err != nil {
			t.Fatalf("HOOK_TEST_BASH: %v", err)
		}
	}
	for _, p := range []string{testBash(), "/usr/bin/curl"} {
		if _, err := os.Stat(p); err != nil {
			t.Skipf("%s is not available: %v", p, err)
		}
	}
}

// writeScript copies a script into dir and replaces only the given
// one-line constants.
func writeScript(t *testing.T, dir, name string, consts map[string]string) string {
	t.Helper()
	src, err := os.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(string(src), "\n")
	replaced := make(map[string]bool)
	for i, line := range lines {
		for k, v := range consts {
			if strings.HasPrefix(line, k+"=") {
				lines[i] = k + "=" + v
				replaced[k] = true
			}
		}
	}
	for k := range consts {
		if !replaced[k] {
			t.Fatalf("%s: no constant line %s=", name, k)
		}
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// rewriteLine replaces the one line of a copied script that equals old.
func rewriteLine(t *testing.T, path, old, new string) {
	t.Helper()
	rewriteLineIn(t, path, "", old, new)
}

// rewriteLineIn is rewriteLine limited to the body of shell function fn,
// from its `fn() {` line to the next line that is only `}`; an empty fn
// means the whole script.
func rewriteLineIn(t *testing.T, path, fn, old, new string) {
	t.Helper()
	src, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(string(src), "\n")
	in := fn == ""
	n := 0
	for i, line := range lines {
		switch {
		case fn != "" && line == fn+"() {":
			in = true
		case fn != "" && line == "}":
			in = false
		case in && line == old:
			lines[i] = new
			n++
		}
	}
	if n != 1 {
		t.Fatalf("%s: %d lines equal %q in %q, want 1", path, n, old, fn)
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")), 0o755); err != nil {
		t.Fatal(err)
	}
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func mkdir(t *testing.T, path string, perm os.FileMode) string {
	t.Helper()
	if err := os.MkdirAll(path, perm); err != nil {
		t.Fatal(err)
	}
	return path
}

func writeFile(t *testing.T, path, content string, perm os.FileMode) {
	t.Helper()
	_ = os.Remove(path)
	if err := os.WriteFile(path, []byte(content), perm); err != nil {
		t.Fatal(err)
	}
}

// sleeper is a stand-in for a runner process.
type sleeper struct {
	cmd     *exec.Cmd
	pid     int
	once    sync.Once
	reaping atomic.Bool
	done    chan struct{}
}

func spawnSleeper(t *testing.T) *sleeper {
	t.Helper()
	return spawnSleeperStdin(t, nil)
}

// spawnSleeperStdin starts a sleeper with stdin as its standard input.
func spawnSleeperStdin(t *testing.T, stdin *os.File) *sleeper {
	t.Helper()
	cmd := exec.Command("/bin/sleep", "300")
	// argv[0] under the TestMain root lets its leftover check find this
	// process; the base name stays sleep for multi-call binaries.
	cmd.Args[0] = filepath.Join(os.TempDir(), "sleep")
	if stdin != nil {
		cmd.Stdin = stdin
	}
	return startStandIn(t, cmd)
}

// startStandIn starts cmd as a runner process stand-in, stopped when the
// test ends.
func startStandIn(t *testing.T, cmd *exec.Cmd) *sleeper {
	t.Helper()
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	s := &sleeper{cmd: cmd, pid: cmd.Process.Pid, done: make(chan struct{})}
	t.Cleanup(s.stop)
	return s
}

// reap starts waiting for the process. Call it only after the hook has
// exited, so the hook never signals a PID that was already reaped.
func (s *sleeper) reap() {
	s.once.Do(func() {
		s.reaping.Store(true)
		go func() {
			_ = s.cmd.Wait()
			close(s.done)
		}()
	})
}

// stop kills the process only while it is still an unreaped child of this
// test, so the PID cannot belong to another process.
func (s *sleeper) stop() {
	if s.reaping.Load() {
		select {
		case <-s.done:
			return
		case <-time.After(200 * time.Millisecond):
		}
	}
	_ = s.cmd.Process.Kill()
	s.reap()
	<-s.done
}

func (s *sleeper) killedWithin(d time.Duration) bool {
	s.reap()
	select {
	case <-s.done:
		ws, ok := s.cmd.ProcessState.Sys().(syscall.WaitStatus)
		return ok && ws.Signaled() && ws.Signal() == syscall.SIGKILL
	case <-time.After(d):
		return false
	}
}

func (s *sleeper) aliveAfter(d time.Duration) bool {
	s.reap()
	select {
	case <-s.done:
		return false
	case <-time.After(d):
		return true
	}
}

// writeProc adds a fake /proc/<pid> entry. A negative ppid omits stat.
func writeProc(t *testing.T, procDir string, pid, ppid int, comm, argv0 string) {
	t.Helper()
	dir := mkdir(t, filepath.Join(procDir, strconv.Itoa(pid)), 0o755)
	if ppid >= 0 {
		stat := fmt.Sprintf("%d (%s) S %d %d %d 0 -1 4194560 100 0 0 0\n", pid, comm, ppid, pid, pid)
		writeFile(t, filepath.Join(dir, "stat"), stat, 0o644)
	}
	writeFile(t, filepath.Join(dir, "cmdline"), argv0+"\x00--arg\x00", 0o644)
}

type request struct {
	method string
	path   string
	header http.Header
	body   []byte
	at     time.Time
}

type verdictServer struct {
	*httptest.Server
	mu   sync.Mutex
	reqs []request
	hits chan int
}

// newVerdictServer answers each request with respond(n), n starting at 1.
// It reads the whole body before respond.
func newVerdictServer(t *testing.T, respond func(n int, w http.ResponseWriter, r *http.Request)) *verdictServer {
	t.Helper()
	s := &verdictServer{hits: make(chan int, 1000)}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		at := time.Now()
		body, _ := io.ReadAll(r.Body)
		s.mu.Lock()
		s.reqs = append(s.reqs, request{
			method: r.Method, path: r.URL.Path, header: r.Header.Clone(), body: body, at: at,
		})
		n := len(s.reqs)
		s.mu.Unlock()
		select {
		case s.hits <- n:
		default:
		}
		respond(n, w, r)
	}))
	t.Cleanup(s.Close)
	return s
}

func (s *verdictServer) requests() []request {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.reqs)
}

func (s *verdictServer) waitHits(t *testing.T, n int, timeout time.Duration) {
	t.Helper()
	deadline := time.After(timeout)
	for {
		select {
		case got := <-s.hits:
			if got >= n {
				return
			}
		case <-deadline:
			t.Fatalf("server got %d requests, want %d", len(s.requests()), n)
		}
	}
}

func verdict(v string) func(int, http.ResponseWriter, *http.Request) {
	return func(_ int, w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, `{"verdict":"`+v+`"}`)
	}
}

func writeJSON(w http.ResponseWriter, code int, body string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	fmt.Fprintln(w, body)
}

// checkHeaders checks that the hook sent event reports (POST /v1/event)
// first, then only verdict queries (GET /v1/verdict), all with the token and
// runner name and nothing injected.
func checkHeaders(t *testing.T, reqs []request) {
	t.Helper()
	if len(reqs) == 0 {
		t.Fatal("no verdict requests")
	}
	queried := false
	for i, r := range reqs {
		switch {
		case r.method == http.MethodPost && r.path == "/v1/event" && !queried:
			if got := r.header.Values("X-Event-Name"); len(got) != 1 {
				t.Errorf("request %d: X-Event-Name %q, want one value", i+1, got)
			}
		case r.method == http.MethodGet && r.path == "/v1/verdict" && i > 0:
			queried = true
		default:
			t.Errorf("request %d: %s %s, want POST /v1/event first and only GET /v1/verdict after a query",
				i+1, r.method, r.path)
		}
		if got := r.header.Get("Authorization"); got != "Bearer "+testToken {
			t.Errorf("request %d: Authorization %q", i+1, got)
		}
		if got := r.header.Get("X-Runner-Name"); got != testName {
			t.Errorf("request %d: X-Runner-Name %q", i+1, got)
		}
		if got := r.header.Get("X-Injected"); got != "" {
			t.Errorf("request %d: unexpected header X-Injected %q", i+1, got)
		}
	}
}

type hookFixture struct {
	stateDir string
	procDir  string
	home     string
	script   string
}

func (f *hookFixture) writeEndpoint(t *testing.T, url string) {
	t.Helper()
	writeFile(t, filepath.Join(f.stateDir, "endpoint"), url+"\n"+testName+"\n", 0o400)
}

func (f *hookFixture) writeToken(t *testing.T) {
	t.Helper()
	writeFile(t, filepath.Join(f.stateDir, "token"), testToken+"\n", 0o400)
}

// newHookFixture prepares a hook whose parent is startPID. An empty url
// leaves the endpoint and token files unwritten.
func newHookFixture(t *testing.T, startPID int, url string, consts map[string]string) *hookFixture {
	t.Helper()
	root := t.TempDir()
	f := &hookFixture{
		stateDir: mkdir(t, filepath.Join(root, "state"), 0o700),
		procDir:  mkdir(t, filepath.Join(root, "proc"), 0o755),
		home:     mkdir(t, filepath.Join(root, "home"), 0o755),
	}
	all := map[string]string{
		"STATE_DIR":       shellQuote(f.stateDir),
		"PROC_DIR":        shellQuote(f.procDir),
		"START_PID":       strconv.Itoa(startPID),
		"POLL_INTERVAL":   pollFast,
		"CONNECT_TIMEOUT": "1",
	}
	for k, v := range consts {
		all[k] = v
	}
	f.script = writeScript(t, mkdir(t, filepath.Join(root, "bin"), 0o755), "job-started.sh", all)
	if url != "" {
		f.writeEndpoint(t, url)
		f.writeToken(t)
	}
	return f
}

func (f *hookFixture) env(extra ...string) []string {
	return append([]string{"HOME=" + f.home, "PATH=/usr/bin:/bin", "LANG=C"}, extra...)
}

type hookRun struct {
	cmd     *exec.Cmd
	gate    chan struct{}
	once    sync.Once
	done    chan struct{}
	err     error
	outPath string
	errPath string
}

// startHook runs the hook the way the runner does: bash -e <path>.
func startHook(t *testing.T, f *hookFixture, env []string) *hookRun {
	t.Helper()
	h := startHookArgv(t, env, testBash(), "-e", f.script)
	h.release()
	return h
}

// startHookArgv starts argv but does not reap it until release is called,
// so the test can signal its PID without racing PID reuse.
// Output goes to files so orphaned curl children cannot delay Wait.
func startHookArgv(t *testing.T, env []string, argv ...string) *hookRun {
	t.Helper()
	dir := t.TempDir()
	outPath, errPath := filepath.Join(dir, "stdout"), filepath.Join(dir, "stderr")
	stdout, err := os.Create(outPath)
	if err != nil {
		t.Fatal(err)
	}
	defer stdout.Close()
	stderr, err := os.Create(errPath)
	if err != nil {
		t.Fatal(err)
	}
	defer stderr.Close()
	h := startHookFiles(t, env, stdout, stderr, argv...)
	h.outPath, h.errPath = outPath, errPath
	return h
}

// startHookFiles is startHookArgv with the given stdout and stderr.
func startHookFiles(t *testing.T, env []string, stdout, stderr *os.File, argv ...string) *hookRun {
	t.Helper()
	h := &hookRun{gate: make(chan struct{}), done: make(chan struct{})}
	h.cmd = exec.Command(argv[0], argv[1:]...)
	h.cmd.Env = env
	h.cmd.Stdout = stdout
	h.cmd.Stderr = stderr
	if err := h.cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go func() {
		<-h.gate
		h.err = h.cmd.Wait()
		close(h.done)
	}()
	t.Cleanup(func() {
		h.release()
		select {
		case <-h.done:
			return
		case <-time.After(200 * time.Millisecond):
		}
		_ = h.cmd.Process.Kill()
		<-h.done
	})
	return h
}

// release lets the hook be reaped once it exits.
func (h *hookRun) release() { h.once.Do(func() { close(h.gate) }) }

func (h *hookRun) pid() int { return h.cmd.Process.Pid }

func (h *hookRun) wait(t *testing.T, timeout time.Duration) {
	t.Helper()
	select {
	case <-h.done:
	case <-time.After(timeout):
		t.Fatalf("hook still running after %v\n%s", timeout, h.output(t))
	}
}

// waitFor polls cond until it holds or timeout passes and reports whether
// it held.
func waitFor(timeout time.Duration, cond func() bool) bool {
	deadline := time.Now().Add(timeout)
	for {
		if cond() {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// waitOutput waits until the hook's output contains s while it runs.
func (h *hookRun) waitOutput(t *testing.T, s string, timeout time.Duration) {
	t.Helper()
	if !waitFor(timeout, func() bool { return strings.Contains(h.output(t), s) || !h.running() }) {
		t.Fatalf("hook output lacks %q after %v\n%s", s, timeout, h.output(t))
	}
	if !strings.Contains(h.output(t), s) {
		t.Fatalf("hook ended before printing %q\n%s", s, h.output(t))
	}
}

func (h *hookRun) running() bool {
	select {
	case <-h.done:
		return false
	default:
		return true
	}
}

func (h *hookRun) exitCode() int { return h.cmd.ProcessState.ExitCode() }

func (h *hookRun) output(t *testing.T) string {
	t.Helper()
	out, _ := os.ReadFile(h.outPath)
	errOut, _ := os.ReadFile(h.errPath)
	return "stdout:\n" + string(out) + "stderr:\n" + string(errOut)
}

// killed returns the PIDs the hook reported on stderr, in order.
func (h *hookRun) killed(t *testing.T) []int {
	t.Helper()
	data, err := os.ReadFile(h.errPath)
	if err != nil {
		t.Fatal(err)
	}
	var pids []int
	sc := bufio.NewScanner(strings.NewReader(string(data)))
	for sc.Scan() {
		rest, ok := strings.CutPrefix(sc.Text(), killPrefix)
		if !ok {
			continue
		}
		pid, err := strconv.Atoi(rest)
		if err != nil {
			t.Fatalf("bad kill line %q", sc.Text())
		}
		pids = append(pids, pid)
	}
	return pids
}

func checkNoToken(t *testing.T, h *hookRun) {
	t.Helper()
	if out := h.output(t); strings.Contains(out, testToken) {
		t.Errorf("hook output contains the token:\n%s", out)
	}
}

func TestEntrypoint(t *testing.T) {
	requireShellTools(t)
	tests := []struct {
		name       string
		stdin      string
		dockerExit int
		wantCode   int
	}{
		{name: "two lines", stdin: testJIT + "\n" + testToken + "\n", wantCode: 0},
		{name: "docker fails", stdin: testJIT + "\n" + testToken + "\n", dockerExit: 3, wantCode: 3},
		{name: "empty JIT line", stdin: "\n" + testToken + "\n", wantCode: 1},
		{name: "empty token line", stdin: testJIT + "\n\n", wantCode: 1},
		{name: "one line", stdin: testJIT + "\n", wantCode: 1},
		{name: "no input", stdin: "", wantCode: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			root := t.TempDir()
			stateDir := filepath.Join(root, "state")
			home := mkdir(t, filepath.Join(root, "home"), 0o755)
			fakeBin := mkdir(t, filepath.Join(root, "fakebin"), 0o755)
			argsLog := filepath.Join(root, "docker-args")
			envLog := filepath.Join(root, "docker-env")
			marker := filepath.Join(root, "command-ran")
			writeFile(t, filepath.Join(fakeBin, "docker"),
				"#!/bin/sh\nprintf '%s\\n' \"$@\" >"+shellQuote(argsLog)+"\nenv >"+shellQuote(envLog)+
					"\nexit "+strconv.Itoa(tt.dockerExit)+"\n", 0o755)
			script := writeScript(t, mkdir(t, filepath.Join(root, "bin"), 0o755), "entrypoint.sh",
				map[string]string{"STATE_DIR": shellQuote(stateDir)})

			umaskOut, err := exec.Command(testBash(), "-c", "umask").Output()
			if err != nil {
				t.Fatal(err)
			}
			// The command marks that it ran, so a failed entrypoint can be
			// shown not to have reached exec.
			cmd := exec.Command(testBash(), script, testBash(), "-c", `umask; env; : >"$0"`, marker)
			cmd.Stdin = strings.NewReader(tt.stdin)
			cmd.Env = []string{
				"HOME=" + home,
				"PATH=" + fakeBin + ":/usr/bin:/bin",
				"SCALER_JOBCHECK_URL=http://host.docker.internal:8765",
				"SCALER_RUNNER_NAME=" + testName,
				"BUILDKIT_HOST=tcp://" + testName + "-buildkit:1234",
			}
			out, err := cmd.Output()
			code := 0
			if err != nil {
				ee, ok := err.(*exec.ExitError)
				if !ok {
					t.Fatal(err)
				}
				code = ee.ExitCode()
			}
			if code != tt.wantCode {
				t.Fatalf("exit code %d, want %d\n%s", code, tt.wantCode, out)
			}
			if strings.Contains(string(out), testToken) {
				t.Errorf("runner environment contains the token:\n%s", out)
			}
			_, markerErr := os.Stat(marker)
			if code != 0 {
				if markerErr == nil {
					t.Error("command ran after the entrypoint failed")
				}
				if tt.dockerExit != 0 {
					return
				}
				if _, err := os.Stat(argsLog); err == nil {
					t.Error("docker was called after bad input")
				}
				if _, err := os.Stat(filepath.Join(stateDir, "token")); err == nil {
					t.Error("token file written after bad input")
				}
				return
			}

			if markerErr != nil {
				t.Errorf("command did not run: %v", markerErr)
			}
			checkMode(t, stateDir, 0o700|os.ModeDir)
			checkFile(t, filepath.Join(stateDir, "token"), testToken+"\n", 0o400)
			checkFile(t, filepath.Join(stateDir, "endpoint"),
				"http://host.docker.internal:8765\n"+testName+"\n", 0o400)

			args, err := os.ReadFile(argsLog)
			if err != nil {
				t.Fatal(err)
			}
			wantArgs := "buildx\ncreate\n--driver\nremote\n--name\nscaler\n--use\ntcp://" + testName + "-buildkit:1234\n"
			if string(args) != wantArgs {
				t.Errorf("docker args:\n%s\nwant:\n%s", args, wantArgs)
			}
			env, err := os.ReadFile(envLog)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(env), testToken) {
				t.Error("docker environment contains the token")
			}
			if strings.Contains(string(env), testJIT) {
				t.Error("docker environment contains the JIT config")
			}
			lines := strings.Split(string(out), "\n")
			if lines[0] != strings.TrimSpace(string(umaskOut)) {
				t.Errorf("runner umask %q, want %q", lines[0], strings.TrimSpace(string(umaskOut)))
			}
			if !slices.Contains(lines, "ACTIONS_RUNNER_INPUT_JITCONFIG="+testJIT) {
				t.Errorf("runner environment lacks the JIT config:\n%s", out)
			}
		})
	}
}

func checkMode(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode() != want {
		t.Errorf("%s: mode %v, want %v", path, fi.Mode(), want)
	}
}

func checkFile(t *testing.T, path, want string, perm os.FileMode) {
	t.Helper()
	checkMode(t, path, perm)
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != want {
		t.Errorf("%s: %q, want %q", path, got, want)
	}
}

func TestHookAllow(t *testing.T) {
	requireShellTools(t)
	t.Parallel()
	srv := newVerdictServer(t, verdict("allow"))
	listener := spawnSleeper(t)
	worker := spawnSleeper(t)
	f := newHookFixture(t, worker.pid, srv.URL, nil)
	writeProc(t, f.procDir, worker.pid, listener.pid, "Runner.Worker", "/home/runner/bin/Runner.Worker")
	writeProc(t, f.procDir, listener.pid, 1, "Runner.Listener", "/home/runner/bin/Runner.Listener")

	h := startHook(t, f, f.env())
	h.wait(t, 5*time.Second)

	if h.exitCode() != 0 {
		t.Fatalf("exit code %d, want 0\n%s", h.exitCode(), h.output(t))
	}
	if _, err := os.Stat(filepath.Join(f.stateDir, "token")); !os.IsNotExist(err) {
		t.Errorf("token file still present: %v", err)
	}
	if k := h.killed(t); len(k) != 0 {
		t.Errorf("killed %v on allow", k)
	}
	if !worker.aliveAfter(200*time.Millisecond) || !listener.aliveAfter(200*time.Millisecond) {
		t.Error("runner processes died on allow")
	}
	checkHeaders(t, srv.requests())
	checkNoToken(t, h)
}

func TestHookKeepsPollingUntilAllow(t *testing.T) {
	requireShellTools(t)
	t.Parallel()
	steps := []func(w http.ResponseWriter, r *http.Request){
		func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, 200, `{"verdict":"pending"}`) },
		func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, 401, `{"reason":"unknown runner"}`) },
		func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, 500, `{"verdict":"allow"}`) },
		func(w http.ResponseWriter, _ *http.Request) {
			conn, _, err := w.(http.Hijacker).Hijack()
			if err == nil {
				conn.Close()
			}
		},
		func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, 200, `not json`) },
		func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, 200, `{"verdict":"allowed"}`) },
		func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) }, // empty body
		func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, 200, `{"verdict":"deny","x":"\"verdict\":\"allow\""}`)
		},
		func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, 200, `{"verdict":"allow"}`) },
	}
	srv := newVerdictServer(t, func(n int, w http.ResponseWriter, r *http.Request) {
		steps[min(n, len(steps))-1](w, r)
	})
	worker := spawnSleeper(t)
	f := newHookFixture(t, worker.pid, srv.URL, nil)

	h := startHook(t, f, f.env())
	h.wait(t, 15*time.Second)

	if h.exitCode() != 0 {
		t.Fatalf("exit code %d, want 0\n%s", h.exitCode(), h.output(t))
	}
	if got := len(srv.requests()); got != len(steps) {
		t.Errorf("hook ended after %d requests, want %d (only the allow may end it)", got, len(steps))
	}
	if k := h.killed(t); len(k) != 0 {
		t.Errorf("killed %v", k)
	}
	if !worker.aliveAfter(200 * time.Millisecond) {
		t.Error("runner process died")
	}
	checkHeaders(t, srv.requests())
	checkNoToken(t, h)
}

// refusedAddr is a loopback port where connections are refused. Port 1 is
// outside every ephemeral range, so no listener another test opens on port
// 0 can take it, unlike a port freed by closing a listener. A socket bound
// to a port but not listening would also keep the port, but macOS drops
// the SYN instead of refusing it (curl exit 28, checked on this host).
const refusedAddr = "127.0.0.1:1"

// refusedURL returns a URL on refusedAddr. If the host drops connections to
// it (a firewall rule, for example) instead of refusing them, the test
// cannot run here and is skipped; that is the environment, not the hook.
func refusedURL(t *testing.T) string {
	t.Helper()
	conn, err := net.DialTimeout("tcp", refusedAddr, 2*time.Second)
	if err == nil {
		conn.Close()
		t.Fatalf("something listens on %s; the test needs a refused port", refusedAddr)
	}
	if !errors.Is(err, syscall.ECONNREFUSED) {
		t.Skipf("connecting to %s: %v, want connection refused; this host does not refuse it", refusedAddr, err)
	}
	return "http://" + refusedAddr
}

func TestHookUnreachableThenAllow(t *testing.T) {
	requireShellTools(t)
	t.Parallel()
	refused := refusedURL(t) // before the hook starts, as it may skip
	srv := newVerdictServer(t, verdict("allow"))
	worker := spawnSleeper(t)
	f := newHookFixture(t, worker.pid, "", nil)

	h := startHook(t, f, f.env())

	// No endpoint or token file yet. The hook logs a state once, on its
	// first poll in that state; a few more poll intervals follow so the
	// hook polls again without the files when not under load.
	h.waitOutput(t, "(endpoint or token file unreadable)", outputTimeout)
	time.Sleep(settle)
	if !h.running() {
		t.Fatalf("hook ended without endpoint files\n%s", h.output(t))
	}

	// Connection refused on every poll. curl exits 7 when the connection
	// is refused.
	f.writeEndpoint(t, refused)
	f.writeToken(t)
	h.waitOutput(t, "(http 000, verdict none, curl exit 7)", outputTimeout)
	time.Sleep(settle)
	if !h.running() {
		t.Fatalf("hook ended on connection failures\n%s", h.output(t))
	}

	f.writeEndpoint(t, srv.URL)
	h.wait(t, 5*time.Second)
	if h.exitCode() != 0 {
		t.Fatalf("exit code %d, want 0\n%s", h.exitCode(), h.output(t))
	}
	if k := h.killed(t); len(k) != 0 {
		t.Errorf("killed %v", k)
	}
	if !worker.aliveAfter(200 * time.Millisecond) {
		t.Error("runner process died")
	}
	checkHeaders(t, srv.requests())
}

func TestHookSignalKillsRunner(t *testing.T) {
	requireShellTools(t)

	type procs struct {
		start  *sleeper
		killed []*sleeper
		alive  []*sleeper
		// childOfHook, if set, gets a stat naming the hook as its parent and
		// the hook gets a matching name; both need the hook's PID.
		childOfHook *sleeper
	}
	tests := []struct {
		name string
		// setup spawns the fake runner processes before the hook starts.
		setup func(t *testing.T, procDir string) procs
	}{
		{
			name: "names and ancestors",
			setup: func(t *testing.T, procDir string) procs {
				runSh, listener, worker := spawnSleeper(t), spawnSleeper(t), spawnSleeper(t)
				otherWorker, bystander := spawnSleeper(t), spawnSleeper(t)
				writeProc(t, procDir, worker.pid, listener.pid, "Runner.Worker", "/home/runner/bin/Runner.Worker")
				// comm with spaces and ')' must not confuse the ppid parsing.
				writeProc(t, procDir, listener.pid, runSh.pid, "Runner) Lis tener", "/home/runner/bin/Runner.Listener")
				writeProc(t, procDir, otherWorker.pid, 1, "Runner.Worker", "./bin/Runner.Worker")
				writeProc(t, procDir, bystander.pid, 1, "sleep", "/bin/sleep")
				if os.Geteuid() != 0 {
					writeProc(t, procDir, 1, 0, "init", "/home/runner/bin/Runner.Listener")
				}
				return procs{
					start:       worker,
					killed:      []*sleeper{worker, listener, runSh, otherWorker},
					alive:       []*sleeper{bystander},
					childOfHook: runSh,
				}
			},
		},
		{
			name: "no name match",
			setup: func(t *testing.T, procDir string) procs {
				parent, start, bystander := spawnSleeper(t), spawnSleeper(t), spawnSleeper(t)
				writeProc(t, procDir, start.pid, parent.pid, "sleep", "/bin/sleep")
				writeProc(t, procDir, parent.pid, 1, "sleep", "/bin/sleep")
				writeProc(t, procDir, bystander.pid, 1, "sleep", "/bin/sleep")
				return procs{start: start, killed: []*sleeper{start, parent}, alive: []*sleeper{bystander}}
			},
		},
		{
			name: "missing START_PID stat",
			setup: func(t *testing.T, procDir string) procs {
				start, other := spawnSleeper(t), spawnSleeper(t)
				writeProc(t, procDir, other.pid, 1, "sleep", "/bin/sleep")
				return procs{start: start, killed: []*sleeper{start}, alive: []*sleeper{other}}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			srv := newVerdictServer(t, verdict("pending"))
			procDir := t.TempDir()
			p := tt.setup(t, procDir)
			f := newHookFixture(t, p.start.pid, srv.URL, map[string]string{"PROC_DIR": shellQuote(procDir)})

			h := startHook(t, f, f.env())
			if p.childOfHook != nil {
				// The ancestor walk must stop at the hook and the name scan
				// must skip it.
				writeProc(t, procDir, p.childOfHook.pid, h.pid(), "run.sh", "/bin/bash")
				writeProc(t, procDir, h.pid(), 1, "bash", "/x/Runner.Worker")
			}
			srv.waitHits(t, 2, 5*time.Second)
			if err := h.cmd.Process.Signal(syscall.SIGTERM); err != nil {
				t.Fatal(err)
			}
			h.wait(t, signalExitTimeout)
			if h.exitCode() == 0 {
				t.Errorf("exit code 0 after SIGTERM")
			}
			checkKills(t, h, p.start, p.killed, p.alive)
			checkNoToken(t, h)
		})
	}
}

func TestHookInterruptDuringSlowQuery(t *testing.T) {
	requireShellTools(t)
	t.Parallel()
	release := make(chan struct{})
	srv := newVerdictServer(t, func(_ int, w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(10 * time.Second):
		case <-r.Context().Done():
		case <-release:
		}
		writeJSON(w, 200, `{"verdict":"allow"}`)
	})
	t.Cleanup(func() { close(release) }) // runs before srv.Close
	listener, worker := spawnSleeper(t), spawnSleeper(t)
	f := newHookFixture(t, worker.pid, srv.URL, nil)
	writeProc(t, f.procDir, worker.pid, listener.pid, "Runner.Worker", "/home/runner/bin/Runner.Worker")
	writeProc(t, f.procDir, listener.pid, 1, "Runner.Listener", "/home/runner/bin/Runner.Listener")

	h := startHook(t, f, f.env())
	// The handler holds the request, so curl is waiting for the answer.
	srv.waitHits(t, 1, 5*time.Second)

	start := time.Now()
	if err := h.cmd.Process.Signal(syscall.SIGINT); err != nil {
		t.Fatal(err)
	}
	// PLAN Proof: INT during a 10s query → kill within 1s. Measured to the
	// first kill line on stderr, written right after kill -KILL of
	// START_PID, so the hook's later exit and this test's reaping are not
	// counted. Without the background curl the kill would wait for curl.
	if !waitFor(time.Second, func() bool { return strings.Contains(h.output(t), killPrefix) }) {
		t.Errorf("no kill within 1s of SIGINT\n%s", h.output(t))
	}
	t.Logf("SIGINT to first kill: %v", time.Since(start))
	h.wait(t, signalExitTimeout)
	checkKills(t, h, worker, []*sleeper{worker, listener}, nil)
}

// denyRunner is a fake runner for the deny tests: a Worker (the hook's
// parent) under a Listener under run.sh, and another Worker outside the
// chain. Every stand-in is a sleeper, so any signal it gets ends it.
type denyRunner struct {
	worker, listener, runSh, otherWorker *sleeper
}

func newDenyRunner(t *testing.T, procDir string) *denyRunner {
	t.Helper()
	r := &denyRunner{}
	r.listener, r.runSh, r.worker, r.otherWorker = spawnSleeper(t), spawnSleeper(t), spawnSleeper(t), spawnSleeper(t)
	writeProc(t, procDir, r.worker.pid, r.listener.pid, "Runner.Worker", "/home/runner/bin/Runner.Worker")
	writeProc(t, procDir, r.listener.pid, r.runSh.pid, "Runner.Listener", "/home/runner/bin/Runner.Listener")
	writeProc(t, procDir, r.runSh.pid, 1, "run.sh", "/bin/bash")
	writeProc(t, procDir, r.otherWorker.pid, 1, "Runner.Worker", "/home/runner/bin/Runner.Worker")
	return r
}

// A deny verdict SIGKILLs the Worker alone and ends the hook with 1 without
// querying again: the Listener, its ancestors and a Worker outside the
// chain stay alive, so the Listener can report the job failed.
func TestHookDenyKillsWorkerOnly(t *testing.T) {
	requireShellTools(t)
	t.Parallel()
	srv := newVerdictServer(t, verdict("deny"))
	procDir := t.TempDir()
	r := newDenyRunner(t, procDir)
	f := newHookFixture(t, r.worker.pid, srv.URL, map[string]string{"PROC_DIR": shellQuote(procDir)})

	h := startHook(t, f, f.env())
	h.wait(t, 5*time.Second)

	if h.exitCode() != 1 {
		t.Errorf("exit status %v, want exit 1\n%s", h.cmd.ProcessState, h.output(t))
	}
	if n := len(srv.requests()); n != 1 {
		t.Errorf("%d requests, want 1 (no query after the deny)", n)
	}
	if k := h.killed(t); !slices.Equal(k, []int{r.worker.pid}) {
		t.Errorf("kill list %v, want only the Worker %d\n%s", k, r.worker.pid, h.output(t))
	}
	if !strings.Contains(h.output(t), "scaler-job-started: job denied; killed the worker\n") {
		t.Errorf("output lacks the deny line:\n%s", h.output(t))
	}
	if !r.worker.killedWithin(time.Second) {
		t.Errorf("the Worker %d was not SIGKILLed", r.worker.pid)
	}
	for _, s := range []*sleeper{r.listener, r.runSh, r.otherWorker} {
		if !s.aliveAfter(200 * time.Millisecond) {
			t.Errorf("process %d died", s.pid)
		}
	}
	checkNoToken(t, h)
}

// A SIGTERM that arrives while the deny kills the Worker is ignored: the
// hook still kills the Worker alone and ends with 1. The kill is slowed by
// a sleep that starts after the signals are ignored.
func TestHookDenySignalDuringWorkerKill(t *testing.T) {
	requireShellTools(t)
	t.Parallel()
	srv := newVerdictServer(t, verdict("deny"))
	procDir := t.TempDir()
	r := newDenyRunner(t, procDir)
	f := newHookFixture(t, r.worker.pid, srv.URL, map[string]string{"PROC_DIR": shellQuote(procDir)})
	marker := filepath.Join(f.stateDir, "in-kill-worker")
	rewriteLineIn(t, f.script, "kill_worker", "\t\t\tkill -KILL \"$START_PID\" 2>/dev/null; then",
		"\t\t\t: >"+shellQuote(marker)+" && /bin/sleep 1 && kill -KILL \"$START_PID\" 2>/dev/null; then")

	h := startHook(t, f, f.env())
	if !waitFor(outputTimeout, func() bool { _, err := os.Stat(marker); return err == nil }) {
		t.Fatalf("the hook did not reach the Worker kill\n%s", h.output(t))
	}
	signalHook(t, h, syscall.SIGTERM)

	if h.exitCode() != 1 {
		t.Errorf("exit status %v, want exit 1\n%s", h.cmd.ProcessState, h.output(t))
	}
	if k := h.killed(t); !slices.Equal(k, []int{r.worker.pid}) {
		t.Errorf("kill list %v, want only the Worker %d\n%s", k, r.worker.pid, h.output(t))
	}
	if !strings.Contains(h.output(t), "scaler-job-started: job denied; killed the worker\n") {
		t.Errorf("output lacks the deny line:\n%s", h.output(t))
	}
	if !r.worker.killedWithin(time.Second) {
		t.Errorf("the Worker %d was not SIGKILLed", r.worker.pid)
	}
	for _, s := range []*sleeper{r.listener, r.runSh, r.otherWorker} {
		if !s.aliveAfter(200 * time.Millisecond) {
			t.Errorf("process %d died", s.pid)
		}
	}
}

// When the Worker cannot be killed, or START_PID names no process the hook
// may kill, a deny kills every runner process it can find, as on any end
// other than allow.
func TestHookDenyWorkerKillFailsKillsRunner(t *testing.T) {
	requireShellTools(t)
	tests := []struct {
		name string
		// startPID, if set, replaces START_PID; the runner processes are
		// then found only by their names.
		startPID string
		// failKill makes the kill of the Worker fail.
		failKill bool
	}{
		{name: "kill fails", failKill: true},
		{name: "START_PID not a number", startPID: "x"},
		{name: "START_PID 1", startPID: "1"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			srv := newVerdictServer(t, verdict("deny"))
			procDir := t.TempDir()
			r := newDenyRunner(t, procDir)
			consts := map[string]string{"PROC_DIR": shellQuote(procDir)}
			if tt.startPID != "" {
				consts["START_PID"] = tt.startPID
			}
			f := newHookFixture(t, r.worker.pid, srv.URL, consts)
			if tt.failKill {
				rewriteLine(t, f.script, "\t\t\tkill -KILL \"$START_PID\" 2>/dev/null; then", "\t\t\tfalse; then")
			}

			h := startHook(t, f, f.env())
			h.wait(t, 5*time.Second)
			if h.exitCode() != 1 {
				t.Errorf("exit status %v, want exit 1\n%s", h.cmd.ProcessState, h.output(t))
			}
			out := h.output(t)
			if !strings.Contains(out, "cannot kill the worker, killing the runner") ||
				strings.Contains(out, "job denied; killed the worker\n") {
				t.Errorf("output lacks the fallback or claims the Worker kill:\n%s", out)
			}
			if tt.startPID == "" {
				// The Worker goes first, then the ancestors read at start
				// and the names.
				checkKills(t, h, r.worker, []*sleeper{r.worker, r.listener, r.runSh, r.otherWorker}, nil)
				return
			}
			// Without a usable START_PID only the name scan finds targets.
			want := []*sleeper{r.worker, r.listener, r.otherWorker}
			var wantPIDs []int
			for _, s := range want {
				wantPIDs = append(wantPIDs, s.pid)
			}
			if got := h.killed(t); !sameSet(got, wantPIDs) {
				t.Errorf("kill list %v, want %v\n%s", got, wantPIDs, out)
			}
			for _, s := range want {
				if !s.killedWithin(time.Second) {
					t.Errorf("process %d was not SIGKILLed", s.pid)
				}
			}
			if !r.runSh.aliveAfter(200 * time.Millisecond) {
				t.Errorf("run.sh %d died though nothing names it", r.runSh.pid)
			}
		})
	}
}

// newRunnerHook prepares a hook whose parent is a fake Runner.Worker under a
// fake Runner.Listener.
func newRunnerHook(t *testing.T, url string, consts map[string]string) (f *hookFixture, worker, listener *sleeper) {
	t.Helper()
	listener, worker = spawnSleeper(t), spawnSleeper(t)
	f = newHookFixture(t, worker.pid, url, consts)
	writeProc(t, f.procDir, worker.pid, listener.pid, "Runner.Worker", "/home/runner/bin/Runner.Worker")
	writeProc(t, f.procDir, listener.pid, 1, "Runner.Listener", "/home/runner/bin/Runner.Listener")
	return f, worker, listener
}

func TestHookAnyEndingSignalKillsRunner(t *testing.T) {
	requireShellTools(t)
	tests := []struct {
		sig syscall.Signal
		// trapped signals end through on_signal (exit 1). An untrapped fatal
		// signal ends bash by that signal after it runs the EXIT trap
		// (checked on bash 3.2), so on_exit must kill the runner.
		trapped bool
	}{
		{syscall.SIGINT, true},
		{syscall.SIGTERM, true},
		{syscall.SIGHUP, true},
		{syscall.SIGQUIT, true},
		{syscall.SIGUSR1, true},
		{syscall.SIGUSR2, true},
		{syscall.SIGALRM, true},
		{syscall.SIGVTALRM, false},
	}
	for _, tt := range tests {
		t.Run(tt.sig.String(), func(t *testing.T) {
			t.Parallel()
			srv := newVerdictServer(t, verdict("pending"))
			f, worker, listener := newRunnerHook(t, srv.URL, nil)
			h := startHook(t, f, f.env())
			srv.waitHits(t, 2, 5*time.Second)
			signalHook(t, h, tt.sig)
			ws, _ := h.cmd.ProcessState.Sys().(syscall.WaitStatus)
			if tt.trapped && h.exitCode() != 1 {
				t.Errorf("exit status %v, want exit 1 from on_signal", h.cmd.ProcessState)
			}
			if !tt.trapped && (!ws.Signaled() || ws.Signal() != tt.sig) {
				t.Errorf("exit status %v, want death by %v", h.cmd.ProcessState, tt.sig)
			}
			checkKills(t, h, worker, []*sleeper{worker, listener}, nil)
		})
	}
}

// signalHook sends sig while the hook runs and waits for a non-zero end.
func signalHook(t *testing.T, h *hookRun, sig syscall.Signal) {
	t.Helper()
	if err := h.cmd.Process.Signal(sig); err != nil {
		t.Fatal(err)
	}
	h.wait(t, signalExitTimeout)
	if h.exitCode() == 0 {
		t.Errorf("exit code 0 after %v", sig)
	}
}

// An exit the hook does not expect (here a forced `exit 3` in place of the
// poll sleep) must still kill the runner; only the EXIT trap covers it.
func TestHookUnexpectedExitKillsRunner(t *testing.T) {
	requireShellTools(t)
	t.Parallel()
	srv := newVerdictServer(t, verdict("pending"))
	f, worker, listener := newRunnerHook(t, srv.URL, nil)
	rewriteLine(t, f.script, "\t/bin/sleep \"$POLL_INTERVAL\" &", "\texit 3")

	h := startHook(t, f, f.env())
	h.wait(t, 5*time.Second)
	if h.exitCode() != 3 {
		t.Errorf("exit code %d, want 3\n%s", h.exitCode(), h.output(t))
	}
	if n := len(srv.requests()); n != 1 {
		t.Errorf("%d requests before the forced exit, want 1", n)
	}
	checkKills(t, h, worker, []*sleeper{worker, listener}, nil)
}

// A second signal that lands while the first is being handled must never
// leave the hook ending without the kills. The hook sends INT and TERM to
// itself (builtin kill, so only the hook is signaled) at fixed points of
// kill_runner. Whether a trap can run inside another depends on the bash
// version: bash 3.2.57 runs no signal trap inside on_signal here (only the
// EXIT trap from its `exit 1`), while bash 5.2.21 runs the trap again inside
// the handler, so this checks the result at each point, not the order of
// the handler's steps. The nested cases prove ordering only on bash 5.x;
// run them with HOOK_TEST_BASH set to the image's bash.
//
// On bash 5.x, before the ignore, a nested trap re-enters kill_runner at the
// injected line, so that injection sends the pair only once. Sent on every
// entry it would recurse until bash crashes; a finite number of outside
// signals cannot cause unbounded recursion.
func TestHookSignalDuringTrap(t *testing.T) {
	requireShellTools(t)
	const (
		pair     = "kill -INT $$; kill -TERM $$; "
		pairOnce = `[ -n "${injected:-}" ] || { injected=1; ` + pair + `}; `
	)
	tests := []struct {
		name, line, inject string
		before             bool
	}{
		{"before ignoring", "\ttrap '' INT TERM HUP QUIT USR1 USR2 ALRM VTALRM PROF XCPU XFSZ SYS TRAP ABRT BUS FPE ILL SEGV", pairOnce, true},
		{"after taking the flag", "\tkilling=1", pair, false},
		{"after the first kill", "\tkill_one \"$START_PID\"", pair, false},
		// VTALRM is not trapped; by default it would end bash mid-kill.
		{"untrapped signal after the first kill", "\tkill_one \"$START_PID\"", "kill -VTALRM $$; ", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			srv := newVerdictServer(t, verdict("pending"))
			f, worker, listener := newRunnerHook(t, srv.URL, nil)
			code := strings.TrimPrefix(tt.line, "\t")
			if tt.before {
				rewriteLineIn(t, f.script, "kill_runner", tt.line, "\t"+tt.inject+code)
			} else {
				rewriteLineIn(t, f.script, "kill_runner", tt.line, tt.line+"; "+tt.inject)
			}
			h := startHook(t, f, f.env())
			srv.waitHits(t, 1, 5*time.Second)
			signalHook(t, h, syscall.SIGTERM)
			checkKills(t, h, worker, []*sleeper{worker, listener}, nil)
		})
	}
}

// INT and TERM sent from outside, one right after the other, repeatedly.
// They are 1ms apart: bash 3.2 (the macOS test shell) can spin forever when
// trapped signals arrive microseconds apart while it is in the wait builtin
// and an EXIT trap is set, before any trap runs. TestHookSignalDuringTrap
// covers the windows inside the handler.
//
// Limit: the 1ms is between the two kill calls, not between their arrival
// in bash. Under heavy load bash may not run in between, so both signals
// are pending when it does and the test can meet that bash 3.2 defect
// (a hang, then a timeout here) even though the hook is right.
func TestHookSignalPair(t *testing.T) {
	requireShellTools(t)
	pairs := [][2]syscall.Signal{
		{syscall.SIGINT, syscall.SIGTERM},
		{syscall.SIGTERM, syscall.SIGINT},
	}
	for _, pair := range pairs {
		for i := range 6 {
			t.Run(fmt.Sprintf("%v then %v/%d", pair[0], pair[1], i), func(t *testing.T) {
				t.Parallel()
				srv := newVerdictServer(t, verdict("pending"))
				f, worker, listener := newRunnerHook(t, srv.URL, nil)
				// Not reaped until released, so both signals reach the hook
				// (or its zombie) and never a reused PID.
				h := startHookArgv(t, f.env(), testBash(), "-e", f.script)
				srv.waitHits(t, 1, 5*time.Second)
				if err := h.cmd.Process.Signal(pair[0]); err != nil {
					t.Fatal(err)
				}
				time.Sleep(time.Millisecond)
				if err := h.cmd.Process.Signal(pair[1]); err != nil {
					t.Fatal(err)
				}
				h.release()
				h.wait(t, signalExitTimeout)
				if h.exitCode() == 0 {
					t.Errorf("exit code 0 after %v", pair)
				}
				checkKills(t, h, worker, []*sleeper{worker, listener}, nil)
			})
		}
	}
}

// After the Worker is killed and reaped its /proc entry is gone; the
// ancestors read at start must still be killed.
func TestHookKillsAncestorsAfterStartReaped(t *testing.T) {
	requireShellTools(t)
	t.Parallel()
	srv := newVerdictServer(t, verdict("pending"))
	grand, parent, start := spawnSleeper(t), spawnSleeper(t), spawnSleeper(t)
	f := newHookFixture(t, start.pid, srv.URL, nil)
	// No Runner names, so only the ancestor chain can reach them.
	writeProc(t, f.procDir, start.pid, parent.pid, "sleep", "/bin/sleep")
	writeProc(t, f.procDir, parent.pid, grand.pid, "sleep", "/bin/sleep")
	writeProc(t, f.procDir, grand.pid, 1, "sleep", "/bin/sleep")

	h := startHook(t, f, f.env())
	srv.waitHits(t, 1, 5*time.Second) // the chain is read before the first query
	if err := os.RemoveAll(filepath.Join(f.procDir, strconv.Itoa(start.pid))); err != nil {
		t.Fatal(err)
	}
	signalHook(t, h, syscall.SIGTERM)
	checkKills(t, h, start, []*sleeper{start, parent, grand}, nil)
}

// In production the hook's stdout and stderr are a pipe the Worker reads,
// and the Worker (START_PID) is killed first. Every later log line then goes
// to a pipe without a reader; unless the hook ignores SIGPIPE, the first
// such write ends it before the remaining kills.
func TestHookKillsAfterOutputReaderDies(t *testing.T) {
	requireShellTools(t)
	t.Parallel()
	srv := newVerdictServer(t, verdict("pending"))
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	// The Worker stand-in holds the only read end once r is closed here.
	worker := spawnSleeperStdin(t, r)
	r.Close()
	listener, runSh := spawnSleeper(t), spawnSleeper(t)
	f := newHookFixture(t, worker.pid, srv.URL, nil)
	writeProc(t, f.procDir, worker.pid, listener.pid, "Runner.Worker", "/home/runner/bin/Runner.Worker")
	writeProc(t, f.procDir, listener.pid, runSh.pid, "Runner.Listener", "/home/runner/bin/Runner.Listener")
	writeProc(t, f.procDir, runSh.pid, 1, "run.sh", "/bin/bash")
	targets := []*sleeper{worker, listener, runSh}
	// More kills after the Worker's, so some log line surely follows its death.
	for range 3 {
		s := spawnSleeper(t)
		writeProc(t, f.procDir, s.pid, 1, "Runner.Worker", "/home/runner/bin/Runner.Worker")
		targets = append(targets, s)
	}

	h := startHookFiles(t, f.env(), w, w, testBash(), "-e", f.script)
	w.Close()
	h.release()
	srv.waitHits(t, 2, 5*time.Second)
	if err := h.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	h.wait(t, signalExitTimeout)
	if h.exitCode() != 1 {
		t.Errorf("exit status %v, want exit 1 from on_signal", h.cmd.ProcessState)
	}
	for _, s := range targets {
		if !s.killedWithin(time.Second) {
			t.Errorf("process %d was not SIGKILLed", s.pid)
		}
	}
}

func checkKills(t *testing.T, h *hookRun, start *sleeper, killed, alive []*sleeper) {
	t.Helper()
	got := h.killed(t)
	if len(got) == 0 || got[0] != start.pid {
		t.Errorf("kill order %v: START_PID %d must be first", got, start.pid)
	}
	if slices.Contains(got, 1) {
		t.Errorf("kill list %v contains PID 1", got)
	}
	if slices.Contains(got, h.pid()) {
		t.Errorf("kill list %v contains the hook itself (%d)", got, h.pid())
	}
	var want []int
	for _, s := range killed {
		want = append(want, s.pid)
	}
	if !sameSet(got, want) {
		t.Errorf("kill list %v, want %v\n%s", got, want, h.output(t))
	}
	for _, s := range killed {
		if !s.killedWithin(time.Second) {
			t.Errorf("process %d was not SIGKILLed", s.pid)
		}
	}
	for _, s := range alive {
		if !s.aliveAfter(200 * time.Millisecond) {
			t.Errorf("unrelated process %d died", s.pid)
		}
	}
}

func sameSet(a, b []int) bool {
	a, b = slices.Clone(a), slices.Clone(b)
	slices.Sort(a)
	slices.Sort(b)
	return slices.Equal(a, b)
}

func TestHookIgnoresEnvironment(t *testing.T) {
	requireShellTools(t)
	t.Parallel()
	srv := newVerdictServer(t, verdict("allow"))
	decoy := newVerdictServer(t, verdict("allow"))
	worker := spawnSleeper(t)
	f := newHookFixture(t, worker.pid, srv.URL, nil)

	root := t.TempDir()
	marker := filepath.Join(root, "fake-command-ran")
	fakeBin := mkdir(t, filepath.Join(root, "fakebin"), 0o755)
	for _, name := range []string{"curl", "env", "sleep", "rm", "bash", "printf", "kill"} {
		writeFile(t, filepath.Join(fakeBin, name), "#!/bin/sh\necho "+name+" >>"+shellQuote(marker)+"\n", 0o755)
	}
	curlHome := mkdir(t, filepath.Join(root, "curlhome"), 0o755)
	curlrc := fmt.Sprintf("proxy = %q\nheader = \"X-Injected: 1\"\nurl = %q\n", decoy.URL, decoy.URL+"/curlrc")
	writeFile(t, filepath.Join(curlHome, ".curlrc"), curlrc, 0o644)

	env := []string{
		"HOME=" + curlHome,
		"CURL_HOME=" + curlHome,
		"PATH=" + fakeBin + ":/usr/bin:/bin",
		"LANG=C",
		"SCALER_JOBCHECK_URL=" + decoy.URL,
		"SCALER_RUNNER_NAME=intruder",
	}
	for _, v := range []string{"http_proxy", "HTTP_PROXY", "https_proxy", "HTTPS_PROXY", "all_proxy", "ALL_PROXY"} {
		env = append(env, v+"="+decoy.URL)
	}
	env = append(env, "NO_PROXY=", "no_proxy=")

	h := startHook(t, f, env)
	h.wait(t, 5*time.Second)

	if h.exitCode() != 0 {
		t.Fatalf("exit code %d, want 0\n%s", h.exitCode(), h.output(t))
	}
	checkHeaders(t, srv.requests())
	if n := len(decoy.requests()); n != 0 {
		t.Errorf("decoy got %d requests", n)
	}
	if ran, err := os.ReadFile(marker); err == nil {
		t.Errorf("hook ran commands from PATH: %s", ran)
	}
}

func TestHookTokenNotInProcessArgs(t *testing.T) {
	requireShellTools(t)
	ps, err := exec.LookPath("ps")
	if err != nil {
		t.Skip("ps is not available")
	}
	t.Parallel()
	var (
		mu    sync.Mutex
		snaps []string
	)
	srv := newVerdictServer(t, func(n int, w http.ResponseWriter, _ *http.Request) {
		// curl is waiting for this response, so its args are visible now.
		out, err := exec.Command(ps, "-A", "-ww", "-o", "command=").Output()
		mu.Lock()
		snaps = append(snaps, string(out))
		mu.Unlock()
		if err != nil || n < 3 {
			writeJSON(w, 200, `{"verdict":"pending"}`)
			return
		}
		writeJSON(w, 200, `{"verdict":"allow"}`)
	})
	worker := spawnSleeper(t)
	// ps runs while curl waits for the answer; under load ps can take
	// longer than the default 5s curl limit, and a snapshot taken after
	// curl gave up would not show it. A longer limit keeps curl waiting.
	f := newHookFixture(t, worker.pid, srv.URL, map[string]string{"CHILD_MAX": "60"})

	h := startHook(t, f, f.env())
	h.wait(t, 3*time.Minute)
	if h.exitCode() != 0 {
		t.Fatalf("exit code %d, want 0\n%s", h.exitCode(), h.output(t))
	}

	mu.Lock()
	defer mu.Unlock()
	if len(snaps) == 0 {
		t.Fatal("no process snapshots")
	}
	for i, s := range snaps {
		if !strings.Contains(s, "/usr/bin/curl -q --noproxy") {
			t.Errorf("snapshot %d does not show the curl process", i+1)
		}
		if strings.Contains(s, testToken) {
			t.Errorf("snapshot %d shows the token in process args", i+1)
		}
	}
	checkNoToken(t, h)
}

func TestHookRecord(t *testing.T) {
	requireShellTools(t)
	t.Parallel()
	srv := newVerdictServer(t, verdict("allow"))
	worker := spawnSleeper(t)
	f := newHookFixture(t, worker.pid, srv.URL, map[string]string{"RECORD": "1"})
	// The hook's parent is this test process; only its cmdline is faked, and
	// without a matching name or stat it can never be a kill target.
	writeProc(t, f.procDir, os.Getpid(), -1, "", "/usr/bin/fake-parent spawnclient")
	status := filepath.Join(t.TempDir(), "status")
	writeFile(t, status, "Name:\tbash\nSigPnd:\t0000000000000000\nSigBlk:\t0000000000000002\n"+
		"SigIgn:\t0000000000001000\nSigCgt:\t0000000000014a03\n", 0o644)

	// The wrapper puts the fake status at the hook's PID before exec, which
	// keeps the PID and the parent.
	wrapper := `/bin/mkdir -p "$1/$$" && /bin/cp "$2" "$1/$$/status" && exec "$4" -e "$3"`
	h := startHookArgv(t, f.env("RECORD_PROBE=value-must-not-appear"),
		testBash(), "-c", wrapper, "wrapper", f.procDir, status, f.script, testBash())
	h.release()
	h.wait(t, 5*time.Second)
	if h.exitCode() != 0 {
		t.Fatalf("exit code %d, want 0\n%s", h.exitCode(), h.output(t))
	}
	rec, err := os.ReadFile(filepath.Join(f.stateDir, "hook-record"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(rec), "parent cmdline: /usr/bin/fake-parent spawnclient --arg") {
		t.Errorf("record lacks the parent cmdline:\n%s", rec)
	}
	if !strings.Contains(string(rec), "\nsignal masks:\nSigBlk:\t0000000000000002\nSigIgn:\t0000000000001000\n") {
		t.Errorf("record lacks the SigBlk and SigIgn lines:\n%s", rec)
	}
	if strings.Contains(string(rec), "SigCgt") || strings.Contains(string(rec), "SigPnd") {
		t.Errorf("record has status lines other than SigBlk and SigIgn:\n%s", rec)
	}
	if strings.Contains(string(rec), "value-must-not-appear") || strings.Contains(string(rec), testToken) {
		t.Errorf("record contains a value:\n%s", rec)
	}
}

// writeEvent writes an event file and returns the environment entries the
// runner gives the hook for it.
func writeEvent(t *testing.T, name string, payload []byte) (path string, env []string) {
	t.Helper()
	path = filepath.Join(t.TempDir(), "event.json")
	if err := os.WriteFile(path, payload, 0o644); err != nil {
		t.Fatal(err)
	}
	return path, []string{"GITHUB_EVENT_NAME=" + name, "GITHUB_EVENT_PATH=" + path}
}

// checkEventReport checks that r is an event report with the given body and
// the one X-Event-Name value event.
func checkEventReport(t *testing.T, i int, r request, body []byte, event string) {
	t.Helper()
	if r.method != http.MethodPost || r.path != "/v1/event" {
		t.Errorf("request %d: %s %s, want POST /v1/event", i+1, r.method, r.path)
	}
	if string(r.body) != string(body) {
		t.Errorf("request %d: body of %d bytes, want %d bytes", i+1, len(r.body), len(body))
	}
	if got := r.header.Values("X-Event-Name"); !slices.Equal(got, []string{event}) {
		t.Errorf("request %d: X-Event-Name %q, want [%q]", i+1, got, event)
	}
}

// 0002 SPEC R139: the first request is POST /v1/event carrying the event
// file's bytes unchanged and the event name; an allow ends the hook.
func TestHookPostsEventThenAllow(t *testing.T) {
	requireShellTools(t)
	large := make([]byte, 3<<20) // over curl's 1 MiB Expect: 100-continue threshold
	for i := range large {
		large[i] = byte(i % 251)
	}
	tests := []struct {
		name    string
		payload []byte
	}{
		{name: "small", payload: []byte("{\"action\":\"opened\"}\n\x00\xff\r\n@-\n")},
		{name: "large", payload: large},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			srv := newVerdictServer(t, verdict("allow"))
			f, worker, listener := newRunnerHook(t, srv.URL, nil)
			_, env := writeEvent(t, "pull_request_target", tt.payload)

			h := startHook(t, f, f.env(env...))
			h.wait(t, 10*time.Second)
			if h.exitCode() != 0 {
				t.Fatalf("exit code %d, want 0\n%s", h.exitCode(), h.output(t))
			}
			reqs := srv.requests()
			if len(reqs) != 1 {
				t.Fatalf("%d requests, want 1", len(reqs))
			}
			checkEventReport(t, 0, reqs[0], tt.payload, "pull_request_target")
			checkHeaders(t, reqs)
			if _, err := os.Stat(filepath.Join(f.stateDir, "token")); !os.IsNotExist(err) {
				t.Errorf("token file still present: %v", err)
			}
			if k := h.killed(t); len(k) != 0 {
				t.Errorf("killed %v on allow", k)
			}
			if !worker.aliveAfter(200*time.Millisecond) || !listener.aliveAfter(200*time.Millisecond) {
				t.Error("runner processes died on allow")
			}
			checkNoToken(t, h)
		})
	}
}

// 0002 SPEC R140: what the hook sends after each answer to the event report.
func TestHookEventResponses(t *testing.T) {
	requireShellTools(t)
	type outcome int
	const (
		allowed outcome = iota // exit 0 after the report alone
		denied                 // the Worker killed after the report alone
		query                  // GET /v1/verdict from then on, no POST again
		report                 // POST /v1/event again after POLL_INTERVAL
	)
	answer := func(code int, body string) func(http.ResponseWriter, *http.Request) {
		return func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, code, body) }
	}
	tests := []struct {
		name   string
		first  func(http.ResponseWriter, *http.Request)
		consts map[string]string
		want   outcome
	}{
		{name: "200 allow", first: answer(200, `{"verdict":"allow"}`), want: allowed},
		{name: "200 deny", first: answer(200, `{"verdict":"deny"}`), want: denied},
		{name: "200 pending", first: answer(200, `{"verdict":"pending"}`), want: query},
		{name: "200 unparsable", first: answer(200, `not json`), want: query},
		{name: "200 unknown verdict", first: answer(200, `{"verdict":"allowed"}`), want: query},
		{name: "400", first: answer(400, `{"verdict":"allow"}`), want: query},
		{name: "404", first: answer(404, `{"reason":"not found"}`), want: query},
		{name: "405", first: answer(405, `{"reason":"method not allowed"}`), want: query},
		{name: "413 deny body", first: answer(413, `{"verdict":"deny"}`), want: query},
		{name: "500", first: answer(500, `{"verdict":"allow"}`), want: query},
		{name: "401", first: answer(401, `{"reason":"unknown runner"}`), want: report},
		{
			name: "connection closed",
			first: func(w http.ResponseWriter, _ *http.Request) {
				if conn, _, err := w.(http.Hijacker).Hijack(); err == nil {
					conn.Close()
				}
			},
			want: report,
		},
		{
			name: "timeout",
			first: func(w http.ResponseWriter, r *http.Request) {
				select {
				case <-r.Context().Done():
				case <-time.After(5 * time.Second):
				}
				writeJSON(w, 200, `{"verdict":"allow"}`)
			},
			consts: map[string]string{"CHILD_MAX": "1"},
			want:   report,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			srv := newVerdictServer(t, func(n int, w http.ResponseWriter, r *http.Request) {
				switch {
				case n == 1:
					tt.first(w, r)
				case n < 4 && r.Method == http.MethodGet:
					writeJSON(w, 200, `{"verdict":"pending"}`)
				default:
					writeJSON(w, 200, `{"verdict":"allow"}`)
				}
			})
			consts := map[string]string{}
			for k, v := range tt.consts {
				consts[k] = v
			}
			if tt.want == report {
				consts["POLL_INTERVAL"] = "2" // the production value, to time the resend
			}
			procDir := t.TempDir()
			consts["PROC_DIR"] = shellQuote(procDir)
			r := newDenyRunner(t, procDir)
			f := newHookFixture(t, r.worker.pid, srv.URL, consts)
			payload := []byte(`{"pull_request":null}`)
			_, env := writeEvent(t, "push", payload)

			h := startHook(t, f, f.env(env...))
			h.wait(t, 20*time.Second)
			reqs := srv.requests()
			checkEventReport(t, 0, reqs[0], payload, "push")
			checkHeaders(t, reqs)
			checkNoToken(t, h)

			if tt.want == denied {
				if h.exitCode() != 1 || len(reqs) != 1 {
					t.Errorf("exit code %d after %d requests, want 1 after 1\n%s", h.exitCode(), len(reqs), h.output(t))
				}
				if k := h.killed(t); !slices.Equal(k, []int{r.worker.pid}) {
					t.Errorf("kill list %v, want only the Worker %d", k, r.worker.pid)
				}
				if !r.worker.killedWithin(time.Second) {
					t.Error("the Worker was not SIGKILLed")
				}
				return
			}
			if h.exitCode() != 0 {
				t.Fatalf("exit code %d, want 0\n%s", h.exitCode(), h.output(t))
			}
			if k := h.killed(t); len(k) != 0 {
				t.Errorf("killed %v", k)
			}
			if _, err := os.Stat(filepath.Join(f.stateDir, "token")); !os.IsNotExist(err) {
				t.Errorf("token file still present: %v", err)
			}
			switch tt.want {
			case allowed:
				if len(reqs) != 1 {
					t.Errorf("%d requests, want 1", len(reqs))
				}
			case query:
				// Two pending queries, then the allowing one.
				if len(reqs) != 4 {
					t.Fatalf("%d requests, want 4\n%s", len(reqs), h.output(t))
				}
				for i, q := range reqs[1:] {
					if q.method != http.MethodGet || q.path != "/v1/verdict" {
						t.Errorf("request %d: %s %s, want GET /v1/verdict", i+2, q.method, q.path)
					}
				}
			case report:
				if len(reqs) != 2 {
					t.Fatalf("%d requests, want 2\n%s", len(reqs), h.output(t))
				}
				checkEventReport(t, 1, reqs[1], payload, "push")
				// Measured from the arrival of the first report, so it
				// includes that request's own time (at most CHILD_MAX).
				if gap := reqs[1].at.Sub(reqs[0].at); gap < 1900*time.Millisecond {
					t.Errorf("report sent again after %v, want about 2s", gap)
				}
			}
		})
	}
}

// 0002 SPEC R139: without a readable event file the report has an empty
// body. Only an absolute path is used, so neither a relative name nor "-"
// (curl's stdin, which carries the token) reaches curl.
func TestHookEventMissingFile(t *testing.T) {
	requireShellTools(t)
	dir := t.TempDir()
	unreadable := filepath.Join(dir, "unreadable.json")
	writeFile(t, unreadable, `{"secret":"must-not-be-sent"}`, 0o000)
	tests := []struct {
		name string
		env  []string
		root bool // root reads a mode-0 file, so the case cannot run as root
	}{
		{name: "unset", env: []string{"GITHUB_EVENT_NAME=push"}},
		{name: "empty", env: []string{"GITHUB_EVENT_NAME=push", "GITHUB_EVENT_PATH="}},
		{name: "missing file", env: []string{"GITHUB_EVENT_NAME=push", "GITHUB_EVENT_PATH=" + filepath.Join(dir, "none.json")}},
		{name: "unreadable file", env: []string{"GITHUB_EVENT_NAME=push", "GITHUB_EVENT_PATH=" + unreadable}, root: true},
		{name: "directory", env: []string{"GITHUB_EVENT_NAME=push", "GITHUB_EVENT_PATH=" + dir}},
		// The hook runs in this package's directory, where this file exists.
		{name: "relative path", env: []string{"GITHUB_EVENT_NAME=push", "GITHUB_EVENT_PATH=scripts_test.go"}},
		{name: "stdin", env: []string{"GITHUB_EVENT_NAME=push", "GITHUB_EVENT_PATH=-"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.root && os.Geteuid() == 0 {
				t.Skip("root can read a mode-0 file")
			}
			t.Parallel()
			srv := newVerdictServer(t, verdict("allow"))
			f, _, _ := newRunnerHook(t, srv.URL, nil)
			h := startHook(t, f, f.env(tt.env...))
			h.wait(t, 5*time.Second)
			if h.exitCode() != 0 {
				t.Fatalf("exit code %d, want 0\n%s", h.exitCode(), h.output(t))
			}
			reqs := srv.requests()
			if len(reqs) != 1 {
				t.Fatalf("%d requests, want 1", len(reqs))
			}
			checkEventReport(t, 0, reqs[0], nil, "push")
			if strings.Contains(string(reqs[0].body), testToken) {
				t.Error("the report body contains the token")
			}
		})
	}
}

// 0002 SPEC R139: an event name outside ^[a-z_]{1,64}$ is sent as an empty
// X-Event-Name, so it cannot add or change headers.
func TestHookEventNameFiltered(t *testing.T) {
	requireShellTools(t)
	tests := []struct {
		name string
		env  []string
		want string
	}{
		{name: "valid", env: []string{"GITHUB_EVENT_NAME=workflow_run"}, want: "workflow_run"},
		{name: "64 characters", env: []string{"GITHUB_EVENT_NAME=" + strings.Repeat("a", 64)}, want: strings.Repeat("a", 64)},
		{name: "65 characters", env: []string{"GITHUB_EVENT_NAME=" + strings.Repeat("a", 65)}},
		{name: "unset"},
		{name: "empty", env: []string{"GITHUB_EVENT_NAME="}},
		{name: "upper case", env: []string{"GITHUB_EVENT_NAME=Pull_request"}},
		{name: "digit", env: []string{"GITHUB_EVENT_NAME=push2"}},
		{name: "hyphen", env: []string{"GITHUB_EVENT_NAME=pull-request"}},
		{name: "space", env: []string{"GITHUB_EVENT_NAME=pull request"}},
		{name: "non-ASCII", env: []string{"GITHUB_EVENT_NAME=pushé"}},
		{name: "header injection", env: []string{"GITHUB_EVENT_NAME=push\r\nX-Injected: 1"}},
		{name: "newline", env: []string{"GITHUB_EVENT_NAME=push\nX-Injected: 1"}},
		// A UTF-8 locale must not widen the letters.
		{name: "upper case in UTF-8 locale", env: []string{"GITHUB_EVENT_NAME=PUSH", "LC_ALL=en_US.UTF-8"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			srv := newVerdictServer(t, verdict("allow"))
			f, _, _ := newRunnerHook(t, srv.URL, nil)
			h := startHook(t, f, f.env(tt.env...))
			h.wait(t, 5*time.Second)
			if h.exitCode() != 0 {
				t.Fatalf("exit code %d, want 0\n%s", h.exitCode(), h.output(t))
			}
			reqs := srv.requests()
			if len(reqs) != 1 {
				t.Fatalf("%d requests, want 1", len(reqs))
			}
			checkEventReport(t, 0, reqs[0], nil, tt.want)
			checkHeaders(t, reqs)
		})
	}
}

// 0002 SPEC R139: the hook reads GITHUB_EVENT_NAME and GITHUB_EVENT_PATH once
// at start; values set later (here before every request) are never used.
func TestHookEventIgnoresLaterEnv(t *testing.T) {
	requireShellTools(t)
	t.Parallel()
	srv := newVerdictServer(t, func(n int, w http.ResponseWriter, _ *http.Request) {
		if n == 1 {
			writeJSON(w, 401, `{"reason":"unknown runner"}`)
			return
		}
		writeJSON(w, 200, `{"verdict":"allow"}`)
	})
	f, _, _ := newRunnerHook(t, srv.URL, nil)
	payload := []byte(`{"original":true}`)
	_, env := writeEvent(t, "pull_request", payload)
	other, _ := writeEvent(t, "push", []byte(`{"later":true}`))
	rewriteLine(t, f.script, "\turl=''",
		"\turl=''; export GITHUB_EVENT_NAME=push GITHUB_EVENT_PATH="+shellQuote(other))

	h := startHook(t, f, f.env(env...))
	h.wait(t, 5*time.Second)
	if h.exitCode() != 0 {
		t.Fatalf("exit code %d, want 0\n%s", h.exitCode(), h.output(t))
	}
	reqs := srv.requests()
	if len(reqs) != 2 {
		t.Fatalf("%d requests, want 2", len(reqs))
	}
	for i, r := range reqs {
		checkEventReport(t, i, r, payload, "pull_request")
	}
}

// jqPath returns a jq for the record variant or skips the test.
func jqPath(t *testing.T) string {
	t.Helper()
	if _, err := os.Stat("/usr/bin/jq"); err == nil {
		return "/usr/bin/jq"
	}
	p, err := exec.LookPath("jq")
	if err != nil {
		t.Skip("jq is not available")
	}
	return p
}

// 0002 PLAN M3-4: the record variant writes whether the event variables are
// set, the file size and one comparison word per ID pair, and never an ID,
// a name or the event name.
func TestHookRecordComparisons(t *testing.T) {
	requireShellTools(t)
	jq := jqPath(t)
	const (
		prHeadBase = "compare pull_request.head.repo.id pull_request.base.repo.id: "
		prHeadRepo = "compare pull_request.head.repo.id repository.id: "
		prBaseRepo = "compare pull_request.base.repo.id repository.id: "
		wrHeadRepo = "compare workflow_run.head_repository.id workflow_run.repository.id: "
		wrHeadTop  = "compare workflow_run.head_repository.id repository.id: "
		wrRepoTop  = "compare workflow_run.repository.id repository.id: "
	)
	words := func(pr, wr [3]string) []string {
		return []string{
			prHeadBase + pr[0], prHeadRepo + pr[1], prBaseRepo + pr[2],
			wrHeadRepo + wr[0], wrHeadTop + wr[1], wrRepoTop + wr[2],
		}
	}
	missing := [3]string{"missing", "missing", "missing"}
	same := [3]string{"same", "same", "same"}
	tests := []struct {
		name    string
		payload string // "" leaves GITHUB_EVENT_PATH unset
		want    []string
		absent  []string
	}{
		{
			name: "same pull request",
			payload: `{"pull_request":{"head":{"repo":{"id":987654321,"full_name":"owner-x/repo-y"}},` +
				`"base":{"repo":{"id":987654321}}},"repository":{"id":987654321}}`,
			want: append(words(same, missing), "event path: set", "event file: readable"),
		},
		{
			name: "fork pull request",
			payload: `{"pull_request":{"head":{"repo":{"id":123456789,"full_name":"fork-z/repo-y"}},` +
				`"base":{"repo":{"id":987654321}}},"repository":{"id":987654321}}`,
			want: words([3]string{"different", "different", "same"}, missing),
		},
		{
			name: "deleted head repository",
			payload: `{"pull_request":{"head":{"repo":null},"base":{"repo":{"id":987654321}}},` +
				`"repository":{"id":987654321}}`,
			want: words([3]string{"null", "null", "same"}, missing),
		},
		{
			name: "same workflow run",
			payload: `{"workflow_run":{"head_repository":{"id":555666777},"repository":{"id":555666777}},` +
				`"repository":{"id":555666777}}`,
			want: words(missing, same),
		},
		{
			name:    "not JSON",
			payload: `not json 987654321`,
			want:    []string{"compare: jq failed"},
		},
		{
			name:   "no event file",
			want:   []string{"event path: unset", "event file: unreadable"},
			absent: []string{"compare ", "event file bytes"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			srv := newVerdictServer(t, verdict("allow"))
			f, _, _ := newRunnerHook(t, srv.URL, map[string]string{"RECORD": "1", "JQ": shellQuote(jq)})
			env := []string{"GITHUB_EVENT_NAME=issue_comment"}
			if tt.payload != "" {
				_, env = writeEvent(t, "issue_comment", []byte(tt.payload))
			}
			h := startHook(t, f, f.env(env...))
			h.wait(t, 5*time.Second)
			if h.exitCode() != 0 {
				t.Fatalf("exit code %d, want 0\n%s", h.exitCode(), h.output(t))
			}
			data, err := os.ReadFile(filepath.Join(f.stateDir, "hook-record"))
			if err != nil {
				t.Fatal(err)
			}
			rec := string(data)
			lines := strings.Split(rec, "\n")
			want := append([]string{"event name: set"}, tt.want...)
			if tt.payload != "" {
				want = append(want, "event file bytes: "+strconv.Itoa(len(tt.payload)))
			}
			for _, w := range want {
				if !slices.Contains(lines, w) {
					t.Errorf("record lacks line %q:\n%s", w, rec)
				}
			}
			for _, a := range tt.absent {
				if strings.Contains(rec, a) {
					t.Errorf("record contains %q:\n%s", a, rec)
				}
			}
			if !strings.Contains(rec, "\nfirst event report: http 200, time_total ") {
				t.Errorf("record lacks the first report's status and time:\n%s", rec)
			}
			for _, v := range []string{"987654321", "123456789", "555666777", "owner-x", "fork-z", "repo-y", "issue_comment", testToken} {
				if strings.Contains(rec, v) {
					t.Errorf("record contains the value %q:\n%s", v, rec)
				}
			}
		})
	}
}

// buildCommands returns the docker buildx build commands of build.sh, each
// with its backslash continuations joined into one line.
func buildCommands(t *testing.T) []string {
	t.Helper()
	data, err := os.ReadFile("build.sh")
	if err != nil {
		t.Fatal(err)
	}
	var cmds []string
	var cur []string
	for _, line := range strings.Split(string(data), "\n") {
		trimmed := strings.TrimSpace(line)
		if cur == nil && !strings.HasPrefix(trimmed, "docker buildx build") {
			continue
		}
		if strings.HasSuffix(trimmed, "\\") {
			cur = append(cur, strings.TrimSpace(strings.TrimSuffix(trimmed, "\\")))
			continue
		}
		cmds = append(cmds, strings.Join(append(cur, trimmed), " "))
		cur = nil
	}
	return cmds
}

// build.sh labels the runner image and not the --record image (0003 SPEC
// R316).
func TestBuildScriptRunnerLabels(t *testing.T) {
	var runner, record []string
	for _, c := range buildCommands(t) {
		if strings.Contains(c, "HOOK_RECORD=1") {
			record = append(record, c)
		} else {
			runner = append(runner, c)
		}
	}
	if len(runner) != 1 || len(record) != 1 {
		t.Fatalf("build.sh has %d runner and %d record build lines, want 1 each", len(runner), len(record))
	}
	if !strings.Contains(runner[0], `--tag "$RUNNER_TAG"`) {
		t.Errorf("runner build line has no RUNNER_TAG: %s", runner[0])
	}
	labels := docker.RunnerImageLabels()
	if len(labels) != 2 {
		t.Fatalf("RunnerImageLabels has %d entries, want 2", len(labels))
	}
	for k, v := range labels {
		if want := "--label " + k + "=" + v; !strings.Contains(runner[0], want+" ") {
			t.Errorf("runner build line lacks %q: %s", want, runner[0])
		}
	}
	if n := strings.Count(runner[0], "--label "); n != len(labels) {
		t.Errorf("runner build line has %d --label flags, want %d", n, len(labels))
	}
	if strings.Contains(record[0], "--label") {
		t.Errorf("record build line has a label: %s", record[0])
	}
}

// The Go constants equal the values in build.sh (0003 SPEC R286).
func TestBuildScriptConstantsMatch(t *testing.T) {
	data, err := os.ReadFile("build.sh")
	if err != nil {
		t.Fatal(err)
	}
	assigned := map[string]string{}
	for _, line := range strings.Split(string(data), "\n") {
		if name, value, ok := strings.Cut(line, "="); ok && (name == "RUNNER_TAG" || name == "BUILDKIT_IMAGE") {
			// One pair of surrounding quotes is shell syntax, not value.
			if n := len(value); n >= 2 && (value[0] == '"' || value[0] == '\'') && value[n-1] == value[0] {
				value = value[1 : n-1]
			}
			assigned[name] = value
		}
	}
	for name, want := range map[string]string{
		"RUNNER_TAG":     docker.RunnerImageTag,
		"BUILDKIT_IMAGE": docker.BuildKitImage,
	} {
		if got, ok := assigned[name]; !ok || got != want {
			t.Errorf("build.sh %s = %q (found %v), Go constant = %q", name, got, ok, want)
		}
	}
}
