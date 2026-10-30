// Package session is the registry of named sessions. `comlink up --name
// NAME` registers NAME for as long as it runs; `comlink codex proxy --name
// NAME` and `comlink acp --name NAME` look NAME up to find the server, and
// `comlink list` shows what is up. Everything lives under the state
// directory ($XDG_STATE_HOME/comlink, default ~/.local/state/comlink):
//
//	run/NAME.json   the record: name, server, the two endpoints and the pid
//	run/NAME.token  the bearer token, mode 0600 (never in the record)
//	run/NAME.lock   held with flock by the `up` process for its lifetime
//	ssh/NAME.conf   the ~/.ssh/config Host block for the Codex app
//
// A session is up while its lock is held: the lock is released by the
// kernel when the `up` process ends, however it ends, so a record whose lock
// is free is stale even if its pid has been reused by another process. The
// pid in the record is for display only.
package session

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Record is what `up` registers for a name.
type Record struct {
	Name   string `json:"name"`
	Server string `json:"server"` // base URL; the token is never here
	Codex  string `json:"codex"`  // <server>/codex, the Codex app-server endpoint
	ACP    string `json:"acp"`    // <server>/acp, the ACP endpoint
	PID    int    `json:"pid"`
}

// NotUpError is returned when a name has no live session.
type NotUpError struct{ Name string }

func (e *NotUpError) Error() string {
	return fmt.Sprintf("session %q is not up: run 'comlink up --name %s --server URL' first", e.Name, e.Name)
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
func (s *Store) lockPath(n string) string   { return filepath.Join(s.runDir(), n+".lock") }
func (s *Store) confPath(n string) string   { return filepath.Join(s.SSHDir(), n+".conf") }

// ValidName reports whether name is a non-empty run of letters, digits,
// '-', '_' and '.': it is a file name here and an ssh Host alias in the
// config block.
func ValidName(name string) bool {
	if name == "" {
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

// Endpoints returns the Codex app-server and ACP endpoints a comlink server
// serves under its base URL: <server>/codex and <server>/acp, joined
// whether or not the base has a path prefix or a trailing slash. The scheme
// must be ws or wss.
func Endpoints(server string) (codex, acp string, err error) {
	u, err := url.Parse(server)
	if err != nil {
		return "", "", fmt.Errorf("server URL: %w", err)
	}
	if (u.Scheme != "ws" && u.Scheme != "wss") || u.Host == "" {
		return "", "", fmt.Errorf("server URL %q: want ws://host[/prefix] or wss://host[/prefix]", server)
	}
	return u.JoinPath("codex").String(), u.JoinPath("acp").String(), nil
}

// Session is a registered name, held by the `up` process until Close.
type Session struct {
	Record
	Replaced *Record // the stale record Start removed, if any
	store    *Store
	lock     *os.File
}

// Start registers name with server and token, writes sshConfig as the
// name's ssh config file, and returns the session, which stays up until
// Close. It fails when name is already up. A stale record (its `up` process
// is gone) is removed and replaced, and returned as Replaced.
func (s *Store) Start(name, server, token, sshConfig string) (*Session, error) {
	if !ValidName(name) {
		return nil, fmt.Errorf("invalid name %q: use only letters, digits, '-', '_' and '.'", name)
	}
	codex, acp, err := Endpoints(server)
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
			return nil, fmt.Errorf("session %q is already up (pid %d)", name, old.PID)
		}
		return nil, err
	}
	sess := &Session{
		Record: Record{Name: name, Server: server, Codex: codex, ACP: acp, PID: os.Getpid()},
		store:  s,
		lock:   lock,
	}
	// The lock is ours, so whatever a previous `up` left behind is stale.
	if oldErr == nil {
		sess.Replaced = &old
	}
	s.removeFiles(name)
	if err := sess.write(token, sshConfig); err != nil {
		_ = sess.Close()
		return nil, err
	}
	return sess, nil
}

// write creates the record, the token file and the ssh config file.
func (sess *Session) write(token, sshConfig string) error {
	s, name := sess.store, sess.Name
	if err := os.WriteFile(s.tokenPath(name), []byte(token), 0o600); err != nil {
		return fmt.Errorf("write token file: %w", err)
	}
	if err := os.WriteFile(s.confPath(name), []byte(sshConfig), 0o600); err != nil {
		return fmt.Errorf("write ssh config: %w", err)
	}
	b, err := json.MarshalIndent(sess.Record, "", "  ")
	if err != nil {
		return err
	}
	// The record is renamed into place so a reader never sees a partial one.
	tmp := s.recordPath(name) + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o600); err != nil {
		return fmt.Errorf("write record: %w", err)
	}
	if err := os.Rename(tmp, s.recordPath(name)); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("write record: %w", err)
	}
	return nil
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

// Load returns the record and the token of name when it is up, and a
// *NotUpError when it has no record or its `up` process is gone. It does
// not remove a stale record; List and Start do.
func (s *Store) Load(name string) (Record, string, error) {
	if !ValidName(name) {
		return Record{}, "", fmt.Errorf("invalid name %q: use only letters, digits, '-', '_' and '.'", name)
	}
	r, err := s.read(name)
	if errors.Is(err, fs.ErrNotExist) {
		return Record{}, "", &NotUpError{Name: name}
	}
	if err != nil {
		return Record{}, "", err
	}
	if !held(s.lockPath(name), r.PID) {
		return Record{}, "", &NotUpError{Name: name}
	}
	token, err := os.ReadFile(s.tokenPath(name))
	if err != nil {
		return Record{}, "", fmt.Errorf("read token file: %w", err)
	}
	return r, strings.TrimSpace(string(token)), nil
}

// List returns the records that are up, sorted by name, and removes and
// returns those whose `up` process is gone.
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
		if held(s.lockPath(name), r.PID) {
			live = append(live, r)
			continue
		}
		s.removeFiles(name)
		stale = append(stale, r)
	}
	sort.Slice(live, func(i, j int) bool { return live[i].Name < live[j].Name })
	sort.Slice(stale, func(i, j int) bool { return stale[i].Name < stale[j].Name })
	return live, stale, nil
}
