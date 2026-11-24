// Package config loads and validates the scaler's TOML configuration file.
package config

import (
	"errors"
	"fmt"
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
	DefaultPoolName       = "linux-arm64-deploy"
	DefaultMax            = 2
	DefaultIdle           = 0
	DefaultRunnerCPUs     = 1.0
	DefaultRunnerMemory   = 1 * Gibibyte
	DefaultRunnerPids     = 512
	DefaultBuildCPUs      = 2.0
	DefaultBuildMemory    = 2 * Gibibyte
	DefaultBuildPids      = 1024
)

// DefaultAllowedEvents returns the default for pools.allowed-events.
func DefaultAllowedEvents() []string {
	return []string{"push"}
}

// Config is the content of the configuration file. The toml tags name the
// keys; decodeFile maps each key explicitly, and the example file test keeps
// the two in step.
type Config struct {
	GitHub   GitHub       `toml:"github"`
	Runner   ImageSection `toml:"runner"`
	BuildKit ImageSection `toml:"buildkit"`
	JobCheck JobCheck     `toml:"jobcheck"`
	Shutdown Shutdown     `toml:"shutdown"`
	// Pools is checked separately by the example file test.
	Pools []Pool `toml:"-"`
}

// GitHub holds the GitHub App credentials shared by all pools. The private
// key is referenced by path only, never held in the configuration.
type GitHub struct {
	AppClientID       string `toml:"app-client-id"`
	AppInstallationID int64  `toml:"app-installation-id"`
	AppPrivateKeyFile string `toml:"app-private-key-file"`
}

// ImageSection names a container image that must already exist locally.
type ImageSection struct {
	Image string `toml:"image"`
}

// JobCheck configures the job check server that runner containers query.
type JobCheck struct {
	Listen string `toml:"listen"`
}

// Shutdown configures draining on a stop signal or error.
type Shutdown struct {
	DrainTimeout time.Duration `toml:"drain-timeout"`
}

// Pool is one [[pools]] entry, served by one runner scale set.
type Pool struct {
	Name             string   `toml:"name"`
	RepoURL          string   `toml:"repo-url"`
	Max              int      `toml:"max"`
	Idle             int      `toml:"idle"`
	RunnerCPUs       float64  `toml:"runner-cpus"`
	RunnerMemory     ByteSize `toml:"runner-memory"`
	RunnerPids       int64    `toml:"runner-pids"`
	BuildCPUs        float64  `toml:"build-cpus"`
	BuildMemory      ByteSize `toml:"build-memory"`
	BuildPids        int64    `toml:"build-pids"`
	AllowedWorkflows []string `toml:"allowed-workflows"`
	AllowedEvents    []string `toml:"allowed-events"`
}

// Default returns a Config with every optional top-level key set to its
// default and no pools.
func Default() Config {
	return Config{
		JobCheck: JobCheck{Listen: DefaultJobCheckListen},
		Shutdown: Shutdown{DrainTimeout: DefaultDrainTimeout},
	}
}

// DefaultPool returns a Pool with every optional key set to its default.
func DefaultPool() Pool {
	return Pool{
		Name:          DefaultPoolName,
		Max:           DefaultMax,
		Idle:          DefaultIdle,
		RunnerCPUs:    DefaultRunnerCPUs,
		RunnerMemory:  DefaultRunnerMemory,
		RunnerPids:    DefaultRunnerPids,
		BuildCPUs:     DefaultBuildCPUs,
		BuildMemory:   DefaultBuildMemory,
		BuildPids:     DefaultBuildPids,
		AllowedEvents: DefaultAllowedEvents(),
	}
}

// Load reads the TOML file at path, applies defaults and validates the
// result. Every unknown key, value of the wrong TOML type, malformed size or
// duration and invalid value is reported together in one error joined with
// errors.Join, in the same order on every run. Only a read or syntax error
// is returned alone.
func Load(path string) (*Config, error) {
	cfg, problems, failed, err := decodeFile(path)
	if err != nil {
		return nil, err
	}
	if err := errors.Join(append(problems, cfg.problems(failed)...)...); err != nil {
		return nil, err
	}
	return cfg, nil
}

// fields maps each key of a table to the variable its value decodes into.
// The lookup is exact because TOML keys are case-sensitive, unlike the
// decoder's own matching of keys to struct fields.
type fields map[string]any

// decodeFile decodes path over the defaults. It returns type errors, unknown
// keys and malformed sizes and durations as problems, the keys and tables
// that failed to decode (such as "github.app-client-id" or "pools[0]") in
// failed, and a read or syntax error as err.
//
// Each table is decoded key by key in sorted order: the decoder walks a
// table in map order and stops at the first bad key, which would report a
// random one of several bad keys and hide the rest.
func decodeFile(path string) (*Config, []error, map[string]bool, error) {
	var top map[string]toml.Primitive
	md, err := toml.DecodeFile(path, &top)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("config %s: %w", path, decodeError(err))
	}

	d := &decoder{md: md, path: path, failed: make(map[string]bool)}
	c := Default()
	for _, name := range slices.Sorted(maps.Keys(top)) {
		raw := top[name]
		switch name {
		case "github":
			d.table(name, raw, fields{
				"app-client-id":        &c.GitHub.AppClientID,
				"app-installation-id":  &c.GitHub.AppInstallationID,
				"app-private-key-file": &c.GitHub.AppPrivateKeyFile,
			})
		case "runner":
			d.table(name, raw, fields{"image": &c.Runner.Image})
		case "buildkit":
			d.table(name, raw, fields{"image": &c.BuildKit.Image})
		case "jobcheck":
			d.table(name, raw, fields{"listen": &c.JobCheck.Listen})
		case "shutdown":
			var timeout text
			decoded := d.table(name, raw, fields{"drain-timeout": &timeout})
			if decoded["drain-timeout"] {
				d.duration(name+".drain-timeout", timeout, &c.Shutdown.DrainTimeout)
			}
		case "pools":
			c.Pools = d.pools(raw)
		default:
			d.unknown("", name)
		}
	}
	return &c, d.errs, d.failed, nil
}

// decoder collects decoding problems for one file.
type decoder struct {
	md     toml.MetaData
	path   string
	errs   []error
	failed map[string]bool
}

// pools decodes the [[pools]] array entry by entry.
func (d *decoder) pools(raw toml.Primitive) []Pool {
	var entries []toml.Primitive
	if err := d.md.PrimitiveDecode(raw, &entries); err != nil {
		d.typeError("pools", err)
		return nil
	}
	pools := make([]Pool, 0, len(entries))
	for i, entry := range entries {
		// A failed entry still counts toward the number of pools.
		pools = append(pools, d.pool(fmt.Sprintf("pools[%d]", i), entry))
	}
	return pools
}

// pool decodes one entry over DefaultPool, so keys left out keep their
// defaults while an explicit zero still reaches validation.
func (d *decoder) pool(name string, raw toml.Primitive) Pool {
	p := DefaultPool()
	var runnerMemory, buildMemory text
	decoded := d.table(name, raw, fields{
		"name":              &p.Name,
		"repo-url":          &p.RepoURL,
		"max":               &p.Max,
		"idle":              &p.Idle,
		"runner-cpus":       &p.RunnerCPUs,
		"runner-memory":     &runnerMemory,
		"runner-pids":       &p.RunnerPids,
		"build-cpus":        &p.BuildCPUs,
		"build-memory":      &buildMemory,
		"build-pids":        &p.BuildPids,
		"allowed-workflows": &p.AllowedWorkflows,
		"allowed-events":    &p.AllowedEvents,
	})
	if decoded["runner-memory"] {
		d.byteSize(name+".runner-memory", runnerMemory, &p.RunnerMemory)
	}
	if decoded["build-memory"] {
		d.byteSize(name+".build-memory", buildMemory, &p.BuildMemory)
	}
	return p
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
	d.errs = append(d.errs, fmt.Errorf("config %s: %s: %w", d.path, key, decodeError(err)))
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
// longest pool name.
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
// is shortened instead of echoed (R31). Upper case alone is shown verbatim
// because it is what makes keys such as "MAX" unknown.
func displaySegment(seg string) string {
	n := utf8.RuneCountInString(seg)
	if n <= maxShownSegment && keySegment.MatchString(strings.ToLower(seg)) {
		return seg
	}
	return fmt.Sprintf("%s…(%d chars)", string([]rune(seg)[:min(n, 4)]), n)
}
