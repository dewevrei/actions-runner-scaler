package gate

import (
	"fmt"
	"slices"
	"testing"
	"time"
)

// summary returns a counted entry (the run's workflow, pool repository,
// main) with id and n.
func summary(id, n int64) RunSummary {
	return RunSummary{ID: id, WorkflowID: goodWorkflowID, RunNumber: n, HeadRepository: "octo-org/deploy-tasks", HeadBranch: "main"}
}

func self() RunSummary { return summary(goodRunID, goodRunNumber) }

// goodLatest is a lookup that read one page holding only the run itself.
func goodLatest() Latest {
	return Latest{Checked: true, SelfSeen: true, Read: 1, Distinct: 1, TotalCount: 1}
}

// TestEvaluate runs the first nine conditions' cases through Evaluate with
// a passing lookup, then breaks the tenth alone.
func TestEvaluate(t *testing.T) {
	for _, tt := range nineConditionCases() {
		t.Run(tt.name, func(t *testing.T) {
			job, run, policy := tt.apply()
			want := tt.want
			if run.ID == 0 { // the case cleared the whole run
				want = append(slices.Clone(want), LatestRun)
			}
			assertDecision(t, Evaluate(job, run, goodLatest(), policy), want)
		})
	}
	t.Run("10 newer main run exists", func(t *testing.T) {
		latest := goodLatest()
		newer := summary(goodRunID+1, goodRunNumber+1)
		latest.Newer = &newer
		assertDecision(t, Evaluate(goodJob(), goodRun(), latest, testPolicy()), []Condition{LatestRun})
	})
	t.Run("everything missing", func(t *testing.T) {
		assertDecision(t, Evaluate(Job{}, Run{}, Latest{}, testPolicy()), append(allConditions(), LatestRun))
	})
}

func TestEvaluateLatestRun(t *testing.T) {
	newer := summary(goodRunID+1, goodRunNumber)
	otherWorkflow := summary(goodRunID-1, 1)
	otherWorkflow.WorkflowID = goodWorkflowID + 1
	tests := []struct {
		name   string
		run    func(*Run)
		latest func(*Latest)
		deny   bool
	}{
		{name: "all correct"},
		{name: "fewer entries in total than read", latest: func(l *Latest) { l.Read, l.Distinct, l.TotalCount = 3, 3, 2 }},
		{name: "zero Latest means no lookup", latest: func(l *Latest) { *l = Latest{} }, deny: true},
		{name: "not checked", latest: func(l *Latest) { l.Checked = false }, deny: true},
		{name: "newer run", latest: func(l *Latest) { l.Newer = &newer }, deny: true},
		{name: "run not in the results", latest: func(l *Latest) { l.SelfSeen = false }, deny: true},
		{name: "total_count larger than read", latest: func(l *Latest) { l.Read, l.Distinct, l.TotalCount = 999, 999, 1000 }, deny: true},
		{name: "999 read of 999", latest: func(l *Latest) { l.Read, l.Distinct, l.TotalCount = 999, 999, 999 }},
		{name: "1,000 read of 1,000 may be cut at the cap", latest: func(l *Latest) { l.Read, l.Distinct, l.TotalCount = 1000, 1000, 1000 }, deny: true},
		{name: "1,000 read of 1,001", latest: func(l *Latest) { l.Read, l.Distinct, l.TotalCount = 1000, 1000, 1001 }, deny: true},
		{name: "more than 1,000 read", latest: func(l *Latest) { l.Read, l.Distinct, l.TotalCount = 1001, 1001, 1001 }, deny: true},
		{name: "entry of another workflow", latest: func(l *Latest) { l.OtherWorkflow = &otherWorkflow }, deny: true},
		{name: "a repeated entry hides a missing one", latest: func(l *Latest) { l.Read, l.Distinct, l.TotalCount = 3, 2, 3 }, deny: true},
		{name: "negative total_count", latest: func(l *Latest) { l.TotalCount = -1 }, deny: true},
		{name: "run id missing", run: func(r *Run) { r.ID = 0 }, deny: true},
		{name: "run id negative", run: func(r *Run) { r.ID = -1 }, deny: true},
		{name: "run workflow_id missing", run: func(r *Run) { r.WorkflowID = 0 }, deny: true},
		{name: "run run_number missing", run: func(r *Run) { r.RunNumber = 0 }, deny: true},
		{name: "run created_at missing", run: func(r *Run) { r.CreatedAt = time.Time{} }, deny: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			run, latest := goodRun(), goodLatest()
			if tt.run != nil {
				tt.run(&run)
			}
			if tt.latest != nil {
				tt.latest(&latest)
			}
			var want []Condition
			if tt.deny {
				want = []Condition{LatestRun}
			}
			assertDecision(t, Evaluate(goodJob(), run, latest, testPolicy()), want)
		})
	}
}

func TestLatestScan(t *testing.T) {
	type page struct {
		runs  []RunSummary
		total int
		stop  bool // what Add should report
	}
	with := func(s RunSummary, f func(*RunSummary)) RunSummary {
		f(&s)
		return s
	}
	// Run IDs are unique across GitHub, so each newer run gets its own.
	var nextNewer int64
	newerFrom := func(repo, branch string) RunSummary {
		nextNewer++
		return RunSummary{ID: goodRunID + 4 + nextNewer, WorkflowID: goodWorkflowID, RunNumber: goodRunNumber + 4 + nextNewer, HeadRepository: repo, HeadBranch: branch}
	}
	byID := summary(goodRunID+1, goodRunNumber-1)
	byNumber := summary(goodRunID-1, goodRunNumber+1)
	caseNewer := summary(goodRunID+2, goodRunNumber+2)
	caseNewer.HeadRepository = "Octo-Org/DEPLOY-TASKS"
	otherWorkflow := summary(goodRunID-1, 1)
	otherWorkflow.WorkflowID = goodWorkflowID + 1
	otherWorkflowFork := newerFrom("fork-user/deploy-tasks", "main")
	otherWorkflowFork.WorkflowID = 0
	// thousand returns pages of 100 older runs and the run, n entries in
	// all, each page with total_count n.
	thousand := func(n int) []page {
		var pages []page
		for i := 0; i < n; i += 100 {
			var runs []RunSummary
			for j := i; j < min(i+100, n); j++ {
				if j == n-1 {
					runs = append(runs, self())
				} else {
					runs = append(runs, summary(int64(j+1), 1))
				}
			}
			pages = append(pages, page{runs: runs, total: n})
		}
		return pages
	}
	tests := []struct {
		name  string
		pages []page
		want  Latest
		deny  bool
	}{
		{
			name:  "only the run itself",
			pages: []page{{runs: []RunSummary{self()}, total: 1}},
			want:  Latest{Checked: true, SelfSeen: true, Read: 1, TotalCount: 1},
		},
		{
			name:  "older runs and the run",
			pages: []page{{runs: []RunSummary{summary(100, 1), self(), summary(goodRunID-1, goodRunNumber-1)}, total: 3}},
			want:  Latest{Checked: true, SelfSeen: true, Read: 3, TotalCount: 3},
		},
		{
			name:  "newer by ID only",
			pages: []page{{runs: []RunSummary{self(), byID, summary(1, 1)}, total: 3, stop: true}},
			want:  Latest{Checked: true, SelfSeen: true, Newer: &byID, Read: 2, TotalCount: 3},
			deny:  true,
		},
		{
			name:  "newer by run_number only",
			pages: []page{{runs: []RunSummary{byNumber, self()}, total: 2, stop: true}},
			want:  Latest{Checked: true, Newer: &byNumber, Read: 1, TotalCount: 2},
			deny:  true,
		},
		{
			name:  "newer run in ASCII case-different repository is counted",
			pages: []page{{runs: []RunSummary{self(), caseNewer}, total: 2, stop: true}},
			want:  Latest{Checked: true, SelfSeen: true, Newer: &caseNewer, Read: 2, TotalCount: 2},
			deny:  true,
		},
		{
			name: "newer runs from forks and other branches are not counted",
			pages: []page{{runs: []RunSummary{
				newerFrom("fork-user/deploy-tasks", "main"),
				newerFrom("octo-org/deploy-tasks-fork", "main"),
				newerFrom("octo-org/deploy-tas"+kelvin+"s", "main"),
				newerFrom("octo-org/deploy-tasks/x", "main"),
				newerFrom("", "main"),
				newerFrom("octo-org/deploy-tasks", "dev"),
				newerFrom("octo-org/deploy-tasks", "Main"),
				newerFrom("octo-org/deploy-tasks", ""),
				self(),
			}, total: 9}},
			want: Latest{Checked: true, SelfSeen: true, Read: 9, TotalCount: 9},
		},
		{
			name:  "run not in the results",
			pages: []page{{runs: []RunSummary{summary(goodRunID-1, goodRunNumber-1)}, total: 1}},
			want:  Latest{Checked: true, Read: 1, TotalCount: 1},
			deny:  true,
		},
		{
			name:  "run only as a fork entry",
			pages: []page{{runs: []RunSummary{with(self(), func(s *RunSummary) { s.HeadRepository = "fork-user/deploy-tasks" })}, total: 1}},
			want:  Latest{Checked: true, Read: 1, TotalCount: 1},
			deny:  true,
		},
		{
			name:  "empty result",
			pages: []page{{total: 0}},
			want:  Latest{Checked: true},
			deny:  true,
		},
		{
			name:  "total_count larger than read",
			pages: []page{{runs: []RunSummary{self()}, total: 1001}},
			want:  Latest{Checked: true, SelfSeen: true, Read: 1, TotalCount: 1001},
			deny:  true,
		},
		{
			name: "newer run on the second page",
			pages: []page{
				{runs: []RunSummary{self(), summary(1, 1)}, total: 4},
				{runs: []RunSummary{summary(2, 2), byNumber}, total: 4, stop: true},
			},
			want: Latest{Checked: true, SelfSeen: true, Newer: &byNumber, Read: 4, TotalCount: 4},
			deny: true,
		},
		{
			name: "largest total_count when it grows",
			pages: []page{
				{runs: []RunSummary{summary(1, 1)}, total: 2},
				{runs: []RunSummary{self(), summary(2, 2)}, total: 3},
			},
			want: Latest{Checked: true, SelfSeen: true, Read: 3, TotalCount: 3},
		},
		{
			// A run deleted between pages shifts the offset, so one entry
			// is skipped and the second page's total_count shrinks to what
			// was read. The first page's larger value still denies.
			name: "largest total_count when it shrinks",
			pages: []page{
				{runs: []RunSummary{summary(1, 1), summary(2, 2)}, total: 4},
				{runs: []RunSummary{self()}, total: 3},
			},
			want: Latest{Checked: true, SelfSeen: true, Read: 3, TotalCount: 4},
			deny: true,
		},
		{
			name:  "entry of another workflow",
			pages: []page{{runs: []RunSummary{self(), otherWorkflow, summary(goodRunID+1, goodRunNumber+1)}, total: 3, stop: true}},
			want:  Latest{Checked: true, SelfSeen: true, OtherWorkflow: &otherWorkflow, Read: 2, TotalCount: 3},
			deny:  true,
		},
		{
			// workflow_id is compared before the head filter.
			name:  "fork entry of another workflow",
			pages: []page{{runs: []RunSummary{self(), otherWorkflowFork}, total: 2, stop: true}},
			want:  Latest{Checked: true, SelfSeen: true, OtherWorkflow: &otherWorkflowFork, Read: 2, TotalCount: 2},
			deny:  true,
		},
		{
			name: "pages after another workflow are ignored",
			pages: []page{
				{runs: []RunSummary{otherWorkflow}, total: 2, stop: true},
				{runs: []RunSummary{self()}, total: 9, stop: true},
			},
			want: Latest{Checked: true, OtherWorkflow: &otherWorkflow, Read: 1, TotalCount: 2},
			deny: true,
		},
		{
			name:  "1,000 entries equal to total_count",
			pages: thousand(1000),
			want:  Latest{Checked: true, SelfSeen: true, Read: 1000, TotalCount: 1000},
			deny:  true,
		},
		{
			name:  "999 entries equal to total_count",
			pages: thousand(999),
			want:  Latest{Checked: true, SelfSeen: true, Read: 999, TotalCount: 999},
		},
		{
			// The order moved between pages: run 2 came back on the second
			// page and run 3, newer or not, was never seen.
			name: "repeated entry while another is missing",
			pages: []page{
				{runs: []RunSummary{summary(1, 1), summary(2, 2)}, total: 3},
				{runs: []RunSummary{summary(2, 2)}, total: 3},
			},
			want: Latest{Checked: true, Read: 3, Distinct: 2, TotalCount: 3},
			deny: true,
		},
		{
			name: "pages after the stop are ignored",
			pages: []page{
				{runs: []RunSummary{byID}, total: 2, stop: true},
				{runs: []RunSummary{self()}, total: 9, stop: true},
			},
			want: Latest{Checked: true, Newer: &byID, Read: 1, TotalCount: 2},
			deny: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			scan := NewLatestScan(goodRun(), testPolicy())
			for i, p := range tt.pages {
				if got := scan.Add(p.runs, p.total); got != p.stop {
					t.Errorf("Add(page %d) = %v, want %v", i+1, got, p.stop)
				}
			}
			got := scan.Result()
			if !equalLatest(got, tt.want) {
				t.Errorf("Result() = %s, want %s", formatLatest(got), formatLatest(tt.want))
			}
			var want []Condition
			if tt.deny {
				want = []Condition{LatestRun}
			}
			assertDecision(t, Evaluate(goodJob(), goodRun(), got, testPolicy()), want)
		})
	}
}

func TestLatestScanZeroValueDenies(t *testing.T) {
	scan := NewLatestScan(goodRun(), testPolicy())
	assertDecision(t, Evaluate(goodJob(), goodRun(), scan.Result(), testPolicy()), []Condition{LatestRun})
}

// The result does not share Newer with the scan.
func TestLatestScanResultIsACopy(t *testing.T) {
	scan := NewLatestScan(goodRun(), testPolicy())
	scan.Add([]RunSummary{summary(goodRunID+1, goodRunNumber)}, 1)
	got := scan.Result()
	got.Newer.ID = 0
	if again := scan.Result(); again.Newer.ID != goodRunID+1 {
		t.Errorf("Newer.ID = %d after changing an earlier result", again.Newer.ID)
	}
}

// The result does not share OtherWorkflow with the scan.
func TestLatestScanResultCopiesOtherWorkflow(t *testing.T) {
	scan := NewLatestScan(goodRun(), testPolicy())
	other := summary(1, 1)
	other.WorkflowID = goodWorkflowID + 1
	scan.Add([]RunSummary{other}, 1)
	got := scan.Result()
	got.OtherWorkflow.ID = 0
	if again := scan.Result(); again.OtherWorkflow.ID != 1 {
		t.Errorf("OtherWorkflow.ID = %d after changing an earlier result", again.OtherWorkflow.ID)
	}
}

func equalSummary(a, b *RunSummary) bool {
	return (a == nil) == (b == nil) && (a == nil || *a == *b)
}

// equalLatest compares a scan result a with want b. A want that leaves
// Distinct at zero means every entry read was a different run.
func equalLatest(a, b Latest) bool {
	if b.Distinct == 0 {
		b.Distinct = b.Read
	}
	if !equalSummary(a.Newer, b.Newer) || !equalSummary(a.OtherWorkflow, b.OtherWorkflow) {
		return false
	}
	a.Newer, b.Newer = nil, nil
	a.OtherWorkflow, b.OtherWorkflow = nil, nil
	return a == b
}

func formatSummary(s *RunSummary) string {
	if s == nil {
		return "nil"
	}
	return fmt.Sprintf("%+v", *s)
}

func formatLatest(l Latest) string {
	return fmt.Sprintf("{Checked:%v Newer:%s OtherWorkflow:%s SelfSeen:%v Read:%d TotalCount:%d}",
		l.Checked, formatSummary(l.Newer), formatSummary(l.OtherWorkflow), l.SelfSeen, l.Read, l.TotalCount)
}
