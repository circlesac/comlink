package codex

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
	"golang.org/x/crypto/ssh"
)

// ReadLimit is the largest WebSocket message accepted from either side. The
// Codex app announces 16 MiB messages; the library default is 32 KiB.
const ReadLimit = 64 << 20

// relay serves the app-server proxy route on ch: the client speaks WebSocket
// over the channel (an HTTP Upgrade to ws://codex-app-server/rpc followed by
// JSON-RPC frames). That WebSocket is terminated here and every message is
// relayed, type preserved, to the upstream WebSocket and back. It returns
// the exit status for the command: 0 after a clean close, 1 otherwise. When
// the client side ends without a close frame the upstream is told 1001
// (going away); any other failure is passed on as 1011.
func (r *Route) relay(ch ssh.Channel) int {
	conn := newChannelConn(ch)
	defer func() { _ = conn.Close() }()
	br := bufio.NewReader(conn)
	req, err := http.ReadRequest(br)
	if err != nil {
		r.Logger.Printf("client did not send an HTTP upgrade: %v", err)
		return 1
	}
	w := &upgradeResponse{conn: conn, br: br, header: http.Header{}}
	client, err := websocket.Accept(w, req, &websocket.AcceptOptions{InsecureSkipVerify: true})
	if err != nil {
		r.Logger.Printf("client websocket handshake failed: %v", err)
		return 1
	}
	client.SetReadLimit(ReadLimit)
	upstream, err := r.dialUpstream()
	if err != nil {
		// err names the upstream without its query or userinfo and never the header.
		r.Logger.Printf("upstream connect failed: %v", err)
		_ = client.Close(websocket.StatusInternalError, "upstream unavailable")
		return 1
	}
	upstream.SetReadLimit(ReadLimit)

	type outcome struct {
		err      error
		upstream bool // the upstream side failed or closed
	}
	outcomes := make(chan outcome, 2)
	pump := func(src, dst *websocket.Conn, srcIsUpstream bool) {
		for {
			typ, data, err := src.Read(context.Background())
			if err != nil {
				outcomes <- outcome{err: err, upstream: srcIsUpstream}
				return
			}
			if err := dst.Write(context.Background(), typ, data); err != nil {
				outcomes <- outcome{err: err, upstream: !srcIsUpstream}
				return
			}
		}
	}
	go pump(client, upstream, false)
	go pump(upstream, client, true)

	first := <-outcomes
	status := websocket.CloseStatus(first.err)
	clean := status == websocket.StatusNormalClosure || status == websocket.StatusGoingAway
	code, reason := websocket.StatusInternalError, "peer failed"
	switch {
	case clean:
		code, reason = status, "peer closed"
	case !first.upstream && status == -1:
		// The SSH channel ended without a close frame: the client (the ssh
		// process) went away rather than reporting a failure.
		code, reason = websocket.StatusGoingAway, "client went away"
	}
	if first.upstream {
		_ = client.Close(code, reason)
		_ = upstream.CloseNow()
	} else {
		_ = upstream.Close(code, reason)
		_ = client.CloseNow()
	}
	<-outcomes

	if clean {
		return 0
	}
	if first.upstream {
		r.Logger.Printf("upstream failed: %v", first.err)
	}
	return 1
}

func (r *Route) dialUpstream() (*websocket.Conn, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var header http.Header
	if r.Token != "" {
		header = http.Header{"Authorization": {"Bearer " + r.Token}}
	}
	c, resp, err := websocket.Dial(ctx, r.Upstream, &websocket.DialOptions{HTTPHeader: header})
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	if err != nil {
		return nil, redactDialError(err, r.Upstream)
	}
	return c, nil
}

// redactDialError rewrites a websocket.Dial failure so that it names the
// upstream without its query or userinfo. Go's URL and HTTP errors quote the
// full URL, so an upstream such as wss://host/rpc?token=... would otherwise
// put the token in the log.
func redactDialError(err error, upstream string) error {
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		err = urlErr.Err // the cause, without the URL
	}
	return fmt.Errorf("dial %s: %w", redactURL(upstream), err)
}

// redactURL returns raw without its userinfo, query and fragment. A URL that
// does not parse is not echoed at all: its secret part cannot be told apart.
func redactURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return "<invalid upstream URL>"
	}
	u.User, u.RawQuery, u.ForceQuery, u.Fragment, u.RawFragment = nil, "", false, "", ""
	return u.String()
}

// channelConn presents an SSH channel as a net.Conn. Reads go through a pipe
// so that Close unblocks a pending Read without closing the channel: the
// WebSocket library closes the conn when a close frame arrives or its close
// handshake times out, and the session handler must still send the exit
// status on the channel afterwards.
type channelConn struct {
	ssh.Channel
	pr     *io.PipeReader
	closed atomic.Bool
}

func newChannelConn(ch ssh.Channel) *channelConn {
	pr, pw := io.Pipe()
	go func() {
		_, err := io.Copy(pw, ch)
		_ = pw.CloseWithError(err)
	}()
	return &channelConn{Channel: ch, pr: pr}
}

type channelAddr struct{}

func (channelAddr) Network() string { return "stdio" }
func (channelAddr) String() string  { return "stdio" }

func (c *channelConn) Read(b []byte) (int, error) { return c.pr.Read(b) }

func (c *channelConn) Write(b []byte) (int, error) {
	if c.closed.Load() {
		return 0, net.ErrClosed
	}
	return c.Channel.Write(b)
}

// Close ends reads and writes on the conn; the channel itself stays open for
// the exit status. The copying goroutine ends when the channel is closed.
func (c *channelConn) Close() error {
	c.closed.Store(true)
	return c.pr.CloseWithError(net.ErrClosed)
}

func (c *channelConn) LocalAddr() net.Addr              { return channelAddr{} }
func (c *channelConn) RemoteAddr() net.Addr             { return channelAddr{} }
func (c *channelConn) SetDeadline(time.Time) error      { return nil }
func (c *channelConn) SetReadDeadline(time.Time) error  { return nil }
func (c *channelConn) SetWriteDeadline(time.Time) error { return nil }

// upgradeResponse is the minimal http.ResponseWriter and http.Hijacker that
// websocket.Accept needs to answer one Upgrade request on a raw connection.
// As with net/http, a 101 status is written to the wire when Hijack is called.
type upgradeResponse struct {
	conn        net.Conn
	br          *bufio.Reader
	header      http.Header
	status      int
	wroteHeader bool
}

func (w *upgradeResponse) Header() http.Header { return w.header }

func (w *upgradeResponse) WriteHeader(status int) {
	if w.wroteHeader {
		return
	}
	w.wroteHeader = true
	w.status = status
	if status != http.StatusSwitchingProtocols {
		w.header.Set("Connection", "close")
		_ = w.flushHeader()
	}
}

func (w *upgradeResponse) Write(b []byte) (int, error) {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	return w.conn.Write(b)
}

func (w *upgradeResponse) flushHeader() error {
	bw := bufio.NewWriter(w.conn)
	if _, err := fmt.Fprintf(bw, "HTTP/1.1 %d %s\r\n", w.status, http.StatusText(w.status)); err != nil {
		return err
	}
	if err := w.header.Write(bw); err != nil {
		return err
	}
	if _, err := bw.WriteString("\r\n"); err != nil {
		return err
	}
	return bw.Flush()
}

func (w *upgradeResponse) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if !w.wroteHeader || w.status != http.StatusSwitchingProtocols {
		return nil, nil, errors.New("hijack before 101 Switching Protocols: status " + strconv.Itoa(w.status))
	}
	if err := w.flushHeader(); err != nil {
		return nil, nil, err
	}
	return w.conn, bufio.NewReadWriter(w.br, bufio.NewWriter(w.conn)), nil
}
