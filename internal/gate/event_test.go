package gate

import (
	"bytes"
	"strings"
	"testing"
)

// prBody is a pull request event body with the given raw JSON values for
// head.repo.id, base.repo.id and the top-level repository.id.
func prBody(head, base, top string) string {
	return `{"action":"opened","pull_request":{"head":{"repo":{"id":` + head +
		`,"full_name":"octo/fork","fork":true}},"base":{"repo":{"id":` + base +
		`,"full_name":"octo/app"}}},"repository":{"id":` + top + `,"full_name":"octo/app"}}`
}

// runBody is a workflow_run event body with the given raw JSON values for
// workflow_run.head_repository.id, workflow_run.repository.id and the
// top-level repository.id.
func runBody(head, base, top string) string {
	return `{"action":"completed","workflow_run":{"head_repository":{"id":` + head +
		`,"full_name":"octo/fork"},"repository":{"id":` + base +
		`}},"repository":{"id":` + top + `}}`
}

func TestDecide(t *testing.T) {
	const (
		same       = `{"pull_request":{"head":{"repo":{"id":42}},"base":{"repo":{"id":42}}},"repository":{"id":42}}`
		beyond53   = "9007199254740993" // 2^53+1: a float64 would read it as 2^53
		beyond53m1 = "9007199254740992"
		maxInt64   = "9223372036854775807"
	)
	tests := []struct {
		name    string
		event   string
		payload string
		want    string // "" allows
	}{
		// R126: pull_request compared by structure.
		{"same-repository PR", "pull_request", same, ""},
		{"fork PR", "pull_request", prBody("7", "42", "42"), ReasonPullRequestFromOtherRepo},
		{"fork pull_request_target", "pull_request_target", prBody("7", "42", "42"), ReasonPullRequestFromOtherRepo},
		{"head and base equal, repository differs", "pull_request_target", prBody("7", "7", "42"), ReasonPullRequestFromOtherRepo},
		{"base differs", "pull_request", prBody("42", "7", "42"), ReasonPullRequestFromOtherRepo},
		{"head.repo.fork true with equal IDs", "pull_request", prBody("42", "42", "42"), ""},
		{"head.repo null", "pull_request_review",
			`{"pull_request":{"head":{"repo":null},"base":{"repo":{"id":42}}},"repository":{"id":42}}`, ReasonPullRequestRepoIDInvalid},
		{"head missing", "pull_request",
			`{"pull_request":{"base":{"repo":{"id":42}}},"repository":{"id":42}}`, ReasonPullRequestRepoIDInvalid},
		{"head.repo.id null", "pull_request", prBody("null", "42", "42"), ReasonPullRequestRepoIDInvalid},
		{"base.repo.id missing", "pull_request",
			`{"pull_request":{"head":{"repo":{"id":42}},"base":{"repo":{}}},"repository":{"id":42}}`, ReasonPullRequestRepoIDInvalid},
		{"top-level repository missing", "pull_request",
			`{"pull_request":{"head":{"repo":{"id":42}},"base":{"repo":{"id":42}}}}`, ReasonPullRequestRepoIDInvalid},
		{"top-level repository not an object", "pull_request",
			`{"pull_request":{"head":{"repo":{"id":42}},"base":{"repo":{"id":42}}},"repository":42}`, ReasonPullRequestRepoIDInvalid},
		{"pull_request not an object", "pull_request", `{"pull_request":42,"repository":{"id":42}}`, ReasonPullRequestRepoIDInvalid},
		{"pull_request an array", "push", `{"pull_request":[],"repository":{"id":42}}`, ReasonPullRequestRepoIDInvalid},
		{"head.repo an array", "pull_request",
			`{"pull_request":{"head":{"repo":[42]},"base":{"repo":{"id":42}}},"repository":{"id":42}}`, ReasonPullRequestRepoIDInvalid},
		{"invalid ID wins over a mismatch", "pull_request", prBody("7", "4.2", "42"), ReasonPullRequestRepoIDInvalid},
		{"non-PR name carrying pull_request compared", "issue_comment", prBody("7", "42", "42"), ReasonPullRequestFromOtherRepo},
		{"non-PR name carrying same-repository pull_request", "check_run", prBody("42", "42", "42"), ""},
		{"white space around values", "pull_request",
			"{ \"pull_request\" : { \"head\" : { \"repo\" : { \"id\" : 42 } } , \"base\" : { \"repo\" : { \"id\" :\n42\t} } } , \"repository\" : { \"id\" : 42 } }", ""},

		// IDs: positive integers by original text (R124, R126).
		{"equal IDs beyond 2^53", "pull_request", prBody(beyond53, beyond53, beyond53), ""},
		{"IDs beyond 2^53 differing by 1", "pull_request", prBody(beyond53, beyond53m1, beyond53), ReasonPullRequestFromOtherRepo},
		{"max int64", "pull_request", prBody(maxInt64, maxInt64, maxInt64), ""},
		{"above int64", "pull_request", prBody("9223372036854775808", "9223372036854775808", "9223372036854775808"), ReasonPullRequestRepoIDInvalid},
		{"fraction", "pull_request", prBody("42.0", "42", "42"), ReasonPullRequestRepoIDInvalid},
		{"exponent", "pull_request", prBody("4.2e1", "42", "42"), ReasonPullRequestRepoIDInvalid},
		{"integer exponent", "pull_request", prBody("42e0", "42", "42"), ReasonPullRequestRepoIDInvalid},
		{"upper-case exponent", "pull_request", prBody("42", "42", "42E0"), ReasonPullRequestRepoIDInvalid},
		{"negative", "pull_request", prBody("-42", "-42", "-42"), ReasonPullRequestRepoIDInvalid},
		{"zero", "pull_request", prBody("0", "0", "0"), ReasonPullRequestRepoIDInvalid},
		// A leading zero is not valid JSON, so the one-object rule denies it
		// first; repoID refuses it too.
		{"leading zero", "pull_request", prBody("042", "042", "042"), ReasonPayloadNotOneObject},
		{"quoted", "pull_request", prBody(`"42"`, `"42"`, `"42"`), ReasonPullRequestRepoIDInvalid},
		{"quoted top-level", "pull_request", prBody("42", "42", `"42"`), ReasonPullRequestRepoIDInvalid},
		{"boolean", "pull_request", prBody("true", "42", "42"), ReasonPullRequestRepoIDInvalid},
		{"object", "pull_request", prBody(`{"id":42}`, "42", "42"), ReasonPullRequestRepoIDInvalid},

		// R127: workflow_run compared by structure.
		{"same-repository workflow_run", "workflow_run", runBody("42", "42", "42"), ""},
		{"fork workflow_run", "workflow_run", runBody("7", "42", "42"), ReasonWorkflowRunFromOtherRepo},
		{"workflow_run repository differs", "workflow_run", runBody("7", "7", "42"), ReasonWorkflowRunFromOtherRepo},
		{"workflow_run head_repository null", "workflow_run",
			`{"workflow_run":{"head_repository":null,"repository":{"id":42}},"repository":{"id":42}}`, ReasonWorkflowRunRepoIDInvalid},
		{"workflow_run quoted ID", "workflow_run", runBody(`"42"`, "42", "42"), ReasonWorkflowRunRepoIDInvalid},
		{"workflow_run IDs beyond 2^53 differing by 1", "workflow_run", runBody(beyond53m1, beyond53, beyond53), ReasonWorkflowRunFromOtherRepo},
		{"workflow_run not an object", "workflow_run", `{"workflow_run":"x","repository":{"id":42}}`, ReasonWorkflowRunRepoIDInvalid},
		{"non-workflow_run name carrying workflow_run compared", "push", runBody("7", "42", "42"), ReasonWorkflowRunFromOtherRepo},
		{"both present, both pass", "pull_request",
			`{"pull_request":{"head":{"repo":{"id":42}},"base":{"repo":{"id":42}}},"workflow_run":{"head_repository":{"id":42},"repository":{"id":42}},"repository":{"id":42}}`, ""},
		{"both present, workflow_run fails", "pull_request",
			`{"pull_request":{"head":{"repo":{"id":42}},"base":{"repo":{"id":42}}},"workflow_run":{"head_repository":{"id":7},"repository":{"id":42}},"repository":{"id":42}}`, ReasonWorkflowRunFromOtherRepo},
		{"both present, pull_request decides first", "workflow_run",
			`{"pull_request":{"head":{"repo":{"id":7}},"base":{"repo":{"id":42}}},"workflow_run":{"head_repository":{"id":7},"repository":{"id":42}},"repository":{"id":42}}`, ReasonPullRequestFromOtherRepo},

		// R128: PR-family and workflow_run names need their object.
		{"pull_request without the object", "pull_request", `{"repository":{"id":42}}`, ReasonPullRequestMissing},
		{"pull_request_target without the object", "pull_request_target", `{"repository":{"id":42}}`, ReasonPullRequestMissing},
		{"pull_request_review with null object", "pull_request_review", `{"pull_request":null,"repository":{"id":42}}`, ReasonPullRequestMissing},
		{"pull_request_review_comment without the object", "pull_request_review_comment", `{}`, ReasonPullRequestMissing},
		{"null object with white space", "pull_request", `{"pull_request" :  null }`, ReasonPullRequestMissing},
		{"workflow_run without the object", "workflow_run", `{"repository":{"id":42}}`, ReasonWorkflowRunMissing},
		{"workflow_run with null object", "workflow_run", `{"workflow_run":null}`, ReasonWorkflowRunMissing},
		{"pull_request name with only workflow_run", "pull_request", runBody("42", "42", "42"), ReasonPullRequestMissing},

		// R129: everything else allowed; names alone deny nothing.
		{"issue_comment", "issue_comment", `{"issue":{"number":1},"comment":{"author_association":"NONE"},"repository":{"id":42}}`, ""},
		{"push", "push", `{"ref":"refs/heads/feature","repository":{"id":42}}`, ""},
		{"empty object", "push", `{}`, ""},
		{"null pull_request on push", "push", `{"pull_request":null,"workflow_run":null}`, ""},
		{"trailing white space", "push", "{}\n\t \r\n", ""},
		{"leading white space", "push", " \n{}", ""},

		// R124: exactly one JSON object.
		{"empty body", "push", ``, ReasonPayloadNotOneObject},
		{"white space only", "push", " \n", ReasonPayloadNotOneObject},
		{"not JSON", "push", `pull_request`, ReasonPayloadNotOneObject},
		{"array", "push", `[{}]`, ReasonPayloadNotOneObject},
		{"string", "push", `"{}"`, ReasonPayloadNotOneObject},
		{"number", "push", `42`, ReasonPayloadNotOneObject},
		{"null", "push", `null`, ReasonPayloadNotOneObject},
		{"trailing value", "push", `{} 1`, ReasonPayloadNotOneObject},
		{"two objects", "push", `{}{}`, ReasonPayloadNotOneObject},
		{"two objects, second a fork PR", "push", `{"repository":{"id":42}}` + prBody("7", "42", "42"), ReasonPayloadNotOneObject},
		{"trailing garbage", "push", `{}x`, ReasonPayloadNotOneObject},
		{"trailing comma", "push", `{},`, ReasonPayloadNotOneObject},
		{"truncated", "pull_request", prBody("42", "42", "42")[:40], ReasonPayloadNotOneObject},
		{"truncated after the object's key", "push", `{"a"`, ReasonPayloadNotOneObject},
		{"invalid nested number", "push", `{"a":01}`, ReasonPayloadNotOneObject},
		{"one-object check before name", "", `[]`, ReasonPayloadNotOneObject},

		// R125: event name.
		{"empty name", "", same, ReasonEventNameInvalid},
		{"upper case", "Pull_request", same, ReasonEventNameInvalid},
		{"hyphen", "pull-request", same, ReasonEventNameInvalid},
		{"hyphen alone", "-", `{}`, ReasonEventNameInvalid},
		{"digit", "push2", `{}`, ReasonEventNameInvalid},
		{"space", "push ", `{}`, ReasonEventNameInvalid},
		{"non-ASCII", "pushé", `{}`, ReasonEventNameInvalid},
		{"65 characters", strings.Repeat("a", 65), `{}`, ReasonEventNameInvalid},
		{"64 characters", strings.Repeat("a", 64), `{}`, ""},
		{"name before structure", "Pull_request", prBody("7", "42", "42"), ReasonEventNameInvalid},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assertEventDecision(t, Decide(tt.event, []byte(tt.payload)), tt.want)
		})
	}
}

func assertEventDecision(t *testing.T, d Decision, want string) {
	t.Helper()
	if d.Allow != (want == "") || d.Reason != want {
		t.Errorf("Decide = {Allow: %v, Reason: %q}, want {Allow: %v, Reason: %q}", d.Allow, d.Reason, want == "", want)
	}
}

// TestDecideSize checks the R136 bound: Decide denies a body above 32 MiB
// before reading it, and judges one of exactly 32 MiB.
func TestDecideSize(t *testing.T) {
	body := prBody("42", "42", "42")
	exact := []byte(body + strings.Repeat(" ", MaxEventPayload-len(body)))
	if len(exact) != 33554432 {
		t.Fatalf("len = %d, want 33554432", len(exact))
	}
	assertEventDecision(t, Decide("pull_request", exact), "")
	over := append(exact, ' ')
	assertEventDecision(t, Decide("pull_request", over), ReasonPayloadTooLarge)
	// Size is checked before anything else.
	assertEventDecision(t, Decide("", bytes.Repeat([]byte("x"), MaxEventPayload+1)), ReasonPayloadTooLarge)
}

// TestDecideHeadRepo checks the head_repo log value (R130): taken from
// pull_request first, then workflow_run, only when it is a JSON string, and
// never changing the verdict.
func TestDecideHeadRepo(t *testing.T) {
	tests := []struct {
		name    string
		event   string
		payload string
		want    string
		reason  string
	}{
		{"fork PR", "pull_request", prBody("7", "42", "42"), "octo/fork", ReasonPullRequestFromOtherRepo},
		{"same-repository PR", "pull_request", prBody("42", "42", "42"), "octo/fork", ""},
		{"fork workflow_run", "workflow_run", runBody("7", "42", "42"), "octo/fork", ReasonWorkflowRunFromOtherRepo},
		{"pull_request before workflow_run", "pull_request",
			`{"pull_request":{"head":{"repo":{"id":7,"full_name":"a/pr"}}},"workflow_run":{"head_repository":{"full_name":"b/run"}}}`, "a/pr", ReasonPullRequestRepoIDInvalid},
		{"workflow_run when pull_request has none", "pull_request",
			`{"pull_request":{"head":{"repo":null}},"workflow_run":{"head_repository":{"full_name":"b/run"}}}`, "b/run", ReasonPullRequestRepoIDInvalid},
		{"escaped string", "pull_request",
			`{"pull_request":{"head":{"repo":{"full_name":"a/b"}}}}`, "a/b", ReasonPullRequestRepoIDInvalid},
		{"number full_name", "pull_request", `{"pull_request":{"head":{"repo":{"full_name":42}}}}`, "", ReasonPullRequestRepoIDInvalid},
		{"null full_name", "pull_request", `{"pull_request":{"head":{"repo":{"full_name":null}}}}`, "", ReasonPullRequestRepoIDInvalid},
		{"null head repo", "pull_request", `{"pull_request":{"head":{"repo":null}}}`, "", ReasonPullRequestRepoIDInvalid},
		{"invalid name keeps head_repo", "BAD", prBody("7", "42", "42"), "octo/fork", ReasonEventNameInvalid},
		{"not one object", "pull_request", prBody("7", "42", "42") + "{}", "", ReasonPayloadNotOneObject},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := Decide(tt.event, []byte(tt.payload))
			assertEventDecision(t, d, tt.reason)
			if d.HeadRepo != tt.want {
				t.Errorf("HeadRepo = %q, want %q", d.HeadRepo, tt.want)
			}
		})
	}
}

// TestDecideIgnoresUnusedFields checks that fork, full_name,
// author_association, branch, ref and workflow path never change the
// verdict (R129).
func TestDecideIgnoresUnusedFields(t *testing.T) {
	const ids = `"head":{"ref":"main","repo":{"id":42,"fork":true,"full_name":"other/name"}},` +
		`"base":{"ref":"x","repo":{"id":42,"fork":false,"full_name":"octo/app"}},"author_association":"NONE"`
	payload := `{"pull_request":{` + ids + `},"ref":"refs/tags/x","workflow":".github/workflows/x.yml","repository":{"id":42,"full_name":"renamed/app"}}`
	assertEventDecision(t, Decide("pull_request_target", []byte(payload)), "")
}

// TestRepoID checks the ID rule on raw number text directly, including
// forms that valid JSON never reaches it with.
func TestRepoID(t *testing.T) {
	tests := []struct {
		raw  string
		want bool
	}{
		{"1", true},
		{"9223372036854775807", true},
		{"9223372036854775808", false},
		{"042", false},
		{"0", false},
		{"-1", false},
		{"+1", false},
		{"1.0", false},
		{"1e3", false},
		{`"1"`, false},
		{" 1", false},
		{"", false},
		{"null", false},
	}
	for _, tt := range tests {
		if _, got := repoID([]byte(tt.raw)); got != tt.want {
			t.Errorf("repoID(%q) = %v, want %v", tt.raw, got, tt.want)
		}
	}
	if _, got := repoID(nil); got {
		t.Error("repoID of a missing value is valid")
	}
}

func TestZeroEventDecisionDenies(t *testing.T) {
	var d Decision
	if d.Allow {
		t.Fatal("zero Decision allows")
	}
}
