// Package configfile edits the scaler's settings file safely: a lock that
// serializes editing commands, writes that never leave a partial file, and
// the text of a [[targets]] block (0003 PLAN M33).
package configfile

import (
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"sync"

	"golang.org/x/sys/unix"
)

// lockSuffix is appended to the real settings file's name to name its lock
// file (0003 SPEC R315).
const lockSuffix = ".lock"

// Lock takes an exclusive flock on the lock file next to the real settings
// file that configPath names, waiting while another process holds it. Only
// when it has to wait does it write one "Blocking: waiting for file lock on
// <lock path>" line to notify, which may be nil. unlock releases the lock;
// the kernel also releases it when the process ends.
//
// The lock lives in its own file because a replace renames a new inode onto
// the settings path, so a lock on the settings file itself would not stop
// the next command. The lock file is never removed: removing it would let
// contenders lock two different inodes (0003 SPEC R315).
func Lock(configPath string, notify io.Writer) (unlock func(), err error) {
	realPath, err := filepath.EvalSymlinks(configPath)
	if err != nil {
		return nil, fmt.Errorf("configfile: settings file: %w", err)
	}
	lockPath := realPath + lockSuffix

	fd, err := openLockFile(lockPath)
	if err != nil {
		return nil, err
	}
	if err := acquire(fd, lockPath, notify); err != nil {
		_ = unix.Close(fd)
		return nil, err
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			_ = unix.Flock(fd, unix.LOCK_UN)
			_ = unix.Close(fd)
		})
	}, nil
}

// openLockFile opens or creates lockPath without following a symbolic link
// and returns its descriptor only when it is a regular file. O_NONBLOCK keeps
// the open from waiting on a FIFO put at the path; it does not affect flock.
func openLockFile(lockPath string) (int, error) {
	const flags = unix.O_RDWR | unix.O_CREAT | unix.O_NOFOLLOW | unix.O_CLOEXEC | unix.O_NONBLOCK
	var fd int
	err := retryEINTR(func() error {
		var err error
		fd, err = unix.Open(lockPath, flags, 0o600)
		return err
	})
	if err != nil {
		if errors.Is(err, unix.ELOOP) {
			return -1, fmt.Errorf("configfile: lock file %s is a symbolic link", lockPath)
		}
		return -1, fmt.Errorf("configfile: open lock file %s: %w", lockPath, err)
	}
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		_ = unix.Close(fd)
		return -1, fmt.Errorf("configfile: inspect lock file %s: %w", lockPath, err)
	}
	if st.Mode&unix.S_IFMT != unix.S_IFREG {
		_ = unix.Close(fd)
		return -1, fmt.Errorf("configfile: lock file %s is not a regular file", lockPath)
	}
	return fd, nil
}

// acquire tries the lock without waiting first, so the notice is written
// only when another process holds it.
func acquire(fd int, lockPath string, notify io.Writer) error {
	err := retryEINTR(func() error { return unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB) })
	if err == nil {
		return nil
	}
	if !errors.Is(err, unix.EWOULDBLOCK) {
		return fmt.Errorf("configfile: lock %s: %w", lockPath, err)
	}
	if notify != nil {
		_, _ = fmt.Fprintf(notify, "Blocking: waiting for file lock on %s\n", lockPath)
	}
	if err := retryEINTR(func() error { return unix.Flock(fd, unix.LOCK_EX) }); err != nil {
		return fmt.Errorf("configfile: lock %s: %w", lockPath, err)
	}
	return nil
}

// retryEINTR calls fn again while a signal interrupts it.
func retryEINTR(fn func() error) error {
	for {
		if err := fn(); !errors.Is(err, unix.EINTR) {
			return err
		}
	}
}
