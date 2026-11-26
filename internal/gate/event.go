// The one-object decode (json.Decoder with UseNumber, then Token must return
// io.EOF) and the head/base repository ID comparison are adapted from
// runner-up:
//
//	Source:    https://github.com/kunchenguid/runner-up
//	Commit:    013cc74188985248185732fb86cb52ec212a1960
//	Path:      internal/agent/forkguard.go
//	Copyright (c) 2026 Kun Chen
//	License:   MIT
//
// The comparison with the top-level repository.id follows the logic of
// https://github.com/actions/checkout src/unsafe-pr-checkout-helper.ts (MIT);
// no code is copied from it.

package gate

import (
	"bytes"
	"encoding/json"
	"io"
	"strconv"
)

// MaxEventPayload is the largest event report body judged; a larger one is
// denied (0002 SPEC R136): 32 MiB.
const MaxEventPayload = 32 << 20

// maxEventName is the longest event name allowed (0002 SPEC R125).
const maxEventName = 64

// Deny reasons, logged as reason (0002 SPEC Design 판정, R130).
const (
	ReasonPayloadTooLarge          = "payload-too-large"
	ReasonPayloadNotOneObject      = "payload-not-one-object"
	ReasonEventNameInvalid         = "event-name-invalid"
	ReasonPullRequestRepoIDInvalid = "pull-request-repo-id-invalid"
	ReasonPullRequestFromOtherRepo = "pull-request-from-other-repo"
	ReasonWorkflowRunRepoIDInvalid = "workflow-run-repo-id-invalid"
	ReasonWorkflowRunFromOtherRepo = "workflow-run-from-other-repo"
	ReasonPullRequestMissing       = "pull-request-missing"
	ReasonWorkflowRunMissing       = "workflow-run-missing"
)

// Decision is the verdict on one event report. The zero value denies.
type Decision struct {
	Allow bool
	// Reason is one of the Reason constants when denied, empty when allowed.
	Reason string
	// HeadRepo is pull_request.head.repo.full_name or
	// workflow_run.head_repository.full_name when it is a JSON string, for
	// the deny log only (0002 SPEC R130). It never affects the verdict.
	HeadRepo string
}

// pullRequestEvent reports whether name is an event that must carry
// pull_request (0002 SPEC R128).
func pullRequestEvent(name string) bool {
	switch name {
	case "pull_request", "pull_request_target", "pull_request_review", "pull_request_review_comment":
		return true
	}
	return false
}

// Decide judges an event report from its event name and event file body
// alone (0002 SPEC R123). The first failing rule decides, in the order of
// 0002 SPEC Design 판정; anything missing or unparsable denies. It never
// reads head.repo.fork, full_name, author_association, branch, ref or
// workflow path for the verdict (0002 SPEC R129).
func Decide(eventName string, payload []byte) Decision {
	if len(payload) > MaxEventPayload {
		return deny(ReasonPayloadTooLarge, "") // 0002 SPEC R136
	}
	event, ok := decodeOneObject(payload)
	if !ok {
		return deny(ReasonPayloadNotOneObject, "") // 0002 SPEC R124
	}
	// Each nested object is decoded once; a body may be 32 MiB.
	pr, hasPR := member(event, "pull_request")
	run, hasRun := member(event, "workflow_run")
	head := headRepo(pr, run)
	if !validEventName(eventName) {
		return deny(ReasonEventNameInvalid, head) // 0002 SPEC R125
	}
	// 0002 SPEC R126, R127: by structure, whatever the event name.
	if hasPR {
		if r := sameRepository(event, pr, []string{"head", "repo", "id"}, []string{"base", "repo", "id"},
			ReasonPullRequestRepoIDInvalid, ReasonPullRequestFromOtherRepo); r != "" {
			return deny(r, head)
		}
	}
	if hasRun {
		if r := sameRepository(event, run, []string{"head_repository", "id"}, []string{"repository", "id"},
			ReasonWorkflowRunRepoIDInvalid, ReasonWorkflowRunFromOtherRepo); r != "" {
			return deny(r, head)
		}
	}
	// 0002 SPEC R128: a PR-family or workflow_run name with nothing compared.
	if pullRequestEvent(eventName) && !hasPR {
		return deny(ReasonPullRequestMissing, head)
	}
	if eventName == "workflow_run" && !hasRun {
		return deny(ReasonWorkflowRunMissing, head)
	}
	return Decision{Allow: true, HeadRepo: head} // 0002 SPEC R129
}

func deny(reason, head string) Decision {
	return Decision{Reason: reason, HeadRepo: head}
}

// decodeOneObject returns the top-level members when payload is exactly one
// JSON object, optionally followed by white space (0002 SPEC R124). Values
// stay raw, so numbers keep their original text.
func decodeOneObject(payload []byte) (map[string]json.RawMessage, bool) {
	dec := json.NewDecoder(bytes.NewReader(payload))
	dec.UseNumber()
	var event map[string]json.RawMessage
	// A top-level null decodes without error into a nil map; it is not an
	// object.
	if err := dec.Decode(&event); err != nil || event == nil {
		return nil, false
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, false
	}
	return event, true
}

// validEventName reports whether name matches ^[a-z_]{1,64}$ (0002 SPEC R125).
func validEventName(name string) bool {
	if len(name) == 0 || len(name) > maxEventName {
		return false
	}
	for i := 0; i < len(name); i++ {
		if c := name[i]; c != '_' && (c < 'a' || c > 'z') {
			return false
		}
	}
	return true
}

// nested is a top-level member that is present and not null, decoded once.
// obj is nil when the member is not a JSON object, so every lookup in it
// fails.
type nested struct {
	obj map[string]json.RawMessage
}

// member returns event's member key and whether it is present and not null.
func member(event map[string]json.RawMessage, key string) (nested, bool) {
	v, ok := event[key]
	if !ok || bytes.Equal(v, []byte("null")) {
		return nested{}, false
	}
	obj, _ := object(v)
	return nested{obj: obj}, true
}

// object decodes v when it is a JSON object; null and other values fail.
func object(v json.RawMessage) (map[string]json.RawMessage, bool) {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(v, &obj); err != nil || obj == nil {
		return nil, false
	}
	return obj, true
}

// lookup walks path through nested objects from obj and returns the raw
// value at its end.
func lookup(obj map[string]json.RawMessage, path ...string) (json.RawMessage, bool) {
	for i, key := range path {
		v, ok := obj[key]
		if !ok {
			return nil, false
		}
		if i == len(path)-1 {
			return v, true
		}
		if obj, ok = object(v); !ok {
			return nil, false
		}
	}
	return nil, false
}

// sameRepository checks that the head and base IDs inside n and the
// top-level repository.id are all valid repository IDs and equal (0002 SPEC
// R126, R127). It returns "" when they pass, invalid when any is not a valid
// ID, and other when they differ.
func sameRepository(event map[string]json.RawMessage, n nested, headPath, basePath []string, invalid, other string) string {
	head, ok1 := repoID(lookup(n.obj, headPath...))
	base, ok2 := repoID(lookup(n.obj, basePath...))
	top, ok3 := repoID(lookup(event, "repository", "id"))
	if !ok1 || !ok2 || !ok3 {
		return invalid
	}
	// Valid IDs have no sign or leading zero, so equal text is equal value.
	if head != base || head != top {
		return other
	}
	return ""
}

// repoID returns the number text of v when it is a positive integer of at
// most 2^63-1, written without sign, fraction, exponent or leading zero. The
// text is never decoded into a json.Number field, which would also accept a
// quoted number.
func repoID(v json.RawMessage, found bool) (string, bool) {
	s := string(v)
	if !found || !positiveDecimal(s) {
		return "", false
	}
	if _, err := strconv.ParseInt(s, 10, 64); err != nil {
		return "", false
	}
	return s, true
}

// positiveDecimal reports whether s matches ^[1-9][0-9]*$.
func positiveDecimal(s string) bool {
	if s == "" || s[0] < '1' || s[0] > '9' {
		return false
	}
	for i := 1; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// headRepo returns the head repository's full_name for the deny log
// (0002 SPEC R130), from pull_request first, then workflow_run.
func headRepo(pr, run nested) string {
	if s, ok := stringValue(lookup(pr.obj, "head", "repo", "full_name")); ok {
		return s
	}
	s, _ := stringValue(lookup(run.obj, "head_repository", "full_name"))
	return s
}

// stringValue decodes v when it is a JSON string.
func stringValue(v json.RawMessage, found bool) (string, bool) {
	if !found || len(v) == 0 || v[0] != '"' {
		return "", false
	}
	var s string
	if err := json.Unmarshal(v, &s); err != nil {
		return "", false
	}
	return s, true
}
