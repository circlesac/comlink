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
