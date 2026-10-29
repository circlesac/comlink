package sshd

import (
	"fmt"
	"strings"
)

// ConfigBlock renders a ~/.ssh/config Host block whose ProxyCommand runs
// 'comlink codex proxy'. ssh hands the ProxyCommand line to the user's shell after expanding
// %-tokens, so every argument is shell-quoted and % is doubled. The token
// file is referenced by path only; its contents never appear. HostKeyAlias
// keeps comlink's pinned key apart from a real host called alias in
// known_hosts. The alias may hold only letters, digits, '-', '_' and '.':
// anything else (spaces, newlines, the ssh patterns '*', '?' and '!') would
// change what the Host line means, so it is refused.
func ConfigBlock(alias, exe, upstream, tokenFile string) (string, error) {
	if !validAlias(alias) {
		return "", fmt.Errorf("invalid alias %q: use only letters, digits, '-', '_' and '.'", alias)
	}
	args := []string{exe, "codex", "proxy", "--upstream", upstream}
	if tokenFile != "" {
		args = append(args, "--token-file", tokenFile)
	}
	quoted := make([]string, len(args))
	for i, a := range args {
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
// '-', '_' and '.'.
func validAlias(alias string) bool {
	if alias == "" {
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
