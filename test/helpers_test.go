// Package test holds every comlink test. The packages under internal/ carry
// no tests of their own: everything is driven through the public entry
// points, mostly an SSH client talking to sshd.Serve with the Codex route
// end to end, and the same flows through real OpenSSH with the comlink
// binary as its ProxyCommand.
package test

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
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

	"github.com/circlesac/comlink/internal/codex"
	"github.com/circlesac/comlink/internal/sshd"
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

// startServer runs sshd.Serve with route over a loopback connection and
// returns a connected x/crypto/ssh client. Serve must return without error
// once the client disconnects.
func startServer(t *testing.T, route sshd.Route) *ssh.Client {
	t.Helper()
	serverSide, clientSide := connPair(t)
	serveErr := make(chan error, 1)
	go func() { serveErr <- sshd.Serve(serverSide, testSigner(t), route) }()

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
	})
	return client
}

// startCodex runs the server with the Codex route and returns the client
// plus the route's log. The log must never contain the token.
func startCodex(t *testing.T, route codex.Route) (*ssh.Client, *lockedBuffer) {
	t.Helper()
	var logBuf lockedBuffer
	route.Logger = log.New(&logBuf, "comlink: ", 0)
	if route.ReportVersion == "" {
		route.ReportVersion = "0.165.0"
	}
	client := startServer(t, &route)
	t.Cleanup(func() {
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

// run sends command as an exec request on a new session with no stdin and
// returns what came back on stdout and stderr and the exit status.
func run(t *testing.T, client *ssh.Client, command string) (stdout, stderr string, status int) {
	t.Helper()
	sess, err := client.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sess.Close() }()
	var errBuf bytes.Buffer
	sess.Stderr = &errBuf
	out, err := sess.Output(command)
	return string(out), errBuf.String(), exitStatus(t, err)
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

// wsURLOf is the WebSocket address of an httptest server.
func wsURLOf(srv *httptest.Server) string { return "ws" + strings.TrimPrefix(srv.URL, "http") }

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
