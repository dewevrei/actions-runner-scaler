package config

import (
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
)

// Lower bounds from the settings table in the SPEC (intent/0001-deploy-runner-autoscale/SPEC.md).
// They apply to [runner] and [buildkit] (0002 SPEC R121, R122).
const (
	MinMemory = 256 * Mebibyte
	MinPids   = 64
	// MinCPUs is the smallest CPU limit. Docker takes the limit as integer
	// NanoCPUs (CPUs × 1e9).
	MinCPUs = 0.01
)

// maxCPUs is the largest CPU limit whose NanoCPUs value fits in an int64.
const maxCPUs = math.MaxInt64 / 1e9

// maxKeyFileSize bounds how much of the private key path is read; a GitHub
// App key is a few kilobytes.
const maxKeyFileSize = 64 * 1024

var (
	runnerImagePattern   = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	buildkitImagePattern = regexp.MustCompile(`^moby/buildkit:[A-Za-z0-9_][A-Za-z0-9_.-]*-rootless@sha256:[0-9a-f]{64}$`)
	// Runner containers are named "<label>-<hex>", and Docker requires a
	// container name to start with a letter or digit.
	labelPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,39}$`)
	// repoURLPattern, with the checks of repoURL, is the rule of
	// github.ParseTargetURL (0002 SPEC R231); a test keeps the two in step.
	repoURLPattern = regexp.MustCompile(`^https://github\.com/([A-Za-z0-9-]+)/([A-Za-z0-9._-]+)$`)
	// ownerURLPattern is an organization or account URL (0002 SPEC R232).
	ownerURLPattern = regexp.MustCompile(`^https://github\.com/([A-Za-z0-9-]+)$`)
)

// Message of an organization or account URL in targets.url (0002 SPEC R232,
// Design 설정 파일).
const msgOwnerURL = `%q names an organization or account; list each repository as its own [[targets]] url = "https://github.com/<owner>/<repo>" and set its max-runners (default 1)`

// Validate reports every problem in c at once, joined with errors.Join:
// missing or out-of-range values and an unusable App private key file.
func (c *Config) Validate() error {
	return errors.Join(c.problems(nil)...)
}

// problems validates c except the keys in skip, and the keys inside a table
// in skip, which failed to decode and hold defaults or partial values.
func (c *Config) problems(skip map[string]bool) []error {
	p := problems{skip: skip}
	p.github(c.GitHub)
	p.check(runnerImagePattern.MatchString(c.Runner.Image),
		"runner.image", "must be a local image ID sha256:<64 hex digits>, got %q", c.Runner.Image)
	p.limits("runner", c.Runner)
	p.check(buildkitImagePattern.MatchString(c.BuildKit.Image),
		"buildkit.image", "must be moby/buildkit:<version>-rootless@sha256:<64 hex digits>, got %q", c.BuildKit.Image)
	p.limits("buildkit", c.BuildKit)
	p.listen("jobcheck.listen", c.JobCheck.Listen)
	p.check(c.Shutdown.DrainTimeout > 0,
		"shutdown.drain-timeout", "must be greater than 0, got %s", c.Shutdown.DrainTimeout)
	p.check(labelPattern.MatchString(c.Scaling.Label),
		"scaling.label", "must be 1-40 characters of a-z, 0-9 and -, starting with a-z or 0-9, got %q", c.Scaling.Label)
	p.check(len(c.Targets) >= 1,
		"targets", "at least 1 [[targets]] entry is required, got %d", len(c.Targets))
	for i, t := range c.Targets {
		p.target(fmt.Sprintf("targets[%d]", i), t)
	}
	p.overlapping(c.Targets)
	return p.errs
}

// problems collects validation errors, each prefixed with its key.
//
// Messages quote offending values with %q. Configuration values are not
// secrets by contract: secrets are referenced by path only (SPEC settings
// table), and the redacting logger is the backstop (R31).
type problems struct {
	errs []error
	skip map[string]bool
}

// skipped reports whether key, or a table that holds it, is in p.skip.
func (p *problems) skipped(key string) bool {
	for {
		if p.skip[key] {
			return true
		}
		i := strings.LastIndexByte(key, '.')
		if i < 0 {
			return false
		}
		key = key[:i]
	}
}

func (p *problems) check(ok bool, key, format string, args ...any) {
	if !ok {
		p.add(key, fmt.Errorf(format, args...))
	}
}

// add records err under key, one entry per error when err is joined.
func (p *problems) add(key string, err error) {
	if p.skipped(key) {
		return
	}
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		for _, e := range joined.Unwrap() {
			p.add(key, e)
		}
		return
	}
	p.errs = append(p.errs, fmt.Errorf("%s: %w", key, err))
}

func (p *problems) github(g GitHub) {
	p.check(strings.TrimSpace(g.AppClientID) != "",
		"github.app-client-id", "is required")
	const key = "github.app-private-key-file"
	path := g.AppPrivateKeyFile
	switch {
	case path == "":
		p.check(false, key, "is required")
	// Key content gets ReadAppPrivateKey's more helpful message. The value is
	// not echoed because it may be a pasted secret (R31).
	case !looksLikeKeyContent(path) && !filepath.IsAbs(path):
		p.check(false, key, "must be an absolute path")
	default:
		if _, err := ReadAppPrivateKey(path); err != nil {
			p.add(key, err)
		}
	}
}

func (p *problems) listen(key, addr string) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		p.check(false, key, "must be <loopback IP>:<port>, got %q", addr)
		return
	}
	ip, err := netip.ParseAddr(host)
	p.check(err == nil && ip.IsLoopback(), key, "host must be a loopback IP address, got %q", host)
	n, err := strconv.ParseUint(port, 10, 16)
	p.check(err == nil && n > 0, key, "port must be 1-65535, got %q", port)
}

func (p *problems) target(prefix string, t Target) {
	key := func(name string) string { return prefix + "." + name }

	switch _, ok := repoURL(t.URL); {
	case ok:
	case ownerURLPattern.MatchString(t.URL):
		p.check(false, key("url"), msgOwnerURL, t.URL)
	default:
		p.check(false, key("url"), `must be https://github.com/<owner>/<repo> without a trailing "/" or ".git", got %q`, t.URL)
	}
	p.check(t.MaxRunners >= 1,
		key("max-runners"), "must be at least 1, got %d", t.MaxRunners)
	p.check(t.Idle >= 0 && t.Idle <= t.MaxRunners,
		key("idle"), "must be between 0 and max-runners (%d), got %d", t.MaxRunners, t.Idle)
}

// repoURL returns the lower-case <owner>/<repo> of s when s follows the
// rule of 0002 SPEC R231: repoURLPattern, an owner other than "enterprises",
// a repository other than "." and "..", and no ".git" suffix, compared
// without case.
func repoURL(s string) (key string, ok bool) {
	m := repoURLPattern.FindStringSubmatch(s)
	if m == nil {
		return "", false
	}
	owner, repo := strings.ToLower(m[1]), strings.ToLower(m[2])
	if owner == "enterprises" || repo == "." || repo == ".." || strings.HasSuffix(repo, ".git") {
		return "", false
	}
	return owner + "/" + repo, true
}

// overlapping reports targets that name the same repository, owner and
// repository compared without case, in one error that lists every value
// involved (0002 SPEC R233). Entries whose url failed to decode or does not
// follow R231 are left out; they have their own error.
func (p *problems) overlapping(targets []Target) {
	keys := make([]string, len(targets))
	count := make(map[string]int)
	for i, t := range targets {
		k, ok := repoURL(t.URL)
		if !ok || p.skipped(fmt.Sprintf("targets[%d].url", i)) {
			continue
		}
		keys[i] = k
		count[k]++
	}
	var urls []string
	for i, t := range targets {
		if keys[i] != "" && count[keys[i]] > 1 {
			urls = append(urls, t.URL)
		}
	}
	p.check(len(urls) == 0,
		"targets", "repository listed more than once (owner and repository names compare without case): %s", strings.Join(urls, ", "))
}

// limits checks the cpus, memory and pids of the table named kind.
func (p *problems) limits(kind string, c Container) {
	// Both comparisons are false for NaN, so NaN fails the check; +Inf fails
	// the upper bound.
	p.check(c.CPUs >= MinCPUs && c.CPUs < maxCPUs,
		kind+".cpus", "must be at least %v and fit in int64 nanoCPUs, got %v", MinCPUs, c.CPUs)
	p.check(c.Memory >= MinMemory,
		kind+".memory", "must be at least %s, got %s", MinMemory, c.Memory)
	p.check(c.Pids >= MinPids,
		kind+".pids", "must be at least %d, got %d", MinPids, c.Pids)
}

// ReadAppPrivateKey reads the GitHub App private key at path. It accepts a
// regular file (a symbolic link counts as its target) of at most 64 KiB
// with mode 0600 or narrower and no setuid, setgid or sticky bit, holding
// an unencrypted RSA key in PKCS#1 or PKCS#8 PEM form. Errors contain
// neither the path nor parser details: a key pasted in place of the path,
// or parser output, would put key material into logs and status (R31).
func ReadAppPrivateKey(path string) (*rsa.PrivateKey, error) {
	if looksLikeKeyContent(path) {
		return nil, errors.New("value looks like key content, not a file path; set the path of the key file")
	}
	// The type is checked before the open, which then cannot block on a
	// FIFO or device in the common case, and again on the open file.
	info, err := os.Stat(path)
	if err != nil {
		return nil, withoutPath(err)
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("not a regular file")
	}
	f, info, err := openRegular(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var modeErr error
	if mode := info.Mode(); mode.Perm()&^0o600 != 0 || mode&(fs.ModeSetuid|fs.ModeSetgid|fs.ModeSticky) != 0 {
		modeErr = fmt.Errorf("file mode %v is wider than 0600 or has special bits; use 0600 or 0400", mode)
	}

	data, err := io.ReadAll(io.LimitReader(f, maxKeyFileSize+1))
	if err != nil {
		return nil, errors.Join(modeErr, withoutPath(err))
	}
	if len(data) > maxKeyFileSize {
		return nil, errors.Join(modeErr, fmt.Errorf("file is larger than %d bytes", maxKeyFileSize))
	}
	key, parseErr := parseRSAPrivateKey(data)
	if err := errors.Join(modeErr, parseErr); err != nil {
		return nil, err
	}
	return key, nil
}

// openRegular opens path for reading and returns it with its FileInfo only
// if it is a regular file. O_NONBLOCK keeps the open from blocking when the
// path was replaced by a FIFO after an earlier Stat: a read-only open of a
// FIFO with O_NONBLOCK returns at once (POSIX open(2); TestOpenRegularFIFO
// blocks without the flag). Reads of a regular file do not block either
// way, and Go keeps regular files out of its poller on macOS (Go 1.27
// os/file_unix.go newFile). Errors do not contain path.
func openRegular(path string) (*os.File, fs.FileInfo, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, nil, withoutPath(err)
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, nil, withoutPath(err)
	}
	if !info.Mode().IsRegular() {
		f.Close()
		return nil, nil, errors.New("not a regular file")
	}
	return f, info, nil
}

func looksLikeKeyContent(path string) bool {
	return strings.Contains(path, "-----BEGIN") || strings.ContainsAny(path, "\r\n")
}

// withoutPath keeps the operation and cause of a file error but drops the
// path it carries.
func withoutPath(err error) error {
	var pe *fs.PathError
	if errors.As(err, &pe) {
		return fmt.Errorf("%s file: %w", pe.Op, pe.Err)
	}
	return errors.New("cannot read the file")
}

func parseRSAPrivateKey(data []byte) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode(data)
	if block == nil {
		return nil, errors.New("no PEM block found")
	}
	switch block.Type {
	case "RSA PRIVATE KEY":
		key, err := x509.ParsePKCS1PrivateKey(block.Bytes)
		if err != nil {
			return nil, errors.New("invalid PKCS#1 RSA private key")
		}
		return key, nil
	case "PRIVATE KEY":
		parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
		if err != nil {
			return nil, errors.New("invalid PKCS#8 private key")
		}
		key, ok := parsed.(*rsa.PrivateKey)
		if !ok {
			return nil, errors.New("PKCS#8 private key is not an RSA key")
		}
		return key, nil
	default:
		return nil, errors.New(`PEM block must be "RSA PRIVATE KEY" (PKCS#1) or "PRIVATE KEY" (PKCS#8)`)
	}
}
