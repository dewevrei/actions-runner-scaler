package gate

import (
	"slices"
	"strings"
	"testing"
	"time"
)

const (
	deployPath      = ".github/workflows/deploy.yml"
	releasePath     = ".github/workflows/release.yml"
	goodWorkflowRef = "octo-org/deploy-tasks/" + deployPath + "@refs/heads/main"

	kelvin = "K" // folds to "k" under Unicode case folding
	longS  = "ſ" // folds to "s" under Unicode case folding
)

func testPolicy() Policy {
	return Policy{
		Owner:            "octo-org",
		Repository:       "deploy-tasks",
		AllowedWorkflows: []string{deployPath, releasePath},
		AllowedEvents:    []string{"push", "workflow_dispatch"},
	}
}

func goodJob() Job {
	return Job{
		Owner:       "octo-org",
		Repository:  "deploy-tasks",
		WorkflowRef: goodWorkflowRef,
		Event:       "workflow_dispatch",
	}
}

const (
	goodRunID      = 9001
	goodWorkflowID = 777
	goodRunNumber  = 42
)

func goodRun() Run {
	return Run{
		ID:                  goodRunID,
		WorkflowID:          goodWorkflowID,
		RunNumber:           goodRunNumber,
		CreatedAt:           time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC),
		HeadRepository:      "octo-org/deploy-tasks",
		HeadBranch:          "main",
		Event:               "workflow_dispatch",
		Path:                deployPath,
		ReferencedWorkflows: ReferencedWorkflowsEmpty,
	}
}

// allConditions returns the nine conditions EvaluateRun checks.
func allConditions() []Condition {
	return []Condition{
		JobRepository, WorkflowRefTarget, WorkflowRefMain, JobEvent, RunHead,
		RunEvent, RunPath, WorkflowPathMatch, NoReferencedWorkflows,
	}
}

func refUnparsed() []Condition {
	return []Condition{WorkflowRefTarget, WorkflowRefMain, WorkflowPathMatch}
}

func withRef(ref string) func(*Job) {
	return func(j *Job) { j.WorkflowRef = "octo-org/deploy-tasks/" + deployPath + "@" + ref }
}

// policyCase changes the good job, run and policy and lists the conditions
// that then fail.
type policyCase struct {
	name   string
	job    func(*Job)
	run    func(*Run)
	policy func(*Policy)
	want   []Condition // violated conditions; empty means allow
}

// apply returns the good inputs with the case's changes.
func (c policyCase) apply() (Job, Run, Policy) {
	job, run, policy := goodJob(), goodRun(), testPolicy()
	if c.job != nil {
		c.job(&job)
	}
	if c.run != nil {
		c.run(&run)
	}
	if c.policy != nil {
		c.policy(&policy)
	}
	return job, run, policy
}

// nineConditionCases covers the first nine R9 conditions, which EvaluateRun
// checks.
func nineConditionCases() []policyCase {
	return []policyCase{
		{name: "all correct"},

		// One case per R9 condition where only that condition is wrong.
		{name: "1 job owner differs", job: func(j *Job) { j.Owner = "other-org" }, want: []Condition{JobRepository}},
		{name: "1 job repository differs", job: func(j *Job) { j.Repository = "other" }, want: []Condition{JobRepository}},
		{name: "2 workflow ref repository differs", job: func(j *Job) { j.WorkflowRef = "octo-org/other/" + deployPath + "@refs/heads/main" }, want: []Condition{WorkflowRefTarget}},
		{name: "2 workflow ref owner differs", job: func(j *Job) { j.WorkflowRef = "other-org/deploy-tasks/" + deployPath + "@refs/heads/main" }, want: []Condition{WorkflowRefTarget}},
		{name: "3 workflow ref is another branch", job: withRef("refs/heads/dev"), want: []Condition{WorkflowRefMain}},
		{name: "3 workflow ref is tag main", job: withRef("refs/tags/main"), want: []Condition{WorkflowRefMain}},
		{name: "3 workflow ref is bare main", job: withRef("main"), want: []Condition{WorkflowRefMain}},
		{name: "3 workflow ref is refs/heads/Main", job: withRef("refs/heads/Main"), want: []Condition{WorkflowRefMain}},
		{name: "3 workflow ref is REFS/HEADS/MAIN", job: withRef("REFS/HEADS/MAIN"), want: []Condition{WorkflowRefMain}},
		{name: "3 workflow ref is refs/heads/main/x", job: withRef("refs/heads/main/x"), want: []Condition{WorkflowRefMain}},
		{name: "4 job event not allowed", job: func(j *Job) { j.Event = "pull_request_target" }, want: []Condition{JobEvent}},
		{name: "5 run head repository is a fork", run: func(r *Run) { r.HeadRepository = "fork-user/deploy-tasks" }, want: []Condition{RunHead}},
		{name: "5 run head branch not main", run: func(r *Run) { r.HeadBranch = "dev" }, want: []Condition{RunHead}},
		{name: "6 run event not allowed", run: func(r *Run) { r.Event = "workflow_run" }, want: []Condition{RunEvent}},
		// 7 cannot fail alone: 8∧2⇒7 (the run path equals the ref path,
		// which is allowed). Breaking only the run path is the smallest case.
		{name: "7 run path not allowed", run: func(r *Run) { r.Path = ".github/workflows/other.yml" }, want: []Condition{RunPath, WorkflowPathMatch}},
		// 2∧7⇏8: both paths allowed but different, so 8 fails alone.
		{name: "8 ref path and run path differ, both allowed", job: func(j *Job) { j.WorkflowRef = "octo-org/deploy-tasks/" + releasePath + "@refs/heads/main" }, want: []Condition{WorkflowPathMatch}},
		{name: "9 referenced_workflows null", run: func(r *Run) { r.ReferencedWorkflows = ReferencedWorkflowsNull }, want: []Condition{NoReferencedWorkflows}},
		{name: "9 referenced_workflows has one entry", run: func(r *Run) { r.ReferencedWorkflows = ReferencedWorkflowsNonEmpty }, want: []Condition{NoReferencedWorkflows}},
		{name: "9 referenced_workflows absent", run: func(r *Run) { r.ReferencedWorkflows = ReferencedWorkflowsAbsent }, want: []Condition{NoReferencedWorkflows}},

		// 8∧7⇒2 for the path part: the same unlisted path in both breaks 2 and 7.
		{name: "same unlisted path in ref and run", job: func(j *Job) { j.WorkflowRef = "octo-org/deploy-tasks/.github/workflows/other.yml@refs/heads/main" }, run: func(r *Run) { r.Path = ".github/workflows/other.yml" }, want: []Condition{WorkflowRefTarget, RunPath}},

		// Repository names are ASCII case-insensitive; everything else is exact.
		{
			name: "owner and repository differ only in ASCII case",
			job: func(j *Job) {
				j.Owner, j.Repository = "Octo-Org", "DEPLOY-TASKS"
				j.WorkflowRef = "OCTO-ORG/Deploy-Tasks/" + deployPath + "@refs/heads/main"
			},
			run: func(r *Run) { r.HeadRepository = "octo-ORG/deploy-TASKS" },
		},
		{name: "path differs only in case", run: func(r *Run) { r.Path = ".github/workflows/Deploy.yml" }, want: []Condition{RunPath, WorkflowPathMatch}},
		{name: "job event differs only in case", job: func(j *Job) { j.Event = "Workflow_Dispatch" }, want: []Condition{JobEvent}},
		{name: "run event differs only in case", run: func(r *Run) { r.Event = "WORKFLOW_DISPATCH" }, want: []Condition{RunEvent}},
		{name: "head branch differs only in case", run: func(r *Run) { r.HeadBranch = "Main" }, want: []Condition{RunHead}},

		// Unicode folding must not make a look-alike name match.
		{name: "job repository with long s", job: func(j *Job) { j.Repository = "deploy-ta" + longS + "ks" }, want: []Condition{JobRepository}},
		{name: "workflow ref repository with Kelvin sign", job: func(j *Job) { j.WorkflowRef = "octo-org/deploy-tas" + kelvin + "s/" + deployPath + "@refs/heads/main" }, want: []Condition{WorkflowRefTarget}},
		{name: "run head repository with Kelvin sign", run: func(r *Run) { r.HeadRepository = "octo-org/deploy-tas" + kelvin + "s" }, want: []Condition{RunHead}},

		// Missing values.
		{name: "everything missing", job: func(j *Job) { *j = Job{} }, run: func(r *Run) { *r = Run{} }, want: allConditions()},
		{name: "job owner missing", job: func(j *Job) { j.Owner = "" }, want: []Condition{JobRepository}},
		{name: "job event missing", job: func(j *Job) { j.Event = "" }, want: []Condition{JobEvent}},
		{name: "run head repository missing", run: func(r *Run) { r.HeadRepository = "" }, want: []Condition{RunHead}},
		{name: "run head branch missing", run: func(r *Run) { r.HeadBranch = "" }, want: []Condition{RunHead}},
		{name: "run event missing", run: func(r *Run) { r.Event = "" }, want: []Condition{RunEvent}},
		{name: "run path missing", run: func(r *Run) { r.Path = "" }, want: []Condition{RunPath, WorkflowPathMatch}},
		{name: "policy repository missing", policy: func(p *Policy) { p.Owner, p.Repository = "", "" }, want: []Condition{JobRepository, WorkflowRefTarget, RunHead}},
		{name: "policy workflows missing", policy: func(p *Policy) { p.AllowedWorkflows = nil }, want: []Condition{WorkflowRefTarget, RunPath}},
		{name: "policy events nil", policy: func(p *Policy) { p.AllowedEvents = nil }, want: []Condition{JobEvent, RunEvent}},
		{name: "policy events empty", policy: func(p *Policy) { p.AllowedEvents = []string{} }, want: []Condition{JobEvent, RunEvent}},

		// Unparseable values.
		{name: "workflow ref missing", job: func(j *Job) { j.WorkflowRef = "" }, want: refUnparsed()},
		{name: "workflow ref without @", job: func(j *Job) { j.WorkflowRef = "octo-org/deploy-tasks/" + deployPath }, want: refUnparsed()},
		{name: "workflow ref with branch evil@refs/heads/main", job: withRef("refs/heads/evil@refs/heads/main"), want: refUnparsed()},
		{name: "workflow ref with @ in path", job: func(j *Job) { j.WorkflowRef = "octo-org/deploy-tasks/.github/workflows/a@b.yml@refs/heads/main" }, want: refUnparsed()},
		{name: "workflow ref without path", job: func(j *Job) { j.WorkflowRef = "octo-org/deploy-tasks@refs/heads/main" }, want: refUnparsed()},
		{name: "workflow ref with empty ref", job: withRef(""), want: refUnparsed()},
		{name: "workflow ref with empty owner", job: func(j *Job) { j.WorkflowRef = "/deploy-tasks/" + deployPath + "@refs/heads/main" }, want: refUnparsed()},
		{name: "run head repository without owner", run: func(r *Run) { r.HeadRepository = "deploy-tasks" }, want: []Condition{RunHead}},
		{name: "run head repository with extra segment", run: func(r *Run) { r.HeadRepository = "octo-org/deploy-tasks/x" }, want: []Condition{RunHead}},
		{name: "unknown referenced_workflows value", run: func(r *Run) { r.ReferencedWorkflows = ReferencedWorkflows(99) }, want: []Condition{NoReferencedWorkflows}},
	}
}

func TestEvaluateRun(t *testing.T) {
	for _, tt := range nineConditionCases() {
		t.Run(tt.name, func(t *testing.T) {
			job, run, policy := tt.apply()

			d := EvaluateRun(job, run, policy)

			assertDecision(t, d, tt.want)
		})
	}
}

// EvaluateRun does not check the tenth condition: a run without the fields
// it needs and without any lookup still passes.
func TestEvaluateRunIgnoresLatestRun(t *testing.T) {
	run := goodRun()
	run.ID, run.WorkflowID, run.RunNumber, run.CreatedAt = 0, 0, 0, time.Time{}
	assertDecision(t, EvaluateRun(goodJob(), run, testPolicy()), nil)
}

// TestEvaluateEvents covers the config default ["push"] and that gate itself
// supplies no default.
func TestEvaluateRunEvents(t *testing.T) {
	tests := []struct {
		name   string
		events []string
		event  string
		want   []Condition
	}{
		{name: "push list allows push", events: []string{"push"}, event: "push"},
		{name: "push list denies workflow_dispatch", events: []string{"push"}, event: "workflow_dispatch", want: []Condition{JobEvent, RunEvent}},
		{name: "push list denies schedule", events: []string{"push"}, event: "schedule", want: []Condition{JobEvent, RunEvent}},
		{name: "nil denies push", events: nil, event: "push", want: []Condition{JobEvent, RunEvent}},
		{name: "empty denies push", events: []string{}, event: "push", want: []Condition{JobEvent, RunEvent}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			job, run, policy := goodJob(), goodRun(), testPolicy()
			policy.AllowedEvents = tt.events
			job.Event, run.Event = tt.event, tt.event

			assertDecision(t, EvaluateRun(job, run, policy), tt.want)
		})
	}
}

// An empty string in an allowed list must not let a missing value through
// (R9: missing values deny), independent of config validation.
func TestEvaluateRunEmptyAllowedEntries(t *testing.T) {
	t.Run("events", func(t *testing.T) {
		job, run, policy := goodJob(), goodRun(), testPolicy()
		policy.AllowedEvents = []string{"", "push"}
		job.Event, run.Event = "", ""
		assertDecision(t, EvaluateRun(job, run, policy), []Condition{JobEvent, RunEvent})
	})
	t.Run("workflows", func(t *testing.T) {
		job, run, policy := goodJob(), goodRun(), testPolicy()
		policy.AllowedWorkflows = []string{""}
		run.Path = ""
		assertDecision(t, EvaluateRun(job, run, policy), []Condition{WorkflowRefTarget, RunPath, WorkflowPathMatch})
	})
}

// An empty policy repository must not match an empty job or run repository.
func TestEvaluateRunEmptyRepositories(t *testing.T) {
	d := EvaluateRun(Job{}, Run{HeadRepository: "/"}, Policy{})
	if d.Allow {
		t.Fatal("empty inputs allowed")
	}
	for _, c := range []Condition{JobRepository, RunHead} {
		if !strings.Contains(d.Reason(), c.String()+": ") {
			t.Errorf("Reason() = %q, missing %v", d.Reason(), c)
		}
	}
}

func assertDecision(t *testing.T, d RunDecision, want []Condition) {
	t.Helper()
	var got []Condition
	for _, v := range d.Violations {
		got = append(got, v.Condition)
		if v.Reason == "" {
			t.Errorf("violation %v has no reason", v.Condition)
		}
	}
	if !slices.Equal(got, want) {
		t.Errorf("violations = %v, want %v (reason %q)", got, want, d.Reason())
	}
	if d.Allow != (len(want) == 0) {
		t.Errorf("Allow = %v, want %v", d.Allow, len(want) == 0)
	}
	if d.Allow && d.Reason() != "" {
		t.Errorf("allowed decision has reason %q", d.Reason())
	}
	for _, c := range want {
		if !strings.Contains(d.Reason(), c.String()+": ") {
			t.Errorf("Reason() = %q, missing %v", d.Reason(), c)
		}
	}
}

func TestZeroDecisionDenies(t *testing.T) {
	var d RunDecision
	if d.Allow {
		t.Fatal("zero RunDecision allows")
	}
}

func TestParseWorkflowRef(t *testing.T) {
	tests := []struct {
		in      string
		want    workflowRef
		wantErr bool
	}{
		{in: goodWorkflowRef, want: workflowRef{owner: "octo-org", repository: "deploy-tasks", path: deployPath, ref: "refs/heads/main"}},
		{in: "", wantErr: true},
		{in: "@", wantErr: true},
		{in: "o/r/p", wantErr: true},
		{in: "o/r@refs/heads/main", wantErr: true},
		{in: "o//p@refs/heads/main", wantErr: true},
		{in: "o/r/@refs/heads/main", wantErr: true},
		{in: "o/r/p@", wantErr: true},
		// More than one @ is never split.
		{in: "o/r/.github/workflows/a@b.yml@refs/heads/main", wantErr: true},
		{in: "o/r/.github/workflows/a.yml@refs/heads/x@y", wantErr: true},
		{in: "o/r/.github/workflows/a.yml@refs/heads/evil@refs/heads/main", wantErr: true},
		{in: "o/r/p@@refs/heads/main", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			got, err := parseWorkflowRef(tt.in)
			if (err != nil) != tt.wantErr {
				t.Fatalf("parseWorkflowRef(%q) error = %v, wantErr %v", tt.in, err, tt.wantErr)
			}
			if got != tt.want {
				t.Errorf("parseWorkflowRef(%q) = %+v, want %+v", tt.in, got, tt.want)
			}
		})
	}
}

func TestEqualFoldASCII(t *testing.T) {
	tests := []struct {
		a, b string
		want bool
	}{
		{a: "deploy-tasks", b: "DEPLOY-TASKS", want: true},
		{a: "Octo-Org_1.x", b: "octo-org_1.X", want: true},
		{a: "", b: "", want: true},
		{a: "tasks", b: "task", want: false},
		{a: "k", b: kelvin, want: false},
		{a: "s", b: longS, want: false},
		{a: "deploy-tasks", b: "deploy-ta" + longS + "ks", want: false},
		// Non-ASCII never matches, even against itself.
		{a: "café", b: "café", want: false},
		// '@' and '[' sit next to 'A' and 'Z'; they must not fold.
		{a: "@", b: "`", want: false},
		{a: "[", b: "{", want: false},
	}
	for _, tt := range tests {
		if got := equalFoldASCII(tt.a, tt.b); got != tt.want {
			t.Errorf("equalFoldASCII(%q, %q) = %v, want %v", tt.a, tt.b, got, tt.want)
		}
	}
}

func TestStringers(t *testing.T) {
	for _, c := range append(allConditions(), LatestRun) {
		if s := c.String(); s == "" || strings.HasPrefix(s, "Condition(") {
			t.Errorf("Condition %d has no name", int(c))
		}
	}
	for r, want := range map[ReferencedWorkflows]string{
		ReferencedWorkflowsAbsent:   "absent",
		ReferencedWorkflowsNull:     "null",
		ReferencedWorkflowsEmpty:    "[]",
		ReferencedWorkflowsNonEmpty: "non-empty",
	} {
		if got := r.String(); got != want {
			t.Errorf("ReferencedWorkflows(%d).String() = %q, want %q", int(r), got, want)
		}
	}
}
