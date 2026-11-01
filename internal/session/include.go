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

// includePattern is the glob the ~/.ssh/config Include line loads: every
// session's ssh config file.
func (s *Store) includePattern() string { return filepath.Join(s.SSHDir(), "*.conf") }

// includeLine is the ~/.ssh/config line that loads the session config
// files. The pattern is double-quoted when it holds whitespace, as ssh
// requires.
func (s *Store) includeLine() string {
	pattern := s.includePattern()
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

// includes reports whether config has a line `Include <pattern>` at top
// level, before the first Host or Match keyword (ssh applies an Include
// under a block only to that block), with the keyword in any case, an
// optional '=' after it and optional double quotes around the pattern.
func (s *Store) includes(config string) bool {
	for _, line := range strings.Split(config, "\n") {
		line = strings.TrimSpace(line)
		// ssh separates the keyword from its argument with whitespace or '='.
		i := strings.IndexAny(line, " \t=")
		if i < 0 {
			i = len(line)
		}
		if strings.EqualFold(line[:i], "Host") || strings.EqualFold(line[:i], "Match") {
			return false
		}
		if !strings.EqualFold(line[:i], "Include") {
			continue
		}
		arg := strings.TrimLeft(line[i:], " \t=")
		if strings.Trim(arg, `"`) == s.includePattern() {
			return true
		}
	}
	return false
}

// addInclude makes sure the ssh config at path has the Include line, adding
// it as include does, and says on out in one line when it changed the file.
// When path is a symbolic link the file it names is the one changed, and the
// message shows both. Nothing is printed when the line was already there.
func (s *Store) addInclude(path string, out io.Writer) error {
	result, err := s.include(path)
	if err != nil {
		return err
	}
	shown := path
	if result.Target != path {
		shown = path + " -> " + result.Target
	}
	switch {
	case result.Created:
		_, _ = fmt.Fprintf(out, "comlink: created %s with %q\n", shown, s.includeLine())
	case result.Backup != "":
		_, _ = fmt.Fprintf(out, "comlink: added %q to the top of %s; previous content backed up to %s\n", s.includeLine(), shown, result.Backup)
	case result.Added:
		_, _ = fmt.Fprintf(out, "comlink: added %q to the top of %s\n", s.includeLine(), shown)
	}
	return nil
}

// includeResult says what include did to the file.
type includeResult struct {
	Target  string // the file read and written: path, or what its symlinks name
	Added   bool   // the line was added; false when it was already there
	Created bool   // the file did not exist and was created
	Backup  string // path of the backup written before the first change, or ""
}

// include puts the Include line at the top of the ssh config at path so it
// applies before any Host or Match block. When path is a symbolic link (or a
// chain of them) the file it finally names is changed and the links are left
// as they are; a link to a file that does not exist yet creates that file.
// A missing file is created with mode 0600 (and its directory with 0700).
// Before the first change to an existing file its content is copied to the
// file's own path + ".comlink-bak" (next to the target, not the link); a
// backup that already exists is kept. Everything else in the file is
// preserved byte for byte and its mode is kept. The new content is renamed
// into place whole. When the line is already there nothing is written.
func (s *Store) include(path string) (includeResult, error) {
	target, err := resolveLinks(path)
	if err != nil {
		return includeResult{}, err
	}
	line := s.includeLine() + "\n"
	old, err := os.ReadFile(target)
	if errors.Is(err, fs.ErrNotExist) {
		if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
			return includeResult{}, fmt.Errorf("create %s: %w", filepath.Dir(target), err)
		}
		if err := writeFile(target, []byte(line), 0o600); err != nil {
			return includeResult{}, fmt.Errorf("write %s: %w", target, err)
		}
		return includeResult{Target: target, Added: true, Created: true}, nil
	}
	if err != nil {
		return includeResult{}, err
	}
	if s.includes(string(old)) {
		return includeResult{Target: target}, nil
	}
	info, err := os.Stat(target)
	if err != nil {
		return includeResult{}, err
	}
	mode := info.Mode().Perm()
	result := includeResult{Target: target, Added: true}
	backup := target + ".comlink-bak"
	if _, err := os.Stat(backup); errors.Is(err, fs.ErrNotExist) {
		if err := os.WriteFile(backup, old, mode); err != nil {
			return includeResult{}, fmt.Errorf("write backup %s: %w", backup, err)
		}
		result.Backup = backup
	}
	if err := writeFile(target, append([]byte(line), old...), mode); err != nil {
		return includeResult{}, fmt.Errorf("write %s: %w", target, err)
	}
	return result, nil
}

// resolveLinks follows path through symbolic links to the path they finally
// name, which need not exist. Only links at the last component are followed:
// the path is returned as written when it is not a link. A relative link is
// taken relative to the real directory that holds it.
func resolveLinks(path string) (string, error) {
	for range 40 { // the kernel's own limit on a chain of links
		info, err := os.Lstat(path)
		if errors.Is(err, fs.ErrNotExist) || err == nil && info.Mode()&fs.ModeSymlink == 0 {
			return path, nil
		}
		if err != nil {
			return "", err
		}
		dest, err := os.Readlink(path)
		if err != nil {
			return "", err
		}
		if !filepath.IsAbs(dest) {
			dir, err := filepath.EvalSymlinks(filepath.Dir(path))
			if err != nil {
				return "", err
			}
			dest = filepath.Join(dir, dest)
		}
		path = dest
	}
	return "", fmt.Errorf("resolve %s: too many levels of symbolic links", path)
}
