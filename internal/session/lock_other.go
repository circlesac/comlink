//go:build !unix

package session

import (
	"errors"
	"fmt"
	"os"
)

// errLocked is returned by acquire and share when a session holds the lock.
var errLocked = errors.New("locked")

// acquire creates or opens the lock file. Without flock there is no lock:
// liveness falls back to pid, the pid of the record found on disk, so a
// reused pid can make a stale record look up.
func acquire(path string, pid int) (*os.File, error) {
	return share(path, pid)
}

// share is acquire: without flock there is no shared lock either, and
// nothing keeps a session from coming up while the file is open.
func share(path string, pid int) (*os.File, error) {
	if held(path, pid) {
		return nil, errLocked
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open lock file: %w", err)
	}
	return f, nil
}

// held reports whether the process with pid exists.
func held(_ string, pid int) bool {
	if pid <= 0 {
		return false
	}
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	_ = p.Release()
	return true
}
