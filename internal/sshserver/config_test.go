package sshserver

import (
	"strings"
	"testing"
)

func TestSSHConfigBlock(t *testing.T) {
	got := ConfigBlock("example-agent", "/opt/com link/comlink", "wss://agent.example.com/codex?a=%20", "/home/u/token")
	want := "Host example-agent\n" +
		"  User comlink\n" +
		"  ProxyCommand '/opt/com link/comlink' stdio --upstream 'wss://agent.example.com/codex?a=%%20' --token-file /home/u/token\n" +
		"  StrictHostKeyChecking accept-new\n"
	if got != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}
	if strings.Contains(ConfigBlock("a", "/x/comlink", "wss://h/", ""), "--token-file") {
		t.Fatal("token-file flag printed without a token file")
	}
}

func TestShellQuote(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"plain", "plain"},
		{"/usr/local/bin/comlink", "/usr/local/bin/comlink"},
		{"/opt/com link/comlink", "'/opt/com link/comlink'"},
		{"it's", `'it'\''s'`},
		{"/home/u/50%", "/home/u/50%"}, // % is plain to the shell; ConfigBlock doubles it for ssh
		{"", "''"},
		{"a\nb", "'a\nb'"},
		{`back\slash`, `'back\slash'`},
		{"$HOME", "'$HOME'"},
	}
	for _, tc := range cases {
		if got := shellQuote(tc.in); got != tc.want {
			t.Errorf("shellQuote(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
