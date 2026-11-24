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
	"slices"
	"strconv"
	"strings"
	"syscall"
)

// Lower bounds from the settings table in SPEC.md.
const (
	MinMemory = 256 * Mebibyte
	MinPids   = 64
	// MinCPUs is the smallest CPU limit. Docker takes the limit as integer
	// NanoCPUs (CPUs × 1e9).
	MinCPUs = 0.01
)

// maxCPUs is the largest CPU limit whose NanoCPUs value fits in an int64.
const maxCPUs = math.MaxInt64 / 1e9

// SupportedPools is the number of [[pools]] entries this version accepts.
const SupportedPools = 1

// maxKeyFileSize bounds how much of the private key path is read; a GitHub
// App key is a few kilobytes.
const maxKeyFileSize = 64 * 1024

// allowedEvents are the only events allowed to run deploy jobs: events where
// outside input cannot make main's code run.
func allowedEvents() []string {
	return []string{"push", "workflow_dispatch", "schedule"}
}

var (
	runnerImagePattern   = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	buildkitImagePattern = regexp.MustCompile(`^moby/buildkit:[A-Za-z0-9_][A-Za-z0-9_.-]*-rootless@sha256:[0-9a-f]{64}$`)
	// Runner containers are named "<pool>-<hex>", and Docker requires a
	// container name to start with a letter or digit.
	poolNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,39}$`)
	repoURLPattern  = regexp.MustCompile(`^https://github\.com/[A-Za-z0-9-]+/[A-Za-z0-9._-]+$`)
	// Workflow files live directly in .github/workflows and end in .yml or
	// .yaml. '@' is excluded because the gate splits JobWorkflowRef at '@'.
	workflowPattern = regexp.MustCompile(`^\.github/workflows/[^/@]+\.ya?ml$`)
)

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
	p.check(buildkitImagePattern.MatchString(c.BuildKit.Image),
		"buildkit.image", "must be moby/buildkit:<version>-rootless@sha256:<64 hex digits>, got %q", c.BuildKit.Image)
	p.listen("jobcheck.listen", c.JobCheck.Listen)
	p.check(c.Shutdown.DrainTimeout > 0,
		"shutdown.drain-timeout", "must be greater than 0, got %s", c.Shutdown.DrainTimeout)
	p.check(len(c.Pools) == SupportedPools,
		"pools", "this version supports exactly %d pool, got %d", SupportedPools, len(c.Pools))
	for i, pool := range c.Pools {
		p.pool(fmt.Sprintf("pools[%d]", i), pool)
	}
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
	p.check(g.AppInstallationID > 0,
		"github.app-installation-id", "must be a positive integer, got %d", g.AppInstallationID)
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

func (p *problems) pool(prefix string, pool Pool) {
	key := func(name string) string { return prefix + "." + name }

	p.check(poolNamePattern.MatchString(pool.Name),
		key("name"), "must be 1-40 characters of a-z, 0-9 and -, starting with a-z or 0-9, got %q", pool.Name)
	p.check(validRepoURL(pool.RepoURL),
		key("repo-url"), "must be https://github.com/<owner>/<repo> without a .git suffix, got %q", pool.RepoURL)
	p.check(pool.Max >= 1,
		key("max"), "must be at least 1, got %d", pool.Max)
	p.check(pool.Idle >= 0 && pool.Idle <= pool.Max,
		key("idle"), "must be between 0 and max (%d), got %d", pool.Max, pool.Idle)

	p.limits(key, "runner", pool.RunnerCPUs, pool.RunnerMemory, pool.RunnerPids)
	p.limits(key, "build", pool.BuildCPUs, pool.BuildMemory, pool.BuildPids)

	p.check(len(pool.AllowedWorkflows) > 0,
		key("allowed-workflows"), "must list at least one workflow")
	for _, w := range pool.AllowedWorkflows {
		p.check(workflowPattern.MatchString(w),
			key("allowed-workflows"), "entry must be .github/workflows/<file>.yml or .yaml without '@', got %q", w)
	}
	p.check(len(pool.AllowedEvents) > 0,
		key("allowed-events"), "must list at least one of %s", strings.Join(allowedEvents(), ", "))
	for _, e := range pool.AllowedEvents {
		p.check(slices.Contains(allowedEvents(), e),
			key("allowed-events"), "entry must be one of %s, got %q", strings.Join(allowedEvents(), ", "), e)
	}
}

// limits checks one container's <kind>-cpus, <kind>-memory and <kind>-pids.
func (p *problems) limits(key func(string) string, kind string, cpus float64, memory ByteSize, pids int64) {
	// Both comparisons are false for NaN, so NaN fails the check; +Inf fails
	// the upper bound.
	p.check(cpus >= MinCPUs && cpus < maxCPUs,
		key(kind+"-cpus"), "must be at least %v and fit in int64 nanoCPUs, got %v", MinCPUs, cpus)
	p.check(memory >= MinMemory,
		key(kind+"-memory"), "must be at least %s, got %s", MinMemory, memory)
	p.check(pids >= MinPids,
		key(kind+"-pids"), "must be at least %d, got %d", MinPids, pids)
}

func validRepoURL(s string) bool {
	if !repoURLPattern.MatchString(s) {
		return false
	}
	repo := s[strings.LastIndexByte(s, '/')+1:]
	// A .git suffix never matches the repository name GitHub reports, so the
	// gate would deny every job.
	return repo != "." && repo != ".." && !strings.HasSuffix(strings.ToLower(repo), ".git")
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
