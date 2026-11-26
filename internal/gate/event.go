package gate

import (
	"bytes"
	"encoding/json"
	"io"
	"slices"
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

// Decide judges an event report from its event name and event file body
// alone (0002 SPEC R123). Rules run in the order of 0002 SPEC Design 판정
// and the first one that fails decides; a missing or unreadable value
// denies. Fork flags, names, author association, branch, ref and workflow
// path are never read for the verdict (0002 SPEC R129).
func Decide(eventName string, payload []byte) Decision {
	if len(payload) > MaxEventPayload {
		return Decision{Reason: ReasonPayloadTooLarge} // 0002 SPEC R136
	}
	event, ok := parseObject(payload)
	if !ok {
		return Decision{Reason: ReasonPayloadNotOneObject} // 0002 SPEC R124
	}
	found := findSources(event)
	reason := denyReason(eventName, event, found)
	return Decision{Allow: reason == "", Reason: reason, HeadRepo: headRepoName(found)}
}

// denyReason applies the rules after the body is known to be one object
// and returns "" when all pass (0002 SPEC R125–R129).
func denyReason(eventName string, event object, found []source) string {
	if !validEventName(eventName) {
		return ReasonEventNameInvalid
	}
	// 0002 SPEC R126, R127: by structure, whatever the event name; every
	// carried object must pass.
	top := event.at("repository", "id")
	for _, s := range found {
		if s.present {
			if r := s.check(top); r != "" {
				return r
			}
		}
	}
	// 0002 SPEC R128: a name that promises an object nothing compared.
	for _, s := range found {
		if !s.present && slices.Contains(s.eventNames, eventName) {
			return s.missing
		}
	}
	return ""
}

// sourceRule describes a top-level object that names the repository a
// run's code came from (0002 SPEC R126–R128, R130). Rules are checked in
// order; another event family of this kind is one more entry.
type sourceRule struct {
	key        string   // top-level member holding the object
	headID     []string // path in the object to the source repository ID
	baseID     []string // path in the object to the target repository ID
	headName   []string // path in the object to the source full_name
	eventNames []string // event names that must carry the object
	invalid    string   // reason when an ID is missing or malformed
	foreign    string   // reason when the IDs differ
	missing    string   // reason when a required object is absent
}

func sourceRules() []sourceRule {
	return []sourceRule{
		{
			key:        "pull_request",
			headID:     []string{"head", "repo", "id"},
			baseID:     []string{"base", "repo", "id"},
			headName:   []string{"head", "repo", "full_name"},
			eventNames: []string{"pull_request", "pull_request_target", "pull_request_review", "pull_request_review_comment"},
			invalid:    ReasonPullRequestRepoIDInvalid,
			foreign:    ReasonPullRequestFromOtherRepo,
			missing:    ReasonPullRequestMissing,
		},
		{
			key:        "workflow_run",
			headID:     []string{"head_repository", "id"},
			baseID:     []string{"repository", "id"},
			headName:   []string{"head_repository", "full_name"},
			eventNames: []string{"workflow_run"},
			invalid:    ReasonWorkflowRunRepoIDInvalid,
			foreign:    ReasonWorkflowRunFromOtherRepo,
			missing:    ReasonWorkflowRunMissing,
		},
	}
}

// source is one rule's object as found in an event body.
type source struct {
	sourceRule
	present bool   // the member exists and is not null
	obj     object // the member when it is a JSON object, else nil
}

// findSources looks up every rule's member once; a body may be 32 MiB.
func findSources(event object) []source {
	rules := sourceRules()
	found := make([]source, len(rules))
	for i, r := range rules {
		raw, ok := event[r.key]
		found[i] = source{
			sourceRule: r,
			present:    ok && string(raw) != "null",
			obj:        asObject(raw),
		}
	}
	return found
}

// check compares the head and base IDs inside the object with the
// top-level repository.id and returns "" when they are one valid ID
// (0002 SPEC R126, R127). An invalid ID wins over a mismatch.
func (s source) check(top json.RawMessage) string {
	valid, equal := sameID(s.obj.at(s.headID...), s.obj.at(s.baseID...), top)
	switch {
	case !valid:
		return s.invalid
	case !equal:
		return s.foreign
	}
	return ""
}

// headRepoName returns the first carried full_name that is a JSON string,
// in rule order, for the deny log only (0002 SPEC R130).
func headRepoName(found []source) string {
	for _, s := range found {
		if name, ok := jsonString(s.obj.at(s.headName...)); ok {
			return name
		}
	}
	return ""
}

// object is a JSON object whose member values are kept undecoded, so
// numbers keep their original text (0002 SPEC R124).
type object map[string]json.RawMessage

// parseObject returns the members of data when it holds exactly one JSON
// object, with nothing after it but white space (0002 SPEC R124).
func parseObject(data []byte) (object, bool) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber() // 0002 SPEC Design 판정
	var obj object
	// A top-level null decodes without error into a nil map.
	if err := dec.Decode(&obj); err != nil || obj == nil {
		return nil, false
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, false
	}
	return obj, true
}

// asObject decodes raw when it is a JSON object; anything else, null and a
// missing value included, gives nil.
func asObject(raw json.RawMessage) object {
	var obj object
	if json.Unmarshal(raw, &obj) != nil {
		return nil
	}
	return obj
}

// at follows path through nested objects and returns the raw value at its
// end, or nil when a step is missing or not an object. path is not empty.
func (o object) at(path ...string) json.RawMessage {
	last := len(path) - 1
	for _, key := range path[:last] {
		o = asObject(o[key])
	}
	return o[path[last]]
}

// sameID reports whether every raw value is a valid repository ID and, if
// so, whether they are all the same ID (0002 SPEC R126). Valid IDs have no
// sign or leading zero, so equal text means equal value, even beyond 2^53.
func sameID(raws ...json.RawMessage) (valid, equal bool) {
	ids := make([]string, len(raws))
	for i, raw := range raws {
		id, ok := repoID(raw)
		if !ok {
			return false, false
		}
		ids[i] = id
	}
	for _, id := range ids[1:] {
		if id != ids[0] {
			return true, false
		}
	}
	return true, true
}

// repoID returns the text of raw when it is a JSON number from 1 to 2^63-1
// written as plain digits: no sign, fraction, exponent or leading zero
// (0002 SPEC R126). It reads the raw text rather than a json.Number field,
// which would also accept a quoted number.
func repoID(raw json.RawMessage) (string, bool) {
	s := string(raw)
	if !digitsFromOne(s) {
		return "", false
	}
	if _, err := strconv.ParseInt(s, 10, 64); err != nil {
		return "", false
	}
	return s, true
}

// digitsFromOne reports whether s matches ^[1-9][0-9]*$.
func digitsFromOne(s string) bool {
	if s == "" || s[0] == '0' {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// validEventName reports whether name matches ^[a-z_]{1,64}$ (0002 SPEC R125).
func validEventName(name string) bool {
	if name == "" || len(name) > maxEventName {
		return false
	}
	for i := 0; i < len(name); i++ {
		if c := name[i]; c != '_' && (c < 'a' || c > 'z') {
			return false
		}
	}
	return true
}

// jsonString decodes raw when it is a JSON string; null and other values
// are not strings.
func jsonString(raw json.RawMessage) (string, bool) {
	if len(raw) == 0 || raw[0] != '"' {
		return "", false
	}
	var s string
	if json.Unmarshal(raw, &s) != nil {
		return "", false
	}
	return s, true
}
