package codex

import (
	"bytes"
	"os"
	"strings"
	"testing"
)

// fixtureMarker is the marker the captured proxy command carries:
// printf '%b' '\234\077\102\274\134\323\277\254'.
var fixtureMarker = []byte{0o234, 0o077, 0o102, 0o274, 0o134, 0o323, 0o277, 0o254}

// readFixture returns an exec string the Codex app sends, from testdata.
func readFixture(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSuffix(string(b), "\n")
}

func TestParseCommandFixtureIsProxyWithMarker(t *testing.T) {
	cmd := parseCommand(readFixture(t, "codex-app-proxy.txt"))
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
		name   string
		exec   string
		want   kind
		suffix string // the payload ends with this once the launcher is peeled off
	}{
		{"probe", readFixture(t, "codex-app-probe.txt"), kindProbe, "; exit 86"},
		{"version", readFixture(t, "codex-app-version.txt"), kindVersion, "codex --version"},
		{"bootstrap", readFixture(t, "codex-app-bootstrap.txt"), kindBootstrap, "--listen 'unix://' </dev/null >/dev/null 2>&1 & }"},
		{"pkill", readFixture(t, "codex-app-kill.txt"), kindKill, "-f 'codex.* app-server.* --listen'"},
		{"unknown", "ls -la /", kindUnknown, "ls -la /"},
		{"unknown-wrapped", readFixture(t, "codex-app-unknown.txt"), kindUnknown, "ls -la /"},
		{"unknown-shell", "bash -lc 'echo hi'", kindUnknown, "bash -lc 'echo hi'"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cmd := parseCommand(tc.exec)
			if cmd.Kind != tc.want {
				t.Fatalf("kind = %v, want %v", cmd.Kind, tc.want)
			}
			if !strings.HasSuffix(cmd.Payload, tc.suffix) || strings.Contains(cmd.Payload, "CODEX_REMOTE_PAYLOAD") {
				t.Fatalf("payload = %q, want the unwrapped payload ending in %q", cmd.Payload, tc.suffix)
			}
			if cmd.Marker != nil {
				t.Fatalf("unexpected marker %q", cmd.Marker)
			}
		})
	}
}

func TestParseCommandBootstrapBeatsPkill(t *testing.T) {
	// The bootstrap payload also contains pkill; it must still be bootstrap.
	if k := parseCommand(readFixture(t, "codex-app-bootstrap.txt")).Kind; k != kindBootstrap {
		t.Fatalf("kind = %v, want bootstrap", k)
	}
}

func TestParseCommandUnwrappedPayload(t *testing.T) {
	cmd := parseCommand("codex --version")
	if cmd.Kind != kindVersion || cmd.Payload != "codex --version" {
		t.Fatalf("got %+v", cmd)
	}
}

func TestParseCommandMarkerWithVersion(t *testing.T) {
	cmd := parseCommand(`printf '%b' '\101\0102\\\n\t'; codex --version`)
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
