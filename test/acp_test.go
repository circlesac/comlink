package test

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/circlesac/comlink/internal/acp"
	"github.com/circlesac/comlink/internal/upstream"
)

// acpSession is one acp.Bridge.Run with pipes for stdin and stdout, the way
// an ACP client runs the agent subprocess.
type acpSession struct {
	stdin  *io.PipeWriter
	stdout *bufio.Reader
	status chan int
	log    *lockedBuffer
}

// startACP runs the bridge against url. stdout is closed once Run returns,
// so rest reads exactly what Run wrote after the lines already read. The
// log must never contain the token.
func startACP(t *testing.T, url, token string) *acpSession {
	t.Helper()
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	s := &acpSession{stdin: inW, stdout: bufio.NewReader(outR), status: make(chan int, 1), log: &lockedBuffer{}}
	bridge := &acp.Bridge{Upstream: url, Token: token, Logger: log.New(s.log, "comlink: ", 0)}
	go func() {
		status := bridge.Run(inR, outW)
		_ = outW.Close()
		s.status <- status
	}()
	t.Cleanup(func() {
		_ = inW.Close() // releases a stdin read Run left behind
		_ = outR.Close()
		if strings.Contains(s.log.String(), testToken) {
			t.Error("bridge log contains the upstream token")
		}
	})
	return s
}

// send writes lines to stdin from another goroutine, so a large write does
// not block the test while it reads stdout.
func (s *acpSession) send(t *testing.T, data string) {
	t.Helper()
	go func() { _, _ = io.WriteString(s.stdin, data) }()
}

// readLine reads one stdout line. The wait is only the test's failure bound.
func (s *acpSession) readLine(t *testing.T) string {
	t.Helper()
	got := make(chan string, 1)
	failed := make(chan error, 1)
	go func() {
		line, err := s.stdout.ReadString('\n')
		if err != nil {
			failed <- fmt.Errorf("%w after %q", err, line)
			return
		}
		got <- line
	}()
	select {
	case line := <-got:
		return line
	case err := <-failed:
		t.Fatalf("read stdout: %v; log %q", err, s.log.String())
	case <-time.After(15 * time.Second):
		t.Fatalf("no stdout line within 15s; log %q", s.log.String())
	}
	return ""
}

// wait returns Run's exit status. The wait is only the test's failure bound.
func (s *acpSession) wait(t *testing.T) int {
	t.Helper()
	select {
	case status := <-s.status:
		return status
	case <-time.After(15 * time.Second):
		t.Fatalf("Run did not return within 15s; log %q", s.log.String())
		return -1
	}
}

// rest returns what Run wrote to stdout after the lines already read. Call
// it after wait.
func (s *acpSession) rest(t *testing.T) string {
	t.Helper()
	b, err := io.ReadAll(s.stdout)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// newACPUpstream is a WebSocket server that requires the bearer token and
// hands each connection to serve.
func newACPUpstream(t *testing.T, serve func(ctx context.Context, c *websocket.Conn)) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+testToken {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		c.SetReadLimit(upstream.ReadLimit)
		serve(r.Context(), c)
	}))
	t.Cleanup(srv.Close)
	return srv
}

type frame struct {
	typ  websocket.MessageType
	data string
}

// TestACPMapsLinesToFramesAndFramesToLines: each stdin line becomes one text
// frame without its newline (a trailing \r dropped, empty lines skipped, a
// line that is not JSON forwarded as is, a last line without a newline sent
// at EOF), each upstream frame becomes one stdout line, and stdin EOF closes
// the upstream with 1000 and exits 0.
func TestACPMapsLinesToFramesAndFramesToLines(t *testing.T) {
	frames := make(chan frame, 16)
	closes := make(chan websocket.StatusCode, 1)
	responses := []string{
		`{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":1}}`,
		`{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"s1"}}`,
	}
	srv := newACPUpstream(t, func(ctx context.Context, c *websocket.Conn) {
		for i := 0; ; i++ {
			typ, data, err := c.Read(ctx)
			if err != nil {
				closes <- websocket.CloseStatus(err)
				return
			}
			frames <- frame{typ, string(data)}
			if i == 2 {
				for _, r := range responses {
					if err := c.Write(ctx, websocket.MessageText, []byte(r)); err != nil {
						return
					}
				}
			}
		}
	})
	s := startACP(t, wsURLOf(srv)+"/acp", testToken)

	s.send(t, `{"jsonrpc":"2.0","id":1,"method":"initialize"}`+"\n\n"+
		`{"jsonrpc":"2.0","id":2,"method":"session/new"}`+"\r\n"+
		"not json\n")
	for _, want := range responses {
		if got := s.readLine(t); got != want+"\n" {
			t.Fatalf("stdout line %q, want %q", got, want+"\n")
		}
	}
	if _, err := io.WriteString(s.stdin, `{"jsonrpc":"2.0","method":"session/cancel"}`); err != nil {
		t.Fatal(err)
	}
	if err := s.stdin.Close(); err != nil {
		t.Fatal(err)
	}
	if got := s.wait(t); got != 0 {
		t.Fatalf("exit status = %d, want 0; log %q", got, s.log.String())
	}
	if rest := s.rest(t); rest != "" {
		t.Fatalf("stdout carries %q after the protocol lines", rest)
	}
	// The upstream read every frame before the close frame.
	if status := <-closes; status != websocket.StatusNormalClosure {
		t.Fatalf("upstream close status = %d, want %d", status, websocket.StatusNormalClosure)
	}
	close(frames)
	var got []frame
	for f := range frames {
		got = append(got, f)
	}
	want := []frame{
		{websocket.MessageText, `{"jsonrpc":"2.0","id":1,"method":"initialize"}`},
		{websocket.MessageText, `{"jsonrpc":"2.0","id":2,"method":"session/new"}`},
		{websocket.MessageText, "not json"},
		{websocket.MessageText, `{"jsonrpc":"2.0","method":"session/cancel"}`},
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("upstream frames %v, want %v", got, want)
	}
	if log := s.log.String(); log != "" {
		t.Fatalf("log %q after a clean session", log)
	}
}

// TestACPRelaysManyMessagesInOrder sends many messages in one write, one of
// them several MiB (far beyond bufio.Scanner's 64 KiB default line), to an
// echoing upstream and reads them back in order.
func TestACPRelaysManyMessagesInOrder(t *testing.T) {
	echo := newEchoUpstream(t)
	s := startACP(t, echo.wsURL()+"/acp", testToken)

	var want []string
	var in strings.Builder
	for i := range 50 {
		msg := fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"session/prompt"}`, i)
		if i == 25 {
			msg = `{"jsonrpc":"2.0","id":25,"method":"fs/write_text_file","params":{"content":"` + strings.Repeat("x", 5<<20) + `"}}`
		}
		want = append(want, msg)
		in.WriteString(msg + "\n")
	}
	s.send(t, in.String())
	for i, msg := range want {
		if got := s.readLine(t); got != msg+"\n" {
			t.Fatalf("stdout line %d has %d bytes, want %d", i, len(got), len(msg)+1)
		}
	}
	if err := s.stdin.Close(); err != nil {
		t.Fatal(err)
	}
	if got := s.wait(t); got != 0 {
		t.Fatalf("exit status = %d, want 0; log %q", got, s.log.String())
	}
	if got := echo.closeStatus(t); got != websocket.StatusNormalClosure {
		t.Fatalf("upstream close status = %d, want %d", got, websocket.StatusNormalClosure)
	}
	if rest := s.rest(t); rest != "" {
		t.Fatalf("stdout carries %q after the protocol lines", rest)
	}
	if auths := echo.seenAuths(); len(auths) != 1 || auths[0] != "Bearer "+testToken {
		t.Fatalf("upstream saw Authorization %q", auths)
	}
}

// TestACPUpstreamCloseEndsRun: when the upstream ends the session, Run
// returns without stdin reaching EOF (it is never closed here), with 0 for a
// normal or going-away close and 1 otherwise.
func TestACPUpstreamCloseEndsRun(t *testing.T) {
	cases := []struct {
		name   string
		end    func(c *websocket.Conn)
		status int
	}{
		{"normal", func(c *websocket.Conn) { _ = c.Close(websocket.StatusNormalClosure, "done") }, 0},
		{"going away", func(c *websocket.Conn) { _ = c.Close(websocket.StatusGoingAway, "restart") }, 0},
		{"error status", func(c *websocket.Conn) { _ = c.Close(websocket.StatusInternalError, "broke") }, 1},
		{"dropped", func(c *websocket.Conn) { _ = c.CloseNow() }, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			notification := `{"jsonrpc":"2.0","method":"session/update","params":{}}`
			srv := newACPUpstream(t, func(ctx context.Context, c *websocket.Conn) {
				if err := c.Write(ctx, websocket.MessageText, []byte(notification)); err != nil {
					return
				}
				tc.end(c)
			})
			s := startACP(t, wsURLOf(srv)+"/acp", testToken)

			if got := s.readLine(t); got != notification+"\n" {
				t.Fatalf("stdout line %q, want the notification", got)
			}
			if got := s.wait(t); got != tc.status {
				t.Fatalf("exit status = %d, want %d; log %q", got, tc.status, s.log.String())
			}
			if rest := s.rest(t); rest != "" {
				t.Fatalf("stdout carries %q after the protocol lines", rest)
			}
			if failed := strings.Contains(s.log.String(), "comlink: upstream failed: "); failed != (tc.status != 0) {
				t.Fatalf("log %q, want an upstream failure line: %v", s.log.String(), tc.status != 0)
			}
		})
	}
}

// TestACPConnectFailure: a refused token or an unreachable upstream exits 1
// with one log line that names the upstream without userinfo, query or
// fragment and never the token; stdout stays empty.
func TestACPConnectFailure(t *testing.T) {
	echo := newEchoUpstream(t)
	cases := []struct {
		name     string
		upstream string
		token    string
		want     string
	}{
		{"wrong token", echo.wsURL() + "/acp?token=secret-value", "wrong-token", "comlink: upstream connect failed: dial " + echo.wsURL() + "/acp: "},
		{"unreachable", "ws://user:secret-value@127.0.0.1:1/acp?token=secret-value#secret-value", testToken, "comlink: upstream connect failed: dial ws://127.0.0.1:1/acp: "},
		{"invalid URL", "ws://127.0.0.1:1/%zz?token=secret-value", testToken, "comlink: upstream connect failed: dial <invalid upstream URL>: "},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := startACP(t, tc.upstream, tc.token)
			if got := s.wait(t); got != 1 {
				t.Fatalf("exit status = %d, want 1", got)
			}
			if rest := s.rest(t); rest != "" {
				t.Fatalf("stdout carries %q", rest)
			}
			log := s.log.String()
			if !strings.HasPrefix(log, tc.want) || strings.Count(log, "\n") != 1 {
				t.Fatalf("log %q, want one line starting %q", log, tc.want)
			}
			if strings.Contains(log, "secret-value") || strings.Contains(log, tc.token) || strings.Contains(log, "user:") {
				t.Fatalf("log %q contains a secret", log)
			}
		})
	}
}

// TestACPBinaryFrameEndsRun: ACP messages are text, so a binary message from
// the upstream is not written to stdout; the upstream is told 1003 and Run
// exits 1.
func TestACPBinaryFrameEndsRun(t *testing.T) {
	closes := make(chan websocket.StatusCode, 1)
	srv := newACPUpstream(t, func(ctx context.Context, c *websocket.Conn) {
		if err := c.Write(ctx, websocket.MessageBinary, []byte("{}\n{}")); err != nil {
			return
		}
		_, _, err := c.Read(ctx)
		closes <- websocket.CloseStatus(err)
	})
	s := startACP(t, wsURLOf(srv)+"/acp", testToken)

	if got := s.wait(t); got != 1 {
		t.Fatalf("exit status = %d, want 1", got)
	}
	if rest := s.rest(t); rest != "" {
		t.Fatalf("stdout carries %q", rest)
	}
	if status := <-closes; status != websocket.StatusUnsupportedData {
		t.Fatalf("upstream close status = %d, want %d", status, websocket.StatusUnsupportedData)
	}
	if !strings.Contains(s.log.String(), "upstream sent a binary message") {
		t.Fatalf("log %q lacks the binary message", s.log.String())
	}
}

// TestACPNewlineFrameEndsRun: a text message holding \n or \r would reach
// the client as several lines, so none of it is written to stdout; the
// messages before it are, the upstream is told 1003 and Run exits 1 with one
// log line.
func TestACPNewlineFrameEndsRun(t *testing.T) {
	for _, data := range []string{"{\"a\":1}\n{\"b\":2}", "{\"a\":1}\r{\"b\":2}", "{\"a\":1}\r\n", "\n"} {
		t.Run(fmt.Sprintf("%q", data), func(t *testing.T) {
			closes := make(chan websocket.StatusCode, 1)
			srv := newACPUpstream(t, func(ctx context.Context, c *websocket.Conn) {
				for _, msg := range []string{`{"ok":true}`, data} {
					if err := c.Write(ctx, websocket.MessageText, []byte(msg)); err != nil {
						return
					}
				}
				_, _, err := c.Read(ctx)
				closes <- websocket.CloseStatus(err)
			})
			s := startACP(t, wsURLOf(srv)+"/acp", testToken)

			if line := s.readLine(t); line != "{\"ok\":true}\n" {
				t.Fatalf("first stdout line = %q", line)
			}
			if got := s.wait(t); got != 1 {
				t.Fatalf("exit status = %d, want 1", got)
			}
			if rest := s.rest(t); rest != "" {
				t.Fatalf("stdout carries %q", rest)
			}
			if status := <-closes; status != websocket.StatusUnsupportedData {
				t.Fatalf("upstream close status = %d, want %d", status, websocket.StatusUnsupportedData)
			}
			if log := s.log.String(); log != "comlink: upstream sent a message that contains a newline\n" {
				t.Fatalf("log %q, want one line naming the newline", log)
			}
		})
	}
}

// TestACPOversizedLineEndsRun: a stdin line longer than the read limit is
// not split or truncated; the upstream is told 1009 and Run exits 1.
func TestACPOversizedLineEndsRun(t *testing.T) {
	echo := newEchoUpstream(t)
	s := startACP(t, echo.wsURL()+"/acp", testToken)

	// Writes until Run stops reading; the cleanup's close releases it.
	go func() { _, _ = io.Copy(s.stdin, io.LimitReader(repeatReader('x'), upstream.ReadLimit+2)) }()
	if got := s.wait(t); got != 1 {
		t.Fatalf("exit status = %d, want 1", got)
	}
	if got := echo.closeStatus(t); got != websocket.StatusMessageTooBig {
		t.Fatalf("upstream close status = %d, want %d", got, websocket.StatusMessageTooBig)
	}
	if rest := s.rest(t); rest != "" {
		t.Fatalf("stdout carries %d bytes", len(rest))
	}
	if want := fmt.Sprintf("comlink: stdin message longer than %d bytes\n", upstream.ReadLimit); s.log.String() != want {
		t.Fatalf("log %q, want %q", s.log.String(), want)
	}
}

// repeatReader yields the same byte forever.
type repeatReader byte

func (r repeatReader) Read(b []byte) (int, error) {
	for i := range b {
		b[i] = byte(r)
	}
	return len(b), nil
}

// TestACPStdoutFailureEndsRun: when stdout cannot be written (the client is
// gone) the upstream is told 1001 and Run exits 1.
func TestACPStdoutFailureEndsRun(t *testing.T) {
	echo := newEchoUpstream(t)
	inR, inW := io.Pipe()
	t.Cleanup(func() { _ = inW.Close() })
	var logBuf lockedBuffer
	bridge := &acp.Bridge{Upstream: echo.wsURL() + "/acp", Token: testToken, Logger: log.New(&logBuf, "comlink: ", 0)}
	status := make(chan int, 1)
	go func() { status <- bridge.Run(inR, failingWriter{}) }()

	if _, err := io.WriteString(inW, `{"jsonrpc":"2.0","id":1,"method":"initialize"}`+"\n"); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-status:
		if got != 1 {
			t.Fatalf("exit status = %d, want 1", got)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("Run did not return within 15s of stdout failing")
	}
	if got := echo.closeStatus(t); got != websocket.StatusGoingAway {
		t.Fatalf("upstream close status = %d, want %d", got, websocket.StatusGoingAway)
	}
	if want := "comlink: write stdout: client gone\n"; logBuf.String() != want {
		t.Fatalf("log %q, want %q", logBuf.String(), want)
	}
}

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errors.New("client gone") }

// TestBinaryACP runs the built binary as an ACP client would: as the agent
// subprocess, with the token from --token-file or $COMLINK_TOKEN.
func TestBinaryACP(t *testing.T) {
	bin := buildComlink(t)
	echo := newEchoUpstream(t)
	roundTrip := func(t *testing.T, cmd *exec.Cmd) {
		t.Helper()
		stdin, err := cmd.StdinPipe()
		if err != nil {
			t.Fatal(err)
		}
		stdoutPipe, err := cmd.StdoutPipe()
		if err != nil {
			t.Fatal(err)
		}
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		stdout := bufio.NewReader(stdoutPipe)
		msg := `{"jsonrpc":"2.0","id":0,"method":"initialize","params":{"protocolVersion":1}}`
		if _, err := io.WriteString(stdin, msg+"\n"); err != nil {
			t.Fatal(err)
		}
		if line, err := stdout.ReadString('\n'); err != nil || line != msg+"\n" {
			t.Fatalf("stdout line %q (%v), want the echo; stderr %q", line, err, stderr.String())
		}
		if err := stdin.Close(); err != nil {
			t.Fatal(err)
		}
		rest, err := io.ReadAll(stdout)
		if err != nil || len(rest) != 0 {
			t.Fatalf("stdout carries %q (%v) after the protocol line", rest, err)
		}
		if err := cmd.Wait(); err != nil {
			t.Fatalf("comlink acp: %v, want exit 0; stderr %q", err, stderr.String())
		}
		if stderr.Len() != 0 {
			t.Fatalf("stderr %q after a clean session", stderr.String())
		}
		if got := echo.closeStatus(t); got != websocket.StatusNormalClosure {
			t.Fatalf("upstream close status = %d, want %d", got, websocket.StatusNormalClosure)
		}
	}

	t.Run("token file", func(t *testing.T) {
		cmd := exec.Command(bin, "acp", "--upstream", echo.wsURL()+"/acp", "--token-file", writeTokenFile(t, 0o600))
		cmd.Env = append(os.Environ(), "COMLINK_TOKEN=")
		roundTrip(t, cmd)
	})
	t.Run("COMLINK_TOKEN", func(t *testing.T) {
		cmd := exec.Command(bin, "acp", "--upstream", echo.wsURL()+"/acp")
		cmd.Env = append(os.Environ(), "COMLINK_TOKEN="+testToken)
		roundTrip(t, cmd)
	})
	t.Run("token file readable by others", func(t *testing.T) {
		path := writeTokenFile(t, 0o644)
		cmd := exec.Command(bin, "acp", "--upstream", "ws://127.0.0.1:1/acp?token=secret-value", "--token-file", path)
		var stdout, stderr bytes.Buffer
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		err := cmd.Run()
		if exitErr, ok := err.(*exec.ExitError); !ok || exitErr.ExitCode() != 1 {
			t.Fatalf("comlink acp: %v, want exit 1; stderr %q", err, stderr.String())
		}
		want := "comlink: warning: token file " + path + " is readable by group or others; restrict it with chmod 600\n" +
			"comlink: upstream connect failed: dial ws://127.0.0.1:1/acp: "
		if !strings.HasPrefix(stderr.String(), want) || strings.Count(stderr.String(), "\n") != 2 {
			t.Fatalf("stderr %q, want the warning and one connect failure line", stderr.String())
		}
		if strings.Contains(stderr.String(), testToken) || strings.Contains(stderr.String(), "secret-value") {
			t.Fatalf("stderr %q contains a secret", stderr.String())
		}
		if stdout.Len() != 0 {
			t.Fatalf("stdout carries %q", stdout.String())
		}
	})
	t.Run("missing token file", func(t *testing.T) {
		cmd := exec.Command(bin, "acp", "--upstream", echo.wsURL()+"/acp", "--token-file", filepath.Join(t.TempDir(), "absent"))
		var stdout, stderr bytes.Buffer
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		err := cmd.Run()
		if exitErr, ok := err.(*exec.ExitError); !ok || exitErr.ExitCode() == 0 {
			t.Fatalf("comlink acp: %v, want a non-zero exit", err)
		}
		if !strings.Contains(stderr.String(), "comlink: error: read token file: ") || stdout.Len() != 0 {
			t.Fatalf("stdout %q, stderr %q, want only the token file error", stdout.String(), stderr.String())
		}
	})
}
