//go:build unix

package session

import (
	"errors"
	"fmt"
	"os"
	"syscall"
)

// errLocked is returned by acquire when another process holds the lock.
var errLocked = errors.New("locked")

// acquire opens path and takes an exclusive flock on it without blocking.
// The lock lives as long as the returned file is open and is released by
// the kernel when the process ends, so it tells session processes apart from
// stale records without trusting a pid. A Close that unlinks the lock file
// may race with this; the open file is checked to still be the one at path
// and the attempt is repeated otherwise. pid is unused here.
func acquire(path string, _ int) (*os.File, error) {
	for {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
		if err != nil {
			return nil, fmt.Errorf("open lock file: %w", err)
		}
		if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
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

// held reports whether another process holds the lock at path. pid is
// unused here: the lock, not the pid, is the sign of life.
func held(path string, _ int) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer func() { _ = f.Close() }()
	err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	return errors.Is(err, syscall.EWOULDBLOCK)
}
