package doctor

import (
	"fmt"
	"io"
	"strings"

	"github.com/dewevrei/actions-runner-scaler/internal/logging"
)

// Print writes rep in the form of 0003 SPEC R296 and the count line of
// R298, each line passed through the registered secrets (R277), and
// returns the number of 실패 lines.
func Print(w io.Writer, rep Report) (failed int) {
	redact := rep.redact
	if redact == nil {
		redact = logging.NewSecrets().Redact
	}
	breaks := strings.NewReplacer("\r", " ", "\n", " ")
	line := func(format string, args ...any) {
		// Redacted before the line breaks go, so a PEM block is matched
		// whole; a field holding a break must not start a line of its own.
		fmt.Fprintln(w, breaks.Replace(redact(fmt.Sprintf(format, args...))))
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
	return counts[Fail]
}
