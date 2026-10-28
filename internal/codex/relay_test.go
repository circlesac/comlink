package codex

import (
	"strings"
	"testing"
)

func TestRedactURL(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"wss://agent.example.com/codex", "wss://agent.example.com/codex"},
		{"wss://agent.example.com/codex?token=secret-value", "wss://agent.example.com/codex"},
		{"wss://user:secret-value@agent.example.com/codex?a=1#f", "wss://agent.example.com/codex"},
		{"ws://127.0.0.1:1/rpc?", "ws://127.0.0.1:1/rpc"},
		{"wss://agent.example.com/%zz?token=secret-value", "<invalid upstream URL>"},
	}
	for _, tc := range cases {
		if got := redactURL(tc.in); got != tc.want {
			t.Errorf("redactURL(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestDialUpstreamErrorOmitsQuery(t *testing.T) {
	r := &Route{Upstream: "ws://127.0.0.1:1/rpc?token=secret-value"}
	_, err := r.dialUpstream()
	if err == nil {
		t.Fatal("dial to a closed port succeeded")
	}
	if msg := err.Error(); strings.Contains(msg, "secret-value") || !strings.Contains(msg, "ws://127.0.0.1:1/rpc") {
		t.Fatalf("error %q, want the redacted upstream and no query", msg)
	}
}
