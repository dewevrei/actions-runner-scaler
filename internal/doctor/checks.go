package doctor

import (
	"fmt"
	"strings"

	"github.com/dewevrei/actions-runner-scaler/internal/config"
)

// Check names of the SPEC's table (0003 SPEC Design doctor).
const (
	configCheck    = "Config"
	appKeyCheck    = "App key file"
	tokenCheckName = "Token file"
	githubCheck    = "GitHub"
	dockerCheck    = "Docker"
	exposureCheck  = "Docker TCP 2375"
	limitCheck     = "Server limit"
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
	notCheckedSummary = "not checked"
	noAnswerSummary   = "could not check"
	fixEarlier        = "fix the earlier check named in the cause, then run doctor again"
	fixAgain          = "run doctor again in a moment"
)

// blocked is the result of a check whose input failed an earlier check
// (0003 SPEC R297).
func blocked(name string, by []string) Result {
	return Result{
		Outcome: Unknown,
		Name:    name,
		Summary: notCheckedSummary,
		Causes:  []string{"an earlier check did not pass: " + strings.Join(by, ", ")},
		Fix:     fixEarlier,
	}
}

// config is the Config check: every problem but those of the key and token
// files, which get lines of their own (0003 SPEC R304). in is nil when the
// file could not be read or parsed.
func (c *checker) config() (Result, *config.Inspection) {
	in, err := c.Inspect(c.ConfigPath)
	if err != nil {
		return Result{
			Outcome: Fail,
			Name:    configCheck,
			Summary: "could not read the config file",
			Causes:  []string{err.Error()},
			Fix:     "check the path with -config, or create the config file with actions-runner-scaler init",
		}, nil
	}
	if len(in.Problems) == 0 {
		return Result{Outcome: Pass, Name: configCheck, Summary: "no problems"}, in
	}
	causes := make([]string, len(in.Problems))
	for i, p := range in.Problems {
		causes[i] = p.Error()
	}
	return Result{
		Outcome: Fail,
		Name:    configCheck,
		Summary: fmt.Sprintf("problems: %d", len(causes)),
		Causes:  causes,
		Fix:     "fix the keys named in the causes in the config file (see config.example.toml)",
	}, in
}

// fileChecks are the key and token file lines and, by key, the name and
// outcome later checks depend on.
type fileChecks struct {
	results []Result
	byKey   map[string]Result
}

// blockers returns nil when the file of key passed, else the check to name
// as the cause: the file's own line, or Config when the key has none.
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
// set, so its lines are UNKNOWN without a key.
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
			r = Result{Outcome: Pass, Name: name, Summary: shown + "read"}
		case checked:
			r = Result{
				Outcome: Fail,
				Name:    name,
				Summary: shown + "could not read",
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
		return "write the absolute path of the App private key (PEM) file and set the file mode to 0600 (chmod 600)"
	}
	return "write the absolute path of the token file, not the token value; the file holds the token alone on one line and has mode 0600 (chmod 600)"
}
