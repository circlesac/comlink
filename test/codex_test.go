package test

import (
	"strings"
	"testing"

	"github.com/circlesac/comlink/internal/codex"
)

// unusedUpstream is given to routes whose commands never reach the relay.
const unusedUpstream = "ws://127.0.0.1:1/unused"

func TestCodexAnswersVersionProbe(t *testing.T) {
	client, _ := startCodex(t, codex.Route{Upstream: unusedUpstream, ReportVersion: "0.170.0"})

	out, stderr, status := run(t, client, readFixture(t, "codex-app-version.txt"))
	if status != 0 || out != "codex-cli 0.170.0\n" || stderr != "" {
		t.Fatalf("wrapped: status %d stdout %q stderr %q", status, out, stderr)
	}
	// Unwrapped, as `ssh alias codex --version` sends it for manual checks.
	out, stderr, status = run(t, client, "codex --version")
	if status != 0 || out != "codex-cli 0.170.0\n" || stderr != "" {
		t.Fatalf("plain: status %d stdout %q stderr %q", status, out, stderr)
	}
}

func TestCodexAcknowledgesSetupCommands(t *testing.T) {
	client, _ := startCodex(t, codex.Route{Upstream: unusedUpstream})
	// The bootstrap payload also contains a pkill; both answers are the same.
	for name, fixture := range map[string]string{"probe": "codex-app-probe.txt", "bootstrap": "codex-app-bootstrap.txt", "pkill": "codex-app-kill.txt"} {
		t.Run(name, func(t *testing.T) {
			out, stderr, status := run(t, client, readFixture(t, fixture))
			if status != 0 || out != "" || stderr != "" {
				t.Fatalf("status %d stdout %q stderr %q", status, out, stderr)
			}
		})
	}
}

func TestCodexRefusesUnknownCommands(t *testing.T) {
	client, logBuf := startCodex(t, codex.Route{Upstream: unusedUpstream})

	out, stderr, status := run(t, client, "rm -rf /tmp/x")
	if status != 127 || out != "" || stderr != "comlink: unsupported command: rm -rf /tmp/x\n" {
		t.Fatalf("plain: status %d stdout %q stderr %q", status, out, stderr)
	}
	// The same refusal when the app's launcher wraps the unknown payload: the
	// payload is unwrapped and named, the launcher is not echoed.
	out, stderr, status = run(t, client, readFixture(t, "codex-app-unknown.txt"))
	if status != 127 || out != "" || stderr != "comlink: unsupported command: ls -la /\n" {
		t.Fatalf("wrapped: status %d stdout %q stderr %q", status, out, stderr)
	}
	// Another shell is not the launcher and is named as written.
	out, stderr, status = run(t, client, "bash -lc 'echo hi'")
	if status != 127 || out != "" || stderr != "comlink: unsupported command: bash -lc 'echo hi'\n" {
		t.Fatalf("other shell: status %d stdout %q stderr %q", status, out, stderr)
	}
	if strings.Contains(logBuf.String(), "unsupported command") {
		t.Fatalf("server log %q repeats the refusal the client already sees", logBuf.String())
	}
}

// TestCodexWritesTheMarkerFirst covers the printf '%b' prefix: its escapes
// are decoded and the bytes reach stdout before the command's own output.
func TestCodexWritesTheMarkerFirst(t *testing.T) {
	client, _ := startCodex(t, codex.Route{Upstream: unusedUpstream})
	cases := []struct {
		name    string
		escapes string
		marker  string
	}{
		{"fixture", `\234\077\102\274\134\323\277\254`, string(fixtureMarker)},
		{"mixed", `\101\0102\\\n\t`, "AB\\\n\t"},
		{"zero-prefixed octal", `\0101\101`, "AA"},
		{"nul", `\0`, "\x00"},
		{"backslash", `\\`, `\`},
		{"named", `\n\t\r\a\b\f\v\e`, "\n\t\r\a\b\f\v\x1b"},
		{"unknown escape kept", `\q`, `\q`},
		{"plain", `plain`, `plain`},
		{"three octal digits at most", `\1234`, "S4"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			payload := "printf '%b' '" + tc.escapes + "'; codex --version"
			for name, exec := range map[string]string{"plain": payload, "wrapped": wrap(t, payload)} {
				out, stderr, status := run(t, client, exec)
				if status != 0 || out != tc.marker+"codex-cli 0.165.0\n" || stderr != "" {
					t.Fatalf("%s: status %d stdout %q stderr %q, want marker %q", name, status, out, stderr, tc.marker)
				}
			}
		})
	}

	t.Run("newline after the marker", func(t *testing.T) {
		out, _, status := run(t, client, "printf '%b' '\\101';\ncodex --version")
		if status != 0 || out != "Acodex-cli 0.165.0\n" {
			t.Fatalf("status %d stdout %q", status, out)
		}
	})
	t.Run("unterminated marker is not a marker", func(t *testing.T) {
		out, _, status := run(t, client, "printf '%b' '\\101; codex --version")
		if status != 0 || out != "codex-cli 0.165.0\n" {
			t.Fatalf("status %d stdout %q", status, out)
		}
	})
	t.Run("not written for an unknown command", func(t *testing.T) {
		out, stderr, status := run(t, client, wrap(t, "printf '%b' '\\101'; ls"))
		if status != 127 || out != "" || stderr != "comlink: unsupported command: printf '%b' '\\101'; ls\n" {
			t.Fatalf("status %d stdout %q stderr %q", status, out, stderr)
		}
	})
}

// TestCodexUnwrapsTheLauncher covers how the exec string is split into
// words: the payload is the last word of `sh -c '<launcher>' sh '<payload>'`
// and anything else is taken as written. The refusal names the payload, so
// the split is visible on stderr; a payload recognised as `codex --version`
// is visible on stdout.
func TestCodexUnwrapsTheLauncher(t *testing.T) {
	client, _ := startCodex(t, codex.Route{Upstream: unusedUpstream})
	const version = "codex-cli 0.165.0\n"
	cases := []struct {
		name       string
		exec       string
		wantStdout string // when the payload is the version command
		wantRefuse string // the payload named in the refusal otherwise
	}{
		{"single-quote escapes and a newline", `sh -c 'x '\''y'\''' sh 'line1` + "\n" + `line2'`, "", "line1\nline2"},
		{"double quotes with escapes", `sh -c "x \"y\"" sh "echo \"hi\" \$x \q"`, "", `echo "hi" $x \q`},
		{"double-quoted version", `sh -c "x" sh "codex --version"`, version, ""},
		{"backslash escapes a space", `sh -c x sh back\ slash`, "", "back slash"},
		{"backslash newline continues the word", "sh -c x sh pay\\\nload", "", "payload"},
		{"trailing backslash is dropped", `sh -c x sh ls\`, "", "ls"},
		{"tabs separate words", "sh\t-c\tx\tsh\t'codex --version'", version, ""},
		{"unterminated quote runs to the end", `sh -c x sh 'ls -la`, "", "ls -la"},
		{"last word is the payload", `sh -c x sh 'ls' 'codex --version'`, version, ""},
		{"fewer than five words are taken as written", `sh -c x sh`, "", "sh -c x sh"},
		{"another launcher name is taken as written", `sh -c x bash 'ls'`, "", "sh -c x bash 'ls'"},
		{"plain command is taken as written", `ls -la /`, "", "ls -la /"},
		{"empty command", ``, "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, stderr, status := run(t, client, tc.exec)
			if tc.wantStdout != "" {
				if status != 0 || out != tc.wantStdout || stderr != "" {
					t.Fatalf("status %d stdout %q stderr %q, want %q", status, out, stderr, tc.wantStdout)
				}
				return
			}
			if status != 127 || out != "" || stderr != "comlink: unsupported command: "+tc.wantRefuse+"\n" {
				t.Fatalf("status %d stdout %q stderr %q, want refusal of %q", status, out, stderr, tc.wantRefuse)
			}
		})
	}
}

// TestCodexClassifiesByPrecedence covers which answer wins when a payload
// matches several commands: proxy, then the setup commands, then version.
func TestCodexClassifiesByPrecedence(t *testing.T) {
	client, logBuf := startCodex(t, codex.Route{Upstream: unusedUpstream})

	// The proxy wins over the version answer: with no WebSocket handshake on
	// stdin the relay gives up with exit 1 and nothing is printed.
	out, stderr, status := run(t, client, wrap(t, "codex --version; exec codex app-server proxy"))
	if status != 1 || out != "" || stderr != "" || !strings.Contains(logBuf.String(), "client did not send an HTTP upgrade") {
		t.Fatalf("proxy: status %d stdout %q stderr %q log %q", status, out, stderr, logBuf.String())
	}
	// The setup commands win over the version answer: nothing is printed.
	for name, payload := range map[string]string{
		"bootstrap": "CODEX_SSH_SKIP_APP_SERVER_BOOT=true; pkill x; codex --version",
		"listen":    "codex app-server --listen 'unix://'; codex --version",
		"pkill":     "pkill -f codex; codex --version",
	} {
		out, stderr, status := run(t, client, wrap(t, payload))
		if status != 0 || out != "" || stderr != "" {
			t.Fatalf("%s: status %d stdout %q stderr %q", name, status, out, stderr)
		}
	}
	// The version answer wins over the probe.
	out, stderr, status = run(t, client, wrap(t, "command -v codex; codex --version"))
	if status != 0 || out != "codex-cli 0.165.0\n" || stderr != "" {
		t.Fatalf("version over probe: status %d stdout %q stderr %q", status, out, stderr)
	}
}
