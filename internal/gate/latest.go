package gate

import (
	"errors"
	"fmt"
)

// searchResultCap is the most results the latest run lookup returns for one
// search: "This endpoint will return up to 1,000 results for each search"
// when filtered by branch or created (GitHub REST "List workflow runs for a
// workflow").
const searchResultCap = 1000

// RunSummary is one entry of the latest run lookup (REST "List workflow
// runs for a workflow"), holding the fields the latest run condition reads.
type RunSummary struct {
	// ID is id, the run ID.
	ID int64
	// WorkflowID is workflow_id, the ID of the entry's workflow.
	WorkflowID int64
	// RunNumber is run_number.
	RunNumber int64
	// HeadRepository is head_repository.full_name ("owner/repo").
	HeadRepository string
	// HeadBranch is head_branch.
	HeadBranch string
}

// Latest is the result of the latest run lookup for one run. The zero
// value means no lookup was made, and LatestRun fails it.
type Latest struct {
	// Checked reports that at least one page of the lookup was counted.
	Checked bool
	// Newer is the first counted run whose ID or run_number is larger than
	// the run's; nil when there is none.
	Newer *RunSummary
	// OtherWorkflow is the first entry whose workflow_id is not the run's;
	// nil when there is none. The lookup asks for the run's workflow only,
	// so such an entry means the filter was misread.
	OtherWorkflow *RunSummary
	// SelfSeen reports that a counted entry had the run's own ID.
	SelfSeen bool
	// Read is how many entries were read, counted or not.
	Read int
	// Distinct is how many different run IDs were read, counted or not. The
	// order of the results is not documented, so a run can come back on two
	// pages while another is missing; comparing total_count with Distinct
	// rather than Read catches that.
	Distinct int
	// TotalCount is the largest total_count of the pages read. A page
	// whose total_count shrank, as when a run is deleted between pages and
	// an entry may have been skipped, does not lower it.
	TotalCount int
}

// LatestScan counts the pages of the latest run lookup for one run (SPEC
// Design judgment 8). It counts only entries whose head repository is the
// policy's repository and whose head branch is exactly main, so a fork's
// main branch is not counted. It compares every entry and does not rely on
// the order of the results. It is not safe for concurrent use.
type LatestScan struct {
	run    Run
	policy Policy
	latest Latest
	seen   map[int64]struct{}
}

// NewLatestScan returns a scan for run under policy.
func NewLatestScan(run Run, policy Policy) *LatestScan {
	return &LatestScan{run: run, policy: policy, seen: make(map[int64]struct{})}
}

// Add counts one page of the lookup with the page's total_count. It reports
// true, and ignores every later page, once an entry has a workflow_id other
// than the run's or a counted entry has an ID or run_number larger than the
// run's: the job is then denied, and the remaining pages need not be read.
func (s *LatestScan) Add(page []RunSummary, totalCount int) (stop bool) {
	if s.latest.Newer != nil || s.latest.OtherWorkflow != nil {
		return true
	}
	s.latest.Checked = true
	s.latest.TotalCount = max(s.latest.TotalCount, totalCount)
	for _, e := range page {
		s.latest.Read++
		if _, dup := s.seen[e.ID]; !dup {
			s.seen[e.ID] = struct{}{}
			s.latest.Distinct++
		}
		if e.WorkflowID != s.run.WorkflowID {
			other := e
			s.latest.OtherWorkflow = &other
			return true
		}
		if e.HeadBranch != mainBranch || !s.policy.sameFullName(e.HeadRepository) {
			continue
		}
		if e.ID > s.run.ID || e.RunNumber > s.run.RunNumber {
			newer := e
			s.latest.Newer = &newer
			return true
		}
		if e.ID == s.run.ID {
			s.latest.SelfSeen = true
		}
	}
	return false
}

// Result returns what the scan counted, to pass to Evaluate. Call it only
// after the lookup returned without error. When the lookup fails, do not
// call Evaluate with a partial result: the judge denies the job on the
// error alone (R65), whatever the pages read so far showed.
func (s *LatestScan) Result() Latest {
	l := s.latest
	if l.Newer != nil {
		newer := *l.Newer
		l.Newer = &newer
	}
	if l.OtherWorkflow != nil {
		other := *l.OtherWorkflow
		l.OtherWorkflow = &other
	}
	return l
}

// checkLatestRun is the tenth R9 condition: no newer main run of the same
// workflow exists, and the lookup proves it.
func checkLatestRun(in input) error {
	run, l := in.run, in.latest
	switch {
	case run.ID <= 0:
		return errors.New("run id is missing")
	case run.WorkflowID <= 0:
		return errors.New("run workflow_id is missing")
	case run.RunNumber <= 0:
		return errors.New("run run_number is missing")
	case run.CreatedAt.IsZero():
		return errors.New("run created_at is missing")
	case !l.Checked:
		return errors.New("latest run lookup was not made")
	case l.Newer != nil:
		return fmt.Errorf("newer main run %d (run_number %d) of the workflow exists; run is %d (run_number %d)",
			l.Newer.ID, l.Newer.RunNumber, run.ID, run.RunNumber)
	case l.OtherWorkflow != nil:
		// The lookup asks for the run's workflow, so the filter was
		// misread and the result proves nothing.
		return fmt.Errorf("latest run lookup returned run %d of workflow %d; run's workflow is %d",
			l.OtherWorkflow.ID, l.OtherWorkflow.WorkflowID, run.WorkflowID)
	case !l.SelfSeen:
		// The lookup's filter always matches the run itself, so a result
		// without it was not read correctly.
		return fmt.Errorf("run %d is not in the latest run lookup", run.ID)
	case l.TotalCount < 0:
		return fmt.Errorf("latest run lookup total_count %d is negative", l.TotalCount)
	case l.Read >= searchResultCap:
		// SPEC Design judgment 8 denies a result cut at the 1,000-result
		// cap. Even when total_count equals what was read, 1,000 results
		// may be the cap rather than all runs, so this denies every lookup
		// that reached the cap: stricter than judgment 8, never looser.
		return fmt.Errorf("latest run lookup read %d runs, the %d-result cap", l.Read, searchResultCap)
	case l.TotalCount > l.Distinct:
		// Unread runs, beyond the pages, skipped when a deletion shifted
		// them, or replaced by a repeat when the order moved, may be newer.
		return fmt.Errorf("latest run lookup read %d different runs of %d", l.Distinct, l.TotalCount)
	}
	return nil
}
