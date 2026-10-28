// Package test holds comlink's integration tests: an SSH client driving the
// server with the Codex route end to end, and the same flows through real
// OpenSSH with the comlink binary as its ProxyCommand.
package test

import (
	"os"
	"strings"
	"testing"
)

// fixtureMarker is the marker the captured proxy command carries:
// printf '%b' '\234\077\102\274\134\323\277\254'.
var fixtureMarker = []byte{0o234, 0o077, 0o102, 0o274, 0o134, 0o323, 0o277, 0o254}

// readFixture returns an exec string the Codex app sends, from the Codex
// route's testdata.
func readFixture(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile("../internal/codex/testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSuffix(string(b), "\n")
}
