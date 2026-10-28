package sshd

import (
	"bytes"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"golang.org/x/crypto/ssh"
)

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
			signers[i], errs[i] = LoadOrCreateHostKey(path)
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
	again, err := LoadOrCreateHostKey(path)
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
}
