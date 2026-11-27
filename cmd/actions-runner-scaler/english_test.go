package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"unicode"
)

// Every string a user sees is English. This test fails when a string or rune
// literal in a non-test Go file holds Hangul, or when a file under image/,
// the service test data or the example configuration does. Comments in Go
// files may name a Korean SPEC section, so they are not scanned.

// hangulLiterals returns "file:line: literal" for each string or rune literal
// of f that holds Hangul.
func hangulLiterals(fset *token.FileSet, f *ast.File) []string {
	var found []string
	ast.Inspect(f, func(n ast.Node) bool {
		lit, ok := n.(*ast.BasicLit)
		if !ok || (lit.Kind != token.STRING && lit.Kind != token.CHAR) {
			return true
		}
		// Unquote decodes escapes, so a Unicode escape is caught like the letter.
		text, err := strconv.Unquote(lit.Value)
		if err != nil {
			text = lit.Value
		}
		if containsHangul(text) {
			found = append(found, fset.Position(lit.Pos()).String()+": "+lit.Value)
		}
		return true
	})
	return found
}

func containsHangul(s string) bool {
	return strings.ContainsFunc(s, func(r rune) bool { return unicode.Is(unicode.Hangul, r) })
}

// moduleRoot returns the directory that holds go.mod, above the test's
// working directory.
func moduleRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("go.mod not found above the working directory")
		}
		dir = parent
	}
}

func TestUserFacingTextIsEnglish(t *testing.T) {
	t.Run("the check finds Hangul in literals and not in comments", func(t *testing.T) {
		const src = `package p

// 주석은 한글이어도 된다.
const a = "plain"
const b = "한글"
const c = '한'
const d = "\uD55C"
const e = ` + "`원문`" + `
`
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, "p.go", src, parser.ParseComments)
		if err != nil {
			t.Fatal(err)
		}
		if got := hangulLiterals(fset, f); len(got) != 4 {
			t.Errorf("found %d literals, want 4 (b, c, d, e): %q", len(got), got)
		}
	})

	root := moduleRoot(t)

	t.Run("string literals of the non-test Go files", func(t *testing.T) {
		fset := token.NewFileSet()
		files := 0
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			name := d.Name()
			if d.IsDir() {
				// Dot directories hold other worktrees and tools, not code.
				if path != root && (strings.HasPrefix(name, ".") || name == "testdata" || name == "vendor") {
					return filepath.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
				return nil
			}
			f, err := parser.ParseFile(fset, path, nil, 0)
			if err != nil {
				return err
			}
			files++
			for _, hit := range hangulLiterals(fset, f) {
				t.Errorf("Hangul in a literal: %s", hit)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		if files < 50 {
			t.Errorf("scanned %d Go files, want the whole module", files)
		}
	})

	t.Run("the image files, the service test data and the example configuration", func(t *testing.T) {
		var paths []string
		for _, dir := range []string{"image", filepath.Join("internal", "service", "testdata")} {
			err := filepath.WalkDir(filepath.Join(root, dir), func(path string, d fs.DirEntry, err error) error {
				if err == nil && !d.IsDir() {
					paths = append(paths, path)
				}
				return err
			})
			if err != nil {
				t.Fatal(err)
			}
		}
		paths = append(paths, filepath.Join(root, "config.example.toml"))
		if len(paths) < 6 {
			t.Fatalf("paths = %q, want the image files, the test data and the example configuration", paths)
		}
		for _, path := range paths {
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if containsHangul(string(data)) {
				t.Errorf("Hangul in %s", path)
			}
		}
	})
}
