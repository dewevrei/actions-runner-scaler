package config

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"unicode"
)

// maxTokenFileSize bounds a token file (0003 SPEC R275); a GitHub token is
// a few hundred bytes at most.
const maxTokenFileSize = 4 * 1024

// Messages of 0003 SPEC R276 and R314. Neither quotes the value.
const (
	msgTokenValue    = "value looks like a token, not a file path; set the path of the token file"
	msgCredentialURL = "value contains a credential; set the repository URL only"
)

// TokenPrefixes returns the prefixes of GitHub's token formats: classic,
// fine-grained, OAuth, user-to-server, server-to-server and refresh tokens
// (0003 SPEC R276, Design 자격). Each call returns a new slice.
func TokenPrefixes() []string {
	return []string{"ghp_", "github_pat_", "gho_", "ghu_", "ghs_", "ghr_"}
}

// credentialPattern is a token prefix followed by 20 or more token
// characters (0003 SPEC R314). Shorter runs are left to the URL rule, so a
// repository named like "ghp_x" stays valid.
var credentialPattern = func() *regexp.Regexp {
	prefixes := TokenPrefixes()
	for i, p := range prefixes {
		prefixes[i] = regexp.QuoteMeta(p)
	}
	return regexp.MustCompile(`(?:` + strings.Join(prefixes, "|") + `)[A-Za-z0-9_]{20,}`)
}()

// ContainsCredential reports whether s, a repository URL value, holds a
// credential: an "@" or a token (0003 SPEC R314). Callers check it before
// any check that quotes s.
func ContainsCredential(s string) bool {
	return strings.Contains(s, "@") || credentialPattern.MatchString(s)
}

// looksLikeToken reports whether a token file value is a pasted token
// rather than a path (0003 SPEC R276).
func looksLikeToken(value string) bool {
	value = strings.TrimSpace(value)
	for _, p := range TokenPrefixes() {
		if strings.HasPrefix(value, p) {
			return true
		}
	}
	return false
}

// ReadTokenFile reads the personal access token in the file at path (0003
// SPEC R275, R276). A value that looks like a token is refused before any
// file access. The file must be a regular file (a symbolic link counts as
// its target) at an absolute path, of at most 4 KiB, with mode 0600 or
// narrower and no special bits; its content, less one trailing "\n" or
// "\r\n", must be one non-empty line without spaces or control characters.
// Errors name the broken rule and contain neither the path nor the content
// (0003 SPEC R277).
func ReadTokenFile(path string) (string, error) {
	if looksLikeToken(path) {
		return "", errors.New(msgTokenValue)
	}
	if !filepath.IsAbs(path) {
		return "", errors.New("must be an absolute path")
	}
	data, modeErr, err := readSecretFile(path, maxTokenFileSize)
	if err != nil {
		return "", errors.Join(modeErr, err)
	}
	token, contentErr := parseToken(data)
	if err := errors.Join(modeErr, contentErr); err != nil {
		return "", err
	}
	return token, nil
}

// parseToken returns the one-line token in data.
func parseToken(data []byte) (string, error) {
	s := string(data)
	if t, ok := strings.CutSuffix(s, "\r\n"); ok {
		s = t
	} else {
		s = strings.TrimSuffix(s, "\n")
	}
	if s == "" {
		return "", errors.New("token file is empty")
	}
	if strings.ContainsFunc(s, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) {
		return "", errors.New("token must be one line without spaces or control characters")
	}
	return s, nil
}

// readSecretFile reads the regular file at path, a key or token file, up to
// limit bytes. It returns the mode rule's error apart, so a caller reports
// it together with its content checks (R28). Errors do not contain path.
func readSecretFile(path string, limit int64) (data []byte, modeErr, err error) {
	// The type is checked before the open, which then cannot block on a
	// FIFO or device in the common case, and again on the open file.
	info, err := os.Stat(path)
	if err != nil {
		return nil, nil, withoutPath(err)
	}
	if !info.Mode().IsRegular() {
		return nil, nil, errors.New("not a regular file")
	}
	f, info, err := openRegular(path)
	if err != nil {
		return nil, nil, err
	}
	defer f.Close()

	modeErr = checkSecretMode(info.Mode())
	data, err = io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, modeErr, withoutPath(err)
	}
	if int64(len(data)) > limit {
		return nil, modeErr, fmt.Errorf("file is larger than %d bytes", limit)
	}
	return data, modeErr, nil
}
