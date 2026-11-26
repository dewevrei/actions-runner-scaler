package configfile

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/dewevrei/actions-runner-scaler/internal/config"
)

// The rules init applies to a value, whether it came from a flag or an
// answer (0003 SPEC R284, R285, R288). None of their errors quotes the
// value: it may be a pasted secret (R277).

var (
	// runnerImagePattern and buildkitImagePattern repeat the image rules of
	// config's validation, which does not export them; a test keeps them in
	// step.
	runnerImagePattern   = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	buildkitImagePattern = regexp.MustCompile(`^moby/buildkit:[A-Za-z0-9_][A-Za-z0-9_.-]*-rootless@sha256:[0-9a-f]{64}$`)
)

// CheckAppClientID checks a GitHub App client ID: one token of printable
// text that is not a pasted GitHub token.
func CheckAppClientID(s string) error {
	switch {
	case s == "":
		return errors.New("must not be empty")
	case !utf8.ValidString(s):
		return errors.New("must be valid UTF-8")
	case strings.ContainsFunc(s, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }):
		return errors.New("must not contain spaces or control characters")
	case looksLikeCredential(s):
		return errors.New("looks like a token, not a GitHub App client ID")
	}
	return nil
}

// looksLikeCredential reports whether s starts with a token prefix or holds
// a credential (config.ContainsCredential).
func looksLikeCredential(s string) bool {
	for _, p := range config.TokenPrefixes() {
		if strings.HasPrefix(s, p) {
			return true
		}
	}
	return config.ContainsCredential(s)
}

// CheckAppKeyPath checks the path of a GitHub App private key file with the
// rules of the key file in a configuration (0001 SPEC R30): absolute, and
// config.ReadAppPrivateKey's.
func CheckAppKeyPath(path string) error {
	if !utf8.ValidString(path) {
		return errors.New("must be valid UTF-8")
	}
	pastedKey := strings.Contains(path, "-----BEGIN") || strings.ContainsAny(path, "\r\n")
	if !pastedKey && !filepath.IsAbs(path) {
		return errors.New("must be an absolute path")
	}
	_, err := config.ReadAppPrivateKey(path)
	return err
}

// CheckTokenPath checks the path of a token file with config.ReadTokenFile
// (0003 SPEC R275, R276). The token it reads is dropped at once.
func CheckTokenPath(path string) error {
	if !utf8.ValidString(path) {
		return errors.New("must be valid UTF-8")
	}
	_, err := config.ReadTokenFile(path)
	return err
}

// CheckRunnerImage checks a runner.image value.
func CheckRunnerImage(s string) error {
	if !runnerImagePattern.MatchString(s) {
		return errors.New("must be a local image ID sha256:<64 hex digits>")
	}
	return nil
}

// CheckBuildKitImage checks a buildkit.image value.
func CheckBuildKitImage(s string) error {
	if !buildkitImagePattern.MatchString(s) {
		return errors.New("must be moby/buildkit:<version>-rootless@sha256:<64 hex digits>")
	}
	return nil
}

// CheckRepository checks a repository URL to add after targets: the rules of
// targets.url, credential first (0003 SPEC R314, 0002 SPEC R231, R232), then
// a repository an earlier target already names (0002 SPEC R233).
func CheckRepository(targets []TargetEntry, url string) error {
	if err := config.ValidateRepositoryURL(url); err != nil {
		return err
	}
	key, _ := config.RepositoryKey(url)
	for _, t := range targets {
		if earlier, ok := config.RepositoryKey(t.URL); ok && earlier == key {
			return fmt.Errorf("repository listed more than once (owner and repository names compare without case): %s, %s", t.URL, url)
		}
	}
	return nil
}

// ParseMaxRunners returns the max-runners n that s writes: an integer of 1
// or more (0002 SPEC R235).
func ParseMaxRunners(s string) (int, error) {
	n, err := strconv.Atoi(s)
	if err != nil || n < 1 {
		return 0, errors.New("max-runners must be an integer of 1 or more")
	}
	return n, nil
}

// Gap is a required value the flags left out.
type Gap struct {
	// What names the value; Flag is the flag that gives it.
	What, Flag string
}

// Gaps returns the required values v lacks, in the order Prompter asks for
// them (0003 SPEC R285, R287). buildkit.image is never one: init fills it
// from a constant (R286).
func Gaps(v Values) []Gap {
	var gaps []Gap
	switch {
	case v.TokenFile != "":
	case v.AppClientID == "" && v.AppPrivateKeyFile == "":
		gaps = append(gaps, Gap{"credential", "-app-client-id and -app-private-key-file, or -token-file"})
	case v.AppClientID == "":
		gaps = append(gaps, Gap{"GitHub App client ID", "-app-client-id"})
	case v.AppPrivateKeyFile == "":
		gaps = append(gaps, Gap{"GitHub App private key file", "-app-private-key-file"})
	}
	if len(v.Targets) == 0 {
		gaps = append(gaps, Gap{"repository", "-repo <URL>[=<max-runners>]"})
	}
	if v.RunnerImage == "" {
		gaps = append(gaps, Gap{"runner.image (run image/build.sh first)", "-runner-image"})
	}
	return gaps
}

// Prompts and messages of the questions (0003 SPEC Design init).
const (
	promptCredential = "자격 [app/token] (기본 app):"
	promptClientID   = "GitHub App client ID:"
	promptKeyPath    = "App 개인키 파일 경로:"
	promptTokenPath  = "토큰 파일 경로:"
	promptRepository = "저장소 URL (빈 줄이면 끝):"
	promptMax        = "max-runners (기본 1):"
	promptRunner     = "runner.image (image/build.sh가 출력한 값):"

	noteAppLink     = "GitHub App이 없으면 먼저 actions-runner-scaler app-link로 만든다."
	msgPickCredType = "app 또는 token 중 하나를 입력"
	msgNeedRepo     = "저장소가 하나 이상 필요함"
)

// Prompter asks, on a terminal, for the values the flags left out (0003
// SPEC R285). It asks for secrets only as file paths.
type Prompter struct {
	in  *bufio.Reader
	out io.Writer
}

// NewPrompter returns a Prompter that reads answers from in and writes
// questions and errors to out.
func NewPrompter(in io.Reader, out io.Writer) *Prompter {
	return &Prompter{in: bufio.NewReader(in), out: out}
}

// Fill asks for each value of v that Gaps lists and stores the answer in v.
// An answer that breaks a rule gets its error and the same question again.
// Input that ends before an answer returns io.EOF.
func (p *Prompter) Fill(v *Values) error {
	if err := p.credential(v); err != nil {
		return err
	}
	if len(v.Targets) == 0 {
		if err := p.repositories(v); err != nil {
			return err
		}
	}
	if v.RunnerImage == "" {
		image, err := p.ask(promptRunner, CheckRunnerImage)
		if err != nil {
			return err
		}
		v.RunnerImage = image
	}
	return nil
}

// credential asks for the missing part of the default credential: the
// kind first when v holds none.
func (p *Prompter) credential(v *Values) error {
	if v.TokenFile != "" {
		return nil
	}
	app := v.AppClientID != "" || v.AppPrivateKeyFile != ""
	if !app {
		kind, err := p.ask(promptCredential, checkCredentialKind)
		if err != nil {
			return err
		}
		if strings.EqualFold(kind, "token") {
			path, err := p.ask(promptTokenPath, CheckTokenPath)
			v.TokenFile = path
			return err
		}
	}
	if v.AppClientID != "" && v.AppPrivateKeyFile != "" {
		return nil
	}
	fmt.Fprintln(p.out, noteAppLink)
	if v.AppClientID == "" {
		id, err := p.ask(promptClientID, CheckAppClientID)
		if err != nil {
			return err
		}
		v.AppClientID = id
	}
	if v.AppPrivateKeyFile == "" {
		path, err := p.ask(promptKeyPath, CheckAppKeyPath)
		if err != nil {
			return err
		}
		v.AppPrivateKeyFile = path
	}
	return nil
}

// checkCredentialKind accepts an empty answer, which means app.
func checkCredentialKind(s string) error {
	if s != "" && !strings.EqualFold(s, "app") && !strings.EqualFold(s, "token") {
		return errors.New(msgPickCredType)
	}
	return nil
}

// repositories asks for URL and max-runners pairs until an empty URL, which
// ends the list only once it holds one repository.
func (p *Prompter) repositories(v *Values) error {
	for {
		url, err := p.ask(promptRepository, func(s string) error {
			if s == "" {
				return nil
			}
			return CheckRepository(v.Targets, s)
		})
		if err != nil {
			return err
		}
		if url == "" {
			if len(v.Targets) > 0 {
				return nil
			}
			fmt.Fprintln(p.out, msgNeedRepo)
			continue
		}
		answer, err := p.ask(promptMax, func(s string) error {
			if s == "" {
				return nil
			}
			_, err := ParseMaxRunners(s)
			return err
		})
		if err != nil {
			return err
		}
		runners := config.DefaultMaxRunners
		if answer != "" {
			runners, _ = ParseMaxRunners(answer)
		}
		v.Targets = append(v.Targets, TargetEntry{URL: url, MaxRunners: runners})
	}
}

// ask writes question and reads an answer until check accepts it.
func (p *Prompter) ask(question string, check func(string) error) (string, error) {
	for {
		fmt.Fprint(p.out, question+" ")
		answer, err := p.readLine()
		if err != nil {
			if err == io.EOF {
				// End the question's line, so the caller's message starts
				// on a new one.
				fmt.Fprintln(p.out)
			}
			return "", err
		}
		if err := check(answer); err != nil {
			fmt.Fprintln(p.out, err)
			continue
		}
		return answer, nil
	}
}

// readLine returns the next line without its line break or the spaces
// around it. A last line with no line break still counts; input that ends
// before any character returns io.EOF as is.
func (p *Prompter) readLine() (string, error) {
	line, err := p.in.ReadString('\n')
	switch {
	case err == nil, err == io.EOF && line != "":
		return strings.TrimSpace(line), nil
	case err == io.EOF:
		return "", io.EOF
	default:
		return "", fmt.Errorf("read answer: %w", err)
	}
}
