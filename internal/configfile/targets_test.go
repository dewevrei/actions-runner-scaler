package configfile

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func targetsTestdata(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// targetsFixtureToken is shaped like a GitHub token and built at run time,
// so no token-shaped literal is committed.
func targetsFixtureToken() string {
	return "ghp_" + strings.Repeat("aB3dE5", 6)
}

// targetsCut returns s without the part from the start of from through the
// end of through, which follows it.
func targetsCut(t *testing.T, s, from, through string) string {
	t.Helper()
	start := strings.Index(s, from)
	last := strings.Index(s, through)
	if start < 0 || last < start {
		t.Fatalf("cannot cut %q through %q", from, through)
	}
	return s[:start] + s[last+len(through):]
}

const (
	targetsOne = "[[targets]]\nurl = \"https://github.com/a/one\"\nmax-runners = 1\n"
	targetsTwo = "[[targets]]\nurl = \"https://github.com/a/two\"\nmax-runners = 2\n"
)

func TestAddTargetPreservesBytes(t *testing.T) {
	data := targetsTestdata(t, "commented.toml")
	entry := TargetEntry{URL: "https://github.com/acme/new", MaxRunners: 2, Idle: intPtr(0), TokenFile: "/keys/new-token"}

	got, err := AddTarget(data, entry)
	if err != nil {
		t.Fatalf("AddTarget: %v", err)
	}
	if !bytes.HasPrefix(got, data) {
		t.Fatal("the result does not start with every original byte")
	}
	if want := string(data) + "\n" + TargetBlock(entry); string(got) != want {
		t.Fatalf("result =\n%s\nwant the original, one blank line and the block", got)
	}
}

func TestAddTargetFileEnds(t *testing.T) {
	entry := TargetEntry{URL: "https://github.com/a/new", MaxRunners: 1}
	block := TargetBlock(entry)
	tests := []struct {
		name string
		data string
		want string
	}{
		{"empty file", "", block},
		{"final newline", "[scaling]\nlabel = \"x\"\n", "[scaling]\nlabel = \"x\"\n\n" + block},
		{"no final newline", "[scaling]\nlabel = \"x\"", "[scaling]\nlabel = \"x\"\n\n" + block},
		{"final blank line", "[scaling]\nlabel = \"x\"\n\n", "[scaling]\nlabel = \"x\"\n\n\n" + block},
		{"carriage returns", "[scaling]\r\nlabel = \"x\"\r\n", "[scaling]\r\nlabel = \"x\"\r\n\n" + block},
		{"comment only", "# nothing here", "# nothing here\n\n" + block},
		{"no targets key", "[github]\ntoken-file = \"/t\"\n", "[github]\ntoken-file = \"/t\"\n\n" + block},
		{
			name: "values of other types",
			data: "when = 1979-05-27T07:32:00-08:00\nratio = 1.5\nlist = [1, \"a\"]\n\n[sub]\nflag = true\n",
			want: "when = 1979-05-27T07:32:00-08:00\nratio = 1.5\nlist = [1, \"a\"]\n\n[sub]\nflag = true\n\n" + block,
		},
		{"other targets", targetsOne, targetsOne + "\n" + block},
		{"last table is a targets subtable", targetsOne + "[targets.extra]\nx = 1\n", targetsOne + "[targets.extra]\nx = 1\n\n" + block},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := AddTarget([]byte(tt.data), entry)
			if err != nil {
				t.Fatalf("AddTarget: %v", err)
			}
			if string(got) != tt.want {
				t.Fatalf("result = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestRemoveTargetDeletesBlockOnly(t *testing.T) {
	data := string(targetsTestdata(t, "commented.toml"))
	tests := []struct {
		name string
		key  string
		want string
	}{
		{
			// The indented header with its comment, a comment inside the
			// block and the last key line go; the blank line and the
			// comments after the block stay.
			name: "middle",
			key:  "acme/web",
			want: targetsCut(t, data, "  [[targets]]  # second", "token-file = \"/keys/web-token\"\n"),
		},
		{
			name: "first",
			key:  "acme/api",
			want: targetsCut(t, data, "[[targets]]\nurl = \"https://github.com/acme/api\"", "idle = 1\n"),
		},
		{
			name: "last of the targets",
			key:  "acme/tools",
			want: targetsCut(t, data, "[[targets]]\nurl = \"https://github.com/acme/tools\"", "max-runners = 1\n"),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := RemoveTarget([]byte(data), tt.key)
			if err != nil {
				t.Fatalf("RemoveTarget: %v", err)
			}
			if string(got) != tt.want {
				t.Fatalf("result =\n%s\nwant\n%s", got, tt.want)
			}
		})
	}
}

func TestRemoveTargetShapes(t *testing.T) {
	tests := []struct {
		name string
		data string
		key  string
		want string
	}{
		{
			name: "url compared without case",
			data: "[[targets]]\nurl = \"https://github.com/Acme/API\"\nmax-runners = 1\n\n" + targetsTwo,
			key:  "acme/api",
			want: "\n" + targetsTwo,
		},
		{
			name: "block at the end without a final newline",
			data: targetsOne + "\n[[targets]]\nurl = \"https://github.com/a/two\"\nmax-runners = 2",
			key:  "a/two",
			want: targetsOne + "\n",
		},
		{
			name: "carriage returns",
			data: "[[targets]]\r\nurl = \"https://github.com/a/one\"\r\nmax-runners = 1\r\n\r\n[[targets]]\r\nurl = \"https://github.com/a/two\"\r\n",
			key:  "a/one",
			want: "\r\n[[targets]]\r\nurl = \"https://github.com/a/two\"\r\n",
		},
		{
			name: "comments between keys belong to the block",
			data: "[[targets]]\nurl = \"https://github.com/a/one\"\n# note\nmax-runners = 1\n# after\n\n" + targetsTwo,
			key:  "a/one",
			want: "# after\n\n" + targetsTwo,
		},
		{
			name: "a table after the block ends it",
			data: targetsOne + "[scaling]\nlabel = \"x\"\n\n" + targetsTwo,
			key:  "a/one",
			want: "[scaling]\nlabel = \"x\"\n\n" + targetsTwo,
		},
		{
			name: "another target's subtable stays with its target",
			data: targetsOne + "\n" + targetsTwo + "[targets.extra]\nx = 1\n",
			key:  "a/one",
			want: "\n" + targetsTwo + "[targets.extra]\nx = 1\n",
		},
		{
			name: "target with a url that is not a repository is left alone",
			data: "[[targets]]\nurl = \"not a url\"\n\n" + targetsOne + targetsTwo,
			key:  "a/two",
			want: "[[targets]]\nurl = \"not a url\"\n\n" + targetsOne,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := RemoveTarget([]byte(tt.data), tt.key)
			if err != nil {
				t.Fatalf("RemoveTarget: %v", err)
			}
			if string(got) != tt.want {
				t.Fatalf("result = %q, want %q", got, tt.want)
			}
		})
	}
}

// Removing what was just added leaves the original and the blank line the
// add put before the block.
func TestRemoveTargetUndoesAdd(t *testing.T) {
	data := targetsTestdata(t, "commented.toml")
	added, err := AddTarget(data, TargetEntry{URL: "https://github.com/acme/new", MaxRunners: 1})
	if err != nil {
		t.Fatal(err)
	}
	got, err := RemoveTarget(added, "acme/new")
	if err != nil {
		t.Fatalf("RemoveTarget: %v", err)
	}
	if want := string(data) + "\n"; string(got) != want {
		t.Fatalf("result differs from the original plus a blank line:\n%s", got)
	}
}

func TestAddTargetRefusals(t *testing.T) {
	token := targetsFixtureToken()
	entry := TargetEntry{URL: "https://github.com/a/new", MaxRunners: 1}
	tests := []struct {
		name  string
		data  string
		entry TargetEntry
		want  error // nil: a refusal with no reason of the package
	}{
		{"not TOML", "[scaling\nlabel = 1\n", entry, ErrNotTOML},
		{"not TOML with a token in it", "x = " + token + "\n", entry, ErrNotTOML},
		{"already present", targetsOne, TargetEntry{URL: "https://github.com/a/one", MaxRunners: 1}, ErrDuplicate},
		{"already present, case differs", targetsOne, TargetEntry{URL: "https://github.com/A/ONE", MaxRunners: 1}, ErrDuplicate},
		{"another repository listed twice", targetsOne + targetsOne, entry, ErrRepeated},
		{"repository listed twice, case differs", targetsOne + strings.ReplaceAll(targetsOne, "a/one", "A/One"), entry, ErrRepeated},
		{"the new repository listed twice", targetsOne + targetsOne, TargetEntry{URL: "https://github.com/a/one", MaxRunners: 1}, ErrRepeated},
		{"inline array", string(targetsTestdata(t, "inline-targets.toml")), entry, ErrShape},
		{"dotted keys", string(targetsTestdata(t, "dotted-targets.toml")), entry, ErrShape},
		{"empty inline array", "targets = []\n", entry, ErrShape},
		{"targets is a string", "targets = \"x\"\n", entry, ErrShape},
		{"targets is one table", "[targets]\nurl = \"https://github.com/a/one\"\n", entry, ErrShape},
		{"header line inside a string", "note = \"\"\"\n[[targets]]\n\"\"\"\n\n" + targetsOne, entry, ErrShape},
		{"entry URL with a credential", targetsOne, TargetEntry{URL: "https://" + token + "@github.com/a/b", MaxRunners: 1}, nil},
		{"entry URL not a repository", targetsOne, TargetEntry{URL: "https://github.com/a", MaxRunners: 1}, nil},
		{"entry token file not UTF-8", targetsOne, TargetEntry{URL: "https://github.com/a/b", MaxRunners: 1, TokenFile: "/t\xff"}, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := AddTarget([]byte(tt.data), tt.entry)
			if err == nil {
				t.Fatalf("AddTarget succeeded: %q", got)
			}
			if tt.want != nil && !errors.Is(err, tt.want) {
				t.Errorf("error %q is not %v", err, tt.want)
			}
			if tt.want == nil && errors.Is(err, ErrShape) {
				t.Errorf("error %q is a shape editRefusal, but the entry is at fault", err)
			}
			if strings.Contains(err.Error(), token) {
				t.Errorf("error %q holds a token", err)
			}
		})
	}
}

func TestRemoveTargetRefusals(t *testing.T) {
	token := targetsFixtureToken()
	tests := []struct {
		name string
		data string
		key  string
		want error
	}{
		{"not TOML", "[[targets\nurl = 1\n", "a/one", ErrNotTOML},
		{"not TOML with a token in it", "x = " + token + "\n", "a/one", ErrNotTOML},
		{"not in the file", targetsOne + targetsTwo, "a/three", ErrNotFound},
		{"no targets at all", "[scaling]\nlabel = \"x\"\n", "a/one", ErrNotFound},
		{"empty key", targetsOne + targetsTwo, "", ErrNotFound},
		{"the last target", targetsOne, "a/one", ErrLastTarget},
		{"the repository listed twice", targetsOne + targetsOne + targetsTwo, "a/one", ErrRepeated},
		{"another repository listed twice", targetsOne + targetsTwo + targetsTwo, "a/one", ErrRepeated},
		{"inline array", string(targetsTestdata(t, "inline-targets.toml")), "acme/api", ErrShape},
		{"dotted keys", string(targetsTestdata(t, "dotted-targets.toml")), "acme/api", ErrShape},
		{"header line inside a string", "note = \"\"\"\n[[targets]]\n\"\"\"\n\n" + targetsOne + targetsTwo, "a/one", ErrShape},
		// The note's second line looks like the next table, so the block
		// would end inside the string.
		{"multi-line string", string(targetsTestdata(t, "multiline-string.toml")), "acme/api", ErrShape},
		{"subtable of the block", targetsOne + "[targets.extra]\nx = 1\n\n" + targetsTwo, "a/one", ErrShape},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := RemoveTarget([]byte(tt.data), tt.key)
			if err == nil {
				t.Fatalf("RemoveTarget succeeded: %q", got)
			}
			if !errors.Is(err, tt.want) {
				t.Errorf("error %q is not %v", err, tt.want)
			}
			if strings.Contains(err.Error(), token) {
				t.Errorf("error %q holds a token", err)
			}
		})
	}
}

// The re-decode comparison refuses an edit that changes any value, also one
// outside the targets.
func TestRemoveTargetMultilineStringOtherBlock(t *testing.T) {
	data := targetsTestdata(t, "multiline-string.toml")
	got, err := RemoveTarget(data, "acme/web")
	if err != nil {
		t.Fatalf("RemoveTarget of the block after the string: %v", err)
	}
	if want := targetsCut(t, string(data), "[[targets]]\nurl = \"https://github.com/acme/web\"", "max-runners = 3\n"); string(got) != want {
		t.Fatalf("result = %q, want %q", got, want)
	}
}

func TestVerifiedRefusesAnyDifference(t *testing.T) {
	doc, targets, err := parseTargets([]byte(targetsOne + "[scaling]\nlabel = \"x\"\n"))
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name   string
		edited string
		ok     bool
	}{
		{"same", targetsOne + "[scaling]\nlabel = \"x\"\n", true},
		{"a value outside the targets changed", targetsOne + "[scaling]\nlabel = \"y\"\n", false},
		{"a key outside the targets removed", targetsOne, false},
		{"a target value changed", strings.Replace(targetsOne, "= 1", "= 2", 1) + "[scaling]\nlabel = \"x\"\n", false},
		{"a target key added", targetsOne + "idle = 0\n[scaling]\nlabel = \"x\"\n", false},
		{"a target added", targetsOne + targetsTwo + "[scaling]\nlabel = \"x\"\n", false},
		{"not TOML", "[scaling\n", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := verifiedEdit([]byte(tt.edited), doc, targets)
			if (err == nil) != tt.ok {
				t.Fatalf("verifiedEdit error = %v, want success %v", err, tt.ok)
			}
			if err != nil && !errors.Is(err, ErrShape) {
				t.Errorf("error %q is not a shape editRefusal", err)
			}
		})
	}
}
