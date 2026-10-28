package main

import (
	"bytes"
	"os"
	"strings"
	"testing"
)

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
	b, err := os.ReadFile("testdata/codex-app-proxy.txt")
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSuffix(string(b), "\n")
}

// wrapLikeApp puts payload into the same `sh -c '<launcher>' sh '<payload>'`
// wrapper the Codex app uses, taking the launcher from the fixture.
func wrapLikeApp(t *testing.T, payload string) string {
	t.Helper()
	words := shellSplit(readFixture(t))
	if len(words) != 5 || words[0] != "sh" || words[1] != "-c" || words[3] != "sh" {
		t.Fatalf("fixture does not have the sh -c <launcher> sh <payload> shape: %d words", len(words))
	}
	return "sh -c " + shellQuote(words[2]) + " sh " + shellQuote(payload)
}

func TestParseCommandFixtureIsProxyWithMarker(t *testing.T) {
	cmd := parseCommand(readFixture(t))
	if cmd.Kind != kindProxy {
		t.Fatalf("kind = %v, want proxy", cmd.Kind)
	}
	if !bytes.Equal(cmd.Marker, fixtureMarker) {
		t.Fatalf("marker = %q, want %q", cmd.Marker, fixtureMarker)
	}
	if !strings.HasSuffix(cmd.Payload, "exec codex app-server proxy") {
		t.Fatalf("payload does not end with the proxy exec: %q", cmd.Payload)
	}
	if strings.Contains(cmd.Payload, "CODEX_REMOTE_PAYLOAD") {
		t.Fatalf("payload still contains the launcher: %q", cmd.Payload)
	}
}

func TestParseCommandClassifiesAppPayloads(t *testing.T) {
	cases := []struct {
		name    string
		payload string
		want    kind
	}{
		{"probe", probePayload, kindProbe},
		{"version", versionPayload, kindVersion},
		{"bootstrap", bootstrapPayload, kindBootstrap},
		{"pkill", killPayload, kindKill},
		{"unknown", "ls -la /", kindUnknown},
		{"unknown-shell", "bash -lc 'echo hi'", kindUnknown},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cmd := parseCommand(wrapLikeApp(t, tc.payload))
			if cmd.Kind != tc.want {
				t.Fatalf("kind = %v, want %v", cmd.Kind, tc.want)
			}
			if cmd.Payload != tc.payload {
				t.Fatalf("payload = %q, want %q", cmd.Payload, tc.payload)
			}
			if cmd.Marker != nil {
				t.Fatalf("unexpected marker %q", cmd.Marker)
			}
		})
	}
}

func TestParseCommandBootstrapBeatsPkill(t *testing.T) {
	// The bootstrap payload also contains pkill; it must still be bootstrap.
	if k := parseCommand(bootstrapPayload).Kind; k != kindBootstrap {
		t.Fatalf("kind = %v, want bootstrap", k)
	}
}

func TestParseCommandUnwrappedPayload(t *testing.T) {
	cmd := parseCommand("codex --version")
	if cmd.Kind != kindVersion || cmd.Payload != "codex --version" {
		t.Fatalf("got %+v", cmd)
	}
}

func TestParseCommandMarkerWithWrappedVersion(t *testing.T) {
	cmd := parseCommand(wrapLikeApp(t, `printf '%b' '\101\0102\\\n\t'; codex --version`))
	if cmd.Kind != kindVersion {
		t.Fatalf("kind = %v, want version", cmd.Kind)
	}
	if want := []byte("AB\\\n\t"); !bytes.Equal(cmd.Marker, want) {
		t.Fatalf("marker = %q, want %q", cmd.Marker, want)
	}
}

func TestDecodePrintfB(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{`\234\077\102\274\134\323\277\254`, string(fixtureMarker)},
		{`\0101\101`, "AA"},
		{`\0`, "\x00"},
		{`\\`, `\`},
		{`\n\t\r\a\b\f\v\e`, "\n\t\r\a\b\f\v\x1b"},
		{`\q`, `\q`},
		{`plain`, `plain`},
		{`\1234`, "S4"}, // at most three octal digits
	}
	for _, tc := range cases {
		if got := string(decodePrintfB(tc.in)); got != tc.want {
			t.Errorf("decodePrintfB(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestShellSplit(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{`a b  c`, []string{"a", "b", "c"}},
		{`'single quoted' "double \"quoted\"" back\ slash`, []string{"single quoted", `double "quoted"`, "back slash"}},
		{`sh -c 'x '\''y'\''' sh 'line1` + "\n" + `line2'`, []string{"sh", "-c", "x 'y'", "sh", "line1\nline2"}},
		{`"a\$b\q"`, []string{`a$b\q`}},
		{``, nil},
	}
	for _, tc := range cases {
		got := shellSplit(tc.in)
		if len(got) != len(tc.want) {
			t.Errorf("shellSplit(%q) = %q, want %q", tc.in, got, tc.want)
			continue
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Errorf("shellSplit(%q)[%d] = %q, want %q", tc.in, i, got[i], tc.want[i])
			}
		}
	}
}

func TestShellQuoteRoundTrip(t *testing.T) {
	for _, s := range []string{"plain", "/usr/local/bin/comlink", "has space", "it's", "", "a\nb", `back\slash`, "$HOME"} {
		got := shellSplit(shellQuote(s))
		if s == "" {
			if len(got) != 1 || got[0] != "" {
				t.Errorf("shellQuote(%q) round-trips to %q", s, got)
			}
			continue
		}
		if len(got) != 1 || got[0] != s {
			t.Errorf("shellQuote(%q) = %q round-trips to %q", s, shellQuote(s), got)
		}
	}
}

func TestSSHConfigBlock(t *testing.T) {
	got := sshConfigBlock("example-agent", "/opt/com link/comlink", "wss://agent.example.com/codex?a=%20", "/home/u/token")
	want := "Host example-agent\n" +
		"  User comlink\n" +
		"  ProxyCommand '/opt/com link/comlink' stdio --upstream 'wss://agent.example.com/codex?a=%%20' --token-file /home/u/token\n" +
		"  StrictHostKeyChecking accept-new\n"
	if got != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}
	if strings.Contains(sshConfigBlock("a", "/x/comlink", "wss://h/", ""), "--token-file") {
		t.Fatal("token-file flag printed without a token file")
	}
}
