package test

import (
	"os"
	"strings"
	"testing"
)

// fixtureMarker is the marker the captured proxy command carries:
// printf '%b' '\234\077\102\274\134\323\277\254'.
var fixtureMarker = []byte{0o234, 0o077, 0o102, 0o274, 0o134, 0o323, 0o277, 0o254}

// fixtures are the exec strings captured from the Codex app, under testdata.
var fixtures = []string{
	"codex-app-probe.txt",
	"codex-app-version.txt",
	"codex-app-bootstrap.txt",
	"codex-app-kill.txt",
	"codex-app-proxy.txt",
	"codex-app-unknown.txt",
}

// readFixture returns an exec string the Codex app sends, from testdata.
func readFixture(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSuffix(string(b), "\n")
}

// launcher returns the app's login-shell launcher: everything in the
// captured exec string before the payload, up to and including the `sh '`
// that opens it.
func launcher(t *testing.T) string {
	t.Helper()
	exec := readFixture(t, "codex-app-unknown.txt")
	const payload = "'ls -la /'"
	if !strings.HasSuffix(exec, payload) {
		t.Fatalf("unknown fixture does not end with %s: %q", payload, exec)
	}
	return strings.TrimSuffix(exec, payload)
}

// wrap puts payload where the Codex app puts it: as the last, single-quoted
// argument after the captured launcher.
func wrap(t *testing.T, payload string) string {
	t.Helper()
	return launcher(t) + "'" + strings.ReplaceAll(payload, "'", `'\''`) + "'"
}

func TestFixturesShareTheLauncher(t *testing.T) {
	prefix := launcher(t)
	if !strings.HasPrefix(prefix, "sh -c '") || !strings.HasSuffix(prefix, "' sh ") {
		t.Fatalf("launcher %q is not sh -c '...' sh", prefix)
	}
	for _, name := range fixtures {
		exec := readFixture(t, name)
		if !strings.HasPrefix(exec, prefix) || !strings.HasSuffix(exec, "'") {
			t.Errorf("%s is not the launcher around a single-quoted payload: %q", name, exec)
		}
	}
	if proxy := readFixture(t, "codex-app-proxy.txt"); !strings.HasSuffix(proxy, "exec codex app-server proxy'") {
		t.Errorf("proxy fixture does not end with the proxy exec: %q", proxy)
	}
}
