package sshconfig

import (
	"strings"
	"testing"
)

func TestSSHConfigBlock(t *testing.T) {
	got := Block("example-agent", "/opt/com link/comlink", "wss://agent.example.com/codex?a=%20", "/home/u/token")
	want := "Host example-agent\n" +
		"  User comlink\n" +
		"  ProxyCommand '/opt/com link/comlink' stdio --upstream 'wss://agent.example.com/codex?a=%%20' --token-file /home/u/token\n" +
		"  StrictHostKeyChecking accept-new\n"
	if got != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}
	if strings.Contains(Block("a", "/x/comlink", "wss://h/", ""), "--token-file") {
		t.Fatal("token-file flag printed without a token file")
	}
}
