package test

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/circlesac/comlink/internal/sshd"
)

// configBlock is sshd.ConfigBlock for an alias that must be accepted.
func configBlock(t *testing.T, alias string, command ...string) string {
	t.Helper()
	block, err := sshd.ConfigBlock(alias, command)
	if err != nil {
		t.Fatalf("ConfigBlock(%q): %v", alias, err)
	}
	return block
}

func TestConfigBlock(t *testing.T) {
	got := configBlock(t, "example-agent", "/opt/com link/comlink", "codex", "proxy", "--upstream", "wss://agent.example.com/codex?a=%20", "--token-file", "/home/u/token")
	want := "Host example-agent\n" +
		"  User comlink\n" +
		"  ProxyCommand '/opt/com link/comlink' codex proxy --upstream 'wss://agent.example.com/codex?a=%%20' --token-file /home/u/token\n" +
		"  HostKeyAlias comlink-example-agent\n" +
		"  StrictHostKeyChecking accept-new\n"
	if got != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}
}

// TestConfigBlockAlias: the alias is a Host pattern and part of the
// HostKeyAlias, so only letters, digits, '-', '_' and '.' are accepted and
// anything else is refused before a block is produced.
func TestConfigBlockAlias(t *testing.T) {
	for _, alias := range []string{"example-agent", "a", "Agent_2", "agent.example.com", "0-9_.-"} {
		block := configBlock(t, alias, "/x/comlink", "run")
		if !strings.HasPrefix(block, "Host "+alias+"\n") || !strings.Contains(block, "\n  HostKeyAlias comlink-"+alias+"\n") {
			t.Errorf("alias %q: block\n%s", alias, block)
		}
	}
	for _, alias := range []string{"", "two words", "agent\nHost *", "agent\r", "*", "agent?", "!agent", "agent,other", "tab\there", "agent/x", "ägent", `"agent"`} {
		block, err := sshd.ConfigBlock(alias, []string{"/x/comlink", "run"})
		if err == nil || block != "" {
			t.Errorf("alias %q accepted: %q, %v", alias, block, err)
			continue
		}
		if want := fmt.Sprintf("invalid alias %q: use only letters, digits, '-', '_' and '.'", alias); err.Error() != want {
			t.Errorf("alias %q: error %q, want %q", alias, err, want)
		}
	}
}

// proxyCommand returns the ProxyCommand line of a config block.
func proxyCommand(t *testing.T, block string) string {
	t.Helper()
	for _, line := range strings.Split(block, "\n") {
		if cmd, ok := strings.CutPrefix(line, "  ProxyCommand "); ok {
			return cmd
		}
	}
	t.Fatalf("no ProxyCommand in:\n%s", block)
	return ""
}

// TestConfigBlockQuotesArguments covers the shell quoting of each argument:
// plain words stay bare, anything else is single-quoted with embedded
// quotes escaped, and % is doubled for ssh's token expansion.
func TestConfigBlockQuotesArguments(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"plain", "plain"},
		{"/usr/local/bin/comlink", "/usr/local/bin/comlink"},
		{"wss://agent.example.com:443/codex@v1,2=3+4", "wss://agent.example.com:443/codex@v1,2=3+4"},
		{"/opt/com link/comlink", "'/opt/com link/comlink'"},
		{"it's", `'it'\''s'`},
		{"/home/u/50%", "/home/u/50%%"},
		{"", "''"},
		{"a\nb", "'a\nb'"},
		{`back\slash`, `'back\slash'`},
		{"$HOME", "'$HOME'"},
		{"a;b", "'a;b'"},
	}
	for _, tc := range cases {
		block := configBlock(t, "a", "/x/comlink", "--flag", tc.in)
		if want := "\n  ProxyCommand /x/comlink --flag " + tc.want + "\n"; !strings.Contains(block, want) {
			t.Errorf("argument %q: block\n%s\nwant %q", tc.in, block, want)
		}
	}
}

// TestConfigBlockProxyCommandSplitsBack runs the ProxyCommand line the way
// ssh does (its %-tokens expanded, then handed to the shell) and checks that
// the shell hands comlink the original arguments back.
func TestConfigBlockProxyCommandSplitsBack(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not on PATH")
	}
	exe := filepath.Join(t.TempDir(), "com link", "it's", "comlink")
	if err := os.MkdirAll(filepath.Dir(exe), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(exe, []byte("#!/bin/sh\nprintf '%s\\0' \"$0\" \"$@\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	upstream := `wss://agent.example.com/codex?a=%20&b=$HOME&c='quoted'`
	tokenFile := `/home/u/50% dir/back\slash/token`
	want := []string{exe, "codex", "proxy", "--upstream", upstream, "--token-file", tokenFile}
	line := proxyCommand(t, configBlock(t, "example-agent", want...))
	line = strings.ReplaceAll(line, "%%", "%") // ssh's token expansion

	out, err := exec.Command("sh", "-c", line).Output()
	if err != nil {
		t.Fatalf("sh -c %q: %v", line, err)
	}
	got := strings.Split(strings.TrimSuffix(string(out), "\x00"), "\x00")
	if !slices.Equal(got, want) {
		t.Fatalf("shell split %q into %q, want %q", line, got, want)
	}
}
