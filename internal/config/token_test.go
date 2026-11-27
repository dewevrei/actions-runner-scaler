package config

import (
	"crypto/rand"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"
)

// fixtureToken returns a token-shaped value built at run time from prefix
// and random characters, so no token-shaped literal is committed (0003 PLAN
// Risks).
func fixtureToken(prefix string) string {
	return prefix + rand.Text() + rand.Text()
}

// tokenChars returns n random characters of [A-Z2-7], all token characters.
func tokenChars(n int) string {
	var s string
	for len(s) < n {
		s += rand.Text()
	}
	return s[:n]
}

func writeToken(t *testing.T, content string, mode os.FileMode) string {
	t.Helper()
	return writeFile(t, "pat.txt", []byte(content), mode)
}

// TestReadTokenFile proves 0003 SPEC R275 and R277: the file rules, and no
// error holds the path or the content.
func TestReadTokenFile(t *testing.T) {
	token := fixtureToken("ghp_")
	dir := t.TempDir()
	validLink := filepath.Join(dir, "link")
	if err := os.Symlink(writeToken(t, token+"\n", 0o600), validLink); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	fifo := filepath.Join(dir, "fifo")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatalf("mkfifo: %v", err)
	}

	tests := []struct {
		name    string
		path    string // when empty, a file of content and mode
		content string
		mode    os.FileMode
		want    string // the token, or with wantErr a part of the error
		wantErr bool
	}{
		{name: "one \\n removed", content: token + "\n", mode: 0o600, want: token},
		{name: "one \\r\\n removed", content: token + "\r\n", mode: 0o600, want: token},
		{name: "no newline", content: token, mode: 0o600, want: token},
		{name: "mode 0400", content: token, mode: 0o400, want: token},
		{name: "mode 0700", content: token, mode: 0o700, want: token},
		{name: "mode 0500", content: token, mode: 0o500, want: token},
		{name: "setuid 0600", content: token, mode: 0o600 | os.ModeSetuid, want: token},
		{name: "4096 bytes", content: strings.Repeat("a", 4095) + "\n", mode: 0o600, want: strings.Repeat("a", 4095)},
		{name: "symbolic link to a valid file", path: validLink, want: token},

		{name: "relative path", path: "token.txt", want: "must be an absolute path", wantErr: true},
		{name: "relative dot path", path: "./token", want: "must be an absolute path", wantErr: true},
		{name: "directory", path: dir, want: "not a regular file", wantErr: true},
		{name: "FIFO", path: fifo, want: "not a regular file", wantErr: true},
		{name: "missing", path: filepath.Join(dir, "missing"), want: "no such file", wantErr: true},
		{name: "4097 bytes", content: strings.Repeat("a", 4097), mode: 0o600, want: "larger than 4096 bytes", wantErr: true},
		{name: "mode 0640", content: token, mode: 0o640, want: "file mode", wantErr: true},
		{name: "mode 0604", content: token, mode: 0o604, want: "file mode", wantErr: true},
		{name: "mode 0750", content: token, mode: 0o750, want: "file mode", wantErr: true},
		{name: "mode 0644 and setuid", content: token, mode: 0o644 | os.ModeSetuid, want: "file mode", wantErr: true},
		{name: "two trailing newlines", content: token + "\n\n", mode: 0o600, want: "one line", wantErr: true},
		{name: "two trailing \\r\\n", content: token + "\r\n\r\n", mode: 0o600, want: "one line", wantErr: true},
		{name: "empty", content: "", mode: 0o600, want: "empty", wantErr: true},
		{name: "newline only", content: "\n", mode: 0o600, want: "empty", wantErr: true},
		{name: "inner space", content: token[:10] + " " + token[10:], mode: 0o600, want: "spaces", wantErr: true},
		{name: "tab", content: token + "\t", mode: 0o600, want: "spaces", wantErr: true},
		{name: "control character", content: token[:10] + "\x01" + token[10:], mode: 0o600, want: "control", wantErr: true},
		{name: "no-break space", content: token + " ", mode: 0o600, want: "spaces", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := tt.path
			if path == "" {
				path = writeToken(t, tt.content, tt.mode)
			}
			got, err := ReadTokenFile(path)
			if !tt.wantErr {
				if err != nil || got != tt.want {
					t.Fatalf("ReadTokenFile = %d characters, %v; want the token", len(got), err)
				}
				return
			}
			if err == nil || got != "" {
				t.Fatalf("ReadTokenFile = %d characters, %v; want only an error", len(got), err)
			}
			msg := err.Error()
			if !strings.Contains(msg, tt.want) {
				t.Errorf("error %q does not contain %q", msg, tt.want)
			}
			if strings.Contains(msg, path) || strings.Contains(msg, filepath.Base(path)) {
				t.Errorf("error contains the path: %q", msg)
			}
			if content := strings.TrimSpace(tt.content); strings.Contains(msg, token[4:12]) || (content != "" && strings.Contains(msg, content)) {
				t.Errorf("error contains the content: %q", msg)
			}
		})
	}
}

// A mode error and a content error are reported together (R28).
func TestReadTokenFileModeAndContent(t *testing.T) {
	_, err := ReadTokenFile(writeToken(t, "", 0o644))
	if errs := errorList(err); len(errs) != 2 {
		t.Fatalf("errors = %v, want the mode and the empty file", errs)
	}
}

// TestReadTokenFileFIFOReturns proves that a FIFO without a writer is
// refused at once instead of blocking the caller (0003 SPEC Design 자격).
func TestReadTokenFileFIFOReturns(t *testing.T) {
	fifo := filepath.Join(t.TempDir(), "fifo")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatalf("mkfifo: %v", err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := ReadTokenFile(fifo)
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Error("ReadTokenFile(fifo) succeeded")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("ReadTokenFile blocked on a FIFO")
	}
}

// TestReadTokenFileTokenAsPath proves 0003 SPEC R276: a value that starts
// with a token prefix is refused without a file access and is not echoed.
func TestReadTokenFileTokenAsPath(t *testing.T) {
	for _, prefix := range TokenPrefixes() {
		value := fixtureToken(prefix)
		_, err := ReadTokenFile(value)
		if err == nil || err.Error() != msgTokenValue {
			t.Errorf("%s: ReadTokenFile = %v, want %q", prefix, err, msgTokenValue)
		}
	}
	// The prefix alone is enough; a leading space does not hide it.
	if _, err := ReadTokenFile(" ghp_"); err == nil || err.Error() != msgTokenValue {
		t.Errorf("ReadTokenFile(\" ghp_\") = %v, want %q", err, msgTokenValue)
	}
}

// TestTokenPrefixes proves the six prefixes of 0003 SPEC R276, and that a
// caller cannot change them.
func TestTokenPrefixes(t *testing.T) {
	want := []string{"ghp_", "github_pat_", "gho_", "ghu_", "ghs_", "ghr_"}
	got := TokenPrefixes()
	if !slices.Equal(got, want) {
		t.Fatalf("TokenPrefixes = %q, want %q", got, want)
	}
	got[0] = "x"
	if TokenPrefixes()[0] != "ghp_" {
		t.Error("changing the returned slice changed the prefixes")
	}
}

// TestContainsCredential proves the rule of 0003 SPEC R314.
func TestContainsCredential(t *testing.T) {
	for _, prefix := range TokenPrefixes() {
		for _, tt := range []struct {
			s    string
			want bool
		}{
			{prefix + tokenChars(20), true},
			{prefix + tokenChars(19), false},
			{"https://github.com/acme/" + prefix + tokenChars(20), true},
			{"https://github.com/acme/" + prefix + tokenChars(19), false},
			{"https://github.com/acme/" + prefix + tokenChars(10) + "_" + tokenChars(9), true},
			{"https://github.com/acme/" + prefix + tokenChars(10) + "-" + tokenChars(10), false},
			{"https://github.com/acme/x" + prefix + tokenChars(20), true},
		} {
			if got := ContainsCredential(tt.s); got != tt.want {
				t.Errorf("ContainsCredential(%s + %d characters) = %v, want %v", prefix, len(tt.s), got, tt.want)
			}
		}
	}
	for _, tt := range []struct {
		s    string
		want bool
	}{
		{"https://x@github.com/o/r", true},
		{"@", true},
		{"https://github.com/acme/ghp_x", false},
		{"https://github.com/acme/widget", false},
		{"", false},
	} {
		if got := ContainsCredential(tt.s); got != tt.want {
			t.Errorf("ContainsCredential(%q) = %v, want %v", tt.s, got, tt.want)
		}
	}
}
