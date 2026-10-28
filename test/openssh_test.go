package test

import (
	"bytes"
	"context"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/circlesac/comlink/internal/sshd"
)

// buildComlink compiles the binary once per test run for the OpenSSH tests.
func buildComlink(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "comlink")
	build := exec.Command("go", "build", "-o", bin, "github.com/circlesac/comlink")
	build.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	return bin
}

// opensshCommand runs the real ssh client through comlink as its
// ProxyCommand, the way the Codex app does, with a temporary XDG_STATE_HOME
// so the host key is generated fresh.
func opensshCommand(t *testing.T, bin, upstream, tokenFile, command string) *exec.Cmd {
	t.Helper()
	cmd := exec.Command("ssh",
		"-F", "/dev/null",
		"-o", "ProxyCommand="+quote(bin)+" stdio --upstream "+quote(upstream)+" --token-file "+quote(tokenFile),
		"-o", "BatchMode=yes",
		"-o", "StrictHostKeyChecking=no",
		"-o", "UserKnownHostsFile=/dev/null",
		"-o", "ExitOnForwardFailure=yes",
		"-R", "127.0.0.1:0:127.0.0.1:9",
		"comlink@example-agent",
		"--", command,
	)
	cmd.Env = append(os.Environ(), "XDG_STATE_HOME="+t.TempDir())
	return cmd
}

// quote single-quotes s for the shell ssh runs the ProxyCommand with.
func quote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

func TestOpenSSHVersionProbe(t *testing.T) {
	if _, err := exec.LookPath("ssh"); err != nil {
		t.Skip("ssh not on PATH")
	}
	bin := buildComlink(t)
	upstream := newEchoUpstream(t)
	tokenFile := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenFile, []byte(testToken+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	stateHome := t.TempDir()

	cmd := opensshCommand(t, bin, upstream.wsURL(), tokenFile, readFixture(t, "codex-app-version.txt"))
	cmd.Env = append(os.Environ(), "XDG_STATE_HOME="+stateHome)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("ssh: %v\nstderr: %s", err, stderr.String())
	}
	if string(out) != "codex-cli 0.165.0\n" {
		t.Fatalf("stdout = %q, stderr %s", out, stderr.String())
	}
	if strings.Contains(stderr.String(), testToken) {
		t.Fatal("ssh stderr contains the token")
	}
	info, err := os.Stat(filepath.Join(stateHome, "comlink", "host_ed25519"))
	if err != nil {
		t.Fatalf("host key was not persisted: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("host key mode = %o, want 600", perm)
	}

	cmd = opensshCommand(t, bin, upstream.wsURL(), tokenFile, "ls /")
	cmd.Env = append(os.Environ(), "XDG_STATE_HOME="+stateHome)
	stderr.Reset()
	cmd.Stderr = &stderr
	if err := cmd.Run(); err == nil {
		t.Fatal("unknown command exited 0")
	} else if exitErr, ok := err.(*exec.ExitError); !ok || exitErr.ExitCode() != 127 {
		t.Fatalf("ssh: %v, want exit 127; stderr %s", err, stderr.String())
	}
	if !strings.Contains(stderr.String(), "comlink: unsupported command: ls /") {
		t.Fatalf("stderr %q lacks the refusal", stderr.String())
	}
}

func TestOpenSSHProxyRelay(t *testing.T) {
	if _, err := exec.LookPath("ssh"); err != nil {
		t.Skip("ssh not on PATH")
	}
	bin := buildComlink(t)
	upstream := newEchoUpstream(t)
	tokenFile := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenFile, []byte(testToken), 0o600); err != nil {
		t.Fatal(err)
	}

	cmd := opensshCommand(t, bin, upstream.wsURL(), tokenFile, readFixture(t, "codex-app-proxy.txt"))
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	marker := make([]byte, len(fixtureMarker))
	if _, err := io.ReadFull(stdout, marker); err != nil {
		t.Fatalf("read marker: %v\nstderr: %s", err, stderr.String())
	}
	if !bytes.Equal(marker, fixtureMarker) {
		t.Fatalf("marker = %q, want %q", marker, fixtureMarker)
	}

	var conn net.Conn = &sshd.StdioConn{In: stdout, Out: stdin}
	ws := dialOverConn(t, conn)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	msg := `{"jsonrpc":"2.0","id":7,"method":"thread/list"}`
	if err := ws.Write(ctx, websocket.MessageText, []byte(msg)); err != nil {
		t.Fatal(err)
	}
	typ, data, err := ws.Read(ctx)
	if err != nil || typ != websocket.MessageText || string(data) != msg {
		t.Fatalf("round trip: typ=%v data=%q err=%v\nstderr: %s", typ, data, err, stderr.String())
	}
	if err := ws.Close(websocket.StatusNormalClosure, "done"); err != nil {
		t.Fatalf("close: %v", err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatalf("ssh exited with %v, want 0\nstderr: %s", err, stderr.String())
	}
	if auths := upstream.seenAuths(); len(auths) != 1 || auths[0] != "Bearer "+testToken {
		t.Fatalf("upstream saw Authorization %q", auths)
	}
	if strings.Contains(stderr.String(), testToken) {
		t.Fatal("ssh stderr contains the token")
	}
}
