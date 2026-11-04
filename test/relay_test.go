package test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"golang.org/x/crypto/ssh"

	"github.com/circlesac/comlink/internal/codex"
	"github.com/circlesac/comlink/internal/sshd"
	comlinkup "github.com/circlesac/comlink/internal/upstream"
)

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
	if err := sess.Start(readFixture(t, "codex-app-proxy.txt")); err != nil {
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
	return sess, &sshd.StdioConn{In: struct{ io.Reader }{stdout}, Out: eofOKCloser{stdin}}
}

// eofOKCloser is the session's stdin with one difference: Close treats io.EOF
// as success. Once comlink has answered the client's close frame it ends the
// channel, so the client's own CloseWrite can race it and report EOF on a
// clean shutdown.
type eofOKCloser struct{ io.WriteCloser }

func (c eofOKCloser) Close() error {
	if err := c.WriteCloser.Close(); err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	return nil
}

func TestProxyRelaysMessagesToUpstreamWithToken(t *testing.T) {
	upstream := newEchoUpstream(t)
	client, _ := startCodex(t, codex.Route{Upstream: upstream.wsURL(), Credential: comlinkup.Credential{Token: testToken}})
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

func TestProxyWithoutTokenSendsNoAuthorization(t *testing.T) {
	var auths lockedBuffer
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = auths.Write([]byte("[" + r.Header.Get("Authorization") + "]"))
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		_ = c.Close(websocket.StatusNormalClosure, "bye")
	}))
	t.Cleanup(srv.Close)
	client, _ := startCodex(t, codex.Route{Upstream: wsURLOf(srv)})
	sess, conn := proxySession(t, client)
	ws := dialOverConn(t, conn)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if _, _, err := ws.Read(ctx); websocket.CloseStatus(err) != websocket.StatusNormalClosure {
		t.Fatalf("read err = %v, want close status %d", err, websocket.StatusNormalClosure)
	}
	if got := exitStatus(t, sess.Wait()); got != 0 {
		t.Fatalf("exit status = %d, want 0", got)
	}
	if auths.String() != "[]" {
		t.Fatalf("upstream saw Authorization %s, want none", auths.String())
	}
}

func TestProxyWithWrongTokenExitsOne(t *testing.T) {
	upstream := newEchoUpstream(t)
	client, logBuf := startCodex(t, codex.Route{Upstream: upstream.wsURL(), Credential: comlinkup.Credential{Token: "wrong-token"}})
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

// TestProxyTokenCommandRunsOnlyForTheRelay: the token command runs when the
// relay dials, not for the app's probe and version commands, so a failing
// helper leaves them answering; the proxy then exits 1 with one log line
// naming the failure, never the output, and the upstream is not dialled.
func TestProxyTokenCommandRunsOnlyForTheRelay(t *testing.T) {
	upstream := newEchoUpstream(t)
	counter := filepath.Join(t.TempDir(), "runs")
	failing := comlinkup.Credential{Command: "echo run >> " + counter + "; printf %s " + testToken + "; exit 3"}
	client, logBuf := startCodex(t, codex.Route{Upstream: upstream.wsURL(), Credential: failing, ReportVersion: "0.165.0"})
	if out, _, status := run(t, client, readFixture(t, "codex-app-version.txt")); status != 0 || out != "codex-cli 0.165.0\n" {
		t.Fatalf("version probe with a failing helper: status %d stdout %q", status, out)
	}
	if _, _, status := run(t, client, readFixture(t, "codex-app-probe.txt")); status != 0 {
		t.Fatalf("codex probe with a failing helper: status %d", status)
	}
	if _, err := os.Stat(counter); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the token command ran for a probe (%v)", err)
	}
	sess, conn := proxySession(t, client)
	ws := dialOverConn(t, conn)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, _, err := ws.Read(ctx); websocket.CloseStatus(err) != websocket.StatusInternalError {
		t.Fatalf("read err = %v, want close status %d", err, websocket.StatusInternalError)
	}
	if got := exitStatus(t, sess.Wait()); got != 1 {
		t.Fatalf("exit status = %d, want 1", got)
	}
	if want := "comlink: upstream token: token command failed: exit status 3\n"; logBuf.String() != want {
		t.Fatalf("server log %q, want %q", logBuf.String(), want)
	}
	if len(upstream.seenAuths()) != 0 {
		t.Fatal("the upstream was dialled without a token")
	}
}

// TestProxyUpstreamURLIsRedactedInLog covers the log line for an upstream
// that cannot be reached: it names the upstream without userinfo, query or
// fragment, and not at all when the URL does not parse.
func TestProxyUpstreamURLIsRedactedInLog(t *testing.T) {
	cases := []struct {
		name     string
		upstream string
		want     string
	}{
		{"plain", "ws://127.0.0.1:1/rpc", "upstream connect failed: dial ws://127.0.0.1:1/rpc: "},
		{"query", "ws://127.0.0.1:1/rpc?token=secret-value", "upstream connect failed: dial ws://127.0.0.1:1/rpc: "},
		{"userinfo query fragment", "ws://user:secret-value@127.0.0.1:1/rpc?a=secret-value#secret-value", "upstream connect failed: dial ws://127.0.0.1:1/rpc: "},
		{"empty query", "ws://127.0.0.1:1/rpc?", "upstream connect failed: dial ws://127.0.0.1:1/rpc: "},
		{"invalid", "ws://127.0.0.1:1/%zz?token=secret-value", "upstream connect failed: dial <invalid upstream URL>: "},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			client, logBuf := startCodex(t, codex.Route{Upstream: tc.upstream})
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
			if log := logBuf.String(); !strings.Contains(log, tc.want) || strings.Contains(log, "secret-value") || strings.Contains(log, "user:") {
				t.Fatalf("server log %q, want %q and no secret", log, tc.want)
			}
		})
	}
}

func TestProxyWithoutHTTPUpgradeExitsOne(t *testing.T) {
	upstream := newEchoUpstream(t)
	client, logBuf := startCodex(t, codex.Route{Upstream: upstream.wsURL(), Credential: comlinkup.Credential{Token: testToken}})
	sess, conn := proxySession(t, client)

	// The client sends EOF before any request.
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}
	if got := exitStatus(t, sess.Wait()); got != 1 {
		t.Fatalf("exit status = %d, want 1", got)
	}
	if !strings.Contains(logBuf.String(), "client did not send an HTTP upgrade") {
		t.Fatalf("server log %q lacks the failure", logBuf.String())
	}
	if auths := upstream.seenAuths(); len(auths) != 0 {
		t.Fatalf("upstream was dialled (%q) without a client handshake", auths)
	}
}

func TestProxyRejectsPlainHTTPRequest(t *testing.T) {
	upstream := newEchoUpstream(t)
	client, logBuf := startCodex(t, codex.Route{Upstream: upstream.wsURL(), Credential: comlinkup.Credential{Token: testToken}})
	sess, conn := proxySession(t, client)

	if _, err := io.WriteString(conn, "GET /rpc HTTP/1.1\r\nHost: codex-app-server\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	response, err := io.ReadAll(conn)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(response), "HTTP/1.1 426 ") || !strings.Contains(string(response), "\r\nConnection: close\r\n") {
		t.Fatalf("response = %q, want 426 Upgrade Required with Connection: close", response)
	}
	if got := exitStatus(t, sess.Wait()); got != 1 {
		t.Fatalf("exit status = %d, want 1", got)
	}
	if !strings.Contains(logBuf.String(), "client websocket handshake failed") {
		t.Fatalf("server log %q lacks the failure", logBuf.String())
	}
	if auths := upstream.seenAuths(); len(auths) != 0 {
		t.Fatalf("upstream was dialled (%q) without a client handshake", auths)
	}
}

// TestProxyEndsWhenClientIgnoresUpstreamClose is the hang case: the upstream
// closes first and the client never reads, so it never answers comlink's
// close frame. The session must still end within the close-handshake
// timeout instead of waiting for a frame that never comes.
func TestProxyEndsWhenClientIgnoresUpstreamClose(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		_ = c.Close(websocket.StatusGoingAway, "bye")
	}))
	t.Cleanup(srv.Close)
	client, _ := startCodex(t, codex.Route{Upstream: wsURLOf(srv)})
	sess, conn := proxySession(t, client)
	ws := dialOverConn(t, conn)
	defer func() { _ = ws.CloseNow() }() // never read, never closed while comlink waits

	waited := make(chan error, 1)
	go func() { waited <- sess.Wait() }()
	select {
	case err := <-waited:
		if got := exitStatus(t, err); got != 0 {
			t.Fatalf("exit status = %d, want 0 after a clean upstream close", got)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("session did not end within 15s of the upstream closing")
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
	client, _ := startCodex(t, codex.Route{Upstream: wsURLOf(srv)})
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

// TestProxyUpstreamDroppingExitsOne: the upstream drops the connection
// without a close frame, so the client is told the peer failed and the
// command exits 1.
func TestProxyUpstreamDroppingExitsOne(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		if _, _, err := c.Read(r.Context()); err != nil {
			return
		}
		_ = c.CloseNow()
	}))
	t.Cleanup(srv.Close)
	client, logBuf := startCodex(t, codex.Route{Upstream: wsURLOf(srv)})
	sess, conn := proxySession(t, client)
	ws := dialOverConn(t, conn)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := ws.Write(ctx, websocket.MessageText, []byte(`{"jsonrpc":"2.0","id":1,"method":"initialize"}`)); err != nil {
		t.Fatal(err)
	}
	_, _, err := ws.Read(ctx)
	if websocket.CloseStatus(err) != websocket.StatusInternalError {
		t.Fatalf("read err = %v, want close status %d", err, websocket.StatusInternalError)
	}
	if got := exitStatus(t, sess.Wait()); got != 1 {
		t.Fatalf("exit status = %d, want 1", got)
	}
	if !strings.Contains(logBuf.String(), "upstream failed") {
		t.Fatalf("server log %q lacks the upstream failure", logBuf.String())
	}
}

// TestProxyClientDroppingExitsOne: the client ends the channel without a
// close frame, so the command exits 1 and the upstream is told the client
// went away (1001), not that something failed.
func TestProxyClientDroppingExitsOne(t *testing.T) {
	upstream := newEchoUpstream(t)
	client, logBuf := startCodex(t, codex.Route{Upstream: upstream.wsURL(), Credential: comlinkup.Credential{Token: testToken}})
	sess, conn := proxySession(t, client)
	ws := dialOverConn(t, conn)
	defer func() { _ = ws.CloseNow() }()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	msg := `{"jsonrpc":"2.0","id":1,"method":"initialize"}`
	if err := ws.Write(ctx, websocket.MessageText, []byte(msg)); err != nil {
		t.Fatal(err)
	}
	if _, data, err := ws.Read(ctx); err != nil || string(data) != msg {
		t.Fatalf("round trip: data=%q err=%v", data, err)
	}
	if err := conn.Close(); err != nil { // EOF on the channel, no close frame
		t.Fatal(err)
	}
	if got := exitStatus(t, sess.Wait()); got != 1 {
		t.Fatalf("exit status = %d, want 1", got)
	}
	if got := upstream.closeStatus(t); got != websocket.StatusGoingAway {
		t.Fatalf("upstream close status = %d, want %d", got, websocket.StatusGoingAway)
	}
	if strings.Contains(logBuf.String(), "upstream failed") {
		t.Fatalf("server log %q blames the upstream for the client's drop", logBuf.String())
	}
}

// TestProxyClientFailureIsPassedOn: a client that closes with an error
// status is a real failure, so the upstream gets 1011 and the command exits 1.
func TestProxyClientFailureIsPassedOn(t *testing.T) {
	upstream := newEchoUpstream(t)
	client, _ := startCodex(t, codex.Route{Upstream: upstream.wsURL(), Credential: comlinkup.Credential{Token: testToken}})
	sess, conn := proxySession(t, client)
	ws := dialOverConn(t, conn)

	if err := ws.Close(websocket.StatusInternalError, "client broke"); err != nil {
		t.Fatalf("close: %v", err)
	}
	if got := exitStatus(t, sess.Wait()); got != 1 {
		t.Fatalf("exit status = %d, want 1", got)
	}
	if got := upstream.closeStatus(t); got != websocket.StatusInternalError {
		t.Fatalf("upstream close status = %d, want %d", got, websocket.StatusInternalError)
	}
}

// sshChannelWindow is the receive window x/crypto/ssh grants each channel
// (channelWindowSize). Without window adjustments from the client, comlink
// can put at most this much data on the channel.
const sshChannelWindow = 2 << 20

// countingConn counts the bytes read from the connection and closes reached
// once at least threshold bytes have arrived.
type countingConn struct {
	net.Conn
	threshold int64
	n         atomic.Int64
	once      sync.Once
	reached   chan struct{}
}

func (c *countingConn) Read(b []byte) (int, error) {
	n, err := c.Conn.Read(b)
	if c.n.Add(int64(n)) >= c.threshold {
		c.once.Do(func() { close(c.reached) })
	}
	return n, err
}

// TestProxyBlockedWriteEndsWhenClientConnectionCloses is the stuck-write
// case: the client stops reading while the upstream sends a message several
// times the SSH channel window, so comlink's write to the channel cannot
// complete. Closing the client's connection, as the ssh process exiting
// does, must release that write: Serve returns and the upstream is told the
// client went away.
func TestProxyBlockedWriteEndsWhenClientConnectionCloses(t *testing.T) {
	upstream := newEchoUpstream(t)
	var logBuf lockedBuffer
	route := &codex.Route{
		Upstream:      upstream.wsURL(),
		Credential:    comlinkup.Credential{Token: testToken},
		ReportVersion: "0.165.0",
		Logger:        log.New(&logBuf, "comlink: ", 0),
	}
	signer := testSigner(t)
	serverSide, clientSide := connPair(t)
	serveErr := make(chan error, 1)
	go func() { serveErr <- sshd.Serve(serverSide, signer, route) }()

	counted := &countingConn{Conn: clientSide, threshold: sshChannelWindow, reached: make(chan struct{})}
	sconn, chans, reqs, err := ssh.NewClientConn(counted, "comlink", &ssh.ClientConfig{
		User:            "comlink",
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         10 * time.Second,
	})
	if err != nil {
		t.Fatalf("client handshake: %v", err)
	}
	client := ssh.NewClient(sconn, chans, reqs)
	defer func() { _ = client.Close() }()
	_, conn := proxySession(t, client)
	ws := dialOverConn(t, conn)
	defer func() { _ = ws.CloseNow() }()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// The upstream echoes this back; the client never reads it.
	big := bytes.Repeat([]byte{0xCD}, 4*sshChannelWindow)
	if err := ws.Write(ctx, websocket.MessageBinary, big); err != nil {
		t.Fatal(err)
	}
	// Once a full window has reached the client unread, comlink has used up
	// the window in the middle of writing the echo; the rest of it can only
	// go out after window adjustments that never come.
	select {
	case <-counted.reached:
	case <-time.After(15 * time.Second):
		t.Fatalf("client received %d bytes in 15s, want a full channel window", counted.n.Load())
	}

	if err := clientSide.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-serveErr:
		if err != nil {
			t.Fatalf("serve returned %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("serve did not return within 15s of the client connection closing")
	}
	if got := counted.n.Load(); got >= int64(len(big)) {
		t.Fatalf("client received %d bytes, the whole %d-byte echo: the write never blocked", got, len(big))
	}
	if got := upstream.closeStatus(t); got != websocket.StatusGoingAway {
		t.Fatalf("upstream close status = %d, want %d", got, websocket.StatusGoingAway)
	}
	if strings.Contains(logBuf.String(), testToken) {
		t.Fatal("server log contains the upstream token")
	}
}
