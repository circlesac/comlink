//go:build unix

package session

import (
	"errors"
	"fmt"
	"os"
	"syscall"
)

// errLocked is returned by acquire and share when a session holds the lock.
var errLocked = errors.New("locked")

// A session holds its lock exclusively (LOCK_EX) for its lifetime; everyone
// else only looks, with a shared lock (LOCK_SH) held for the instant of the
// look. Shared locks do not conflict with each other, so two looks at a
// stale name both see it free, and a look never makes a name seem up.

// acquire opens path and takes an exclusive flock on it without blocking.
// The lock lives as long as the returned file is open and is released by
// the kernel when the process ends, so it tells session processes apart from
// stale records without trusting a pid. When the exclusive lock is refused,
// a shared lock tells why: granted, only shared locks (looks) are in the
// way, so it is dropped and the exclusive lock tried again once they are
// gone, which is the next instant; refused, a session holds the name and
// acquire returns errLocked. A Close that unlinks the lock file may race
// with this; the open file is checked to still be the one at path and the
// attempt is repeated otherwise. pid is unused here.
func acquire(path string, _ int) (*os.File, error) {
	for {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
		if err != nil {
			return nil, fmt.Errorf("open lock file: %w", err)
		}
		err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if errors.Is(err, syscall.EWOULDBLOCK) {
			err = syscall.Flock(int(f.Fd()), syscall.LOCK_SH|syscall.LOCK_NB)
			_ = f.Close()
			if err == nil {
				continue
			}
			if errors.Is(err, syscall.EWOULDBLOCK) {
				return nil, errLocked
			}
			return nil, fmt.Errorf("lock %s: %w", path, err)
		}
		if err != nil {
			_ = f.Close()
			return nil, fmt.Errorf("lock %s: %w", path, err)
		}
		if sameFile(f, path) {
			return f, nil
		}
		_ = f.Close()
	}
}

// share opens path, creating it when missing, and takes a shared flock on
// it without blocking: errLocked when a session holds the name. While the
// returned file is open no session can take the name, so its stale files
// can be removed without racing a session that comes up. Like acquire it
// retries when a Close unlinked the file it opened. pid is unused here.
func share(path string, _ int) (*os.File, error) {
	for {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
		if err != nil {
			return nil, fmt.Errorf("open lock file: %w", err)
		}
		if err := syscall.Flock(int(f.Fd()), syscall.LOCK_SH|syscall.LOCK_NB); err != nil {
			_ = f.Close()
			if errors.Is(err, syscall.EWOULDBLOCK) {
				return nil, errLocked
			}
			return nil, fmt.Errorf("lock %s: %w", path, err)
		}
		if sameFile(f, path) {
			return f, nil
		}
		_ = f.Close()
	}
}

// sameFile reports whether f is still the file at path.
func sameFile(f *os.File, path string) bool {
	opened, err := f.Stat()
	if err != nil {
		return false
	}
	onDisk, err := os.Stat(path)
	return err == nil && os.SameFile(opened, onDisk)
}

// held reports whether a session holds the lock at path: whether a shared
// lock is refused. It creates nothing. pid is unused here: the lock, not the
// pid, is the sign of life.
func held(path string, _ int) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer func() { _ = f.Close() }()
	err = syscall.Flock(int(f.Fd()), syscall.LOCK_SH|syscall.LOCK_NB)
	return errors.Is(err, syscall.EWOULDBLOCK)
}
