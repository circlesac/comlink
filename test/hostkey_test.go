package test

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"golang.org/x/crypto/ssh"

	"github.com/circlesac/comlink/internal/sshd"
)

func TestHostKeyPath(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", "/var/state")
	if got, want := sshd.HostKeyPath(), filepath.Join("/var/state", "comlink", "host_ed25519"); got != want {
		t.Fatalf("with XDG_STATE_HOME: %q, want %q", got, want)
	}
	t.Setenv("XDG_STATE_HOME", "")
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	if got, want := sshd.HostKeyPath(), filepath.Join(home, ".local", "state", "comlink", "host_ed25519"); got != want {
		t.Fatalf("without XDG_STATE_HOME: %q, want %q", got, want)
	}
	t.Setenv("HOME", "")
	if got, want := sshd.HostKeyPath(), filepath.Join(".local", "state", "comlink", "host_ed25519"); got != want {
		t.Fatalf("without a home directory: %q, want %q", got, want)
	}
}

func TestLoadOrCreateHostKeyConcurrentFirstRuns(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state", "comlink", "host_ed25519")
	const n = 32
	signers := make([]ssh.Signer, n)
	errs := make([]error, n)
	var start, done sync.WaitGroup
	start.Add(1)
	for i := range n {
		done.Add(1)
		go func() {
			defer done.Done()
			start.Wait()
			signers[i], errs[i] = sshd.LoadOrCreateHostKey(path)
		}()
	}
	start.Done()
	done.Wait()

	for i := range n {
		if errs[i] != nil {
			t.Fatalf("call %d: %v", i, errs[i])
		}
		if !bytes.Equal(signers[i].PublicKey().Marshal(), signers[0].PublicKey().Marshal()) {
			t.Fatalf("call %d returned a different public key", i)
		}
	}
	again, err := sshd.LoadOrCreateHostKey(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(again.PublicKey().Marshal(), signers[0].PublicKey().Marshal()) {
		t.Fatal("the persisted key differs from the one returned")
	}
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "host_ed25519" {
		t.Fatalf("directory holds %v, want only host_ed25519 (temporary files removed)", entries)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("host key mode = %o, want 600", perm)
	}
	dirInfo, err := os.Stat(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	if perm := dirInfo.Mode().Perm(); perm != 0o700 {
		t.Fatalf("host key directory mode = %o, want 700", perm)
	}
}

func TestLoadOrCreateHostKeyErrors(t *testing.T) {
	dir := t.TempDir()

	corrupt := filepath.Join(dir, "corrupt")
	if err := os.WriteFile(corrupt, []byte("not a key\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := sshd.LoadOrCreateHostKey(corrupt); err == nil || !strings.HasPrefix(err.Error(), "parse host key "+corrupt+": ") {
		t.Fatalf("corrupt key: %v, want a parse error", err)
	}

	isDir := filepath.Join(dir, "is-a-directory")
	if err := os.Mkdir(isDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := sshd.LoadOrCreateHostKey(isDir); err == nil || !strings.HasPrefix(err.Error(), "read host key "+isDir+": ") {
		t.Fatalf("directory as key: %v, want a read error", err)
	}

	// A file in the way of the path is a read failure, not a missing key.
	underFile := filepath.Join(corrupt, "host_ed25519")
	if _, err := sshd.LoadOrCreateHostKey(underFile); err == nil || !strings.HasPrefix(err.Error(), "read host key "+underFile+": ") {
		t.Fatalf("parent is a file: %v, want a read error", err)
	}

	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	readOnly := filepath.Join(dir, "read-only")
	if err := os.Mkdir(readOnly, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(readOnly, 0o700) })
	unwritable := filepath.Join(readOnly, "host_ed25519")
	if _, err := sshd.LoadOrCreateHostKey(unwritable); err == nil || !strings.HasPrefix(err.Error(), "create host key "+unwritable+": ") {
		t.Fatalf("unwritable directory: %v, want a create error", err)
	}
	noDir := filepath.Join(readOnly, "comlink", "host_ed25519")
	if _, err := sshd.LoadOrCreateHostKey(noDir); err == nil || !strings.HasPrefix(err.Error(), "create host key directory: ") {
		t.Fatalf("directory cannot be created: %v, want a directory error", err)
	}
}
