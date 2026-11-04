package test

import (
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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
	wd, _ := os.Getwd()
	if cred, err := upstream.LoadCredential("", "exit 1", logger); err != nil || cred != (upstream.Credential{Command: "exit 1", Dir: wd, Path: os.Getenv("PATH")}) {
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

// TestCredentialBearerTimeoutKillsTheGroup: a token command that outlives
// TokenCommandTimeout is an error about the timeout, and Bearer returns
// shortly after it even when a grandchild still holds the command's stdout,
// because the whole process group is killed.
func TestCredentialBearerTimeoutKillsTheGroup(t *testing.T) {
	t.Parallel()
	start := time.Now()
	token, err := (upstream.Credential{Command: "sleep 600 & sleep 600; echo late"}).Bearer()
	if err == nil || err.Error() != "token command timed out after 30s" || token != "" {
		t.Fatalf("Bearer of a slow command = %q, %v", token, err)
	}
	if took := time.Since(start); took > upstream.TokenCommandTimeout+10*time.Second {
		t.Fatalf("Bearer took %s, want about %s", took, upstream.TokenCommandTimeout)
	}
}

// TestCredentialBearerPinsDirAndPath: the command runs in Dir with PATH set
// to Path, whatever the caller's working directory and PATH are, so a
// session's ProxyCommand runs it as the terminal that started the session.
func TestCredentialBearerPinsDirAndPath(t *testing.T) {
	dir, bin := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "example-token-helper"), []byte("#!/bin/sh\nprintf %s "+testToken+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", "/usr/bin:/bin")
	if got, err := (upstream.Credential{Command: "pwd", Dir: dir}).Bearer(); err != nil || got != mustEvalSymlinks(t, dir) && got != dir {
		t.Fatalf("Bearer(pwd) in %s = %q, %v", dir, got, err)
	}
	if _, err := (upstream.Credential{Command: "example-token-helper"}).Bearer(); err == nil {
		t.Fatal("helper found without its PATH")
	}
	if got, err := (upstream.Credential{Command: "example-token-helper", Path: bin + ":/usr/bin:/bin"}).Bearer(); err != nil || got != testToken {
		t.Fatalf("Bearer with the pinned PATH = %q, %v", got, err)
	}
}
