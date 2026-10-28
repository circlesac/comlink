package main

import (
	"strings"
)

// sshConfigBlock renders a ~/.ssh/config Host block whose ProxyCommand runs
// comlink. ssh hands the ProxyCommand line to the user's shell after
// expanding %-tokens, so every argument is shell-quoted and % is doubled.
// The token file is referenced by path only; its contents never appear.
func sshConfigBlock(alias, exe, upstream, tokenFile string) string {
	args := []string{exe, "stdio", "--upstream", upstream}
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
	b.WriteString("  StrictHostKeyChecking accept-new\n")
	return b.String()
}
