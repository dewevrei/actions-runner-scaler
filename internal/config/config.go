// Package config loads and validates the scaler's TOML configuration file.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/BurntSushi/toml"
)

// Default values for optional keys.
const (
	DefaultJobCheckListen = "127.0.0.1:8765"
	DefaultDrainTimeout   = 10 * time.Minute
	DefaultLabel          = "linux-arm64-deploy"
	DefaultMaxRunners     = 1 // 0002 SPEC R235
	DefaultIdle           = 0
	DefaultRunnerCPUs     = 1.0
	DefaultRunnerMemory   = 1 * Gibibyte
	DefaultRunnerPids     = 512
	DefaultBuildCPUs      = 2.0
	DefaultBuildMemory    = 2 * Gibibyte
	DefaultBuildPids      = 1024
	// DefaultServerLimit means "use the limit computed from Docker".
	DefaultServerLimit = 0
	// DefaultDockerHost is the Docker Engine endpoint the scaler used before
	// docker.host existed; it equals the Docker client's own default.
	DefaultDockerHost = "unix:///var/run/docker.sock"
	DefaultLogLevel   = "info"
)

// Config is the content of the configuration file (0002 SPEC Design 설정
// 파일). The toml tags name the keys; decodeFile maps each key explicitly,
// and the example file test keeps the two in step.
type Config struct {
	GitHub   GitHub    `toml:"github"`
	Runner   Container `toml:"runner"`
	BuildKit Container `toml:"buildkit"`
	JobCheck JobCheck  `toml:"jobcheck"`
	Shutdown Shutdown  `toml:"shutdown"`
	Scaling  Scaling   `toml:"scaling"`
	Docker   Docker    `toml:"docker"`
	Log      Log       `toml:"log"`
	// Targets is checked separately by the example file test.
	Targets []Target `toml:"-"`
}

// GitHub holds the default credential, used by every target without its own
// token file: a GitHub App or a personal access token, never both (0003
// SPEC R272, R274). Secrets are referenced by path only, never held in the
// configuration. An App's installation ID is looked up for each App target
// (0002 SPEC R236); a token target checks its token instead (0003 SPEC R278,
// R279).
type GitHub struct {
	AppClientID       string `toml:"app-client-id"`
	AppPrivateKeyFile string `toml:"app-private-key-file"`
	// TokenFile is the path of a personal access token file (0003 SPEC
	// R272, R275). Inspect blanks a value refused as a token (R276); callers
	// of Inspect check Failed("github.token-file") before using it.
	TokenFile string `toml:"token-file"`
}

// Container is the image and the limits of one kind of container. Image is
// a local image ID or an image reference; a reference missing locally is
// pulled (0003 SPEC R318-R320; 0002 SPEC R121, R122 for the limits).
type Container struct {
	Image  string   `toml:"image"`
	CPUs   float64  `toml:"cpus"`
	Memory ByteSize `toml:"memory"`
	Pids   int64    `toml:"pids"`
}

// JobCheck configures the job check server that runner containers query.
type JobCheck struct {
	Listen string `toml:"listen"`
}

// Shutdown configures draining on a stop signal or error.
type Shutdown struct {
	DrainTimeout time.Duration `toml:"drain-timeout"`
}

// Scaling holds the settings every target shares.
type Scaling struct {
	// Label is the scale set name and the runs-on label.
	Label string `toml:"label"`
	// ServerLimit, when positive, is the server limit in place of the value
	// computed from Docker; 0 leaves the computed value. Read it through
	// ServerLimitOr.
	ServerLimit int `toml:"server-limit"`
}

// ServerLimitOr returns the configured server limit, or computed when the
// key is absent or 0.
func (s Scaling) ServerLimitOr(computed int) int {
	if s.ServerLimit > 0 {
		return s.ServerLimit
	}
	return computed
}

// Docker holds the Docker Engine endpoint.
type Docker struct {
	// Host is a unix:// or tcp:// URL.
	Host string `toml:"host"`
}

// Log configures the scaler's log output.
type Log struct {
	// Level is debug, info, warn or error. Read it through SlogLevel.
	Level string `toml:"level"`
}

// SlogLevel returns Level as a slog level; a value validation refuses reads
// as info.
func (l Log) SlogLevel() slog.Level {
	level, _ := parseLogLevel(l.Level)
	return level
}

// parseLogLevel maps the name of a level to its slog level and reports
// whether name is one of debug, info, warn and error.
func parseLogLevel(name string) (slog.Level, bool) {
	switch name {
	case "debug":
		return slog.LevelDebug, true
	case "info":
		return slog.LevelInfo, true
	case "warn":
		return slog.LevelWarn, true
	case "error":
		return slog.LevelError, true
	}
	return slog.LevelInfo, false
}

// Target is one [[targets]] entry: a repository the scaler registers
// runners for (0002 SPEC R231).
type Target struct {
	// URL is the repository URL. Inspect blanks a value that holds a
	// credential (0003 SPEC R314); callers of Inspect check
	// Failed("targets[<i>].url") before using it.
	URL        string `toml:"url"`
	MaxRunners int    `toml:"max-runners"`
	Idle       int    `toml:"idle"`
	// TokenFile, when set, is the path of the personal access token file
	// this target uses in place of the default credential (0003 SPEC R273,
	// R275). Inspect blanks a value refused as a token (R276); callers of
	// Inspect check Failed("targets[<i>].token-file") before using it.
	TokenFile string `toml:"token-file"`
	// AppClientID and AppPrivateKeyFile, when set, are the GitHub App this
	// target will use in place of the default credential: an App pair or a
	// TokenFile, never both. They are read and loaded (Credentials.
	// TargetAppKeys), but validation refuses them until the daemon and the
	// commands use them (TODO(wiring), see targetCredential).
	AppClientID       string `toml:"app-client-id"`
	AppPrivateKeyFile string `toml:"app-private-key-file"`
}

// Default returns a Config with every optional key set to its default and no
// targets.
func Default() Config {
	return Config{
		Runner:   Container{CPUs: DefaultRunnerCPUs, Memory: DefaultRunnerMemory, Pids: DefaultRunnerPids},
		BuildKit: Container{CPUs: DefaultBuildCPUs, Memory: DefaultBuildMemory, Pids: DefaultBuildPids},
		JobCheck: JobCheck{Listen: DefaultJobCheckListen},
		Shutdown: Shutdown{DrainTimeout: DefaultDrainTimeout},
		Scaling:  Scaling{Label: DefaultLabel, ServerLimit: DefaultServerLimit},
		Docker:   Docker{Host: DefaultDockerHost},
		Log:      Log{Level: DefaultLogLevel},
	}
}

// DefaultTarget returns a Target with every optional key set to its default.
func DefaultTarget() Target {
	return Target{MaxRunners: DefaultMaxRunners, Idle: DefaultIdle}
}

// Load reads the TOML file at path, applies defaults and validates the
// result. Every unknown key, value of the wrong TOML type, malformed size or
// duration and invalid value is reported together in one error joined with
// errors.Join, in the same order on every run. Only a read or syntax error
// is returned alone. Errors of the key and token files are among them, in
// the place they had in 0002 (0003 SPEC R271); Inspect returns them apart.
func Load(path string) (*Config, error) {
	in, err := Inspect(path)
	if err != nil {
		return nil, err
	}
	return in.result()
}

// Decode is Load for a file's content held in memory: data is decoded and
// validated by the same rules, and the key and token files it names are
// read by path (0003 SPEC R283). Errors name the configuration "config"
// where Load names its path.
func Decode(data []byte) (*Config, error) {
	in, err := inspect(data, "config")
	if err != nil {
		return nil, err
	}
	return in.result()
}

// fields maps each key of a table to the variable its value decodes into.
// The lookup is exact because TOML keys are case-sensitive, unlike the
// decoder's own matching of keys to struct fields.
type fields map[string]any

// decode decodes data over the defaults. It returns type errors, unknown
// keys and malformed sizes and durations as problems, the keys and tables
// that failed to decode (such as "github.app-client-id" or "targets[0]") in
// failed, and a syntax error as err. source starts the messages that name
// the configuration, such as "config <path>".
//
// Each table is decoded key by key in sorted order: the decoder walks a
// table in map order and stops at the first bad key, which would report a
// random one of several bad keys and hide the rest.
func decode(data []byte, source string) (*Config, []error, map[string]bool, error) {
	var top map[string]toml.Primitive
	md, err := toml.NewDecoder(bytes.NewReader(data)).Decode(&top)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("%s: %w", source, decodeError(err))
	}

	d := &decoder{md: md, source: source, failed: make(map[string]bool)}
	c := Default()
	for _, name := range slices.Sorted(maps.Keys(top)) {
		raw := top[name]
		switch name {
		case "github":
			// The 0001 key still reads, to report it (0002 SPEC R107).
			var installationID any
			decoded := d.table(name, raw, fields{
				"app-client-id":        &c.GitHub.AppClientID,
				"app-installation-id":  &installationID,
				"app-private-key-file": &c.GitHub.AppPrivateKeyFile,
				"token-file":           &c.GitHub.TokenFile,
			})
			if decoded["app-installation-id"] {
				d.errs = append(d.errs, errors.New(msgInstallationID))
			}
		case "runner":
			d.container(name, raw, &c.Runner)
		case "buildkit":
			d.container(name, raw, &c.BuildKit)
		case "jobcheck":
			d.table(name, raw, fields{"listen": &c.JobCheck.Listen})
		case "scaling":
			d.table(name, raw, fields{
				"label":        &c.Scaling.Label,
				"server-limit": &c.Scaling.ServerLimit,
			})
		case "docker":
			d.table(name, raw, fields{"host": &c.Docker.Host})
		case "log":
			d.table(name, raw, fields{"level": &c.Log.Level})
		case "shutdown":
			var timeout text
			decoded := d.table(name, raw, fields{"drain-timeout": &timeout})
			if decoded["drain-timeout"] {
				d.duration(name+".drain-timeout", timeout, &c.Shutdown.DrainTimeout)
			}
		case "targets":
			c.Targets = d.targets(raw)
		case "pools":
			// Any shape and every key inside count as this one error (0002
			// SPEC R107).
			d.errs = append(d.errs, errors.New(msgPools))
		default:
			d.unknown("", name)
		}
	}
	return &c, d.errs, d.failed, nil
}

// decoder collects decoding problems for one file.
type decoder struct {
	md     toml.MetaData
	source string
	errs   []error
	failed map[string]bool
}

// Messages for the 0001 keys (0002 SPEC Design 설정 파일, R107).
const (
	msgPools = `pools: [[pools]] is the 0001 format; move max to [[targets]] max-runners, idle to [[targets]] idle, runner-*/build-* to [runner]/[buildkit] cpus, memory, pids, name to [scaling] label, and set [[targets]] url = "https://github.com/<owner>/<repo>" for each repository; allowed-workflows and allowed-events no longer exist`

	msgInstallationID = "github.app-installation-id: no longer used; the installation is looked up for each target"

	// msgRunnerGroup follows targets[<i>].runner-group (0002 SPEC R234).
	msgRunnerGroup = "no longer used; repository scale sets always use the default runner group, remove the key"
)

// container decodes [runner] or [buildkit] over the defaults in dst.
func (d *decoder) container(name string, raw toml.Primitive, dst *Container) {
	var memory text
	decoded := d.table(name, raw, fields{
		"image":  &dst.Image,
		"cpus":   &dst.CPUs,
		"memory": &memory,
		"pids":   &dst.Pids,
	})
	if decoded["memory"] {
		d.byteSize(name+".memory", memory, &dst.Memory)
	}
}

// targets decodes the [[targets]] array entry by entry.
func (d *decoder) targets(raw toml.Primitive) []Target {
	var entries []toml.Primitive
	if err := d.md.PrimitiveDecode(raw, &entries); err != nil {
		d.typeError("targets", err)
		return nil
	}
	targets := make([]Target, 0, len(entries))
	for i, entry := range entries {
		// A failed entry still counts toward the number of targets.
		targets = append(targets, d.target(fmt.Sprintf("targets[%d]", i), entry))
	}
	return targets
}

// target decodes one entry over DefaultTarget, so keys left out keep their
// defaults while an explicit zero still reaches validation. The first
// version's runner-group key still reads, with any value, to report it
// (0002 SPEC R234).
func (d *decoder) target(name string, raw toml.Primitive) Target {
	t := DefaultTarget()
	var runnerGroup any
	decoded := d.table(name, raw, fields{
		"url":                  &t.URL,
		"max-runners":          &t.MaxRunners,
		"runner-group":         &runnerGroup,
		"idle":                 &t.Idle,
		"token-file":           &t.TokenFile,
		"app-client-id":        &t.AppClientID,
		"app-private-key-file": &t.AppPrivateKeyFile,
	})
	if decoded["runner-group"] {
		d.errs = append(d.errs, fmt.Errorf("%s.runner-group: %s", name, msgRunnerGroup))
	}
	return t
}

// table decodes raw as the table name, each key into its variable in f, and
// records every type error and unknown key. It returns the keys that
// decoded.
func (d *decoder) table(name string, raw toml.Primitive, f fields) map[string]bool {
	var values map[string]toml.Primitive
	err := d.md.PrimitiveDecode(raw, &values)
	// The decoder leaves the map nil, without an error, for a non-table.
	if err == nil && values == nil {
		err = errors.New("must be a table")
	}
	if err != nil {
		d.typeError(name, err)
		return nil
	}
	decoded := make(map[string]bool, len(values))
	for _, k := range slices.Sorted(maps.Keys(values)) {
		dst, ok := f[k]
		if !ok {
			d.unknown(name, k)
			continue
		}
		if err := d.md.PrimitiveDecode(values[k], dst); err != nil {
			d.typeError(name+"."+k, err)
			continue
		}
		decoded[k] = true
	}
	return decoded
}

// unknown records key, under the table named prefix, as not in the schema.
// Keys inside an unknown table are not reported on their own.
func (d *decoder) unknown(prefix, key string) {
	shown := displaySegment(key)
	if prefix != "" {
		shown = prefix + "." + shown
	}
	d.errs = append(d.errs, fmt.Errorf("unknown key %q", shown))
}

func (d *decoder) typeError(key string, err error) {
	d.errs = append(d.errs, fmt.Errorf("%s: %s: %w", d.source, key, decodeError(err)))
	d.failed[key] = true
}

// byteSize parses s into dst. On failure dst keeps its default and key is
// marked failed, so a malformed value yields one error.
func (d *decoder) byteSize(key string, s text, dst *ByteSize) {
	v, err := ParseByteSize(string(s))
	if err != nil {
		d.invalid(key, err)
		return
	}
	*dst = v
}

// duration parses s into dst like byteSize.
func (d *decoder) duration(key string, s text, dst *time.Duration) {
	v, err := parseDuration(string(s))
	if err != nil {
		d.invalid(key, err)
		return
	}
	*dst = v
}

func (d *decoder) invalid(key string, err error) {
	d.errs = append(d.errs, fmt.Errorf("%s: %w", key, err))
	d.failed[key] = true
}

// decodeError drops the message of a syntax error, which can quote the
// offending text, such as a pasted secret (R31), and keeps its position.
// Type mismatch errors name only the key and types, so they pass through.
func decodeError(err error) error {
	var pe toml.ParseError
	if !errors.As(err, &pe) {
		return err
	}
	msg := fmt.Sprintf("invalid TOML at line %d, column %d", pe.Position.Line, pe.Position.Col)
	if pe.LastKey != "" {
		msg += fmt.Sprintf(" (last key %q)", displayKey(strings.Split(pe.LastKey, ".")))
	}
	return errors.New(msg)
}

// maxShownSegment is the longest key segment shown verbatim; it matches the
// longest label.
const maxShownSegment = 40

// keySegment matches a segment that looks like a key someone meant to write.
var keySegment = regexp.MustCompile(`^[a-z0-9_-]+$`)

// displayKey joins k for an error message, each segment as displaySegment
// shows it.
func displayKey(k toml.Key) string {
	parts := make([]string, len(k))
	for i, seg := range k {
		parts[i] = displaySegment(seg)
	}
	return strings.Join(parts, ".")
}

// displaySegment returns seg for an error message. A segment that is too
// long or has unusual characters may be a secret pasted as a bare key, so it
// is shortened instead of echoed (R31); so is one holding a token, which
// can be short enough to pass the other checks (0003 SPEC R277). Upper
// case alone is shown verbatim because it is what makes keys such as "MAX"
// unknown.
func displaySegment(seg string) string {
	n := utf8.RuneCountInString(seg)
	if n <= maxShownSegment && keySegment.MatchString(strings.ToLower(seg)) && !ContainsCredential(seg) {
		return seg
	}
	return fmt.Sprintf("%s…(%d chars)", string([]rune(seg)[:min(n, 4)]), n)
}
