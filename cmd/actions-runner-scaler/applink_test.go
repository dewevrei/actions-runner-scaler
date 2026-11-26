package main

import (
	"bytes"
	"context"
	"os"
	"strings"
	"testing"
)

const appLinkSteps = `
1. 위 주소를 브라우저로 열고 App 이름을 넣습니다. 이름은 GitHub 전체에서 유일해야 합니다.
2. 'Create GitHub App'을 누릅니다.
3. 'Generate a private key'로 개인키를 만들어 받고, 모드 0600으로 저장합니다.
4. App 설정의 'Install App'에서 스케일러가 섬길 저장소에 설치합니다.
   다른 owner의 저장소에도 쓰려면 -public으로 만들어야 합니다. 아니면 그 owner에는 token-file을 씁니다.
   owner마다 그 owner 소유의 비공개 App 하나를 권합니다.
5. client ID와 개인키 경로를 준비하고 ` + "`actions-runner-scaler init`" + `을 실행합니다.
`

func runAppLink(args ...string) (stdout, stderr string, code int) {
	var out, errOut bytes.Buffer
	code = dispatch(context.Background(), newEnv(&out, &errOut), append([]string{"app-link"}, args...), commands())
	return out.String(), errOut.String(), code
}

// R305: the first line is the registration URL with the query in a fixed
// order; the next steps follow; nothing goes to stderr.
func TestAppLink(t *testing.T) {
	const home = "https://github.com/dewevrei/actions-runner-scaler"
	tests := []struct {
		name string
		args []string
		url  string
	}{
		{"personal", nil,
			"https://github.com/settings/apps/new?url=" + home + "&public=false&webhook_active=false&administration=write"},
		{"organization", []string{"-org", "acme-1"},
			"https://github.com/organizations/acme-1/settings/apps/new?url=" + home + "&public=false&webhook_active=false&administration=write"},
		{"public", []string{"-public"},
			"https://github.com/settings/apps/new?url=" + home + "&public=true&webhook_active=false&administration=write"},
		{"organization public", []string{"-public", "-org", "Acme"},
			"https://github.com/organizations/Acme/settings/apps/new?url=" + home + "&public=true&webhook_active=false&administration=write"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stdout, stderr, code := runAppLink(tt.args...)
			if code != 0 {
				t.Fatalf("exit = %d, stderr %q", code, stderr)
			}
			if stderr != "" {
				t.Errorf("stderr = %q, want empty", stderr)
			}
			if want := tt.url + "\n" + appLinkSteps; stdout != want {
				t.Errorf("stdout =\n%s\nwant\n%s", stdout, want)
			}
		})
	}
}

// R306: a bad -org writes nothing to stdout and exits 1 with an error.
func TestAppLinkBadOrg(t *testing.T) {
	for _, org := range []string{"a/b", "a b", "a?x", "", "a_b", "a\n", "한글"} {
		t.Run(org, func(t *testing.T) {
			stdout, stderr, code := runAppLink("-org", org)
			if code != 1 {
				t.Errorf("exit = %d, want 1", code)
			}
			if stdout != "" {
				t.Errorf("stdout = %q, want empty", stdout)
			}
			if !strings.Contains(stderr, "-org") {
				t.Errorf("stderr = %q, want an error naming -org", stderr)
			}
		})
	}
}

func TestAppLinkExtraArgument(t *testing.T) {
	stdout, stderr, code := runAppLink("acme")
	if code != 1 || stdout != "" || !strings.Contains(stderr, "usage: app-link") {
		t.Errorf("exit %d, stdout %q, stderr %q; want a usage error", code, stdout, stderr)
	}
}

// R305: the command only writes to stdout; a temporary HOME stays empty.
// appLinkCmd takes no dependencies beyond env, so it has no HTTP client or
// file writer to hold.
func TestAppLinkNoIO(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if _, _, code := runAppLink("-org", "acme"); code != 0 {
		t.Fatalf("exit = %d", code)
	}
	entries, err := os.ReadDir(home)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("HOME holds %d entries, want none", len(entries))
	}
}
