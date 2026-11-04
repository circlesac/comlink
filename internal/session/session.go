// Package session is the registry of the Codex app's named sessions.
// `comlink codex --server URL [--name NAME]` registers NAME (by default the
// first label of the server's host) for as long as it runs;
// `comlink codex --stdio --name NAME`, which ssh runs for the alias, looks NAME
// up to find the server, and `comlink codex list` shows what is up.
// Everything lives under the state directory ($XDG_STATE_HOME/comlink,
// default ~/.local/state/comlink):
//
//	run/NAME.json   the record: name, server, the /codex endpoint and the pid
//	run/NAME.token          the bearer token, mode 0600 (never in the record)
//	run/NAME.token-command  instead of the token: the command that prints one
//	                        with the working directory and PATH it was given
//	                        with (JSON), run for each connection, mode 0600
//	run/NAME.lock   held with flock by the session process for its lifetime
//	ssh/NAME.conf   the ~/.ssh/config Host block for the Codex app
//
// The record, the token (or token command) file and the ssh config file are each written to a
// temporary file in the same directory and renamed into place, so ssh, the
// Codex app and `codex --stdio --name` never read a half-written file.
//
// A session is up while its lock is held: the lock is released by the
// kernel when the session process ends, however it ends, so a record whose
// lock is free is stale even if its pid has been reused by another process.
// The pid in the record is for display only.
package session

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/circlesac/comlink/internal/upstream"
)

// Record is what `comlink codex` registers for a name.
type Record struct {
	Name   string `json:"name"`
	Server string `json:"server"` // base URL; the token is never here
	Codex  string `json:"codex"`  // <server>/codex, the Codex app-server endpoint
	PID    int    `json:"pid"`
}

// NotUpError is returned when a name has no live session.
type NotUpError struct{ Name string }

func (e *NotUpError) Error() string {
	return fmt.Sprintf("session %q is not up: run 'comlink codex --name %s --server URL' first", e.Name, e.Name)
}

// Store is the registry under one state directory.
type Store struct{ Dir string }

// Open returns the store under $XDG_STATE_HOME/comlink, falling back to
// ~/.local/state/comlink. It fails when XDG_STATE_HOME is unset and the home
// directory cannot be determined.
func Open() (*Store, error) {
	dir := os.Getenv("XDG_STATE_HOME")
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, fmt.Errorf("locate state directory: XDG_STATE_HOME is unset and the home directory is unknown: %w", err)
		}
		dir = filepath.Join(home, ".local", "state")
	}
	return &Store{Dir: filepath.Join(dir, "comlink")}, nil
}

// SSHDir is the directory of the per-session ssh config files.
func (s *Store) SSHDir() string { return filepath.Join(s.Dir, "ssh") }

func (s *Store) runDir() string             { return filepath.Join(s.Dir, "run") }
func (s *Store) recordPath(n string) string { return filepath.Join(s.runDir(), n+".json") }
func (s *Store) tokenPath(n string) string  { return filepath.Join(s.runDir(), n+".token") }
func (s *Store) commandPath(n string) string {
	return filepath.Join(s.runDir(), n+".token-command")
}
func (s *Store) lockPath(n string) string { return filepath.Join(s.runDir(), n+".lock") }
func (s *Store) confPath(n string) string { return filepath.Join(s.SSHDir(), n+".conf") }

// ValidName reports whether name is a non-empty run of letters, digits,
// '-', '_' and '.' that does not start with '-': it is a file name here and
// an ssh Host alias in the config block, and ssh would read `ssh -name` as
// an option.
func ValidName(name string) bool {
	if name == "" || name[0] == '-' {
		return false
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		ok := c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_' || c == '.'
		if !ok {
			return false
		}
	}
	return true
}

// upError is Start's error for a name that is up.
type upError struct {
	name string
	pid  int // 0 when the record does not say
}

func (e *upError) Error() string {
	if e.pid <= 0 {
		return fmt.Sprintf("session %q is already up", e.name)
	}
	return fmt.Sprintf("session %q is already up (pid %d)", e.name, e.pid)
}

// tokenCommand is the run/NAME.token-command file.
type tokenCommand struct {
	Command string `json:"command"`
	Dir     string `json:"dir"`
	Path    string `json:"path"`
}

// Session is a registered name, held by the session process until Close.
type Session struct {
	Record
	Replaced *Record // the stale record Start removed, if any
	store    *Store
	lock     *os.File
}

// Start registers name with server and credential cred (its token, or its
// token command, which is stored rather than run), writes sshConfig as the
// name's ssh config file, and returns the session, which stays up until
// Close. It fails when name is already up. A stale record (its session
// process is gone) is removed and replaced, and returned as Replaced.
func (s *Store) Start(name, server string, cred upstream.Credential, sshConfig string) (*Session, error) {
	if !ValidName(name) {
		return nil, fmt.Errorf("invalid name %q: use only letters, digits, '-', '_' and '.', not starting with '-'", name)
	}
	codex, err := upstream.Endpoint(server, "codex")
	if err != nil {
		return nil, err
	}
	for _, dir := range []string{s.runDir(), s.SSHDir()} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, fmt.Errorf("create %s: %w", dir, err)
		}
	}
	old, oldErr := s.read(name)
	lock, err := acquire(s.lockPath(name), old.PID)
	if err != nil {
		if errors.Is(err, errLocked) {
			// The record can be missing or unreadable while the lock is
			// held (deleted, or the moment inside Close between removing
			// it and releasing the lock); then there is no pid to name.
			if oldErr != nil {
				old.PID = 0
			}
			return nil, &upError{name: name, pid: old.PID}
		}
		return nil, err
	}
	sess := &Session{
		Record: Record{Name: name, Server: server, Codex: codex, PID: os.Getpid()},
		store:  s,
		lock:   lock,
	}
	// The lock is ours, so whatever a previous session left behind is stale.
	if oldErr == nil {
		sess.Replaced = &old
	}
	s.removeFiles(name)
	if err := sess.write(cred, sshConfig); err != nil {
		_ = sess.Close()
		return nil, err
	}
	return sess, nil
}

// write creates the token file (or, for a token command, the token command
// file), the ssh config file and the record, each renamed into place whole.
func (sess *Session) write(cred upstream.Credential, sshConfig string) error {
	s, name := sess.store, sess.Name
	if cred.Command != "" {
		b, err := json.Marshal(tokenCommand{Command: cred.Command, Dir: cred.Dir, Path: cred.Path})
		if err != nil {
			return err
		}
		if err := writeFile(s.commandPath(name), b, 0o600); err != nil {
			return fmt.Errorf("write token command file: %w", err)
		}
	} else if err := writeFile(s.tokenPath(name), []byte(cred.Token), 0o600); err != nil {
		return fmt.Errorf("write token file: %w", err)
	}
	if err := writeFile(s.confPath(name), []byte(sshConfig), 0o600); err != nil {
		return fmt.Errorf("write ssh config: %w", err)
	}
	b, err := json.MarshalIndent(sess.Record, "", "  ")
	if err != nil {
		return err
	}
	if err := writeFile(s.recordPath(name), append(b, '\n'), 0o600); err != nil {
		return fmt.Errorf("write record: %w", err)
	}
	return nil
}

// writeFile replaces path with data and mode atomically: it writes a
// temporary file in the same directory, sets its mode and renames it over
// path, so a reader sees the old file or the new one and never part of
// either. The temporary name ends in ".tmp" plus a random suffix, which
// neither the record (*.json) nor the ssh config (*.conf) globs match. The
// temporary file is removed when anything fails.
func writeFile(path string, data []byte, mode os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".tmp*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// Close removes the files Start created and releases the lock. The lock
// file is removed before the lock is released, so a concurrent Start on the
// same name sees a fresh file rather than one about to vanish.
func (sess *Session) Close() error {
	sess.store.removeFiles(sess.Name)
	_ = os.Remove(sess.store.lockPath(sess.Name))
	return sess.lock.Close()
}

// removeFiles removes the record, the token file and the ssh config file of
// name, but not the lock file: only its holder removes that.
func (s *Store) removeFiles(name string) {
	_ = os.Remove(s.recordPath(name))
	_ = os.Remove(s.tokenPath(name))
	_ = os.Remove(s.commandPath(name))
	_ = os.Remove(s.confPath(name))
}

// read returns the record of name as it is on disk, live or not.
func (s *Store) read(name string) (Record, error) {
	var r Record
	b, err := os.ReadFile(s.recordPath(name))
	if err != nil {
		return r, err
	}
	if err := json.Unmarshal(b, &r); err != nil {
		return r, fmt.Errorf("parse %s: %w", s.recordPath(name), err)
	}
	return r, nil
}

// Load returns the record and the credential of name when it is up (a
// token command is returned, not run), and a *NotUpError when it has no
// record or token file (as inside Close) or its session process is gone. It
// does not remove a stale record; List and Start do.
func (s *Store) Load(name string) (Record, upstream.Credential, error) {
	if !ValidName(name) {
		return Record{}, upstream.Credential{}, fmt.Errorf("invalid name %q: use only letters, digits, '-', '_' and '.', not starting with '-'", name)
	}
	r, err := s.read(name)
	if errors.Is(err, fs.ErrNotExist) {
		return Record{}, upstream.Credential{}, &NotUpError{Name: name}
	}
	if err != nil {
		return Record{}, upstream.Credential{}, err
	}
	if !held(s.lockPath(name), r.PID) {
		return Record{}, upstream.Credential{}, &NotUpError{Name: name}
	}
	command, err := os.ReadFile(s.commandPath(name))
	if err == nil {
		var tc tokenCommand
		if err := json.Unmarshal(command, &tc); err != nil || tc.Command == "" {
			return Record{}, upstream.Credential{}, fmt.Errorf("read token command file: not a token command")
		}
		return r, upstream.Credential{Command: tc.Command, Dir: tc.Dir, Path: tc.Path}, nil
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return Record{}, upstream.Credential{}, fmt.Errorf("read token command file: %w", err)
	}
	token, err := os.ReadFile(s.tokenPath(name))
	if errors.Is(err, fs.ErrNotExist) {
		// Close removes the files before it releases the lock, so a session
		// that is ending can be held without its token.
		return Record{}, upstream.Credential{}, &NotUpError{Name: name}
	}
	if err != nil {
		return Record{}, upstream.Credential{}, fmt.Errorf("read token file: %w", err)
	}
	return r, upstream.Credential{Token: strings.TrimSpace(string(token))}, nil
}

// List returns the records that are up, sorted by name, and removes and
// returns those whose session process is gone. A stale name's files are
// removed only while List holds its lock shared, which keeps a session from
// taking the name meanwhile; a name a session holds by then is live, and
// one whose record is gone by then (its session closed) is neither. A name
// whose lock file cannot be opened is left alone.
func (s *Store) List() (live, stale []Record, err error) {
	entries, err := os.ReadDir(s.runDir())
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, fmt.Errorf("read %s: %w", s.runDir(), err)
	}
	for _, e := range entries {
		name, ok := strings.CutSuffix(e.Name(), ".json")
		if !ok || !ValidName(name) {
			continue
		}
		r, err := s.read(name)
		if err != nil {
			// A record that is being renamed into place or cannot be parsed is
			// not a session; it is left alone.
			continue
		}
		lock, err := share(s.lockPath(name), r.PID)
		if errors.Is(err, errLocked) {
			// Read again: a session may have replaced the record read above.
			if r, err := s.read(name); err == nil {
				live = append(live, r)
			}
			continue
		}
		if err != nil {
			continue // a lock file that cannot be opened: left alone
		}
		if r, err = s.read(name); err == nil {
			s.removeFiles(name)
			stale = append(stale, r)
		}
		_ = lock.Close()
	}
	sort.Slice(live, func(i, j int) bool { return live[i].Name < live[j].Name })
	sort.Slice(stale, func(i, j int) bool { return stale[i].Name < stale[j].Name })
	return live, stale, nil
}
