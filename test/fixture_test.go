// Package test holds comlink's integration tests: an SSH client driving the
// server with the Codex route end to end, and the same flows through real
// OpenSSH with the comlink binary as its ProxyCommand.
package test

import (
	"os"
	"strings"
	"testing"

	"github.com/circlesac/comlink/internal/shell"
)

// The payloads the Codex app sends, as in internal/route/codex/command_test.go.
const (
	probePayload     = `if command -v codex >/dev/null 2>&1; then exit 0; fi; exit 86`
	versionPayload   = `codex --version`
	bootstrapPayload = `if [ "${CODEX_SSH_SKIP_APP_SERVER_BOOT:-}" = "true" ]; then exit 0; fi; (umask 077; mkdir -p -- "${CODEX_HOME:-$HOME/.codex}/app-server-control" && (pkill -9 -U "$(id -u)" -f 'codex.*[d]esktop-ssh-websocket-v0.sock' || true)) && { nohup codex -c features.code_mode_host=true app-server --listen 'unix://' </dev/null >/dev/null 2>&1 & }`
	killPayload      = `pkill -9 -U "$(id -u)" -f 'codex.* app-server.* --listen'`
)

// fixtureMarker is the marker the captured proxy command carries:
// printf '%b' '\234\077\102\274\134\323\277\254'.
var fixtureMarker = []byte{0o234, 0o077, 0o102, 0o274, 0o134, 0o323, 0o277, 0o254}

func readFixture(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile("../internal/route/codex/testdata/codex-app-proxy.txt")
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSuffix(string(b), "\n")
}

// wrapLikeApp puts payload into the same `sh -c '<launcher>' sh '<payload>'`
// wrapper the Codex app uses, taking the launcher from the fixture.
func wrapLikeApp(t *testing.T, payload string) string {
	t.Helper()
	words := shell.Split(readFixture(t))
	if len(words) != 5 || words[0] != "sh" || words[1] != "-c" || words[3] != "sh" {
		t.Fatalf("fixture does not have the sh -c <launcher> sh <payload> shape: %d words", len(words))
	}
	return "sh -c " + shell.Quote(words[2]) + " sh " + shell.Quote(payload)
}
