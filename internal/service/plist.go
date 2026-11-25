// Package service installs, stops and removes the per-user LaunchAgent that
// keeps the scaler running (R19, R44).
package service

import (
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"
)

// Label is the launchd job label. The plist file is named <Label>.plist, the
// convention launchd.plist(5) states under XML PROPERTY LIST KEYS.
const Label = "io.github.dewevrei.actions-runner-scaler"

// ExitTimeOut is the plist's ExitTimeOut: the seconds launchd waits between
// SIGTERM and SIGKILL when it stops the job (bootout, logout, shutdown).
// Since macOS 26.4 launchd caps it at 60 seconds whatever the plist says,
// a cap launchd.plist(5) does not document (Apple Developer Forums thread
// 854315; on this host a plist value of 660 read back as 60 and the job
// was killed 60 seconds after SIGTERM). The plist states the value launchd
// applies, so a drain longer than this is never left to launchd: "service
// stop" and "service uninstall" signal the scaler themselves and wait for
// the drain (Manager.Stop).
const ExitTimeOut = 60 * time.Second

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
//   - ExitTimeOut: seconds between SIGTERM and SIGKILL when launchd stops
//     the job; see the ExitTimeOut constant for the 60-second cap.
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
	fmt.Fprintf(&b, "\t<integer>%d</integer>\n", int(ExitTimeOut/time.Second))
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

// configPathOf returns the argument after "-config" in the top-level
// ProgramArguments array of a plist written by Plist, or false if there is
// none or the plist cannot be parsed.
func configPathOf(data []byte) (string, bool) {
	d := xml.NewDecoder(bytes.NewReader(data))
	// Depth 0 is outside <plist>, 1 inside it, 2 inside its top-level dict.
	depth := 0
	var lastKey string
	for {
		tok, err := d.Token()
		if err != nil {
			return "", false
		}
		switch t := tok.(type) {
		case xml.StartElement:
			depth++
			if depth != 3 {
				continue
			}
			switch {
			case t.Name.Local == "key":
				var k string
				if err := d.DecodeElement(&k, &t); err != nil {
					return "", false
				}
				depth--
				lastKey = k
				continue
			case t.Name.Local == "array" && lastKey == "ProgramArguments":
				var args struct {
					S []string `xml:"string"`
				}
				if err := d.DecodeElement(&args, &t); err != nil {
					return "", false
				}
				for i := 0; i+1 < len(args.S); i++ {
					if args.S[i] == "-config" && args.S[i+1] != "" {
						return args.S[i+1], true
					}
				}
				return "", false
			}
			lastKey = ""
		case xml.EndElement:
			depth--
		}
	}
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
