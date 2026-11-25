// Package service installs and removes the per-user LaunchAgent that keeps
// the scaler running (R19, R44).
package service

import (
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"math"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// Label is the launchd job label. The plist file is named <Label>.plist, the
// convention launchd.plist(5) states under XML PROPERTY LIST KEYS.
const Label = "io.github.dewevrei.actions-runner-scaler"

// exitTimeOutMargin is added to drain-timeout for ExitTimeOut: the scaler's
// finalization after a drain takes at most 55 seconds (PLAN M5-8), so 60
// leaves 5 seconds before launchd sends SIGKILL.
const exitTimeOutMargin = 60 * time.Second

// maxExitTimeOut caps ExitTimeOut at the signed 32-bit range. launchd.plist(5)
// types it only as <integer>; the width launchd stores it in is not
// documented, and a value past a 32-bit field could wrap to zero or a
// negative number, and zero means "infinity". The cap allows drains of
// about 68 years, so no real setting is refused.
const maxExitTimeOut = math.MaxInt32

// throttleInterval is the minimum seconds between launches (R44).
const throttleInterval = 60

// Log file names inside the log directory. The scaler logs JSON to stderr.
const (
	stdoutLog = "stdout.log"
	stderrLog = "stderr.log"
)

// Job describes the LaunchAgent. Every path must be absolute.
type Job struct {
	// Label identifies the job to launchd; normally the Label constant.
	Label string
	// Program is the scaler binary, the first element of ProgramArguments.
	Program string
	// ConfigPath is passed to "run -config".
	ConfigPath string
	// Home is the user's home directory. The job gets it as HOME, so the
	// paths the scaler derives from HOME do not depend on what launchd
	// sets, and the log directory is under it.
	Home string
	// DrainTimeout is shutdown.drain-timeout from the configuration.
	DrainTimeout time.Duration
}

// ExitTimeOut returns the seconds launchd waits between SIGTERM and
// SIGKILL: drain-timeout rounded up to a whole second, plus 60 (R44).
func (j Job) ExitTimeOut() int64 {
	secs := int64(j.DrainTimeout / time.Second)
	if j.DrainTimeout%time.Second != 0 {
		secs++
	}
	return secs + int64(exitTimeOutMargin/time.Second)
}

// LogDir is the directory of the stdout and stderr files, LogDir(j.Home).
func (j Job) LogDir() string { return LogDir(j.Home) }

// StdoutPath is the file launchd connects to the job's stdout.
func (j Job) StdoutPath() string { return filepath.Join(j.LogDir(), stdoutLog) }

// StderrPath is the file launchd connects to the job's stderr.
func (j Job) StderrPath() string { return filepath.Join(j.LogDir(), stderrLog) }

func (j Job) validate() error {
	var errs []error
	if j.Label == "" || strings.ContainsRune(j.Label, '/') {
		errs = append(errs, fmt.Errorf("label %q must be non-empty and contain no '/'", j.Label))
	}
	for _, p := range []struct{ name, path string }{
		{"program", j.Program}, {"config path", j.ConfigPath}, {"home", j.Home},
	} {
		if !filepath.IsAbs(p.path) {
			errs = append(errs, fmt.Errorf("%s %q must be an absolute path", p.name, p.path))
		}
	}
	if j.DrainTimeout <= 0 {
		errs = append(errs, fmt.Errorf("drain timeout must be greater than 0, got %s", j.DrainTimeout))
	} else if j.ExitTimeOut() > maxExitTimeOut {
		errs = append(errs, fmt.Errorf("drain timeout %s makes ExitTimeOut exceed %d seconds", j.DrainTimeout, maxExitTimeOut))
	}
	for _, s := range []string{j.Label, j.Program, j.ConfigPath, j.Home} {
		if !xmlText(s) {
			errs = append(errs, fmt.Errorf("%q has characters a plist cannot hold", s))
		}
	}
	return errors.Join(errs...)
}

// Plist returns the launchd.plist(5) XML for j. Key meanings, from
// launchd.plist(5):
//   - RunAtLoad: launch once when the job is loaded, so at login.
//   - KeepAlive SuccessfulExit false: restart only after a non-zero exit, so
//     a signal stop (exit 0) stays stopped (R84, R85).
//   - ThrottleInterval 60: spawn at most once every 60 seconds.
//   - ExitTimeOut: seconds between SIGTERM and SIGKILL when stopped.
//   - EnvironmentVariables: HOME only. The scaler runs no external
//     programs, so PATH is not needed, and it reaches Docker through a fixed
//     socket, so no DOCKER_* variable either.
//   - StandardOutPath, StandardErrorPath: files for stdout and stderr.
func Plist(j Job) ([]byte, error) {
	if err := j.validate(); err != nil {
		return nil, err
	}
	var b bytes.Buffer
	b.WriteString(xml.Header)
	b.WriteString(`<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">` + "\n")
	b.WriteString(`<plist version="1.0">` + "\n<dict>\n")
	key := func(k string) { fmt.Fprintf(&b, "\t<key>%s</key>\n", k) }
	str := func(indent, s string) {
		b.WriteString(indent + "<string>")
		xml.EscapeText(&b, []byte(s)) // writes to a bytes.Buffer, which does not fail
		b.WriteString("</string>\n")
	}

	key("Label")
	str("\t", j.Label)
	key("ProgramArguments")
	b.WriteString("\t<array>\n")
	for _, arg := range []string{j.Program, "run", "-config", j.ConfigPath} {
		str("\t\t", arg)
	}
	b.WriteString("\t</array>\n")
	key("RunAtLoad")
	b.WriteString("\t<true/>\n")
	key("KeepAlive")
	b.WriteString("\t<dict>\n\t\t<key>SuccessfulExit</key>\n\t\t<false/>\n\t</dict>\n")
	key("ThrottleInterval")
	fmt.Fprintf(&b, "\t<integer>%d</integer>\n", throttleInterval)
	key("ExitTimeOut")
	fmt.Fprintf(&b, "\t<integer>%d</integer>\n", j.ExitTimeOut())
	key("EnvironmentVariables")
	b.WriteString("\t<dict>\n\t\t<key>HOME</key>\n")
	str("\t\t", j.Home)
	b.WriteString("\t</dict>\n")
	key("StandardOutPath")
	str("\t", j.StdoutPath())
	key("StandardErrorPath")
	str("\t", j.StderrPath())
	b.WriteString("</dict>\n</plist>\n")
	return b.Bytes(), nil
}

// xmlText reports whether s is valid UTF-8 made only of XML 1.0 Char
// values, so xml.EscapeText keeps it unchanged instead of replacing
// characters with U+FFFD.
func xmlText(s string) bool {
	if !utf8.ValidString(s) {
		return false
	}
	for _, r := range s {
		switch {
		case r == '\t' || r == '\n' || r == '\r':
		case r >= 0x20 && r <= 0xD7FF:
		case r >= 0xE000 && r <= 0xFFFD:
		case r >= 0x10000 && r <= 0x10FFFF:
		default:
			return false
		}
	}
	return true
}

// exitTimeOutOf returns the top-level ExitTimeOut of a plist written by
// Plist, or false if it has none or cannot be parsed.
func exitTimeOutOf(data []byte) (time.Duration, bool) {
	d := xml.NewDecoder(bytes.NewReader(data))
	// Depth 0 is outside <plist>, 1 inside it, 2 inside its top-level dict.
	depth := 0
	var lastKey string
	for {
		tok, err := d.Token()
		if err != nil {
			return 0, false
		}
		switch t := tok.(type) {
		case xml.StartElement:
			depth++
			if depth != 3 {
				continue
			}
			var text string
			if err := d.DecodeElement(&text, &t); err != nil {
				return 0, false
			}
			depth--
			switch t.Name.Local {
			case "key":
				lastKey = text
				continue
			case "integer":
				if lastKey == "ExitTimeOut" {
					n, err := strconv.ParseInt(strings.TrimSpace(text), 10, 64)
					if err != nil || n <= 0 || n > int64(maxSeconds) {
						return 0, false
					}
					return seconds(n), true
				}
			}
			lastKey = ""
		case xml.EndElement:
			depth--
		}
	}
}

// maxSeconds is the largest whole-second count a time.Duration holds.
const maxSeconds = int64(1<<63-1) / int64(time.Second)

// seconds converts n seconds to a Duration, capped at maxSeconds.
func seconds(n int64) time.Duration {
	return time.Duration(min(n, maxSeconds)) * time.Second
}
