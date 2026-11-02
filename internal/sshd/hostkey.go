package sshd

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"golang.org/x/crypto/ssh"
)

// HostKeyPath is $XDG_STATE_HOME/comlink/host_ed25519, falling back to
// ~/.local/state/comlink/host_ed25519. It fails when XDG_STATE_HOME is unset
// and the home directory cannot be determined, rather than keeping the key
// relative to whatever directory comlink was started in.
func HostKeyPath() (string, error) {
	dir := os.Getenv("XDG_STATE_HOME")
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("locate host key: XDG_STATE_HOME is unset and the home directory is unknown: %w", err)
		}
		dir = filepath.Join(home, ".local", "state")
	}
	return filepath.Join(dir, "comlink", "host_ed25519"), nil
}

// LoadOrCreateHostKey reads the ed25519 host key at path, generating and
// persisting one (directory 0700, file 0600) on first use so that ssh's
// StrictHostKeyChecking=accept-new pins a stable key. Concurrent first runs
// (the Codex app starts several ssh processes at once) are safe: the key is
// written to a temporary file and linked into place, so path only ever
// holds a complete key and the loser of the race reads the winner's.
func LoadOrCreateHostKey(path string) (ssh.Signer, error) {
	if b, err := os.ReadFile(path); err == nil {
		signer, err := ssh.ParsePrivateKey(b)
		if err != nil {
			return nil, fmt.Errorf("parse host key %s: %w", path, err)
		}
		return signer, nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("read host key %s: %w", path, err)
	}

	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate host key: %w", err)
	}
	block, err := ssh.MarshalPrivateKey(priv, "comlink host key")
	if err != nil {
		return nil, fmt.Errorf("encode host key: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("create host key directory: %w", err)
	}
	f, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".*.tmp") // mode 0600
	if err != nil {
		return nil, fmt.Errorf("create host key %s: %w", path, err)
	}
	defer func() { _ = os.Remove(f.Name()) }()
	if err := pem.Encode(f, block); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("write host key %s: %w", path, err)
	}
	if err := f.Close(); err != nil {
		return nil, fmt.Errorf("write host key %s: %w", path, err)
	}
	if err := os.Link(f.Name(), path); errors.Is(err, fs.ErrExist) {
		// Another comlink won the race; use its key. Read it once rather than
		// starting over: a name that exists for Link but not for ReadFile (a
		// dangling symlink) would otherwise loop forever.
		b, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read host key %s: %w", path, err)
		}
		signer, err := ssh.ParsePrivateKey(b)
		if err != nil {
			return nil, fmt.Errorf("parse host key %s: %w", path, err)
		}
		return signer, nil
	} else if err != nil {
		return nil, fmt.Errorf("create host key %s: %w", path, err)
	}
	return ssh.NewSignerFromKey(priv)
}
