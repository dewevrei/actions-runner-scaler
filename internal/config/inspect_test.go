package config

import (
	"bytes"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// targetTOML returns a [[targets]] entry of url with the given extra lines.
func targetTOML(url, lines string) string {
	return "\n[[targets]]\nurl = \"" + url + "\"\n" + lines
}

// tokenLine returns a token-file line for path.
func tokenLine(path string) string {
	return `token-file = "` + path + `"` + "\n"
}

// TestInspectSeparatesFiles proves 0003 SPEC R304 and R271: key and token
// file results are in Files by key, every other error in Problems, Failed
// names what failed, and Load joins them in the 0002 order.
func TestInspectSeparatesFiles(t *testing.T) {
	badKey := writeKey(t, []byte("garbage"), 0o644)
	content := withRunner(requiredTOML(badKey), "memory = \"1x\"\n") +
		tokenLine("pat.txt") +
		targetTOML("https://github.com/acme/second",
			tokenLine(writeToken(t, fixtureToken("github_pat_"), 0o600))+"max-runners = \"two\"\n")
	content = strings.Replace(content, `image = "`+testRunnerImage+`"`, `image = "Bad"`, 1)
	path := writeFile(t, "config.toml", []byte(content), 0o600)

	in, err := Inspect(path)
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	if in.Config == nil || len(in.Config.Targets) != 2 {
		t.Fatalf("Inspect config = %+v, want two targets", in.Config)
	}

	type fileResult struct{ key, err string }
	var files []fileResult
	for _, f := range in.Files {
		r := fileResult{key: f.Key}
		if f.Err != nil {
			r.err = f.Err.Error()
		}
		files = append(files, r)
	}
	wantFiles := []fileResult{
		{"github.app-private-key-file", "file mode -rw-r--r-- is wider than 0600 (group or other bits are set); use 0600 or 0400\nno PEM block found"},
		{"targets[0].token-file", "must be an absolute path"},
		{"targets[1].token-file", ""},
	}
	if !slices.Equal(files, wantFiles) {
		t.Errorf("Files:\n got %q\nwant %q", files, wantFiles)
	}

	problems := errorTexts(errors.Join(in.Problems...))
	// Decoding problems come table by table in name order.
	wantProblems := []string{
		"runner.memory: invalid size",
		"config <path>: targets[1].max-runners: ",
		"runner.image: must be an image ID or an image reference",
	}
	if len(problems) != len(wantProblems) {
		t.Fatalf("Problems = %q, want %d", problems, len(wantProblems))
	}
	for i, w := range wantProblems {
		if got := strings.ReplaceAll(problems[i], path, "<path>"); !strings.HasPrefix(got, w) {
			t.Errorf("problem %d = %q, want it to start with %q", i, got, w)
		}
	}

	for key, want := range map[string]bool{
		"targets[1].max-runners":      true,
		"runner.memory":               true,
		"runner.image":                true,
		"github.app-private-key-file": true,
		"targets[0].token-file":       true,
		"targets[1].token-file":       false,
		"targets[0].url":              false,
		"scaling.label":               false,
	} {
		if got := in.Failed(key); got != want {
			t.Errorf("Failed(%q) = %v, want %v", key, got, want)
		}
	}

	// Load: decoding problems, then validation in the 0002 order, the key
	// file errors at the App's place.
	_, loadErr := Load(path)
	want := []string{
		problems[0], problems[1],
		"github.app-private-key-file: file mode -rw-r--r-- is wider than 0600 (group or other bits are set); use 0600 or 0400",
		"github.app-private-key-file: no PEM block found",
		problems[2],
		"targets[0].token-file: must be an absolute path",
	}
	if got := errorTexts(loadErr); !slices.Equal(got, want) {
		t.Errorf("Load errors:\n got %q\nwant %q", got, want)
	}
}

// TestInspectFailedTable proves that Failed reports the keys inside a table
// that failed to decode, and that no file of that table is checked.
func TestInspectFailedTable(t *testing.T) {
	in, err := Inspect(writeFile(t, "config.toml", []byte("github = 5\n"+githubTOML("")), 0o600))
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	if !in.Failed("github") || !in.Failed("github.token-file") {
		t.Errorf("Failed(github) = %v, Failed(github.token-file) = %v; want both true", in.Failed("github"), in.Failed("github.token-file"))
	}
	if len(in.Files) != 0 {
		t.Errorf("Files = %v, want none", in.Files)
	}
	if got := errorTexts(errors.Join(in.Problems...)); len(got) != 1 || !strings.Contains(got[0], ": github: ") {
		t.Errorf("Problems = %q, want only the type error of github", got)
	}
}

// TestInspectBlanksRefusedValues proves 0003 SPEC R277 for Inspection.Config:
// values refused by R276 and R314 are blank, their keys are Failed, and the
// other values stay.
func TestInspectBlanksRefusedValues(t *testing.T) {
	defaultToken, targetToken := fixtureToken("ghp_"), fixtureToken("github_pat_")
	credentialURL := "https://github.com/acme/" + fixtureToken("ghs_")
	content := strings.Replace(githubTOML(tokenLine(defaultToken)), "https://github.com/acme/widget", credentialURL, 1) +
		targetTOML("https://github.com/acme/second", tokenLine(targetToken))
	in, err := Inspect(writeFile(t, "config.toml", []byte(content), 0o600))
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	c := in.Config
	if len(c.Targets) != 2 {
		t.Fatalf("targets = %d, want 2", len(c.Targets))
	}
	if c.GitHub.TokenFile != "" || c.Targets[0].URL != "" || c.Targets[1].TokenFile != "" {
		t.Error("a refused value is still in the configuration")
	}
	if c.Targets[1].URL != "https://github.com/acme/second" {
		t.Errorf("targets[1].url = %q, want it kept", c.Targets[1].URL)
	}
	for _, key := range []string{"github.token-file", "targets[0].url", "targets[1].token-file"} {
		if !in.Failed(key) {
			t.Errorf("Failed(%q) = false, want true", key)
		}
	}
	printed := fmt.Sprintf("%+v %#v", *c, *c)
	for _, s := range []string{defaultToken, targetToken, credentialURL} {
		if strings.Contains(printed, s) {
			t.Errorf("the printed configuration holds a refused value: %s", printed)
		}
	}
}

// TestInspectReadAndSyntaxErrors proves that only a read or syntax error is
// returned as Inspect's error, naming the path as Load does.
func TestInspectReadAndSyntaxErrors(t *testing.T) {
	missing := t.TempDir() + "/missing.toml"
	syntax := writeFile(t, "config.toml", []byte("max = \n"), 0o600)
	for _, path := range []string{missing, syntax} {
		in, err := Inspect(path)
		_, loadErr := Load(path)
		if in != nil || err == nil || loadErr == nil || err.Error() != loadErr.Error() {
			t.Errorf("Inspect = %v, %v; Load error %v; want the same error", in, err, loadErr)
		}
		if err != nil && !strings.HasPrefix(err.Error(), "config "+path+": ") {
			t.Errorf("error %q does not name the path", err)
		}
	}
}

// TestDecodeMatchesLoad proves 0003 SPEC R271 and R283: Decode of a file's
// bytes gives what Load of the file gives, apart from the path that Load
// names in read, syntax and type errors.
func TestDecodeMatchesLoad(t *testing.T) {
	key := writeKey(t, pkcs1PEM(t), 0o600)
	token := writeToken(t, fixtureToken("ghp_"), 0o600)
	tokenOnly := githubTOML(tokenLine(token))
	cases := map[string]string{
		"example":           exampleTOML(t, key),
		"0002 file":         requiredTOML(key),
		"broken 0002 file":  broken0002(writeKey(t, []byte("garbage"), 0o644)),
		"token only":        tokenOnly,
		"token target":      requiredTOML(key) + targetTOML("https://github.com/acme/other", tokenLine(token)),
		"conflict":          githubTOML(`app-client-id = "id"` + "\n" + tokenLine(token)),
		"no default":        githubTOML("") + targetTOML("https://github.com/acme/other", ""),
		"token value":       githubTOML(tokenLine(fixtureToken("gho_"))),
		"relative token":    githubTOML(tokenLine("pat.txt")),
		"credential URL":    strings.Replace(tokenOnly, "acme/widget", "acme/"+fixtureToken("ghs_"), 1),
		"syntax":            requiredTOML(key) + "max-runners = \n",
		"type and unknown":  requiredTOML(key) + "max-runners = \"two\"\ntypo = 1\n",
		"token-file of int": tokenOnly + "token-file = 5\n",
	}
	for name, content := range cases {
		t.Run(name, func(t *testing.T) {
			path := writeFile(t, "config.toml", []byte(content), 0o600)
			fromFile, fileErr := Load(path)
			fromBytes, bytesErr := Decode([]byte(content))
			if !reflect.DeepEqual(fromFile, fromBytes) {
				t.Errorf("Decode config = %+v, Load config = %+v", fromBytes, fromFile)
			}
			var fileText, bytesText string
			if fileErr != nil {
				fileText = strings.ReplaceAll(fileErr.Error(), "config "+path, "config")
			}
			if bytesErr != nil {
				bytesText = bytesErr.Error()
			}
			if fileText != bytesText {
				t.Errorf("Decode error:\n%s\nLoad error without the path:\n%s", bytesText, fileText)
			}
		})
	}
}

// TestLoadCredentialsPerTarget proves 0003 SPEC R273 and R277: each target
// gets its own token or the default credential, and Credentials prints no
// secret through fmt or slog.
func TestLoadCredentialsPerTarget(t *testing.T) {
	tokens := []string{fixtureToken("ghp_"), fixtureToken("github_pat_"), fixtureToken("github_pat_")}
	files := make([]string, len(tokens))
	for i, tok := range tokens {
		files[i] = writeToken(t, tok+"\n", 0o600)
	}

	t.Run("token default", func(t *testing.T) {
		cfg, err := loadString(t, githubTOML(tokenLine(files[0]))+
			targetTOML("https://github.com/acme/second", tokenLine(files[1]))+
			targetTOML("https://github.com/acme/third", tokenLine(files[2])))
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		creds, err := LoadCredentials(cfg)
		if err != nil {
			t.Fatalf("LoadCredentials: %v", err)
		}
		if creds.AppKey != nil || creds.DefaultToken != tokens[0] || !slices.Equal(creds.TargetTokens, []string{"", tokens[1], tokens[2]}) {
			t.Errorf("credentials are not the files' tokens in target order")
		}
		assertRedacted(t, creds, tokens)
	})

	t.Run("App default", func(t *testing.T) {
		cfg, err := loadString(t, requiredTOML(writeKey(t, pkcs1PEM(t), 0o600))+
			targetTOML("https://github.com/acme/second", tokenLine(files[1])))
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		creds, err := LoadCredentials(cfg)
		if err != nil {
			t.Fatalf("LoadCredentials: %v", err)
		}
		if creds.AppKey == nil || !creds.AppKey.Equal(rsaKey(t)) || creds.DefaultToken != "" || !slices.Equal(creds.TargetTokens, []string{"", tokens[1]}) {
			t.Errorf("credentials are not the App key and the target's token")
		}
		assertRedacted(t, creds, tokens)
	})

	t.Run("file errors by key", func(t *testing.T) {
		cfg := Config{
			GitHub:  GitHub{TokenFile: "pat.txt"},
			Targets: []Target{{}, {TokenFile: writeToken(t, tokens[1], 0o644)}},
		}
		creds, err := LoadCredentials(&cfg)
		want := []string{
			"github.token-file: must be an absolute path",
			"targets[1].token-file: file mode -rw-r--r-- is wider than 0600 (group or other bits are set); use 0600 or 0400",
		}
		if got := errorTexts(err); !slices.Equal(got, want) {
			t.Errorf("LoadCredentials errors:\n got %q\nwant %q", got, want)
		}
		if !reflect.DeepEqual(creds, Credentials{}) {
			t.Error("LoadCredentials returned credentials with an error")
		}
	})
}

// assertRedacted fails if any way of printing creds shows a token or the
// key.
func assertRedacted(t *testing.T, creds Credentials, tokens []string) {
	t.Helper()
	var out bytes.Buffer
	fmt.Fprintf(&out, "%v %+v %#v %s %q %d %x\n", creds, creds, creds, creds, creds, creds, creds)
	fmt.Fprintf(&out, "%v %+v %#v\n", &creds, struct{ C Credentials }{creds}, []Credentials{creds})
	log := slog.New(slog.NewJSONHandler(&out, nil))
	log.Info("loaded", "credentials", creds, "pointer", &creds, "nested", struct{ C Credentials }{creds})
	slog.New(slog.NewTextHandler(&out, nil)).Info("loaded", "credentials", creds, "nested", struct{ C Credentials }{creds})

	text := out.String()
	for _, tok := range tokens {
		if strings.Contains(text, tok) {
			t.Fatalf("printed credentials hold a token:\n%s", text)
		}
	}
	if strings.Contains(text, "PrivateKey") || strings.Contains(text, "TargetTokens") {
		t.Fatalf("printed credentials show their fields:\n%s", text)
	}
	if !strings.Contains(text, "<redacted>") {
		t.Fatalf("printed credentials lack <redacted>:\n%s", text)
	}
}

// TestNoTokenInAnyError proves 0003 SPEC R277: a configuration that breaks
// every rule with fixture tokens, in token files, in token file keys, in
// URLs and as a bare key, gives Load, Decode, Inspect and LoadCredentials
// errors that hold none of them and no token file path.
func TestNoTokenInAnyError(t *testing.T) {
	var secrets []string
	token := func(prefix string) string {
		tok := fixtureToken(prefix)
		secrets = append(secrets, tok)
		return tok
	}
	tokenFile := func(content string, mode os.FileMode) string {
		path := writeToken(t, content, mode)
		secrets = append(secrets, path)
		return path
	}
	// A classic token is 40 characters, short enough to pass as a key.
	bareKey := "ghp_" + tokenChars(36)
	secrets = append(secrets, bareKey)

	// Targets: a token before "@"; a token as the repository, twice (R233);
	// token files of a wide mode and of two words; a token as token-file,
	// and a token as a bare key.
	repoToken := "https://github.com/acme/" + token("github_pat_")
	targets := targetTOML("https://"+token("ghp_")+"@github.com/acme/widget", "") +
		targetTOML(repoToken, tokenLine(tokenFile(token("ghp_"), 0o644))) +
		targetTOML(repoToken, tokenLine(tokenFile(token("ghu_")+" x\n", 0o600))) +
		targetTOML("https://github.com/acme/other", tokenLine(token("ghs_"))+bareKey+" = 1\n")
	app := `app-client-id = "id"` + "\n" + `app-private-key-file = "` + writeKey(t, pkcs1PEM(t), 0o600) + `"` + "\n"
	configs := map[string]string{
		"App and token":  githubTOML(app+tokenLine(token("ghr_"))) + targets,
		"no default":     githubTOML("") + targets,
		"token default":  githubTOML(tokenLine(tokenFile(token("gho_")+"\n\n", 0o600))) + targets,
		"syntax value":   requiredTOML("KEY") + "url = " + token("ghp_") + "\n",
		"syntax bad key": requiredTOML("KEY") + bareKey + " = \n",
	}

	for name, content := range configs {
		t.Run(name, func(t *testing.T) {
			path := writeFile(t, "config.toml", []byte(content), 0o600)
			var texts []string
			add := func(err error) {
				if err != nil {
					texts = append(texts, err.Error())
				}
			}
			_, err := Load(path)
			if err == nil {
				t.Fatal("Load succeeded")
			}
			add(err)
			_, err = Decode([]byte(content))
			add(err)
			in, err := Inspect(path)
			add(err)
			if in != nil {
				add(errors.Join(in.Problems...))
				for _, f := range in.Files {
					add(f.Err)
				}
				_, err := LoadCredentials(in.Config)
				add(err)
			}
			all := strings.Join(texts, "\n")
			for _, s := range secrets {
				if strings.Contains(all, s) {
					t.Errorf("an error holds a token or token file path:\n%s", all)
					break
				}
			}
		})
	}
}
