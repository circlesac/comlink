package test

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/circlesac/comlink/internal/sshd"
)

func TestConfigBlock(t *testing.T) {
	got := sshd.ConfigBlock("example-agent", "/opt/com link/comlink", "wss://agent.example.com/codex?a=%20", "/home/u/token")
	want := "Host example-agent\n" +
		"  User comlink\n" +
		"  ProxyCommand '/opt/com link/comlink' stdio --upstream 'wss://agent.example.com/codex?a=%%20' --token-file /home/u/token\n" +
		"  HostKeyAlias comlink-example-agent\n" +
		"  StrictHostKeyChecking accept-new\n"
	if got != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}
	if strings.Contains(sshd.ConfigBlock("a", "/x/comlink", "wss://h/", ""), "--token-file") {
		t.Fatal("token-file flag printed without a token file")
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
		block := sshd.ConfigBlock("a", "/x/comlink", tc.in, "")
		if want := "\n  ProxyCommand /x/comlink stdio --upstream " + tc.want + "\n"; !strings.Contains(block, want) {
			t.Errorf("upstream %q: block\n%s\nwant %q", tc.in, block, want)
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
	line := proxyCommand(t, sshd.ConfigBlock("example-agent", exe, upstream, tokenFile))
	line = strings.ReplaceAll(line, "%%", "%") // ssh's token expansion

	out, err := exec.Command("sh", "-c", line).Output()
	if err != nil {
		t.Fatalf("sh -c %q: %v", line, err)
	}
	got := strings.Split(strings.TrimSuffix(string(out), "\x00"), "\x00")
	want := []string{exe, "stdio", "--upstream", upstream, "--token-file", tokenFile}
	if !slices.Equal(got, want) {
		t.Fatalf("shell split %q into %q, want %q", line, got, want)
	}
}
