package github

import (
	"fmt"
	"regexp"
	"strings"
)

// targetURLPrefix starts every target URL; owner and repository follow.
const targetURLPrefix = "https://github.com/"

var (
	// ownerPattern is the owner (organization or account) a target URL may
	// carry.
	ownerPattern = regexp.MustCompile(`^[A-Za-z0-9-]+$`)
	// repoPattern is the repository name a target URL may carry.
	repoPattern = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)
)

// Target is one repository the scaler registers runners for; its owner is
// an organization or a personal account. It is a comparable value.
type Target struct {
	// Owner and Repo are the names as written in the URL.
	Owner, Repo string
}

// ParseTargetURL reads a target's URL, which is exactly
// https://github.com/<owner>/<repo>: no trailing slash, query, fragment or
// third path segment; an owner other than "enterprises", a repository other
// than "." and "..", and no ".git" suffix, all compared without case (0002
// SPEC R231). An organization URL is refused like any other.
//
// scaleset v0.5.0 reads a two-segment config URL whose first segment is not
// "enterprises" as repository scope and asks
// /repos/{owner}/{repo}/actions/runners/registration-token for its
// registration token, confirmed (0002 SPEC Design 구성 요소 target; v0.5.0
// config.go 32-73, client.go 1052-1054).
func ParseTargetURL(s string) (Target, error) {
	if owner, repo, ok := splitRepoURL(s); ok {
		return Target{Owner: owner, Repo: repo}, nil
	}
	return Target{}, fmt.Errorf("github: target URL %q is not https://github.com/<owner>/<repo>", s)
}

// splitRepoURL returns the owner and repository of s when s follows the
// rule of ParseTargetURL.
func splitRepoURL(s string) (owner, repo string, ok bool) {
	rest, ok := strings.CutPrefix(s, targetURLPrefix)
	if !ok {
		return "", "", false
	}
	owner, repo, ok = strings.Cut(rest, "/")
	if !ok || !ownerPattern.MatchString(owner) || !repoPattern.MatchString(repo) {
		return "", "", false
	}
	if strings.EqualFold(owner, "enterprises") || repo == "." || repo == ".." ||
		strings.HasSuffix(strings.ToLower(repo), ".git") {
		return "", "", false
	}
	return owner, repo, true
}

// ConfigURL is the URL the scale set library takes as its config URL (0002
// SPEC R239).
func (t Target) ConfigURL() string { return targetURLPrefix + t.FullName() }

// InstallationPath is the REST path of the App installation lookup (0002
// SPEC R236).
func (t Target) InstallationPath() string { return "repos/" + t.FullName() + "/installation" }

// RunnersPath is the REST path of the repository runner list (0002 SPEC
// R241, R242).
func (t Target) RunnersPath() string { return "repos/" + t.FullName() + "/actions/runners" }

// Permission returns the App permission the target needs and the access
// level it must have (0002 SPEC R238).
func (t Target) Permission() (name, level string) {
	return "administration", "write"
}

// Key is the name that tells targets apart: GitHub owner and repository
// names are case-insensitive, so it is the lower-case <owner>/<repo> (0002
// SPEC R233).
func (t Target) Key() string { return strings.ToLower(t.Owner) + "/" + strings.ToLower(t.Repo) }

// FullName is <owner>/<repo> as written.
func (t Target) FullName() string { return t.Owner + "/" + t.Repo }

// String describes the target in messages.
func (t Target) String() string { return "repository " + t.FullName() }

// valid reports whether t names a repository.
func (t Target) valid() bool { return t.Owner != "" && t.Repo != "" }
