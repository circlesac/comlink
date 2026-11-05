package test

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/circlesac/comlink/internal/sshd"
)

// fakeRoute is a Route for exercising the server alone. It records every
// command it is handed, writes "out:<command>" to stdout and "err:<command>"
// to stderr, waits on block when set, and exits with status.
type fakeRoute struct {
	status int
	block  chan struct{}
	mu     sync.Mutex
	got    []string
}

func (r *fakeRoute) Exec(ch ssh.Channel, command string) int {
	r.mu.Lock()
	r.got = append(r.got, command)
	r.mu.Unlock()
	_, _ = io.WriteString(ch, "out:"+command)
	_, _ = io.WriteString(ch.Stderr(), "err:"+command)
	if r.block != nil {
		<-r.block
	}
	return r.status
}

func (r *fakeRoute) commands() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.got...)
}

func TestServeHandsExecToRoute(t *testing.T) {
	route := &fakeRoute{status: 42}
	client := startServer(t, route)

	command := `sh -c 'exec "$0"' sh 'echo hi'`
	out, stderr, status := run(t, client, command)
	if status != 42 {
		t.Fatalf("exit status = %d, want 42", status)
	}
	if out != "out:"+command || stderr != "err:"+command {
		t.Fatalf("stdout %q stderr %q", out, stderr)
	}
	if got := route.commands(); len(got) != 1 || got[0] != command {
		t.Fatalf("route got %q, want [%q]", got, command)
	}
}

func TestServeRunsOneExecPerSession(t *testing.T) {
	route := &fakeRoute{status: 3, block: make(chan struct{})}
	client := startServer(t, route)

	sess, err := client.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sess.Close() }()
	if err := sess.Start("first"); err != nil {
		t.Fatal(err)
	}
	ok, err := sess.SendRequest("exec", true, ssh.Marshal(struct{ Command string }{"second"}))
	if err != nil || ok {
		t.Fatalf("second exec: ok=%v err=%v, want refused", ok, err)
	}
	close(route.block)
	if got := exitStatus(t, sess.Wait()); got != 3 {
		t.Fatalf("exit status = %d, want 3", got)
	}
	if got := route.commands(); len(got) != 1 || got[0] != "first" {
		t.Fatalf("route got %q, want [first]", got)
	}
}

func TestServeRefusesMalformedExec(t *testing.T) {
	route := &fakeRoute{}
	client := startServer(t, route)

	sess, err := client.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sess.Close() }()
	ok, err := sess.SendRequest("exec", true, []byte{0, 0, 0, 9, 'x'}) // length 9, one byte
	if err != nil || ok {
		t.Fatalf("malformed exec: ok=%v err=%v, want refused", ok, err)
	}
	// The session is still usable: a well-formed exec follows.
	out, err := sess.Output("later")
	if got := exitStatus(t, err); got != 0 || string(out) != "out:later" {
		t.Fatalf("exec after a malformed one: status %d stdout %q", got, out)
	}
	if got := route.commands(); len(got) != 1 || got[0] != "later" {
		t.Fatalf("route got %q, want [later]", got)
	}
}

func TestServeAnswersGlobalRequests(t *testing.T) {
	client := startServer(t, &fakeRoute{})
	forward := func(addr string, port uint32) []byte {
		return ssh.Marshal(struct {
			Addr string
			Port uint32
		}{addr, port})
	}

	ok, reply, err := client.SendRequest("tcpip-forward", true, forward("127.0.0.1", 0))
	if err != nil || !ok {
		t.Fatalf("tcpip-forward: ok=%v err=%v", ok, err)
	}
	var port struct{ Port uint32 }
	if err := ssh.Unmarshal(reply, &port); err != nil || port.Port == 0 {
		t.Fatalf("tcpip-forward reply = %x (%v), want a dynamic port", reply, err)
	}
	ok, reply, err = client.SendRequest("tcpip-forward", true, forward("127.0.0.1", 62870))
	if err != nil || !ok || len(reply) != 0 {
		t.Fatalf("tcpip-forward with fixed port: ok=%v reply=%x err=%v", ok, reply, err)
	}
	ok, _, err = client.SendRequest("tcpip-forward", true, []byte{1})
	if err != nil || ok {
		t.Fatalf("malformed tcpip-forward: ok=%v err=%v, want refused", ok, err)
	}
	ok, _, err = client.SendRequest("cancel-tcpip-forward", true, forward("127.0.0.1", 62870))
	if err != nil || !ok {
		t.Fatalf("cancel-tcpip-forward: ok=%v err=%v", ok, err)
	}
	ok, _, err = client.SendRequest("keepalive@openssh.com", true, nil)
	if err != nil || ok {
		t.Fatalf("keepalive: ok=%v err=%v, want refused", ok, err)
	}
	// A request without a reply is ignored and the connection stays usable.
	if _, _, err := client.SendRequest("no-more-sessions@openssh.com", false, nil); err != nil {
		t.Fatalf("no-reply request: %v", err)
	}
	if out, _, status := run(t, client, "after"); status != 0 || out != "out:after" {
		t.Fatalf("exec after global requests: status %d stdout %q", status, out)
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

func TestServeFailsWhenTheClientIsNotSSH(t *testing.T) {
	serverSide, clientSide := connPair(t)
	go func() {
		_, _ = io.WriteString(clientSide, "GET / HTTP/1.1\r\nHost: example\r\n\r\n")
		_ = clientSide.Close()
	}()
	done := make(chan error, 1)
	go func() { done <- sshd.Serve(serverSide, testSigner(t), &fakeRoute{}) }()
	select {
	case err := <-done:
		if err == nil || !strings.HasPrefix(err.Error(), "ssh handshake: ") {
			t.Fatalf("serve returned %v, want an ssh handshake error", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("serve did not return")
	}
}

// closeRecorder is a reader or writer whose Close reports whether it was
// called and returns err.
type closeRecorder struct {
	io.Reader
	io.Writer
	err    error
	closed bool
}

func (c *closeRecorder) Close() error {
	c.closed = true
	return c.err
}

func TestStdioConn(t *testing.T) {
	in := &closeRecorder{Reader: strings.NewReader("from stdin"), err: errors.New("stdin close failed")}
	var outBuf bytes.Buffer
	out := &closeRecorder{Writer: &outBuf}
	conn := &sshd.StdioConn{In: in, Out: out}

	got, err := io.ReadAll(conn)
	if err != nil || string(got) != "from stdin" {
		t.Fatalf("read %q, %v", got, err)
	}
	if _, err := io.WriteString(conn, "to stdout"); err != nil || outBuf.String() != "to stdout" {
		t.Fatalf("write: %v, stdout %q", err, outBuf.String())
	}
	if conn.LocalAddr().Network() != "stdio" || conn.LocalAddr().String() != "stdio" || conn.RemoteAddr().String() != "stdio" {
		t.Fatalf("addresses %v %v, want stdio", conn.LocalAddr(), conn.RemoteAddr())
	}
	if err := errors.Join(conn.SetDeadline(time.Now()), conn.SetReadDeadline(time.Now()), conn.SetWriteDeadline(time.Now())); err != nil {
		t.Fatalf("deadlines: %v", err)
	}
	if err := conn.Close(); !errors.Is(err, in.err) || !in.closed || !out.closed {
		t.Fatalf("close: err %v, stdin closed %v, stdout closed %v", err, in.closed, out.closed)
	}
	// Ends that cannot be closed are left alone.
	plain := &sshd.StdioConn{In: strings.NewReader(""), Out: io.Discard}
	if err := plain.Close(); err != nil {
		t.Fatalf("close without closers: %v", err)
	}
}

// subsystemRoute is a fakeRoute that also serves the "echo" subsystem,
// which writes "sub:<name>" and exits 7.
type subsystemRoute struct {
	fakeRoute
}

func (r *subsystemRoute) Subsystem(name string) (func(ssh.Channel) int, bool) {
	if name != "echo" {
		return nil, false
	}
	return func(ch ssh.Channel) int {
		_, _ = io.WriteString(ch, "sub:"+name)
		return 7
	}, true
}

func TestServeHandsSubsystemToRoute(t *testing.T) {
	route := &subsystemRoute{fakeRoute{block: make(chan struct{})}}
	client := startServer(t, route)

	sess, err := client.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	// The ssh client does not start a session for a subsystem, so its
	// output is read from the channel until the handler closes it.
	stdout, err := sess.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := sess.RequestSubsystem("echo"); err != nil {
		t.Fatalf("echo subsystem: %v", err)
	}
	out, err := io.ReadAll(stdout)
	if err != nil || string(out) != "sub:echo" {
		t.Fatalf("stdout %q, %v", out, err)
	}
	_ = sess.Close()

	// A name the route does not serve is refused, as is a second request on
	// a session that ran one, and the exec in that order.
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
	defer func() { _ = sess.Close() }()
	if err := sess.Start("first"); err != nil {
		t.Fatal(err)
	}
	ok, err := sess.SendRequest("subsystem", true, ssh.Marshal(struct{ Name string }{"echo"}))
	if err != nil || ok {
		t.Fatalf("subsystem after exec: ok=%v err=%v, want refused", ok, err)
	}
	ok, err = sess.SendRequest("subsystem", true, []byte{0, 0, 0, 9, 'x'})
	if err != nil || ok {
		t.Fatalf("malformed subsystem: ok=%v err=%v, want refused", ok, err)
	}
	close(route.block)
	if got := exitStatus(t, sess.Wait()); got != 0 {
		t.Fatalf("exit status = %d", got)
	}
}
