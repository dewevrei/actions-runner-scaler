package github

import "testing"

// testTarget is the organization the 0002 tests use.
var testTarget = Target{Kind: Organization, Name: testOwner}

func TestParseTargetURL(t *testing.T) {
	good := map[string]string{
		"https://github.com/test-org": "test-org",
		"https://github.com/Acme-1":   "Acme-1",
		"https://github.com/a":        "a",
	}
	for in, name := range good {
		got, err := ParseTargetURL(in)
		if err != nil {
			t.Errorf("ParseTargetURL(%q): %v", in, err)
			continue
		}
		if got != (Target{Kind: Organization, Name: name}) {
			t.Errorf("ParseTargetURL(%q) = %+v, want organization %q", in, got, name)
		}
	}
	bad := []string{
		"",
		"https://github.com",
		"https://github.com/",
		"https://github.com/org/",
		"https://github.com/org/repo",
		"https://github.com/org.git",
		"https://github.com/org?x=1",
		"https://github.com/org#frag",
		"https://github.com/org name",
		"https://github.com/org_name",
		"http://github.com/org",
		"https://example.com/org",
		"https://www.github.com/org",
		"HTTPS://github.com/org",
		" https://github.com/org",
		"github.com/org",
		"https://user@github.com/org",
	}
	for _, in := range bad {
		if got, err := ParseTargetURL(in); err == nil {
			t.Errorf("ParseTargetURL(%q) = %+v, want an error", in, got)
		}
	}
}

func TestTargetPaths(t *testing.T) {
	tg, err := ParseTargetURL("https://github.com/Test-Org")
	if err != nil {
		t.Fatal(err)
	}
	if got := tg.ConfigURL(); got != "https://github.com/Test-Org" {
		t.Errorf("ConfigURL = %q", got)
	}
	if got := tg.InstallationPath(); got != "orgs/Test-Org/installation" {
		t.Errorf("InstallationPath = %q", got)
	}
	if got := tg.RunnersPath(); got != "orgs/Test-Org/actions/runners" {
		t.Errorf("RunnersPath = %q", got)
	}
	if name, level := tg.Permission(); name != "organization_self_hosted_runners" || level != "write" {
		t.Errorf("Permission = %q, %q", name, level)
	}
	if got := tg.Key(); got != "test-org" {
		t.Errorf("Key = %q, want the lower-case name", got)
	}
	if got := tg.String(); got != "organization Test-Org" {
		t.Errorf("String = %q", got)
	}
	other := Target{Kind: Organization, Name: "TEST-ORG"}
	if tg.Key() != other.Key() {
		t.Errorf("keys differ for names that differ in case: %q, %q", tg.Key(), other.Key())
	}
}
