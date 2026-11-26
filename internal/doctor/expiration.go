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
		return ", 토큰 만료 값을 읽을 수 없음", ""
	}
	suffix = ", 토큰 만료 " + value
	for _, layout := range expirationLayouts {
		at, err := time.Parse(layout, value)
		if err != nil {
			continue
		}
		if !at.After(now.Add(expiryWarningWindow)) {
			warning = "토큰이 " + value + "에 만료됨. 새 토큰을 만들어 파일을 바꾸고 스케일러를 다시 시작한다"
		}
		break
	}
	return suffix, warning
}
