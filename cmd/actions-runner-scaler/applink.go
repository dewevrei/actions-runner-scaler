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
		"1. Open the address above in a browser and enter the App name. The name must be unique across all of GitHub.",
		"2. Click 'Create GitHub App'.",
		"3. Generate and download a private key with 'Generate a private key', and save it with mode 0600.",
		"4. In 'Install App' of the App settings, install it on the repositories the scaler serves.",
		"   To use it for repositories of another owner, make the App with -public. Otherwise use a token-file for that owner.",
		"   One private App owned by each owner is recommended.",
		"5. Have the client ID and the private key path ready, and run `actions-runner-scaler init`.",
	}, "\n"))
	b.WriteString("\n")
	return b.String()
}
