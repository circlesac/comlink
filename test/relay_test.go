package test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"golang.org/x/crypto/ssh"

	"github.com/circlesac/comlink/internal/codex"
	"github.com/circlesac/comlink/internal/sshd"
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
	client, _ := startCodex(t, codex.Route{Upstream: upstream.wsURL(), Token: testToken})
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
	client, logBuf := startCodex(t, codex.Route{Upstream: upstream.wsURL(), Token: "wrong-token"})
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
	client, logBuf := startCodex(t, codex.Route{Upstream: upstream.wsURL(), Token: testToken})
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
	client, logBuf := startCodex(t, codex.Route{Upstream: upstream.wsURL(), Token: testToken})
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
// close frame, so the command exits 1 and the upstream is closed.
func TestProxyClientDroppingExitsOne(t *testing.T) {
	upstream := newEchoUpstream(t)
	client, logBuf := startCodex(t, codex.Route{Upstream: upstream.wsURL(), Token: testToken})
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
	if strings.Contains(logBuf.String(), "upstream failed") {
		t.Fatalf("server log %q blames the upstream for the client's drop", logBuf.String())
	}
}
