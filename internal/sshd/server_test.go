package sshd

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

// fakeRoute records every command it is handed, writes "out:<command>" to
// stdout and "err:<command>" to stderr and exits with status.
type fakeRoute struct {
	status int
	mu     sync.Mutex
	got    []string
}

func (r *fakeRoute) Exec(ch ssh.Channel, command string) int {
	r.mu.Lock()
	r.got = append(r.got, command)
	r.mu.Unlock()
	_, _ = io.WriteString(ch, "out:"+command)
	_, _ = io.WriteString(ch.Stderr(), "err:"+command)
	return r.status
}

func (r *fakeRoute) commands() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.got...)
}

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

// startServer runs Serve with route over a loopback connection and returns
// a connected x/crypto/ssh client.
func startServer(t *testing.T, route Route) *ssh.Client {
	t.Helper()
	serverSide, clientSide := connPair(t)
	serveErr := make(chan error, 1)
	go func() { serveErr <- Serve(serverSide, testSigner(t), route) }()

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

func TestServeAcceptsForwardAndRefusesOtherGlobalRequests(t *testing.T) {
	client := startServer(t, &fakeRoute{})

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
	ok, _, err = client.SendRequest("cancel-tcpip-forward", true, ssh.Marshal(struct {
		Addr string
		Port uint32
	}{"127.0.0.1", 62870}))
	if err != nil || !ok {
		t.Fatalf("cancel-tcpip-forward: ok=%v err=%v", ok, err)
	}
	ok, _, err = client.SendRequest("keepalive@openssh.com", true, nil)
	if err != nil || ok {
		t.Fatalf("keepalive: ok=%v err=%v, want refused", ok, err)
	}
}

func TestServeHandsExecToRoute(t *testing.T) {
	route := &fakeRoute{status: 42}
	client := startServer(t, route)

	sess, err := client.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sess.Close() }()
	var stderr bytes.Buffer
	sess.Stderr = &stderr
	command := `sh -c 'exec "$0"' sh 'echo hi'`
	out, err := sess.Output(command)
	if got := exitStatus(t, err); got != 42 {
		t.Fatalf("exit status = %d, want 42", got)
	}
	if string(out) != "out:"+command || stderr.String() != "err:"+command {
		t.Fatalf("stdout %q stderr %q", out, stderr.String())
	}
	if got := route.commands(); len(got) != 1 || got[0] != command {
		t.Fatalf("route got %q, want [%q]", got, command)
	}
}

func TestServeRefusesShellSubsystemPtyAndOtherChannels(t *testing.T) {
	route := &fakeRoute{}
	client := startServer(t, route)

	sess, err := client.NewSession()
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
	if got := route.commands(); len(got) != 0 {
		t.Fatalf("route was handed %q", got)
	}
}
