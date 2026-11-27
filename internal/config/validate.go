package config

import (
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"net"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"unicode"
)

// Lower bounds of [runner] and [buildkit] (0002 SPEC R121, R122), Docker's
// own: a memory limit of at least 6m (Docker docs, resource constraints) and
// a positive pids limit.
const (
	MinMemory = 6 * Mebibyte
	MinPids   = 1
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
	// Runner containers are named "<label>-<hex>", and Docker requires a
	// container name to start with a letter or digit.
	labelPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,39}$`)
	// repoURLPattern, with the checks of repoURL, is the rule of
	// github.ParseTargetURL (0002 SPEC R231); a test keeps the two in step.
	repoURLPattern = regexp.MustCompile(`^https://github\.com/([A-Za-z0-9-]+)/([A-Za-z0-9._-]+)$`)
	// ownerURLPattern, with an owner other than "enterprises", is an
	// organization or account URL (0002 SPEC R232); see ownerURL.
	ownerURLPattern = regexp.MustCompile(`^https://github\.com/([A-Za-z0-9-]+)$`)
)

// Message of an organization or account URL in targets.url (0002 SPEC R232,
// Design 설정 파일).
const msgOwnerURL = `%q names an organization or account; list each repository as its own [[targets]] url = "https://github.com/<owner>/<repo>" and set its max-runners (default 1)`

// Messages of the credential rules (0003 SPEC R272, R274), after "github: "
// or the target's key.
const (
	msgCredentialConflict  = "set either app-client-id and app-private-key-file, or token-file, not both"
	msgNoDefaultCredential = "no default credential (app-client-id and app-private-key-file, or token-file) for targets without token-file: %s"

	// msgTargetApp refuses a per-target GitHub App, which is read and loaded
	// but not yet used.
	msgTargetApp = "a per-target GitHub App is not supported yet; use token-file or the [github] App"
)

// Messages of docker.host and log.level. The host is not quoted: a tcp URL
// may carry a user name or password.
const (
	msgDockerHost = "must be unix://<absolute socket path> or tcp://<host>:<port>"
	msgLogLevel   = "must be debug, info, warn or error, got %q"
)

// Validate reports every problem in c at once, joined with errors.Join:
// missing or out-of-range values and unusable App key and token files.
func (c *Config) Validate() error {
	return errors.Join(c.validate(nil).errors()...)
}

// validate checks c except the keys in skip, and the keys inside a table in
// skip, which failed to decode and hold defaults or partial values.
func (c *Config) validate(skip map[string]bool) *problems {
	p := &problems{skip: skip}
	p.credentials(c)
	p.image("runner.image", c.Runner.Image)
	p.limits("runner", c.Runner)
	p.image("buildkit.image", c.BuildKit.Image)
	p.limits("buildkit", c.BuildKit)
	p.listen("jobcheck.listen", c.JobCheck.Listen)
	p.check(c.Shutdown.DrainTimeout > 0,
		"shutdown.drain-timeout", "must be greater than 0, got %s", c.Shutdown.DrainTimeout)
	p.check(labelPattern.MatchString(c.Scaling.Label),
		"scaling.label", "must be 1-40 characters of a-z, 0-9 and -, starting with a-z or 0-9, got %q", c.Scaling.Label)
	p.check(c.Scaling.ServerLimit >= 0,
		"scaling.server-limit", "must be 0 (the value computed from Docker) or greater, got %d", c.Scaling.ServerLimit)
	p.check(validDockerHost(c.Docker.Host), "docker.host", msgDockerHost)
	_, ok := parseLogLevel(c.Log.Level)
	p.check(ok, "log.level", msgLogLevel, c.Log.Level)
	p.check(len(c.Targets) >= 1,
		"targets", "at least 1 [[targets]] entry is required, got %d", len(c.Targets))
	for i, t := range c.Targets {
		p.target(fmt.Sprintf("targets[%d]", i), t)
	}
	p.overlapping(c.Targets)
	return p
}

// problems collects validation errors, each prefixed with its key, and the
// result of every key and token file check.
//
// Messages quote offending values with %q. Configuration values are not
// secrets by contract: secrets are referenced by path only (SPEC settings
// table), values that look like secrets are refused without being quoted
// (0003 SPEC R276, R314), and the redacting logger is the backstop (R31).
type problems struct {
	found []finding
	files []FileCheck
	skip  map[string]bool
}

// finding is one validation error. file marks the errors of a key or token
// file check, which Inspect reports apart (0003 SPEC R304).
type finding struct {
	key  string
	err  error
	file bool
}

// errors returns every error in the order found.
func (p *problems) errors() []error {
	errs := make([]error, len(p.found))
	for i, f := range p.found {
		errs[i] = f.err
	}
	return errs
}

// skipped reports whether key, or a table that holds it, is in p.skip.
func (p *problems) skipped(key string) bool {
	return inTable(p.skip, key)
}

// inTable reports whether key, or a table that holds it, is in keys.
func inTable(keys map[string]bool, key string) bool {
	for {
		if keys[key] {
			return true
		}
		i := strings.LastIndexByte(key, '.')
		if i < 0 {
			return false
		}
		key = key[:i]
	}
}

// set reports whether key holds value or failed to decode; a key that
// failed to decode was written in the file all the same.
func (p *problems) set(key, value string) bool {
	return value != "" || p.skipped(key)
}

func (p *problems) check(ok bool, key, format string, args ...any) {
	if !ok {
		p.add(key, fmt.Errorf(format, args...))
	}
}

// add records err under key, one entry per error when err is joined.
func (p *problems) add(key string, err error) {
	p.record(key, err, false)
}

func (p *problems) record(key string, err error, file bool) {
	if p.skipped(key) {
		return
	}
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		for _, e := range joined.Unwrap() {
			p.record(key, e, file)
		}
		return
	}
	p.found = append(p.found, finding{key: key, err: fmt.Errorf("%s: %w", key, err), file: file})
}

// file records the check of the key or token file that key names, passed
// (err nil) or not.
func (p *problems) file(key string, err error) {
	if p.skipped(key) {
		return
	}
	p.files = append(p.files, FileCheck{Key: key, Err: err})
	if err != nil {
		p.record(key, err, true)
	}
}

// credentials checks the default credential and every token file (0003
// SPEC R271~R276). A file with both App keys and no token-file, the 0002
// format, gets the 0002 checks in the 0002 order (R271).
func (p *problems) credentials(c *Config) {
	const (
		clientKey = "github.app-client-id"
		appKey    = "github.app-private-key-file"
		tokenKey  = "github.token-file"
	)
	g := c.GitHub
	app := p.set(clientKey, g.AppClientID) || p.set(appKey, g.AppPrivateKeyFile)
	token := p.set(tokenKey, g.TokenFile)
	switch {
	case app && token:
		p.add("github", errors.New(msgCredentialConflict))
	case app:
		p.check(strings.TrimSpace(g.AppClientID) != "", clientKey, "is required")
		p.check(g.AppPrivateKeyFile != "", appKey, "is required")
	case !token:
		p.noDefaultCredential(c.Targets)
	}
	if g.AppPrivateKeyFile != "" {
		p.file(appKey, checkAppKeyFile(g.AppPrivateKeyFile))
	}
	if g.TokenFile != "" {
		p.file(tokenKey, checkTokenFile(g.TokenFile))
	}
}

// targetCredential checks the credential of the target named prefix: a token
// file or an App pair, never both and never half a pair, with the checks of
// the default credential. A target with neither uses the default. A target
// with App keys is refused first: nothing outside this package uses them yet.
func (p *problems) targetCredential(prefix string, t Target) {
	var (
		clientKey = prefix + ".app-client-id"
		appKey    = prefix + ".app-private-key-file"
		tokenKey  = prefix + ".token-file"
	)
	app := p.set(clientKey, t.AppClientID) || p.set(appKey, t.AppPrivateKeyFile)
	token := p.set(tokenKey, t.TokenFile)
	// TODO(wiring): remove this refusal when the daemon, delete-scaleset and
	// doctor pick the credential of a target from its App keys
	// (Credentials.TargetAppKeys). Until then such a target would run on the
	// default credential without a word.
	if app {
		p.add(prefix, errors.New(msgTargetApp))
	}
	switch {
	case app && token:
		p.add(prefix, errors.New(msgCredentialConflict))
	case app:
		p.check(strings.TrimSpace(t.AppClientID) != "", clientKey, "is required")
		p.check(t.AppPrivateKeyFile != "", appKey, "is required")
	}
	if t.AppPrivateKeyFile != "" {
		p.file(appKey, checkAppKeyFile(t.AppPrivateKeyFile))
	}
	if t.TokenFile != "" {
		p.file(tokenKey, checkTokenFile(t.TokenFile))
	}
}

// noDefaultCredential reports, in one error, the targets that would use the
// missing default credential (0003 SPEC R274).
func (p *problems) noDefaultCredential(targets []Target) {
	var shown []string
	for i, t := range targets {
		prefix := fmt.Sprintf("targets[%d]", i)
		if p.set(prefix+".token-file", t.TokenFile) {
			continue
		}
		shown = append(shown, p.shownURL(prefix, t.URL))
	}
	p.check(len(shown) == 0, "github", msgNoDefaultCredential, strings.Join(shown, ", "))
}

// shownURL returns how a list of targets shows the target named prefix:
// prefix, followed by its url in parentheses, as written when it is a
// repository URL and quoted when it is not. A url that failed to decode or
// holds a credential (0003 SPEC R314) is left out.
func (p *problems) shownURL(prefix, url string) string {
	switch _, ok := repoURL(url); {
	case p.skipped(prefix+".url") || ContainsCredential(url):
		return prefix
	case ok:
		return prefix + " (" + url + ")"
	default:
		return fmt.Sprintf("%s (%q)", prefix, url)
	}
}

// checkAppKeyFile checks the App private key file value path (R30).
func checkAppKeyFile(path string) error {
	// Key content gets ReadAppPrivateKey's more helpful message. The value is
	// not echoed because it may be a pasted secret (R31).
	if !looksLikeKeyContent(path) && !filepath.IsAbs(path) {
		return errors.New("must be an absolute path")
	}
	_, err := ReadAppPrivateKey(path)
	return err
}

// checkTokenFile checks a token file value path (0003 SPEC R275, R276).
func checkTokenFile(path string) error {
	_, err := ReadTokenFile(path)
	return err
}

// image checks an image value (0003 SPEC R318). The value is quoted like
// the other keys' values.
func (p *problems) image(key, value string) {
	if err := CheckImage(value); err != nil {
		p.add(key, fmt.Errorf("%w, got %q", err, value))
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

	if err := ValidateRepositoryURL(t.URL); err != nil {
		p.add(key("url"), err)
	}
	p.check(t.MaxRunners >= 1,
		key("max-runners"), "must be at least 1, got %d", t.MaxRunners)
	p.check(t.Idle >= 0 && t.Idle <= t.MaxRunners,
		key("idle"), "must be between 0 and max-runners (%d), got %d", t.MaxRunners, t.Idle)
	p.targetCredential(prefix, t)
}

// ValidateRepositoryURL checks a repository URL value with the rules of
// targets.url and returns their message without a key: a credential first
// (0003 SPEC R314), then an organization or account URL (0002 SPEC R232),
// then the repository URL form (0002 SPEC R231). Only the last two quote s.
func ValidateRepositoryURL(s string) error {
	switch _, ok := repoURL(s); {
	case ContainsCredential(s):
		return errors.New(msgCredentialURL)
	case ok:
		return nil
	case ownerURL(s):
		return fmt.Errorf(msgOwnerURL, s)
	default:
		return fmt.Errorf(`must be https://github.com/<owner>/<repo> without a trailing "/" or ".git", got %q`, s)
	}
}

// RepositoryKey returns the lower-case <owner>/<repo> by which targets are
// compared (0002 SPEC R233). It reports false for a value that breaks
// ValidateRepositoryURL's rules.
func RepositoryKey(s string) (string, bool) {
	if ContainsCredential(s) {
		return "", false
	}
	return repoURL(s)
}

// ownerURL reports whether s names an organization or account (0002 SPEC
// R232). An "enterprises" owner, compared without case, is not one and gets
// the R231 message.
func ownerURL(s string) bool {
	m := ownerURLPattern.FindStringSubmatch(s)
	return m != nil && !strings.EqualFold(m[1], "enterprises")
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
// involved (0002 SPEC R233). Entries whose url failed to decode, does not
// follow R231 or holds a credential (0003 SPEC R314) are left out; they
// have their own error.
func (p *problems) overlapping(targets []Target) {
	keys := make([]string, len(targets))
	count := make(map[string]int)
	for i, t := range targets {
		k, ok := RepositoryKey(t.URL)
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
// with no group or other permission bits, holding an unencrypted RSA key in
// PKCS#1 or PKCS#8 PEM form. Errors contain
// neither the path nor parser details: a key pasted in place of the path,
// or parser output, would put key material into logs and status (R31).
func ReadAppPrivateKey(path string) (*rsa.PrivateKey, error) {
	if looksLikeKeyContent(path) {
		return nil, errors.New("value looks like key content, not a file path; set the path of the key file")
	}
	data, modeErr, err := readSecretFile(path, maxKeyFileSize)
	if err != nil {
		return nil, errors.Join(modeErr, err)
	}
	key, parseErr := parseRSAPrivateKey(data)
	if err := errors.Join(modeErr, parseErr); err != nil {
		return nil, err
	}
	return key, nil
}

// checkSecretMode applies the mode rule of R30 to a key or token file
// (0003 SPEC R275): no group or other permission bit, as OpenSSH
// (authfile.c, st_mode & 077) and runpool (config/filepermissions.go) check
// their key files. Owner bits and special bits are not looked at.
func checkSecretMode(mode fs.FileMode) error {
	if mode.Perm()&0o077 != 0 {
		return fmt.Errorf("file mode %v is wider than 0600 (group or other bits are set); use 0600 or 0400", mode)
	}
	return nil
}

// validDockerHost reports whether host is a unix:// URL with an absolute
// socket path or a tcp:// URL with a host and a port, no user name, no query
// and no fragment. No character of host may be a control character.
func validDockerHost(host string) bool {
	if strings.ContainsFunc(host, unicode.IsControl) {
		return false
	}
	scheme, rest, ok := strings.Cut(host, "://")
	if !ok {
		return false
	}
	switch scheme {
	case "unix":
		return strings.HasPrefix(rest, "/") && len(rest) > 1
	case "tcp":
		if strings.ContainsAny(rest, "?#") {
			return false
		}
		u, err := url.Parse("tcp://" + rest)
		if err != nil || u.User != nil {
			return false
		}
		// SplitHostPort refuses an IPv6 address without brackets and a
		// host with more than one port.
		name, port, err := net.SplitHostPort(u.Host)
		if err != nil || name == "" {
			return false
		}
		n, err := strconv.ParseUint(port, 10, 16)
		return err == nil && n > 0
	}
	return false
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
