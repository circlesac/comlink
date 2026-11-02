package sshd

import (
	"fmt"
	"strings"
)

// ConfigBlock renders a ~/.ssh/config Host block whose ProxyCommand runs
// command, the executable followed by its arguments. ssh hands the
// ProxyCommand line to the user's shell after expanding %-tokens, so every
// argument is shell-quoted and % is doubled. HostKeyAlias keeps comlink's
// pinned key apart from a real host called alias in known_hosts. The alias
// may hold only letters, digits, '-', '_' and '.': anything else (spaces,
// newlines, the ssh patterns '*', '?' and '!') would change what the Host
// line means, so it is refused, as is a leading '-', which ssh would read as
// an option. ssh_config is line based, so an argument
// holding a newline or carriage return, which quoting cannot keep on one
// line, is refused too.
func ConfigBlock(alias string, command []string) (string, error) {
	if !validAlias(alias) {
		return "", fmt.Errorf("invalid alias %q: use only letters, digits, '-', '_' and '.', not starting with '-'", alias)
	}
	quoted := make([]string, len(command))
	for i, a := range command {
		if strings.ContainsAny(a, "\n\r") {
			return "", fmt.Errorf("invalid ProxyCommand argument %q: it must not contain a newline or carriage return", a)
		}
		quoted[i] = strings.ReplaceAll(shellQuote(a), "%", "%%")
	}
	var b strings.Builder
	b.WriteString("Host " + alias + "\n")
	b.WriteString("  User comlink\n")
	b.WriteString("  ProxyCommand " + strings.Join(quoted, " ") + "\n")
	b.WriteString("  HostKeyAlias comlink-" + alias + "\n")
	b.WriteString("  StrictHostKeyChecking accept-new\n")
	return b.String(), nil
}

// validAlias reports whether alias is a non-empty run of letters, digits,
// '-', '_' and '.' that does not start with '-'.
func validAlias(alias string) bool {
	if alias == "" || alias[0] == '-' {
		return false
	}
	for i := 0; i < len(alias); i++ {
		c := alias[i]
		ok := c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_' || c == '.'
		if !ok {
			return false
		}
	}
	return true
}

// shellQuote quotes s for a POSIX shell. Plain words are returned as is.
func shellQuote(s string) string {
	if s == "" {
		return "''"
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		plain := c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.IndexByte("_-./:@%=+,", c) >= 0
		if !plain {
			return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
		}
	}
	return s
}
