package github

import (
	"fmt"
	"regexp"
	"strings"
)

// TargetKind says what scope a Target names.
type TargetKind int

// Target kinds. The zero kind is not valid.
const (
	// Organization is a GitHub organization (0002 SPEC R106).
	Organization TargetKind = iota + 1
)

// targetURLPrefix starts every target URL; the organization name follows.
const targetURLPrefix = "https://github.com/"

// orgNamePattern is the organization name a target URL may carry.
var orgNamePattern = regexp.MustCompile(`^[A-Za-z0-9-]+$`)

// Target is a place the scaler serves runners for. It is a comparable value.
type Target struct {
	Kind TargetKind
	// Name is the organization name as written in the URL.
	Name string
}

// ParseTargetURL reads a target's URL, which is exactly
// https://github.com/<org>: no trailing slash, ".git", query, fragment or
// further path segment (0002 SPEC R106). scaleset v0.5.0 reads a one-segment
// config URL as organization scope.
func ParseTargetURL(s string) (Target, error) {
	name, ok := strings.CutPrefix(s, targetURLPrefix)
	if !ok || !orgNamePattern.MatchString(name) {
		return Target{}, fmt.Errorf("github: target URL %q is not https://github.com/<organization>", s)
	}
	return Target{Kind: Organization, Name: name}, nil
}

// ConfigURL is the URL the scale set library takes as its config URL.
func (t Target) ConfigURL() string { return targetURLPrefix + t.Name }

// InstallationPath is the REST path of the App installation lookup.
func (t Target) InstallationPath() string { return "orgs/" + t.Name + "/installation" }

// RunnersPath is the REST path of the runner list.
func (t Target) RunnersPath() string { return "orgs/" + t.Name + "/actions/runners" }

// Permission returns the App permission the target needs and the access
// level it must have (0002 SPEC R110).
func (t Target) Permission() (name, level string) {
	return "organization_self_hosted_runners", "write"
}

// Key is the name that tells targets apart: GitHub organization names are
// case-insensitive, so it is the lower-case name (0002 SPEC R166).
func (t Target) Key() string { return strings.ToLower(t.Name) }

// String describes the target in messages.
func (t Target) String() string { return "organization " + t.Name }

// valid reports whether t names an organization.
func (t Target) valid() bool { return t.Kind == Organization && t.Name != "" }
