package configfile

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// ErrExists means CreateNew found a file already at its path.
var ErrExists = errors.New("configfile: file already exists")

// fileOps holds the steps whose failures tests inject.
type fileOps struct {
	write   func(f *os.File, data []byte) error
	sync    func(f *os.File) error
	link    func(oldname, newname string) error
	rename  func(oldpath, newpath string) error
	syncDir func(dir string) error
}

func osOps() fileOps {
	return fileOps{
		write: func(f *os.File, data []byte) error {
			_, err := f.Write(data)
			return err
		},
		sync:    (*os.File).Sync,
		link:    os.Link,
		rename:  os.Rename,
		syncDir: syncDir,
	}
}

// syncDir flushes dir's entries, so a file just linked or renamed into it
// survives a crash.
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	if err := d.Sync(); err != nil {
		_ = d.Close()
		return err
	}
	return d.Close()
}

// CreateNew writes data to a new file at path with mode 0600. It returns an
// error wrapping ErrExists, and writes nothing there, when path already
// exists, including when another process creates it meanwhile. Only the
// whole content ever appears at path (0003 SPEC R282, R283).
func CreateNew(path string, data []byte) error {
	return osOps().createNew(path, data)
}

func (ops fileOps) createNew(path string, data []byte) error {
	tmp, err := ops.writeTemp(filepath.Dir(path), filepath.Base(path), 0o600, data)
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp) }()
	// link(2), unlike rename(2), fails when the target exists.
	if err := ops.link(tmp, path); err != nil {
		if errors.Is(err, fs.ErrExist) {
			return fmt.Errorf("configfile: %s: %w", path, ErrExists)
		}
		return fmt.Errorf("configfile: place %s: %w", path, err)
	}
	// The file is in place; a failed flush reports but does not remove it.
	if err := ops.syncDir(filepath.Dir(path)); err != nil {
		return fmt.Errorf("configfile: sync directory of %s: %w", path, err)
	}
	return nil
}

// Replace replaces the content of the existing regular file at path with
// data, keeping its mode. When path is a symbolic link, the file it resolves
// to is replaced and the link stays. On failure the original is unchanged
// (0003 SPEC R293).
func Replace(path string, data []byte) error {
	return osOps().replace(path, data)
}

func (ops fileOps) replace(path string, data []byte) error {
	// Renaming onto the link itself would turn it into a plain file.
	realPath, err := filepath.EvalSymlinks(path)
	if err != nil {
		return fmt.Errorf("configfile: %w", err)
	}
	info, err := os.Stat(realPath)
	if err != nil {
		return fmt.Errorf("configfile: %w", err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("configfile: %s is not a regular file", realPath)
	}
	tmp, err := ops.writeTemp(filepath.Dir(realPath), filepath.Base(realPath), info.Mode().Perm(), data)
	if err != nil {
		return err
	}
	if err := ops.rename(tmp, realPath); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("configfile: replace %s: %w", realPath, err)
	}
	if err := ops.syncDir(filepath.Dir(realPath)); err != nil {
		return fmt.Errorf("configfile: sync directory of %s: %w", realPath, err)
	}
	return nil
}

// writeTemp writes data to a new hidden file in dir with the given mode,
// flushed to disk and closed, and returns its path. On failure it leaves no
// file behind.
func (ops fileOps) writeTemp(dir, base string, perm fs.FileMode, data []byte) (path string, err error) {
	f, err := os.CreateTemp(dir, "."+base+".tmp-*")
	if err != nil {
		return "", fmt.Errorf("configfile: temporary file: %w", err)
	}
	defer func() {
		if err != nil {
			_ = f.Close()
			_ = os.Remove(f.Name())
		}
	}()
	// Chmod, not the creation mode, so the umask does not change it.
	if err := f.Chmod(perm); err != nil {
		return "", fmt.Errorf("configfile: temporary file: %w", err)
	}
	if err := ops.write(f, data); err != nil {
		return "", fmt.Errorf("configfile: write temporary file: %w", err)
	}
	if err := ops.sync(f); err != nil {
		return "", fmt.Errorf("configfile: sync temporary file: %w", err)
	}
	if err := f.Close(); err != nil {
		return "", fmt.Errorf("configfile: close temporary file: %w", err)
	}
	return f.Name(), nil
}
