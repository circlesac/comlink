package session

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// maxIncludeDepth is how deep Include lines are followed, ssh's
// READCONF_MAX_DEPTH.
const maxIncludeDepth = 16

// everyHost stands for a host no Host line names: it holds only '/', which
// no alias can contain, so the only patterns that match it are wildcards
// alone (*, ?*), never one that starts, ends or contains a name, such as
// comlink-* for the names comlink derives.
const everyHost = "////////////////"

// sshConfigured reports where the user's ssh configuration already applies
// to name, which the session's Host block would shadow while it runs: the
// file and line of the first Host line in the ssh config at sshConfig, or
// in a file it Includes, with a pattern that matches name, or "" when there
// is none. A Host line that also matches everyHost (Host *, or
// Host * !other) applies to every host alike and does not count. The files
// are only read, never run through ssh: ssh -G percent-expands options such
// as ControlPath %h, runs Match exec commands and may look names up in DNS.
// Match blocks are not evaluated, and a short name that reaches a host only
// through DNS search domains has no configuration either. ssh matches Host
// patterns against the host exactly as typed (it lowercases only the
// HostName it then connects to), so name is matched as is.
func (s *Store) sshConfigured(sshConfig, name string) string {
	var hosts []hostLine
	s.scanConfig(sshConfig, filepath.Dir(sshConfig), name, nil, &hosts)
	for _, h := range hosts {
		if matchHost(name, h.patterns) && !matchHost(everyHost, h.patterns) {
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
// follows the Include lines that apply to name, the way ssh resolves them:
// one at top level or under a Host line that matches name (Host * too),
// and one under a Match block, which is not evaluated, so its Host lines
// count rather than go unseen; ~ is the home directory (the one holding
// sshDir, ~/.ssh), a relative path is relative to sshDir and globs expand.
// Files that are missing or unreadable are skipped, as are the session
// config files (comlink's own Host blocks) and a file already being read
// (an Include loop). stack is the chain of files being read; it stops at
// maxIncludeDepth.
func (s *Store) scanConfig(file, sshDir, name string, stack []string, hosts *[]hostLine) {
	b, err := os.ReadFile(file)
	if err != nil {
		return
	}
	stack = append(stack, file)
	applies := true // whether the block the line is in applies to name
	for i, line := range strings.Split(string(b), "\n") {
		keyword, args := configWords(line)
		switch {
		case strings.EqualFold(keyword, "Host"):
			*hosts = append(*hosts, hostLine{fmt.Sprintf("%s:%d", file, i+1), args})
			applies = matchHost(name, args)
		case strings.EqualFold(keyword, "Match"):
			applies = true
		case strings.EqualFold(keyword, "Include") && applies && len(stack) <= maxIncludeDepth:
			for _, arg := range args {
				for _, included := range s.includeFiles(arg, sshDir) {
					if !slices.Contains(stack, included) {
						s.scanConfig(included, sshDir, name, stack, hosts)
					}
				}
			}
		}
	}
}

// includePath is the glob an Include argument stands for, as ssh resolves
// it in a user's config: ~ is the home directory (the one holding sshDir,
// ~/.ssh) and a relative path is relative to sshDir. ok is false for
// ~user, another user's home.
func includePath(arg, sshDir string) (path string, ok bool) {
	switch {
	case arg == "~" || strings.HasPrefix(arg, "~/"):
		return filepath.Join(filepath.Dir(sshDir), arg[1:]), true
	case strings.HasPrefix(arg, "~"):
		return "", false
	case !filepath.IsAbs(arg):
		return filepath.Join(sshDir, arg), true
	}
	return filepath.Clean(arg), true
}

// includeFiles is what an Include argument names: the files its glob
// matches, without the session config files.
func (s *Store) includeFiles(arg, sshDir string) []string {
	arg, ok := includePath(arg, sshDir)
	if !ok {
		return nil // never this user's Host lines
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
// separated from its arguments by whitespace or one '=', and the arguments
// are split as argv_split does: on whitespace outside quotes, with double
// or single quotes grouping, a backslash escaping a following quote or
// backslash (and, outside quotes, a space) and otherwise kept, and a '#'
// that starts a word starting a comment. A blank or comment line has no
// keyword.
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
		var quote byte
	word:
		for ; rest != ""; rest = rest[1:] {
			c := rest[0]
			switch {
			case c == '\\' && len(rest) > 1 && (strings.IndexByte(`'"\`, rest[1]) >= 0 || quote == 0 && rest[1] == ' '):
				rest = rest[1:]
				arg.WriteByte(rest[0])
			case quote == 0 && (c == ' ' || c == '\t' || c == '\r'):
				break word
			case quote == 0 && (c == '"' || c == '\''):
				quote = c
			case quote != 0 && c == quote:
				quote = 0
			default:
				arg.WriteByte(c)
			}
		}
		args = append(args, arg.String())
	}
}

// matchHost reports whether a Host line with patterns matches name as ssh
// decides it: some pattern without '!' matches and no pattern with '!'
// does. Both are compared as written, case and all, so Host DEVBOX
// matches DEVBOX and not devbox.
func matchHost(name string, patterns []string) bool {
	matched := false
	for _, p := range patterns {
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
