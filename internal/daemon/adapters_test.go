package daemon

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/actions/scaleset"

	"github.com/dewevrei/actions-runner-scaler/internal/docker"
	"github.com/dewevrei/actions-runner-scaler/internal/gate"
	"github.com/dewevrei/actions-runner-scaler/internal/github"
	"github.com/dewevrei/actions-runner-scaler/internal/retry"
	"github.com/dewevrei/actions-runner-scaler/internal/scaler"
)

// Every table verdict maps to the server's verdict; an unknown value is
// pending, never allow (R10).
func TestGateVerdict(t *testing.T) {
	tests := []struct {
		in   scaler.Verdict
		want gate.Verdict
	}{
		{scaler.VerdictPending, gate.VerdictPending},
		{scaler.VerdictAllow, gate.VerdictAllow},
		{scaler.VerdictDeny, gate.VerdictDeny},
		{scaler.Verdict(99), gate.VerdictPending},
		{scaler.Verdict(-1), gate.VerdictPending},
	}
	for _, tt := range tests {
		if got := gateVerdict(tt.in); got != tt.want {
			t.Errorf("gateVerdict(%v) = %v, want %v", tt.in, got, tt.want)
		}
	}
}

func TestJobCheckAuthenticate(t *testing.T) {
	tb := scaler.NewTable(scaler.TableConfig{ServerLimit: 1})
	tok, err := gate.NewTokenGenerator(nil).New()
	if err != nil {
		t.Fatal(err)
	}
	if err := tb.Add("pool-00000001", "owner", 1, testEpoch); err != nil {
		t.Fatal(err)
	}
	tb.SetToken("pool-00000001", tok)
	tb.MarkStarted("pool-00000001")
	auth := jobCheck{t: tb}

	if v, ok := auth.Authenticate("pool-00000001", "wrong", testEpoch); ok || v != gate.VerdictPending {
		t.Fatalf("wrong token = %v, %v", v, ok)
	}
	if info, _ := tb.Get("pool-00000001"); !info.BusyAt.IsZero() {
		t.Fatal("a failed check recorded busy (R40)")
	}
	if v, ok := auth.Authenticate("pool-00000002", tok.Reveal(), testEpoch); ok || v != gate.VerdictPending {
		t.Fatalf("other runner = %v, %v (R14)", v, ok)
	}
	if v, ok := auth.Authenticate("pool-00000001", tok.Reveal(), testEpoch); !ok || v != gate.VerdictPending {
		t.Fatalf("pending = %v, %v", v, ok)
	}
	tb.Allow("pool-00000001")
	if v, ok := auth.Authenticate("pool-00000001", tok.Reveal(), testEpoch); !ok || v != gate.VerdictAllow {
		t.Fatalf("allow = %v, %v", v, ok)
	}
}

func TestResourceKindConversion(t *testing.T) {
	for _, tt := range []struct {
		docker string
		kind   scaler.ResourceKind
	}{
		{docker.KindRunner, scaler.ResourceRunner},
		{docker.KindBuildKit, scaler.ResourceBuildKit},
		{docker.KindNetwork, scaler.ResourceNetwork},
	} {
		if got := resourceKind(tt.docker); got != tt.kind {
			t.Errorf("resourceKind(%q) = %q", tt.docker, got)
		}
		if got := dockerKind(tt.kind); got != tt.docker {
			t.Errorf("dockerKind(%q) = %q", tt.kind, got)
		}
	}
	if got := resourceKind(""); got != "" {
		t.Errorf("resourceKind(\"\") = %q, want unknown kept", got)
	}
	if got := resourceKind("odd"); got != "odd" {
		t.Errorf("resourceKind(odd) = %q", got)
	}
	// Unknown kinds are removed as containers.
	if got := dockerKind("odd"); got != "" {
		t.Errorf("dockerKind(odd) = %q, want empty (container)", got)
	}
}

type recordingDocker struct {
	fakeDocker
	params  []docker.Params
	stdin   []docker.Stdin
	removed [][2]string
}

func (r *recordingDocker) StartRunner(ctx context.Context, p docker.Params, in docker.Stdin) error {
	r.params = append(r.params, p)
	r.stdin = append(r.stdin, in)
	return nil
}

func (r *recordingDocker) RemoveByID(ctx context.Context, kind, id string) error {
	r.removed = append(r.removed, [2]string{kind, id})
	return nil
}

func TestDockerAdapter(t *testing.T) {
	rd := &recordingDocker{fakeDocker: *newFakeDocker(&calls{})}
	rd.resources = []docker.Resource{
		{Kind: docker.KindRunner, ID: "c1", Name: "pool-00000001", RunnerName: "pool-00000001", State: "running"},
		{Kind: "", ID: "n9", Name: "x-net"},
	}
	a := dockerAdapter{d: rd, params: docker.Params{RunnerImage: "sha256:r", BuildKitImage: "bk", JobCheckURL: "http://host.docker.internal:8765"}}

	res, err := a.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := []scaler.Resource{
		{Kind: scaler.ResourceRunner, ID: "c1", Name: "pool-00000001", RunnerName: "pool-00000001", State: "running"},
		{Kind: "", ID: "n9", Name: "x-net"},
	}
	if !slices.Equal(res, want) {
		t.Fatalf("List = %+v, want %+v", res, want)
	}

	if err := a.RemoveByID(context.Background(), scaler.ResourceNetwork, "n9"); err != nil {
		t.Fatal(err)
	}
	if err := a.RemoveByID(context.Background(), "", "c2"); err != nil {
		t.Fatal(err)
	}
	if got := rd.removed; !slices.Equal(got, [][2]string{{docker.KindNetwork, "n9"}, {"", "c2"}}) {
		t.Fatalf("RemoveByID = %v", got)
	}

	tok, _ := gate.NewTokenGenerator(nil).New()
	if err := a.StartRunner(context.Background(), "pool-00000001", scaler.NewJITConfig("jit-value"), tok); err != nil {
		t.Fatal(err)
	}
	p := rd.params[0]
	if p.Name != "pool-00000001" || p.RunnerImage != "sha256:r" || p.BuildKitImage != "bk" || p.JobCheckURL == "" {
		t.Fatalf("params = %+v", p)
	}
	if s := fmt.Sprintf("%v %+v %#v", rd.stdin[0], rd.stdin[0], rd.stdin[0]); strings.Contains(s, "jit-value") ||
		strings.Contains(s, tok.Reveal()) {
		t.Fatalf("stdin printed its secrets: %s", s)
	}
}

// fakeScaleSetClient is the part of *github.ScaleSetClient a target uses.
type fakeScaleSetClient struct {
	set     *scaleset.RunnerScaleSet
	setErr  error
	runner  *github.Runner
	jit     github.JITConfig
	sessErr error
	removed []int64
	info    scaleset.SystemInfo
	// group and setName are those of the last scale set call.
	group   int
	setName string
}

func (f *fakeScaleSetClient) GetOrCreateScaleSetInGroup(ctx context.Context, groupID int, name string) (*scaleset.RunnerScaleSet, error) {
	f.group, f.setName = groupID, name
	return f.set, f.setErr
}

func (f *fakeScaleSetClient) GetScaleSetInGroup(ctx context.Context, groupID int, name string) (*scaleset.RunnerScaleSet, error) {
	f.group, f.setName = groupID, name
	return f.set, f.setErr
}

func (f *fakeScaleSetClient) SetSystemInfo(info scaleset.SystemInfo) { f.info = info }

func (f *fakeScaleSetClient) GenerateJIT(ctx context.Context, scaleSetID int, name string) (github.JITConfig, error) {
	return f.jit, nil
}

func (f *fakeScaleSetClient) GetRunnerByName(ctx context.Context, name string) (*github.Runner, error) {
	return f.runner, nil
}

func (f *fakeScaleSetClient) RemoveRunner(ctx context.Context, id int64) error {
	f.removed = append(f.removed, id)
	return nil
}

func (f *fakeScaleSetClient) CreateSession(ctx context.Context, scaleSetID int, owner string) (*github.Session, error) {
	return nil, f.sessErr
}

type fakeREST struct {
	runners []github.Runner
	target  github.Target
	name    string
}

func (f *fakeREST) ListRunners(ctx context.Context, t github.Target) ([]github.Runner, error) {
	f.target = t
	return f.runners, nil
}

func (f *fakeREST) FindRunner(ctx context.Context, t github.Target, name string) (github.Runner, bool, error) {
	f.target, f.name = t, name
	for _, r := range f.runners {
		if r.Name == name {
			return r, true, nil
		}
	}
	return github.Runner{}, false, nil
}

func TestTargetGitHub(t *testing.T) {
	ctx := context.Background()
	ss := &fakeScaleSetClient{}
	rest := &fakeREST{runners: []github.Runner{{ID: 3, Name: "pool-00000003", Busy: true}}}
	target := github.Target{Owner: "acme", Repo: "widget"}
	g := newTargetGitHub(ss, rest, target)

	if _, found, err := g.FindScaleSet(ctx, 5, "pool"); found || err != nil {
		t.Fatalf("FindScaleSet without a set = %v, %v", found, err)
	}
	ss.set = &scaleset.RunnerScaleSet{ID: 7}
	if id, found, err := g.FindScaleSet(ctx, 5, "pool"); !found || id != 7 || err != nil || ss.group != 5 || ss.setName != "pool" {
		t.Fatalf("FindScaleSet = %d, %v, %v (group %d, name %q)", id, found, err, ss.group, ss.setName)
	}
	if id, err := g.GetOrCreateScaleSet(ctx, 6, "pool"); id != 7 || err != nil || ss.group != 6 {
		t.Fatalf("GetOrCreateScaleSet = %d, %v (group %d)", id, err, ss.group)
	}
	ss.setErr = errors.New("boom")
	if _, _, err := g.FindScaleSet(ctx, 5, "pool"); err == nil {
		t.Fatal("FindScaleSet lost the error")
	}

	if _, found, err := g.GetRunnerByName(ctx, "x"); found || err != nil {
		t.Fatalf("GetRunnerByName none = %v, %v", found, err)
	}
	ss.runner = &github.Runner{ID: 4, Name: "x", ScaleSetID: 7}
	if r, found, _ := g.GetRunnerByName(ctx, "x"); !found || r != (scaler.RegisteredRunner{ID: 4, Name: "x", ScaleSetID: 7}) {
		t.Fatalf("GetRunnerByName = %+v, %v", r, found)
	}

	ss.jit = github.JITConfig{RunnerID: 9, RunnerName: "x", Encoded: "enc"}
	jit, err := boundScaleSet{gh: g, id: 7}.GenerateJITConfig(ctx, "x")
	if err != nil || jit.ID != 9 || jit.Config.Reveal() != "enc" {
		t.Fatalf("GenerateJITConfig = %+v, %v", jit, err)
	}
	if err := (boundScaleSet{gh: g, id: 7}).RemoveRunner(ctx, 9); err != nil || !slices.Equal(ss.removed, []int64{9}) {
		t.Fatalf("RemoveRunner = %v, %v", err, ss.removed)
	}

	rs, err := g.ListRunners(ctx)
	if err != nil || len(rs) != 1 || rs[0] != (scaler.ListedRunner{ID: 3, Name: "pool-00000003", Busy: true}) {
		t.Fatalf("ListRunners = %+v, %v", rs, err)
	}
	if rest.target != target {
		t.Fatalf("listed for %+v", rest.target)
	}
	if r, found, err := g.FindRunner(ctx, "pool-00000003"); err != nil || !found || r != (scaler.ListedRunner{ID: 3, Name: "pool-00000003", Busy: true}) {
		t.Fatalf("FindRunner = %+v, %v, %v", r, found, err)
	}
	if _, found, err := g.FindRunner(ctx, "other"); found || err != nil || rest.name != "other" {
		t.Fatalf("FindRunner of an unknown name = %v, %v", found, err)
	}

	ss.sessErr = errors.New("conflict")
	if s, err := g.CreateSession(ctx, 7, "host"); s != nil || err == nil {
		t.Fatalf("CreateSession = %v, %v; want a nil Session and the error", s, err)
	}
	ss.sessErr = nil
	if s, err := g.CreateSession(ctx, 7, "host"); s != nil || err == nil {
		t.Fatalf("CreateSession without a session = %v, %v", s, err)
	}

	g.SetSystemInfo(scaleset.SystemInfo{ScaleSetID: 7})
	if ss.info.ScaleSetID != 7 {
		t.Fatal("SetSystemInfo not passed on")
	}
}

func TestWithTimeoutFollowsInjectedClock(t *testing.T) {
	clock := newFakeClock()
	ctx, cancel := withTimeout(context.Background(), clock, time.Minute)
	defer cancel()
	// A context derived before the end, as the github and docker clients do.
	child, cancelChild := context.WithCancel(context.WithValue(ctx, struct{}{}, 1))
	defer cancelChild()
	if d, ok := ctx.Deadline(); !ok || time.Until(d) > time.Minute {
		t.Fatalf("deadline %v, %v", d, ok)
	}
	clock.Advance(time.Minute - time.Nanosecond)
	if ctx.Err() != nil {
		t.Fatal("ended early")
	}
	clock.Advance(time.Nanosecond)
	for _, c := range []context.Context{ctx, child} {
		select {
		case <-c.Done():
		case <-time.After(failsafe):
			t.Fatal("not ended on the clock")
		}
		if !errors.Is(c.Err(), context.DeadlineExceeded) {
			t.Errorf("Err = %v, want DeadlineExceeded", c.Err())
		}
		if !errors.Is(context.Cause(c), context.DeadlineExceeded) {
			t.Errorf("Cause = %v, want DeadlineExceeded", context.Cause(c))
		}
		// The call's own deadline is transient at the classification site.
		err := opErr(retry.OpGetMessage, fmt.Errorf("call: %w", c.Err()))
		if got := retry.Classify(context.Background(), retry.OpGetMessage, err); got != retry.Transient {
			t.Errorf("Classify = %v, want transient", got)
		}
	}
}

// A parent that ends first gives its own error, and Classify reads the
// parent's end as Canceled.
func TestWithTimeoutParentCanceled(t *testing.T) {
	clock := newFakeClock()
	parent, cancelParent := context.WithCancel(context.Background())
	ctx, cancel := withTimeout(parent, clock, time.Minute)
	defer cancel()
	cancelParent()
	<-ctx.Done()
	if !errors.Is(ctx.Err(), context.Canceled) {
		t.Fatalf("Err = %v, want Canceled", ctx.Err())
	}
	clock.Advance(time.Minute)
	if !errors.Is(ctx.Err(), context.Canceled) {
		t.Fatalf("Err after the clock = %v, want Canceled still", ctx.Err())
	}
	if got := retry.Classify(parent, retry.OpGetMessage, ctx.Err()); got != retry.Canceled {
		t.Fatalf("Classify = %v, want canceled", got)
	}
	// A parent ended before the call: ended at once, as context.WithTimeout.
	ended, cancel3 := withTimeout(parent, clock, time.Minute)
	defer cancel3()
	select {
	case <-ended.Done():
	default:
		t.Fatal("Done not closed for an ended parent")
	}
	if !errors.Is(ended.Err(), context.Canceled) {
		t.Fatalf("Err = %v", ended.Err())
	}
	// cancel ends it as Canceled too.
	ctx2, cancel2 := withTimeout(context.Background(), clock, time.Minute)
	cancel2()
	<-ctx2.Done()
	if !errors.Is(ctx2.Err(), context.Canceled) {
		t.Fatalf("Err after cancel = %v", ctx2.Err())
	}
}

// With the real clock, Err is never non-nil while Done is open (the context
// contract), and an expiry reports DeadlineExceeded.
func TestWithTimeoutContractRealClock(t *testing.T) {
	clock := scaler.SystemClock()
	for i := range 200 {
		ctx, cancel := withTimeout(context.Background(), clock, time.Millisecond)
		deadline := time.Now().Add(failsafe)
		var err error
		for err = ctx.Err(); err == nil; err = ctx.Err() {
			if time.Now().After(deadline) {
				t.Fatal("never expired")
			}
			runtime.Gosched()
		}
		select {
		case <-ctx.Done():
		default:
			t.Fatalf("run %d: Err = %v while Done is open", i, err)
		}
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("run %d: Err = %v, want DeadlineExceeded", i, err)
		}
		cancel()
	}
}
