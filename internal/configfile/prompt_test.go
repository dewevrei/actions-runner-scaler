package configfile

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/dewevrei/actions-runner-scaler/internal/config"
)

const promptRunnerID = "sha256:abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789"

// promptTokenValue is a value shaped like a classic token, built here so no
// token-shaped literal is committed.
func promptTokenValue() string { return "ghp_" + strings.Repeat("aB3dE5", 6) }

// runPrompter fills v from input and returns what it wrote.
func runPrompter(v *Values, input string) (out string, err error) {
	var w bytes.Buffer
	err = NewPrompter(strings.NewReader(input), &w).Fill(v)
	return w.String(), err
}

// appAnswers answer the App branch with one repository.
func appAnswers(f renderFiles) string {
	return strings.Join([]string{
		"app", "Iv1.0123456789abcdef", f.key,
		"https://github.com/acme/widget", "",
		"",
		promptRunnerID,
	}, "\n") + "\n"
}

// 0003 SPEC R285: the questions come in order and the answers fill v.
func TestPrompterAppBranch(t *testing.T) {
	f := newRenderFiles(t)
	var v Values
	out, err := runPrompter(&v, appAnswers(f))
	if err != nil {
		t.Fatalf("Fill: %v\n%s", err, out)
	}
	want := Values{
		AppClientID: "Iv1.0123456789abcdef", AppPrivateKeyFile: f.key, RunnerImage: promptRunnerID,
		Targets: []TargetEntry{{URL: "https://github.com/acme/widget", MaxRunners: 1}},
	}
	if !reflect.DeepEqual(v, want) {
		t.Errorf("v = %+v, want %+v", v, want)
	}
	assertInOrder(t, out, promptCredential, noteAppLink, promptClientID, promptKeyPath, promptRepository, promptMax, promptRepository, promptRunner)
	if strings.Contains(out, promptTokenPath) {
		t.Errorf("the App branch asked for a token file:\n%s", out)
	}
}

func TestPrompterTokenBranch(t *testing.T) {
	f := newRenderFiles(t)
	var v Values
	input := strings.Join([]string{"token", f.token, "https://github.com/acme/widget", "3", "https://github.com/acme/gadget", "", "", promptRunnerID}, "\n") + "\n"
	out, err := runPrompter(&v, input)
	if err != nil {
		t.Fatalf("Fill: %v\n%s", err, out)
	}
	want := Values{
		TokenFile: f.token, RunnerImage: promptRunnerID,
		Targets: []TargetEntry{
			{URL: "https://github.com/acme/widget", MaxRunners: 3},
			{URL: "https://github.com/acme/gadget", MaxRunners: 1},
		},
	}
	if !reflect.DeepEqual(v, want) {
		t.Errorf("v = %+v, want %+v", v, want)
	}
	assertInOrder(t, out, promptCredential, promptTokenPath, promptRepository, promptMax, promptRepository, promptMax, promptRepository, promptRunner)
	for _, asked := range []string{promptClientID, promptKeyPath, noteAppLink} {
		if strings.Contains(out, asked) {
			t.Errorf("the token branch wrote %q:\n%s", asked, out)
		}
	}
}

// assertInOrder checks that each of parts appears in out after the one
// before it.
func assertInOrder(t *testing.T, out string, parts ...string) {
	t.Helper()
	rest := out
	for _, p := range parts {
		i := strings.Index(rest, p)
		if i < 0 {
			t.Errorf("output lacks %q after the earlier questions:\n%s", p, out)
			return
		}
		rest = rest[i+len(p):]
	}
}

// Only the values the flags left out are asked for (0003 SPEC R285).
func TestPrompterAsksOnlyGaps(t *testing.T) {
	f := newRenderFiles(t)
	repo := []TargetEntry{{URL: "https://github.com/acme/widget", MaxRunners: 2}}
	tests := []struct {
		name  string
		v     Values
		input string
		asked []string
		not   []string
		want  Values
	}{
		{
			"everything but the image",
			Values{TokenFile: f.token, Targets: repo},
			promptRunnerID + "\n",
			[]string{promptRunner},
			[]string{promptCredential, promptTokenPath, promptRepository},
			Values{TokenFile: f.token, Targets: repo, RunnerImage: promptRunnerID},
		},
		{
			"client ID given",
			Values{AppClientID: "Iv1.x", Targets: repo, RunnerImage: promptRunnerID},
			f.key + "\n",
			[]string{promptKeyPath},
			[]string{promptCredential, promptClientID, promptRepository, promptRunner},
			Values{AppClientID: "Iv1.x", AppPrivateKeyFile: f.key, Targets: repo, RunnerImage: promptRunnerID},
		},
		{
			"key file given",
			Values{AppPrivateKeyFile: f.key, Targets: repo, RunnerImage: promptRunnerID},
			"Iv1.x\n",
			[]string{promptClientID},
			[]string{promptCredential, promptKeyPath},
			Values{AppClientID: "Iv1.x", AppPrivateKeyFile: f.key, Targets: repo, RunnerImage: promptRunnerID},
		},
		{
			"repositories only",
			Values{TokenFile: f.token, RunnerImage: promptRunnerID},
			"https://github.com/acme/widget\n2\n\n",
			[]string{promptRepository, promptMax},
			[]string{promptCredential, promptRunner},
			Values{TokenFile: f.token, Targets: repo, RunnerImage: promptRunnerID},
		},
		{
			"nothing missing",
			Values{TokenFile: f.token, Targets: repo, RunnerImage: promptRunnerID},
			"",
			nil,
			[]string{promptCredential, promptRepository, promptRunner},
			Values{TokenFile: f.token, Targets: repo, RunnerImage: promptRunnerID},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			v := tt.v
			out, err := runPrompter(&v, tt.input)
			if err != nil {
				t.Fatalf("Fill: %v\n%s", err, out)
			}
			if !reflect.DeepEqual(v, tt.want) {
				t.Errorf("v = %+v, want %+v", v, tt.want)
			}
			for _, q := range tt.asked {
				if !strings.Contains(out, q) {
					t.Errorf("output lacks %q:\n%s", q, out)
				}
			}
			for _, q := range tt.not {
				if strings.Contains(out, q) {
					t.Errorf("output has %q, which the flags answered:\n%s", q, out)
				}
			}
		})
	}
}

// A bad answer prints its error and the same question again (0003 SPEC
// R285).
func TestPrompterRetry(t *testing.T) {
	f := newRenderFiles(t)
	wide := filepath.Join(t.TempDir(), "wide")
	if err := os.WriteFile(wide, []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(wide, 0o644); err != nil {
		t.Fatal(err)
	}
	repo := []TargetEntry{{URL: "https://github.com/a/b", MaxRunners: 1}}
	tests := []struct {
		name     string
		v        Values
		input    []string
		question string
		wantErr  string
	}{
		{"bad URL", Values{TokenFile: f.token, RunnerImage: promptRunnerID},
			[]string{"https://github.com/acme", "https://github.com/acme/widget", "", ""},
			promptRepository, "organization or account"},
		{"credential URL", Values{TokenFile: f.token, RunnerImage: promptRunnerID},
			[]string{"https://x@github.com/acme/widget", "https://github.com/acme/widget", "", ""},
			promptRepository, "value contains a credential"},
		{"relative key path", Values{AppClientID: "x", Targets: repo, RunnerImage: promptRunnerID},
			[]string{"app.pem", f.key},
			promptKeyPath, "must be an absolute path"},
		{"wide token file", Values{Targets: repo, RunnerImage: promptRunnerID},
			[]string{"token", wide, f.token},
			promptTokenPath, "wider than 0600"},
		{"bad image", Values{TokenFile: f.token, Targets: repo},
			[]string{"Runner:latest", promptRunnerID},
			promptRunner, "must be an image ID or an image reference <name>[:<tag>][@<digest>]"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			v := tt.v
			out, err := runPrompter(&v, strings.Join(tt.input, "\n")+"\n")
			if err != nil {
				t.Fatalf("Fill: %v\n%s", err, out)
			}
			// The error comes before the repeated question.
			assertInOrder(t, out, tt.question, tt.wantErr, tt.question)
			if g := Gaps(v); len(g) != 0 {
				t.Errorf("gaps left: %v", g)
			}
		})
	}
}

// A repeated repository, and an empty list, are refused.
func TestPrompterRepositories(t *testing.T) {
	v := Values{TokenFile: "/t", RunnerImage: promptRunnerID}
	input := strings.Join([]string{
		"", // the list cannot be empty
		"https://github.com/acme/widget", "",
		"https://github.com/ACME/Widget",      // the same repository
		"https://github.com/acme/gadget", "x", // bad number
		"0", "5",
		"",
	}, "\n") + "\n"
	out, err := runPrompter(&v, input)
	if err != nil {
		t.Fatalf("Fill: %v\n%s", err, out)
	}
	want := []TargetEntry{{URL: "https://github.com/acme/widget", MaxRunners: 1}, {URL: "https://github.com/acme/gadget", MaxRunners: 5}}
	if !reflect.DeepEqual(v.Targets, want) {
		t.Errorf("targets = %+v, want %+v", v.Targets, want)
	}
	for _, msg := range []string{msgNeedRepo, "listed more than once", "max-runners must be an integer of 1 or more"} {
		if !strings.Contains(out, msg) {
			t.Errorf("output lacks %q:\n%s", msg, out)
		}
	}
}

// Input that ends before the last answer returns io.EOF at any question and
// leaves the answers it did get.
func TestPrompterEOF(t *testing.T) {
	f := newRenderFiles(t)
	full := appAnswers(f)
	lines := strings.SplitAfter(full, "\n")
	lines = lines[:len(lines)-1] // SplitAfter leaves an empty tail
	for n := 0; n < len(lines); n++ {
		input := strings.Join(lines[:n], "")
		var v Values
		_, err := runPrompter(&v, input)
		if err != io.EOF {
			t.Errorf("input of %d lines: err = %v, want io.EOF", n, err)
		}
	}
}

// A last answer with no line break still counts.
func TestPrompterLastLineWithoutNewline(t *testing.T) {
	v := Values{TokenFile: "/t", Targets: []TargetEntry{{URL: "https://github.com/acme/widget", MaxRunners: 1}}}
	if _, err := runPrompter(&v, promptRunnerID); err != nil {
		t.Fatalf("Fill: %v", err)
	}
	if v.RunnerImage != promptRunnerID {
		t.Errorf("RunnerImage = %q", v.RunnerImage)
	}
}

// A line ending in CRLF and spaces around an answer are ignored.
func TestPrompterTrimsAnswers(t *testing.T) {
	v := Values{TokenFile: "/t", Targets: []TargetEntry{{URL: "https://github.com/acme/widget", MaxRunners: 1}}}
	if _, err := runPrompter(&v, "  "+promptRunnerID+"  \r\n"); err != nil {
		t.Fatalf("Fill: %v", err)
	}
	if v.RunnerImage != promptRunnerID {
		t.Errorf("RunnerImage = %q", v.RunnerImage)
	}
}

type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, errors.New("terminal gone") }

func TestPrompterReadError(t *testing.T) {
	var w bytes.Buffer
	v := Values{TokenFile: "/t", Targets: []TargetEntry{{URL: "https://github.com/acme/widget", MaxRunners: 1}}}
	err := NewPrompter(failingReader{}, &w).Fill(&v)
	if err == nil || err == io.EOF || !strings.Contains(err.Error(), "terminal gone") {
		t.Errorf("Fill = %v, want the read error", err)
	}
}

// A token typed where a path or a URL is asked is never printed back (0003
// SPEC R277).
func TestPrompterNeverEchoesCredentials(t *testing.T) {
	token := promptTokenValue()
	f := newRenderFiles(t)
	input := strings.Join([]string{
		"token", token, f.token, // a token pasted as a path (R276)
		"https://" + token + "@github.com/acme/widget", // a credential in a URL (R314)
		token + "/acme/widget",
		"https://github.com/acme/widget", "", "",
		promptRunnerID,
	}, "\n") + "\n"
	var v Values
	out, err := runPrompter(&v, input)
	if err != nil {
		t.Fatalf("Fill: %v\n%s", err, out)
	}
	if strings.Contains(out, token) {
		t.Errorf("output holds the token:\n%s", out)
	}
	for _, msg := range []string{"value looks like a token, not a file path", "value contains a credential"} {
		if !strings.Contains(out, msg) {
			t.Errorf("output lacks %q:\n%s", msg, out)
		}
	}
	// A token as a client ID is refused too.
	app := Values{AppPrivateKeyFile: f.key, Targets: []TargetEntry{{URL: "https://github.com/a/b", MaxRunners: 1}}, RunnerImage: promptRunnerID}
	out, err = runPrompter(&app, token+"\nIv1.x\n")
	if err != nil || strings.Contains(out, token) || !strings.Contains(out, "looks like a token") {
		t.Errorf("client ID answer: %v\n%s", err, out)
	}
}

func TestGaps(t *testing.T) {
	repo := []TargetEntry{{URL: "https://github.com/a/b", MaxRunners: 1}}
	tests := []struct {
		name string
		v    Values
		want []string // flags
	}{
		{"all missing", Values{}, []string{"-app-client-id and -app-private-key-file, or -token-file", "-repo <URL>[=<max-runners>]", "-runner-image"}},
		{"complete App", Values{AppClientID: "x", AppPrivateKeyFile: "/k", Targets: repo, RunnerImage: "i"}, nil},
		{"complete token", Values{TokenFile: "/t", Targets: repo, RunnerImage: "i"}, nil},
		{"client ID only", Values{AppClientID: "x", Targets: repo, RunnerImage: "i"}, []string{"-app-private-key-file"}},
		{"key file only", Values{AppPrivateKeyFile: "/k", Targets: repo, RunnerImage: "i"}, []string{"-app-client-id"}},
		{"no repository", Values{TokenFile: "/t", RunnerImage: "i"}, []string{"-repo <URL>[=<max-runners>]"}},
		{"no image", Values{TokenFile: "/t", Targets: repo}, []string{"-runner-image"}},
		{"buildkit image is never a gap", Values{TokenFile: "/t", Targets: repo, RunnerImage: "i"}, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got []string
			for _, g := range Gaps(tt.v) {
				got = append(got, g.Flag)
				if g.What == "" {
					t.Errorf("gap %q has no description", g.Flag)
				}
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("Gaps flags = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestChecks(t *testing.T) {
	f := newRenderFiles(t)
	wide := filepath.Join(t.TempDir(), "wide.pem")
	if err := os.WriteFile(wide, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(wide, 0o644); err != nil {
		t.Fatal(err)
	}
	token := promptTokenValue()
	tests := []struct {
		name    string
		err     error
		wantErr string // empty: no error
	}{
		{"client ID", CheckAppClientID("Iv1.0123456789abcdef"), ""},
		{"empty client ID", CheckAppClientID(""), "must not be empty"},
		{"client ID with space", CheckAppClientID("Iv1 x"), "spaces"},
		{"client ID token", CheckAppClientID(token), "looks like a token"},
		{"client ID invalid UTF-8", CheckAppClientID("\xff"), "UTF-8"},
		{"key file", CheckAppKeyPath(f.key), ""},
		{"relative key file", CheckAppKeyPath("app.pem"), "absolute"},
		{"wide key file", CheckAppKeyPath(wide), "wider than 0600"},
		{"pasted key", CheckAppKeyPath("-----BEGIN RSA PRIVATE KEY-----\nabc"), "looks like key content"},
		{"token file", CheckTokenPath(f.token), ""},
		{"relative token file", CheckTokenPath("token"), "absolute"},
		{"pasted token", CheckTokenPath(token), "looks like a token"},
		{"runner image", CheckRunnerImage(promptRunnerID), ""},
		{"upper case runner image", CheckRunnerImage("Runner:latest"), "must be an image ID or an image reference <name>[:<tag>][@<digest>]"},
		{"buildkit image", CheckBuildKitImage(renderBuildKitImage), ""},
		{"empty buildkit image", CheckBuildKitImage(""), "must be an image ID or an image reference <name>[:<tag>][@<digest>]"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			switch {
			case tt.wantErr == "" && tt.err != nil:
				t.Errorf("error = %v, want none", tt.err)
			case tt.wantErr != "" && (tt.err == nil || !strings.Contains(tt.err.Error(), tt.wantErr)):
				t.Errorf("error = %v, want one with %q", tt.err, tt.wantErr)
			}
			if tt.err != nil && strings.Contains(tt.err.Error(), token) {
				t.Errorf("error holds the token: %v", tt.err)
			}
		})
	}
}

// TestCheckImageAnswers proves 0003 SPEC R318, R285 and R322: both checks
// accept every value of an image key that config accepts, and refuse the
// others, without quoting the value.
func TestCheckImageAnswers(t *testing.T) {
	hex64 := strings.Repeat("c", 64)
	accepted := []string{
		"sha256:" + hex64, hex64,
		"actions-runner-scaler-runner:local", "busybox", "busybox:1",
		"moby/buildkit:v0.35.1-rootless@sha256:" + hex64,
		"moby/buildkit@sha256:" + hex64,
		"localhost:5000/a/b:1", "sha256:abc",
		renderRunnerImage, renderBuildKitImage,
	}
	refused := []string{"", "Runner:latest", "a b", ":tag", "a@b"}
	checks := map[string]func(string) error{"runner": CheckRunnerImage, "buildkit": CheckBuildKitImage}
	for name, check := range checks {
		for _, value := range accepted {
			if err := check(value); err != nil {
				t.Errorf("%s: %q refused: %v", name, value, err)
			}
			if err := config.CheckImage(value); err != nil {
				t.Errorf("config refuses %q: %v", value, err)
			}
		}
		for _, value := range refused {
			err := check(value)
			if err == nil {
				t.Errorf("%s: %q accepted", name, value)
				continue
			}
			if value != "" && strings.Contains(err.Error(), value) {
				t.Errorf("%s: the error quotes %q: %v", name, value, err)
			}
		}
	}
}

// The image rules here and config's agree.
func TestImageChecksMatchConfig(t *testing.T) {
	f := newRenderFiles(t)
	values := []string{
		promptRunnerID, renderBuildKitImage, "", "ubuntu:latest",
		"sha256:" + strings.Repeat("A", 64), "sha256:" + strings.Repeat("a", 63), "sha256:" + strings.Repeat("a", 65),
		"moby/buildkit:v0.35.1-rootless@sha256:" + strings.Repeat("a", 64),
		"moby/buildkit:v0.35.1@sha256:" + strings.Repeat("a", 64),
		"moby/buildkit:-x-rootless@sha256:" + strings.Repeat("a", 64),
		"moby/buildkit:v0.35.1-rootless",
		"other/buildkit:v1-rootless@sha256:" + strings.Repeat("a", 64),
	}
	for _, value := range values {
		for _, field := range []string{"runner", "buildkit"} {
			v := Values{AppClientID: "x", AppPrivateKeyFile: f.key, RunnerImage: renderRunnerImage, BuildKitImage: renderBuildKitImage,
				Targets: []TargetEntry{{URL: "https://github.com/acme/widget", MaxRunners: 1}}}
			check := CheckRunnerImage
			if field == "runner" {
				v.RunnerImage = value
			} else {
				v.BuildKitImage = value
				check = CheckBuildKitImage
			}
			_, err := config.Decode(Render(v))
			configAccepts := err == nil
			if got := check(value) == nil; got != configAccepts {
				t.Errorf("%s.image %q: configfile accepts %v, config accepts %v (%v)", field, value, got, configAccepts, err)
			}
		}
	}
}

func TestCheckRepository(t *testing.T) {
	taken := []TargetEntry{{URL: "https://github.com/acme/widget", MaxRunners: 1}}
	token := promptTokenValue()
	tests := []struct {
		url     string
		wantErr string
	}{
		{"https://github.com/acme/gadget", ""},
		{"https://github.com/acme", "organization or account"},
		{"https://github.com/acme/widget/", `must be https://github.com/<owner>/<repo>`},
		{"https://github.com/Acme/Widget", "listed more than once"},
		{"https://user@github.com/acme/gadget", "value contains a credential; set the repository URL only"},
		{"https://github.com/acme/" + token, "value contains a credential; set the repository URL only"},
	}
	for _, tt := range tests {
		err := CheckRepository(taken, tt.url)
		switch {
		case tt.wantErr == "" && err != nil:
			t.Errorf("CheckRepository(%q) = %v, want nil", tt.url, err)
		case tt.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tt.wantErr)):
			t.Errorf("CheckRepository(%q) = %v, want %q", tt.url, err, tt.wantErr)
		}
		if err != nil && (strings.Contains(err.Error(), "@") || strings.Contains(err.Error(), token)) {
			t.Errorf("CheckRepository(%q) quotes the credential: %v", tt.url, err)
		}
	}
}

func TestParseMaxRunners(t *testing.T) {
	tests := []struct {
		in   string
		want int
		ok   bool
	}{
		{"1", 1, true}, {"12", 12, true}, {"0", 0, false}, {"-1", 0, false}, {"x", 0, false},
		{"", 0, false}, {"1.5", 0, false}, {"99999999999999999999", 0, false},
	}
	for _, tt := range tests {
		got, err := ParseMaxRunners(tt.in)
		if (err == nil) != tt.ok || got != tt.want {
			t.Errorf("ParseMaxRunners(%q) = %d, %v; want %d, ok=%v", tt.in, got, err, tt.want, tt.ok)
		}
	}
	// The error never quotes its input, which may be a pasted secret.
	if _, err := ParseMaxRunners("secret-value"); err == nil || strings.Contains(err.Error(), "secret-value") {
		t.Errorf("ParseMaxRunners error = %v", err)
	}
}
