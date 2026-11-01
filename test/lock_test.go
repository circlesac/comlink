//go:build unix

package test

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"

	"github.com/circlesac/comlink/internal/session"
)

// TestSessionLockLooks: a session holds its lock exclusively and a look at
// a name (Load, List, another process's look) takes it shared for an
// instant. The test holds a shared lock on a stale name's lock file, as a
// concurrent look would: the name is still not up for Load, List still
// removes its stale files, and a name a session holds is still up.
func TestSessionLockLooks(t *testing.T) {
	dir := t.TempDir()
	store := &session.Store{Dir: filepath.Join(dir, "comlink")}
	writeStaleSession(t, dir, "gone")
	record, token, lock, conf := sessionFiles(dir, "gone")
	look, err := os.Open(lock)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = look.Close() }()
	if err := syscall.Flock(int(look.Fd()), syscall.LOCK_SH|syscall.LOCK_NB); err != nil {
		t.Fatal(err)
	}

	var notUp *session.NotUpError
	if r, _, err := store.Load("gone"); !errors.As(err, &notUp) {
		t.Fatalf("Load of a stale name during a look = %+v, %v; want not up", r, err)
	}
	live, stale, err := store.List()
	if err != nil || len(live) != 0 || len(stale) != 1 || stale[0].Name != "gone" {
		t.Fatalf("List during a look = %+v, %+v, %v; want gone stale", live, stale, err)
	}
	absent(t, record, token, conf)

	sess, err := store.Start("held", "wss://agent.example.com", testToken, "x")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sess.Close() }()
	if _, _, err := store.Load("held"); err != nil {
		t.Fatalf("Load of a held name: %v", err)
	}
	if live, _, err := store.List(); err != nil || len(live) != 1 || live[0].Name != "held" {
		t.Fatalf("List with a held name = %+v, %v", live, err)
	}
	if _, err := store.Start("held", "wss://other.example.com", "", "y"); err == nil || err.Error() != `session "held" is already up (pid `+strconv.Itoa(os.Getpid())+`)` {
		t.Fatalf("Start of a held name: %v", err)
	}
}
