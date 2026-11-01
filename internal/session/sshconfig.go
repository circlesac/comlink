package session

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// maxIncludeDepth is how deep Include lines are followed, ssh's
// READCONF_MAX_DEPTH.
const maxIncludeDepth = 16

// sshConfigured reports where the user's ssh configuration already applies
// to name, which the session's Host block would shadow while it runs: the
// file and line of the first Host line in the ssh config at sshConfig, or
// in a file it Includes, with a pattern that matches name, or "" when there
// is none. A Host line that also matches a random name (Host *, or
// Host * !other) applies to every host alike and does not count. The files
// are only read, never run through ssh: ssh -G percent-expands options such
// as ControlPath %h, runs Match exec commands and may look names up in DNS.
// Match blocks are not evaluated, and a short name that reaches a host only
// through DNS search domains has no configuration either.
func (s *Store) sshConfigured(sshConfig, name string) string {
	random := make([]byte, 8)
	_, _ = rand.Read(random)
	unmatched := "comlink-probe-" + hex.EncodeToString(random)
	name = strings.ToLower(name)
	var hosts []hostLine
	s.scanConfig(sshConfig, filepath.Dir(sshConfig), nil, &hosts)
	for _, h := range hosts {
		if matchHost(name, h.patterns) && !matchHost(unmatched, h.patterns) {
			return h.where
		}
	}
	return ""
}

// hostLine is a Host line's patterns and where it is (file:line).
type hostLine struct {
	where    string
	patterns []string
}

// scanConfig appends the Host lines of the ssh config file to hosts and
// follows its Include lines, wherever they are (an Include under a block
// still brings in Host lines), the way ssh resolves them: ~ is the home
// directory (the one holding sshDir, ~/.ssh), a relative path is relative
// to sshDir and globs expand. Files that are missing or unreadable are
// skipped, as are the session config files (comlink's own Host blocks) and
// a file already being read (an Include loop). stack is the chain of files
// being read; it stops at maxIncludeDepth.
func (s *Store) scanConfig(file, sshDir string, stack []string, hosts *[]hostLine) {
	b, err := os.ReadFile(file)
	if err != nil {
		return
	}
	stack = append(stack, file)
	for i, line := range strings.Split(string(b), "\n") {
		keyword, args := configWords(line)
		switch {
		case strings.EqualFold(keyword, "Host"):
			*hosts = append(*hosts, hostLine{fmt.Sprintf("%s:%d", file, i+1), args})
		case strings.EqualFold(keyword, "Include") && len(stack) <= maxIncludeDepth:
			for _, arg := range args {
				for _, included := range s.includeFiles(arg, sshDir) {
					if !slices.Contains(stack, included) {
						s.scanConfig(included, sshDir, stack, hosts)
					}
				}
			}
		}
	}
}

// includeFiles is what an Include argument names: the files its glob
// matches, without the session config files.
func (s *Store) includeFiles(arg, sshDir string) []string {
	switch {
	case arg == "~" || strings.HasPrefix(arg, "~/"):
		arg = filepath.Join(filepath.Dir(sshDir), arg[1:])
	case strings.HasPrefix(arg, "~"):
		return nil // ~user: another user's home, never this user's Host lines
	case !filepath.IsAbs(arg):
		arg = filepath.Join(sshDir, arg)
	}
	matches, _ := filepath.Glob(arg)
	var files []string
	for _, m := range matches {
		if filepath.Dir(m) != s.SSHDir() {
			files = append(files, m)
		}
	}
	return files
}

// configWords splits an ssh config line the way ssh does: the keyword is
// separated from its arguments by whitespace or one '=', the arguments are
// split on whitespace with double quotes grouping, and a '#' that starts a
// word starts a comment. A blank or comment line has no keyword.
func configWords(line string) (keyword string, args []string) {
	line = strings.TrimLeft(line, " \t\r")
	if line == "" || line[0] == '#' {
		return "", nil
	}
	i := strings.IndexAny(line, " \t\r=")
	if i < 0 {
		return line, nil
	}
	keyword = line[:i]
	rest := strings.TrimPrefix(strings.TrimLeft(line[i:], " \t\r"), "=")
	for {
		rest = strings.TrimLeft(rest, " \t\r")
		if rest == "" || rest[0] == '#' {
			return keyword, args
		}
		var arg strings.Builder
		quoted := false
		for ; rest != ""; rest = rest[1:] {
			c := rest[0]
			if c == '"' {
				quoted = !quoted
				continue
			}
			if !quoted && (c == ' ' || c == '\t' || c == '\r') {
				break
			}
			arg.WriteByte(c)
		}
		args = append(args, arg.String())
	}
}

// matchHost reports whether a Host line with patterns matches the
// lowercased name as ssh decides it: some pattern without '!' matches and
// no pattern with '!' does. Patterns are compared lowercased.
func matchHost(name string, patterns []string) bool {
	matched := false
	for _, p := range patterns {
		p = strings.ToLower(p)
		if negated, ok := strings.CutPrefix(p, "!"); ok {
			if matchWildcard(negated, name) {
				return false
			}
		} else if matchWildcard(p, name) {
			matched = true
		}
	}
	return matched
}

// matchWildcard matches s against pattern, where '*' is any run of
// characters and '?' is any one; nothing else is special, as in ssh.
func matchWildcard(pattern, s string) bool {
	for pattern != "" {
		switch pattern[0] {
		case '*':
			for i := range len(s) + 1 {
				if matchWildcard(pattern[1:], s[i:]) {
					return true
				}
			}
			return false
		case '?':
			if s == "" {
				return false
			}
		default:
			if s == "" || s[0] != pattern[0] {
				return false
			}
		}
		pattern, s = pattern[1:], s[1:]
	}
	return s == ""
}
