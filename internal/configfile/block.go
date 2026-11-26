package configfile

import (
	"fmt"
	"strconv"
	"strings"
)

// TargetEntry is one repository target as add-repo and init write it.
type TargetEntry struct {
	URL        string
	MaxRunners int
	Idle       *int   // nil leaves the key out
	TokenFile  string // empty leaves the key out
}

// TargetBlock returns the [[targets]] table of e, each line ending in a
// newline (0003 SPEC R290, R292). Callers must reject a URL or TokenFile that
// is not valid UTF-8 (utf8.ValidString) first: TOML cannot hold it, so it
// would be written with U+FFFD in place of the invalid bytes.
func TargetBlock(e TargetEntry) string {
	var b strings.Builder
	b.WriteString("[[targets]]\n")
	writeKey(&b, "url", quote(e.URL))
	writeKey(&b, "max-runners", strconv.Itoa(e.MaxRunners))
	if e.Idle != nil {
		writeKey(&b, "idle", strconv.Itoa(*e.Idle))
	}
	if e.TokenFile != "" {
		writeKey(&b, "token-file", quote(e.TokenFile))
	}
	return b.String()
}

func writeKey(b *strings.Builder, key, value string) {
	b.WriteString(key)
	b.WriteString(" = ")
	b.WriteString(value)
	b.WriteByte('\n')
}

// quote returns s as a TOML basic string. It escapes like strconv.Quote but
// only with the escapes TOML has: strconv.Quote's \a, \v and \x forms are
// not TOML. An invalid UTF-8 byte, which TOML cannot hold, becomes U+FFFD,
// as range decodes it.
func quote(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\b':
			b.WriteString(`\b`)
		case '\t':
			b.WriteString(`\t`)
		case '\n':
			b.WriteString(`\n`)
		case '\f':
			b.WriteString(`\f`)
		case '\r':
			b.WriteString(`\r`)
		default:
			if r < 0x20 || r == 0x7f {
				fmt.Fprintf(&b, `\u%04X`, r)
				continue
			}
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}
