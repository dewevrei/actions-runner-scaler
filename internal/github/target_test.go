package github

import "testing"

// testTarget is the repository the 0002 tests use.
var testTarget = Target{Owner: testOwner, Repo: testRepo}

// targetURLCases are target URLs and whether ParseTargetURL accepts them
// (0002 SPEC R231). The config package's TestValidateTargetURL holds the
// same rows.
var targetURLCases = []struct {
	in   string
	want bool
}{
	{"https://github.com/acme-1/repo.name_x-1", true},
	{"https://github.com/Acme/Widget", true},
	{"https://github.com/a/b", true},
	{"https://github.com/acme/.github", true},
	{"https://github.com/acme/x.gitx", true},
	{"https://github.com/acme/...", true},
	{"https://github.com/acme", false},
	{"https://github.com/Acme-1", false},
	{"https://github.com/enterprises/x", false},
	{"https://github.com/Enterprises/x", false},
	{"https://github.com/acme/widget/", false},
	{"https://github.com/acme/widget.git", false},
	{"https://github.com/acme/widget.GIT", false},
	{"https://github.com/acme/.git", false},
	{"https://github.com/acme/.", false},
	{"https://github.com/acme/..", false},
	{"https://github.com/acme/widget/tree", false},
	{"https://github.com/acme/widget?x=1", false},
	{"https://github.com/acme/widget#frag", false},
	{"http://github.com/acme/widget", false},
	{"https://github.com//widget", false},
	{"https://github.com/acme/", false},
	{"https://github.com/ac_me/widget", false},
	{"https://github.com/acme/wid get", false},
	{"https://github.com/acme/wid/get", false},
	{"", false},
	{"https://github.com", false},
	{"https://github.com/", false},
	{"https://example.com/acme/widget", false},
	{"https://www.github.com/acme/widget", false},
	{"HTTPS://github.com/acme/widget", false},
	{" https://github.com/acme/widget", false},
	{"github.com/acme/widget", false},
	{"https://user@github.com/acme/widget", false},
}

// TestParseTargetURL proves 0002 SPEC R231.
func TestParseTargetURL(t *testing.T) {
	for _, c := range targetURLCases {
		got, err := ParseTargetURL(c.in)
		if (err == nil) != c.want {
			t.Errorf("ParseTargetURL(%q) = %+v, %v; want accepted %v", c.in, got, err, c.want)
			continue
		}
		if err != nil {
			want := `github: target URL "` + c.in + `" is not https://github.com/<owner>/<repo>`
			if err.Error() != want {
				t.Errorf("ParseTargetURL(%q) error = %q, want %q", c.in, err, want)
			}
			continue
		}
		if got.ConfigURL() != c.in {
			t.Errorf("ParseTargetURL(%q).ConfigURL() = %q, want the input", c.in, got.ConfigURL())
		}
	}
	got, err := ParseTargetURL("https://github.com/acme-1/repo.name_x-1")
	if err != nil || got != (Target{Owner: "acme-1", Repo: "repo.name_x-1"}) {
		t.Errorf("ParseTargetURL = %+v, %v; want owner acme-1, repo repo.name_x-1", got, err)
	}
}

// TestTargetPaths proves 0002 SPEC R236, R238, R239, R241.
func TestTargetPaths(t *testing.T) {
	tg, err := ParseTargetURL("https://github.com/Acme/Widget")
	if err != nil {
		t.Fatal(err)
	}
	if got := tg.ConfigURL(); got != "https://github.com/Acme/Widget" {
		t.Errorf("ConfigURL = %q", got)
	}
	if got := tg.InstallationPath(); got != "repos/Acme/Widget/installation" {
		t.Errorf("InstallationPath = %q", got)
	}
	if got := tg.RunnersPath(); got != "repos/Acme/Widget/actions/runners" {
		t.Errorf("RunnersPath = %q", got)
	}
	if name, level := tg.Permission(); name != "administration" || level != "write" {
		t.Errorf("Permission = %q, %q", name, level)
	}
	if got := tg.FullName(); got != "Acme/Widget" {
		t.Errorf("FullName = %q", got)
	}
	if got := tg.String(); got != "repository Acme/Widget" {
		t.Errorf("String = %q", got)
	}
}

// TestTargetKey proves 0002 SPEC R233: names that differ only in case have
// one key.
func TestTargetKey(t *testing.T) {
	a := Target{Owner: "Acme", Repo: "Repo"}
	b := Target{Owner: "acme", Repo: "repo"}
	if a.Key() != "acme/repo" || b.Key() != "acme/repo" {
		t.Errorf("keys = %q, %q; want acme/repo for both", a.Key(), b.Key())
	}
	if c := (Target{Owner: "acme", Repo: "other"}); c.Key() == a.Key() {
		t.Errorf("different repositories share key %q", c.Key())
	}
}
