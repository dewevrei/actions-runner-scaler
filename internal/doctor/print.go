package doctor

import (
	"errors"
	"fmt"
	"io"
	"strings"
)

// Print writes rep in the form of 0003 SPEC R296 and the count line of
// R298, each line passed through the registered secrets (R277), and
// returns the number of 실패 lines. It writes nothing and returns an error
// for a Report that Run did not make, since that has no redaction.
func Print(w io.Writer, rep Report) (failed int, err error) {
	if rep.redact == nil {
		return 0, errors.New("doctor: report has no redaction; make it with Run")
	}
	breaks := strings.NewReplacer("\r", " ", "\n", " ")
	line := func(format string, args ...any) {
		if err != nil {
			return
		}
		// Redacted before the line breaks go, so a PEM block is matched
		// whole; a field holding a break must not start a line of its own.
		_, err = fmt.Fprintln(w, breaks.Replace(rep.redact(fmt.Sprintf(format, args...))))
	}
	var counts [3]int
	for _, r := range rep.Results {
		counts[r.Outcome]++
		line("%s %s: %s", r.Outcome, r.Name, r.Summary)
		if r.Outcome != Pass {
			for _, c := range r.Causes {
				line("  원인: %s", c)
			}
			line("  고치는 법: %s", r.Fix)
		}
		for _, warning := range r.Warnings {
			line("  경고: %s", warning)
		}
	}
	line("%s %d, %s %d, %s %d", Pass, counts[Pass], Fail, counts[Fail], Unknown, counts[Unknown])
	return counts[Fail], err
}
