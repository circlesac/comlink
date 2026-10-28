package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"time"

	"github.com/coder/websocket"
	"golang.org/x/crypto/ssh"
)

// readLimit is the largest WebSocket message accepted from either side. The
// Codex app announces 16 MiB messages; the library default is 32 KiB.
const readLimit = 64 << 20

// relay serves the app-server proxy route on ch: the client speaks WebSocket
// over the channel (an HTTP Upgrade to ws://codex-app-server/rpc followed by
// JSON-RPC frames). That WebSocket is terminated here and every message is
// relayed, type preserved, to the upstream WebSocket and back. It returns
// the exit status for the command: 0 after a clean close, 1 otherwise.
func relay(ch ssh.Channel, opts options) int {
	conn := &channelConn{Channel: ch}
	br := bufio.NewReader(conn)
	req, err := http.ReadRequest(br)
	if err != nil {
		opts.logger.Printf("client did not send an HTTP upgrade: %v", err)
		return 1
	}
	w := &upgradeResponse{conn: conn, br: br, header: http.Header{}}
	client, err := websocket.Accept(w, req, &websocket.AcceptOptions{InsecureSkipVerify: true})
	if err != nil {
		opts.logger.Printf("client websocket handshake failed: %v", err)
		return 1
	}
	client.SetReadLimit(readLimit)
	upstream, err := dialUpstream(opts)
	if err != nil {
		// err may quote the URL and the response status but never the header.
		opts.logger.Printf("upstream connect failed: %v", err)
		_ = client.Close(websocket.StatusInternalError, "upstream unavailable")
		return 1
	}
	upstream.SetReadLimit(readLimit)

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
	if clean {
		code, reason = status, "peer closed"
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
		opts.logger.Printf("upstream failed: %v", first.err)
	}
	return 1
}

func dialUpstream(opts options) (*websocket.Conn, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var header http.Header
	if opts.token != "" {
		header = http.Header{"Authorization": {"Bearer " + opts.token}}
	}
	c, resp, err := websocket.Dial(ctx, opts.upstream, &websocket.DialOptions{HTTPHeader: header})
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	return c, err
}

// channelConn presents an SSH channel as a net.Conn. Close is a no-op: the
// session handler closes the channel after the exit status has been sent.
type channelConn struct {
	ssh.Channel
}

func (c *channelConn) Close() error                     { return nil }
func (c *channelConn) LocalAddr() net.Addr              { return stdioAddr{} }
func (c *channelConn) RemoteAddr() net.Addr             { return stdioAddr{} }
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
