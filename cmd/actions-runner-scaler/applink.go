package main

import (
	"context"
	"flag"
	"fmt"
	"strings"
)

// appLinkHomepage is the App's Homepage URL: this public repository.
const appLinkHomepage = "https://github.com/dewevrei/actions-runner-scaler"

// appLinkCmd prints the link that creates the GitHub App (0003 SPEC R305,
// R306). It reads no configuration file and makes no network call.
func appLinkCmd(_ context.Context, e env, args []string) error {
	fs := newFlagSet("app-link", e)
	org := fs.String("org", "", "create the App in this organization instead of your account")
	public := fs.Bool("public", false, "let any owner install the App")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return badUsage("usage: app-link [-org organization] [-public]")
	}
	orgSet := false
	fs.Visit(func(f *flag.Flag) { orgSet = orgSet || f.Name == "org" })
	if orgSet && !validOrganization(*org) {
		return badUsage(fmt.Sprintf("app-link: -org %q must match [A-Za-z0-9-]+", *org))
	}
	_, err := fmt.Fprint(e.stdout, appLinkOutput(*org, *public))
	return err
}

// validOrganization reports whether s matches [A-Za-z0-9-]+ (R306).
func validOrganization(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '-':
		default:
			return false
		}
	}
	return true
}

// appLinkURL returns the App registration URL for org, or for the personal
// account when org is empty. The permission is the only one the scaler needs;
// webhooks stay off (R305).
func appLinkURL(org string, public bool) string {
	base := "https://github.com/settings/apps/new"
	if org != "" {
		base = "https://github.com/organizations/" + org + "/settings/apps/new"
	}
	return fmt.Sprintf("%s?url=%s&public=%t&webhook_active=false&administration=write",
		base, appLinkHomepage, public)
}

// appLinkOutput returns the URL line followed by the next steps (R305).
func appLinkOutput(org string, public bool) string {
	var b strings.Builder
	b.WriteString(appLinkURL(org, public))
	b.WriteString("\n\n")
	b.WriteString(strings.Join([]string{
		"1. 위 주소를 브라우저로 열고 App 이름을 넣습니다. 이름은 GitHub 전체에서 유일해야 합니다.",
		"2. 'Create GitHub App'을 누릅니다.",
		"3. 'Generate a private key'로 개인키를 만들어 받고, 모드 0600으로 저장합니다.",
		"4. App 설정의 'Install App'에서 스케일러가 섬길 저장소에 설치합니다.",
		"   다른 owner의 저장소에도 쓰려면 -public으로 만들어야 합니다. 아니면 그 owner에는 token-file을 씁니다.",
		"   owner마다 그 owner 소유의 비공개 App 하나를 권합니다.",
		"5. client ID와 개인키 경로를 준비하고 `actions-runner-scaler init`을 실행합니다.",
	}, "\n"))
	b.WriteString("\n")
	return b.String()
}
