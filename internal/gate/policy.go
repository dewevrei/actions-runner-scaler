// Package gate decides whether a job that started on a runner may run, and
// holds the per-runner token the job start hook uses to read that verdict.
//
// Verdict state lives in the scaler's runner table; this package only holds
// the policy and the token.
package gate

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode/utf8"
)

// mainRef is the only workflow ref allowed to run; a tag named main is
// refs/tags/main and must not pass.
const mainRef = "refs/heads/main"

// mainBranch is the only head branch a workflow run may come from.
const mainBranch = "main"

// Job holds the fields of a JobStarted message that the verdict depends on.
// All of them come from GitHub, so job code cannot change them.
type Job struct {
	// Owner is the message's OwnerName.
	Owner string
	// Repository is the message's RepositoryName, without the owner.
	Repository string
	// WorkflowRef is the message's JobWorkflowRef, expected as
	// "owner/repo/.github/workflows/file.yml@refs/heads/main".
	WorkflowRef string
	// Event is the message's EventName.
	Event string
}

// ReferencedWorkflows records how the run's referenced_workflows field
// appeared in the REST response. The zero value is ReferencedWorkflowsAbsent,
// so an unset field is denied.
type ReferencedWorkflows int

// The four shapes referenced_workflows can take. Only ReferencedWorkflowsEmpty
// passes.
const (
	ReferencedWorkflowsAbsent ReferencedWorkflows = iota
	ReferencedWorkflowsNull
	ReferencedWorkflowsEmpty
	ReferencedWorkflowsNonEmpty
)

// String returns the JSON shape the value stands for.
func (r ReferencedWorkflows) String() string {
	switch r {
	case ReferencedWorkflowsAbsent:
		return "absent"
	case ReferencedWorkflowsNull:
		return "null"
	case ReferencedWorkflowsEmpty:
		return "[]"
	case ReferencedWorkflowsNonEmpty:
		return "non-empty"
	default:
		return fmt.Sprintf("ReferencedWorkflows(%d)", int(r))
	}
}

// Run holds the fields of the workflow run (REST "Get a workflow run",
// looked up by the message's WorkflowRunID) that the verdict depends on.
// Missing numbers are zero and a missing time is the zero time; the latest
// run condition denies both.
type Run struct {
	// ID is id, the run ID.
	ID int64
	// WorkflowID is workflow_id, the ID of the run's workflow.
	WorkflowID int64
	// RunNumber is run_number, which counts the workflow's runs.
	RunNumber int64
	// CreatedAt is created_at.
	CreatedAt time.Time
	// HeadRepository is head_repository.full_name ("owner/repo").
	HeadRepository string
	// HeadBranch is head_branch.
	HeadBranch string
	// Event is event.
	Event string
	// Path is path, e.g. ".github/workflows/deploy.yml".
	Path string
	// ReferencedWorkflows is the shape of referenced_workflows.
	ReferencedWorkflows ReferencedWorkflows
}

// Policy is one pool's rule for which jobs may run.
type Policy struct {
	// Owner and Repository name the pool's repository; both are compared
	// ASCII case-insensitively, and non-ASCII names never match.
	Owner      string
	Repository string
	// AllowedWorkflows lists workflow paths such as
	// ".github/workflows/deploy.yml", compared exactly.
	AllowedWorkflows []string
	// AllowedEvents lists allowed event names, compared exactly. Empty
	// allows no event; the ["push"] default belongs to the config.
	AllowedEvents []string
}

// sameFullName reports whether fullName ("owner/repo") is the policy's
// repository. Anything but exactly two non-empty parts never matches.
func (p Policy) sameFullName(fullName string) bool {
	owner, name, ok := strings.Cut(fullName, "/")
	return ok && !strings.Contains(name, "/") && p.sameRepository(owner, name)
}

// sameRepository reports whether owner/name is the policy's repository.
// Empty parts never match.
func (p Policy) sameRepository(owner, name string) bool {
	if owner == "" || name == "" || p.Owner == "" || p.Repository == "" {
		return false
	}
	return equalFoldASCII(owner, p.Owner) && equalFoldASCII(name, p.Repository)
}

// equalFoldASCII compares ASCII case-insensitively. Any non-ASCII byte makes
// it false, because Unicode folding (strings.EqualFold) maps the Kelvin sign
// to "k" and the long s to "s", letting a look-alike name match.
func equalFoldASCII(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := 0; i < len(a); i++ {
		ca, cb := a[i], b[i]
		if ca >= utf8.RuneSelf || cb >= utf8.RuneSelf {
			return false
		}
		if lowerASCII(ca) != lowerASCII(cb) {
			return false
		}
	}
	return true
}

func lowerASCII(c byte) byte {
	if 'A' <= c && c <= 'Z' {
		return c + ('a' - 'A')
	}
	return c
}

// Condition identifies one of the conditions a job must meet (SPEC R9).
type Condition int

// The R9 conditions, in SPEC order.
const (
	JobRepository         Condition = iota + 1 // message owner/repo is the pool's repository
	WorkflowRefTarget                          // ref's repository is the pool's, its path is allowed
	WorkflowRefMain                            // ref's ref is exactly refs/heads/main
	JobEvent                                   // message event is allowed
	RunHead                                    // run's head repository is the pool's, head branch is main
	RunEvent                                   // run's event is allowed
	RunPath                                    // run's path is allowed
	WorkflowPathMatch                          // ref's path equals the run's path
	NoReferencedWorkflows                      // run's referenced_workflows is []
	LatestRun                                  // no newer main run of the run's workflow
)

// String returns a short name for logs.
func (c Condition) String() string {
	switch c {
	case JobRepository:
		return "job-repository"
	case WorkflowRefTarget:
		return "workflow-ref-target"
	case WorkflowRefMain:
		return "workflow-ref-main"
	case JobEvent:
		return "job-event"
	case RunHead:
		return "run-head"
	case RunEvent:
		return "run-event"
	case RunPath:
		return "run-path"
	case WorkflowPathMatch:
		return "workflow-path-match"
	case NoReferencedWorkflows:
		return "no-referenced-workflows"
	case LatestRun:
		return "latest-run"
	default:
		return fmt.Sprintf("Condition(%d)", int(c))
	}
}

// Violation is one condition a job failed and why.
type Violation struct {
	Condition Condition
	Reason    string
}

// RunDecision is the 0001 verdict for one job (0002 PLAN M2 renames it; M8
// deletes it). The zero value denies.
type RunDecision struct {
	Allow      bool
	Violations []Violation
}

// Reason joins the violations into one line for logs; it is empty when the
// job is allowed.
func (d RunDecision) Reason() string {
	parts := make([]string, len(d.Violations))
	for i, v := range d.Violations {
		parts[i] = v.Condition.String() + ": " + v.Reason
	}
	return strings.Join(parts, "; ")
}

// EvaluateRun checks the first nine R9 conditions, all but LatestRun, and
// allows the job only if all of them pass. The judge calls it before the
// latest run lookup, which it makes only for a job this allows. Missing or
// unparseable values fail the conditions that need them.
func EvaluateRun(job Job, run Run, policy Policy) RunDecision {
	return evaluate(newInput(job, run, Latest{}, policy), func(c Condition) bool { return c != LatestRun })
}

// Evaluate checks all ten R9 conditions and allows the job only if all pass.
// latest is the result of the latest run lookup (LatestScan.Result); its
// zero value means no lookup and fails LatestRun.
func Evaluate(job Job, run Run, latest Latest, policy Policy) RunDecision {
	return evaluate(newInput(job, run, latest, policy), func(Condition) bool { return true })
}

// evaluate runs the rules whose condition is checked and returns every
// violation.
func evaluate(in input, checked func(Condition) bool) RunDecision {
	var d RunDecision
	for _, r := range rules() {
		if !checked(r.cond) {
			continue
		}
		if err := r.check(in); err != nil {
			d.Violations = append(d.Violations, Violation{Condition: r.cond, Reason: err.Error()})
		}
	}
	d.Allow = len(d.Violations) == 0
	return d
}

// input is what every rule sees: the raw values plus the parsed workflow ref.
type input struct {
	job    Job
	run    Run
	latest Latest
	policy Policy
	ref    workflowRef
	refErr error
}

func newInput(job Job, run Run, latest Latest, policy Policy) input {
	ref, err := parseWorkflowRef(job.WorkflowRef)
	return input{job: job, run: run, latest: latest, policy: policy, ref: ref, refErr: err}
}

// rule pairs a condition with its check; check returns nil when it passes.
type rule struct {
	cond  Condition
	check func(input) error
}

// rules returns the R9 conditions. A new condition is a new entry here.
func rules() []rule {
	return []rule{
		{JobRepository, checkJobRepository},
		{WorkflowRefTarget, checkWorkflowRefTarget},
		{WorkflowRefMain, checkWorkflowRefMain},
		{JobEvent, checkJobEvent},
		{RunHead, checkRunHead},
		{RunEvent, checkRunEvent},
		{RunPath, checkRunPath},
		{WorkflowPathMatch, checkWorkflowPathMatch},
		{NoReferencedWorkflows, checkNoReferencedWorkflows},
		{LatestRun, checkLatestRun},
	}
}

func checkJobRepository(in input) error {
	if !in.policy.sameRepository(in.job.Owner, in.job.Repository) {
		return fmt.Errorf("job repository %q is not the pool's", in.job.Owner+"/"+in.job.Repository)
	}
	return nil
}

func checkWorkflowRefTarget(in input) error {
	if in.refErr != nil {
		return in.refErr
	}
	if !in.policy.sameRepository(in.ref.owner, in.ref.repository) {
		return fmt.Errorf("workflow ref repository %q is not the pool's", in.ref.owner+"/"+in.ref.repository)
	}
	return checkAllowed("workflow ref path", in.ref.path, in.policy.AllowedWorkflows)
}

func checkWorkflowRefMain(in input) error {
	if in.refErr != nil {
		return in.refErr
	}
	if in.ref.ref != mainRef {
		return fmt.Errorf("workflow ref %q is not %s", in.ref.ref, mainRef)
	}
	return nil
}

func checkJobEvent(in input) error {
	return checkAllowed("job event", in.job.Event, in.policy.AllowedEvents)
}

func checkRunHead(in input) error {
	if !in.policy.sameFullName(in.run.HeadRepository) {
		return fmt.Errorf("run head repository %q is not the pool's", in.run.HeadRepository)
	}
	if in.run.HeadBranch != mainBranch {
		return fmt.Errorf("run head branch %q is not %s", in.run.HeadBranch, mainBranch)
	}
	return nil
}

func checkRunEvent(in input) error {
	return checkAllowed("run event", in.run.Event, in.policy.AllowedEvents)
}

func checkRunPath(in input) error {
	return checkAllowed("run path", in.run.Path, in.policy.AllowedWorkflows)
}

func checkWorkflowPathMatch(in input) error {
	if in.refErr != nil {
		return in.refErr
	}
	// The message has no top-level workflow field, so requiring the job's
	// workflow to be the run's own file is what rejects reusable calls.
	// Kept although RunPath also rejects an empty path: each check must
	// stand on its own if another is changed.
	if in.run.Path == "" || in.ref.path != in.run.Path {
		return fmt.Errorf("workflow ref path %q differs from run path %q", in.ref.path, in.run.Path)
	}
	return nil
}

func checkNoReferencedWorkflows(in input) error {
	if in.run.ReferencedWorkflows != ReferencedWorkflowsEmpty {
		return fmt.Errorf("run referenced_workflows is %s, want []", in.run.ReferencedWorkflows)
	}
	return nil
}

// checkAllowed reports an error unless value is non-empty and in allowed.
// An empty allowed list allows nothing.
func checkAllowed(what, value string, allowed []string) error {
	if value == "" {
		return fmt.Errorf("%s is missing", what)
	}
	if len(allowed) == 0 { // only for a clearer reason; Contains would also fail
		return fmt.Errorf("%s %q: allowed list is empty", what, value)
	}
	if !slices.Contains(allowed, value) {
		return fmt.Errorf("%s %q is not allowed", what, value)
	}
	return nil
}

// workflowRef is a parsed JobWorkflowRef.
type workflowRef struct {
	owner      string
	repository string
	path       string
	ref        string
}

// parseWorkflowRef splits "owner/repo/path@ref". It requires exactly one
// "@": with more, no split is trustworthy, since a branch named
// "evil@refs/heads/main" would otherwise yield ref "refs/heads/main".
func parseWorkflowRef(s string) (workflowRef, error) {
	if s == "" {
		return workflowRef{}, errors.New("workflow ref is missing")
	}
	if n := strings.Count(s, "@"); n != 1 {
		return workflowRef{}, fmt.Errorf("workflow ref %q has %d @, want 1", s, n)
	}
	at := strings.IndexByte(s, '@')
	parts := strings.SplitN(s[:at], "/", 3)
	r := workflowRef{ref: s[at+1:]}
	if len(parts) == 3 {
		r.owner, r.repository, r.path = parts[0], parts[1], parts[2]
	}
	if r.owner == "" || r.repository == "" || r.path == "" || r.ref == "" {
		return workflowRef{}, fmt.Errorf("workflow ref %q is not owner/repo/path@ref", s)
	}
	return r, nil
}
