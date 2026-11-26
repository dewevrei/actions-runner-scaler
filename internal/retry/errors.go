package retry

import (
	"errors"
	"strings"
	"time"
)

// StatusCoder is implemented by errors that carry the HTTP status of the
// response that caused them.
//
// The github and docker packages convert their client errors into values
// that implement it. Docker errors report the status the Engine API returned,
// so a "not found" from a container, network, or volume remove must report
// 404 for Classify to treat it as Gone.
type StatusCoder interface {
	HTTPStatus() int
}

// Op names the call site of a failed operation. Classify uses it because the
// same status means different things at different call sites (R97).
type Op string

// GitHub operations.
const (
	OpSessionCreate       Op = "create message session"
	OpSessionRefresh      Op = "refresh message session"
	OpSessionClose        Op = "close message session"
	OpGetMessage          Op = "get message"
	OpDeleteMessage       Op = "delete message"
	OpAcquireJobs         Op = "acquire jobs"
	OpGenerateJIT         Op = "generate jit runner config"
	OpRemoveRunner        Op = "remove runner"
	OpGetRunnerByName     Op = "get runner by name"
	OpGetRunnerScaleSet   Op = "get runner scale set"
	OpGetOrCreateScaleSet Op = "get or create runner scale set"
	OpSetSystemInfo       Op = "set system info"
	OpDeleteScaleSet      Op = "delete runner scale set"
	OpInstallationToken   Op = "create installation token"
	OpGetInstallation     Op = "get app installation"
	OpListRepoRunners     Op = "list repository runners"
	OpFindRepoRunner      Op = "find repository runner"
)

// Docker operations.
const (
	OpDockerPing       Op = "docker ping"
	OpDockerInfo       Op = "docker info"
	OpImageInspect     Op = "inspect image"
	OpContainerList    Op = "list containers"
	OpNetworkList      Op = "list networks"
	OpVolumeList       Op = "list volumes"
	OpNetworkCreate    Op = "create network"
	OpContainerCreate  Op = "create container"
	OpContainerStart   Op = "start container"
	OpContainerWait    Op = "wait container"
	OpContainerInspect Op = "inspect container"
	OpContainerTop     Op = "list container processes"
	OpContainerRemove  Op = "remove container"
	OpNetworkRemove    Op = "remove network"
	OpVolumeRemove     Op = "remove volume"
)

// OpError records the operation that failed together with its cause.
type OpError struct {
	Op  Op
	Err error
}

func (e *OpError) Error() string {
	if e.Err == nil {
		return string(e.Op) + ": <nil>"
	}
	return string(e.Op) + ": " + e.Err.Error()
}

// Unwrap returns the cause.
func (e *OpError) Unwrap() error { return e.Err }

// RateLimitError reports a rate-limited response (429, or a 403 that GitHub
// marks as a rate limit).
type RateLimitError struct {
	// RetryAfter is the delay from Retry-After; zero when absent or
	// unusable. A Retry-After too large for a time.Duration is given as
	// MaxRateLimitWait, not as zero, so it never shortens the wait to
	// DefaultRateLimitWait.
	RetryAfter time.Duration
	// Reset is when the limit window resets; zero when absent.
	Reset time.Time
	// Err is the underlying error, kept for its message.
	Err error
}

func (e *RateLimitError) Error() string {
	var hints []string
	if e.RetryAfter > 0 {
		hints = append(hints, "retry after "+e.RetryAfter.String())
	}
	if !e.Reset.IsZero() {
		hints = append(hints, "reset at "+e.Reset.UTC().Format(time.RFC3339))
	}
	msg := "rate limited"
	if len(hints) > 0 {
		msg += " (" + strings.Join(hints, ", ") + ")"
	}
	if e.Err != nil {
		msg += ": " + e.Err.Error()
	}
	return msg
}

// Unwrap returns the underlying error.
func (e *RateLimitError) Unwrap() error { return e.Err }

// Sentinel errors the github package maps library errors to.
var (
	// ErrRunnerGone means the runner registration no longer exists.
	ErrRunnerGone = errors.New("runner gone")
	// ErrJobStillRunning means the runner cannot be removed while it runs a job.
	ErrJobStillRunning = errors.New("job still running")
	// ErrRunnerExists means a runner with the same name is already registered.
	ErrRunnerExists = errors.New("runner exists")
	// ErrAuth means GitHub refused, with a 4xx other than a rate limit, the
	// GitHub App authentication that a call performs before its own request
	// (installation token, registration token, admin connection). The
	// status then says nothing about the call's target, so it is not read
	// as Gone or SessionLost (R51).
	ErrAuth = errors.New("GitHub App authentication refused")
	// ErrAmbiguousScaleSet means a scale set lookup by name found more than
	// one scale set with that name. The server answers it with 200, so it
	// has no error status; names must be unique within a runner group, so
	// only a person can resolve it by removing all but one (R41).
	ErrAmbiguousScaleSet = errors.New("several runner scale sets with one name")
	// ErrTargetSetup means the GitHub target is not set up for the scaler:
	// the App installation lacks a permission. Only a person can fix it
	// (0002 SPEC R238).
	ErrTargetSetup = errors.New("the GitHub target is not set up for the scaler")
)
