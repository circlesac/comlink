package session

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// IncludePattern is the glob the ~/.ssh/config Include line loads: every
// session's ssh config file.
func (s *Store) IncludePattern() string { return filepath.Join(s.SSHDir(), "*.conf") }

// IncludeLine is the ~/.ssh/config line that loads the session config
// files. The pattern is double-quoted when it holds whitespace, as ssh
// requires.
func (s *Store) IncludeLine() string {
	pattern := s.IncludePattern()
	if strings.ContainsAny(pattern, " \t") {
		pattern = `"` + pattern + `"`
	}
	return "Include " + pattern
}

// SSHConfigPath is ~/.ssh/config.
func SSHConfigPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("locate ~/.ssh/config: %w", err)
	}
	return filepath.Join(home, ".ssh", "config"), nil
}

// Included reports whether the ssh config at path has an Include line for
// the session config files. A missing file includes nothing.
func (s *Store) Included(path string) (bool, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return s.includes(string(b)), nil
}

// includes reports whether config has a line `Include <pattern>`, with the
// keyword in any case, an optional '=' after it and optional double quotes
// around the pattern.
func (s *Store) includes(config string) bool {
	for _, line := range strings.Split(config, "\n") {
		line = strings.TrimSpace(line)
		// ssh separates the keyword from its argument with whitespace or '='.
		i := strings.IndexAny(line, " \t=")
		if i < 0 || !strings.EqualFold(line[:i], "Include") {
			continue
		}
		arg := strings.TrimLeft(line[i:], " \t=")
		if strings.Trim(arg, `"`) == s.IncludePattern() {
			return true
		}
	}
	return false
}

// Include puts the Include line at the top of the ssh config at path, as
// include does, and prints what it did on out.
func (s *Store) Include(path string, out io.Writer) error {
	result, err := s.include(path)
	if err != nil {
		return err
	}
	switch {
	case result.Created:
		_, _ = fmt.Fprintf(out, "created %s with %q\n", path, s.IncludeLine())
	case result.Added:
		_, _ = fmt.Fprintf(out, "added %q to the top of %s\n", s.IncludeLine(), path)
		if result.Backup != "" {
			_, _ = fmt.Fprintf(out, "previous content backed up to %s\n", result.Backup)
		}
	default:
		_, _ = fmt.Fprintf(out, "%s already includes %s; nothing changed\n", path, s.IncludePattern())
	}
	return nil
}

// includeResult says what include did to the file.
type includeResult struct {
	Added   bool   // the line was added; false when it was already there
	Created bool   // the file did not exist and was created
	Backup  string // path of the backup written before the first change, or ""
}

// include puts the Include line at the top of the ssh config at path so it
// applies before any Host or Match block. A missing file is created with
// mode 0600 (and its directory with 0700). Before the first change to an
// existing file its content is copied to path + ".comlink-bak"; a backup
// that already exists is kept. Everything else in the file is preserved
// byte for byte. Running include again changes nothing.
func (s *Store) include(path string) (includeResult, error) {
	line := s.IncludeLine() + "\n"
	old, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return includeResult{}, fmt.Errorf("create %s: %w", filepath.Dir(path), err)
		}
		if err := os.WriteFile(path, []byte(line), 0o600); err != nil {
			return includeResult{}, fmt.Errorf("write %s: %w", path, err)
		}
		return includeResult{Added: true, Created: true}, nil
	}
	if err != nil {
		return includeResult{}, err
	}
	if s.includes(string(old)) {
		return includeResult{}, nil
	}
	info, err := os.Stat(path)
	if err != nil {
		return includeResult{}, err
	}
	mode := info.Mode().Perm()
	result := includeResult{Added: true}
	backup := path + ".comlink-bak"
	if _, err := os.Stat(backup); errors.Is(err, fs.ErrNotExist) {
		if err := os.WriteFile(backup, old, mode); err != nil {
			return includeResult{}, fmt.Errorf("write backup %s: %w", backup, err)
		}
		result.Backup = backup
	}
	// Written next to path and renamed into place so the file is never
	// half-written; the mode is set before the rename.
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".comlink.*")
	if err != nil {
		return includeResult{}, fmt.Errorf("write %s: %w", path, err)
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if _, err := tmp.Write(append([]byte(line), old...)); err != nil {
		_ = tmp.Close()
		return includeResult{}, fmt.Errorf("write %s: %w", path, err)
	}
	if err := tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		return includeResult{}, fmt.Errorf("write %s: %w", path, err)
	}
	if err := tmp.Close(); err != nil {
		return includeResult{}, fmt.Errorf("write %s: %w", path, err)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return includeResult{}, fmt.Errorf("write %s: %w", path, err)
	}
	return result, nil
}
