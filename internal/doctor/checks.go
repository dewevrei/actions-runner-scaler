package doctor

import (
	"fmt"
	"strings"

	"github.com/dewevrei/actions-runner-scaler/internal/config"
)

// Check names of the SPEC's table (0003 SPEC Design doctor).
const (
	configCheck    = "설정"
	appKeyCheck    = "App 키 파일"
	tokenCheckName = "토큰 파일"
	githubCheck    = "GitHub"
	dockerCheck    = "Docker"
	exposureCheck  = "Docker TCP 2375"
	limitCheck     = "서버 한도"
)

// Keys of the credential files (0003 SPEC R304).
const (
	appKeyKey       = "github.app-private-key-file"
	defaultTokenKey = "github.token-file"
)

func targetTokenKey(i int) string { return fmt.Sprintf("targets[%d].token-file", i) }

func targetAppKeyKey(i int) string { return fmt.Sprintf("targets[%d].app-private-key-file", i) }

func targetClientKey(i int) string { return fmt.Sprintf("targets[%d].app-client-id", i) }

// isAppKeyKey reports whether key names an App private key file, the
// default App's or a target's.
func isAppKeyKey(key string) bool { return strings.HasSuffix(key, ".app-private-key-file") }

func tokenCheck(key string) string { return tokenCheckName + " " + key }

// Texts shared by several checks.
const (
	notCheckedSummary = "확인하지 않음"
	noAnswerSummary   = "확인하지 못함"
	fixEarlier        = "원인에 적힌 앞 검사를 먼저 고친 뒤 다시 실행한다"
	fixAgain          = "잠시 뒤 다시 실행한다"
)

// blocked is the result of a check whose input failed an earlier check
// (0003 SPEC R297).
func blocked(name string, by []string) Result {
	return Result{
		Outcome: Unknown,
		Name:    name,
		Summary: notCheckedSummary,
		Causes:  []string{"앞 검사가 통과하지 못함: " + strings.Join(by, ", ")},
		Fix:     fixEarlier,
	}
}

// config is the 설정 check: every problem but those of the key and token
// files, which get lines of their own (0003 SPEC R304). in is nil when the
// file could not be read or parsed.
func (c *checker) config() (Result, *config.Inspection) {
	in, err := c.Inspect(c.ConfigPath)
	if err != nil {
		return Result{
			Outcome: Fail,
			Name:    configCheck,
			Summary: "설정 파일을 읽지 못함",
			Causes:  []string{err.Error()},
			Fix:     "-config로 경로를 확인하거나 actions-runner-scaler init으로 설정 파일을 만든다",
		}, nil
	}
	if len(in.Problems) == 0 {
		return Result{Outcome: Pass, Name: configCheck, Summary: "문제 없음"}, in
	}
	causes := make([]string, len(in.Problems))
	for i, p := range in.Problems {
		causes[i] = p.Error()
	}
	return Result{
		Outcome: Fail,
		Name:    configCheck,
		Summary: fmt.Sprintf("문제 %d개", len(causes)),
		Causes:  causes,
		Fix:     "원인에 적힌 키를 설정 파일에서 고친다(config.example.toml 참고)",
	}, in
}

// fileChecks are the key and token file lines and, by key, the name and
// outcome later checks depend on.
type fileChecks struct {
	results []Result
	byKey   map[string]Result
}

// blockers returns nil when the file of key passed, else the check to name
// as the cause: the file's own line, or 설정 when the key has none.
func (f fileChecks) blockers(key string) []string {
	r, ok := f.byKey[key]
	switch {
	case !ok:
		return []string{configCheck}
	case r.Outcome != Pass:
		return []string{r.Name}
	}
	return nil
}

// files makes the App key and token file lines, by key and never by path
// (0003 SPEC R304). A key with neither a check nor a decoding error is not
// set and gets no line. A file that cannot be parsed hides which keys are
// set, so its lines are 확인 못 함 without a key.
func (c *checker) files(in *config.Inspection) fileChecks {
	if in == nil {
		by := []string{configCheck}
		return fileChecks{results: []Result{blocked(appKeyCheck, by), blocked(tokenCheckName, by)}}
	}
	keys := []string{appKeyKey, defaultTokenKey}
	for i := range in.Config.Targets {
		keys = append(keys, targetAppKeyKey(i), targetTokenKey(i))
	}
	checks := make(map[string]config.FileCheck, len(in.Files))
	for _, f := range in.Files {
		checks[f.Key] = f
	}
	out := fileChecks{byKey: make(map[string]Result)}
	for _, key := range keys {
		// The default App line's name lacks its key, so its summary
		// carries it.
		name, shown := tokenCheck(key), ""
		switch {
		case key == appKeyKey:
			name, shown = appKeyCheck, key+" "
		case isAppKeyKey(key):
			name = appKeyCheck + " " + key
		}
		f, checked := checks[key]
		var r Result
		switch {
		case checked && f.Err == nil:
			r = Result{Outcome: Pass, Name: name, Summary: shown + "읽음"}
		case checked:
			r = Result{
				Outcome: Fail,
				Name:    name,
				Summary: shown + "읽지 못함",
				Causes:  []string{key + ": " + f.Err.Error()},
				Fix:     fileFix(key),
			}
		case in.Failed(key):
			r = blocked(name, []string{configCheck})
		default:
			continue
		}
		out.results = append(out.results, r)
		out.byKey[key] = r
	}
	return out
}

// fileFix names the rules of the file of key (R30, 0003 SPEC R275, R276).
func fileFix(key string) string {
	if isAppKeyKey(key) {
		return "App 개인키(PEM) 파일의 절대 경로를 적고, 파일 모드를 0600으로 한다(chmod 600)"
	}
	return "토큰 값이 아니라 토큰 파일의 절대 경로를 적고, 파일은 토큰 한 줄만 담아 모드를 0600으로 한다(chmod 600)"
}
