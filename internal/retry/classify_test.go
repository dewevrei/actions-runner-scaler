package retry

import (
	"context"
	"errors"
	"fmt"
	"net"
	"testing"
	"time"
)

// statusError stands in for converted github and docker errors.
type statusError struct {
	code int
	err  error
}

func (e statusError) Error() string   { return fmt.Sprintf("status %d: %v", e.code, e.err) }
func (e statusError) HTTPStatus() int { return e.code }
func (e statusError) Unwrap() error   { return e.err }

func status(code int) error { return statusError{code: code, err: errors.New("response")} }

// inStatus wraps err in an error carrying code, as the library does with
// its sentinels.
func inStatus(code int, err error) error { return statusError{code: code, err: err} }

// beside chains a status and a sentinel side by side, the other shape the
// library produces ("%w: %w").
func beside(code int, err error) error { return fmt.Errorf("%w: %w", status(code), err) }

func TestClassify(t *testing.T) {
	transport := &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")}
	tests := []struct {
		name string
		op   Op
		err  error
		want Class
	}{
		// Rule 2: rate limits come before any meaning of the status and
		// before the per-call deadline.
		{"rate limit carrying 404 at remove runner", OpRemoveRunner, &RateLimitError{Err: status(404)}, RateLimited},
		{"rate limit carrying auth refusal", OpRemoveRunner, &RateLimitError{Err: inStatus(403, ErrAuth)}, RateLimited},
		{"rate limit carrying call deadline", OpGetMessage, &RateLimitError{Err: inStatus(429, context.DeadlineExceeded)}, RateLimited},
		{"rate limit beside call deadline", OpAcquireJobs, fmt.Errorf("%w: %w", &RateLimitError{Err: status(403)}, context.DeadlineExceeded), RateLimited},
		{"bare 429 carrying call deadline", OpGenerateJIT, inStatus(429, context.DeadlineExceeded), RateLimited},
		{"bare 429 beside call deadline", OpGetOrCreateScaleSet, beside(429, context.DeadlineExceeded), RateLimited},

		// Rule 4: a refused App authentication is permanent before gone and
		// session lost.
		{"auth refusal 404 at remove runner", OpRemoveRunner, inStatus(404, ErrAuth), Permanent},
		{"auth refusal beside 404 at remove runner", OpRemoveRunner, beside(404, ErrAuth), Permanent},
		{"auth refusal 404 at get message", OpGetMessage, inStatus(404, ErrAuth), Permanent},
		{"auth refusal 404 at session refresh", OpSessionRefresh, inStatus(404, ErrAuth), Permanent},
		{"auth refusal 401 at jit", OpGenerateJIT, inStatus(401, ErrAuth), Permanent},
		{"auth refusal wrapped in op error", OpDeleteMessage, &OpError{Op: OpDeleteMessage, Err: inStatus(404, ErrAuth)}, Permanent},

		// Rule 4: several scale sets with one name are permanent whatever
		// status rides along, including one a call site reads as transient.
		{"ambiguous scale set", OpGetRunnerScaleSet, ErrAmbiguousScaleSet, Permanent},
		{"ambiguous scale set wrapped", OpGetOrCreateScaleSet, &OpError{Op: OpGetOrCreateScaleSet, Err: fmt.Errorf("lookup: %w", ErrAmbiguousScaleSet)}, Permanent},
		{"ambiguous scale set in 409 at session create", OpSessionCreate, inStatus(409, ErrAmbiguousScaleSet), Permanent},
		{"ambiguous scale set in 404 at get message", OpGetMessage, inStatus(404, ErrAmbiguousScaleSet), Permanent},
		{"ambiguous scale set in 503", OpGetOrCreateScaleSet, inStatus(503, ErrAmbiguousScaleSet), Permanent},
		{"rate limit before ambiguous scale set", OpGetOrCreateScaleSet, &RateLimitError{Err: ErrAmbiguousScaleSet}, RateLimited},

		// Rule 4: a target that is not set up is permanent at every
		// call site, whatever status rides along (0002 SPEC R110, R111).
		{"target setup", OpGetRunnerGroup, ErrTargetSetup, Permanent},
		{"target setup at installation", OpGetInstallation, &OpError{Op: OpGetInstallation, Err: fmt.Errorf("lookup: %w", ErrTargetSetup)}, Permanent},
		{"target setup in 503", OpGetRunnerGroup, inStatus(503, ErrTargetSetup), Permanent},
		{"target setup in 409 at session create", OpSessionCreate, inStatus(409, ErrTargetSetup), Permanent},
		{"target setup in 404 at get message", OpGetMessage, inStatus(404, ErrTargetSetup), Permanent},
		{"rate limit before target setup", OpGetRunnerGroup, &RateLimitError{Err: ErrTargetSetup}, RateLimited},

		// Rule 9 and 10 at the organization lookups (0002 SPEC R108, R109).
		{"installation 404", OpGetInstallation, status(404), Permanent},
		{"installation 401", OpGetInstallation, status(401), Permanent},
		{"installation 502", OpGetInstallation, status(502), Transient},
		{"installation 429", OpGetInstallation, status(429), RateLimited},
		{"runner group 404", OpGetRunnerGroup, status(404), Permanent},
		{"organization runners 403", OpListOrgRunners, status(403), Permanent},
		{"organization runner 404", OpFindOrgRunner, status(404), Permanent},
		{"organization runners 500", OpListOrgRunners, status(500), Transient},
		{"organization runners 429", OpFindOrgRunner, status(429), RateLimited},

		// Rule 5: gone.
		{"runner gone sentinel", OpRemoveRunner, ErrRunnerGone, Gone},
		{"runner gone in 404", OpRemoveRunner, inStatus(404, ErrRunnerGone), Gone},
		{"runner gone at another op", OpGetRunnerByName, ErrRunnerGone, Gone},
		{"remove runner plain 404", OpRemoveRunner, status(404), Gone},
		{"container remove not found", OpContainerRemove, status(404), Gone},
		{"network remove not found", OpNetworkRemove, status(404), Gone},
		{"volume remove not found", OpVolumeRemove, status(404), Gone},
		{"container inspect not found", OpContainerInspect, status(404), Permanent},
		{"get runner by name 404", OpGetRunnerByName, status(404), Permanent},

		// Rule 6: sentinels before status, in both 400 and 409 shapes.
		{"job still running in 400", OpRemoveRunner, inStatus(400, ErrJobStillRunning), Transient},
		{"job still running in 409", OpRemoveRunner, inStatus(409, ErrJobStillRunning), Transient},
		{"job still running beside 400", OpRemoveRunner, beside(400, ErrJobStillRunning), Transient},
		{"job still running beside 409", OpRemoveRunner, beside(409, ErrJobStillRunning), Transient},
		{"runner exists in 400", OpGenerateJIT, inStatus(400, ErrRunnerExists), Permanent},
		{"runner exists in 409", OpGenerateJIT, inStatus(409, ErrRunnerExists), Permanent},
		{"runner exists beside 409", OpGenerateJIT, beside(409, ErrRunnerExists), Permanent},

		// Rate limits (rule 2) and transient conflicts (rule 7).
		{"rate limit", OpGenerateJIT, &RateLimitError{}, RateLimited},
		{"rate limit carrying 403", OpListRepoRunners, &RateLimitError{Err: status(403)}, RateLimited},
		{"rate limit carrying 429", OpAcquireJobs, &RateLimitError{Err: status(429)}, RateLimited},
		{"bare 429", OpGetOrCreateScaleSet, status(429), RateLimited},
		{"bare 429 wrapped", OpGenerateJIT, &OpError{Op: OpGenerateJIT, Err: status(429)}, RateLimited},
		{"bare 429 at docker remove", OpContainerRemove, status(429), RateLimited},
		{"session create 409", OpSessionCreate, status(409), Transient},
		{"container remove 409", OpContainerRemove, status(409), Transient},
		{"network remove 403", OpNetworkRemove, status(403), Transient},
		{"volume remove 409", OpVolumeRemove, status(409), Transient},
		{"volume remove 403", OpVolumeRemove, status(403), Transient},
		{"container create 409", OpContainerCreate, status(409), Permanent},
		{"network create 403", OpNetworkCreate, status(403), Permanent},
		{"jit 409", OpGenerateJIT, status(409), Permanent},
		{"remove runner 409", OpRemoveRunner, status(409), Permanent},
		{"get or create scale set 409", OpGetOrCreateScaleSet, status(409), Permanent},

		// Rule 8: session lost.
		{"get message 404", OpGetMessage, status(404), SessionLost},
		{"delete message 404", OpDeleteMessage, status(404), SessionLost},
		{"acquire jobs 404", OpAcquireJobs, status(404), SessionLost},
		{"session refresh 404", OpSessionRefresh, status(404), SessionLost},
		{"session create 404", OpSessionCreate, status(404), Permanent},

		// Rules 9 and 10: remaining statuses.
		{"session refresh 401", OpSessionRefresh, status(401), Permanent},
		{"jit 401", OpGenerateJIT, status(401), Permanent},
		{"scale set 403 not rate limit", OpGetOrCreateScaleSet, status(403), Permanent},
		{"scale set 422", OpGetOrCreateScaleSet, status(422), Permanent},
		{"list runners 403", OpListRepoRunners, status(403), Permanent},
		{"bad request", OpSetSystemInfo, status(400), Permanent},
		{"server error", OpGetMessage, status(500), Transient},
		{"bad gateway", OpGenerateJIT, status(502), Transient},
		{"unavailable", OpRemoveRunner, status(503), Transient},
		{"decode error after 200", OpFindOrgRunner, status(200), Transient},
		{"transport error", OpAcquireJobs, transport, Transient},
		{"unknown error", OpDockerPing, errors.New("boom"), Transient},
		{"wrapped in op error", OpGetMessage, &OpError{Op: OpGetMessage, Err: status(404)}, SessionLost},

		// Rule 3: a per-call deadline with a live parent.
		{"call deadline exceeded", OpFindOrgRunner, fmt.Errorf("get: %w", context.DeadlineExceeded), Transient},
		{"call deadline beside 4xx", OpFindOrgRunner, beside(400, context.DeadlineExceeded), Transient},
		{"call deadline before auth refusal", OpRemoveRunner, inStatus(404, errors.Join(ErrAuth, context.DeadlineExceeded)), Transient},
		{"call deadline beside 404 at get message", OpGetMessage, beside(404, context.DeadlineExceeded), Transient},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Classify(context.Background(), tt.op, tt.err); got != tt.want {
				t.Fatalf("Classify(%q, %v) = %v, want %v", tt.op, tt.err, got, tt.want)
			}
		})
	}
}

func TestClassifyParentContext(t *testing.T) {
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	expired, cancelExpired := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancelExpired()

	tests := []struct {
		name string
		ctx  context.Context
		err  error
		want Class
	}{
		{"parent canceled", canceled, context.Canceled, Canceled},
		{"parent canceled with permanent error", canceled, status(422), Canceled},
		{"parent canceled with rate limit", canceled, &RateLimitError{}, Canceled},
		{"parent deadline with rate limit", expired, &RateLimitError{Err: inStatus(429, context.DeadlineExceeded)}, Canceled},
		{"parent canceled with ambiguous scale set", canceled, ErrAmbiguousScaleSet, Canceled},
		{"parent canceled with auth refusal", canceled, inStatus(404, ErrAuth), Canceled},
		{"parent deadline exceeded", expired, context.DeadlineExceeded, Canceled},
		{"only call deadline exceeded", context.Background(), context.DeadlineExceeded, Transient},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Classify(tt.ctx, OpGetMessage, tt.err); got != tt.want {
				t.Fatalf("Classify = %v, want %v", got, tt.want)
			}
		})
	}
}

// The op argument decides call-site rules even when err wraps an OpError
// for another call.
func TestClassifyPrefersOpArgument(t *testing.T) {
	tests := []struct {
		name string
		op   Op
		err  error
		want Class
	}{
		{"404 at remove runner wrapping another op", OpRemoveRunner, &OpError{Op: OpGetRunnerByName, Err: status(404)}, Gone},
		{"404 at another op wrapping remove runner", OpGetRunnerByName, &OpError{Op: OpRemoveRunner, Err: status(404)}, Permanent},
		{"409 at session create wrapping another op", OpSessionCreate, &OpError{Op: OpSetSystemInfo, Err: status(409)}, Transient},
		{"409 at another op wrapping session create", OpSetSystemInfo, &OpError{Op: OpSessionCreate, Err: status(409)}, Permanent},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Classify(context.Background(), tt.op, tt.err); got != tt.want {
				t.Fatalf("Classify(%q, %v) = %v, want %v", tt.op, tt.err, got, tt.want)
			}
		})
	}
}

func TestClassifyNil(t *testing.T) {
	if got := Classify(context.Background(), OpRemoveRunner, nil); got != 0 {
		t.Fatalf("Classify(nil) = %v, want zero Class", got)
	}
}

func TestErrorMessages(t *testing.T) {
	tests := []struct {
		err  error
		want string
	}{
		{&OpError{Op: OpRemoveRunner, Err: errors.New("boom")}, "remove runner: boom"},
		{&RateLimitError{}, "rate limited"},
		{&RateLimitError{RetryAfter: 90 * time.Second, Err: errors.New("429")}, "rate limited (retry after 1m30s): 429"},
		{&RateLimitError{Reset: time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)}, "rate limited (reset at 2026-11-24T12:00:00Z)"},
		{
			&RateLimitError{RetryAfter: time.Minute, Reset: time.Date(2026, 11, 24, 21, 0, 0, 0, time.FixedZone("KST", 9*3600)), Err: errors.New("403")},
			"rate limited (retry after 1m0s, reset at 2026-11-24T12:00:00Z): 403",
		},
	}
	for _, tt := range tests {
		if got := tt.err.Error(); got != tt.want {
			t.Errorf("Error() = %q, want %q", got, tt.want)
		}
	}
}

func TestClassString(t *testing.T) {
	for c, want := range map[Class]string{
		Canceled: "canceled", Transient: "transient", Gone: "gone",
		RateLimited: "rate limited", SessionLost: "session lost", Permanent: "permanent",
		Class(0): "Class(0)",
	} {
		if got := c.String(); got != want {
			t.Errorf("String() = %q, want %q", got, want)
		}
	}
}
