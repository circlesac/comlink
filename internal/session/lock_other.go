//go:build !unix

package session

import (
	"errors"
	"fmt"
	"os"
)

// errLocked is returned by acquire when another process holds the lock.
var errLocked = errors.New("locked")

// acquire creates or opens the lock file. Without flock there is no lock:
// liveness falls back to pid, the pid of the record found on disk, so a
// reused pid can make a stale record look up.
func acquire(path string, pid int) (*os.File, error) {
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
	_, err := os.FindProcess(pid)
	return err == nil
}
