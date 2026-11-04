package test

import (
	"log"
	"path/filepath"
	"strings"
	"testing"

	"github.com/circlesac/comlink/internal/upstream"
)

// TestUpstreamLoadToken: the token is the trimmed file contents, else
// $COMLINK_TOKEN trimmed, else empty; a file others can read warns once
// naming the path, never the token; a missing file is an error.
func TestUpstreamLoadToken(t *testing.T) {
	var logBuf lockedBuffer
	logger := log.New(&logBuf, "comlink: ", 0)
	t.Setenv("COMLINK_TOKEN", "  "+testToken+"\n")
	if token, err := upstream.LoadToken("", logger); err != nil || token != testToken {
		t.Fatalf("LoadToken from the environment = %q, %v", token, err)
	}
	t.Setenv("COMLINK_TOKEN", "")
	if token, err := upstream.LoadToken("", logger); err != nil || token != "" {
		t.Fatalf("LoadToken without a token = %q, %v", token, err)
	}
	if logBuf.Len() != 0 {
		t.Fatalf("log %q without a file", logBuf.String())
	}

	private := writeTokenFile(t, 0o600)
	if token, err := upstream.LoadToken(private, logger); err != nil || token != testToken || logBuf.Len() != 0 {
		t.Fatalf("LoadToken(0600) = %q, %v, log %q", token, err, logBuf.String())
	}
	shared := writeTokenFile(t, 0o640)
	if token, err := upstream.LoadToken(shared, logger); err != nil || token != testToken {
		t.Fatalf("LoadToken(0640) = %q, %v", token, err)
	}
	if want := "comlink: warning: token file " + shared + " is readable by group or others; restrict it with chmod 600\n"; logBuf.String() != want {
		t.Fatalf("log %q, want %q", logBuf.String(), want)
	}
	if strings.Contains(logBuf.String(), testToken) {
		t.Fatal("log contains the token")
	}
	if token, err := upstream.LoadToken(filepath.Join(t.TempDir(), "absent"), logger); err == nil || token != "" || !strings.HasPrefix(err.Error(), "read token file: ") {
		t.Fatalf("LoadToken(missing) = %q, %v", token, err)
	}
}

// TestCredentialBearer: a token is returned as given; a token command's
// stdout is trimmed and returned, with the caller's environment and no
// stdin; a command that fails, prints nothing, prints two lines or prints
// too much is an error that never carries its output.
func TestCredentialBearer(t *testing.T) {
	if token, err := (upstream.Credential{Token: testToken}).Bearer(); err != nil || token != testToken {
		t.Fatalf("Bearer of a token = %q, %v", token, err)
	}
	if token, err := (upstream.Credential{}).Bearer(); err != nil || token != "" {
		t.Fatalf("Bearer of nothing = %q, %v", token, err)
	}
	t.Setenv("COMLINK_TEST_SECRET", testToken)
	if token, err := (upstream.Credential{Command: `printf '  %s\n' "$COMLINK_TEST_SECRET"`}).Bearer(); err != nil || token != testToken {
		t.Fatalf("Bearer of a command = %q, %v", token, err)
	}
	for _, tc := range []struct{ command, want string }{
		{"printf %s " + testToken + "; exit 3", "token command failed: exit status 3"},
		{"true", "token command printed no token"},
		{"cat", "token command printed no token"}, // stdin is empty, not the caller's
		{`printf 'a\nb'`, "token command printed more than one line"},
		{"head -c 70000 /dev/zero | tr '\\0' a", "token command printed more than 65536 bytes"},
	} {
		token, err := (upstream.Credential{Command: tc.command}).Bearer()
		if err == nil || err.Error() != tc.want || token != "" {
			t.Errorf("Bearer(%q) = %q, %v, want error %q", tc.command, token, err, tc.want)
		}
		if err != nil && strings.Contains(err.Error(), testToken) {
			t.Errorf("Bearer(%q) error carries the output", tc.command)
		}
	}
}

// TestLoadCredential: --token-command is taken as is (not run), and is
// exclusive with --token-file; otherwise the token comes from LoadToken.
func TestLoadCredential(t *testing.T) {
	logger := log.New(&lockedBuffer{}, "", 0)
	t.Setenv("COMLINK_TOKEN", testToken)
	if cred, err := upstream.LoadCredential("", "exit 1", logger); err != nil || cred != (upstream.Credential{Command: "exit 1"}) {
		t.Fatalf("LoadCredential(command) = %+v, %v", cred, err)
	}
	if cred, err := upstream.LoadCredential("", "", logger); err != nil || cred != (upstream.Credential{Token: testToken}) {
		t.Fatalf("LoadCredential(env) = %+v, %v", cred, err)
	}
	file := writeTokenFile(t, 0o600)
	if _, err := upstream.LoadCredential(file, "x", logger); err == nil || err.Error() != "--token-file and --token-command are exclusive" {
		t.Fatalf("LoadCredential(file, command) error %v", err)
	}
}
