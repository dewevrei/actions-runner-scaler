package configfile

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

var errInjected = errors.New("injected")

// dirNames lists dir's entries.
func dirNames(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

func assertFile(t *testing.T, path, want string, perm fs.FileMode) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != perm {
		t.Errorf("%s mode = %v, want %v", path, got, perm)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != want {
		t.Errorf("%s = %q, want %q", path, data, want)
	}
}

func TestCreateNewMode0600(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	if err := CreateNew(path, []byte("new\n")); err != nil {
		t.Fatalf("CreateNew: %v", err)
	}
	assertFile(t, path, "new\n", 0o600)
	if got := dirNames(t, dir); !slices.Equal(got, []string{"config.toml"}) {
		t.Fatalf("directory holds %v", got)
	}
}

func TestCreateNewRefusesExisting(t *testing.T) {
	tests := []struct {
		name  string
		make  func(path string) error
		check func(t *testing.T, path string)
	}{
		{
			"file",
			func(path string) error { return os.WriteFile(path, []byte("old\n"), 0o644) },
			func(t *testing.T, path string) { assertFile(t, path, "old\n", 0o644) },
		},
		{
			"dangling link",
			func(path string) error { return os.Symlink(path+".missing", path) },
			func(t *testing.T, path string) {
				if _, err := os.Lstat(path + ".missing"); !errors.Is(err, fs.ErrNotExist) {
					t.Errorf("link target made: %v", err)
				}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "config.toml")
			if err := tt.make(path); err != nil {
				t.Fatal(err)
			}
			before := dirNames(t, dir)
			if err := CreateNew(path, []byte("new\n")); !errors.Is(err, ErrExists) {
				t.Fatalf("CreateNew error = %v, want ErrExists", err)
			}
			if got := dirNames(t, dir); !slices.Equal(got, before) {
				t.Fatalf("directory holds %v, want %v", got, before)
			}
			tt.check(t, path)
		})
	}
}

func TestCreateNewLinkRace(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	ops := osOps()
	// Another process creates the file after the temporary file is written.
	ops.link = func(oldname, newname string) error {
		if err := os.WriteFile(newname, []byte("other\n"), 0o644); err != nil {
			return err
		}
		return os.Link(oldname, newname)
	}
	if err := ops.createNew(path, []byte("new\n")); !errors.Is(err, ErrExists) {
		t.Fatalf("createNew error = %v, want ErrExists", err)
	}
	assertFile(t, path, "other\n", 0o644)
	if got := dirNames(t, dir); !slices.Equal(got, []string{"config.toml"}) {
		t.Fatalf("directory holds %v", got)
	}
}

func TestCreateNewLeavesNoTemp(t *testing.T) {
	tests := []struct {
		name   string
		inject func(*fileOps)
	}{
		{"write", func(o *fileOps) { o.write = func(*os.File, []byte) error { return errInjected } }},
		{"fsync", func(o *fileOps) { o.sync = func(*os.File) error { return errInjected } }},
		{"link", func(o *fileOps) { o.link = func(string, string) error { return errInjected } }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "config.toml")
			ops := osOps()
			tt.inject(&ops)
			if err := ops.createNew(path, []byte("new\n")); !errors.Is(err, errInjected) {
				t.Fatalf("createNew error = %v, want the injected one", err)
			}
			if got := dirNames(t, dir); len(got) != 0 {
				t.Fatalf("directory holds %v", got)
			}
		})
	}
}

func TestCreateNewReadOnlyDir(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root writes into a read-only directory")
	}
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	if err := CreateNew(filepath.Join(dir, "config.toml"), []byte("new\n")); err == nil {
		t.Fatal("CreateNew in a read-only directory succeeded")
	}
	if got := dirNames(t, dir); len(got) != 0 {
		t.Fatalf("directory holds %v", got)
	}
}

func TestReplaceKeepsModeAndLink(t *testing.T) {
	realDir := t.TempDir()
	realPath := filepath.Join(realDir, "config.toml")
	if err := os.WriteFile(realPath, []byte("old\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Set after creation so the umask does not change it.
	if err := os.Chmod(realPath, 0o640); err != nil {
		t.Fatal(err)
	}
	linkDir := t.TempDir()
	link := filepath.Join(linkDir, "linked.toml")
	if err := os.Symlink(realPath, link); err != nil {
		t.Fatal(err)
	}

	if err := Replace(realPath, []byte("direct\n")); err != nil {
		t.Fatalf("Replace: %v", err)
	}
	assertFile(t, realPath, "direct\n", 0o640)

	if err := Replace(link, []byte("through link\n")); err != nil {
		t.Fatalf("Replace through a link: %v", err)
	}
	info, err := os.Lstat(link)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&fs.ModeSymlink == 0 {
		t.Fatalf("link became %v", info.Mode())
	}
	assertFile(t, realPath, "through link\n", 0o640)
	if got := dirNames(t, realDir); !slices.Equal(got, []string{"config.toml"}) {
		t.Fatalf("real directory holds %v", got)
	}
	if got := dirNames(t, linkDir); !slices.Equal(got, []string{"linked.toml"}) {
		t.Fatalf("link directory holds %v", got)
	}
}

func TestReplaceFailureKeepsOriginal(t *testing.T) {
	tests := []struct {
		name   string
		inject func(*fileOps)
	}{
		{"write", func(o *fileOps) { o.write = func(*os.File, []byte) error { return errInjected } }},
		{"fsync", func(o *fileOps) { o.sync = func(*os.File) error { return errInjected } }},
		{"rename", func(o *fileOps) { o.rename = func(string, string) error { return errInjected } }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "config.toml")
			if err := os.WriteFile(path, []byte("old\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			ops := osOps()
			tt.inject(&ops)
			if err := ops.replace(path, []byte("new\n")); !errors.Is(err, errInjected) {
				t.Fatalf("replace error = %v, want the injected one", err)
			}
			assertFile(t, path, "old\n", 0o600)
			if got := dirNames(t, dir); !slices.Equal(got, []string{"config.toml"}) {
				t.Fatalf("directory holds %v", got)
			}
		})
	}
}

func TestReplaceRefuses(t *testing.T) {
	dir := t.TempDir()
	if err := Replace(filepath.Join(dir, "missing.toml"), []byte("new\n")); err == nil {
		t.Error("Replace of a missing file succeeded")
	}
	if err := Replace(dir, []byte("new\n")); err == nil {
		t.Error("Replace of a directory succeeded")
	}
	if got := dirNames(t, dir); len(got) != 0 {
		t.Fatalf("directory holds %v", got)
	}
}
