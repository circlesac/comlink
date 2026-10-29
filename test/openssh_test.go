package test

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
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
		"-o", "ProxyCommand="+quote(bin)+" codex proxy --upstream "+quote(upstream)+" --token-file "+quote(tokenFile),
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

// TestBinaryStartupChecks runs the built binary directly. In proxy mode
// stdin is empty, so it gets as far as the SSH handshake and stops there;
// what it prints on stderr before that is the startup behaviour: a warning
// for a token file others can read, and a clear failure when there is no
// place for the host key. codex ssh-config refuses a bad alias before printing.
func TestBinaryStartupChecks(t *testing.T) {
	bin := buildComlink(t)
	proxy := func(t *testing.T, dir string, env []string, tokenFile string) (stderr string, err error) {
		t.Helper()
		cmd := exec.Command(bin, "codex", "proxy", "--upstream", "ws://127.0.0.1:1/rpc", "--token-file", tokenFile)
		cmd.Env = append(os.Environ(), env...)
		cmd.Dir = dir
		var errBuf bytes.Buffer
		cmd.Stderr = &errBuf
		err = cmd.Run() // stdin is empty: the handshake fails after startup
		if strings.Contains(errBuf.String(), testToken) {
			t.Fatal("stderr contains the token")
		}
		return errBuf.String(), err
	}
	warning := func(path string) string {
		return "comlink: warning: token file " + path + " is readable by group or others; restrict it with chmod 600\n"
	}

	for _, mode := range []os.FileMode{0o644, 0o640, 0o604} {
		t.Run(fmt.Sprintf("token file %04o", mode), func(t *testing.T) {
			path := writeTokenFile(t, mode)
			stderr, _ := proxy(t, t.TempDir(), []string{"XDG_STATE_HOME=" + t.TempDir()}, path)
			if strings.Count(stderr, warning(path)) != 1 {
				t.Fatalf("stderr %q, want one line %q", stderr, warning(path))
			}
			if !strings.Contains(stderr, "ssh handshake") {
				t.Fatalf("stderr %q: comlink did not continue past the warning", stderr)
			}
		})
	}
	t.Run("token file 0600", func(t *testing.T) {
		path := writeTokenFile(t, 0o600)
		stderr, _ := proxy(t, t.TempDir(), []string{"XDG_STATE_HOME=" + t.TempDir()}, path)
		if strings.Contains(stderr, "warning") {
			t.Fatalf("stderr %q warns about a private token file", stderr)
		}
		if !strings.Contains(stderr, "ssh handshake") {
			t.Fatalf("stderr %q: comlink did not reach the handshake", stderr)
		}
	})
	t.Run("missing token file", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "absent")
		stderr, err := proxy(t, t.TempDir(), []string{"XDG_STATE_HOME=" + t.TempDir()}, path)
		if exitErr, ok := err.(*exec.ExitError); !ok || exitErr.ExitCode() == 0 {
			t.Fatalf("comlink codex proxy: %v, want a non-zero exit", err)
		}
		if !strings.Contains(stderr, "comlink: error: read token file: ") {
			t.Fatalf("stderr %q, want the token file error", stderr)
		}
	})
	t.Run("no home directory", func(t *testing.T) {
		if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
			t.Skip("os.UserHomeDir reads $HOME only on darwin and linux")
		}
		dir := t.TempDir()
		stderr, err := proxy(t, dir, []string{"HOME=", "XDG_STATE_HOME="}, writeTokenFile(t, 0o600))
		if exitErr, ok := err.(*exec.ExitError); !ok || exitErr.ExitCode() == 0 {
			t.Fatalf("comlink codex proxy: %v, want a non-zero exit", err)
		}
		if want := "comlink: error: locate host key: XDG_STATE_HOME is unset and the home directory is unknown: "; !strings.Contains(stderr, want) {
			t.Fatalf("stderr %q, want %q", stderr, want)
		}
		if entries, err := os.ReadDir(dir); err != nil || len(entries) != 0 {
			t.Fatalf("working directory holds %v (%v), want nothing created there", entries, err)
		}
	})
	t.Run("codex ssh-config alias", func(t *testing.T) {
		sshConfig := func(alias string) (stdout, stderr string, err error) {
			cmd := exec.Command(bin, "codex", "ssh-config", "--alias", alias, "--upstream", "wss://agent.example.com/codex")
			var outBuf, errBuf bytes.Buffer
			cmd.Stdout, cmd.Stderr = &outBuf, &errBuf
			err = cmd.Run()
			return outBuf.String(), errBuf.String(), err
		}
		if stdout, stderr, err := sshConfig("example-agent"); err != nil || !strings.HasPrefix(stdout, "Host example-agent\n") {
			t.Fatalf("valid alias: %v, stdout %q, stderr %q", err, stdout, stderr)
		}
		stdout, stderr, err := sshConfig("agent\nHost *")
		if exitErr, ok := err.(*exec.ExitError); !ok || exitErr.ExitCode() == 0 {
			t.Fatalf("invalid alias: %v, want a non-zero exit", err)
		}
		if stdout != "" {
			t.Fatalf("invalid alias printed %q", stdout)
		}
		if want := `comlink: error: invalid alias "agent\nHost *": use only letters, digits, '-', '_' and '.'`; !strings.Contains(stderr, want) {
			t.Fatalf("stderr %q, want %q", stderr, want)
		}
	})
}

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

// writeTokenFile writes the test token to a file with the given mode.
func writeTokenFile(t *testing.T, mode os.FileMode) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(path, []byte(testToken+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
	return path
}
