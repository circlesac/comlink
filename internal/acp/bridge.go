// Package acp bridges an ACP client's stdio to a remote agent. The client
// launches comlink as its agent subprocess and speaks newline-delimited
// JSON-RPC on its stdin/stdout; the agent serves ACP over a WebSocket, one
// message per text frame. The bridge moves lines to frames and frames to
// lines without parsing them.
package acp

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"

	"github.com/coder/websocket"

	"github.com/circlesac/comlink/internal/upstream"
)

// Bridge relays one ACP session between stdio and Upstream.
type Bridge struct {
	Upstream string      // WebSocket endpoint that serves ACP
	Token    string      // bearer token for Upstream; empty sends no Authorization header
	Logger   *log.Logger // diagnostics; never given the token
}

// errBinary ends the session when the upstream sends a binary message: ACP
// messages are text, and stdout carries nothing but protocol lines.
var errBinary = errors.New("upstream sent a binary message")

// errNewline ends the session when a text message from the upstream holds a
// newline (\n or \r): the client would read it as several lines.
var errNewline = errors.New("upstream sent a message that contains a newline")

// stdoutError is a failed write to stdout: the client has gone away.
type stdoutError struct{ err error }

func (e stdoutError) Error() string { return "write stdout: " + e.err.Error() }
func (e stdoutError) Unwrap() error { return e.err }

// upstreamWriteError is a failed write of a stdin line to the upstream.
type upstreamWriteError struct{ err error }

func (e upstreamWriteError) Error() string { return "write upstream: " + e.err.Error() }
func (e upstreamWriteError) Unwrap() error { return e.err }

// Run relays until one side ends and returns the exit status. Each stdin line
// (without its newline; a trailing \r is dropped and empty lines are skipped)
// is sent as one text frame, and each text frame from the upstream is written
// to stdout followed by \n; a binary frame or a text frame holding \n or \r
// is not written and ends the session with 1003. At stdin EOF the upstream
// is closed with 1000 and Run returns 0 once the close handshake completes. When the upstream closes
// first, Run returns without reading stdin further: 0 for a normal or
// going-away close, 1 otherwise. A failed connect returns 1.
func (b *Bridge) Run(stdin io.Reader, stdout io.Writer) int {
	conn, err := upstream.Dial(b.Upstream, b.Token)
	if err != nil {
		// err names the upstream without its query or userinfo and never the header.
		b.Logger.Printf("upstream connect failed: %v", err)
		return 1
	}
	// The stdin goroutine is not waited for once the upstream has ended: a
	// read from stdin cannot be interrupted. The reader always is, so stdout
	// is not written after Run returns; it ends once conn is closed.
	stdinDone := make(chan error, 1)
	go func() { stdinDone <- forwardLines(stdin, conn) }()
	var readErr error
	readerDone := make(chan struct{})
	go func() {
		readErr = forwardFrames(conn, stdout)
		close(readerDone)
	}()

	select {
	case <-readerDone:
		return b.upstreamEnded(conn, readErr)
	case err := <-stdinDone:
		var writeErr upstreamWriteError
		switch {
		case err == nil:
			closeErr := conn.Close(websocket.StatusNormalClosure, "")
			<-readerDone
			// The upstream may have failed at the same moment; its own close
			// status, when the reader saw one, wins over ours.
			if status := websocket.CloseStatus(readErr); status != -1 && !clean(status) {
				b.Logger.Printf("upstream failed: %v", readErr)
				return 1
			}
			if closeErr != nil {
				b.Logger.Printf("upstream close failed: %v", closeErr)
				return 1
			}
			return 0
		case errors.As(err, &writeErr) && errors.Is(err, net.ErrClosed):
			// The upstream closed under the write; the reader has its status.
			<-readerDone
			return b.upstreamEnded(conn, readErr)
		case errors.Is(err, bufio.ErrTooLong):
			b.Logger.Printf("stdin message longer than %d bytes", upstream.ReadLimit)
			_ = conn.Close(websocket.StatusMessageTooBig, "message too big")
		default:
			b.Logger.Printf("%v", err)
			_ = conn.Close(websocket.StatusInternalError, "client failed")
		}
		<-readerDone
		return 1
	}
}

// upstreamEnded handles the reader's end: the upstream closed or failed, it
// sent a binary message or one with a newline, or stdout could not be written.
func (b *Bridge) upstreamEnded(conn *websocket.Conn, err error) int {
	var outErr stdoutError
	switch {
	case errors.As(err, &outErr):
		b.Logger.Printf("%v", err)
		_ = conn.Close(websocket.StatusGoingAway, "client went away")
		return 1
	case errors.Is(err, errBinary):
		b.Logger.Printf("%v", err)
		_ = conn.Close(websocket.StatusUnsupportedData, "binary message")
		return 1
	case errors.Is(err, errNewline):
		b.Logger.Printf("%v", err)
		_ = conn.Close(websocket.StatusUnsupportedData, "message contains a newline")
		return 1
	}
	_ = conn.CloseNow()
	if clean(websocket.CloseStatus(err)) {
		return 0
	}
	b.Logger.Printf("upstream failed: %v", err)
	return 1
}

func clean(status websocket.StatusCode) bool {
	return status == websocket.StatusNormalClosure || status == websocket.StatusGoingAway
}

// forwardLines sends each stdin line to conn as a text frame and returns nil
// at EOF.
func forwardLines(stdin io.Reader, conn *websocket.Conn) error {
	// Bytes are examined as they arrive and only once: a bufio.Scanner rescans
	// its whole buffer after every read (quadratic in the line length), and
	// ReadSlice waits for a full buffer or a newline before the length can be
	// checked.
	br := bufio.NewReaderSize(stdin, 64<<10)
	var line []byte
	for {
		if _, err := br.Peek(1); err != nil {
			if !errors.Is(err, io.EOF) {
				return fmt.Errorf("read stdin: %w", err)
			}
			// EOF: a last line without a newline is still a message.
			return sendLine(conn, line)
		}
		data, _ := br.Peek(br.Buffered())
		if i := bytes.IndexByte(data, '\n'); i >= 0 {
			line = append(line, data[:i]...)
			_, _ = br.Discard(i + 1)
			if err := sendLine(conn, line); err != nil {
				return err
			}
			line = line[:0]
			continue
		}
		line = append(line, data...)
		_, _ = br.Discard(len(data))
		// No newline yet: ReadLimit message bytes plus a possible "\r" fit,
		// anything more cannot be a message within the limit.
		if len(line) > upstream.ReadLimit+1 {
			return fmt.Errorf("read stdin: %w", bufio.ErrTooLong)
		}
	}
}

// sendLine sends line (a trailing \r dropped) as one text frame; an empty
// line is skipped.
func sendLine(conn *websocket.Conn, line []byte) error {
	msg := bytes.TrimSuffix(line, []byte("\r"))
	if len(msg) > upstream.ReadLimit {
		return fmt.Errorf("read stdin: %w", bufio.ErrTooLong)
	}
	if len(msg) == 0 {
		return nil
	}
	if err := conn.Write(context.Background(), websocket.MessageText, msg); err != nil {
		return upstreamWriteError{err}
	}
	return nil
}

// forwardFrames writes each text frame from conn to stdout as one line until
// the read or the write fails, or a frame is binary or holds a newline.
func forwardFrames(conn *websocket.Conn, stdout io.Writer) error {
	for {
		typ, data, err := conn.Read(context.Background())
		if err != nil {
			return err
		}
		if typ != websocket.MessageText {
			return errBinary
		}
		if bytes.ContainsAny(data, "\r\n") {
			return errNewline
		}
		if _, err := stdout.Write(append(data, '\n')); err != nil {
			return stdoutError{err}
		}
	}
}
