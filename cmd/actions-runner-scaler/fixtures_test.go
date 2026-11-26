package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fixtureToken returns a value shaped like a GitHub token: prefix (such as
// "ghp_" or "github_pat_") followed by 36 letters and digits. It is built
// when called, so no token-shaped literal is committed.
func fixtureToken(t *testing.T, prefix string) string {
	t.Helper()
	return prefix + strings.Repeat("aB3dE5", 6)
}

// writeTokenFile writes value and a newline to a file of mode 0600 in
// t.TempDir() and returns its absolute path.
func writeTokenFile(t *testing.T, value string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(path, []byte(value+"\n"), 0o600); err != nil {
		t.Fatalf("write token file: %v", err)
	}
	// WriteFile applies the umask; make the mode exact.
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatalf("chmod token file: %v", err)
	}
	return path
}

func TestFixtureToken(t *testing.T) {
	got := fixtureToken(t, "ghp_")
	if !strings.HasPrefix(got, "ghp_") || len(got) != len("ghp_")+36 {
		t.Errorf("fixtureToken = %q, want ghp_ plus 36 characters", got)
	}
}

func TestWriteTokenFile(t *testing.T) {
	value := fixtureToken(t, "github_pat_")
	path := writeTokenFile(t, value)
	if !filepath.IsAbs(path) {
		t.Errorf("path %q is not absolute", path)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("mode = %o, want 600", info.Mode().Perm())
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != value+"\n" {
		t.Errorf("file content differs from the value and a newline")
	}
}
