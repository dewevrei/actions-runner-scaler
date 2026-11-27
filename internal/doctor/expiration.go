package doctor

import (
	"regexp"
	"time"

	"github.com/dewevrei/actions-runner-scaler/internal/github"
)

// shownExpiration is the only shape of GitHub-Authentication-Token-
// Expiration that is printed: the header is GitHub's text (0003 SPEC R317).
var shownExpiration = regexp.MustCompile(`^[0-9A-Za-z :+-]{1,40}$`)

// expirationLayouts are tried in order, as go-github's parseTokenExpiration
// does (0003 SPEC Design 토큰 만료).
var expirationLayouts = [...]string{"2006-01-02 15:04:05 MST", "2006-01-02 15:04:05 -0700"}

// expiryWarningWindow leaves time to make a new token (0003 SPEC R317).
const expiryWarningWindow = 7 * 24 * time.Hour

// expiration returns the summary suffix and the warning, if any, for the
// expiry header of a token check's answer, measured from now (0003 SPEC
// R317). Without the header both are empty.
func expiration(a github.Access, now time.Time) (suffix, warning string) {
	if !a.ExpirationPresent {
		return "", ""
	}
	value := a.Expiration
	if !shownExpiration.MatchString(value) {
		return ", token expiry unreadable", ""
	}
	suffix = ", token expiry " + value
	for _, layout := range expirationLayouts {
		at, err := time.Parse(layout, value)
		if err != nil {
			continue
		}
		if !at.After(now.Add(expiryWarningWindow)) {
			warning = "the token expires at " + value + " (or has already expired); make a new token, replace the file and restart the scaler"
		}
		break
	}
	return suffix, warning
}
