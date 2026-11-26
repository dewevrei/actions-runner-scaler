package configfile

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/dewevrei/actions-runner-scaler/internal/config"
)

// Values are what init writes into a new configuration file. Secrets are
// paths only (0003 SPEC R285). Render writes what it is given and checks
// nothing; config.Decode checks the result.
type Values struct {
	// AppClientID and AppPrivateKeyFile are the default App credential;
	// TokenFile is the default token credential. A file has one of them.
	AppClientID       string
	AppPrivateKeyFile string
	TokenFile         string
	RunnerImage       string
	BuildKitImage     string
	Targets           []TargetEntry
}

const renderHeader = `# Configuration for actions-runner-scaler, written by init.
# Keys marked "default" hold their default value and may be left out.
# Secrets are referenced by path only.

`

// The comments above the two image keys (0003 SPEC R318-R320), the same
// lines as in config.example.toml.
const (
	runnerImageComment = "# An image reference <name>[:<tag>][@<digest>] or a local image ID; a reference\n" +
		"# missing locally is pulled. image/build.sh tags its image actions-runner-scaler-runner:local.\n"
	buildKitImageComment = "# An image reference <name>[:<tag>][@<digest>] or a local image ID; a reference\n" +
		"# missing locally is pulled. Pinned index digest printed by image/build.sh.\n"
)

// defaultMark follows every key written with its default value.
const defaultMark = " # default"

// Render returns the configuration file for v: the tables and comments of
// config.example.toml in its order, v's values, and every other key with
// its default value and a "# default" comment, so the file shows which keys
// exist (0003 SPEC R283, Design init). Each repository is a TargetBlock.
func Render(v Values) []byte {
	d := config.Default()
	var b strings.Builder
	b.WriteString(renderHeader)
	renderGitHub(&b, v)
	renderContainer(&b, "runner", runnerImageComment, v.RunnerImage, d.Runner)
	renderContainer(&b, "buildkit", buildKitImageComment, v.BuildKitImage, d.BuildKit)

	b.WriteString("\n[scaling]\n")
	writeKey(&b, "label", quote(d.Scaling.Label)+defaultMark+"; scale set name and runs-on label")
	b.WriteString("\n[jobcheck]\n")
	writeKey(&b, "listen", quote(d.JobCheck.Listen)+defaultMark+"; loopback addresses only")
	b.WriteString("\n[shutdown]\n")
	writeKey(&b, "drain-timeout", quote(shortDuration(d.Shutdown.DrainTimeout))+defaultMark)

	b.WriteString("\n# One [[targets]] entry per repository; there is no upper limit.\n")
	b.WriteString("# A token-file key in an entry replaces the [github] credential for that repository only.\n")
	for i, t := range v.Targets {
		if i > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(markDefaults(t))
	}
	return []byte(b.String())
}

// renderGitHub writes the [github] table: the App keys, the token file, or
// both when v holds both (config.Decode refuses that).
func renderGitHub(b *strings.Builder, v Values) {
	b.WriteString("[github]\n")
	if v.AppClientID != "" || v.AppPrivateKeyFile != "" {
		b.WriteString("# The default credential of every target: a GitHub App (these two keys) or a\n")
		b.WriteString("# personal access token file, not both.\n")
		writeKey(b, "app-client-id", quote(v.AppClientID))
		writeKey(b, "app-private-key-file", quote(v.AppPrivateKeyFile)+" # absolute path; mode 0600 or 0400")
		if v.TokenFile == "" {
			b.WriteString("# A token file replaces the App above for every target without its own token-file.\n")
			b.WriteString("# token-file = \"<absolute-path-to-token-file>\"\n")
		}
	}
	if v.TokenFile != "" {
		b.WriteString("# The default credential of every target: a personal access token file.\n")
		b.WriteString("# For a GitHub App instead, replace this key with app-client-id and app-private-key-file.\n")
		writeKey(b, "token-file", quote(v.TokenFile)+" # absolute path; mode 0600 or 0400")
	}
}

// renderContainer writes the [runner] or [buildkit] table named name.
func renderContainer(b *strings.Builder, name, imageComment, image string, c config.Container) {
	fmt.Fprintf(b, "\n[%s]\n", name)
	b.WriteString(imageComment)
	writeKey(b, "image", quote(image))
	writeKey(b, "cpus", strconv.FormatFloat(c.CPUs, 'g', -1, 64)+defaultMark)
	writeKey(b, "memory", quote(c.Memory.String())+defaultMark)
	writeKey(b, "pids", strconv.FormatInt(c.Pids, 10)+defaultMark)
}

// markDefaults returns t's TargetBlock with the "# default" mark on each key
// that holds its default, and idle written when t leaves it out.
func markDefaults(t TargetEntry) string {
	if t.Idle == nil {
		idle := config.DefaultIdle
		t.Idle = &idle
	}
	var b strings.Builder
	for _, line := range strings.SplitAfter(TargetBlock(t), "\n") {
		key, value, ok := strings.Cut(strings.TrimSuffix(line, "\n"), " = ")
		isDefault := (key == "max-runners" && value == strconv.Itoa(config.DefaultMaxRunners)) ||
			(key == "idle" && value == strconv.Itoa(config.DefaultIdle))
		if ok && isDefault {
			line = strings.TrimSuffix(line, "\n") + defaultMark + "\n"
		}
		b.WriteString(line)
	}
	return b.String()
}

// shortDuration formats d without the zero units time.Duration.String
// adds, such as "10m" for ten minutes.
func shortDuration(d time.Duration) string {
	s := d.String()
	if strings.HasSuffix(s, "m0s") {
		s = strings.TrimSuffix(s, "0s")
	}
	if strings.HasSuffix(s, "h0m") {
		s = strings.TrimSuffix(s, "0m")
	}
	return s
}
