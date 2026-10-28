package test

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"golang.org/x/crypto/ssh"

	"github.com/circlesac/comlink/internal/route/codex"
	"github.com/circlesac/comlink/internal/sshserver"
)

const testToken = "test-token-do-not-print"

func testSigner(t *testing.T) ssh.Signer {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	return signer
}

// echoUpstream is a WebSocket server that requires the bearer token and
// echoes every message back with the same type.
type echoUpstream struct {
	*httptest.Server
	mu    sync.Mutex
	auths []string
}

func newEchoUpstream(t *testing.T) *echoUpstream {
	t.Helper()
	u := &echoUpstream{}
	u.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u.mu.Lock()
		u.auths = append(u.auths, r.Header.Get("Authorization"))
		u.mu.Unlock()
		if r.Header.Get("Authorization") != "Bearer "+testToken {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		c.SetReadLimit(codex.ReadLimit)
		for {
			typ, data, err := c.Read(r.Context())
			if err != nil {
				return
			}
			if err := c.Write(r.Context(), typ, data); err != nil {
				return
			}
		}
	}))
	t.Cleanup(u.Close)
	return u
}

func (u *echoUpstream) wsURL() string { return "ws" + strings.TrimPrefix(u.URL, "http") }

func (u *echoUpstream) seenAuths() []string {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]string(nil), u.auths...)
}

// connPair returns two ends of a loopback TCP connection. net.Pipe cannot be
// used: it is unbuffered and both SSH sides write their version banner
// before reading, which deadlocks.
func connPair(t *testing.T) (serverSide, clientSide net.Conn) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	accepted := make(chan net.Conn, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			close(accepted)
			return
		}
		accepted <- c
	}()
	clientSide, err = net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	serverSide, ok := <-accepted
	if !ok {
		t.Fatal("accept failed")
	}
	t.Cleanup(func() {
		_ = serverSide.Close()
		_ = clientSide.Close()
	})
	return serverSide, clientSide
}

// startServer runs comlink's SSH server with the Codex route over a loopback
// connection and returns a connected x/crypto/ssh client plus the server's
// stderr log.
func startServer(t *testing.T, route codex.Route) (*ssh.Client, *lockedBuffer) {
	t.Helper()
	var logBuf lockedBuffer
	route.Logger = log.New(&logBuf, "comlink: ", 0)
	if route.ReportVersion == "" {
		route.ReportVersion = "0.165.0"
	}
	serverSide, clientSide := connPair(t)
	serveErr := make(chan error, 1)
	go func() { serveErr <- sshserver.Serve(serverSide, testSigner(t), &route) }()

	conn, chans, reqs, err := ssh.NewClientConn(clientSide, "comlink", &ssh.ClientConfig{
		User:            "comlink",
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         10 * time.Second,
	})
	if err != nil {
		t.Fatalf("client handshake: %v", err)
	}
	client := ssh.NewClient(conn, chans, reqs)
	t.Cleanup(func() {
		_ = client.Close()
		select {
		case err := <-serveErr:
			if err != nil {
				t.Errorf("serve returned %v", err)
			}
		case <-time.After(10 * time.Second):
			t.Error("serve did not return after the client disconnected")
		}
		if strings.Contains(logBuf.String(), testToken) {
			t.Error("server log contains the upstream token")
		}
	})
	return client, &logBuf
}

type lockedBuffer struct {
	mu sync.Mutex
	bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.Buffer.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.Buffer.String()
}

func exitStatus(t *testing.T, err error) int {
	t.Helper()
	if err == nil {
		return 0
	}
	var exitErr *ssh.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitStatus()
	}
	t.Fatalf("command failed without an exit status: %v", err)
	return -1
}

func TestServerAcceptsForwardAndAnswersVersionProbe(t *testing.T) {
	upstream := newEchoUpstream(t)
	client, _ := startServer(t, codex.Route{Upstream: upstream.wsURL(), Token: testToken, ReportVersion: "0.170.0"})

	ok, reply, err := client.SendRequest("tcpip-forward", true, ssh.Marshal(struct {
		Addr string
		Port uint32
	}{"127.0.0.1", 0}))
	if err != nil || !ok {
		t.Fatalf("tcpip-forward: ok=%v err=%v", ok, err)
	}
	var port struct{ Port uint32 }
	if err := ssh.Unmarshal(reply, &port); err != nil || port.Port == 0 {
		t.Fatalf("tcpip-forward reply = %x (%v), want a dynamic port", reply, err)
	}
	ok, _, err = client.SendRequest("tcpip-forward", true, ssh.Marshal(struct {
		Addr string
		Port uint32
	}{"127.0.0.1", 62870}))
	if err != nil || !ok {
		t.Fatalf("tcpip-forward with fixed port: ok=%v err=%v", ok, err)
	}
	ok, _, err = client.SendRequest("keepalive@openssh.com", true, nil)
	if err != nil || ok {
		t.Fatalf("keepalive: ok=%v err=%v, want refused", ok, err)
	}

	sess, err := client.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sess.Close() }()
	var stderr bytes.Buffer
	sess.Stderr = &stderr
	out, err := sess.Output(wrapLikeApp(t, versionPayload))
	if got := exitStatus(t, err); got != 0 {
		t.Fatalf("exit status = %d, stderr %q", got, stderr.String())
	}
	if string(out) != "codex-cli 0.170.0\n" {
		t.Fatalf("stdout = %q", out)
	}
}

func TestServerAcknowledgesSetupCommands(t *testing.T) {
	client, _ := startServer(t, codex.Route{Upstream: "ws://127.0.0.1:1/unused"})
	for name, payload := range map[string]string{"probe": probePayload, "bootstrap": bootstrapPayload, "pkill": killPayload} {
		t.Run(name, func(t *testing.T) {
			sess, err := client.NewSession()
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = sess.Close() }()
			out, err := sess.CombinedOutput(wrapLikeApp(t, payload))
			if got := exitStatus(t, err); got != 0 || len(out) != 0 {
				t.Fatalf("exit status = %d, output %q", got, out)
			}
		})
	}
}

func TestServerRefusesUnknownCommandShellAndSubsystem(t *testing.T) {
	client, logBuf := startServer(t, codex.Route{Upstream: "ws://127.0.0.1:1/unused"})

	sess, err := client.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	sess.Stderr = &stderr
	out, err := sess.Output(wrapLikeApp(t, "rm -rf /tmp/x"))
	if got := exitStatus(t, err); got != 127 {
		t.Fatalf("exit status = %d, want 127", got)
	}
	if len(out) != 0 || !strings.Contains(stderr.String(), "comlink: unsupported command: rm -rf /tmp/x") {
		t.Fatalf("stdout %q stderr %q", out, stderr.String())
	}
	if strings.Contains(logBuf.String(), "unsupported command") {
		t.Fatalf("server log %q repeats the refusal the client already sees", logBuf.String())
	}
	_ = sess.Close()

	sess, err = client.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	if err := sess.Shell(); err == nil {
		t.Fatal("shell request was accepted")
	}
	_ = sess.Close()

	sess, err = client.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	if err := sess.RequestSubsystem("sftp"); err == nil {
		t.Fatal("sftp subsystem request was accepted")
	}
	_ = sess.Close()

	sess, err = client.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	if err := sess.RequestPty("xterm", 24, 80, nil); err == nil {
		t.Fatal("pty-req was accepted")
	}
	_ = sess.Close()

	sess, err = client.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	if err := sess.Setenv("TERM", "xterm"); err != nil {
		t.Fatalf("env request refused: %v", err)
	}
	_ = sess.Close()

	ch, _, err := client.OpenChannel("direct-tcpip", nil)
	if err == nil {
		_ = ch.Close()
		t.Fatal("direct-tcpip channel was accepted")
	}
}

// proxySession starts the captured proxy command on a session, consumes the
// marker and returns the channel's stdin/stdout as a net.Conn plus the
// session for Wait.
func proxySession(t *testing.T, client *ssh.Client) (*ssh.Session, net.Conn) {
	t.Helper()
	sess, err := client.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	stdin, err := sess.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := sess.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := sess.Start(readFixture(t)); err != nil {
		t.Fatal(err)
	}
	marker := make([]byte, len(fixtureMarker))
	if _, err := io.ReadFull(stdout, marker); err != nil {
		t.Fatalf("read marker: %v", err)
	}
	if !bytes.Equal(marker, fixtureMarker) {
		t.Fatalf("marker = %q, want %q", marker, fixtureMarker)
	}
	// StdoutPipe returns the channel itself; hide its Close so closing the
	// WebSocket only sends EOF (like the ssh client) instead of closing the
	// channel before the exit status arrives.
	return sess, &sshserver.StdioConn{In: struct{ io.Reader }{stdout}, Out: stdin}
}

// dialOverConn opens the WebSocket the Codex app opens over the channel.
func dialOverConn(t *testing.T, conn net.Conn) *websocket.Conn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, _, err := websocket.Dial(ctx, "ws://codex-app-server/rpc", &websocket.DialOptions{
		HTTPClient: &http.Client{Transport: &http.Transport{
			DialContext: func(context.Context, string, string) (net.Conn, error) { return conn, nil },
		}},
	})
	if err != nil {
		t.Fatalf("dial over channel: %v", err)
	}
	c.SetReadLimit(codex.ReadLimit)
	return c
}

func TestProxyRelaysMessagesToUpstreamWithToken(t *testing.T) {
	upstream := newEchoUpstream(t)
	client, _ := startServer(t, codex.Route{Upstream: upstream.wsURL(), Token: testToken})
	sess, conn := proxySession(t, client)
	ws := dialOverConn(t, conn)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	msg := `{"jsonrpc":"2.0","id":1,"method":"initialize"}`
	if err := ws.Write(ctx, websocket.MessageText, []byte(msg)); err != nil {
		t.Fatal(err)
	}
	typ, data, err := ws.Read(ctx)
	if err != nil || typ != websocket.MessageText || string(data) != msg {
		t.Fatalf("round trip: typ=%v data=%q err=%v", typ, data, err)
	}

	big := bytes.Repeat([]byte{0xAB}, 300<<10) // above the 32 KiB library default
	if err := ws.Write(ctx, websocket.MessageBinary, big); err != nil {
		t.Fatal(err)
	}
	typ, data, err = ws.Read(ctx)
	if err != nil || typ != websocket.MessageBinary || !bytes.Equal(data, big) {
		t.Fatalf("large binary round trip: typ=%v len=%d err=%v", typ, len(data), err)
	}

	if err := ws.Close(websocket.StatusNormalClosure, "done"); err != nil {
		t.Fatalf("close: %v", err)
	}
	if got := exitStatus(t, sess.Wait()); got != 0 {
		t.Fatalf("exit status = %d, want 0", got)
	}
	auths := upstream.seenAuths()
	if len(auths) != 1 || auths[0] != "Bearer "+testToken {
		t.Fatalf("upstream saw Authorization %q", auths)
	}
}

func TestProxyWithWrongTokenExitsOne(t *testing.T) {
	upstream := newEchoUpstream(t)
	client, logBuf := startServer(t, codex.Route{Upstream: upstream.wsURL(), Token: "wrong-token"})
	sess, conn := proxySession(t, client)
	ws := dialOverConn(t, conn)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	_, _, err := ws.Read(ctx)
	if websocket.CloseStatus(err) != websocket.StatusInternalError {
		t.Fatalf("read err = %v, want close status %d", err, websocket.StatusInternalError)
	}
	if got := exitStatus(t, sess.Wait()); got != 1 {
		t.Fatalf("exit status = %d, want 1", got)
	}
	if !strings.Contains(logBuf.String(), "upstream connect failed") {
		t.Fatalf("server log %q lacks the upstream failure", logBuf.String())
	}
	if strings.Contains(logBuf.String(), "wrong-token") {
		t.Fatalf("server log %q contains the token", logBuf.String())
	}
}

func TestProxyUpstreamCloseEndsCommand(t *testing.T) {
	closeUpstream := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		<-closeUpstream
		_ = c.Close(websocket.StatusGoingAway, "bye")
	}))
	t.Cleanup(srv.Close)
	client, _ := startServer(t, codex.Route{Upstream: "ws" + strings.TrimPrefix(srv.URL, "http")})
	sess, conn := proxySession(t, client)
	ws := dialOverConn(t, conn)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	close(closeUpstream)
	_, _, err := ws.Read(ctx)
	if websocket.CloseStatus(err) != websocket.StatusGoingAway {
		t.Fatalf("read err = %v, want close status %d", err, websocket.StatusGoingAway)
	}
	if got := exitStatus(t, sess.Wait()); got != 0 {
		t.Fatalf("exit status = %d, want 0 after a clean upstream close", got)
	}
}
